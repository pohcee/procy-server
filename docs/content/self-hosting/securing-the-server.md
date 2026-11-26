---
title: "Securing the server"
weight: 40
slug: securing-the-server
aliases:
- /advanced/securing-the-server/
- /advanced/securing-the-rport-server/
- /docs/no10-securing-the-server.html
---
{{< toc >}}

Harden a self-hosted Procy server: what it protects by default, and what to add. On [procy.sh](https://procy.sh)
this is done for you.

A server usually exposes two TCP ports to the internet:

* the **client listener** (`[server] address`, port 8080 by default), where clients connect;
* the **API** (`[api] address`), which also serves the web UI.

Both require authentication, so strong passwords keep you safe. Login attempts still cost the server some work, so it
slows down and bans password guessers, and you can ban them at the network level with [fail2ban](https://www.fail2ban.org/).

## Secrets

`rportd` refuses to start with the placeholders and sample values of `rportd.example.conf` and these docs
(`<YOUR_SEED>`, `<YOUR_SECRET>`, the documented example JWT secret, `admin:foobaz`, `clientAuth1:1234`), and with a
`jwt_secret` shorter than 16 characters.

If `jwt_secret` is empty, `rportd` generates one on the first start and saves it as `rportd-jwt-secret.txt` (mode 0600)
in the data directory. Keep that file private and back it up with the rest of the data directory. Set `jwt_secret`
yourself only when several servers must accept the same tokens (for example `openssl rand -hex 32`); on
[Postgres](/docs/self-hosting/postgres/) it is required.

## The client listener

### Default protection

These defaults in `[server]` protect against password guessing:

```text
  ## Clients wait this many seconds between failed logins, per client auth id.
  #client_login_wait = 2.0
  ## After this many failed logins in a row, the source IP is banned for ban_time seconds (HTTP 423).
  #max_failed_login = 5
  #ban_time = 3600
```

The counters reset only on a successful login or a restart. Bans are per IP address: if several clients share one
internet connection and one of them has a wrong password, the others are banned too when they reconnect. If all
clients arrive through a load balancer, they share its address; set `max_failed_login = 0` to turn the IP ban off
there (the per-id wait still applies).

{{< hint type=note >}}
Refused logins are not errors, so they aren't logged at the `error` log level. Set `log_level = "info"` in
`[logging]` to see them, which fail2ban needs as well.
{{< /hint >}}

### Ban with fail2ban

Two kinds of visitors are worth banning on the client port:

* **Password guessers.** The log shows `Failed login attempt for client auth id "abc", forcing to wait for 2s (<ip>)`
  and, once banned, `Maximum of 5 login attempts reached. Visitor (<ip>) banned.`
* **Scanners.** The internet is full of scanners looking for vulnerable web applications. They get HTTP 404 on the
  client port, which a Procy client never does, so any IP that causes a 404 can be banned. Find them with
  `egrep " 404 [0-9]+\w+ \(.*\)" /var/log/rport/rportd.log`.

Create the filter `/etc/fail2ban/filter.d/rportd-client-connect.conf`:

```text
# Fail2Ban filter for the rportd client listener
[Definition]
failregex = 404 [0-9]+\w+ \(<HOST>\)
            Failed login attempt for client auth id .* \(<HOST>\)$
            Visitor \(<HOST>\) banned
```

Test it:

```shell
fail2ban-regex /var/log/rport/rportd.log /etc/fail2ban/filter.d/rportd-client-connect.conf
```

The output counts the matching lines, the requests that would be banned. Then create the jail
`/etc/fail2ban/jail.d/rportd-client-connect.conf`. Set `port` to the port of your client listener
(`grep "address =" /etc/rport/rportd.conf`):

```text
[rportd-client-connect]
enabled  = true
port     = 8080
filter   = rportd-client-connect
logpath  = /var/log/rport/rportd.log
# ban an IP for an hour after two matches within 20 seconds
maxretry = 2
findtime = 20
bantime  = 3600
```

Restart fail2ban with `systemctl restart fail2ban` and check the jail with
`fail2ban-client status rportd-client-connect`.

{{< hint type=caution >}}
fail2ban ships with many default rules, and the SSH jail is on by default. To run only the rportd jail, keep only its
file in `/etc/fail2ban/jail.d/` (on Debian, delete or comment out `defaults-debian.conf`). `fail2ban-client status`
shows which jails are active.
{{< /hint >}}

## The API

### Use HTTPS

Never expose the API and the web UI over plain HTTP. Use the built-in
[HTTPS support or ACME](/docs/self-hosting/securing-rportd-with-https/), or a TLS reverse proxy.

### Password guessing

The API has its own protection in `[api]`:

```text
  ## Users wait this many seconds between failed logins, per username.
  #user_login_wait = 2.0
  ## After this many failed logins, the source IP is banned for ban_time seconds.
  #max_failed_login = 5
  #ban_time = 3600
```

Bans are per IP address. Behind a reverse proxy, list the proxy in `trusted_proxies`; otherwise all users share the
proxy's address and one user's failed logins ban everybody. See
[behind a reverse proxy](/docs/self-hosting/securing-rportd-with-https/#behind-a-reverse-proxy).

For an extra layer, turn on [two-factor authentication](/docs/self-hosting/user-authentication/#two-factor-auth), or let
users [sign in with Google](/docs/self-hosting/oidc/), which applies Google's own 2-Step Verification.

### Users and permissions

* Users who aren't administrators can only use what their user groups allow. If the user store can't hold group
  permissions (a JSON `auth_file` or a single user), only administrators can run commands and scripts, create
  tunnels, or use the vault, scheduler and uploads. See the [permissions model](/docs/hosted/permissions-model/).
* With [delegated authentication](/docs/self-hosting/user-authentication/#delegated-authentication), `auth_header` is
  accepted only from `trusted_proxies`, and users are created on the fly only with `create_missing_users = true`.
* New passwords need at least `password_min_length` characters (14 by default) and must pass the
  `password_zxcvbn_minscore` check.
* Deleting a user ends their sessions and revokes their API tokens at once.

### Audit log

Every action through the API is written to the audit log (`enable_audit_log`, on by default). Set
`use_ip_obfuscation = true` if storing full IP addresses conflicts with your privacy rules.
