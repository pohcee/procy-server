---
title: "HTTPS for the API and UI"
weight: 30
slug: securing-rportd-with-https
aliases:
- /get-started/securing-rportd-with-https/
- /docs/no08-https-howto.html
- /docs/get-started/no08-https-howto.md
---
{{< toc >}}

Serve the API and the web UI of a self-hosted server over HTTPS. `rportd` does this itself, without a reverse proxy:
with Let's Encrypt certificates from its built-in ACME client, or with certificate files you manage. On
[procy.sh](https://procy.sh) this is done for you.

{{< hint type=caution title="Always use encryption" >}}
Even on your intranet. Plain HTTP is trivial to sniff, and your passwords and tokens travel with every request.
{{< /hint >}}

## Use the built-in ACME

The built-in ACME client gets and renews Let's Encrypt certificates while the server runs, with no restarts. You need
a hostname with a public DNS record pointing at the server. In `rportd.conf`:

```text
[api]
  address = "0.0.0.0:443"
  ## The hostname must have a public DNS record before you start the server.
  base_url = "https://procy.example.com"
  enable_acme = true
```

Let's Encrypt validates the request over port 443 or port 80; no other ports work. If the API doesn't listen on 443,
answer the validation on port 80 in the `[server]` section:

```text
[server]
  acme_http_port = 80
```

`acme_http_port` may be another local port, but then the firewall or a proxy must map public port 80 to it. If you
can't expose port 80 or 443 to the internet, neither the built-in ACME nor `certbot` will work: use certificates from
another source (below).

For the **tunnel proxy** of [noVNC](/docs/remote-access/novnc-proxy/), [RDP](/docs/remote-access/rdp-proxy/) and
other HTTP tunnels, set `tunnel_enable_acme = true` and a `tunnel_host` with a public DNS record in `[server]`. The
certificate is requested for `tunnel_host`.

Certificates are stored in `<data_dir>/acme`, usually `/var/lib/rport/acme`. If something goes wrong, run the server
with `log_level = "debug"` in `[logging]` for details.

## Use your own certificate files

Point `rportd` at a certificate (or chain) and its key. The key must not be password protected. When both are set,
the API switches from HTTP to HTTPS:

```text
[api]
  address = "0.0.0.0:3000"
  ## Include intermediate certificates in cert_file.
  cert_file = "/etc/letsencrypt/live/procy.example.com/fullchain.pem"
  key_file = "/etc/letsencrypt/live/procy.example.com/privkey.pem"
```

Restart `rportd` after changing the files in the config, then check the setup:

```shell
curl -Iv -u admin:$PASSWORD https://procy.example.com:3000/api/v1/status
```

### Get certificates with certbot

Port 80 must be free and reachable from the internet while certbot runs. If `rportd` uses port 80, stop it first.

```shell
DOMAIN=procy.example.com
sudo apt install certbot
sudo certbot certonly -d $DOMAIN -n --agree-tos --standalone -m <YOUR_EMAIL>
# certbot's files are readable by root only: let the rport group read them
sudo chgrp -R rport /etc/letsencrypt/live/ /etc/letsencrypt/archive/
sudo chmod -R g+rX /etc/letsencrypt/live/ /etc/letsencrypt/archive/
```

The key, certificates and chains are now in `/etc/letsencrypt/live/$DOMAIN/`.

### Renew them

On Ubuntu and Debian the certbot package installs a systemd timer that renews certificates (see
`systemctl list-timers certbot*`). Elsewhere, run `certbot -q renew` from cron twice a day.

{{< hint type=important title="Add a deploy hook" >}}
Without one, `rportd` keeps serving the old certificate, and the new files may not be readable by it. Add a script to
`/etc/letsencrypt/renewal-hooks/deploy/` that repeats the `chgrp`/`chmod` above and runs `systemctl restart rportd`.
{{< /hint >}}

## Behind a reverse proxy

If a reverse proxy (Caddy, nginx, a cloud load balancer) terminates TLS in front of the API, list its addresses in
`trusted_proxies`:

```text
[api]
  ## Reverse proxies (IP addresses or CIDR ranges) in front of rportd.
  trusted_proxies = ["127.0.0.1/32"]
```

`rportd` takes the client's IP address from `X-Forwarded-For` only when the request comes from one of these addresses;
otherwise it uses the address of the connection. The IP is used for login bans, the audit log, sessions and pairing.
Without `trusted_proxies`, every request seems to come from the proxy, so one user's failed logins ban everyone. Don't
list addresses that aren't proxies: anyone connecting from them could fake their IP.

Set `base_url` to the public `https://` URL, so links the server generates (pairing, emails) point to
the proxy. Clients can also reach the client listener through a TLS proxy (SSH over HTTPS); give them an `https://`
server URL then.
