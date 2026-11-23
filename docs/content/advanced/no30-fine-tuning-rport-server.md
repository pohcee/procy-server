---
title: "Fine-tuning the Procy server"
weight: 30
slug: fine-tuning-the-server
aliases:
  - /advanced/fine-tuing-rport-server/
---
{{< toc >}}

This page explains `max_concurrent_ssh_handshakes` in the `[server]` section of `rportd.conf`, the main setting for
servers with many clients.

## What is `max_concurrent_ssh_handshakes`?

When the server comes back after a downtime, all clients try to reconnect at once (the "thundering herd"). Each
connection starts with an SSH handshake, which costs CPU time. If too many handshakes run at the same time, they all
slow down, run into timeouts and have to start over.

`max_concurrent_ssh_handshakes` caps how many handshakes the server processes at the same time. The others wait
in a queue. By default, it's half the number of CPU cores (at least 1). `rportd` logs a warning if you set it higher than
twice the number of cores.

```text
[server]
  ## Defaults to half the number of CPU cores.
  #max_concurrent_ssh_handshakes = 4
```

## Questions

### Can I remove the limit?

No. Without a cap, the server is overloaded exactly when it needs its resources most: during mass reconnections.

### What happens if I raise it?

More handshakes run at the same time. That only helps if the server has the CPU to finish them before the clients time
out. Setting it to 100 on a machine with 2 slow cores makes all 100 handshakes compete for CPU time; they take so long
that they all time out and no client gets connected. On a machine with 256 cores, 100 would be a conservative value.

### What happens if I set it too low?

Clients queue up and wait longer. Long waits can turn into client-side timeouts. Clients retry with an increasing
back-off, so every timeout delays the next attempt further, and it takes longer until all clients are connected again.

### How should I tune it?

* **Start with the default.** CPU is usually the limit, and half the number of cores has given the most stable results.
* **Size the server** for the peak load after a downtime: CPU, memory and network.
* **Measure.** Watch CPU usage, bandwidth and how long it takes until all clients are back after a restart. Adjust the
  value in small steps based on that.

## Further recommendations

* **Plan downtimes.** Keep restarts short and, where possible, avoid restarting during busy hours.
* **Monitor** CPU, bandwidth and the share of successful connections, especially right after a restart.
* **Test** reconnection storms in a test environment before you change the value in production.
