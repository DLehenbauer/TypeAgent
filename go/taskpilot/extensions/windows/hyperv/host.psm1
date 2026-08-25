Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'retry.ps1')

function New-HyperVManagedVM {
    [CmdletBinding()]
    [OutputType([pscustomobject])]
    param(
        [Parameter(Mandatory)][string]$VMName,
        [Parameter(Mandatory)][string]$BaseVhdxPath,
        [Parameter(Mandatory)][string]$VmRoot,
        [Parameter(Mandatory)][string]$VhdRoot,
        [Parameter(Mandatory)][string]$SwitchName,
        [int]$MemoryMB = 4096,
        [int]$ProcessorCount = 2,
        [ValidateSet(1, 2)][int]$Generation = 1,
        [switch]$DisableSecureBoot,
        [switch]$Start
    )

    $existingVM = Get-VM -Name $VMName -ErrorAction SilentlyContinue
    if ($existingVM) {
        $adapter = @(Get-VMNetworkAdapter -VMName $VMName -ErrorAction SilentlyContinue | Select-Object -First 1)
        if ($adapter.Count -gt 0 -and $adapter[0].SwitchName -ne $SwitchName) {
            Connect-VMNetworkAdapter -VMName $VMName -SwitchName $SwitchName -ErrorAction Stop
        }

        $gsi = Get-VMIntegrationService -VMName $VMName -Name 'Guest Service Interface' -ErrorAction SilentlyContinue
        if ($gsi -and -not $gsi.Enabled) {
            Enable-VMIntegrationService -VMName $VMName -Name 'Guest Service Interface' -ErrorAction Stop | Out-Null
        }

        if ($Start -and $existingVM.State -eq 'Off') {
            Start-VM -Name $VMName -ErrorAction Stop | Out-Null
        }

        $currentVM = Get-VM -Name $VMName -ErrorAction Stop
        return [pscustomobject][ordered]@{
            VMName       = $VMName
            Reused       = $true
            State        = $currentVM.State.ToString()
            DiffDiskPath = @((Get-VMHardDiskDrive -VMName $VMName -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Path))
            SwitchName   = $SwitchName
        }
    }

    if (-not (Test-Path -LiteralPath $BaseVhdxPath -PathType Leaf)) {
        throw "Base VHDX not found: '$BaseVhdxPath'."
    }
    if (-not (Test-Path -LiteralPath $VmRoot -PathType Container)) {
        New-Item -Path $VmRoot -ItemType Directory -Force | Out-Null
    }
    if (-not (Test-Path -LiteralPath $VhdRoot -PathType Container)) {
        New-Item -Path $VhdRoot -ItemType Directory -Force | Out-Null
    }

    $vhdDir = Join-Path $VhdRoot $VMName
    $vmDir = Join-Path $VmRoot $VMName
    $diffDiskPath = Join-Path $vhdDir 'osdiff.vhdx'
    $vmCreated = $false

    try {
        if (Test-Path -LiteralPath $vhdDir) {
            Remove-Item -LiteralPath $vhdDir -Recurse -Force -ErrorAction Stop
        }
        New-Item -Path $vhdDir -ItemType Directory -Force -ErrorAction Stop | Out-Null
        New-VHD -Path $diffDiskPath -ParentPath $BaseVhdxPath -Differencing -ErrorAction Stop | Out-Null

        if (Test-Path -LiteralPath $vmDir) {
            Remove-Item -LiteralPath $vmDir -Recurse -Force -ErrorAction Stop
        }

        $memoryBytes = [int64]$MemoryMB * 1MB
        New-VM -Name $VMName `
            -Path $VmRoot `
            -MemoryStartupBytes $memoryBytes `
            -VHDPath $diffDiskPath `
            -Generation $Generation `
            -SwitchName $SwitchName `
            -ErrorAction Stop | Out-Null
        $vmCreated = $true

        Set-VM -Name $VMName -ProcessorCount $ProcessorCount -ErrorAction Stop | Out-Null
        # Automatic checkpoints interleave Hyper-V-named snapshots into a chain
        # whose names are content-addressed node IDs, so they are noise the
        # lookup and the sweep both have to step around.
        try { Set-VM -Name $VMName -AutomaticCheckpointsEnabled $false -ErrorAction Stop | Out-Null } catch { }

        if ($Generation -eq 2 -and $DisableSecureBoot) {
            Set-VMFirmware -VMName $VMName -EnableSecureBoot Off -ErrorAction Stop
        }

        Enable-VMIntegrationService -VMName $VMName -Name 'Guest Service Interface' -ErrorAction Stop | Out-Null

        if ($Start) {
            Start-VM -Name $VMName -ErrorAction Stop | Out-Null
        }

        $createdVM = Get-VM -Name $VMName -ErrorAction Stop
        [pscustomobject][ordered]@{
            VMName         = $VMName
            Reused         = $false
            State          = $createdVM.State.ToString()
            VMRoot         = $VmRoot
            VhdRoot        = $VhdRoot
            DiffDiskPath   = $diffDiskPath
            SwitchName     = $SwitchName
            MemoryMB       = $MemoryMB
            ProcessorCount = $ProcessorCount
            Generation     = $Generation
        }
    }
    catch {
        $original = $_
        if ($vmCreated) {
            $runningVM = Get-VM -Name $VMName -ErrorAction SilentlyContinue
            if ($runningVM -and $runningVM.State -ne 'Off') {
                Stop-VM -Name $VMName -TurnOff -Force -ErrorAction SilentlyContinue
            }
            Remove-VM -Name $VMName -Force -ErrorAction SilentlyContinue
        }
        if (Test-Path -LiteralPath $vmDir -ErrorAction SilentlyContinue) {
            Remove-Item -LiteralPath $vmDir -Recurse -Force -ErrorAction SilentlyContinue
        }
        if (Test-Path -LiteralPath $vhdDir -ErrorAction SilentlyContinue) {
            Remove-Item -LiteralPath $vhdDir -Recurse -Force -ErrorAction SilentlyContinue
        }
        throw $original
    }
}

function Get-HyperVCheckpoint {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$VMName,
        [Parameter(Mandatory)][string]$Name
    )

    Get-VMSnapshot -VMName $VMName -Name $Name -ErrorAction SilentlyContinue
}

function Test-HyperVCheckpoint {
    [CmdletBinding()]
    [OutputType([bool])]
    param(
        [Parameter(Mandatory)][string]$VMName,
        [Parameter(Mandatory)][string]$Name
    )

    $null -ne (Get-VMSnapshot -VMName $VMName -Name $Name -ErrorAction SilentlyContinue)
}

function New-HyperVCheckpoint {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$VMName,
        [Parameter(Mandatory)][string]$Name,
        [int]$TimeoutSeconds = 180,
        [switch]$Force
    )

    $existing = Get-HyperVCheckpoint -VMName $VMName -Name $Name
    if ($existing) {
        if (-not $Force) { throw "Checkpoint '$Name' already exists on VM '$VMName'." }
        Remove-VMSnapshot -VMSnapshot $existing -ErrorAction Stop | Out-Null
    }

    # Checkpoint-VM can fail while Hyper-V is asynchronously applying the
    # requested name. One retry operation owns an entire attempt: invoke
    # Checkpoint-VM, wait briefly for a late rename, and compensate by removing
    # any stray default-named snapshot before the next attempt.
    try {
        Invoke-WithRetry `
            -OperationName "create checkpoint '$Name' on VM '$VMName'" `
            -TimeoutSeconds $TimeoutSeconds `
            -BaseDelaySeconds 5 `
            -MaxDelaySeconds 5 `
            -ScriptBlock {
                $before = @(Get-VMSnapshot -VMName $VMName -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Name)
                $checkpointError = $null
                try {
                    Checkpoint-VM -Name $VMName -SnapshotName $Name -ErrorAction Stop
                }
                catch {
                    $checkpointError = $_.Exception
                }

                try {
                    Wait-Until `
                        -OperationName "checkpoint '$Name' rename" `
                        -TimeoutSeconds 20 `
                        -PollIntervalSeconds 2 `
                        -MaxRetries 9 `
                        -Probe { Get-HyperVCheckpoint -VMName $VMName -Name $Name } `
                        -Until { param($snapshot) $null -ne $snapshot } `
                        -DescribeResult { "checkpoint '$Name' has not appeared yet" } |
                        Out-Null
                    return
                }
                catch {
                    foreach ($stray in @(Get-VMSnapshot -VMName $VMName -ErrorAction SilentlyContinue | Where-Object { $_.Name -notin $before })) {
                        Remove-VMSnapshot -VMSnapshot $stray -ErrorAction SilentlyContinue | Out-Null
                    }
                    if ($checkpointError) { throw $checkpointError }
                    throw
                }
            } |
            Out-Null
    }
    catch {
        throw "Checkpoint '$Name' could not be created on VM '$VMName' within ${TimeoutSeconds}s. Last error: $($_.Exception.Message)"
    }
}

function Restore-HyperVCheckpoint {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$VMName,
        [Parameter(Mandatory)][string]$Name,
        [string]$SwitchName
    )

    $snapshot = Get-HyperVCheckpoint -VMName $VMName -Name $Name
    if (-not $snapshot) { throw "Checkpoint '$Name' was not found on VM '$VMName'." }
    Restore-VMSnapshot -VMSnapshot $snapshot -Confirm:$false -ErrorAction Stop | Out-Null
    if ($SwitchName) {
        $adapter = Get-VMNetworkAdapter -VMName $VMName -ErrorAction Stop | Select-Object -First 1
        if (-not $adapter) { throw "VM '$VMName' has no network adapter after restoring checkpoint '$Name'." }
        if ($adapter.SwitchName -ne $SwitchName) {
            Connect-VMNetworkAdapter -VMNetworkAdapter $adapter -SwitchName $SwitchName -ErrorAction Stop
        }
    }
    Get-VM -Name $VMName -ErrorAction Stop
}

function Remove-HyperVCheckpoint {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$VMName,
        [Parameter(Mandatory)][string]$Name,
        [switch]$IgnoreMissing
    )

    $snapshot = Get-HyperVCheckpoint -VMName $VMName -Name $Name
    if (-not $snapshot) {
        if ($IgnoreMissing) { return $null }
        throw "Checkpoint '$Name' was not found on VM '$VMName'."
    }

    Remove-VMSnapshot -VMSnapshot $snapshot -ErrorAction Stop | Out-Null
    [pscustomobject][ordered]@{ VMName = $VMName; Name = $Name; Removed = $true }
}

function Remove-HyperVManagedVM {
    [CmdletBinding()]
    [OutputType([pscustomobject])]
    param(
        [Parameter(Mandatory)][string]$VMName,
        [string]$VmRoot,
        [string]$VhdRoot
    )

    $vm = Get-VM -Name $VMName -ErrorAction SilentlyContinue
    $ownedVhdPath = if ($VhdRoot) { Join-Path $VhdRoot $VMName } else { $null }
    $ownedVmPath = if ($VmRoot) { Join-Path $VmRoot $VMName } else { $null }
    if (-not $vm) {
        $removedDisks = [System.Collections.Generic.List[string]]::new()
        if ($ownedVhdPath -and (Test-Path -LiteralPath $ownedVhdPath -PathType Container)) {
            Remove-Item -LiteralPath $ownedVhdPath -Recurse -Force -ErrorAction Stop
            $removedDisks.Add($ownedVhdPath)
        }
        if ($ownedVmPath -and (Test-Path -LiteralPath $ownedVmPath -PathType Container)) {
            Remove-Item -LiteralPath $ownedVmPath -Recurse -Force -ErrorAction Stop
        }
        return [pscustomobject][ordered]@{
            VMName       = $VMName
            Removed      = $false
            RemovedDisks = $removedDisks.ToArray()
        }
    }
    $configurationPath = if ($vm.PSObject.Properties['Path']) { [string]$vm.Path } else { $null }
    $vhdPaths = @((Get-VMHardDiskDrive -VMName $VMName -ErrorAction SilentlyContinue | Where-Object { $_.Path } | Select-Object -ExpandProperty Path))

    if ($vm.State -ne 'Off') {
        Stop-VM -Name $VMName -TurnOff -Force -ErrorAction Stop
    }

    Remove-VM -Name $VMName -Force -ErrorAction Stop

    $removedDisks = [System.Collections.Generic.List[string]]::new()
    foreach ($vhdPath in $vhdPaths) {
        if (-not (Test-Path -LiteralPath $vhdPath -ErrorAction SilentlyContinue)) { continue }

        $leaf = Split-Path -Path $vhdPath -Leaf
        $parent = Split-Path -Path $vhdPath -Parent
        if ($leaf -eq 'osdiff.vhdx' -and $parent -and (Test-Path -LiteralPath $parent -PathType Container)) {
            Remove-Item -LiteralPath $parent -Recurse -Force -ErrorAction Stop
            $removedDisks.Add($parent)
        } else {
            Remove-Item -LiteralPath $vhdPath -Force -ErrorAction Stop
            $removedDisks.Add($vhdPath)
        }
    }
    if ($ownedVhdPath -and (Test-Path -LiteralPath $ownedVhdPath -PathType Container)) {
        Remove-Item -LiteralPath $ownedVhdPath -Recurse -Force -ErrorAction Stop
        if ($ownedVhdPath -notin $removedDisks) { $removedDisks.Add($ownedVhdPath) }
    }

    if ($configurationPath -and
        (Split-Path -Path $configurationPath -Leaf) -eq $VMName -and
        (Test-Path -LiteralPath $configurationPath -PathType Container)) {
        Remove-Item -LiteralPath $configurationPath -Recurse -Force -ErrorAction Stop
    }
    if ($ownedVmPath -and
        $ownedVmPath -ne $configurationPath -and
        (Test-Path -LiteralPath $ownedVmPath -PathType Container)) {
        Remove-Item -LiteralPath $ownedVmPath -Recurse -Force -ErrorAction Stop
    }

    [pscustomobject][ordered]@{
        VMName       = $VMName
        Removed      = $true
        RemovedDisks = $removedDisks.ToArray()
    }
}

Export-ModuleMember -Function @(
    'New-HyperVManagedVM',
    'Get-HyperVCheckpoint',
    'Test-HyperVCheckpoint',
    'New-HyperVCheckpoint',
    'Restore-HyperVCheckpoint',
    'Remove-HyperVCheckpoint',
    'Remove-HyperVManagedVM'
)
