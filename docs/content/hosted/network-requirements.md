---
title: "Network requirements"
weight: 60
slug: network-requirements
aliases:
- /get-started/network-requirements/
---
{{< toc >}}

This page lists what your firewall, proxy or allow-list must let through for hosted Procy at `procy.sh`. It's for the
IT and network administrators of the sites where your devices and users are. If you run your own Procy server, the
same rules apply with your own server's names and ports.

Everything is **outbound** from your network. Procy never needs an inbound port or a port forward on your side:
the device connects out and keeps the connection open.

## Devices (the Procy client)

Each device running the client needs:

| Destination | Port | Why |
| --- | --- | --- |
| `connect.procy.sh` | TCP 80 (or 443) | The device's connection to Procy (a long-lived WebSocket). |
| `us-central1.connect.procy.sh` | TCP 80 | Regional fallback, used if `connect.procy.sh` is unreachable (when set as a fallback server). |
| `us-east1.connect.procy.sh` | TCP 80 | Regional fallback (when set as a fallback server). |
| `procy.sh` | TCP 443 | Installing and updating the client (`https://procy.sh/downloads/...`) and pairing. |

While installing, the Linux and Windows installers also ask `ip-api.com` (TCP 80) for the device's city and country,
to set them as [labels](/docs/devices/attributes/). If that fails, the device simply gets no location labels.

**Ports 80 and 443:** clients normally connect to `connect.procy.sh:80`. Where only HTTPS is allowed out, set the
client's server to `https://connect.procy.sh` (port 443). The client checks the server's own key fingerprint either
way, so port 80 is not weaker in practice.

**Web proxies and TLS inspection:** the connection is a WebSocket. A proxy must allow the HTTP `Upgrade` to WebSocket
and must not cut long-lived connections; the client reconnects on its own after a cut, but each cut drops running
sessions. If your proxy inspects TLS, exclude `connect.procy.sh` from inspection, or use port 80.

## People using the web interface

Browsers of the people who manage devices need:

| Destination | Port | Why |
| --- | --- | --- |
| `procy.sh` | TCP 443 | The web interface, the API and these docs. |
| `usc1-1.connect.procy.sh`, `use1-1.connect.procy.sh` | TCP 20000–30000 | Tunnels (SSH, remote desktop, web pages). |
| `accounts.google.com` | TCP 443 | Only when signing in with Google. |

A tunnel opens on the server holding the device, on a port from this range; its link shows which. When you open a
tunnel you can restrict it to your own address (the tunnel's access list). Do so for anything sensitive.

## If your firewall allows IP addresses only

Prefer rules by host name: the addresses can change when Procy adds regions. If your equipment can only allow
addresses, these are the current ones:

| Name | Address |
| --- | --- |
| `connect.procy.sh`, `procy.sh` | `34.149.36.189` (Google global load balancer; one address worldwide) |
| `us-central1.connect.procy.sh`, `usc1-1.connect.procy.sh` | `35.232.37.46` |
| `us-east1.connect.procy.sh`, `use1-1.connect.procy.sh` | `34.26.223.147` |

These addresses are static. Check this page when a device stops connecting after a firewall change.

## Email from Procy

Signup, invitation and notification emails come from `procy@procy.sh` through Google Workspace (SPF, DKIM and
DMARC are set for `procy.sh`). If your mail filter holds them, allow that sender. Questions go to
[support@procy.sh](mailto:support@procy.sh); security reports to [security@procy.sh](mailto:security@procy.sh).

## Checking from a device

From the device's network:

```sh
# Linux / macOS: a 404 means the connection works (the server answers plain web requests with 404).
curl -s -o /dev/null -w '%{http_code}\n' http://connect.procy.sh/
curl -s -o /dev/null -w '%{http_code}\n' https://connect.procy.sh/
```

```powershell
# Windows
Test-NetConnection connect.procy.sh -Port 80
Test-NetConnection connect.procy.sh -Port 443
```

If these fail while other web sites work, your firewall or proxy blocks Procy: allow the destinations above.
