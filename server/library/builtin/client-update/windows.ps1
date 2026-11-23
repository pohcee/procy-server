# Procy client update: installs the latest stable client only if newer, checks it reconnects and rolls back
# otherwise; result C:\Program Files\rport\update\status.json
$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
$script = 'C:\Windows\Temp\rport-update.ps1'
Invoke-WebRequest -UseBasicParsing -Uri '{{server_url}}/pairing/update' -OutFile $script
& $script -m
