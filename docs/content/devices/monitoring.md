---
title: "Monitoring"
weight: 60
slug: monitoring
aliases:
- /advanced/monitoring/
  - /docs/no17-monitoring.html
---
{{< toc >}}

The Procy client collects basic performance data from its device and sends it to the server. Use it to see how busy a
device is and what fills its disks. It applies to hosted Procy and to self-hosted servers. Monitoring is on by
default.

The client collects:

- CPU usage (percent)
- memory usage (percent)
- IO usage (percent)
- the fill level of disks and mount points
- the list of running processes
- optionally, the bandwidth of up to two network cards

## Where to see it

Open a device and go to its **Monitoring** tab. It shows CPU, memory and disk usage over the last 2 to 48 hours.
Users need the `monitoring` [permission](/docs/hosted/permissions-model/).

Through the API, `/api/v1/clients/{client_id}/metrics`, `/graph-metrics`, `/mountpoints` and `/processes` return the
data. See the [API reference](/docs/api/).

Procy doesn't send alerts. To alert on thresholds, fetch the data through the API and feed it into your own alerting.

## Client settings

The `[monitoring]` section of `rport.conf` controls what the client collects:

```toml
[monitoring]
  ## Set to false to stop sending monitoring data.
  enabled = true
  ## How often to collect, in seconds. Values below 60 are raised to 60.
  #interval = 60
  ## Which file systems and mount points to watch.
  #fs_type_include = ['ext3','ext4','xfs','jfs','ntfs','btrfs','hfs','apfs','exfat','smbfs','nfs']
  #fs_path_exclude = ['/mnt/*','h:']
  ## Process monitoring.
  pm_enabled = true
  #pm_max_number_monitored_processes = 500
  ## Bandwidth of two network cards: device name and maximum speed in Mbit/s.
  #net_lan = ['eth0', '1000']
  #net_wan = ['', '1000']
```

Turn monitoring off on devices where you don't need it, to save bandwidth. If a client's configuration has no
`[monitoring]` section, copy it from
[rport.example.conf](https://github.com/pohcee/procy-server/blob/main/rport.example.conf), which documents every
option.

## Server settings (self-hosted)

The `[monitoring]` section of `rportd.conf` turns monitoring off for the whole server (`enabled = false`) and sets how
long the data is kept:

```toml
[monitoring]
  #enabled = true
  ## Use the suffix d (days) or h (hours).
  #data_storage_duration = "7d"
```

Older data is deleted automatically. With SQLite the data is in `monitoring.db` in the server's data directory; with
[Postgres](/docs/self-hosting/postgres/) it's in the database. Plan the space: with hundreds of devices the data can
grow to 10 GB or more. To keep `monitoring.db` on another disk, replace it with a symbolic link.
