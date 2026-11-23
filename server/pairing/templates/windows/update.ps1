if ($h)
{
    Write-Output "Update the rport client.
Invoking without parameters updates to the latest stable version.

Parameters:
-t  Use the latest unstable version.
-x  Enable command and script execution without asking for confirmation.
-d  Disable command and script execution.
-v [version] Upgrade to the specified version.
-m  do not install or update tacoscript
-f  force update without comparing versions
-r  activate file reception
"
    exit
}
$release = If ($t)
{
    "unstable"
}
Else
{
    "stable"
}
$enableScripts = $null
$enableScripts = If ($x)
{
    "true"
}
$enableScripts = If ($d)
{
    "false"
}

$myLocation = (Get-Location).path
$configFile = 'C:\Program Files\rport\rport.conf'
$installDir = "$( $Env:Programfiles )\rport"
if (-Not(Test-Path "$( $installDir )\rport.exe"))
{
    Write-Output "You don't have RPort installed. Nothing to do."
    exit 0
}
Set-Location $installDir
Enable-ModernTls
if ($f)
{
    # If current version is set to 0 an update will always be forced
    Write-Output "* Forcing an update or redownload"
    $currentVersion = '0'
}
else
{
    $versionString = (& 'C:\Program Files\rport\rport.exe' --version)
    $currentVersion = $( $versionString -split " " )[1]
}

$isMsiInstallation = $False

$downloadFile = Invoke-Download -gt $currentVersion -pkgUrl $pkgUrl
If ((Get-Item $downloadFile).length -eq 0)
{
    Write-Output "* No RPort update needed. You are on the latest $currentVersion version."
    Remove-Item $downloadFile
    # Install or update tacoscript
    if ($m -eq $false)
    {
        Install-Tacoscript
    }
    Set-Location $myLocation
    exit 0
}
Write-Output "* Download finished and stored to $( $downloadFile ) ."

# Updates are installed by a supervisor that runs as a scheduled task, detached from the rport
# service. It keeps the previous rport.exe and rport.conf, checks that the new client connects to the
# server and rolls back if it does not.
$stage = Join-Path $installDir 'update'
$healthTimeout = 120
$stableSecs = 20
if ((Test-Path "$stage\status.json") -and ((Get-Content -Raw "$stage\status.json") -match '"status":"running"') -and
        (Get-ScheduledTask -TaskName 'rport-update-*' -ErrorAction SilentlyContinue | Where-Object { $_.State -eq 'Running' }))
{
    Remove-Item $downloadFile -Force
    throw "Another update is running. See $stage\update.log"
}
New-Item -ItemType Directory -Force -Path $stage | Out-Null
# Not $d: that is the -d switch of this script.
foreach ($stageDir in 'previous', 'new')
{
    if (Test-Path "$stage\$stageDir")
    {
        Remove-Item "$stage\$stageDir" -Recurse -Force
    }
    New-Item -ItemType Directory -Force -Path "$stage\$stageDir" | Out-Null
}
# Back up before anything is changed, including the config.
Copy-Item "$installDir\rport.exe" "$stage\previous\rport.exe"
Copy-Item $configFile "$stage\previous\rport.conf"
Write-Output "* Backed up rport.exe ($currentVersion) and rport.conf to $stage\previous"

$kind = 'exe'
$migrate = $false
if ($downloadFile -match '\.msi$')
{
    $kind = 'msi'
    # Installed from a ZIP, updated with an MSI: the supervisor removes the old service registration first.
    $migrate = -not $isMsiInstallation
    Move-Item $downloadFile "$stage\new\rport.msi"
    $targetVersion = (Get-MSIVersionInfo -Path "$stage\new\rport.msi")
    if (-not $targetVersion)
    {
        $targetVersion = 'msi'
    }
}
else
{
    Write-Output "* Extracting rport.exe to $stage\new"
    Expand-Zip -Path $downloadFile -DestinationPath "$stage\new"
    Remove-Item $downloadFile
    if (Test-Path "$stage\new\rport.example.conf")
    {
        Remove-Item "$stage\new\rport.example.conf"
    }
    if (-not (Test-Path "$stage\new\rport.exe"))
    {
        throw "The download does not contain rport.exe."
    }
    $targetVersion = (& "$stage\new\rport.exe" --version) -replace "version ", ""
}
Write-Output "* New version will be $targetVersion."

# True if this script was started by the rport client (a remote command), whose restart would end it.
function Test-RportSubprocess
{
    $id = $PID
    for ($i = 0; $i -lt 10; $i++) {
        $proc = Get-CimInstance Win32_Process -Filter "ProcessId=$id" -ErrorAction SilentlyContinue
        if (-not $proc -or -not $proc.ParentProcessId)
        {
            return $false
        }
        $parent = Get-CimInstance Win32_Process -Filter "ProcessId=$( $proc.ParentProcessId )" -ErrorAction SilentlyContinue
        if (-not $parent)
        {
            return $false
        }
        if ($parent.Name -eq 'rport.exe')
        {
            return $true
        }
        $id = $parent.ProcessId
    }
    return $false
}

# The client's log file from rport.conf.
function Get-ClientLogFile
{
    foreach ($line in (Get-Content $configFile))
    {
        if ($line -match "^\s*log_file\s*=\s*[`"']([^`"']*)[`"']")
        {
            return $Matches[1]
        }
    }
    return ''
}

function ExtendConfig
{
    Add-Content -Path $configFile -Value "[remote-scripts]
  enabled = $( $enableScripts )"
}
function abortOnNoTerminal
{
    if ([System.Environment]::UserInteractive)
    {
        return
    }
    Remove-Item -Path $downloadFile -Force
    $host.ui.WriteErrorLine('This script cannot run without a terminal because questions are asked.')
    $host.ui.WriteErrorLine("Execute with '-x' to enable remote script execution or ")
    $host.ui.WriteErrorLine("execute with '-d' to disable remote script execution.")
    exit 1
}
function askForScriptEnabling
{
    abortOnNoTerminal
    do
    {
        $yesNo = Read-Host -prompt 'Do you want to enabale remote script execution? Y/N'
        if ( $yesNo.Tolower().StartsWith('y'))
        {
            $enableScripts = 'true'
            return $enableScripts
        }
        elseif ( $yesNo.ToLower().StartsWith('n'))
        {
            $enableScripts = 'false'
            return $enableScripts
        }
    } while ($true)
}
# Check if the config needs an update
if (Select-String -Path $configFile -Pattern "[remote-scripts]")
{
    Write-Output "* Scripts are already configured. Not changing."
}
else
{
    if ($null -eq $enableScripts)
    {
        askForScriptEnabling
    }
    ExtendConfig
}

# Add file reception to the config file
function Add-FileRecption
{
    if ((Get-Content $configFile) -match "^\[file-reception\]")
    {
        Write-Output "* Monitoring already enabled."
        return
    }
    Add-Content -Path $configFile -Value "
[file-reception]
  ## Receive files pushed by the server, enabled by default
  # enabled = true
  ## The rport client will reject writing files to any of the following folders and its subfolders.
  ## Wildcards (glob) are supported.
  ## Linux defaults
  # protected = ['/bin', '/sbin', '/boot', '/usr/bin', '/usr/sbin', '/dev', '/lib*', '/run']
  ## Windows defaults
  # protected = ['C:\Windows\', 'C:\ProgramData']
"
}

# Add Monitoring reception to the config file
function Add-Monitoring
{
    if ((Get-Content $configFile) -match "^\[monitoring\]")
    {
        Write-Output "* Monitoring section already present in configuration file."
        return
    }
    Add-Content -Path $configFile -Value "
[monitoring]
  ## The rport client can collect and report performance data of the operating system.
  ## Monitoring is enabled by default
  enabled = true
  ## How often (seconds) monitoring data should be collected.
  ## A value below 60 seconds will be overwritten by the hard-coded default of 60 seconds.
  # interval = 60
  ## RPort monitors the fill level of almost all volumes or mount points.
  ## Change the below defaults to include or exclude volumes or mount points from the monitoring.
  #fs_type_include = ['ext3','ext4','xfs','jfs','ntfs','btrfs','hfs','apfs','exfat','smbfs','nfs']
  ## List of excluded mount points or device letters
  #fs_path_exclude = []
  ## Example:
  # fs_path_exclude = ['/mnt/*','h:']
  ## Having fs_path_exclude_recurse = false the specified path
  ## must match a mountpoint or it will be ignored
  ## Having fs_path_exclude_recurse = true the specified path
  ## can be any folder and all mountpoints underneath will be excluded
  #fs_path_exclude_recurse = false
  ## To avoid monitoring of so-called mount binds,
  ## mount points are identified by the path and device name.
  ## Mountpoints pointing to the same device are ignored.
  ## What appears first in /proc/self/mountinfo is considered as the original.
  ## Applies only to Linux
  #fs_identify_mountpoints_by_device = true
  ## RPort monitors all running processes
  ## Process monitoring is enabled by default
  pm_enabled = true
  ## Monitor kernel tasks identified by process group 0
  #pm_enable_kerneltask_monitoring = true
  ## The process list is sorted by PID descending. Only the top N processes are monitored.
  #pm_max_number_monitored_processes = 500
  ## Monitor the bandwidth usage of the following maximum two network cards:
  ## 'net_lan' and 'net_wan'.
  ## You must specify the device name and the maximum speed in Megabits.
  ## On Windows use 'Get-Netadapter' to discover adapter names.
  ## Examples:
  ## net_lan = [ 'eth0' , '1000' ]
  ## net_wan = ['Ethernet0', '1000']
  #net_lan = ['', '1000']
  #net_wan = ['', '1000']"
}

function Add-WatchdogIntegration
{
    <#
    .SYNOPSIS
        Push watchdog integration to the rport.conf
    #>
    [OutputType([Object[]])]
    param (
        [Parameter(Mandatory)]
        [Object[]]$ConfigContent
    )

    if ($ConfigContent -match "watchdog_integration")
    {
        Write-Information "* Watchdog Integration already present in configuration file."
        return $ConfigContent
    }

    if ([System.Version]$targetVersion -lt [System.Version]"0.8.8")
    {
        Write-Information "* Version $( $targetVersion ) does not support watchdog integration"
        return $ConfigContent
    }

    $watchdogSnippet = "
  ## Write a state file to {data_dir}/state.json that can be evaluated by external watchdog implementations.
  ## On Linux this also enables the systemd watchdog integration by using the systemd notify socket.
  ## Requires max_retry_count = -1 and keep_alive > 0
  ## Disabled by default.
  #watchdog_integration = false
"
    $ConfigContent -replace "## Optionally set the 'Host' header", "$( $watchdogSnippet )`n`n  $&"
    Write-Information "* watchdog integration inserted"
}

function Invoke-Later
{
    Param
    (
        [Parameter(Mandatory = $true)]
        [string] $ScriptBlock,
        [Parameter(Mandatory = $false)]
        [int] $Delay = 10,
        [Parameter(Mandatory = $false)]
        [string] $Description = "Background Task"
    )
    $taskName = 'Invoke-Later-' + (Get-Random)
    $taskFile = [System.Environment]::GetEnvironmentVariable('TEMP', 'Machine') + '\' + $taskName + '.ps1'
    $ScriptBlock.Split("`n") | ForEach-Object {
        if ($_)
        {
            $_.Trim() | Out-File -FilePath $taskFile -Append
        }
    }
    "Unregister-ScheduledTask -Taskname $( $taskName ) -Confirm:`$false" | Out-File -FilePath $taskFile -Append
    "Remove-Item `"$( $taskFile )`" -Force" | Out-File -FilePath $taskFile -Append
    $action = New-ScheduledTaskAction -Execute "powershell" -Argument "-ExecutionPolicy bypass -file $( $taskFile )"
    $trigger = New-ScheduledTaskTrigger -Once -At (Get-Date).AddSeconds($Delay)
    $principal = New-ScheduledTaskPrincipal -UserID "NT AUTHORITY\SYSTEM" -LogonType ServiceAccount -RunLevel Highest
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries
    $task = New-ScheduledTask -Action $action -Principal $principal -Trigger $trigger -Settings $settings
    Register-ScheduledTask $taskName -InputObject $task
    Write-Output "* Task `"$( $Description )`" [$( $taskFile )] scheduled."
    Write-Output "  It will be executed within $( $Delay ) seconds."
}

# Install or update tacoscript
if ($m -eq $false)
{
    Install-Tacoscript
}

# Add new features
Add-Monitoring
Add-FileRecption
# Enable new features
$configContent = Get-Content $configFile -Encoding utf8
$configContent = Enable-InterpreterAlias -ConfigContent $configContent
$configContent = Enable-Network-Monitoring -ConfigContent $configContent
# Enable/Disable file reception
$configContent = Enable-FileReception -ConfigContent $configContent -Switch $r
# Insert watchdog integration
$configContent = Add-WatchdogIntegration -ConfigContent $configContent
# Finally, write the config to a file
$Utf8NoBomEncoding = New-Object System.Text.UTF8Encoding $False
[IO.File]::WriteAllLines($configFile, $configContent, $Utf8NoBomEncoding)

# Set the service startup and recovery actions
Optimize-ServiceStartup

# Hand over to the supervisor, run as SYSTEM by a scheduled task.
$taskName = 'rport-update-' + (Get-Date -Format 'yyyyMMddHHmmss')
$settingsFile = "$stage\settings.json"
$settings = [ordered]@{
    Stage = $stage; InstallDir = $installDir; ConfigFile = $configFile; LogFile = (Get-ClientLogFile)
    FromVersion = $currentVersion; ToVersion = $targetVersion; Kind = $kind; Migrate = $migrate
    HealthTimeout = $healthTimeout; StableSecs = $stableSecs; TaskName = $taskName
}
$Utf8NoBomEncoding = New-Object System.Text.UTF8Encoding $False
[IO.File]::WriteAllText($settingsFile, ($settings | ConvertTo-Json), $Utf8NoBomEncoding)
Write-UpdateSupervisor -Path "$stage\supervise.ps1"
if (Test-Path "$stage\status.json")
{
    Remove-Item "$stage\status.json" -Force
}
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$stage\supervise.ps1`" -Settings `"$settingsFile`""
$principal = New-ScheduledTaskPrincipal -UserID 'NT AUTHORITY\SYSTEM' -LogonType ServiceAccount -RunLevel Highest
$taskSettings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Minutes 30)
$task = New-ScheduledTask -Action $action -Principal $principal -Settings $taskSettings
Register-ScheduledTask -TaskName $taskName -InputObject $task | Out-Null
Start-ScheduledTask -TaskName $taskName
# The supervisor writes status.json as soon as it runs. Some hosts leave the task "Queued" and
# never run it; start the supervisor through WMI then, also as SYSTEM and outside the rport service.
$supervisorRunning = $false
for ($i = 0; $i -lt 20; $i++) {
    if (Test-Path "$stage\status.json")
    {
        $supervisorRunning = $true
        break
    }
    Start-Sleep -Seconds 1
}
if ($supervisorRunning)
{
    Write-Output "* Update supervisor started as scheduled task $taskName."
}
else
{
    Write-Output "* Scheduled task $taskName did not start; starting the supervisor through WMI."
    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
    $cmdLine = "powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$stage\supervise.ps1`" -Settings `"$settingsFile`""
    $wmiResult = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = $cmdLine }
    if ($wmiResult.ReturnValue -ne 0)
    {
        throw "Starting the update supervisor failed (Win32_Process.Create returned $( $wmiResult.ReturnValue )). The client was not restarted; the previous config is in $stage\previous."
    }
    Write-Output "* Update supervisor started as process $( $wmiResult.ProcessId )."
}
Set-Location $myLocation
Get-Log

if (Test-RportSubprocess)
{
    Write-Output "* The update runs in the background, because this script was started by the rport client."
    Write-Output "* The client restarts; if $targetVersion does not connect within $( $healthTimeout )s, $currentVersion is restored."
    Write-Output "* Result: $stage\status.json, log: $stage\update.log"
    exit 0
}

Write-Output "* Waiting for the new client to connect (up to $( $healthTimeout )s, then $( $stableSecs )s to confirm) ..."
$limit = 2 * ($healthTimeout + $stableSecs) + 120
for ($i = 0; $i -lt $limit; $i += 2) {
    if ((Test-Path "$stage\status.json") -and -not ((Get-Content -Raw "$stage\status.json") -match '"status":"running"'))
    {
        break
    }
    Start-Sleep -Seconds 2
}
$result = Get-Content -Raw "$stage\status.json" -ErrorAction SilentlyContinue
Write-Output $result
if ($result -match '"status":"success"')
{
    Write-Output "
#  Update of the Procy client (rport) finished: $targetVersion is connected.
#  Update log: $stage\update.log
"
    exit 0
}
Write-Output "
# -------------!!   ERROR  !!-------------
# The update did not succeed; see the result above and $stage\update.log.
# If the new version did not connect, the previous version was restored.
"
exit 1
