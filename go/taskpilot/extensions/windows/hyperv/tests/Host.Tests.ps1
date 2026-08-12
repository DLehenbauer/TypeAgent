Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

BeforeAll {
    Import-Module "$PSScriptRoot\Fakes\HyperVCommands.psm1" -Force
    Get-Module host | Remove-Module -Force -ErrorAction SilentlyContinue
    $module = Import-Module (Join-Path (Split-Path $PSScriptRoot -Parent) 'host.psm1') -Force -PassThru
    $moduleName = $module.Name
}

Describe 'Hyper-V VM lifecycle' {
    BeforeEach {
        $global:HyperVTestCreated = $false
        $global:HyperVTestExisting = $null
        Mock Test-Path { $true } -ModuleName $moduleName
        Mock New-Item {} -ModuleName $moduleName
        Mock Remove-Item {} -ModuleName $moduleName
        Mock New-VHD {} -ModuleName $moduleName
        Mock New-VM { $global:HyperVTestCreated = $true } -ModuleName $moduleName
        Mock Set-VM {} -ModuleName $moduleName
        Mock Set-VMFirmware {} -ModuleName $moduleName
        Mock Enable-VMIntegrationService {} -ModuleName $moduleName
        Mock Start-VM {} -ModuleName $moduleName
        Mock Connect-VMNetworkAdapter {} -ModuleName $moduleName
        Mock Get-VMIntegrationService { [pscustomobject]@{ Enabled = $false } } -ModuleName $moduleName
        Mock Get-VMNetworkAdapter { [pscustomobject]@{ SwitchName = 'OldSwitch' } } -ModuleName $moduleName
        Mock Get-VMHardDiskDrive { [pscustomobject]@{ Path = "C:\vhds\$VMName\osdiff.vhdx" } } -ModuleName $moduleName
        Mock Get-VM {
            if ($global:HyperVTestExisting) { return $global:HyperVTestExisting }
            if ($global:HyperVTestCreated) { return [pscustomobject]@{ Name = $Name; State = 'Off' } }
            $null
        } -ModuleName $moduleName
    }

    AfterEach {
        Remove-Variable HyperVTestCreated -Scope Global -ErrorAction SilentlyContinue
        Remove-Variable HyperVTestExisting -Scope Global -ErrorAction SilentlyContinue
    }

    It 'creates a differencing-disk VM with requested hardware settings' {
        $result = New-HyperVManagedVM -VMName 'vm1' -BaseVhdxPath 'C:\golden.vhdx' `
            -VmRoot 'C:\vms' -VhdRoot 'C:\vhds' -SwitchName 'Switch' `
            -MemoryMB 2048 -ProcessorCount 4 -Generation 2 -DisableSecureBoot

        $result.Reused | Should -BeFalse
        Should -Invoke New-VHD -ModuleName $moduleName -Times 1 -Exactly -ParameterFilter {
            $ParentPath -eq 'C:\golden.vhdx' -and $Differencing
        }
        Should -Invoke Set-VMFirmware -ModuleName $moduleName -Times 1 -Exactly
    }

    It 'reuses and starts an existing VM without creating disks' {
        $global:HyperVTestExisting = [pscustomobject]@{ Name = 'vm1'; State = 'Off' }
        $result = New-HyperVManagedVM -VMName 'vm1' -BaseVhdxPath 'C:\golden.vhdx' `
            -VmRoot 'C:\vms' -VhdRoot 'C:\vhds' -SwitchName 'Switch' -Start

        $result.Reused | Should -BeTrue
        Should -Invoke New-VHD -ModuleName $moduleName -Times 0 -Exactly
        Should -Invoke Connect-VMNetworkAdapter -ModuleName $moduleName -Times 1 -Exactly
        Should -Invoke Start-VM -ModuleName $moduleName -Times 1 -Exactly
    }

    It 'treats removal of an absent VM as successful no-op' {
        Mock Get-VM { $null } -ModuleName $moduleName
        Mock Remove-VM {} -ModuleName $moduleName

        $result = Remove-HyperVManagedVM -VMName 'missing'

        $result.Removed | Should -BeFalse
        Should -Invoke Remove-VM -ModuleName $moduleName -Times 0 -Exactly
    }

    It 'cleans owned root directories even after the VM is already absent' {
        Mock Get-VM { $null } -ModuleName $moduleName
        Mock Test-Path { $true } -ModuleName $moduleName
        Mock Remove-Item {} -ModuleName $moduleName

        $result = Remove-HyperVManagedVM -VMName 'missing' -VmRoot 'C:\vms' -VhdRoot 'C:\vhds'

        $result.Removed | Should -BeFalse
        Should -Invoke Remove-Item -ModuleName $moduleName -Times 1 -Exactly -ParameterFilter {
            $LiteralPath -eq 'C:\vhds\missing'
        }
        Should -Invoke Remove-Item -ModuleName $moduleName -Times 1 -Exactly -ParameterFilter {
            $LiteralPath -eq 'C:\vms\missing'
        }
    }
}

Describe 'Hyper-V checkpoint lifecycle' {
    It 'creates and observes the requested checkpoint' {
        $script:snapshots = @()
        Mock Get-VMSnapshot {
            if ($Name) { @($script:snapshots | Where-Object Name -eq $Name) } else { $script:snapshots }
        } -ModuleName $moduleName
        Mock Checkpoint-VM {
            $script:snapshots = @([pscustomobject]@{ Name = $SnapshotName })
        } -ModuleName $moduleName

        New-HyperVCheckpoint -VMName 'vm1' -Name 'ready'

        Test-HyperVCheckpoint -VMName 'vm1' -Name 'ready' | Should -BeTrue
        Should -Invoke Checkpoint-VM -ModuleName $moduleName -Times 1 -Exactly
    }

    It 'restores and removes a named checkpoint through mocked commands' {
        Mock Get-VMSnapshot { [pscustomobject]@{ Name = $Name } } -ModuleName $moduleName
        Mock Restore-VMSnapshot {} -ModuleName $moduleName
        Mock Remove-VMSnapshot {} -ModuleName $moduleName
        Mock Get-VM { [pscustomobject]@{ Name = $Name; State = 'Off' } } -ModuleName $moduleName

        Restore-HyperVCheckpoint -VMName 'vm1' -Name 'ready' | Out-Null
        $removed = Remove-HyperVCheckpoint -VMName 'vm1' -Name 'ready'

        $removed.Removed | Should -BeTrue
        Should -Invoke Restore-VMSnapshot -ModuleName $moduleName -Times 1 -Exactly
        Should -Invoke Remove-VMSnapshot -ModuleName $moduleName -Times 1 -Exactly
    }
}
