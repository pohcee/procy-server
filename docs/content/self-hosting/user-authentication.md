---
title: "User authentication"
weight: 25
slug: user-authentication
---
{{< toc >}}

**Self-hosted only.** This page covers where a self-hosted `rportd` keeps its users and passwords, two-factor sign-in,
and handing authentication to a reverse proxy. On hosted Procy, people sign in with Google and members are managed
under [organizations](/docs/hosted/organizations/). For API tokens and calling the API, see
[API tokens and authentication](/docs/api-and-cli/api-authentication/).

{{< hint type=tip >}}
Where you can, let people sign in through your identity provider with [OIDC](/docs/self-hosting/oidc/) instead of
keeping passwords on the server. Scripts then use [API tokens](/docs/api-and-cli/api-authentication/).
{{< /hint >}}

## Storing credentials, managing users

`rportd` reads its users from one of three places:

1. a single user with a plaintext password in `rportd.conf`,
2. a user file with bcrypt-hashed passwords,
3. database tables with bcrypt-hashed passwords.

Use exactly one; `rportd` refuses to start with more than one. To manage users in the web interface or through the
API, store them in a database. A server on [Postgres](/docs/self-hosting/postgres/) keeps users in Postgres and
needs none of the options below.

{{< hint type=important title="Permissions need a database" >}}
Only a database with a `group_details` table (`auth_group_details_table`) can store
[user group permissions](/docs/hosted/permissions-model/). With a single user or a user file, permissions can't
be evaluated, so only members of `Administrators` can run commands and scripts, create tunnels, or use the vault,
scheduler and uploads; all other users get "403 Forbidden". `rportd` logs a notice about this at startup.
{{< /hint >}}

### Hardcoded single user

In the `[api]` section of `rportd.conf`:

```toml
[api]
  auth = "admin:<your-password>"
```

`rportd` refuses to start with the sample credentials from `rportd.example.conf`. Two-factor sign-in isn't
available with a single user.

### User file

For several users, create a JSON file with bcrypt-hashed passwords:

{{< tabs "user-file" >}}
{{< tab "2FA off" >}}

```json
[
  {
    "username": "admin",
    "password": "$2y$10$ezwCZekHE/qxMb4g9n6rU.XIIdCnHnOo.q2wqqA8LyYf3ihonenmu",
    "groups": ["Administrators"]
  },
  {
    "username": "bunny",
    "password": "$2y$10$ezwCZekHE/qxMb4g9n6rU.XIIdCnHnOo.q2wqqA8LyYf3ihonenmu",
    "groups": ["Bunnies"]
  }
]
```

{{< /tab >}}
{{< tab "2FA by email or Pushover" >}}

```json
[
  {
    "username": "admin",
    "password": "$2y$10$ezwCZekHE/qxMb4g9n6rU.XIIdCnHnOo.q2wqqA8LyYf3ihonenmu",
    "groups": ["Administrators"],
    "two_fa_send_to": "admin@example.com"
  }
]
```

{{< /tab >}}
{{< tab "Authenticator app (TOTP)" >}}

```json
[
  {
    "username": "admin",
    "password": "$2y$10$ezwCZekHE/qxMb4g9n6rU.XIIdCnHnOo.q2wqqA8LyYf3ihonenmu",
    "groups": ["Administrators"],
    "totp_secret": ""
  }
]
```

`totp_secret` is filled in when the user creates a secret with `/me/totp-secret`.

{{< /tab >}}
{{< /tabs >}}

Point `rportd` at it, for example `/var/lib/rport/api-auth.json`, and make sure no other auth option is set:

```toml
[api]
  auth_file = "/var/lib/rport/api-auth.json"
```

The file is read at start and on reload (`kill -SIGUSR1 <pid>`); changes while `rportd` runs have no effect until
then. Create bcrypt hashes with `htpasswd` from the Apache utilities and copy the part after the colon:

```shell
htpasswd -nbB admin 'Super-Secret$Passw0rD'
admin:$2y$05$Wgzg0fwtiCNYfP69k2uYKuYbmmFtd5RPK7W7mkgemuGkfXB2kgcdW
```

### Database

To keep users in a database (for example to share them with other software), set up the global database connection
in the `[database]` section of `rportd.conf` (MySQL/MariaDB or SQLite; see `rportd.example.conf`), then name the
tables in `[api]`:

```toml
[api]
  #auth = ... and auth_file = ... must not be set
  auth_user_table = "users"
  auth_group_table = "groups"
  auth_group_details_table = "group_details"
```

Create the tables. Passwords are bcrypt-hashed; `two_fa_send_to` and `totp_secret` are needed for two-factor
sign-in.

{{< tabs "db-tables" >}}
{{< tab "MySQL/MariaDB" >}}

```toml
[database]
  db_type = "mysql"
  db_host = "localhost:3306"
  #db_host = "socket:/var/run/mysqld/mysqld.sock"
  db_user = "rport"
  db_password = "<password>"
  db_name = "rport"
```

```sql
CREATE TABLE `users` (
  `username` varchar(150) NOT NULL,
  `password` varchar(255) NOT NULL,
  `password_expired` bool NOT NULL default false,
  `two_fa_send_to` varchar(150),
  `token` varchar(128) default NULL,
  `totp_secret` longtext,
  UNIQUE KEY `username` (`username`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8;
CREATE TABLE `groups` (
  `username` varchar(150) NOT NULL,
  `group` varchar(150) NOT NULL,
  UNIQUE KEY `username_group` (`username`,`group`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8;
CREATE TABLE `group_details` (
  `name` varchar(150) NOT NULL,
  `permissions` longtext DEFAULT '{}',
  UNIQUE KEY `name` (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8;
```

{{< /tab >}}
{{< tab "SQLite" >}}

```toml
[database]
  db_type = "sqlite"
  db_name = "/var/lib/rport/database.sqlite3"
```

Create the file, owned by the `rport` user, and the tables:

```shell
sudo -u rport sqlite3 /var/lib/rport/database.sqlite3
```

```sql
CREATE TABLE "users" (
  "username" TEXT(150) NOT NULL,
  "password" TEXT(255) NOT NULL,
  "password_expired" BOOLEAN NOT NULL CHECK (password_expired IN (0, 1)) DEFAULT 0,
  "token" TEXT(36) DEFAULT NULL,
  "two_fa_send_to" TEXT(150),
  "totp_secret" TEXT
);
CREATE UNIQUE INDEX "main"."username" ON "users" ("username" ASC);
CREATE TABLE "groups" (
  "username" TEXT(150) NOT NULL,
  "group" TEXT(150) NOT NULL
);
CREATE UNIQUE INDEX "main"."username_group" ON "groups" ("username" ASC, "group" ASC);
CREATE TABLE "group_details" (
  "name" TEXT(150) NOT NULL,
  "permissions" TEXT DEFAULT "{}"
);
CREATE UNIQUE INDEX "main"."group_details_name" ON "group_details" ("name" ASC);
```

{{< /tab >}}
{{< /tabs >}}

Restart `rportd`, then add the first administrator. Hash a strong password with `htpasswd -nbB admin '<password>'`
and insert it:

```sql
INSERT INTO users (username, password) VALUES ('admin', '<bcrypt hash>');
INSERT INTO groups VALUES ('admin', 'Administrators');
```

With email or Pushover two-factor sign-in, also set `two_fa_send_to` for the user.

### Manage users

With users in a database, administrators manage them in the web interface (**Settings → Users** and **User groups**)
or through the API:

```shell
# list
curl -s -u admin:$PASSWORD https://procy.example.com/api/v1/users | jq
# add
curl -s -u admin:$PASSWORD -X POST https://procy.example.com/api/v1/users \
  -H 'Content-Type: application/json' \
  --data-raw '{"username": "willy", "password": "<NEW_PASSWORD>", "groups": ["Administrators"]}'
# change a password
curl -s -u admin:$PASSWORD -X PUT https://procy.example.com/api/v1/users/willy \
  -H 'Content-Type: application/json' \
  --data-raw '{"password": "<NEW_PASSWORD>"}'
```

New passwords need at least 14 characters (`password_min_length`) and must pass a check against common words and
patterns (`password_zxcvbn_minscore`).

### Manage users from the command line

`rportd user` adds, changes and deletes users directly in the configured user file or database, whichever you use.
Run it as the user `rportd` runs as (usually `rport`), not as root, so file permissions stay right:

```shell
sudo -u rport rportd user change -u <USERNAME> -p -c /etc/rport/rportd.conf
```

`rportd user help` lists the commands (`add`, `change`, `delete`) and their options.

## Two-factor auth

Two-factor sign-in confirms each sign-in with a code, either **sent** to the user (email, Pushover, your own script
or URL) or generated by an **authenticator app** (TOTP). Use one or the other, not both. Two-factor sign-in needs a
user file or database (not a single user in `rportd.conf`). Once it is on, basic authentication with a password stops
working on every endpoint except `/login`; scripts use [API tokens](/docs/api-and-cli/api-authentication/).

### Codes sent to the user

1. Choose the delivery in `[api]`:

   ```toml
   [api]
     two_fa_token_delivery = 'smtp'   # or 'pushover', a URL, or the path of a script
     two_fa_token_ttl_seconds = 600
   ```

2. Set up the delivery: [SMTP](/docs/self-hosting/2fa-messaging/#smtp),
   [Pushover](/docs/self-hosting/2fa-messaging/#pushover), or a script or URL as described in
   [2FA messaging](/docs/self-hosting/2fa-messaging/).
3. Give each user a `two_fa_send_to` (an email address or Pushover user key). With a script or URL,
   `two_fa_send_to_type` sets how changes to it are checked: `'email'`, or `'regex'` with `two_fa_send_to_regex`.
4. Restart `rportd`.

Signing in then takes two steps. `/login` sends the code and returns a login token that is only good for
`/verify-2fa`:

```shell
curl -s -u admin:$PASSWORD https://procy.example.com/api/v1/login | jq
```

```json
{
  "data": {
    "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
    "two_fa": {"send_to": "admin@example.com", "delivery_method": "email"}
  }
}
```

Send the code with that token to get a full session token:

```shell
curl -s https://procy.example.com/api/v1/verify-2fa -X POST \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $LOGIN_TOKEN" \
  --data-raw '{"username": "admin", "token": "05Nfqm"}' | jq
```

A code works once, and only with the login token it was sent for.

### Authenticator app (TOTP)

1. Set `totp_enabled = true` in `[api]` (and leave `two_fa_token_delivery` unset). Users need a `totp_secret`
   field in the user file or database.
2. Restart `rportd`.

On the first sign-in, `/login` answers with `"delivery_method": "totp_authenticator_app"` and
`"totp_key_status": "pending"`. In the web interface, users set up the app under **Settings → Profile**, which shows a
QR code to scan. Through the API, create the secret with the login token:

```shell
curl -s -X POST -H "Authorization: Bearer $LOGIN_TOKEN" https://procy.example.com/api/v1/me/totp-secret
```

The answer holds the `secret` and a `qr` code (a base64 PNG). Add it to the authenticator app, then send the app's
current code to `/verify-2fa` as above. A secret can be created only once; keep it safe.

Settings in `[api]`:

* `totp_login_session_ttl` (default `600s`): how long after `/login` a code is accepted.
* `totp_account_name`: the account name shown in the authenticator app; give each server its own.

If a user loses the secret:

* another administrator can delete it: `DELETE /api/v1/users/{username}/totp-secret`,
* the user can delete their own with a full session token: `DELETE /api/v1/me/totp-secret`,
* or clear `totp_secret` for the user in the user file or database.

The user then creates a new secret at the next sign-in.

## Delegated authentication

You can let a reverse proxy authenticate users, for example with Keycloak, an Apache auth module or a Caddy auth
portal. If the proxy sends a configured header, `rportd` treats the request as authenticated and takes the username
from a header. It accepts these headers only from the proxies listed in `trusted_proxies`, and refuses to start with
`auth_header` but without `trusted_proxies`; otherwise anyone who can reach `rportd` directly could sign in as anyone.
Delegated authentication isn't supported on Postgres; use [OIDC](/docs/self-hosting/oidc/) there.

```toml
[api]
  ## The request counts as authenticated if this header is present.
  auth_header = "Authentication-IsAuthenticated"
  ## The header holding the username (may be the same header).
  user_header = "Authentication-User"
  ## Header auth is accepted only from these proxies. Required.
  trusted_proxies = ["127.0.0.1/32"]
  ## Create users that don't exist yet, in this group.
  create_missing_users = true
  default_user_group = "Users"
```

Clients still call `/api/v1/login` to get a session token and send it with every request. To sign in to the web
interface straight away, send users to `https://<SERVER>/auth/?auto-login=1&remember-me=24h`.

The proxy must require its own authentication on `/api/v1/login` only. If it requires basic authentication on every
path, the web interface breaks, because its background requests carry a bearer token instead.

Example with Caddy, which checks a password on `/api/v1/login` and passes a fixed username:

```text
https://procy.example.com {
  reverse_proxy * 127.0.0.1:3000 {
    header_up Authentication-IsAuthenticated 1
    header_up Authentication-User jopapa
  }
  basicauth /api/v1/login {
    jomama <hash from caddy hash-password>
  }
}
```

Here you sign in as `jomama` with the proxy's password, and Procy treats you as `jopapa`. In practice the proxy
would set the user from your identity provider's session.
