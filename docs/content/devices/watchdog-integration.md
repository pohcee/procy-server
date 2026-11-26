---
title: "Watchdog integration"
weight: 70
slug: watchdog-integration
aliases:
- /advanced/watchdog-integration/
---
{{< toc >}}

The Procy client reconnects on its own after a broken connection, so most devices need no watchdog. Turn on the
watchdog integration only if you see a client process that keeps running but stops trying to connect. Then a watchdog,
systemd's on Linux or your own script on Windows, can restart it. This page applies to hosted Procy and to self-hosted
servers.

## Turn it on

In `rport.conf`, in the `[connection]` section:

```toml
[connection]
  watchdog_integration = true
  ## Required: keep_alive must be on, and max_retry_count must be unlimited (-1, the default).
  keep_alive = '3m'
  max_retry_interval = '3m'
```

Restart the client.

## The state file

With the integration on, the client writes `state.json` in its data directory (`/var/lib/rport` on Linux,
`C:\Program Files\rport\data` on Windows when installed with the installer):

```json
{
  "last_update": "2026-11-15T10:37:25.643839+02:00",
  "last_update_ts": 1763195845,
  "last_state": "connected",
  "last_message": "ping to 203.0.113.10:80 succeeded within 14.682823ms"
}
```

`last_update` and `last_update_ts` (a Unix timestamp) tell when the file was last written. Base your watchdog on
them: while they keep moving forward, the client works.

`last_state` is one of:

- `initialized`: the integration has started, but the client's state isn't known yet;
- `connected`: the client is connected to the server;
- `reconnecting`: the client isn't connected and is trying to reconnect.

`last_message` is one of `ping to {server} succeeded within {latency}`, `Connected to {server} within {latency}` or
`Retrying in {time}...`.

The file is written when the client connects, on each ping to the server (every `keep_alive`), and on each
reconnection attempt (at most every `max_retry_interval`). If it's older than the larger of these two intervals,
the client hangs and should be restarted.

## Linux: the systemd watchdog

On Linux, systemd has a built-in watchdog. With the integration on, the client notices when systemd provides a
watchdog socket and reports to it on the same events. Add `WatchdogSec` to the client's service:

```shell
sudo systemctl edit rport
```

```text
[Service]
WatchdogSec=200
```

Then restart it with `sudo systemctl restart rport`. If systemd gets no report within `WatchdogSec`, it restarts the
client. Set `WatchdogSec` a little longer than `keep_alive` and `max_retry_interval`. With both at 3 minutes, 200
seconds works well. Shorter intervals let the watchdog act sooner, but the client uses more bandwidth while idle.

With `log_level = "debug"` the client logs `Using NOTIFY_SOCKET ... for systemd watchdog integration` when it found
the socket. It writes `state.json` too, so you can see what's going on.

## Windows: a scheduled task

The Windows service has no built-in watchdog. Create a scheduled task that runs a check like this every few minutes:

```powershell
$stateFile = 'C:\Program Files\rport\data\state.json'
$thresholdSec = 600

$now = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$lastUpdate = (Get-Content $stateFile | ConvertFrom-Json).last_update_ts
$diff = $now - $lastUpdate
if ($diff -gt $thresholdSec) {
    Write-Output "Procy client hangs. No activity for $diff seconds."
    Restart-Service rport
} else {
    Write-Output "Procy client is running fine. Last activity $diff seconds ago."
}
```
