---
title: "Tunnels on subdomains"
weight: 50
slug: tunnels-on-subdomains
aliases:
- /advanced/tunnels-on-subdomains/
---
{{< toc >}}

**Self-hosted only.** A self-hosted server can publish browser tunnels on a random subdomain on port 443, such as
`https://pio0toazivaeGheif2.tunnels.example.com`, instead of a random port such as `https://procy.example.com:34567`.
Use this where firewalls block outgoing connections to unusual ports.

Subdomains are made for tunnels that go through the server's HTTPS proxy: web pages with the
[reverse proxy](/docs/remote-access/managing-tunnels/#reverse-proxy-for-web-pages),
[VNC in the browser](/docs/remote-access/novnc-proxy/) and [RDP in the browser](/docs/remote-access/rdp-proxy/).
Plain port tunnels (such as SSH) keep their port.

`rportd` routes the subdomains with [Caddy](https://caddyserver.com/). It starts, configures and stops Caddy itself;
you don't write a Caddy configuration.

## Prerequisites

* The [HTTPS tunnel proxy](/docs/remote-access/managing-tunnels/#reverse-proxy-for-web-pages) is enabled.
* Caddy is installed on the same host as `rportd` (not in a separate container; `rportd` talks to it over a Unix
  socket).
* A wildcard DNS record and a wildcard certificate for the subdomains.

### Install Caddy

Install Caddy [with your package manager](https://caddyserver.com/docs/install#debian-ubuntu-raspbian), so it is
updated with the OS, or download the binary:

```bash
sudo curl -L "https://caddyserver.com/api/download?os=linux&arch=amd64" -o /usr/local/bin/caddy
sudo chmod +x /usr/local/bin/caddy
```

Then:

* **Allow it to bind port 443.** Neither `rportd` nor Caddy runs as root, so give Caddy the capability:
  `sudo setcap 'cap_net_bind_service=+ep' $(which caddy)`. If `setcap` is missing, install `libcap2-bin`.
* **Disable Caddy's own service** if the package installed one, so it doesn't compete with the Caddy that `rportd`
  starts: `sudo systemctl disable --now caddy`.

### Wildcard DNS record

Create a wildcard record for the subdomains, for example `*.tunnels.example.com` pointing to the server. The API
and web interface may be inside the same wildcard (for example `*.example.com` for the tunnels and
`procy.example.com` for the API); then you need no second record.

### Wildcard certificate

You need a certificate for `*.tunnels.example.com`. With Let's Encrypt, a wildcard needs the DNS challenge:

```bash
sudo certbot certonly --manual --preferred-challenges dns -d '*.tunnels.example.com'
```

certbot shows a TXT record to create, then waits. Check the record is visible before you continue:

```shell
dig -t txt _acme-challenge.tunnels.example.com
```

Check the result is a wildcard certificate:

```bash
openssl x509 -noout -subject -in /etc/letsencrypt/live/tunnels.example.com/fullchain.pem
```

The user `rportd` runs as (usually `rport`) must be able to read the certificate and key. Let's Encrypt makes them
readable for root only; give the `rport` user read access, for example through a group.

## Configure rportd

There are two layouts:

1. **Split ports:** the subdomains listen on 443 and the API and web interface on another port (such as 8443) with
   their own built-in web server.
2. **Shared port:** the subdomains, the API and the web interface all use 443. Caddy routes everything and the API
   runs behind it without TLS.

### Split ports

Add a `[caddy-integration]` section to `rportd.conf`:

```toml
[caddy-integration]
  ## Path to the caddy binary.
  caddy = "/usr/bin/caddy"
  ## Where caddy listens for subdomain tunnels.
  address = "0.0.0.0:443"
  ## The domain the random subdomains are made under, without the "*.".
  subdomain_prefix = "tunnels.example.com"
  ## The wildcard certificate for that domain.
  cert_file = "/etc/letsencrypt/live/tunnels.example.com/fullchain.pem"
  key_file = "/etc/letsencrypt/live/tunnels.example.com/privkey.pem"
```

The port in `address` must differ from the one in `[api] address`. Restart `rportd`.

### Shared port

Add the API's host name and certificate to the same section:

```toml
  ## The host name of the API and web interface.
  api_hostname = "procy.example.com"
  ## Its certificate, even if the wildcard certificate covers it.
  api_cert_file = "/etc/letsencrypt/live/procy.example.com/fullchain.pem"
  api_key_file = "/etc/letsencrypt/live/procy.example.com/privkey.pem"
  ## The port of [api] address. The API must run without TLS.
  api_port = "3000"
```

and run the API on that port without TLS (comment out `cert_file` and `key_file` in `[api]`):

```toml
[api]
  address = "127.0.0.1:3000"
  #cert_file = "/etc/letsencrypt/live/procy.example.com/fullchain.pem"
  #key_file = "/etc/letsencrypt/live/procy.example.com/privkey.pem"
```

Restart `rportd`.

## Use it

Create a tunnel with the browser proxy (`http_proxy=1` through the API, or **Open in browser via built-in HTTP proxy**
in the tunnel form). The response contains a `tunnel_url` with the subdomain, and the web interface opens that URL
instead of the port. `GET /api/v1/status` reports `"subdomain_tunnels": true` under `capabilities`.

## Troubleshooting

1. If `rportd` doesn't start, read the reason with `journalctl -u rportd.service --no-pager -e`, and look at
   `/var/log/rport/rportd.log`.
2. Set `log_level = "debug"` in `rportd.conf` for more detail.
3. List Caddy's current routes and the active subdomains (the socket is in the server's `data_dir`):

    ```bash
    curl -s http://localhost/config/apps/http/servers/srv0/routes \
      --unix-socket /var/lib/rport/caddy-admin.sock -H "host:unix" | jq
    ```
