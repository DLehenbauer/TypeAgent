$ErrorActionPreference = 'Stop'
$payload = Get-Content -Raw -LiteralPath $args[0] | ConvertFrom-Json
$opts = $payload.options

Import-Module -Force -Name (Join-Path $PSScriptRoot 'host.psm1')
Import-Module -Force -Name (Join-Path $PSScriptRoot 'guest.psm1')
$retryHelperPath = Join-Path $PSScriptRoot 'retry.ps1'
$retryHelperSource = Get-Content -Raw -LiteralPath $retryHelperPath
. $retryHelperPath

function Write-Json($value) {
    $value | ConvertTo-Json -Depth 40 -Compress
}

function New-HyperVGuestCredential($options) {
    $guest = $options.guest
    if (-not $guest -or [string]::IsNullOrWhiteSpace([string]$guest.username) -or
        [string]::IsNullOrWhiteSpace([string]$guest.password)) {
        throw 'hyperv: guest username and password are required for guest operations'
    }
    $secure = ConvertTo-SecureString -String ([string]$guest.password) -AsPlainText -Force
    [pscredential]::new([string]$guest.username, $secure)
}

function Get-HyperVWorkspaceMount($options) {
    $workspace = if ($options.PSObject.Properties['workspace']) { $options.workspace } else { $null }
    if (-not $workspace) {
        return [pscustomobject]@{ Unc = $null; Drive = $null; User = $null; Password = $null }
    }
    [pscustomobject]@{
        Unc      = [string]$workspace.uncPath
        Drive    = [string]$workspace.drive
        User     = [string]$workspace.username
        Password = [string]$workspace.password
    }
}

function Mount-HyperVWorkspace {
    param(
        [Parameter(Mandatory)][string]$UncPath,
        [Parameter(Mandatory)][string]$Drive,
        [Parameter(Mandatory)][string]$Username,
        [Parameter(Mandatory)][string]$Password,
        [int]$TimeoutSeconds = 60,
        [int]$DelaySeconds = 10,
        [scriptblock]$NetUse
    )
    if (-not $NetUse) {
        $NetUse = {
            param([object[]]$Arguments)
            $output = @(& net @Arguments 2>&1)
            [pscustomobject]@{ ExitCode = $LASTEXITCODE; Output = $output }
        }
    }
    $letter = $Drive.TrimEnd(':')
    Invoke-WithRetry `
        -OperationName "map ${Drive} to ${UncPath}" `
        -TimeoutSeconds $TimeoutSeconds `
        -BaseDelaySeconds $DelaySeconds `
        -MaxDelaySeconds $DelaySeconds `
        -RetryCondition {
            param($failure)
            if ($failure.Message -match '(?i)System error\s+(\d+)') {
                return [int]$Matches[1] -notin @(5, 86, 1326)
            }
            return $true
        } `
        -ScriptBlock {
            $null = & $NetUse @('use', "${letter}:", '/delete', '/y')
            $mapped = & $NetUse @('use', "${letter}:", $UncPath, $Password, "/user:$Username", '/persistent:no')
            if ([int]$mapped.ExitCode -ne 0) {
                throw (@($mapped.Output) -join "`n")
            }
        } |
        Out-Null
}

function Wait-HyperVGuest($options, [string]$vmName) {
    $guest = $options.guest
    $credential = New-HyperVGuestCredential $options
    Wait-HyperVGuestReady `
        -VMName $vmName `
        -Credential $credential `
        -TimeoutSeconds ([int]$guest.readinessTimeoutSeconds) `
        -WinRMPort ([int]$guest.winRMPort) `
        -ConnectionTimeoutSec ([int]$guest.connectionTimeoutSeconds)
}

switch ($payload.action) {
    'acquire' {
        $params = @{
            VMName = [string]$opts.vmName
            BaseVhdxPath = [string]$opts.baseImage
            VmRoot = [string]$opts.vmRoot
            VhdRoot = [string]$opts.vhdRoot
            SwitchName = [string]$opts.switchName
            MemoryMB = [int]$opts.memoryMB
            ProcessorCount = [int]$opts.processorCount
            Generation = [int]$opts.generation
            Start = $true
        }
        if ($opts.PSObject.Properties['disableSecureBoot'] -and $opts.disableSecureBoot) {
            $params.DisableSecureBoot = $true
        }
        $vm = New-HyperVManagedVM @params
        $baseline = [string]$opts.baselineState
        $existed = Test-HyperVCheckpoint -VMName $opts.vmName -Name $baseline
        if (-not $existed) {
            New-HyperVCheckpoint -VMName $opts.vmName -Name $baseline | Out-Null
        }
        $materialized = if (-not $existed) { $baseline } else { $null }
        Write-Json ([ordered]@{
            id = [string]$opts.vmName
            baselineState = $baseline
            materializedState = $materialized
            reused = [bool]$vm.Reused
        })
    }
    'testCheckpoint' {
        Write-Json ([ordered]@{
            exists = [bool](Test-HyperVCheckpoint -VMName $payload.id -Name $payload.state)
        })
    }
    'restoreCheckpoint' {
        Restore-HyperVCheckpoint -VMName $payload.id -Name $payload.state -SwitchName ([string]$opts.switchName) | Out-Null
        Write-Json ([ordered]@{ restored = $true })
    }
    'newCheckpoint' {
        New-HyperVCheckpoint -VMName $payload.id -Name $payload.state -Force | Out-Null
        Write-Json ([ordered]@{ checkpointed = $true })
    }
    'runGuest' {
        $ready = Wait-HyperVGuest $opts $payload.id
        $guest = $opts.guest
        $credential = New-HyperVGuestCredential $opts
        $session = $null
        try {
            $session = Invoke-WithRetry `
                -OperationName "WinRM session to $($ready.Host)" `
                -TimeoutSeconds 30 `
                -MaxRetries 5 `
                -BaseDelaySeconds 2 `
                -MaxDelaySeconds 5 `
                -RetryCondition {
                    param($failure)
                    $failure.Message -notmatch '(?i)access is denied|user name or password|authentication failed'
                } `
                -ScriptBlock {
                    New-HyperVWinRMSession `
                        -ComputerName $ready.Host `
                        -Port ([int]$guest.winRMPort) `
                        -Credential $credential `
                        -OpenTimeoutSec ([int]$guest.connectionTimeoutSeconds) `
                        -OperationTimeoutSec ([int]$guest.connectionTimeoutSeconds * 2)
                }

            $workspace = Get-HyperVWorkspaceMount $opts
                $workspaceHelperSource = "function Mount-HyperVWorkspace {`n$(${function:Mount-HyperVWorkspace}.ToString())`n}"
                $remote = Invoke-Command `
                    -Session $session `
                    -ArgumentList $payload.script,@($payload.args),$payload.cwd,([int]$payload.timeoutSeconds),$workspace.Unc,$workspace.Drive,$workspace.User,$workspace.Password,$retryHelperSource,$workspaceHelperSource `
                    -ScriptBlock {
                        param($script, [object[]]$boundArgs, $cwd, [int]$timeoutSeconds, $shareUnc, $shareDrive, $shareUser, $sharePassword, $retrySource, $workspaceSource)
                    $dir = Join-Path $env:TEMP ('taskpilot-' + [guid]::NewGuid().ToString('N'))
                    New-Item -ItemType Directory -Force -Path $dir | Out-Null
                    try {
                        $scriptPath = Join-Path $dir 'body.ps1'
                        $stdoutPath = Join-Path $dir 'stdout.txt'
                        $stderrPath = Join-Path $dir 'stderr.txt'
                        Set-Content -LiteralPath $scriptPath -Value $script -Encoding UTF8
                        $old = Get-Location
                        try {
                            if (-not [string]::IsNullOrWhiteSpace($cwd)) {
                                Set-Location -LiteralPath $cwd
                            }
                            $job = Start-Job -ScriptBlock {
                                param($file, [object[]]$argv, $out, $err, $unc, $drive, $user, $password, $retrySource, $workspaceSource)
                                . ([scriptblock]::Create($retrySource))
                                . ([scriptblock]::Create($workspaceSource))
                                if ($unc -and $drive) {
                                    try {
                                        Mount-HyperVWorkspace -UncPath $unc -Drive $drive -Username $user -Password $password
                                    }
                                    catch {
                                        @("Unable to map ${drive} to ${unc} within 60s.", $_.Exception.Message) |
                                            Out-File -FilePath $err -Encoding utf8
                                        return 1
                                    }
                                }
                                & powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $file @argv > $out 2> $err
                                $LASTEXITCODE
                            } -ArgumentList $scriptPath,$boundArgs,$stdoutPath,$stderrPath,$shareUnc,$shareDrive,$shareUser,$sharePassword,$retrySource,$workspaceSource

                            if (-not (Wait-Job -Job $job -Timeout $timeoutSeconds)) {
                                Stop-Job -Job $job
                                [pscustomobject]@{
                                    stdout = [string](Get-Content -Raw -LiteralPath $stdoutPath -ErrorAction SilentlyContinue)
                                    stderr = "timed out after ${timeoutSeconds}s"
                                    exitCode = 1
                                    timedOut = $true
                                }
                            }
                            else {
                                $code = Receive-Job -Job $job
                                [pscustomobject]@{
                                    stdout = [string](Get-Content -Raw -LiteralPath $stdoutPath -ErrorAction SilentlyContinue)
                                    stderr = [string](Get-Content -Raw -LiteralPath $stderrPath -ErrorAction SilentlyContinue)
                                    exitCode = [int]$code[-1]
                                    timedOut = $false
                                }
                            }
                        }
                        finally {
                            Set-Location $old
                            if ($job) { Remove-Job -Job $job -Force -ErrorAction SilentlyContinue }
                        }
                    }
                    finally {
                        Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
                    }
                }
            Write-Json $remote
        }
        finally {
            if ($session) { Remove-PSSession -Session $session -ErrorAction SilentlyContinue }
        }
    }
    'rebootGuest' {
        $guest = $opts.guest
        $credential = New-HyperVGuestCredential $opts
        $ready = Wait-HyperVGuest $opts $payload.id
        $priorBootTime = Get-HyperVGuestBootTime `
            -VMName $payload.id `
            -Credential $credential `
            -ComputerName $ready.Host `
            -WinRMPort ([int]$guest.winRMPort) `
            -ConnectionTimeoutSec ([int]$guest.connectionTimeoutSeconds)
        if (-not $priorBootTime) {
            throw "hyperv: could not read the current boot time of VM '$($payload.id)'"
        }
        Restart-HyperVGuest `
            -VMName $payload.id `
            -Credential $credential `
            -ComputerName $ready.Host `
            -Reason 'TaskPilot layer requested reboot' `
            -WinRMPort ([int]$guest.winRMPort) `
            -ConnectionTimeoutSec ([int]$guest.connectionTimeoutSeconds) |
            Out-Null
        $proof = Wait-HyperVGuestReboot `
            -VMName $payload.id `
            -Credential $credential `
            -PriorBootTime $priorBootTime `
            -TimeoutSeconds ([int]$guest.readinessTimeoutSeconds) `
            -WinRMPort ([int]$guest.winRMPort) `
            -ConnectionTimeoutSec ([int]$guest.connectionTimeoutSeconds)
        Write-Json ([ordered]@{ rebooted = $true; durationSec = [double]$proof.DurationSec })
    }
    'removeVM' {
        $removeParams = @{ VMName = [string]$payload.id }
        if ($opts.PSObject.Properties['vmRoot']) { $removeParams.VmRoot = [string]$opts.vmRoot }
        if ($opts.PSObject.Properties['vhdRoot']) { $removeParams.VhdRoot = [string]$opts.vhdRoot }
        Remove-HyperVManagedVM @removeParams | Out-Null
        Write-Json ([ordered]@{ removed = $true })
    }
    'removeCheckpoint' {
        Remove-HyperVCheckpoint -VMName $payload.id -Name $payload.state -IgnoreMissing | Out-Null
        Write-Json ([ordered]@{ removed = $true })
    }
    default {
        throw "unknown hyperv action '$($payload.action)'"
    }
}
