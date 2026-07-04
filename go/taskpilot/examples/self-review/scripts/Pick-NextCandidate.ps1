#Requires -Version 7.4
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$RepoRoot,
    [Parameter(Mandatory)][string]$RankedFile,
    [Parameter(Mandatory)][int]$Index
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$ranked = @()
if (Test-Path -LiteralPath $RankedFile) {
    try { $ranked = @(Get-Content -LiteralPath $RankedFile -Raw | ConvertFrom-Json) }
    catch { $ranked = @() }
}
$total = $ranked.Count

if ($Index -ge 0 -and $Index -lt $total) {
    $c = $ranked[$Index]
    $title = if ($c.PSObject.Properties.Name -contains 'title') { [string]$c.title } else { "candidate $Index" }
    $axis = if ($c.PSObject.Properties.Name -contains 'axis') { [string]$c.axis } else { 'R0' }
    $out = [ordered]@{
        candidate    = $c
        title        = $title
        axis         = $axis
        hasCandidate = $true
        total        = $total
    }
}
else {
    $out = [ordered]@{
        candidate    = @{}
        title        = 'none'
        axis         = 'R0'
        hasCandidate = $false
        total        = $total
    }
}
[Console]::Out.WriteLine(($out | ConvertTo-Json -Depth 8 -Compress))
