#Requires -Version 7.4
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$RepoRoot,
    [Parameter(Mandatory)][string]$RunDir,
    [Parameter(Mandatory)][string]$Label
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Write-Err([string]$msg) { [Console]::Error.WriteLine($msg) }

$null = New-Item -ItemType Directory -Force -Path $RunDir

# --- Hard gates: build + test ---
$null = (& go build ./... ) 2>&1
$buildOk = ($LASTEXITCODE -eq 0)

# The coverage profile path is interpolated into the R1 reviewer prompt
# ({{coverProfile}}), which is cached on the reviewer's inputs. Writing it under
# the per-run $RunDir would bake the run nonce into every reviewer prompt and
# defeat cross-run caching of those (expensive) copilot.invoke calls. Keying it
# by label under the stable parent dir (.tmp/selfreview/<label>.cover.out)
# instead keeps the prompt identical across runs when the source is unchanged.
# Runs are sequential (preflight requires a clean tree), so overwriting a shared
# profile between runs is safe.
$coverDir = Split-Path -Parent $RunDir
$cover = Join-Path $coverDir "$Label.cover.out"
$null = (& go test ./... "-coverprofile=$cover") 2>&1
$testOk = ($LASTEXITCODE -eq 0)

# Covered statements (sum of numStmt where the trailing hit flag is nonzero;
# default `set` mode records 0/1). Robust to a missing profile (e.g. a build
# failure leaves none).
$coveredLines = 0
if (Test-Path -LiteralPath $cover) {
    foreach ($line in Get-Content -LiteralPath $cover) {
        if ($line -match ' (\d+) (\d+)$') {
            if ([int]$Matches[2] -gt 0) { $coveredLines += [int]$Matches[1] }
        }
    }
}

# Test count via -list (names print one per line, plus per-package summary lines).
$testCount = 0
$list = (& go test ./... -list '.*') 2>&1
foreach ($line in $list) {
    if ("$line" -match '^(Test|Benchmark|Example|Fuzz)\w*$') { $testCount++ }
}

# go vet diagnostics (file:line: ...). Advisory.
$vetCount = 0
$vet = (& go vet ./... ) 2>&1
foreach ($line in $vet) { if ("$line" -match ':\d+:\d*:') { $vetCount++ } }

# --- Proxy metrics. Preflight guarantees rg/gocyclo/staticcheck are present. ---
$patterns = @(
    '^func [A-Z]',
    '^func \([^)]*\) [A-Z]',
    '^type [A-Z]',
    '^const [A-Z]',
    '^var [A-Z]'
)
$exportedCount = 0
foreach ($p in $patterns) {
    $c = (& rg --no-messages -t go -c -- $p $RepoRoot) 2>&1
    foreach ($line in $c) { if ("$line" -match ':(\d+)$') { $exportedCount += [int]$Matches[1] } }
}
$globalVarCount = 0
$gv = (& rg --no-messages -t go -c -- '^var ' $RepoRoot) 2>&1
foreach ($line in $gv) { if ("$line" -match ':(\d+)$') { $globalVarCount += [int]$Matches[1] } }

$g = (& gocyclo -over 15 ./...) 2>&1
$gocycloOver = @($g | Where-Object { "$_" -match '\S' }).Count

$sc = (& staticcheck ./...) 2>&1
$deadcodeCount = @($sc | Where-Object { "$_" -match 'U1000' }).Count

$outFile = Join-Path $RunDir "$Label.json"
$metrics = [ordered]@{
    buildOk        = $buildOk
    testOk         = $testOk
    coveredLines   = $coveredLines
    coverProfile   = $cover
    testCount      = $testCount
    vetCount       = $vetCount
    exportedCount  = $exportedCount
    gocycloOver    = $gocycloOver
    deadcodeCount  = $deadcodeCount
    globalVarCount = $globalVarCount
    outFile        = $outFile
}
$json = $metrics | ConvertTo-Json -Depth 6 -Compress
Set-Content -LiteralPath $outFile -Value $json -Encoding ascii
[Console]::Out.WriteLine($json)
