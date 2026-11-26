---
title: "VNC in the browser"
weight: 40
slug: novnc-proxy
aliases:
- /advanced/novnc-proxy/
- /docs/no18-novnc-proxy.html
---
{{< toc >}}

**Self-hosted only.** A self-hosted Procy server can show a VNC desktop in your browser with
[noVNC](https://novnc.com/), so you don't need a VNC viewer, and the session travels over HTTPS instead of the
unencrypted VNC protocol. On hosted Procy, and on servers without this set up, open a VNC tunnel and use a VNC viewer
(see [managing tunnels](/docs/remote-access/managing-tunnels/#connect)).

## How it works

The server opens a tunnel to the device's VNC port and makes it available on localhost only. In front of it, the
server's HTTPS proxy serves the noVNC app on the tunnel's server port and relays the VNC traffic over a WebSocket.
Opening that address in a browser starts the session.

## Prerequisites

* The [HTTPS tunnel proxy](/docs/remote-access/managing-tunnels/#reverse-proxy-for-web-pages):
  `tunnel_proxy_cert_file` and `tunnel_proxy_key_file` in `[server]`.
* The noVNC app on the server's file system. Procy is tested with noVNC 1.3.0.

## Install noVNC

Download noVNC and unpack it where `rportd` can read it, for example:

```shell
cd /tmp
curl -LO https://github.com/novnc/noVNC/archive/refs/tags/v1.3.0.zip
sudo unzip -d /var/lib/rport v1.3.0.zip
rm -f v1.3.0.zip
```

This leaves the app in `/var/lib/rport/noVNC-1.3.0`.

## Configure rportd

Set `novnc_root` in the `[server]` section of `rportd.conf` to that directory and restart `rportd`:

```toml
[server]
  tunnel_proxy_cert_file = "/etc/rport/tunnels.crt"
  tunnel_proxy_key_file = "/etc/rport/tunnels.key"
  novnc_root = "/var/lib/rport/noVNC-1.3.0"
```

`GET /api/v1/status` then reports `"browser_vnc": true` under `capabilities`.

## Open a desktop

Create the tunnel with scheme `vnc` and the proxy turned on:

```shell
curl -s -u "$CREDS" -X PUT -G "https://procy.example.com/api/v1/clients/$CLIENT_ID/tunnels" \
  -d remote=5900 \
  -d scheme=vnc \
  -d http_proxy=1 \
  -d acl=203.0.113.7 | jq
```

Open `https://procy.example.com:<lport>/` (or the `tunnel_url` of the response, with
[subdomains](/docs/remote-access/tunnels-on-subdomains/)) and enter the VNC password of the device.
