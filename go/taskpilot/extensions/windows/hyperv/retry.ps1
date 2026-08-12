Set-StrictMode -Version Latest

function Invoke-WithRetry {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][scriptblock]$ScriptBlock,
        [string]$OperationName = 'Operation',
        [ValidateRange(0, [int]::MaxValue)][int]$MaxRetries = [int]::MaxValue,
        [ValidateRange(0, [int]::MaxValue)][int]$BaseDelaySeconds = 2,
        [ValidateRange(0, [int]::MaxValue)][int]$MaxDelaySeconds = 0,
        [ValidateRange(0, [int]::MaxValue)][int]$TimeoutSeconds = 0,
        [scriptblock]$RetryCondition,
        [scriptblock]$SuccessCondition,
        [scriptblock]$DescribeResult,
        [switch]$IncludeMetadata
    )

    if ($MaxDelaySeconds -le 0) { $MaxDelaySeconds = $BaseDelaySeconds }
    $started = Get-Date
    $deadline = if ($TimeoutSeconds -gt 0) { $started.AddSeconds($TimeoutSeconds) } else { $null }
    $attempt = 0
    $lastFailure = $null
    $lastResult = $null

    while ($true) {
        $attempt++
        $failure = $null
        try {
            $lastResult = & $ScriptBlock
            $succeeded = if ($SuccessCondition) { [bool](& $SuccessCondition $lastResult) } else { $true }
            if ($succeeded) {
                if (-not $IncludeMetadata) { return $lastResult }
                return [pscustomobject][ordered]@{
                    Value       = $lastResult
                    Attempts    = $attempt
                    DurationSec = [int]((Get-Date) - $started).TotalSeconds
                }
            }
            $lastFailure = if ($DescribeResult) { [string](& $DescribeResult $lastResult) } else { 'success condition was not met' }
            $failure = [System.Management.Automation.RuntimeException]::new($lastFailure)
        }
        catch {
            $failure = $_.Exception
            $lastFailure = $failure.Message
        }

        if ($RetryCondition -and -not [bool](& $RetryCondition $failure)) {
            throw $failure
        }
        if ($attempt -gt $MaxRetries) {
            throw [System.Management.Automation.RuntimeException]::new(
                "$OperationName failed after $attempt attempts. Last failure: $lastFailure", $failure)
        }

        $now = Get-Date
        if ($deadline -and $now -ge $deadline) {
            throw [System.TimeoutException]::new(
                "$OperationName timed out after ${TimeoutSeconds}s ($attempt attempts). Last failure: $lastFailure", $failure)
        }

        $delay = [int][math]::Min(
            $BaseDelaySeconds * [math]::Pow(2, $attempt - 1),
            $MaxDelaySeconds)
        if ($deadline) {
            $remaining = [int][math]::Ceiling(($deadline - $now).TotalSeconds)
            if ($remaining -le 0) {
                throw [System.TimeoutException]::new(
                    "$OperationName timed out after ${TimeoutSeconds}s ($attempt attempts). Last failure: $lastFailure", $failure)
            }
            $delay = [math]::Min($delay, $remaining)
        }
        if ($delay -gt 0) { Start-Sleep -Seconds $delay }

        if ($deadline -and (Get-Date) -ge $deadline) {
            throw [System.TimeoutException]::new(
                "$OperationName timed out after ${TimeoutSeconds}s ($attempt attempts). Last failure: $lastFailure", $failure)
        }
    }
}

function Wait-Until {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][scriptblock]$Probe,
        [Parameter(Mandatory)][scriptblock]$Until,
        [string]$OperationName = 'condition',
        [ValidateRange(0, [int]::MaxValue)][int]$TimeoutSeconds = 300,
        [ValidateRange(0, [int]::MaxValue)][int]$PollIntervalSeconds = 2,
        [ValidateRange(0, [int]::MaxValue)][int]$MaxRetries = [int]::MaxValue,
        [scriptblock]$DescribeResult
    )

    Invoke-WithRetry `
        -ScriptBlock $Probe `
        -OperationName $OperationName `
        -TimeoutSeconds $TimeoutSeconds `
        -MaxRetries $MaxRetries `
        -BaseDelaySeconds $PollIntervalSeconds `
        -MaxDelaySeconds $PollIntervalSeconds `
        -SuccessCondition $Until `
        -DescribeResult $DescribeResult `
        -IncludeMetadata
}
