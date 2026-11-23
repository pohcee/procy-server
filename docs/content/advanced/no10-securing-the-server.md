---
title: "Securing the Procy server"
weight: 10
slug: securing-the-server
aliases:
  - /advanced/securing-the-rport-server/
  - /docs/no10-securing-the-server.html
---
{{< toc >}}
Your Procy server usually exposes two TCP ports to the public internet:

* one for the client connections
* one for the API and the web frontend

Both require authentication and unless you use weak passwords you are safe.
But even if login attempts are prevented on the HTTP level they produce some load on your server.

Learn more about the default security measures and how to add a second level of security using [fail2ban](https://www.fail2ban.org/).

## Client Connection Listener

### The client connection listener is secured by default

By default, the following options are activated for the client connection listener.

```text
## Protect your server against password guessing.
## Force clients to wait N seconds (float) between unsuccessful login attempts.
## This is per client auth id.
## A message like
##    'client-listener: Failed login attempt for client auth id "abc", forcing to wait for {client_login_wait}s ({ip})'
## is logged to the info log.
## Consider changing the log_level to 'info' to trace failed login attempts.
## Learn more https://procy.sh/docs/advanced/securing-the-server/
## Defaults: 2.0
#client_login_wait = 2.0

## After {max_failed_login} consecutive failed login-in attempts ban the source IP address for {ban_time} seconds.
## HTTP Status 423 is returned.
## A message like
##     'Maximum of {max_failed_login} login attempts reached. Visitor ({remote-ip}) banned. Ban expiry: 2021-04-16T11:22:26+00:00'
## is logged to the info log.
## Banning happens on HTTP level.
## Consider banning on network level using fail2ban.
## Learn more https://procy.sh/docs/advanced/securing-the-server/
## Defaults: max_failed_login = 5, ban_time = 3600
#max_failed_login = 5
#ban_time = 3600
```

These are good settings to protect your server against password guessing.
The counters for failed logins are constantly increasing and only reset by a successful login or a server restart.

For example, if a client fails to log in for the fifth time, any login attempts of the IP address are blocked for one hour.
If there are more clients on the same network with correct credentials but sharing common internet access and these
clients are restarted, they are banned too.

{{< hint type=warning >}}
Because rejecting connections on failed logins is handled properly by the server, they are not considered an error and
not logged to the log file when you are on the `error` log level. Consider changing the `log_level` to `info` to trace
failed logins and to eventually activate fail2ban.
{{< /hint >}}

### Using fail2ban for additional security

#### Ban password guesser

#### Ban scanners

After a short period, you will notice HTTP requests to arbitrary files and folders on the client connect port that are
answered with HTTP 404. The internet is full of scanners searching for vulnerable web applications.
You can safely ban any IP address that produces HTTP 404. A Procy client will never do this.

Search your log file for the following pattern:
`egrep " 404 [0-9]+\w+ \(.*\)" /var/log/rport/rportd.log`

#### Fail2ban configuration

{{< hint type=tip >}} Remember
All fail2ban rules require running `rportd` with `log_level = 'info'`.
{{< /hint >}}

To create rules and filters to ban both, password guessers and scanners, proceed with the following setup.

A fail2ban filter `/etc/fail2ban/filter.d/rportd-client-connect.conf` can look like this.

```text
# Fail2Ban filter for rportd client connect
[Definition]
# Identify scanners
failregex = 404 [0-9]+\w+ \(<HOST>\)
# Identify password guesser
```

Test the definition with
`fail2ban-regex /var/log/rport/rportd.log /etc/fail2ban/filter.d/rport-client-connect.conf`
It should output something like

```text
Lines: 16900 lines, 0 ignored, 755 matched, 16145 missed
[processed in 1.04 sec]
```

That means fail2ban has found 755 requests that will be banned.

Create a ban action in `/etc/fail2ban/jail.d/rportd-client-connect.conf`.
Change `port` to the port your rportd is listening for client connections. `grep "url =" /etc/rport/rportd.conf`

```text
# service name
[rportd-client-connect]
# turn on /off
enabled  = true
# ports to ban (numeric or text)
port     = 8080
# filter from previous step
filter   = rportd-client-connect
# file to parse
logpath  = /var/log/rport/rportd.log
# ban rule:
# ban all IPs that have created two 404 request during the last 20 seconds for 1hour
maxretry = 2
findtime = 20
# ban on 10 minutes
bantime = 3600
```

Restart fail2ban to activate the new configuration with `service fail2ban restart` and check the status.

```shell
root@localhost:~# fail2ban-client status
Status
|- Number of jail: 2
`- Jail list: rportd-client-connect, sshd
fail2ban-server[1905]: Server ready
root@localhost:~# fail2ban-client status
Status
|- Number of jail: 2
`- Jail list: rportd-client-connect, sshd
root@localhost:~# fail2ban-client status rportd-client-connect
Status for the jail: rportd-client-connect
|- Filter
|  |- Currently failed: 0
|  |- Total failed: 0
|  `- File list: /var/log/rport/rportd.log
`- Actions
   |- Currently banned: 0
   |- Total banned: 0
   `- Banned IP list: 
```

{{< hint type=warning >}}
Fail2ban ships with a lot of default rules and the SSH is enabled by default.
To enable fail2ban only for rportd and to disable the ssh rule, make sure only rportd rules are in `/etc/fail2ban/jail.d/`.
Either delete `/etc/fail2ban/jail.d/defaults-debian.conf` or open the file and comment out all lines.
Use `fail2ban-client status` to verify which rules are active.
{{< /hint >}}

## Securing the API

### Secrets and credentials

`rportd` refuses to start with the placeholders and sample values from `rportd.example.conf` and these docs: the
`<YOUR_SEED>` and `<YOUR_SECRET>` placeholders, the documented example JWT secret, and the sample credentials
`admin:foobaz` and `clientAuth1:1234`. A `jwt_secret` shorter than 16 characters is refused as well.

If you leave `jwt_secret` empty, `rportd` generates a random secret on the first start and saves it as
`rportd-jwt-secret.txt` (mode 0600) in the data directory. Keep that file private, and back it up with the rest of the
data directory. Set `jwt_secret` yourself only if several servers must accept the same tokens, for example with
`openssl rand -hex 32`.

### Use HTTPS

Never expose the API and the web UI over plain HTTP. Use the built-in
[HTTPS support or ACME](/docs/get-started/securing-rportd-with-https/), or a TLS reverse proxy.

### Password guessing

The API has its own protection against password guessing in the `[api]` section:

```text
  ## Force users to wait N seconds (float) between unsuccessful login attempts. Per username.
  #user_login_wait = 2.0
  ## After X failed login-in attempts, ban the source IP address for Z seconds.
  #max_failed_login = 5
  #ban_time = 3600
```

Bans are per IP address. If `rportd` runs behind a reverse proxy, list the proxy in `trusted_proxies`, otherwise all
users share the proxy's address and one user's failed logins ban everybody. `rportd` reads `X-Forwarded-For` only
from those proxies, so a client connecting directly can't fake its address. See
[behind a reverse proxy](/docs/get-started/securing-rportd-with-https/#behind-a-reverse-proxy).

Turn on [two-factor authentication](/docs/get-started/api-authentication/#two-factor-auth) for an extra layer.

### Users and permissions

* Users who aren't administrators can only use what their user groups allow. If the user store can't hold group
  permissions (a JSON `auth_file` or a single user), only administrators can run commands and scripts, create
  tunnels, or use the vault, scheduler and uploads. See the [permissions model](/docs/get-started/permissions-model/).
* With [delegated authentication](/docs/get-started/api-authentication/#delegated-authentication), `auth_header` is
  only accepted from `trusted_proxies`, and users are created on the fly only with `create_missing_users = true`.
* New passwords need at least `password_min_length` characters (14 by default) and must pass the
  `password_zxcvbn_minscore` check.
* Deleting a user revokes their sessions and API tokens immediately.

### Audit log

Each action through the API is written to the audit log (`enable_audit_log`, on by default) in the data directory.
Set `use_ip_obfuscation = true` if storing full IP addresses conflicts with your privacy rules.
