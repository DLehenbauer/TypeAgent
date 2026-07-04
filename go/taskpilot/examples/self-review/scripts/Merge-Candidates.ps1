#Requires -Version 7.4
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$RepoRoot,
    [Parameter(Mandatory)][string]$RunDir,
    [Parameter(Mandatory)][int]$MaxCandidates
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Write-Err([string]$msg) { [Console]::Error.WriteLine($msg) }

# Axis application priority (structural -> cosmetic). Unknown axes sort last.
$priority = @{ R1 = 1; R2 = 2; R3 = 3; R4 = 4; R5 = 5; R6 = 6 }

$issues = Join-Path $RunDir 'issues'
$all = [System.Collections.Generic.List[object]]::new()
if (Test-Path -LiteralPath $issues) {
    foreach ($file in Get-ChildItem -LiteralPath $issues -Filter '*.candidates.json' -ErrorAction SilentlyContinue) {
        try {
            $doc = Get-Content -LiteralPath $file.FullName -Raw | ConvertFrom-Json
        }
        catch {
            Write-Err "merge: skipping unparseable $($file.Name): $_"
            continue
        }
        if ($null -eq $doc) { continue }
        $cands = if ($doc.PSObject.Properties.Name -contains 'candidates') { $doc.candidates } else { $doc }
        foreach ($c in @($cands)) {
            if ($null -eq $c) { continue }
            $all.Add($c)
        }
    }
}

# Dedup by fixTarget: keep the highest-confidence candidate per target.
$byTarget = @{}
foreach ($c in $all) {
    $target = if ($c.PSObject.Properties.Name -contains 'fixTarget') { [string]$c.fixTarget } else { '' }
    if ([string]::IsNullOrWhiteSpace($target)) { $target = [guid]::NewGuid().ToString() }
    $conf = if ($c.PSObject.Properties.Name -contains 'confidence') { [double]$c.confidence } else { 0.0 }
    if (-not $byTarget.ContainsKey($target)) { $byTarget[$target] = $c }
    else {
        $existing = $byTarget[$target]
        $exConf = if ($existing.PSObject.Properties.Name -contains 'confidence') { [double]$existing.confidence } else { 0.0 }
        if ($conf -gt $exConf) { $byTarget[$target] = $c }
    }
}

function Get-AxisRank($c) {
    $axis = if ($c.PSObject.Properties.Name -contains 'axis') { [string]$c.axis } else { 'R9' }
    if ($priority.ContainsKey($axis)) { return $priority[$axis] } else { return 99 }
}
function Get-Conf($c) {
    if ($c.PSObject.Properties.Name -contains 'confidence') { return [double]$c.confidence } else { return 0.0 }
}

$ranked = @($byTarget.Values |
    Sort-Object @{ Expression = { Get-AxisRank $_ } }, @{ Expression = { Get-Conf $_ }; Descending = $true } |
    Select-Object -First $MaxCandidates)

$rankedFile = Join-Path $RunDir 'ranked.json'
Set-Content -LiteralPath $rankedFile -Value ($ranked | ConvertTo-Json -Depth 8 -AsArray) -Encoding ascii

$count = @($ranked).Count
$out = [ordered]@{
    count       = $count
    iterations  = [Math]::Max($count, 1)
    rankedFile  = $rankedFile
}
[Console]::Out.WriteLine(($out | ConvertTo-Json -Depth 4 -Compress))
