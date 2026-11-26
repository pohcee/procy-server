---
title: "On-demand tunnels"
weight: 20
slug: on-demand-tunnels
aliases:
- /get-started/on-demand-tunnels/
- /docs/no01-on-demand-tunnels.html
---
{{< toc >}}

Procy opens tunnels only when you ask for one and closes them when they are idle. This page explains how that
works and what a self-hosted server needs for it. On hosted Procy it works out of the box: install the client and
[create a tunnel](/docs/remote-access/managing-tunnels/).

![On-demand tunnels: the client connects out and waits; you request a tunnel through the API; the server opens a port and tells the client over the existing connection; you connect to that port and land on the client's port 22](/docs/images/procy-on-demand-tunnel.svg)

## How it works

1. The client connects out to the server and keeps that one connection open. No port is open on the server for it
   yet, and nothing is open on the client's network.
2. You request a tunnel in the web interface or through the API.
3. The server opens a port from its tunnel range and tells the client, over the existing connection, which port on
   the device (or on a host the device can reach) to forward to.
4. You connect to the server port. Only the addresses in the tunnel's access list get through.
5. When the tunnel has been idle for its timeout (5 minutes by default), or its fixed lifetime ends, the server
   closes it and the port is free again.

Compared with tunnels the client opens at start (`remotes` in `rport.conf`), on-demand tunnels keep nothing exposed
while nobody is working on a device, and you choose who may connect each time.

## What a self-hosted server needs

**Self-hosted only.**

* **The API.** Tunnels are requested through the API, so `[api] address` must be set in `rportd.conf` (the web
  interface uses the same API). See the [quick start](/docs/self-hosting/quick-start/) and
  [the web interface](/docs/self-hosting/web-ui/).
* **A tunnel port range** that clients can be reached on from where you work. `used_ports` in `[server]` sets the
  range (default `20000-30000`), and `excluded_ports` removes ports from it (default `1-1024`). Open that range in
  the server's firewall for the addresses you connect from.
* **A tunnel host**, if the API sits behind a reverse proxy that can't pass raw TCP. Set `[server] tunnel_host` to a
  name that reaches `rportd` directly; links and connect commands for tunnels then use it.

```toml
[server]
  used_ports = ['20000-30000']
  excluded_ports = ['1-1024']
  #tunnel_host = "tunnels.procy.example.com"
```

Check the server with `curl -s -u admin:$PASSWORD https://procy.example.com/api/v1/status | jq`. The response has a
`capabilities` object that lists the features this server offers, for example `"port_tunnels": true` and
`"http_proxy_tunnels": false`. The web interface hides what is not available.

## Connect a client without a tunnel

A client installed by the Procy installer connects without tunnels and waits. If you configure a client by hand,
leave `remotes` unset in `rport.conf`, or start it without a tunnel argument:

```shell
rport --auth client1:<CLIENT_PASSWORD> --fingerprint <FINGERPRINT> procy.example.com:8080
```

The client shows up as connected, with no tunnels. Open one when you need it, as described in
[managing tunnels](/docs/remote-access/managing-tunnels/).

## Running the API on a privileged port

**Self-hosted only.** `rportd` runs as an unprivileged user, and you should keep it that way. Unprivileged users can't
open ports below 1024, but you can give the `rportd` binary the capability to do so:

```shell
sudo setcap CAP_NET_BIND_SERVICE=+eip /usr/local/bin/rportd
```

Now you can use `0.0.0.0:443` as the API address. Run the command again every time the `rportd` binary changes, for
example after each update.
