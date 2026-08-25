Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Describe 'Hyper-V driver actions with fake bundle modules' {
    BeforeEach {
        Get-Module host,guest | Remove-Module -Force -ErrorAction SilentlyContinue
        $bundle = Join-Path $TestDrive ([guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Path $bundle | Out-Null
        Copy-Item (Join-Path (Split-Path $PSScriptRoot -Parent) 'driver.ps1') $bundle
        Copy-Item (Join-Path (Split-Path $PSScriptRoot -Parent) 'retry.ps1') $bundle

        @'
function New-HyperVManagedVM { [pscustomobject]@{ Reused = $false } }
function Test-HyperVCheckpoint { $false }
function New-HyperVCheckpoint { [pscustomobject]@{ Name = $Name } }
function Restore-HyperVCheckpoint { param($VMName, $Name, $SwitchName) }
function Remove-HyperVCheckpoint { param($VMName, $Name, [switch]$IgnoreMissing) }
function Remove-HyperVManagedVM { param($VMName, $VmRoot, $VhdRoot) }
Export-ModuleMember -Function *
'@ | Set-Content -LiteralPath (Join-Path $bundle 'host.psm1')

        @'
function Wait-HyperVGuestReady { [pscustomobject]@{ Host = '10.0.0.5' } }
function New-HyperVWinRMSession { [pscustomobject]@{ Host = $ComputerName } }
function Get-HyperVGuestBootTime { [datetime]'2026-01-01T00:00:00Z' }
function Restart-HyperVGuest { [pscustomobject]@{ RestartIssued = $true } }
function Wait-HyperVGuestReboot {
    param($PriorBootTime)
    $global:HyperVDriverPriorBootTime = $PriorBootTime
    [pscustomobject]@{ DurationSec = 3; Ready = $true }
}
Export-ModuleMember -Function *
'@ | Set-Content -LiteralPath (Join-Path $bundle 'guest.psm1')
    }

    AfterEach {
        Get-Module host,guest | Remove-Module -Force -ErrorAction SilentlyContinue
        Remove-Variable HyperVDriverPriorBootTime -Scope Global -ErrorAction SilentlyContinue
    }

    It 'creates the baseline through only bundle-owned fake commands' {
        $payload = @{
            action = 'acquire'
            options = @{
                vmName = 'vm1'
                baseImage = 'C:\base.vhdx'
                vmRoot = 'C:\vms'
                vhdRoot = 'C:\vhds'
                switchName = 'Switch'
                memoryMB = 2048
                processorCount = 2
                generation = 2
                baselineState = 'baseline'
            }
        }
        $payloadPath = Join-Path $bundle 'payload.json'
        $payload | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $payloadPath

        $result = (& (Join-Path $bundle 'driver.ps1') $payloadPath | Select-Object -Last 1) | ConvertFrom-Json

        $result.id | Should -Be 'vm1'
        $result.materializedState | Should -Be 'baseline'
        $result.reused | Should -BeFalse
    }

    It 'proves reboot by passing the captured prior boot time to the wait' {
        $payload = @{
            action = 'rebootGuest'
            id = 'vm1'
            options = @{
                guest = @{
                    username = 'Administrator'
                    password = 'private'
                    winRMPort = 5985
                    readinessTimeoutSeconds = 30
                    connectionTimeoutSeconds = 1
                }
            }
        }
        $payloadPath = Join-Path $bundle 'payload.json'
        $payload | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $payloadPath

        $result = (& (Join-Path $bundle 'driver.ps1') $payloadPath | Select-Object -Last 1) | ConvertFrom-Json

        $result.rebooted | Should -BeTrue
        $result.durationSec | Should -Be 3
        $global:HyperVDriverPriorBootTime | Should -Be ([datetime]'2026-01-01T00:00:00Z')
    }

    It 'retries a transient workspace mapping failure end to end' {
        $payloadPath = Join-Path $bundle 'payload.json'
        @{ action = 'removeVM'; id = 'unused'; options = @{} } |
            ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $payloadPath
        . (Join-Path $bundle 'driver.ps1') $payloadPath | Out-Null
        $script:mapAttempts = 0
        $netUse = {
            param([object[]]$Arguments)
            if ($Arguments -contains '\\host\share') {
                $script:mapAttempts++
                if ($script:mapAttempts -eq 1) {
                    return [pscustomobject]@{ ExitCode = 53; Output = 'System error 53 has occurred.' }
                }
            }
            [pscustomobject]@{ ExitCode = 0; Output = '' }
        }

        Mount-HyperVWorkspace -UncPath '\\host\share' -Drive 'Z:' -Username 'host\worker' `
            -Password 'private' -TimeoutSeconds 5 -DelaySeconds 0 -NetUse $netUse

        $script:mapAttempts | Should -Be 2
    }

    It 'fails fast for a permanent workspace credential error' {
        $payloadPath = Join-Path $bundle 'payload.json'
        @{ action = 'removeVM'; id = 'unused'; options = @{} } |
            ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $payloadPath
        . (Join-Path $bundle 'driver.ps1') $payloadPath | Out-Null
        $script:mapAttempts = 0
        $netUse = {
            param([object[]]$Arguments)
            if ($Arguments -contains '\\host\share') {
                $script:mapAttempts++
                return [pscustomobject]@{ ExitCode = 5; Output = 'System error 5 has occurred.' }
            }
            [pscustomobject]@{ ExitCode = 0; Output = '' }
        }

        {
            Mount-HyperVWorkspace -UncPath '\\host\share' -Drive 'Z:' -Username 'host\worker' `
                -Password 'private' -TimeoutSeconds 5 -DelaySeconds 0 -NetUse $netUse
        } | Should -Throw '*System error 5*'
        $script:mapAttempts | Should -Be 1
    }
}
