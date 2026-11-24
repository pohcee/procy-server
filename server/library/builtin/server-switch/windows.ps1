# Procy server switch: points the client at the server's connect URLs ([server] url), checks it reconnects
# and restores the previous rport.conf otherwise; result C:\Program Files\rport\update\status.json
$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
$script = 'C:\Windows\Temp\rport-update.ps1'
Invoke-WebRequest -UseBasicParsing -Uri '{{server_url}}/pairing/update' -OutFile $script
& $script -s
