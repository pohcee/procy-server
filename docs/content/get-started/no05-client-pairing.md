---
title: "Pairing clients"
weight: 5
slug: client-pairing
---
{{< toc >}}
Pairing installs a client with a one-liner. You create a pairing code on the server, run the generated command on the
device, and the installer sets up `rport` with the right server address, fingerprint and credentials. The pairing
service is built into `rportd`; it's what the "Install client" dialog of the web UI uses.

![Pairing: create a code in the web UI, run the one-liner on the device, the installer sets up rport, and the client connects](https://raw.githubusercontent.com/pohcee/openrport/main/docs/static/images/procy-pairing.svg)

## Enable pairing

Add a `[pairing]` section to `/etc/rport/rportd.conf`:

```text
[pairing]
  enabled = true
  ## Public base URL of rportd used in the one-liners. Defaults to api.base_url,
  ## and without that to the scheme and host of each request.
  url = "https://procy.example.com"
  ## Where the installer downloads the client packages from. Required.
  downloads_url = "https://procy.example.com/downloads"
  ## How long a code stays valid. Defaults to 1h.
  #code_ttl = "1h"
  ## A code is invalidated by its first successful download. Set to true to allow reuse until it expires.
  #allow_code_reuse = false
```

Restart the server with `sudo systemctl restart rportd.service`.

When pairing is enabled, `rportd` serves these routes next to the API:

| Route                 | Access               | Returns                                                           |
|-----------------------|----------------------|-------------------------------------------------------------------|
| `POST /pairing`       | API administrators   | a pairing code and the Linux and Windows one-liners               |
| `GET /pairing/<code>` | anyone with the code | the installer: PowerShell for PowerShell clients, shell otherwise |
| `GET /pairing/update` | anyone               | the client update script                                          |

The [status API](https://procy.sh/docs/api/) reports `<url>/pairing` as `pairing_url`, so the web UI finds it
without further setup. `[server] pairing_url` points the web UI at an external pairing service instead; it's ignored
while the built-in one is enabled.

## Serve the client packages

The installer doesn't download from GitHub. It expects the client packages under `downloads_url` in this layout:

```text
<downloads_url>/rport/stable/VERSION
<downloads_url>/rport/stable/SHA256SUMS
<downloads_url>/rport/stable/rport_linux_<arch>.tar.gz
<downloads_url>/rport/stable/rport_windows_x86_64.zip
```

`VERSION` holds the version number, and `SHA256SUMS` the checksums of the archives; the installer stops if a
download doesn't match. Any static web server works, for example a folder under the `doc_root` of `rportd`. Without
`downloads_url` the generated scripts refuse to run.

## Pair a client

In the web UI, open "Install client". The UI deposits the new client's credentials with `POST /pairing` and shows
the one-liners. On Linux:

```shell
curl https://procy.example.com/pairing/<code> > rport-installer.sh
sudo sh rport-installer.sh
```

On Windows, in PowerShell as Administrator:

```powershell
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$url="https://procy.example.com/pairing/<code>"
Invoke-WebRequest -Uri $url -OutFile "rport-installer.ps1"
powershell -ExecutionPolicy Bypass -File .\rport-installer.ps1
```

The installer downloads and verifies the client, writes `rport.conf`, and installs and starts the service. The new
client then connects and appears in the web UI.

You can also create a code through the API. It needs an administrator and client credentials that the server accepts,
for example ones created with the [`/clients-auth` endpoint](/docs/get-started/client-authentication/#manage-client-credentials-via-the-api):

```shell
curl -s -u admin:$PASSWORD https://procy.example.com/pairing \
  -H "Content-Type: application/json" \
  --data-raw '{
    "connect_url": "http://procy.example.com:8080",
    "fingerprint": "<FINGERPRINT>",
    "client_id": "client7",
    "password": "<CLIENT_PASSWORD>"
  }' | jq
```

The response holds `pairing_code`, `expires` and the one-liners in `installers.linux` and `installers.windows`.

## Security notes

* A pairing code gives access to the client credentials embedded in the installer. Codes are 20 random characters,
  single-use by default and expire after `code_ttl`.
* Codes are kept in memory. Restarting `rportd` invalidates all open codes.
* Failed code lookups are rate-limited per IP address, and the responses are not cached.
* Serve `rportd`, and therefore the pairing routes, over [HTTPS](/docs/get-started/securing-rportd-with-https/).
