#Requires -Version 7.4
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$RepoRoot,
    [Parameter(Mandatory)][string]$RunNonce
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Write-Err([string]$msg) { [Console]::Error.WriteLine($msg) }

# Every tool the workflow or its prompts invoke must be present. A missing tool
# is a fast, loud failure, never a silent degradation: go = build/test gate,
# git = commit/reset sandbox, rg = the R2/R6 reviewers' grep, gocyclo +
# staticcheck = the probe's complexity and dead-code metrics.
$required = @('go', 'git', 'rg', 'gocyclo', 'staticcheck')
$missingRequired = @($required | Where-Object { -not (Get-Command $_ -ErrorAction SilentlyContinue) })
if ($missingRequired.Count -gt 0) {
    Write-Err ("preflight: missing required tools: {0}" -f ($missingRequired -join ', '))
    Write-Err "install hint: go install github.com/fzipp/gocyclo/cmd/gocyclo@latest ; go install honnef.co/go/tools/cmd/staticcheck@latest"
    exit 1
}

# A dirty tree would make commit-per-candidate and reset --hard unsafe.
$status = (& git -C $RepoRoot status --porcelain) 2>&1
if ($LASTEXITCODE -ne 0) { Write-Err "preflight: 'git status' failed"; exit 1 }
if (-not [string]::IsNullOrWhiteSpace(($status | Out-String))) {
    Write-Err "preflight: working tree is not clean; commit or stash first"
    exit 1
}

# Dedicated branch so commit/reset only ever touch this run's work.
$branch = "selfreview/$RunNonce"
$null = (& git -C $RepoRoot checkout -B $branch) 2>&1
if ($LASTEXITCODE -ne 0) { Write-Err "preflight: failed to create branch $branch"; exit 1 }

$runDir = Join-Path $RepoRoot ".tmp/selfreview/$RunNonce"
$issues = Join-Path $runDir 'issues'
$null = New-Item -ItemType Directory -Force -Path $issues

# The decision log is cross-run (the reversal-check reads prior entries), so it
# lives outside the per-run dir.
$logPath = Join-Path $RepoRoot ".tmp/selfreview/applied-changes.md"
if (-not (Test-Path -LiteralPath $logPath)) {
    Set-Content -LiteralPath $logPath -Value "# taskpilot self-review -- applied changes (chronological)`n" -Encoding ascii
}

$out = [ordered]@{
    ok      = $true
    branch  = $branch
    runDir  = $runDir
    logPath = $logPath
}
[Console]::Out.WriteLine(($out | ConvertTo-Json -Depth 6 -Compress))
