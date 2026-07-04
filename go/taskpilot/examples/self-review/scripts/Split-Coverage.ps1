#Requires -Version 7.4
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$RepoRoot,
    [Parameter(Mandatory)][string]$CoverProfile,
    [Parameter(Mandatory)][string]$CoverDir
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Write-Err([string]$msg) { [Console]::Error.WriteLine($msg) }

# Split a whole-module `go test -coverprofile` file into one fragment per source
# file so each R1 (dead-code) reviewer can be fed ONLY its file's coverage via a
# content-addressed file.ref. The reviewer then re-runs iff its fragment's bytes
# change, and skips (cache hit) when they do not. To make that stable, each
# fragment's block lines are SORTED: an unchanged file yields byte-identical
# output even if `go test` emits the profile in a different order between runs.

if (-not (Test-Path -LiteralPath $CoverProfile)) {
    # A build/test failure upstream can leave no profile. Emit an empty result
    # rather than failing: the reviewers will see absent fragments and treat the
    # file as having no coverage signal.
    [pscustomobject]@{ coverDir = $CoverDir; files = 0 } | ConvertTo-Json -Compress
    return
}

# Coverage lines name files by import path (module + '/' + repo-relative path).
# Strip the module prefix to recover the repo-relative path the reviewer keys on.
$modLine = Get-Content -LiteralPath (Join-Path $RepoRoot 'go.mod') |
    Where-Object { $_ -match '^module\s+' } | Select-Object -First 1
if (-not $modLine) { Write-Err "split-coverage: no module line in go.mod"; exit 1 }
$module = ($modLine -replace '^module\s+', '').Trim()

$lines = @(Get-Content -LiteralPath $CoverProfile)
if ($lines.Count -eq 0) {
    [pscustomobject]@{ coverDir = $CoverDir; files = 0 } | ConvertTo-Json -Compress
    return
}
$mode = $lines[0]  # e.g. "mode: set"

# Group block lines by their source file.
$byFile = @{}
foreach ($line in ($lines | Select-Object -Skip 1)) {
    if ($line -match '^(?<f>.+?\.go):\d') {
        $imp = $Matches['f']
        $rel = $imp
        if ($imp.StartsWith("$module/")) { $rel = $imp.Substring($module.Length + 1) }
        if (-not $byFile.ContainsKey($rel)) {
            $byFile[$rel] = [System.Collections.Generic.List[string]]::new()
        }
        $byFile[$rel].Add($line)
    }
}

$count = 0
foreach ($rel in $byFile.Keys) {
    $frag = "$CoverDir/$rel.cover"
    $dir = Split-Path -Parent $frag
    $null = New-Item -ItemType Directory -Force -Path $dir
    # `mode:` header + this file's blocks, sorted for byte-stability.
    $content = @($mode) + (@($byFile[$rel]) | Sort-Object)
    Set-Content -LiteralPath $frag -Value ($content -join "`n") -Encoding ascii
    $count++
}

[pscustomobject]@{ coverDir = $CoverDir; files = $count } | ConvertTo-Json -Compress
