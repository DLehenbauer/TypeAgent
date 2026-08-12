Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

BeforeAll {
    Import-Module "$PSScriptRoot\Fakes\HyperVCommands.psm1" -Force
    Get-Module guest | Remove-Module -Force -ErrorAction SilentlyContinue
    $module = Import-Module (Join-Path (Split-Path $PSScriptRoot -Parent) 'guest.psm1') -Force -PassThru
    $moduleName = $module.Name

    function New-HyperVTestCredential {
        [pscredential]::new('guest\user', (ConvertTo-SecureString 'secret' -AsPlainText -Force))
    }
}

Describe 'Hyper-V guest address and readiness' {
    BeforeEach {
        Mock Get-VM {
            [pscustomobject]@{
                Name = $Name
                State = 'Running'
                Heartbeat = 'OkApplicationsHealthy'
                Uptime = [TimeSpan]::FromMinutes(2)
            }
        } -ModuleName $moduleName
        Mock Test-Connection { $true } -ModuleName $moduleName
        Mock Get-NetNeighbor { @() } -ModuleName $moduleName
        Mock Test-HyperVTcpPort { $true } -ModuleName $moduleName
        Mock Add-HyperVTrustedHost {} -ModuleName $moduleName
        Mock Invoke-HyperVBounded { 'WSMan 1.0' } -ModuleName $moduleName
        Mock New-HyperVWinRMSession { [pscustomobject]@{ ComputerName = $ComputerName } } -ModuleName $moduleName
        Mock Remove-PSSession {} -ModuleName $moduleName
    }

    It 'uses only addresses reported by the leased VM adapters' {
        Mock Get-VMNetworkAdapter {
            @([pscustomobject]@{ IPAddresses = @(); SwitchName = 'Switch' })
        } -ModuleName $moduleName
        Mock Get-NetNeighbor {
            @([pscustomobject]@{ IPAddress = '192.168.1.9'; State = 'Reachable' })
        } -ModuleName $moduleName

        Resolve-HyperVGuestIPv4 -VMName 'vm1' | Should -BeNullOrEmpty
        Should -Invoke Get-NetNeighbor -ModuleName $moduleName -Times 0 -Exactly
    }

    It 'becomes ready only after the endpoint UUID matches the VM firmware GUID' {
        Mock Invoke-Command {
            [pscustomobject]@{ Smoke = 2; Uuid = 'AAAAAAAA-0000-0000-0000-000000000001'; Name = 'GUEST1' }
        } -ModuleName $moduleName
        Mock Get-HyperVVMBiosGuid { 'AAAAAAAA-0000-0000-0000-000000000001' } -ModuleName $moduleName

        $state = Get-HyperVGuestReadiness -VMName 'vm1' -Credential (New-HyperVTestCredential) `
            -KnownIP '10.0.0.5' -ConnectionTimeoutSec 1

        $state.Ready | Should -BeTrue
        $state.HighestPassed | Should -Be 'Identity'
    }

    It 'rejects a responsive endpoint belonging to another VM' {
        Mock Invoke-Command {
            [pscustomobject]@{ Smoke = 2; Uuid = 'BBBBBBBB-0000-0000-0000-000000000002'; Name = 'OTHER' }
        } -ModuleName $moduleName
        Mock Get-HyperVVMBiosGuid { 'AAAAAAAA-0000-0000-0000-000000000001' } -ModuleName $moduleName

        $state = Get-HyperVGuestReadiness -VMName 'vm1' -Credential (New-HyperVTestCredential) `
            -KnownIP '10.0.0.5' -ConnectionTimeoutSec 1

        $state.Ready | Should -BeFalse
        $state.FirstFailed | Should -Be 'Identity'
        $state.FailureReason | Should -BeLike '*OTHER*'
    }
}

Describe 'Hyper-V guest reboot proof' {
    It 'does not accept readiness on the prior boot and waits for a newer boot time' {
        $t0 = ([datetime]'2026-01-01T00:00:00Z').ToUniversalTime()
        $script:attempt = 0
        Mock Get-HyperVGuestReadiness {
            [pscustomobject]@{ Ready = $true; Host = '10.0.0.5' }
        } -ModuleName $moduleName
        Mock Get-HyperVGuestBootTime {
            $script:attempt++
            if ($script:attempt -eq 1) { $t0 } else { $t0.AddMinutes(5) }
        } -ModuleName $moduleName
        Mock Start-Sleep {} -ModuleName $moduleName

        $result = Wait-HyperVGuestReboot -VMName 'vm1' -Credential (New-HyperVTestCredential) `
            -PriorBootTime $t0 -TimeoutSeconds 10 -PollIntervalSeconds 1 -ConnectionTimeoutSec 1

        $result.Ready | Should -BeTrue
        $result.Attempts | Should -Be 2
        $result.BootTimeUtc | Should -Be ($t0.AddMinutes(5))
    }

    It 'issues restart through a mocked guest session' {
        Mock Wait-HyperVGuestReady {
            [pscustomobject]@{ Host = '10.0.0.5' }
        } -ModuleName $moduleName
        Mock New-HyperVWinRMSession { [pscustomobject]@{ Id = 1 } } -ModuleName $moduleName
        Mock Invoke-Command { [pscustomobject]@{ State = 'Running' } } -ModuleName $moduleName
        Mock Start-Sleep {} -ModuleName $moduleName
        Mock Remove-PSSession {} -ModuleName $moduleName

        $result = Restart-HyperVGuest -VMName 'vm1' -Credential (New-HyperVTestCredential) `
            -ComputerName '10.0.0.5' -DelaySeconds 1

        $result.RestartIssued | Should -BeTrue
        Should -Invoke Invoke-Command -ModuleName $moduleName -Times 1 -Exactly
    }
}
