---
title: 'Monitoring'
weight: 17
slug: monitoring
aliases:
  - /docs/no17-monitoring.html
---

{{< toc >}}

## Preface

Procy comes with basic monitoring. The Procy client can collect the following metrics from the underlying operating
system:

* CPU usage (percent)
* Memory usage (percent)
* IO usage (percent)
* List of running processes
* Fill level of hard disks and mount points

The monitoring is enabled by default. All monitoring data is stored on the server in a sqlite3 database file `monitoring.db` inside the data dir
of the Procy server.

## Server configuration options

Choose `data_storage_duration` in the `[monitoring]` section of the `rportd.conf` carefully (default `"7d"`; use the
suffix `d` for days or `h` for hours). The older `data_storage_days` setting is deprecated.
The more clients you have, the more data will be collected. Having hundreds of clients collecting monitoring data, the
database file can quickly grow to 10 Gigabytes or more. Use a symbolic link, if you want to store the `monitoring.db`
file outside the data dir.

## Client configuration options

If you client configuration after an update does not contain a `[monitoring]` section, copy it from the
[rport.example.conf](https://github.com/pohcee/procy-server/blob/main/rport.example.conf).
To save bandwidth and disk space on the server, you can disable the monitoring for clients completely.
Please refer to the documentation inside the configuration example to explore all options of the monitoring.

## Fetching monitoring data

All collected monitoring data can be fetched using the API. Please refer to our
[API docs](https://procy.sh/docs/api/).

## Processing monitoring data

Neither the client nor the server processes the monitoring data. Procy doesn't send alerts based on thresholds;
fetch the data through the API and feed it into your own alerting if you need that.
