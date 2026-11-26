---
title: "Pairing and updating clients"
weight: 20
slug: client-pairing
aliases:
- /get-started/client-pairing/
---
{{< toc >}}

Pairing installs a client with a one-line command. You create a pairing code, run the generated command on the device,
and the installer sets up `rport` with the right server address, fingerprint and credentials. The same service gives
you a script to update clients safely. On hosted Procy pairing is always on. On a self-hosted server you enable it in
`rportd.conf`. For the steps in the web interface, see [Add a device](/docs/devices/install-and-run-a-client/).

![Pairing: create a code in the web UI, run the one-liner on the device, the installer sets up rport, and the client connects](/docs/images/procy-pairing.svg)

## How pairing works

1. The web interface creates client credentials and deposits them with the server (`POST /pairing`). The server
   answers with a pairing code and the Linux and Windows commands.
2. On the device, the command downloads `<server>/pairing/<code>`. The server returns an installer with the
   credentials, the connect URL and the server's fingerprint built in: a shell script for `curl` and `wget`,
   PowerShell for `Invoke-WebRequest`.
3. The installer downloads the client package, checks it against the published checksums, writes `rport.conf`,
   installs the service and starts it. The client connects.

On hosted Procy the server is `https://procy.sh` and clients connect to `https://connect.procy.sh`.

### Security

- A pairing code gives access to the client credentials in the installer. Codes are 20 random characters, work once
  and expire after an hour (self-hosted: `code_ttl`).
- Codes are kept in memory. On a self-hosted server, restarting `rportd` invalidates all open codes.
- Failed code lookups are rate-limited per IP address, and the answers are never cached.
- The installer refuses a package whose checksum doesn't match.

## Enable pairing (self-hosted)

Add a `[pairing]` section to `/etc/rport/rportd.conf`:

```toml
[pairing]
  enabled = true
  ## Public base URL of rportd used in the commands. Defaults to [api] base_url,
  ## and without that to the scheme and host of each request.
  url = "https://procy.example.com"
  ## Where the installer downloads the client packages from. Required.
  downloads_url = "https://procy.example.com/downloads"
  ## How long a code stays valid. Defaults to 1h.
  #code_ttl = "1h"
  ## A code stops working after its first download. Set to true to allow reuse until it expires.
  #allow_code_reuse = false
```

List the addresses clients should connect to in `[server] url`. The first one is the default in the web interface.
Unspecified and loopback addresses (`0.0.0.0`, `127.0.0.1`, `localhost`) are never given to clients.

```toml
[server]
  url = ["https://connect.example.com", "http://connect.example.com"]
```

Restart the server with `sudo systemctl restart rportd.service`. Serve `rportd` over
[HTTPS](/docs/self-hosting/securing-rportd-with-https/), since the installers carry credentials.

With pairing enabled, `rportd` serves these routes:

| Route | Access | Returns |
| --- | --- | --- |
| `POST /pairing` | API administrators | a pairing code and the Linux and Windows commands |
| `GET /pairing/<code>` | anyone with the code | the installer |
| `GET /pairing/update` | anyone | the client update script |

The [status API](/docs/api/) reports `<url>/pairing` as `pairing_url`, so the web interface finds it without more
setup.

### Serve the client packages

The installer doesn't download from GitHub. It expects the client packages under `downloads_url` in this layout:

```text
<downloads_url>/rport/stable/VERSION
<downloads_url>/rport/stable/SHA256SUMS
<downloads_url>/rport/stable/rport_linux_<arch>.tar.gz
<downloads_url>/rport/stable/rport_windows_x86_64.zip
```

`VERSION` holds the version number and `SHA256SUMS` the checksums of the archives. `<arch>` is the output of
`uname -m`, with `aarch64` as `arm64` and `armv6l`/`armv7l` as `armv6`/`armv7`. Take the archives from the
[releases page](https://github.com/pohcee/procy-server/releases/latest) and rename them without the version. For
the `-t` (unstable) option, publish the same layout under `rport/unstable/`. To offer Tacoscript, publish it under
`<downloads_url>/tacoscript/stable/`.

Any static web server works, for example a folder under the `doc_root` of `rportd`.

### Create a code through the API

You can create a code without the web interface. You need an administrator and client credentials the server
accepts, for example from the [`/clients-auth` endpoint](/docs/self-hosting/client-authentication/):

```shell
curl -s -u admin:$TOKEN https://procy.example.com/pairing \
  -H "Content-Type: application/json" \
  --data-raw '{
    "connect_url": "https://connect.example.com",
    "fingerprint": "<FINGERPRINT>",
    "client_id": "client7",
    "password": "<CLIENT_PASSWORD>"
  }'
```

The answer holds `pairing_code`, `expires` and the commands in `installers.linux` and `installers.windows`. The
fingerprint is shown under **Settings → Server info**.

## Updating clients

`GET /pairing/update` returns the client update script. Run it as root on Linux or as an administrator on Windows,
on the device or as a remote command. The easiest way is the built-in library entry **Client update** in the
**Script library**: run it on one device, a client group or all of them.

By hand on Linux:

```shell
curl -fsS https://procy.sh/pairing/update -o rport-update.sh && sudo sh rport-update.sh -n -m
```

On Windows:

```powershell
Invoke-WebRequest -UseBasicParsing https://procy.sh/pairing/update -OutFile rport-update.ps1
powershell -ExecutionPolicy Bypass -File .\rport-update.ps1 -m
```

On a self-hosted server, use your own server's address instead of `https://procy.sh`.

The update is supervised, so a failed update doesn't leave a device unreachable:

1. The script downloads the latest client, checks it against `SHA256SUMS`, and backs up the current program and
   `rport.conf`.
2. A supervisor, started separately from the client (a `systemd-run` unit on Linux, a task running as SYSTEM on
   Windows), stops the client, installs the new version and starts it again.
3. The new client must connect to the server within 120 seconds and keep running for 20 more. If it doesn't, the
   supervisor restores the previous program and configuration and restarts the client.

Run as a remote command, the script returns as soon as the supervisor has started, and the client restarts a few
seconds later. The outcome is in `/var/lib/rport-update/status.json` on Linux and
`C:\Program Files\rport\update\status.json` on Windows, next to an `update.log`. Windows also logs to the
Application event log (source `rport-update`). `status` is `success`, `rolled_back` or `rollback_failed`.

Use `-t` for the unstable channel. To install a specific package, pass `-z <url or file>` on Linux (`tar.gz`, `deb`
or `rpm`; set `PKG_SHA256` to check it) or `-pkgUrl <url or file>` on Windows. Run the script with `-h` for all
options.

{{< hint type=note >}}
Windows before Windows 11 and Server 2022 can't use TLS 1.3. On a self-hosted server with such devices, keep
`tls_min` in `[api]` at its default, `"1.2"`. With `"1.3"` they can't download the installer, the update script or
the client.
{{< /hint >}}

## Moving clients to a new server address (self-hosted)

Clients connect to `server` in their `rport.conf`, and try `fallback_servers` when it can't be reached. To move them
to a new address, for example after a domain change, keep the old address working until every client has moved:

1. Point the new names at the server, and list them in `[server] url` in `rportd.conf`, the one clients should use
   first. The others become the clients' fallback servers:

   ```toml
   [server]
     url = ["http://connect.example.com", "http://eu.connect.example.com"]
   ```

   Restart `rportd`. New install commands now use the first URL.
2. Run the built-in library entry **Switch server** on a few test clients, then on the rest. It changes only
   `server` and `fallback_servers` in the `[client]` section and restarts the client under the same supervisor as an
   update: if the client doesn't reconnect within two minutes, the previous `rport.conf` is restored. The result is
   in the same `status.json`.
3. Watch progress. Each device shows the host name it connected with: **Connected via** on the device's Overview
   tab, `connected_host` in the API:

   ```text
   GET /api/v1/clients?fields[clients]=id,name,connected_host,disconnected_at
   ```

   A client has moved when its `connected_host` is one of the new names. An empty `connected_host` means the client
   hasn't connected since the server started recording it: count it as not moved. Offline clients keep the name of
   their last connection until they come back and are switched.
4. Keep the old name pointing at the server until every client shows a new name, plus a grace period for devices that
   are rarely online.
