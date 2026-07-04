#requires -Version 7
<#
.SYNOPSIS
    Locate a taskpilot run log and surface the spans that failed, root cause first.

.DESCRIPTION
    taskpilot writes one JSONL trace file per run to <stateDir>/logs/<runID>.jsonl.
    Each line is a telemetry.SpanRecord with phase "start" or "end". A failed run
    has one or more "end" records with status "error". The deepest/earliest
    erroring node span is the root cause; ancestor spans (the node's parents and
    the top-level run span) repeat the same statusMessage as the failure
    propagates upward.

    This script asks the tp binary for the logs directory (`tp
    paths`) so state-dir resolution lives only in cache.ResolveStateDir, picks
    the most recent run (or a run you name), and prints the error spans ordered
    by endTime so the first row is the root cause.

.PARAMETER RunId
    Specific run id (with or without the "run-" prefix and ".jsonl" suffix). When
    omitted, the most recently modified .jsonl in the logs directory is used.

.PARAMETER LogsDir
    Override the logs directory. Defaults to the resolved <stateDir>/logs.

.PARAMETER All
    Print every span (not just errors) ordered by start time, for full context.

.EXAMPLE
    ./Find-FailedRun.ps1
    Diagnose the latest run.

.EXAMPLE
    ./Find-FailedRun.ps1 -RunId run-7dd8511272d6c6f16091e507ecf7ef61
    Diagnose a specific run.
#>
[CmdletBinding()]
param(
    [string] $RunId,
    [string] $LogsDir,
    [switch] $All
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Resolve-LogsDir {
    param([string] $Override)
    if ($Override) { return $Override }

    # Delegate to the tp binary so state-dir resolution lives in exactly
    # one place (cache.ResolveStateDir). `tp paths` prints the resolved
    # locations as "<name><tab><dir>" lines; we consume the "logs" line.
    $lines = & tp paths
    if ($LASTEXITCODE -ne 0) {
        throw "could not resolve logs directory: 'tp paths' exited $LASTEXITCODE"
    }
    foreach ($line in $lines) {
        $parts = $line -split "`t", 2
        if ($parts.Length -eq 2 -and $parts[0] -eq 'logs') {
            return $parts[1]
        }
    }
    throw "'tp paths' did not report a logs directory"
}

$resolvedLogsDir = Resolve-LogsDir -Override $LogsDir
if (-not (Test-Path -LiteralPath $resolvedLogsDir)) {
    throw "logs directory not found: $resolvedLogsDir"
}

if ($RunId) {
    $id = $RunId -replace '\.jsonl$', ''
    if ($id -notmatch '^run-') { $id = "run-$id" }
    $logFile = Get-Item -LiteralPath (Join-Path $resolvedLogsDir "$id.jsonl")
}
else {
    $logFile = Get-ChildItem -LiteralPath $resolvedLogsDir -Filter '*.jsonl' |
        Sort-Object LastWriteTime -Descending |
        Select-Object -First 1
    if (-not $logFile) { throw "no .jsonl logs found in $resolvedLogsDir" }
}

$runIdResolved = [System.IO.Path]::GetFileNameWithoutExtension($logFile.Name)
Write-Host "run:   $runIdResolved" -ForegroundColor Cyan
Write-Host "log:   $($logFile.FullName)" -ForegroundColor Cyan
Write-Host " pretty render: tp log $runIdResolved" -ForegroundColor DarkGray
Write-Host ''

$records = Get-Content -LiteralPath $logFile.FullName |
    Where-Object { $_.Trim() } |
    ForEach-Object { $_ | ConvertFrom-Json }

# Safely read a property that may be absent. ConvertFrom-Json objects under
# Set-StrictMode throw on missing members, so go through PSObject.Properties.
function Get-Prop {
    param($obj, [string] $Name)
    if ($null -eq $obj) { return $null }
    $p = $obj.PSObject.Properties[$Name]
    if ($p) { return $p.Value }
    return $null
}

function Format-Span {
    param($rec)
    $attrs = Get-Prop $rec 'attributes'
    $events = Get-Prop $rec 'events'
    $exception = ($events |
        Where-Object { $_ -and (Get-Prop $_ 'name') -eq 'exception' } |
        ForEach-Object { Get-Prop (Get-Prop $_ 'attributes') 'exception.message' }) -join '; '
    [pscustomobject]@{
        node       = Get-Prop $attrs 'taskpilot.node.name'
        task       = Get-Prop $attrs 'taskpilot.task'
        kind       = Get-Prop $attrs 'taskpilot.span.kind'
        stage      = Get-Prop $attrs 'taskpilot.stage'
        cache      = Get-Prop $attrs 'taskpilot.cache.status'
        durationMs = Get-Prop $rec 'durationMs'
        status     = Get-Prop $rec 'status'
        endTime    = Get-Prop $rec 'endTime'
        error      = Get-Prop $rec 'statusMessage'
        exception  = $exception
    }
}

if ($All) {
    $records |
        Where-Object { $_.phase -eq 'end' } |
        Sort-Object startTime |
        ForEach-Object { Format-Span $_ } |
        Format-Table -AutoSize
    return
}

$errors = $records |
    Where-Object { $_.phase -eq 'end' -and $_.status -eq 'error' } |
    Sort-Object endTime |
    ForEach-Object { Format-Span $_ }

if (-not $errors) {
    Write-Host 'No error spans found. The run may have succeeded or is still in progress.' -ForegroundColor Green
    Write-Host 'Re-run with -All to see every span.' -ForegroundColor DarkGray
    return
}

Write-Host 'ROOT CAUSE (earliest erroring span):' -ForegroundColor Yellow
$errors[0] | Format-List

if ($errors.Count -gt 1) {
    Write-Host 'Propagated failures (parents repeating the same error):' -ForegroundColor DarkGray
    $errors | Select-Object -Skip 1 |
        Format-Table node, task, kind, stage, error -AutoSize
}
