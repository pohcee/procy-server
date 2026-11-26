---
title: "Fine-tuning the server"
weight: 110
slug: fine-tuning-the-server
aliases:
- /advanced/fine-tuning-the-server/
- /advanced/fine-tuing-rport-server/
---
{{< toc >}}

Tune a self-hosted server with many clients so they all reconnect quickly after a restart. The main setting is
`max_concurrent_ssh_handshakes` in the `[server]` section of `rportd.conf`.

## What it does

When the server comes back after a downtime, all clients try to reconnect at once (the "thundering herd"). Each
connection starts with an SSH handshake, which costs CPU time. If too many handshakes run at the same time, they all
slow down, run into timeouts and start over.

`max_concurrent_ssh_handshakes` caps how many handshakes run at the same time; the others wait in a queue. The default
is half the number of CPU cores (at least 1). `rportd` logs a warning if you set it above twice the number of cores.

```text
[server]
  ## Defaults to half the number of CPU cores.
  #max_concurrent_ssh_handshakes = 4
```

## Questions

### Can I remove the limit?

No. Without a cap, the server is overloaded exactly when it needs its resources most: during mass reconnections.

### What happens if I raise it?

More handshakes run at once. That only helps if the server has the CPU to finish them before clients time out. On a
machine with 2 slow cores, 100 handshakes compete for CPU time, all time out, and no client gets connected. On a machine
with 256 cores, 100 would be a conservative value.

### What happens if I set it too low?

Clients queue up and wait longer, and long waits turn into client-side timeouts. Clients retry with an increasing
back-off (up to their `max_retry_interval`), so every timeout delays the next attempt, and it takes longer until all
clients are back.

### How should I tune it?

* **Start with the default.** CPU is usually the limit, and half the number of cores has given the most stable results.
* **Size the server** for the peak after a downtime: CPU, memory and network.
* **Measure** CPU usage, bandwidth and how long it takes until all clients are back after a restart, then adjust in
  small steps.

## Further recommendations

* **Plan downtimes.** Keep restarts short and avoid busy hours.
* **Monitor** CPU, bandwidth and the share of successful connections, especially right after a restart.
* **Test** reconnection storms in a test environment before changing the value in production.
* **Spread the load.** With [several nodes on Postgres](/docs/self-hosting/postgres/#run-several-nodes) and peer calls,
  any number of nodes accept clients, and the setting applies to each one.
