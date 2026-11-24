# Procy client update supervisor for Windows. Written by the update script and run once by a
# scheduled task as SYSTEM, so it survives the restart of the rport service.
#
# It installs the staged client, restarts the service and checks that the new client connects to the
# server. If it does not, it restores the previous rport.exe and rport.conf and restarts again, so a
# failed update never leaves the machine unreachable.
#
# Settings come from the JSON file given as -Settings:
#   Stage, InstallDir, ConfigFile, LogFile, FromVersion, ToVersion, Kind (exe|msi), Migrate (bool),
#   HealthTimeout, StableSecs, TaskName, Change (optional: what is changed, for the messages;
#   default "<FromVersion> to <ToVersion>")
# Result: <Stage>\status.json and <Stage>\update.log, plus Application event log entries (source rport-update).
param([string]$Settings)
$ErrorActionPreference = 'Continue'
$s = Get-Content -Raw -Path $Settings | ConvertFrom-Json
$stage = $s.Stage
$ulog = Join-Path $stage 'update.log'
$exe = Join-Path $s.InstallDir 'rport.exe'
$started = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
$script:HealthError = ''
$change = "$( $s.FromVersion ) to $( $s.ToVersion )"
if ($s.Change)
{
    $change = $s.Change
}
$utf8 = New-Object System.Text.UTF8Encoding $False

function Write-UpdateLog([string]$Message, [string]$Level = 'Information')
{
    $line = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ') + " $Message"
    [IO.File]::AppendAllText($ulog, $line + "`r`n", $utf8)
    try
    {
        if (-not [System.Diagnostics.EventLog]::SourceExists('rport-update'))
        {
            New-EventLog -LogName Application -Source 'rport-update'
        }
        Write-EventLog -LogName Application -Source 'rport-update' -EventId 1000 -EntryType $Level -Message $Message
    }
    catch
    {
    }
}

function Set-UpdateStatus([string]$State, [string]$Message)
{
    $finished = ''
    if ($State -ne 'running')
    {
        $finished = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    }
    $o = [ordered]@{
        status = $State; from = $s.FromVersion; to = $s.ToVersion
        started = $started; finished = $finished; message = $Message
    }
    $tmp = Join-Path $stage 'status.json.tmp'
    [IO.File]::WriteAllText($tmp, ($o | ConvertTo-Json -Compress), $utf8)
    Move-Item -Force $tmp (Join-Path $stage 'status.json')
}

function Get-LogSize
{
    if ($s.LogFile -and (Test-Path $s.LogFile))
    {
        return (Get-Item $s.LogFile).Length
    }
    return 0
}

# Text the client logged after byte offset $Offset (all of it if the log was rotated).
function Read-LogFrom([long]$Offset)
{
    if (-not ($s.LogFile -and (Test-Path $s.LogFile)))
    {
        return ''
    }
    $fs = [IO.File]::Open($s.LogFile, 'Open', 'Read', 'ReadWrite')
    try
    {
        if ($fs.Length -lt $Offset)
        {
            $Offset = 0
        }
        $null = $fs.Seek($Offset, 'Begin')
        $reader = New-Object IO.StreamReader($fs)
        return $reader.ReadToEnd()
    }
    finally
    {
        $fs.Close()
    }
}

function Test-ClientRunning
{
    $svc = Get-Service rport -ErrorAction SilentlyContinue
    return ($svc -and $svc.Status -eq 'Running')
}

# The service runs, connects within HealthTimeout and keeps running for StableSecs after that.
function Wait-Healthy([long]$Offset)
{
    for ($i = 0; $i -lt $s.HealthTimeout; $i++) {
        if ((Read-LogFrom $Offset) -match 'Connected to ')
        {
            Start-Sleep -Seconds $s.StableSecs
            if (Test-ClientRunning)
            {
                return $true
            }
            $script:HealthError = "the client connected but stopped within $( $s.StableSecs )s"
            return $false
        }
        Start-Sleep -Seconds 1
    }
    if (Test-ClientRunning)
    {
        $script:HealthError = "the client is running but did not connect to the server within $( $s.HealthTimeout )s"
    }
    else
    {
        $script:HealthError = "the client did not start or did not connect to the server within $( $s.HealthTimeout )s"
    }
    return $false
}

function Stop-Client
{
    Stop-Service rport -Force -ErrorAction SilentlyContinue
    for ($i = 0; $i -lt 30 -and (Get-Process rport -ErrorAction SilentlyContinue); $i++) {
        Start-Sleep -Seconds 1
    }
    Get-Process rport -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 1
}

function Start-Client
{
    try
    {
        Start-Service rport -ErrorAction Stop
    }
    catch
    {
        Write-UpdateLog "starting the rport service failed: $( $_.Exception.Message )" 'Warning'
    }
}

function Install-New
{
    if ($s.Kind -eq 'msi')
    {
        if ($s.Migrate)
        {
            & $exe --service uninstall | Out-Null
        }
        $msiLog = Join-Path $stage 'rport.msi-install.log'
        $p = Start-Process msiexec.exe -Wait -PassThru -ArgumentList "/i `"$( Join-Path $stage 'new\rport.msi' )`" /qn /norestart /log `"$msiLog`""
        return ($p.ExitCode -eq 0 -or $p.ExitCode -eq 3010)
    }
    Copy-Item (Join-Path $stage 'new\rport.exe') (Join-Path $s.InstallDir 'rport.new') -Force
    Move-Item (Join-Path $s.InstallDir 'rport.new') $exe -Force
    return $true
}

function Restore-Previous
{
    Copy-Item (Join-Path $stage 'previous\rport.exe') (Join-Path $s.InstallDir 'rport.rollback') -Force
    Move-Item (Join-Path $s.InstallDir 'rport.rollback') $exe -Force
    Copy-Item (Join-Path $stage 'previous\rport.conf') $s.ConfigFile -Force
    if (-not (Get-Service rport -ErrorAction SilentlyContinue))
    {
        # An MSI migration removed the service; register the previous client again.
        & $exe --service install --config $s.ConfigFile | Out-Null
    }
}

function Complete([string]$State, [string]$Message, [int]$Code)
{
    Set-UpdateStatus $State $Message
    Unregister-ScheduledTask -TaskName $s.TaskName -Confirm:$false -ErrorAction SilentlyContinue
    exit $Code
}

Write-UpdateLog "update started: $change ($( $s.Kind ))"
Set-UpdateStatus 'running' "updating $change"

# Give the client time to report the result of the command that started the update.
Write-UpdateLog "waiting 5s so the client can report the result of the update command"
Start-Sleep -Seconds 5
$offset = Get-LogSize
Stop-Client
$installed = $false
try
{
    $installed = Install-New
}
catch
{
    Write-UpdateLog "installing failed: $( $_.Exception.Message )" 'Error'
}
if (-not $installed)
{
    Restore-Previous
    Start-Client
    Complete 'rolled_back' "installing $( $s.ToVersion ) failed; kept $( $s.FromVersion )" 1
}
Start-Client
if (Wait-Healthy $offset)
{
    Write-UpdateLog "update to $( $s.ToVersion ) succeeded: the client is connected"
    Complete 'success' "updated $change; the client is connected" 0
}

$firstError = $script:HealthError
$recent = (Read-LogFrom $offset) -split "`n" | Select-Object -Last 20
Write-UpdateLog "update to $( $s.ToVersion ) failed: $firstError. Last client output:`r`n$( $recent -join "`n" )" 'Warning'
Write-UpdateLog "rolling back to $( $s.FromVersion )" 'Warning'
Stop-Client
Restore-Previous
$offset = Get-LogSize
Start-Client
if (Wait-Healthy $offset)
{
    Write-UpdateLog "rollback to $( $s.FromVersion ) succeeded: the client is connected" 'Warning'
    Complete 'rolled_back' "$( $s.ToVersion ) failed ($firstError); rolled back to $( $s.FromVersion ), which is connected" 1
}
Write-UpdateLog "rollback to $( $s.FromVersion ) did not reconnect: $( $script:HealthError )" 'Error'
Complete 'rollback_failed' "$( $s.ToVersion ) failed ($firstError) and $( $s.FromVersion ) did not reconnect after the rollback: $( $script:HealthError )" 2
