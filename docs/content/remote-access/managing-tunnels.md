---
title: "Managing tunnels"
weight: 10
slug: managing-tunnels
aliases:
- /get-started/managing-tunnels/
- /docs/no09-managing-tunnels.html
- /docs/content/get-started/no09-managing-tunnels.md
---
{{< toc >}}

A tunnel makes one port on a device (SSH, remote desktop, a web admin page) reachable through the Procy server,
without a VPN and without opening a port on the device's network. This page covers creating, using and closing
tunnels in the web interface and through the API. It applies to hosted Procy and to self-hosted servers.

![On-demand tunnels: the device connects out and waits; you request a tunnel; the server opens a port and tells the device over its existing connection; you connect to that port and land on the device's port](/docs/images/procy-on-demand-tunnel.svg)

The device keeps one outbound connection to the server. When you ask for a tunnel, the server opens a port (the
**server port**) and forwards everything that arrives there, over the device's connection, to a port on the device
(the **device port**). Nothing is open until you ask, and tunnels close themselves when they are idle.

To create tunnels your user group needs the **tunnels** permission (see [permissions](/docs/hosted/permissions-model/)),
and you only see the devices you have access to.

## Create a tunnel in the web interface

You can start from the **Tunnels** page (pick the device under **New tunnel**) or from a device's page, tab
**Tunnels**, **Add tunnel**. Both show the same form.

1. **Service**: pick what you want to reach. The device port is filled in for you:

   | Service | Device port |
   | --- | --- |
   | SSH | 22 |
   | RDP | 3389 |
   | VNC | 5900 |
   | HTTP | 80 |
   | HTTPS | 443 |
   | Other | any port; choose TCP, UDP or TCP + UDP |
   | Forwarding | a port on *another* host the device can reach (see [service forwarding](#reach-other-hosts-through-a-device)) |

   Change **Device port** if the service listens elsewhere.
2. **Server port**: leave **Random free port**. The server picks a free port from its tunnel range.
3. **Allowed from**: who may connect to the server port.
   * **Only my current IP address** (default): only the public address you are browsing from.
   * **Specific network range**: a comma-separated list of IP addresses or CIDR ranges, for example
     `198.51.100.0/24,203.0.113.7`. Only IPv4 is supported.
   * **No restrictions (anyone can access)**: anyone who finds the port can reach the device. You must tick
     **I am aware of the risk**. Avoid this for anything sensitive.
4. **Close automatically**:
   * **After inactivity of _N_ min** (on by default, 5 minutes): the tunnel closes when no data has passed for that
     long. Untick it to keep the tunnel open while idle.
   * **Destroy after _h_ h _m_ min**: closes the tunnel after a fixed time, whether it is used or not (at most 31
     days).
5. Optionally tick **Store in library for later re-use** and give it a name. The tunnel's settings are saved on the
   device's **Tunnels** tab under **Stored tunnels and service forwarding**.
6. Click **Create tunnel**.

If the server can't create the tunnel, the form says so. The usual causes are that nothing listens on the device
port, or a tunnel to that port with the same access list already exists.

{{< hint type=note >}}
The server checks that the device port is listening before it opens the tunnel. A device can also limit which
targets tunnels may reach with `tunnel_allowed` in its `rport.conf`; a tunnel outside that list is refused with
"Tunnel destination is not allowed by client configuration."
{{< /hint >}}

## Connect

Active tunnels are listed on the **Tunnels** page and on the device's **Tunnels** tab, with the server port, who is
allowed to connect and when the tunnel closes. Select a tunnel on the Tunnels page to see **Connect to _device_**
with a ready-made command and a **Copy** button. The **Connect** (or **Open**) button launches a client for you:

* **SSH**: asks for the username on the device, then opens your system's SSH client through an `ssh://` link, or
  the Secure Shell extension for Chrome if you turned that on. The command it shows looks like
  `ssh -p 20417 <user>@usc1-1.connect.procy.sh`.
* **RDP**: downloads an `.rdp` file for any RDP client, or opens an `rdp://` link, depending on your preference.
* **VNC**: opens a `vnc://` link in your VNC viewer.
* **HTTP / HTTPS**: opens the page in a new browser tab.

Set your defaults under **Settings → Profile**: **Default RDP username**, **Preferred window width** and **height**,
**Open non-browser-based RDP connections with**, and **Use Secure Shell Extension for Chrome**. These are stored in
your browser.

The tunnel's host is the server holding the device's connection. On hosted Procy that is a regional node such as
`usc1-1.connect.procy.sh`, with ports 20000–30000; your firewall must allow these outbound (see
[network requirements](/docs/hosted/network-requirements/)). Always use the host and port the tunnel shows.

## Close a tunnel

Click the close action next to the tunnel on the Tunnels page, or **Delete** in the tunnel's menu on the device's
Tunnels tab. Open connections through the tunnel are cut.

## Reach other hosts through a device

A device can forward to other hosts on its network, for example a printer's web page, a switch or a server without
the Procy client. Pick **Forwarding** as the service, choose the **Forwarded service** (SSH, RDP, HTTP, ...), enter the
**Destination IP / hostname** as the device sees it (such as `192.168.1.20`), and set the **Device port**.

The device must be allowed to reach that target. By default it may reach any address; its administrator can restrict
this with `tunnel_allowed` in `rport.conf`, for example `tunnel_allowed = ['127.0.0.0/8']` to allow tunnels to the
device itself only. See `rport.example.conf` for more examples.

## Manage tunnels through the API

Everything above is available through the API. The examples use an [API token](/docs/api-and-cli/api-authentication/)
and hosted Procy; for your own server, replace `procy.sh` with its address.

```shell
export PROCY=https://procy.sh/api/v1
export CREDS='you@example.com:<api-token>'
```

### List tunnels

All active tunnels you can see:

```shell
curl -s -u "$CREDS" "$PROCY/tunnels" | jq
```

The tunnels of one device are part of the device's details:

```shell
CLIENT_ID=2ba9174e-640e-4694-ad35-34a2d6f3986b
curl -s -u "$CREDS" "$PROCY/clients/$CLIENT_ID" | jq '.data.tunnels'
```

### Create a tunnel

`PUT /clients/{client_id}/tunnels` with the settings as query parameters:

```shell
curl -s -u "$CREDS" -X PUT -G "$PROCY/clients/$CLIENT_ID/tunnels" \
  -d remote=22 \
  -d scheme=ssh \
  -d acl=203.0.113.7 \
  -d idle-timeout-minutes=15 | jq
```

*`-G` makes curl append the `-d` pairs to the URL.*

The response contains the new tunnel:

```json
{
  "data": {
    "id": "1",
    "lhost": "0.0.0.0",
    "lport": "20417",
    "rhost": "0.0.0.0",
    "rport": "22",
    "lport_random": true,
    "scheme": "ssh",
    "acl": "203.0.113.7",
    "idle_timeout_minutes": 15,
    "tunnel_host": "usc1-1.connect.procy.sh"
  }
}
```

Connect to `tunnel_host` (or your server's address, when it is empty) on port `lport`.

Parameters:

`remote` (required)
: The device port, such as `22`, or `<host>:<port>` to forward through the device to another host, such as
  `192.168.1.20:80`.

`local`
: The server port. Leave it out to get a random free port. It must be within the server's `used_ports` (default
  `20000-30000`) and not in `excluded_ports`.

`scheme`
: What the tunnel carries: `ssh`, `rdp`, `vnc`, `http`, `https`, or another URI scheme of up to 15 characters. Used
  to build links and to choose the browser proxy.

`protocol`
: `tcp` (default), `udp` or `tcp+udp`.

`acl`
: Comma-separated IPv4 addresses or CIDR ranges allowed to connect, such as `203.0.113.7,198.51.100.0/24`. Leave it
  out to allow anyone.

`idle-timeout-minutes`
: Close the tunnel after this many idle minutes. Default 5, at most one week (10080).

`skip-idle-timeout`
: `1` keeps the tunnel open while idle. Don't combine it with `idle-timeout-minutes`.

`auto-close`
: Close the tunnel after a fixed time, as a duration such as `90m` or `2h30m` (at most 31 days).

`check_port`
: `0` skips the check that the device port is listening (TCP only; on by default).

`name`
: A label for the tunnel.

`http_proxy`, `host_header`, `auth_user`, `auth_password`
: The built-in browser proxy on self-hosted servers; see [the reverse proxy below](#reverse-proxy-for-web-pages).

Errors come back with a code you can test for, for example `ERR_CODE_TUNNEL_TO_PORT_EXIST` (a tunnel to that port
with the same access list exists), `ERR_CODE_REMOTE_PORT_NOT_OPEN` (nothing listens on the device port),
`ERR_CODE_LOCAL_PORT_IN_USE` and `ERR_CODE_INVALID_ACL`. On hosted Procy, an organization at its
[hard transfer limit](/docs/hosted/billing-and-usage/#when-you-reach-the-transfer-allowance) gets `403` with
`ERR_CODE_TRANSFER_LIMIT` for new tunnels.

### Change who may connect

Replace the access list of a running tunnel:

```shell
curl -s -u "$CREDS" -X PUT "$PROCY/clients/$CLIENT_ID/tunnels/1/acl" \
  -H 'Content-Type: application/json' \
  --data-raw '{"acl": "203.0.113.7,198.51.100.0/24"}'
```

### Close a tunnel

```shell
curl -s -u "$CREDS" -X DELETE "$PROCY/clients/$CLIENT_ID/tunnels/1"
```

Add `?force=1` to close it even while connections are open.

### Stored tunnels

Stored tunnels are saved tunnel settings per device: `GET`, `POST`, `PUT` and `DELETE` on
`/clients/{client_id}/stored-tunnels`. See the [API reference](/docs/api/).

## Tunnels the device opens itself

A device can also open tunnels when it starts, listed as `remotes` in its `rport.conf`:

```toml
[client]
  remotes = ['22', '3389:3389']
```

`'22'` makes the device's port 22 available on a random server port; `'2222:22'` uses server port 2222. These
tunnels stay up as long as the device is connected and keep a port open the whole time, so prefer
[on-demand tunnels](/docs/remote-access/on-demand-tunnels/) unless you need a fixed port.

## Reverse proxy for web pages

**Self-hosted only.** A self-hosted server can put its own HTTPS reverse proxy in front of a tunnel, so you open a
device's web interface (router, switch, NAS) with the server's valid certificate instead of the device's
self-signed one, and optionally behind a username and password. The same proxy serves
[noVNC](/docs/remote-access/novnc-proxy/) and [RDP in the browser](/docs/remote-access/rdp-proxy/). It isn't
available on hosted Procy; the web interface hides the option when the server doesn't offer it.

{{< mermaid class="text-center">}}
flowchart LR
  0[Your browser] --> 1[HTTPS proxy on the server] --> 2[Tunnel] --> 3[Procy client] --> 4[Web server]
{{< /mermaid >}}

Enable it with a certificate and key in the `[server]` section of `rportd.conf`, then restart `rportd`:

```toml
[server]
  tunnel_proxy_cert_file = "/etc/rport/tunnels.crt"
  tunnel_proxy_key_file = "/etc/rport/tunnels.key"
```

The certificate must match the host name you open tunnels with, so reach the server by name, not by IP address.
If you run the API on the same name, you can use the same certificate. You can also let the server get a
certificate itself with `tunnel_enable_acme` (see
[the built-in ACME](/docs/self-hosting/securing-rportd-with-https/#use-the-built-in-acme)).

With the proxy enabled, the tunnel form shows **Open in browser via built-in HTTP proxy** for HTTP and HTTPS. Through
the API, add `http_proxy=1`:

```shell
curl -s -u "$CREDS" -X PUT -G "https://procy.example.com/api/v1/clients/$CLIENT_ID/tunnels" \
  -d remote=192.168.1.1:443 \
  -d scheme=https \
  -d acl=203.0.113.7 \
  -d http_proxy=1 \
  -d host_header=router.lan \
  -d auth_user=admin \
  -d auth_password='<password>' | jq
```

* The proxy listens on the tunnel's server port; the raw tunnel behind it is bound to localhost only. The access list
  applies to the proxy.
* `host_header` replaces the `Host` header sent to the device, for web servers that need a particular name. By
  default the header from your browser is passed on.
* `auth_user` and `auth_password` (both or neither) put HTTP basic authentication in front of the page.

Open `https://procy.example.com:<lport>/`, or the `tunnel_url` from the response if the server uses
[subdomains](/docs/remote-access/tunnels-on-subdomains/).
