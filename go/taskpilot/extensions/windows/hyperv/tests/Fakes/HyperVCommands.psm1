function Get-VM { param($Name, $ErrorAction) }
function Get-VMNetworkAdapter { param($VMName, $ErrorAction) }
function Connect-VMNetworkAdapter { param($VMName, $SwitchName, $ErrorAction) }
function Get-VMIntegrationService { param($VMName, $Name, $ErrorAction) }
function Enable-VMIntegrationService { param($VMName, $Name, $ErrorAction) }
function Start-VM { param($Name, $ErrorAction) }
function Stop-VM { param($Name, [switch]$TurnOff, [switch]$Force, $ErrorAction) }
function Remove-VM { param($Name, [switch]$Force, $ErrorAction) }
function Get-VMHardDiskDrive { param($VMName, $ErrorAction) }
function New-VHD { param($Path, $ParentPath, [switch]$Differencing, $ErrorAction) }
function New-VM { param($Name, $Path, $MemoryStartupBytes, $VHDPath, $Generation, $SwitchName, $ErrorAction) }
function Set-VM { param($Name, $ProcessorCount, $AutomaticCheckpointsEnabled, $ErrorAction) }
function Set-VMFirmware { param($VMName, $EnableSecureBoot, $ErrorAction) }
function Get-VMSnapshot { param($VMName, $Name, $ErrorAction) }
function Checkpoint-VM { param($Name, $SnapshotName, $ErrorAction) }
function Remove-VMSnapshot { param($VMSnapshot, $ErrorAction) }
function Restore-VMSnapshot { param($VMSnapshot, [switch]$Confirm, $ErrorAction) }
function Get-NetIPAddress { param($InterfaceAlias, $AddressFamily, $ErrorAction) }
function Get-NetNeighbor { param($IPAddress, $ErrorAction) }
function Test-WSMan { param($ComputerName, $Port, $Authentication, $Credential, $ErrorAction) }
function Invoke-Command { param($Session, $ScriptBlock, $ArgumentList, [switch]$AsJob, $ErrorAction) }
function Remove-PSSession { param($Session, $ErrorAction) }

Export-ModuleMember -Function *
