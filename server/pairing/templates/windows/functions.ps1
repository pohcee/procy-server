$InformationPreference = "continue"
$ErrorActionPreference = "Stop"
$PSDefaultParameterValues = @{ '*:Encoding' = 'utf8' }
trap
{
    "
#
# -------------!!   ERROR  !!-------------
#
# Installation or updare of rport finished with errors.
#

Error in line $( $_.InvocationInfo.ScriptLineNumber )
    $_

Try the following to investigate:
1) sc query rport

2) open C:\Program Files\rport\rport.log

"
    Set-Location $myLocation
    exit 1
}

$InstallerLogFile = $false
if (-not(Get-Command Write-Information -erroraction silentlycontinue))
{
    $InstallerLogFile = (Get-Location).path + "\rport-installer.log"
    if (Test-Path $InstallerLogFile)
    {
        Remove-Item $InstallerLogFile
    }
    Write-Output "# Compatibility mode for PowerShell $( $PSVersionTable.PSVersion ) activated"
    Write-Output "# All information stream messages are redirected to $( $InstallerLogFile )"
    function Write-Information
    {
        [Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSAvoidOverwritingBuiltInCmdlets', '', Justification = 'Applies only to old PS Versions')]
        Param(
            [parameter(Mandatory = $false)]
            [String] $MessageData = ""
        )
        Add-Content -Path $InstallerLogFile -Value $MessageData
    }
}

function Get-Log
{
    if (Test-Path $InstallerLogFile)
    {
        Write-Output ""
        Write-Output "= The following information has been logged:"
        Get-Content $InstallerLogFile
        Remove-Item $InstallerLogFile -Force
    }
}

# Extract a ZIP file
function Expand-Zip
{
    Param(
        [parameter(Mandatory = $true)]
        [String] $Path,
        [parameter(Mandatory = $true)]
        [String] $DestinationPath
    )
    if (Get-Command Expand-Archive -errorAction SilentlyContinue)
    {
        Expand-Archive -Path $Path -DestinationPath $DestinationPath -force
    }
    else
    {
        # Use a fallback for old powershells < 5
        Remove-Item (-join ($DestinationPath, "\*")) -force -Recurse
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        [System.IO.Compression.ZipFile]::ExtractToDirectory($Path, $DestinationPath)
    }
}

function Add-ToConfig
{
    [OutputType([String])]
    Param(
        [Parameter(Mandatory)]
        [Object[]]$ConfigContent,
        [parameter(Mandatory = $true)]
        [String] $Block,
        [parameter(Mandatory = $true)]
        [String] $Line
    )
    <#
    .SYNOPSIS
        Add a line to a block of a the rport toml configuration.
    #>
    if ($configContent -NotMatch "\[$block\]")
    {
        # Append the block if missing
        $configContent = "$configContent`n`n[$block]"
    }
    Write-Information "* Adding `"$Line`" to [$Block]"
    $configContent = $configContent -replace "\[$Block\]", "$&`n  $Line"
    $configContent
}

function Find-Interpreter
{
    <#
    .SYNOPSIS
        Find common script interpreters installed on the system
    #>
    $interpreters = @{
    }
    if (Test-Path -Path 'C:\Program Files\PowerShell\7\pwsh.exe')
    {
        $interpreters.add('powershell7', 'C:\Program Files\PowerShell\7\pwsh.exe')
    }
    if (Test-Path -Path 'C:\Program Files\Git\bin\bash.exe')
    {
        $interpreters.add('bash', 'C:\Program Files\Git\bin\bash.exe')
    }
    $interpreters
}

function Enable-FileReception
{
    [OutputType([String])]
    param (
        [Parameter(Mandatory)]
        [Object[]]$ConfigContent,
        [Parameter(Mandatory)]
        [Boolean]$Switch
    )

    if ($Switch)
    {
        try
        {
            $ConfigContent = Set-TomlVar -ConfigContent $ConfigContent "file-reception" -Key "enabled" -value "true"
            Write-Information "* File reception has been enabled."
        }
        catch
        {
            Write-Information ": Enabling file-reception failed."
            Write-Information ": Check the settings of [file-reception] manually and change to your needs."
        }

    }
    else
    {
        try
        {
            $ConfigContent = Set-TomlVar -ConfigContent $ConfigContent "file-reception" -Key "enabled" -value "false"
            Write-Information "* File reception has been disabled."
        }
        catch
        {
            Write-Information ": Disabling file-reception failed."
            Write-Information ": Check the settings of [file-reception] manually and change to your needs."
        }

    }


    $ConfigContent
    return
}

function Enable-InterpreterAlias
{
    <#
    .SYNOPSIS
        Push interpreters to the rport.conf
    #>
    [OutputType([Object[]])]
    param (
        [Parameter(Mandatory)]
        [Object[]]$ConfigContent
    )

    Write-Information "* Looking for script interpreters."
    $interpreters = Find-Interpreter
    Write-Information "* $( $interpreters.count ) script interpreters found."
    if ($interpreters.count -eq 0)
    {
        $ConfigContent
        return
    }
    $interpreters.keys|ForEach-Object {
        $key = $_
        $value = $interpreters[$_]
        if (Test-TomlKeyExist -ConfigContent $ConfigContent -Block "interpreter-aliases" -Key $key)
        {
            Write-Information ": $key already present in configuration."
        }
        else
        {
            $ConfigContent = Add-ToConfig -ConfigContent $configContent -Block "interpreter-aliases" -Line "$( $key ) = '$( $value )'"
        }
    }
    $configContent
}

# Install or update Tacoscript from the downloads site configured on the pairing service.
# Skipped with a notice when no Tacoscript package is published there.
function Install-Tacoscript
{
    $tacoDir = "C:\Program Files\tacoscript"
    $tacoBin = $tacoDir + '\bin\tacoscript.exe'
    if (-not $RportDownloadBase)
    {
        Write-Output "* No downloads URL configured. Skipping Tacoscript."
        return
    }
    $url = "$( $RportDownloadBase )/tacoscript/$( $release )/tacoscript_windows_x86_64.zip"
    $Temp = [System.Environment]::GetEnvironmentVariable('TEMP', 'Machine')
    $file = $Temp + "\tacoscript.zip"
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    try
    {
        Invoke-WebRequest -Uri $url -OutFile $file -UseBasicParsing
    }
    catch
    {
        Write-Output "* Tacoscript is not available from $( $url ). Skipping."
        return
    }
    $isUpdate = Test-Path -Path $tacoBin
    New-Item -ItemType Directory -Force -Path "$( $tacoDir )\bin"|Out-Null
    Expand-Zip -Path $file -DestinationPath $tacoDir
    Move-Item "$( $tacoDir )\tacoscript.exe" "$( $tacoDir )\bin\" -Force
    Remove-Item $file -Force
    if (-not $isUpdate)
    {
        $ENV:PATH = "$ENV:PATH;$( $tacoDir )\bin"
        [Environment]::SetEnvironmentVariable(
                "Path",
                [Environment]::GetEnvironmentVariable("Path", [EnvironmentVariableTarget]::Machine) + ";$( $tacoDir )\bin",
                [EnvironmentVariableTarget]::Machine
        )
    }
    Write-Output "* Tacoscript installed to '$( $tacoDir )' $( (& $tacoBin --version) -match "Version" )"
}


function Test-TomlKeyExist
{
    [OutputType([Boolean])]
    param (
        [Parameter(Mandatory)]
        [Object[]]$ConfigContent,
        [Parameter(Mandatory)]
        [String]$Block,
        [Parameter(Mandatory)]
        [String]$Key
    )
    if (-not$ConfigContent -match [Regex]::Escape("^[$( $Block )]"))
    {
        $ConfigContent
        Write-Error "Block [$( $Block )] not found in config content"
        $false
        return
    }
    $inBlock = $false
    foreach ($Line in $ConfigContent -split "`n")
    {
        if ($Line -match "^\[$( $Block )\]")
        {
            $inBlock = $true
        }
        elseif ($Line -match "^\[.*\]")
        {
            $inBlock = $false
        }
        if ($inBlock -and ($line -match "$key = ") -and ($line -notmatch "#.*$key ="))
        {
            $true
            return
        }
    }
    $false
    return
}

function Set-TomlVar
{
    [OutputType([String])]
    [CmdletBinding(SupportsShouldProcess)]
    param (
        [Parameter(Mandatory)]
        [Object[]]$ConfigContent,
        [Parameter(Mandatory)]
        [String]$Block,
        [Parameter(Mandatory)]
        [String]$Key,
        [Parameter(Mandatory)]
        [String]$Value
    )
    if (-not$ConfigContent -match [Regex]::Escape("^[$( $Block )]"))
    {
        Write-Error "Block [$( $Block )] not found in config content"
        $configContent
        return
    }
    $inBlock = $false
    $new = ""
    $ok = $false
    foreach ($Line in $configContent -split "`n")
    {
        if ($Line -match "^\[$( $Block )\]")
        {
            $inBlock = $true
        }
        elseif ($Line -match "^\[.*\]")
        {
            $inBlock = $false
        }
        if ($inBlock -and ($line -match "^([#, ])*$key = "))
        {
            $new = $new + "  $key = $value`n"
            $ok = $true
            $inBlock = $false
        }
        else
        {
            $new = $new + $line + "`n"
        }
    }
    if (-not$ok)
    {
        $e = @()
        $e += ": Key '$( $Key )' not found in config section [$( $Block )]."
        $e += ": Please add manually '$( $Key ) = `"$( $Value )`"'"
        Write-Error ($e -join "`n")
        return
    }
    if ( $PSCmdlet.ShouldProcess($ConfigContent))
    {
        Write-Debug $new
    }
    $new
    return
}

function Add-Netcard
{
    [OutputType([Object[]])]
    param (
        [Parameter(Mandatory)]
        [Object[]]$ConfigContent,
        [Parameter(Mandatory)]
        [CimInstance[]]$Interface,
        [Parameter(Mandatory)]
        [ValidateSet('net_lan', 'net_wan')]
        [String]$InterfaceType
    )
    if ($Interface.Length -gt 1)
    {
        Write-Information ""
        Write-Information "-----------------------::CAUTION::-----------------------"
        Write-Information ": You have more than one connected $( $InterfaceType ) card."
        Write-Information ": Just the first one will be activated for the monitoring."
        Write-Information ": Review the configuration file and adjust to your needs manually once the installation has finished."
        Write-Information ""
    }
    $InterfaceAlias = $Interface[0].InterfaceAlias
    $linkSpeed = ((Get-Netadapter|Where-Object Name -eq $InterfaceAlias)[0].LinkSpeed) -replace " Gbps", "000" -replace " Mbps", ""
    $linkSpeed = [math]::floor($linkSpeed);
    if (Test-TomlKeyExist -ConfigContent $ConfigContent -Block "monitoring" -Key $InterfaceType)
    {
        Write-Information "* Monitoring for $InterfaceType '$InterfaceAlias' already activated. Skipping."
        $ConfigContent
        return
    }
    try
    {
        $ConfigContent = Set-TomlVar -ConfigContent $ConfigContent -Block "monitoring" -Key $InterfaceType -Value "['$InterfaceAlias', '$linkSpeed']"
        Write-Information "* Monitoring for $InterfaceType '$InterfaceAlias' activated."
    }
    catch
    {
        Write-Information ": Monitoring for $InterfaceType '$InterfaceAlias' NOT activated."
        Write-Information $_
    }
    $ConfigContent
}

function Select-EnabledNetCard
{
    [OutputType([Object[]])]
    param (
        [Parameter(Position = 1, ValueFromPipeline = $true)]
        [object[]]$NetAdapters
    )
    process
    {
        $filtered = @()
        foreach ($NetAdapter in $NetAdapters)
        {
            try
            {
                if ("Up" -eq (Get-NetAdapter -Name $NetAdapter.InterfaceAlias).Status)
                {
                    $filtered += $NetAdapter
                }
            }
            catch
            {
                Write-Information ": Failed to get status of $( $NetAdapter.InterfaceAlias ). Net Adapter ignored."
            }

        }
        $filtered
        return
    }
}

function Enable-Network-Monitoring
{
    [OutputType([Object[]])]
    param (
        [Parameter(Mandatory)]
        [Object[]]$ConfigContent
    )
    if ($ConfigContent -match "^\s*net_[lw]an")
    {
        Write-Information "* Network Monitoring already enabled."
        $ConfigContent
        return
    }
    try
    {
        $netLan = (Get-NetIPAddress|Where-Object IPAddress -Match "^(10|192.168|172.16)"|Select-EnabledNetCard)
        $netWan = (Get-NetIPAddress|Where-Object AddressFamily -eq "IPv4"|Where-Object IPAddress -NotMatch "^(10|192.168|172.16|127.|169.254.)"|Select-EnabledNetCard)
    }
    catch
    {
        Write-Information ": Getting list of Network adapters with 'Get-NetIPAddress' failed. Notwork monitoring not activated."
        $ConfigContent
        return
    }

    if (-Not$netLan -and -Not$netWan)
    {
        Write-Information "* No Lan cards detected. Check manually with 'Get-NetAdapter'"
        $ConfigContent
        return
    }
    if ($netLan)
    {
        $ConfigContent = Add-Netcard -ConfigContent $ConfigContent -Interface $netLan -InterfaceType 'net_lan'
    }
    if ($netWan)
    {
        $ConfigContent = Add-Netcard -ConfigContent $ConfigContent -Interface $netWan -InterfaceType 'net_wan'
    }
    $ConfigContent
}

function Get-HostUUID
{
    try
    {
        (Get-CimInstance -Class Win32_ComputerSystemProduct).UUID
        return
    }
    catch
    {
        Write-Information ": Reading system UUID with 'Get-CimInstance -Class Win32_ComputerSystemProduct' failed."
        Write-Information ": Falling back to a md5 hash of the computer name."
        $hash = [System.Security.Cryptography.HashAlgorithm]::Create("md5").ComputeHash(
                [System.Text.Encoding]::UTF8.GetBytes($( $env:computername )))
        [System.BitConverter]::ToString($hash).Replace("-", "")
        return
    }
}

# Set the start type of the service
function Optimize-ServiceStartup
{
    param()
    #@formatter:off
    & sc.exe config rport start= delayed-auto
    & sc.exe failure rport reset= 0 actions= restart/5000
    #@formatter:on
}

function Invoke-Download
{
    Param(
        [Parameter()]
        [string]$gt = "0",
        [string]$pkgUrl
    )
    $Headers = @{ }
    if ($pkgUrl)
    {
        # Download from a custom URL given by global switch
        if ($pkgUrl -match ("^http.*windows_x86_64.zip"))
        {
            $downloadFile = "C:\Windows\temp\rport_Windows_x86_64.zip"
        }
        elseif ($pkgUrl -match ("^http.*windows_x86_64.msi"))
        {
            $downloadFile = "C:\Windows\temp\rport_Windows_x86_64.msi"
        }
        else
        {
            Write-Error "PkgUrl $( $pkgUrl ) is not a valid rport download url."
        }
        $url = $pkgUrl
        if ($env:RPORT_INSTALLER_DL_USERNAME -and $env:RPORT_INSTALLER_DL_PASSWORD)
        {
            $pair = "$( $env:RPORT_INSTALLER_DL_USERNAME ):$( $env:RPORT_INSTALLER_DL_PASSWORD )"
            $encodedCreds = [System.Convert]::ToBase64String([System.Text.Encoding]::ASCII.GetBytes($pair))
            $basicAuthValue = "Basic $encodedCreds"
            $Headers = @{
                Authorization = $basicAuthValue
            }
            Write-Information "* Downloading using HTTP basic auth"
        }
    }
    else
    {
        # Packages only come from the downloads site configured on the pairing service.
        if (-not $RportDownloadBase)
        {
            throw "No downloads URL configured (RportDownloadBase). Get this script from your pairing service."
        }
        $downloadFile = "C:\Windows\temp\rport_Windows_x86_64.zip"
        $base = "$( $RportDownloadBase )/rport/$( $release )"
        $url = "$( $base )/rport_windows_x86_64.zip"
        if ($gt -ne "0")
        {
            # Updating: only download when the published version is newer.
            $latest = (Get-UrlText "$( $base )/VERSION").Trim()
            if (-not (Test-NewerVersion -Latest $latest -Current $gt))
            {
                if (Test-Path $downloadFile -PathType leaf)
                {
                    Remove-Item $downloadFile -Force
                }
                New-Item -ItemType File -Path $downloadFile | Out-Null
                return $downloadFile
            }
        }
        $verify = $true
    }

    if (Test-Path $downloadFile -PathType leaf)
    {
        Remove-Item $downloadFile -Force
    }
    Write-Information "* Downloading  $( $url )."
    $ProgressPreference = 'SilentlyContinue'
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $downloadFile -Headers $Headers
    if ($verify)
    {
        Confirm-Checksum -Path $downloadFile -Name 'rport_windows_x86_64.zip' -SumsUrl "$( $base )/SHA256SUMS"
    }
    return $downloadFile
}

# Text content of a URL (Windows PowerShell may return bytes for text responses).
function Get-UrlText
{
    Param([string]$Url)
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $content = (Invoke-WebRequest -UseBasicParsing -Uri $Url).Content
    if ($content -is [byte[]])
    {
        return [System.Text.Encoding]::UTF8.GetString($content)
    }
    return [string]$content
}

# True if version $Latest is newer than $Current.
function Test-NewerVersion
{
    Param([string]$Latest, [string]$Current)
    try
    {
        return ([version]$Latest) -gt ([version]$Current)
    }
    catch
    {
        return $Latest -ne $Current
    }
}

# Stop unless the file matches its entry in the published SHA256SUMS.
function Confirm-Checksum
{
    Param([string]$Path, [string]$Name, [string]$SumsUrl)
    $expected = $null
    foreach ($line in ((Get-UrlText $SumsUrl) -split "`n"))
    {
        $parts = $line.Trim() -split '\s+'
        if ($parts.Count -ge 2 -and ($parts[1] -eq $Name -or $parts[1] -eq "*$Name"))
        {
            $expected = $parts[0].ToLower()
        }
    }
    if (-not $expected)
    {
        throw "No checksum published for $Name."
    }
    $actual = (Get-FileHash -Algorithm SHA256 -Path $Path).Hash.ToLower()
    if ($actual -ne $expected)
    {
        Remove-Item $Path -Force
        throw "Checksum mismatch for $Name (expected $expected, got $actual)."
    }
    Write-Output "* Checksum of $Name verified." | Out-Host
}

# Create an uninstaller script for rport
function New-Uninstaller
{
    [CmdletBinding(SupportsShouldProcess)]
    param()

    Set-Content -Path "$( $installDir )\uninstall.bat" -Value '
ECHO off
net session > NUL
IF %ERRORLEVEL% EQU 0 (
    ECHO You are Administrator. Fine ...
) ELSE (
    ECHO You are NOT Administrator. Exiting...
    PING -n 5 127.0.0.1 > NUL 2>&1
    EXIT /B 1
)
echo Removing rport now
ping -n 5 127.0.0.1 > null
sc stop rport
"%PROGRAMFILES%"\rport\rport.exe --service uninstall -c "%PROGRAMFILES%"\rport\rport.conf
cd C:\
rmdir /S /Q "%PROGRAMFILES%"\rport\
echo RPort removed
ping -n 2 127.0.0.1 > null
'
    Write-Output "* Uninstaller created in $( $installDir )\uninstall.bat."
}

function New-PSScriptFile
{
    [CmdletBinding(SupportsShouldProcess)]
    param
    (
        [Parameter(Mandatory = $true)]
        [string] $Path,
        [Parameter(Mandatory = $true)]
        [string] $ScriptBlock
    )
    $ScriptBlock.Split("`n") | ForEach-Object {
        if ($_)
        {
            $_.Trim() | Out-File -FilePath $Path -Append
        }
    }
    $null = $Path
}

function Get-MSIVersionInfo
{
    param (
        [parameter(Mandatory = $true)]
        [ValidateNotNullOrEmpty()]
        [System.IO.FileInfo] $path
    )
    if (!(Test-Path $path.FullName))
    {
        throw "File '{0}' does not exist" -f $path.FullName
    }
    try
    {
        $WindowsInstaller = New-Object -com WindowsInstaller.Installer
        $Database = $WindowsInstaller.GetType().InvokeMember("OpenDatabase", "InvokeMethod", $Null, $WindowsInstaller, @($path.FullName, 0))
        $Query = "SELECT Value FROM Property WHERE Property = 'ProductVersion'"
        $View = $database.GetType().InvokeMember("OpenView", "InvokeMethod", $Null, $Database, ($Query))
        $View.GetType().InvokeMember("Execute", "InvokeMethod", $Null, $View, $Null) | Out-Null
        $Record = $View.GetType().InvokeMember("Fetch", "InvokeMethod", $Null, $View, $Null)
        $Version = $Record.GetType().InvokeMember("StringData", "GetProperty", $Null, $Record, 1)
        return $Version
    }
    catch
    {
        throw "Failed to get MSI file version: {0}." -f $_
    }
}