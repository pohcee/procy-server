---
title: "OS update status"
weight: 50
slug: supervising-update-status
aliases:
- /advanced/supervising-update-status/
  - /docs/no16-update-status.html
---
{{< toc >}}

The Procy client regularly checks which operating system updates are pending on its device and reports a summary to
the server. You see the patch level of all your devices in one place, without signing in to each of them. This page
applies to hosted Procy and to self-hosted servers.

## Where to see it

Open a device and go to its **Inventory** tab. The **Updates status** card shows the number of pending updates and
security updates, whether a reboot is pending, and when the client last checked. It also lists each pending update.

Through the API, each device has an `updates_status` object:

```json
{
  "updates_status": {
    "refreshed": "2026-11-16T06:45:56Z",
    "updates_available": 109,
    "security_updates_available": 41,
    "update_summaries": [
      {
        "title": "libatomic1",
        "description": "libatomic1 10.3.0-1ubuntu1~20.04 Ubuntu:20.04/focal-updates, Ubuntu:20.04/focal-security [amd64]",
        "reboot_required": false,
        "is_security_update": true
      }
    ],
    "reboot_pending": false
  }
}
```

`POST /api/v1/clients/{client_id}/updates-status` asks a connected device to check again now.

To install the updates, run the built-in **OS updates** entry from the **Script library** on a device or a client
group. It installs all pending updates and restarts the device if needed.

## Supported systems

- Debian and Ubuntu, with `apt-get`
- Red Hat, CentOS and their derivatives, with `yum` or `dnf`
- SUSE, with `zypper`
- Windows, with Windows Update

## How often it checks

The client checks every 4 hours by default. Change it with `updates_interval` in the `[client]` section of
`rport.conf`, with the unit `h` (hours) or `m` (minutes):

```toml
[client]
  updates_interval = '4h'
```

`updates_interval = '0'` turns the feature off. Then a refresh from the server is refused too.

## Sudo rules (Linux)

On Debian, Ubuntu and SUSE only root can refresh the package lists, and the client runs as the unprivileged `rport`
user. The [installer](/docs/devices/install-and-run-a-client/) creates the sudo rule it needs. If you installed the
client by hand, create `/etc/sudoers.d/rport-update-status` yourself.

Debian and Ubuntu:

```text
rport ALL=NOPASSWD: SETENV: /usr/bin/apt-get update -o Debug\:\:NoLocking=true
```

SUSE:

```text
rport ALL=NOPASSWD: SETENV: /usr/bin/zypper refresh *
```
