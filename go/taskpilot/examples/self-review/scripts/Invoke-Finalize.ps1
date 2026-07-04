#Requires -Version 7.4
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$RepoRoot,
    [Parameter(Mandatory)][string]$RunDir,
    [Parameter(Mandatory)][string]$BaselineFile,
    [Parameter(Mandatory)][string]$Axis,
    [Parameter(Mandatory)][string]$Title,
    [Parameter(Mandatory)][string]$LogPath,
    [Parameter(Mandatory)][int]$Index,
    [Parameter(Mandatory)][int]$Total,
    [Parameter(Mandatory)][string]$HasCandidate
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Write-Err([string]$msg) { [Console]::Error.WriteLine($msg) }
$has = ($HasCandidate -match '^(?i)true$')
$shouldContinue = (($Index + 1) -lt $Total)

function New-Result([bool]$accepted, [string]$reason) {
    $o = [ordered]@{ accepted = $accepted; reason = $reason; shouldContinue = $shouldContinue }
    [Console]::Out.WriteLine(($o | ConvertTo-Json -Depth 4 -Compress))
}

function Add-LogEntry([string]$verdict, [string]$reason, $base, $cur) {
    $stamp = (Get-Date).ToString('yyyy-MM-dd HH:mm:ss')
    $lines = @("", "## $stamp -- ${Axis}: $Title", "- Verdict: $verdict ($reason)")
    if ($base -and $cur) {
        $lines += "- Gate: build=$($cur.buildOk) test=$($cur.testOk) coveredLines $($base.coveredLines)->$($cur.coveredLines) testCount $($base.testCount)->$($cur.testCount)"
        $lines += "- Proxies: vet $($base.vetCount)->$($cur.vetCount) exported $($base.exportedCount)->$($cur.exportedCount) gocyclo $($base.gocycloOver)->$($cur.gocycloOver) deadcode $($base.deadcodeCount)->$($cur.deadcodeCount) globalVar $($base.globalVarCount)->$($cur.globalVarCount)"
    }
    Add-Content -LiteralPath $LogPath -Value ($lines -join "`n") -Encoding ascii
}

if (-not $has) {
    Add-LogEntry 'SKIPPED' 'no candidate for this slot' $null $null
    New-Result $false 'no candidate'
    return
}

# Did the fixer actually change tracked files? (.tmp is git-ignored, so the
# baseline/ranked artifacts never count as changes.)
$status = (& git -C $RepoRoot status --porcelain) 2>&1
if ([string]::IsNullOrWhiteSpace(($status | Out-String))) {
    Add-LogEntry 'SKIPPED' 'fixer made no source changes' $null $null
    New-Result $false 'no changes'
    return
}

# Re-probe in a separate process so its stdout does not leak into ours.
$scriptDir = Split-Path -Parent $PSCommandPath
$probe = Join-Path $scriptDir 'Invoke-Probe.ps1'
$curJson = (& pwsh -NoProfile -NonInteractive -File $probe -RepoRoot $RepoRoot -RunDir $RunDir -Label current) 2>$null
try { $cur = $curJson | ConvertFrom-Json } catch { $cur = $null }
try { $base = Get-Content -LiteralPath $BaselineFile -Raw | ConvertFrom-Json } catch { $base = $null }

if ($null -eq $cur -or $null -eq $base) {
    # Cannot evaluate the gate -> reject conservatively.
    $null = (& git -C $RepoRoot reset --hard HEAD) 2>&1
    Add-LogEntry 'REJECTED' 'probe/baseline unavailable' $base $cur
    New-Result $false 'probe failed'
    return
}

# Do-no-harm floor: build + test green, covered lines + test count not dropping.
$accept = $cur.buildOk -and $cur.testOk -and
    ($cur.coveredLines -ge $base.coveredLines) -and
    ($cur.testCount -ge $base.testCount)

if ($accept) {
    $null = (& git -C $RepoRoot add -A) 2>&1
    $null = (& git -C $RepoRoot commit -m "selfreview ${Axis}: ${Title}") 2>&1
    if ($LASTEXITCODE -ne 0) {
        $null = (& git -C $RepoRoot reset --hard HEAD) 2>&1
        Add-LogEntry 'REJECTED' 'commit failed' $base $cur
        New-Result $false 'commit failed'
        return
    }
    # Accepted run becomes the new baseline for the next candidate.
    Set-Content -LiteralPath $BaselineFile -Value ($cur | ConvertTo-Json -Depth 6 -Compress) -Encoding ascii
    Add-LogEntry 'ACCEPTED' 'gate held' $base $cur
    New-Result $true 'accepted'
}
else {
    $reason = if (-not $cur.buildOk) { 'build red' }
    elseif (-not $cur.testOk) { 'test red' }
    elseif ($cur.coveredLines -lt $base.coveredLines) { 'covered lines dropped' }
    else { 'test count dropped' }
    $null = (& git -C $RepoRoot reset --hard HEAD) 2>&1
    Add-LogEntry 'REJECTED' $reason $base $cur
    New-Result $false $reason
}
