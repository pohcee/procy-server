# Procy OS updates (Windows): installs pending Windows updates and restarts the machine if they need it.
# Uses the built-in Windows Update service (no extra modules), so it works alongside Windows' own
# automatic updates and WSUS: it waits while another update install is running. Skipped: driver updates,
# optional (preview) updates, feature upgrades to a new Windows version and updates that need someone at the screen.
# Large cumulative updates that need a second download (0x8024200D) are downloaded and installed once more.
# Restart: when an update needs one, the machine restarts 2 minutes after this script reports. Set
# $Reboot = 'never' below to skip restarts.
# Result: C:\ProgramData\Procy\os-updates\last.json
$Reboot = 'auto'
$ErrorActionPreference = 'Stop'
$stateDir = Join-Path $env:ProgramData 'Procy\os-updates'
$started = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')

function Write-Log([string]$Message) {
  Write-Output ((Get-Date).ToUniversalTime().ToString('HH:mm:ss') + " $Message")
}

function Write-Result([string]$State, [int]$Updated, [string]$RebootState, [string]$Message) {
  New-Item -ItemType Directory -Force -Path $stateDir | Out-Null
  $o = [ordered]@{
    status = $State; updated = $Updated; reboot = $RebootState; started = $started
    finished = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ'); message = $Message
  }
  [IO.File]::WriteAllText((Join-Path $stateDir 'last.json'), ($o | ConvertTo-Json -Compress), (New-Object System.Text.UTF8Encoding $False))
  Write-Output "RESULT: $State - $Message"
}

function Stop-WithError([string]$Message) {
  Write-Result 'failed' 0 'no' $Message
  exit 1
}

function Test-RebootPending {
  $keys = @(
    'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\WindowsUpdate\Auto Update\RebootRequired',
    'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending'
  )
  foreach ($k in $keys) {
    if (Test-Path $k) {
      return $true
    }
  }
  return $false
}

$hr = @{ 0 = 'not started'; 1 = 'in progress'; 2 = 'succeeded'; 3 = 'succeeded with errors'; 4 = 'failed'; 5 = 'aborted' }

try {
  $os = (Get-CimInstance Win32_OperatingSystem).Caption
  Write-Log "OS updates on $env:COMPUTERNAME ($os)"
  $wu = Get-Service wuauserv -ErrorAction SilentlyContinue
  if (-not $wu) {
    Stop-WithError 'the Windows Update service (wuauserv) is missing'
  }
  if ($wu.StartType -eq 'Disabled') {
    Stop-WithError 'the Windows Update service (wuauserv) is disabled; enable it to install updates'
  }

  $session = New-Object -ComObject Microsoft.Update.Session
  $session.ClientApplicationID = 'Procy OS updates'
  $installer = $session.CreateUpdateInstaller()
  for ($i = 0; $installer.IsBusy -and $i -lt 120; $i++) {
    if ($i -eq 0) {
      Write-Log 'another update install is running; waiting for it (up to 20 minutes)'
    }
    Start-Sleep -Seconds 10
  }
  if ($installer.IsBusy) {
    Stop-WithError 'another update install was still running after 20 minutes'
  }

  Write-Log 'searching for updates'
  $searcher = $session.CreateUpdateSearcher()
  $found = $searcher.Search("IsInstalled=0 and IsHidden=0 and Type='Software'")
  $todo = New-Object -ComObject Microsoft.Update.UpdateColl
  foreach ($u in $found.Updates) {
    $cats = @($u.Categories | ForEach-Object { $_.Name })
    if ($u.BrowseOnly -or ($cats -contains 'Upgrades') -or $u.Title -match '\bPreview\b' -or $u.InstallationBehavior.CanRequestUserInput) {
      Write-Log "  skipped: $( $u.Title )"
      continue
    }
    if (-not $u.EulaAccepted) {
      $u.AcceptEula()
    }
    $sec = ''
    if ($u.MsrcSeverity) {
      $sec = " [security: $( $u.MsrcSeverity )]"
    }
    Write-Log "  $( $u.Title )$sec"
    $null = $todo.Add($u)
  }
  $count = $todo.Count
  Write-Log "$count update(s) to install"

  $updated = 0
  $needReboot = $false
  if ($count -gt 0) {
    Write-Log 'downloading'
    $downloader = $session.CreateUpdateDownloader()
    $downloader.Updates = $todo
    $dl = $downloader.Download()
    Write-Log "download $( $hr[[int]$dl.ResultCode] )"

    $ready = New-Object -ComObject Microsoft.Update.UpdateColl
    foreach ($u in $todo) {
      if ($u.IsDownloaded) {
        $null = $ready.Add($u)
      }
    }
    if ($ready.Count -eq 0) {
      Stop-WithError "none of the $count update(s) could be downloaded (result: $( $hr[[int]$dl.ResultCode] ))"
    }

    $needReboot = $false
    for ($pass = 1; $pass -le 2 -and $ready.Count -gt 0; $pass++) {
      Write-Log "installing $( $ready.Count ) update(s)"
      $installer.Updates = $ready
      $res = $installer.Install()
      $retry = New-Object -ComObject Microsoft.Update.UpdateColl
      for ($i = 0; $i -lt $ready.Count; $i++) {
        $r = $res.GetUpdateResult($i)
        $state = $hr[[int]$r.ResultCode]
        if ($r.ResultCode -eq 2 -or $r.ResultCode -eq 3) {
          $updated++
        } else {
          $state = "$state (0x{0:X8})" -f $r.HResult
          # WU_E_UH_NEEDANOTHERDOWNLOAD: the update needs another download before it can install.
          if ($pass -eq 1 -and ('{0:X8}' -f $r.HResult) -eq '8024200D') {
            $null = $retry.Add($ready.Item($i))
            $state += ', downloading again'
          }
        }
        Write-Log "  $state`: $( $ready.Item($i).Title )"
      }
      $needReboot = $needReboot -or [bool]$res.RebootRequired
      $ready = $retry
      if ($ready.Count -gt 0) {
        $downloader.Updates = $ready
        $downloader.IsForced = $true
        $dl = $downloader.Download()
        Write-Log "download $( $hr[[int]$dl.ResultCode] )"
      }
    }
    if ($updated -eq 0) {
      Stop-WithError "installing failed for all $count update(s) (result: $( $hr[[int]$res.ResultCode] ))"
    }
  }

  $failedCount = $count - $updated
  $summary = "installed $updated update(s)"
  if ($failedCount -gt 0) {
    $summary += ", $failedCount failed or not downloaded"
  }
  $status = 'success'
  if ($failedCount -gt 0) {
    $status = 'partial'
  }

  if ($needReboot -or (Test-RebootPending)) {
    if ($Reboot -eq 'never') {
      Write-Result $status $updated 'pending' "$summary; a restart is needed (`$Reboot = 'never')"
    } else {
      & shutdown.exe /r /t 120 /d p:2:17 /c 'Procy OS updates: restarting to finish installing updates' | Out-Null
      # 1190: a restart is already scheduled (for example by an earlier run), which is just as good.
      if ($LASTEXITCODE -ne 0 -and $LASTEXITCODE -ne 1190) {
        Stop-WithError "$summary but scheduling the restart failed (shutdown.exe exit $LASTEXITCODE)"
      }
      Write-Result $status $updated 'yes' "$summary; restarting in 2 minutes"
    }
  } else {
    Write-Result $status $updated 'no' "$summary; no restart needed"
  }
  if ($status -ne 'success') {
    exit 1
  }
  exit 0
} catch {
  Stop-WithError "Windows Update error: $( $_.Exception.Message )"
}
