Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'retry.ps1')
$script:TrustedHostsTimeoutSec = 15
function Invoke-HyperVBounded {
    param(
        [Parameter(Mandatory)][scriptblock]$ScriptBlock,
        [Parameter(Mandatory)][ValidateRange(1, 3600)][int]$TimeoutSec,
        [object[]]$ArgumentList = @(),
        [string]$OperationName = 'operation'
    )
    $ps = [powershell]::Create()
    $abandoned = $false
    try {
        $null = $ps.AddScript($ScriptBlock.ToString())
        foreach ($arg in $ArgumentList) { $null = $ps.AddArgument($arg) }
        $async = $ps.BeginInvoke()
        if (-not $async.AsyncWaitHandle.WaitOne([TimeSpan]::FromSeconds($TimeoutSec))) {
            $abandoned = $true
            $stuck = $ps
            $null = [System.Threading.Tasks.Task]::Run([Action]{ try { $stuck.Dispose() } catch { } })
            throw [System.TimeoutException]::new("Timed out after ${TimeoutSec}s waiting for $OperationName.")
        }
        try { $result = $ps.EndInvoke($async) }
        catch {
            $ex = $_.Exception
            if ($ex.InnerException) { throw $ex.InnerException }
            throw $ex
        }
        if ($ps.HadErrors -and $ps.Streams.Error.Count -gt 0) { throw $ps.Streams.Error[0].Exception }
        return $result
    }
    finally { if (-not $abandoned) { $ps.Dispose() } }
}
function Test-HyperVTcpPort {
    param([Parameter(Mandatory)][string]$ComputerName, [int]$Port = 5985, [int]$TimeoutMilliseconds = 5000)
    $tcp = $null
    try {
        $tcp = [System.Net.Sockets.TcpClient]::new()
        $ar = $tcp.BeginConnect($ComputerName, $Port, $null, $null)
        if (-not $ar.AsyncWaitHandle.WaitOne($TimeoutMilliseconds, $false)) { return $false }
        $tcp.EndConnect($ar)
        return $true
    }
    catch { return $false }
    finally { if ($tcp) { $tcp.Dispose() } }
}
function Test-HyperVRoutableIPv4 {
    param([AllowNull()][string]$Address)
    if ([string]::IsNullOrWhiteSpace($Address) -or $Address.Contains(':') -or $Address.StartsWith('169.254.')) { return $false }
    $parsed = $null
    return [System.Net.IPAddress]::TryParse($Address, [ref]$parsed) -and $parsed.AddressFamily -eq [System.Net.Sockets.AddressFamily]::InterNetwork
}
function Test-HyperVIPv4InSubnet {
    param([string]$IPAddress, [string]$NetworkAddress, [ValidateRange(0, 32)][int]$PrefixLength)
    $ipParsed = $null; $netParsed = $null
    if (-not [System.Net.IPAddress]::TryParse($IPAddress, [ref]$ipParsed)) { return $false }
    if (-not [System.Net.IPAddress]::TryParse($NetworkAddress, [ref]$netParsed)) { return $false }
    if ($ipParsed.AddressFamily -ne [System.Net.Sockets.AddressFamily]::InterNetwork -or $netParsed.AddressFamily -ne [System.Net.Sockets.AddressFamily]::InterNetwork) { return $false }
    if ($PrefixLength -eq 0) { return $true }
    $ipBytes = $ipParsed.GetAddressBytes(); $netBytes = $netParsed.GetAddressBytes()
    $ipU = ([uint32]$ipBytes[0] -shl 24) -bor ([uint32]$ipBytes[1] -shl 16) -bor ([uint32]$ipBytes[2] -shl 8) -bor [uint32]$ipBytes[3]
    $netU = ([uint32]$netBytes[0] -shl 24) -bor ([uint32]$netBytes[1] -shl 16) -bor ([uint32]$netBytes[2] -shl 8) -bor [uint32]$netBytes[3]
    $mask = [uint32]([uint32]::MaxValue -shl (32 - $PrefixLength))
    return (($ipU -band $mask) -eq ($netU -band $mask))
}
function Get-HyperVSwitchHostSubnet {
    param([AllowEmptyString()][string]$SwitchName)
    if ([string]::IsNullOrWhiteSpace($SwitchName)) { return $null }
    $ipInfo = Get-NetIPAddress -InterfaceAlias "vEthernet ($SwitchName)" -AddressFamily IPv4 -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $ipInfo) { return $null }
    return @{ Address = [string]$ipInfo.IPAddress; PrefixLength = [int]$ipInfo.PrefixLength }
}
function Select-HyperVGuestIPv4 {
    param([AllowNull()][object[]]$Adapters, [int]$ProbePort = 0, [int]$ProbeTimeoutMs = 1000)
    $raw = [System.Collections.Generic.List[object]]::new()
    foreach ($adapter in @($Adapters)) {
        if (-not $adapter) { continue }
        foreach ($addr in @($adapter.IPAddresses)) { $raw.Add([pscustomobject]@{ Address = $addr; SwitchName = $adapter.SwitchName }) }
    }
    $seen = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    $candidates = [System.Collections.Generic.List[object]]::new()
    foreach ($entry in $raw) {
        if ((Test-HyperVRoutableIPv4 -Address $entry.Address) -and $seen.Add([string]$entry.Address)) {
            $candidates.Add([pscustomobject]@{ Address = [string]$entry.Address; SwitchName = $entry.SwitchName; InSubnet = $false; Reachable = $false })
        }
    }
    if ($candidates.Count -eq 0) { return $null }
    if ($candidates.Count -eq 1) { return $candidates[0].Address }
    $subnetCache = @{}
    foreach ($c in $candidates) {
        if ([string]::IsNullOrWhiteSpace($c.SwitchName)) { continue }
        if (-not $subnetCache.ContainsKey($c.SwitchName)) { $subnetCache[$c.SwitchName] = Get-HyperVSwitchHostSubnet -SwitchName $c.SwitchName }
        $subnet = $subnetCache[$c.SwitchName]
        if ($subnet -and (Test-HyperVIPv4InSubnet -IPAddress $c.Address -NetworkAddress $subnet.Address -PrefixLength $subnet.PrefixLength)) { $c.InSubnet = $true }
    }
    if ($ProbePort -gt 0) {
        foreach ($c in $candidates) { $c.Reachable = Test-HyperVTcpPort -ComputerName $c.Address -Port $ProbePort -TimeoutMilliseconds $ProbeTimeoutMs }
    }
    $winner = $candidates[0]; $winnerScore = 0
    foreach ($c in $candidates) {
        $score = 0
        if ($c.Reachable) { $score += 2 }
        if ($c.InSubnet) { $score += 1 }
        if ($score -gt $winnerScore) { $winner = $c; $winnerScore = $score }
    }
    return $winner.Address
}
function Get-HyperVVMBiosGuid {
    param([Parameter(Mandatory)][string]$VMName)
    $setting = Get-CimInstance -Namespace root\virtualization\v2 -ClassName Msvm_VirtualSystemSettingData -ErrorAction SilentlyContinue |
        Where-Object { $_.ElementName -eq $VMName -and $_.VirtualSystemType -eq 'Microsoft:Hyper-V:System:Realized' } |
        Select-Object -First 1
    if (-not $setting) { return $null }
    return ([string]$setting.BIOSGUID).Trim('{', '}')
}
function Resolve-HyperVGuestIPv4 {
    param([Parameter(Mandatory)][string]$VMName, [int]$Port = 5985, [int]$TimeoutMilliseconds = 1000)
    # Only addresses the VM reports for its own adapters are usable. An earlier
    # revision fell back to scanning the host's neighbour table for anything
    # answering on the WinRM port, which on a switch shared with other VMs
    # silently returned a different VM's address -- layers then converged, and
    # rebooted, someone else's machine while this VM's checkpoints were written.
    # A VM that has not published an address yet is simply not ready.
    $adapters = @(Get-VMNetworkAdapter -VMName $VMName -ErrorAction SilentlyContinue)
    if ($adapters.Count -eq 0) { return $null }
    return Select-HyperVGuestIPv4 -Adapters $adapters -ProbePort $Port -ProbeTimeoutMs $TimeoutMilliseconds
}
function Add-HyperVTrustedHost {
    param([Parameter(Mandatory)][string]$ComputerName)
    $current = Invoke-HyperVBounded -TimeoutSec $script:TrustedHostsTimeoutSec -OperationName 'read WinRM TrustedHosts' -ScriptBlock {
        (Get-Item WSMan:\localhost\Client\TrustedHosts -ErrorAction Stop).Value
    }
    $entries = @($current -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ })
    $trusted = '*' -in $entries -or $ComputerName -in $entries
    if (-not $trusted) { foreach ($entry in $entries) { if (($entry.Contains('*') -or $entry.Contains('?')) -and $ComputerName -like $entry) { $trusted = $true; break } } }
    if ($trusted) { return }
    $null = Invoke-HyperVBounded -TimeoutSec $script:TrustedHostsTimeoutSec -OperationName 'update WinRM TrustedHosts' -ArgumentList @($ComputerName) -ScriptBlock {
        param($target) Set-Item WSMan:\localhost\Client\TrustedHosts -Value $target -Concatenate -Force -ErrorAction Stop
    }
}
function New-HyperVWinRMSession {
    param(
        [Parameter(Mandatory)][string]$ComputerName,
        [Parameter(Mandatory)][pscredential]$Credential,
        [int]$Port = 5985,
        [int]$OpenTimeoutSec = 5,
        [int]$OperationTimeoutSec = 10
    )
    Add-HyperVTrustedHost -ComputerName $ComputerName
    $option = New-PSSessionOption -OpenTimeout ($OpenTimeoutSec * 1000) -OperationTimeout ($OperationTimeoutSec * 1000) -MaxConnectionRetryCount 0
    return New-PSSession -ComputerName $ComputerName -Port $Port -Credential $Credential -Authentication Negotiate -SessionOption $option -ErrorAction Stop
}
function Get-HyperVGuestReadiness {
    param(
        [Parameter(Mandatory)][string]$VMName,
        [Parameter(Mandatory)][pscredential]$Credential,
        [int]$WinRMPort = 5985,
        [int]$ConnectionTimeoutSec = 5,
        [string]$KnownIP
    )
    $layers = [System.Collections.Generic.List[object]]::new(); $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $highest = $null; $failed = $null; $reason = ''; $ip = $KnownIP
    try { $vm = Get-VM -Name $VMName -ErrorAction Stop } catch { $vm = $null }
    if (-not $vm) { $layers.Add([pscustomobject]@{Level='Running';Passed=$false;Detail="VM '$VMName' not found"}); $failed='Running'; $reason=$layers[0].Detail; return [pscustomobject]@{Ready=$false;Host=$null;Port=$WinRMPort;HighestPassed=$highest;FirstFailed=$failed;FailureReason=$reason;Layers=$layers;DurationMs=[int]$sw.ElapsedMilliseconds} }
    $stateOk = $vm.State -eq 'Running'; $layers.Add([pscustomobject]@{Level='Running';Passed=$stateOk;Detail="State=$($vm.State)"})
    if (-not $stateOk) { $failed='Running'; $reason=$layers[-1].Detail; return [pscustomobject]@{Ready=$false;Host=$null;Port=$WinRMPort;HighestPassed=$highest;FirstFailed=$failed;FailureReason=$reason;Layers=$layers;DurationMs=[int]$sw.ElapsedMilliseconds} }
    $highest='Running'
    $hb = $vm.Heartbeat; $uptime = if ($vm.PSObject.Properties['Uptime']) { [int]$vm.Uptime.TotalSeconds } else { 0 }
    $hbOk = $hb -in @('OkApplicationsHealthy', 'OkApplicationsUnknown')
    $layers.Add([pscustomobject]@{Level='Heartbeat';Passed=$hbOk;Detail="Heartbeat=$hb, Uptime=${uptime}s"})
    if (-not $hbOk) { $failed='Heartbeat'; $reason=$layers[-1].Detail; return [pscustomobject]@{Ready=$false;Host=$null;Port=$WinRMPort;HighestPassed=$highest;FirstFailed=$failed;FailureReason=$reason;Layers=$layers;DurationMs=[int]$sw.ElapsedMilliseconds} }
    $highest='Heartbeat'
    if (-not $ip) { $ip = Resolve-HyperVGuestIPv4 -VMName $VMName -Port $WinRMPort -TimeoutMilliseconds ($ConnectionTimeoutSec * 1000) }
    $ipOk = -not [string]::IsNullOrWhiteSpace($ip)
    $layers.Add([pscustomobject]@{Level='IPv4';Passed=$ipOk;Detail=if($ipOk){"IP=$ip"}else{'No routable IPv4 discovered'}})
    if (-not $ipOk) { $failed='IPv4'; $reason=$layers[-1].Detail; return [pscustomobject]@{Ready=$false;Host=$null;Port=$WinRMPort;HighestPassed=$highest;FirstFailed=$failed;FailureReason=$reason;Layers=$layers;DurationMs=[int]$sw.ElapsedMilliseconds} }
    $highest='IPv4'
    $reachable = $false; $reachDetail = 'ICMP=fail, ARP=miss (advisory only)'
    try {
        if (Test-Connection -ComputerName $ip -Count 1 -TimeoutSeconds 2 -Quiet -ErrorAction SilentlyContinue) { $reachable = $true; $reachDetail = 'ICMP=OK' }
        elseif (Get-NetNeighbor -IPAddress $ip -ErrorAction SilentlyContinue | Where-Object { $_.State -in @('Reachable','Stale','Permanent') } | Select-Object -First 1) { $reachable = $true; $reachDetail = 'ICMP=blocked, ARP=present' }
    } catch { $reachDetail = "probe-error: $($_.Exception.Message) (advisory only)" }
    $layers.Add([pscustomobject]@{Level='Reachable';Passed=$reachable;Detail=$reachDetail})
    $portOk = Test-HyperVTcpPort -ComputerName $ip -Port $WinRMPort -TimeoutMilliseconds ($ConnectionTimeoutSec * 1000)
    $layers.Add([pscustomobject]@{Level='PortOpen';Passed=$portOk;Detail="TCP $WinRMPort on ${ip}=$(if($portOk){'open'}else{'closed'})"})
    if (-not $portOk) { $failed='PortOpen'; $reason=$layers[-1].Detail; return [pscustomobject]@{Ready=$false;Host=$ip;Port=$WinRMPort;HighestPassed=$highest;FirstFailed=$failed;FailureReason=$reason;Layers=$layers;DurationMs=[int]$sw.ElapsedMilliseconds} }
    $highest='PortOpen'
    try {
        Add-HyperVTrustedHost -ComputerName $ip
        $product = Invoke-HyperVBounded -TimeoutSec ($ConnectionTimeoutSec * 3) -OperationName "Test-WSMan ${ip}:$WinRMPort" -ArgumentList @($ip, $WinRMPort, $Credential) -ScriptBlock {
            param($target, $port, $cred) $r = Test-WSMan -ComputerName $target -Port $port -Authentication Negotiate -Credential $cred -ErrorAction Stop; if ($r) { [string]$r.ProductVersion } else { $null }
        }
        $wsmanOk = $null -ne $product; $wsmanDetail = if ($wsmanOk) { "ProductVersion=$product" } else { 'Test-WSMan returned null' }
    } catch { $wsmanOk = $false; $wsmanDetail = "WSMan failed: $($_.Exception.Message)" }
    $layers.Add([pscustomobject]@{Level='WSMan';Passed=$wsmanOk;Detail=$wsmanDetail})
    if (-not $wsmanOk) { $failed='WSMan'; $reason=$layers[-1].Detail; return [pscustomobject]@{Ready=$false;Host=$ip;Port=$WinRMPort;HighestPassed=$highest;FirstFailed=$failed;FailureReason=$reason;Layers=$layers;DurationMs=[int]$sw.ElapsedMilliseconds} }
    $highest='WSMan'
    $session = $null
    try {
        $session = New-HyperVWinRMSession -ComputerName $ip -Credential $Credential -Port $WinRMPort -OpenTimeoutSec $ConnectionTimeoutSec -OperationTimeoutSec ($ConnectionTimeoutSec * 2)
        $probe = Invoke-Command -Session $session -ScriptBlock {
            [pscustomobject]@{ Smoke = 1 + 1; Uuid = [string](Get-CimInstance Win32_ComputerSystemProduct -ErrorAction SilentlyContinue).UUID; Name = $env:COMPUTERNAME }
        } -ErrorAction Stop
        $opOk = $probe.Smoke -eq 2; $opDetail = if ($opOk) { 'PSSession opened + smoke=2' } else { "smoke returned $($probe.Smoke), expected 2" }
    } catch { $probe = $null; $opOk = $false; $opDetail = "operation threw: $($_.Exception.Message)" }
    finally { if ($session) { Remove-PSSession -Session $session -ErrorAction SilentlyContinue } }
    $layers.Add([pscustomobject]@{Level='Operation';Passed=$opOk;Detail=$opDetail})
    if (-not $opOk) {
        $failed='Operation'; $reason=$opDetail
        return [pscustomobject]@{Ready=$false;Host=$ip;Port=$WinRMPort;HighestPassed=$highest;FirstFailed=$failed;FailureReason=$reason;Layers=$layers;DurationMs=[int]$sw.ElapsedMilliseconds}
    }
    $highest='Operation'
    # Addressing is by IP, so proving the session landed on the leased VM is the
    # only thing standing between a layer and someone else's machine. The
    # firmware GUID Hyper-V assigns the VM is what the guest reports as its
    # SMBIOS UUID, so the two must agree.
    $expectedGuid = Get-HyperVVMBiosGuid -VMName $VMName
    $actualGuid = if ($probe) { ([string]$probe.Uuid).Trim('{', '}') } else { '' }
    if ([string]::IsNullOrWhiteSpace($expectedGuid)) {
        $identityOk = $false
        $identityDetail = "firmware GUID unavailable for '$VMName'; identity could not be verified"
    } else {
        $identityOk = $expectedGuid -eq $actualGuid
        $identityDetail = if ($identityOk) { "guest is '$($probe.Name)' ($actualGuid)" } else { "session on $ip reached '$($probe.Name)' ($actualGuid), expected VM '$VMName' ($expectedGuid)" }
    }
    $layers.Add([pscustomobject]@{Level='Identity';Passed=$identityOk;Detail=$identityDetail})
    if ($identityOk) { $highest='Identity' } else { $failed='Identity'; $reason=$identityDetail }
    return [pscustomobject]@{Ready=$identityOk;Host=$ip;Port=$WinRMPort;HighestPassed=$highest;FirstFailed=$failed;FailureReason=$reason;Layers=$layers;DurationMs=[int]$sw.ElapsedMilliseconds}
}
# Harvested from Public\Wait-HyperVReady.ps1, Private\ReadinessLoop.ps1, Private\Probe.ps1, and Private\Communicator.WinRM.ps1.
function Test-HyperVGuestReady {
    [CmdletBinding()]
    [OutputType([bool])]
    param(
        [Parameter(Mandatory)][string]$VMName,
        [Parameter(Mandatory)][pscredential]$Credential,
        [int]$WinRMPort = 5985,
        [int]$ConnectionTimeoutSec = 5,
        [string]$KnownIP
    )
    try { return [bool](Get-HyperVGuestReadiness -VMName $VMName -Credential $Credential -WinRMPort $WinRMPort -ConnectionTimeoutSec $ConnectionTimeoutSec -KnownIP $KnownIP).Ready } catch { return $false }
}
# Harvested from Public\Wait-HyperVReady.ps1, Private\ReadinessLoop.ps1, Private\Probe.ps1, and Private\Communicator.WinRM.ps1.
function Wait-HyperVGuestReady {
    [CmdletBinding()]
    [OutputType([pscustomobject])]
    param(
        [Parameter(Mandatory)][string]$VMName,
        [Parameter(Mandatory)][pscredential]$Credential,
        [int]$TimeoutSeconds = 300,
        [int]$PollIntervalSeconds = 2,
        [int]$WinRMPort = 5985,
        [int]$ConnectionTimeoutSec = 5
    )
    $wait = @{ KnownIP = $null; Last = $null }
    try {
        $outcome = Wait-Until `
            -OperationName "guest '$VMName' readiness" `
            -TimeoutSeconds $TimeoutSeconds `
            -PollIntervalSeconds ([Math]::Max(1, $PollIntervalSeconds)) `
            -Probe {
                $probe = Get-HyperVGuestReadiness -VMName $VMName -Credential $Credential -WinRMPort $WinRMPort -ConnectionTimeoutSec $ConnectionTimeoutSec -KnownIP $wait.KnownIP
                if ($probe.Host) { $wait.KnownIP = $probe.Host }
                $wait.Last = $probe
                $probe
            } `
            -Until { param($probe) [bool]$probe.Ready } `
            -DescribeResult {
                param($probe)
                if ($probe) { "$($probe.FirstFailed): $($probe.FailureReason)" } else { 'no probe completed' }
            }
    }
    catch {
        $detail = if ($wait.Last) { "$($wait.Last.FirstFailed): $($wait.Last.FailureReason)" } else { $_.Exception.Message }
        throw "Guest '$VMName' was not ready before ${TimeoutSeconds}s deadline. Last failure: $detail"
    }
    $last = $outcome.Value
    return [pscustomobject]@{ VMName=$VMName; Ready=$true; Host=$last.Host; Port=$last.Port; Attempts=$outcome.Attempts; DurationSec=$outcome.DurationSec; Probe=$last }
}
# Proof that a restart actually happened has to survive a guest that never logs
# anyone on. The guest's own boot time is authoritative, needs no logon, and
# cannot be satisfied by state left over from an earlier restart.
function Get-HyperVGuestBootTime {
    param(
        [Parameter(Mandatory)][string]$VMName,
        [Parameter(Mandatory)][pscredential]$Credential,
        [string]$ComputerName,
        [int]$WinRMPort = 5985,
        [int]$ConnectionTimeoutSec = 30
    )
    if (-not $ComputerName) { $ComputerName = Resolve-HyperVGuestIPv4 -VMName $VMName -Port $WinRMPort -TimeoutMilliseconds ($ConnectionTimeoutSec * 1000) }
    if (-not $ComputerName) { return $null }
    $session = New-HyperVWinRMSession -ComputerName $ComputerName -Credential $Credential -Port $WinRMPort -OpenTimeoutSec $ConnectionTimeoutSec -OperationTimeoutSec ($ConnectionTimeoutSec * 2)
    try {
        $raw = Invoke-Command -Session $session -ScriptBlock { (Get-CimInstance Win32_OperatingSystem).LastBootUpTime } -ErrorAction Stop
        if (-not $raw) { return $null }
        return ([datetime]$raw).ToUniversalTime()
    }
    finally { Remove-PSSession -Session $session -ErrorAction SilentlyContinue }
}

function Restart-HyperVGuest {
    [CmdletBinding()]
    [OutputType([pscustomobject])]
    param(
        [Parameter(Mandatory)][string]$VMName,
        [Parameter(Mandatory)][pscredential]$Credential,
        [string]$ComputerName,
        [string]$Reason = 'TaskPilot guest reboot',
        [int]$DelaySeconds = 5,
        [int]$WinRMPort = 5985,
        [int]$ConnectionTimeoutSec = 30
    )
    if (-not $ComputerName) { $ComputerName = (Wait-HyperVGuestReady -VMName $VMName -Credential $Credential -WinRMPort $WinRMPort -ConnectionTimeoutSec $ConnectionTimeoutSec).Host }
    $session = New-HyperVWinRMSession -ComputerName $ComputerName -Credential $Credential -Port $WinRMPort -OpenTimeoutSec $ConnectionTimeoutSec -OperationTimeoutSec ($ConnectionTimeoutSec * 2)
    $job = $null
    try {
        $job = Invoke-Command -Session $session -AsJob -ArgumentList $DelaySeconds,$Reason -ScriptBlock {
            param($delay,$reason)
            & shutdown.exe /r /t $delay /f /d p:2:4 /c $reason
            if ($LASTEXITCODE -ne 0) { throw "shutdown.exe /r failed with exit code $LASTEXITCODE" }
        } -ErrorAction Stop
        Start-Sleep -Milliseconds 750
    }
    finally {
        if ($job -and ($job.State -in @('Completed','Failed','Stopped'))) { Remove-Job -Job $job -Force -ErrorAction SilentlyContinue }
        Remove-PSSession -Session $session -ErrorAction SilentlyContinue
    }
    return [pscustomobject]@{ VMName=$VMName; Host=$ComputerName; RestartIssued=$true; DelaySeconds=$DelaySeconds; Reason=$Reason }
}
# Waits for the restart requested by Restart-HyperVGuest to have actually happened.
# Readiness alone is not proof: the session can come back on the very same boot
# (the shutdown was still pending, or never took), which is how a layer ends up
# running against the pre-reboot kernel.
function Wait-HyperVGuestReboot {
    [CmdletBinding()]
    [OutputType([pscustomobject])]
    param(
        [Parameter(Mandatory)][string]$VMName,
        [Parameter(Mandatory)][pscredential]$Credential,
        [Parameter(Mandatory)][datetime]$PriorBootTime,
        [int]$TimeoutSeconds = 600,
        [int]$PollIntervalSeconds = 2,
        [int]$WinRMPort = 5985,
        [int]$ConnectionTimeoutSec = 10
    )
    $expected = $PriorBootTime.ToUniversalTime()
    $wait = @{ Last = $null }
    try {
        $outcome = Wait-Until `
            -OperationName "VM '$VMName' restart" `
            -TimeoutSeconds $TimeoutSeconds `
            -PollIntervalSeconds ([Math]::Max(1, $PollIntervalSeconds)) `
            -Probe {
                try {
                    $state = Get-HyperVGuestReadiness -VMName $VMName -Credential $Credential -WinRMPort $WinRMPort -ConnectionTimeoutSec $ConnectionTimeoutSec
                    if (-not $state.Ready) {
                        $result = [pscustomobject]@{ Ready=$false; Host=$state.Host; BootTimeUtc=$null; Detail="$($state.FirstFailed): $($state.FailureReason)" }
                    }
                    else {
                        $boot = Get-HyperVGuestBootTime -VMName $VMName -Credential $Credential -ComputerName $state.Host -WinRMPort $WinRMPort -ConnectionTimeoutSec $ConnectionTimeoutSec
                        $isNewBoot = $boot -and $boot -gt $expected
                        $detail = if ($isNewBoot) { '' } else { "guest answered but is still on the pre-reboot boot (boot=$boot, expected newer than $expected)" }
                        $result = [pscustomobject]@{ Ready=[bool]$isNewBoot; Host=$state.Host; BootTimeUtc=$boot; Detail=$detail }
                    }
                }
                catch {
                    $result = [pscustomobject]@{ Ready=$false; Host=$null; BootTimeUtc=$null; Detail=$_.Exception.Message }
                }
                $wait.Last = $result
                $result
            } `
            -Until { param($result) [bool]$result.Ready } `
            -DescribeResult { param($result) [string]$result.Detail }
    }
    catch {
        $lastError = if ($wait.Last) { $wait.Last.Detail } else { $_.Exception.Message }
        throw "VM '$VMName' did not complete the requested restart before ${TimeoutSeconds}s deadline. Last observation: $lastError"
    }
    $result = $outcome.Value
    return [pscustomobject]@{ VMName=$VMName; Ready=$true; Host=$result.Host; BootTimeUtc=$result.BootTimeUtc; Attempts=$outcome.Attempts; DurationSec=$outcome.DurationSec }
}
Export-ModuleMember -Function @(
    'Resolve-HyperVGuestIPv4',
    'Get-HyperVGuestReadiness',
    'Test-HyperVGuestReady',
    'Wait-HyperVGuestReady',
    'New-HyperVWinRMSession',
    'Get-HyperVGuestBootTime',
    'Restart-HyperVGuest',
    'Wait-HyperVGuestReboot'
)
