---
title: "RDP in the browser"
weight: 30
slug: rdp-proxy
aliases:
- /advanced/rdp-proxy/
- /docs/no19-rdp-proxy.html
---
{{< toc >}}

**Self-hosted only.** A self-hosted Procy server can show a Windows remote desktop in your browser, so you don't need
an RDP client. On hosted Procy, and on servers without this set up, open an RDP tunnel and connect with any RDP
client instead (see [managing tunnels](/docs/remote-access/managing-tunnels/#connect)).

## How it works

The server opens an RDP tunnel to the device and puts its HTTPS proxy in front of it. When you open the proxy's
address, the server shows a small login page (username, password, domain, security and keyboard). After you sign in,
the browser talks to the server over a WebSocket, and the server connects to the RDP port through the Apache
Guacamole proxy daemon `guacd`.

{{< mermaid class="text-center">}}
flowchart LR
  0[Your browser] -->|HTTPS, WebSocket| 1[rportd] --> 2[guacd] --> 3[RDP tunnel] --> 4[Procy client] --> 5[Windows RDP]
{{< /mermaid >}}

## Prerequisites

* The [HTTPS tunnel proxy](/docs/remote-access/managing-tunnels/#reverse-proxy-for-web-pages):
  `tunnel_proxy_cert_file` and `tunnel_proxy_key_file` in `[server]`.
* `guacd` (1.4.x), running where `rportd` can reach it. You don't need a full Guacamole installation, only the
  daemon, which needs no configuration.

## Install guacd

Pick one:

* Install the `guacd` package of your distribution, if it has one.
* Build the Guacamole server from source, as described in the
  [Guacamole installation guide](https://guacamole.apache.org/doc/gug/installing-guacamole.html).
* Run it in Docker, on the host network so it can reach the tunnels `rportd` opens on `127.0.0.1`:

  ```shell
  docker run -d --name=guacd --net=host --restart unless-stopped lscr.io/linuxserver/guacd
  ```

## Configure rportd

Point `rportd` at `guacd` in the `[server]` section of `rportd.conf` and restart `rportd`:

```toml
[server]
  tunnel_proxy_cert_file = "/etc/rport/tunnels.crt"
  tunnel_proxy_key_file = "/etc/rport/tunnels.key"
  guacd_address = "127.0.0.1:4822"
```

`GET /api/v1/status` then reports `"browser_rdp": true` under `capabilities`.

## Open a desktop

Create the tunnel with scheme `rdp` and the proxy turned on:

```shell
curl -s -u "$CREDS" -X PUT -G "https://procy.example.com/api/v1/clients/$CLIENT_ID/tunnels" \
  -d remote=3389 \
  -d scheme=rdp \
  -d http_proxy=1 \
  -d acl=203.0.113.7 | jq
```

Open `https://procy.example.com:<lport>/` (or the `tunnel_url` of the response, with
[subdomains](/docs/remote-access/tunnels-on-subdomains/)) and sign in with a Windows account.

The login page can be pre-filled with query parameters, for example
`/?username=Administrator&width=1280&height=800&security=nla&keyboard=en-us-qwerty`. The password can't be passed
this way.

`username`
: The account to sign in with.

`width`, `height`
: The screen size in pixels. Default 1024 × 768.

`security`
: `any`, `nla`, `nla-ext`, `tls`, `vmconnect` or `rdp`. Windows usually uses `nla`. See the
  [Guacamole documentation](https://guacamole.apache.org/doc/gug/configuring-guacamole.html#authentication-and-security).

`keyboard`
: The layout of *your* keyboard: `en-us-qwerty`, `en-gb-qwerty`, `de-de-qwertz`, `de-ch-qwertz`, `fr-fr-azerty`,
  `fr-be-azerty`, `fr-ch-qwertz`, `it-it-qwerty`, `es-es-qwerty`, `es-latam-qwerty`, `pt-br-qwerty`,
  `hu-hu-qwertz`, `ja-jp-qwerty`, `no-no-qwerty`, `sv-se-qwerty` or `tr-tr-qwerty`.
