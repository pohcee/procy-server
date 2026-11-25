---
title: "Running the server on Postgres (preview)"
weight: 31
slug: postgres
---
{{< toc >}}

{{< hint type=caution title="Preview" >}}
**Preview.** Postgres storage is new. Several servers can share one database (see
[Running several servers](#running-several-servers)); with peer calls several of them accept client connections. Without a
`[storage]` section the server uses its SQLite files, as it always has.
{{< /hint >}}

## What changes

With `[storage] backend = "postgres"` the server keeps all its data in a Postgres database instead of the SQLite
files in `data_dir`. That includes users, API tokens, sessions, client credentials, clients, jobs, schedules, the
library, the vault, the audit log, monitoring data and the notification log. The data directory still holds the
server key, uploads and other runtime files.

Every row belongs to a tenant (organization). A server for one organization keeps everything in the tenant
`default`. Row-level security in Postgres keeps the tenants apart, in addition to the server's own checks.

* Users and client credentials are stored in Postgres. The `auth`, `auth_file` and `auth_table` settings in
  `[server]`, the `auth`, `auth_file`, `auth_user_table`, `auth_group_table` and `auth_group_details_table`
  settings in `[api]`, and the `[database]` section are refused. Logins by proxy header (`auth_header`) and Plus
  OAuth aren't supported on Postgres yet. Manage users with `rportd user` or the API, and
  client credentials with the API.
* The server connects as an **application role** that doesn't own the tables and isn't a superuser. It refuses to
  start otherwise, because row-level security would not apply. The schema owner runs the migrations.
* The server never migrates the database at start-up. Run `rportd migrate` after each upgrade.
* With the API enabled, `[api] jwt_secret` is required: the server doesn't generate one into `data_dir`, because
  every server on the database must sign and check API tokens with the same secret.
* The passphrase vault is refused: set `[vault] key_provider` to `kms` or `local` (see [The vault](#the-vault)).
* `[server] key_seed` should be set when the server runs the client listener. Without it the server's fingerprint
  changes on each start, and a warning is logged.

## Set up the database

Create a database, a schema owner and an application role, for example:

```sql
CREATE ROLE rport_owner LOGIN PASSWORD '…';
CREATE ROLE rport_app LOGIN PASSWORD '…' NOSUPERUSER NOBYPASSRLS;
CREATE DATABASE rport OWNER rport_owner;
```

Then configure the server:

```toml
[storage]
  backend = "postgres"
  ## the application role; or dsn_file = "/etc/rport/postgres-dsn" (a file holding the URL)
  dsn = "postgres://rport_app:…@db.example.com:5432/rport?sslmode=require"
  ## "all" (default): the API and the client listener; "api": the API only
  role = "all"

[api]
  ## required on Postgres, the same on every server of the database (e.g. openssl rand -hex 32)
  jwt_secret = "…"

[vault]
  key_provider = "local"
  local_key_file = "/etc/rport/vault.key"

[cluster]
  ## the name of this server among those sharing the database; default: the hostname
  #node_id = "node-1"
```

`[cluster] node_id` names the server among the servers sharing the database: up to 63 letters, digits, `.`, `_` or
`-`, starting with a letter or digit. It defaults to the hostname, and the server refuses to start if the hostname
can't be read or isn't a valid node id. It is used only with `backend = "postgres"`.

Create the schema as the owner, and grant the application role what it needs:

```shell
rportd migrate -c /etc/rport/rportd.conf \
  --dsn 'postgres://rport_owner:…@db.example.com:5432/rport?sslmode=require' \
  --grant-to rport_app
```

Running `rportd migrate` again says `nothing to migrate`. Add the first user, then start the server:

```shell
rportd user add -c /etc/rport/rportd.conf -u admin -g Administrators
rportd -c /etc/rport/rportd.conf
```

## The vault

```toml
[vault]
  ## "kms" or "local" (required on Postgres)
  key_provider = "local"
  local_key_file = "/etc/rport/vault.key"
  ## for "kms": kms_key = "projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key>"
```

* `passphrase` (the default on SQLite) is refused on Postgres: several servers can share the database, and the
  passphrase unlocks the vault of one server process only. An empty `key_provider` is refused too.
* `kms` and `local` (Postgres only): each tenant gets its own random vault key. That key is stored in Postgres,
  wrapped by a Google Cloud KMS key (`kms_key`) or by a 32-byte key read from a local file (`local_key_file`, hex or
  base64; create one with `openssl rand -hex 32` and keep a backup). The vault needs no passphrase and can't be
  locked. Losing the wrapping key loses the vault.

For `kms` the server uses the Google application default credentials. They need permission to encrypt and decrypt
with the key.

## Moving a standalone server to Postgres

`rportd import` copies the data of a **stopped** standalone server into a tenant. It only reads the source. It
refuses a data directory that a running server still holds, as far as it can tell: stop the old server before
importing. Run the import before starting servers on the Postgres database (or while they are stopped): logins and
vault writes during an import can collide with imported ids.

```shell
rportd import -c /etc/rport/rportd.conf \
  --from /var/lib/rport --from-config /etc/rport/rportd.conf.sqlite \
  --dsn 'postgres://rport_owner:…@db.example.com:5432/rport' \
  --with-history --dry-run
```

* `-c` is the configuration of the Postgres server. `--from` is the old data directory, and `--from-config` is the
  old configuration file. The users and client credentials are read from the old auth settings, which can be auth
  files, `auth` or `[database]` tables on SQLite.
* `--tenant` (default `default`) is the tenant to import into. With `--dsn` (the schema owner), the tenant is
  created if it doesn't exist.
* `--with-history` also copies jobs, the audit log, monitoring data and the notification log.
* `--dry-run` reads and checks everything and prints the counts, but writes nothing.

The import runs in one transaction and prints, for each kind of data, how many rows it read, added, found already
present and skipped, with the reasons. Running it again adds nothing new. Client IDs, credentials, passwords,
API tokens and job IDs are kept, so clients and users carry on without changes.

The vault values are decrypted with the old vault passphrase and encrypted again with the tenant's new key
(`key_provider = "kms"` or `"local"`). Pass the passphrase in `RPORT_VAULT_PASSPHRASE`, or with
`--vault-passphrase-stdin` on the first line of stdin.

## Tenants

`rportd tenant add ID [NAME] --dsn <owner URL>` adds another tenant. Its users log in to the same server, and each
login lands in its tenant. Schedules and clean-ups run for every active tenant.

Devices of every tenant connect to the same client listener. Client auth ids are unique across all tenants, so the
id a device logs in with tells the server its tenant; creating a credential whose id another tenant uses fails
(HTTP 409). Everything the device does afterwards (command results, measurements, tunnels) belongs to that tenant.

* A tenant that is not `active` (for example `suspended`) refuses its devices when they connect. Devices that are
  connected at that moment stay connected until they reconnect.
* `max_clients` in the `tenants` table limits how many clients a tenant may have (clients connected or disconnected
  within `keep_disconnected_clients`, as the client list shows them). Empty (the default) means no limit. A client
  the tenant already has always reconnects, to any node; a new one is refused once the limit is reached. The count
  is the database's, across every node and device process accepting devices, and it is the device count the
  operator API shows. Give every node the same `purge_disconnected_clients` and `keep_disconnected_clients`: each
  node counts with its own.

A refused device logs the reason, for example `connection refused: the organization's limit of 10 clients is
reached`, and the server logs a line naming the client, its client auth id and the tenant. A device refused by its
organization retries after its `max_retry_interval`, but not sooner than a minute.

## Running several servers

Several servers (*nodes*) can use one database: put a load balancer in front of their API addresses and they serve
the same users, clients and data. Each node keeps working on its own; nodes don't talk to each other, only to the
database.

Give every node:

* the same `[api] jwt_secret`, so a token issued by one node is accepted by the others;
* the same `[server] key_seed`, so the server fingerprint is the same whichever node a device reaches;
* the same vault key: the same `[vault] kms_key`, or the same key file for `key_provider = "local"`;
* its own `[cluster] node_id` (the hostname by default; it must differ between nodes);
* the same time zone (`TZ`): schedules fire at the cron time of the node's zone.

Without [peer calls](#peer-calls) **one** node accepts the client connections: run it with `[storage] role = "all"`
and every other node with `role = "api"`. The node with role `all` holds a lease in the database while it runs; a second node with
role `all` refuses to start ("node … accepts the client connections of this database") until the first has been
stopped for a minute. API nodes answer "client is not connected" for work on clients (commands, scripts,
tunnels); send that work to the `all` node.

What the nodes share and how:

* **Caches.** Each node caches clients and API sessions. A change made on one node reaches the others within about
  a second through a change feed in the database (a `changes` table, `NOTIFY rport_changes`, and polling every few
  seconds). A session revoked on one node (logout, password change, user deleted) is refused by the others.
* **Background work.** Clean-ups (jobs, API sessions, monitoring data, notifications, obsolete clients) and the
  pruning of old library versions run for each tenant on one node at a time: a node takes a lease (table
  `work_leases`) and renews it on each run; when it stops, another node takes over after about one and a half
  intervals. Built-in library entries are synced once per tenant and version when nodes start.
* **Schedules.** Every node keeps every schedule, whichever node created it. Before a run starts, the node records
  it in `schedule_runs` with the schedule and its due time, so each run starts once however many nodes are due to
  start it. `@every` schedules fire at multiples of their period, so all nodes agree on the due time. Without peer
  calls only the node with role `all` starts runs (the others can't reach the clients).
* The change feed keeps three days of changes. A node further behind (stopped longer, or cut off from the
  database) reloads what it caches.
* A transaction left open on the database server (an idle `psql` session that changed something, a long import)
  holds back the change feed of every node until it ends: changes made meanwhile reach the other nodes late, and
  the nodes log a warning. Set `idle_in_transaction_session_timeout` on the database, and prefer a database server
  that doesn't also run long transactions for other applications.
* After restoring the database from a dump into another Postgres server, run `rportd migrate` before starting the
  nodes: it resets the change feed, whose transaction ids come from the old server (nodes refuse to start until
  then).
* `node_id` must be unique: two nodes with role `all` and the same `node_id` would both accept client connections.

### Peer calls

With peer calls on, any number of nodes accept client connections, and every node does work on every client: an
API node finds the node holding the client's connection and passes the work (commands and scripts with their live
output, uploads, tunnels, port checks, updates status, tags and labels) to it.

* **Session registry.** A node accepting client connections records each one in the database (tables `nodes` and
  `client_sessions`, written through the functions `rport_register_session`, `rport_renew_sessions`,
  `rport_end_session` and `rport_node_heartbeat`). It renews the sessions every 20 seconds; a session not renewed
  for a minute is no longer used. A client connecting to a second node replaces its session there (the newest
  wins), and the first node closes the older connection when it next renews. The registry decides whether a client
  is shown connected: a node that stopped without saying so has its clients marked disconnected within about a
  minute, and clients with a live session are never purged as obsolete.
* **Mutual TLS, bound to the role.** Nodes call each other over HTTPS (HTTP/2) with certificates of one CA. Each
  certificate carries the node id and the roles of the process as URIs
  `spiffe://<trust_domain>/role/<role>/node/<node_id>`: `api` for a process that makes calls (role `api`), `device`
  for one that answers them, both for role `all`. A node answers only callers whose certificate has the `api` role,
  so a node that only accepts devices can't make other nodes run commands; a caller checks that the answering
  certificate has the `device` role of the node the registry names. Certificate files are read again when they
  change, so short-lived certificates can be replaced without a restart. Issuing them is the deployment's job;
  `rportd cluster ca` and `rportd cluster cert` make a CA and node certificates for development and tests.

```toml
[cluster]
  node_id = "node-1"
  region = "us-central1"            # informational
  peer_address = "10.20.0.5:7443"   # where other nodes reach this one (nodes with a client listener)
  # peer_listen = "0.0.0.0:7443"    # the address to bind, if not peer_address
  trust_domain = "rport"            # default
  ca_file = "/etc/rport/peer/ca.crt"
  cert_file = "/etc/rport/peer/node.crt"
  key_file = "/etc/rport/peer/node.key"
```

* For development only, `peer_secret_file` (a file with a shared secret of at least 16 characters) with
  `insecure_dev = true` replaces the certificates: plain HTTP, and every holder of the secret can pose as any role.
* **Tunnels** go straight to the node holding them: give each node its own public name in `[server] tunnel_host`;
  each tunnel carries it as `tunnel_host`, which links to the tunnel use. Ports are allocated per node from its
  `used_ports`. Tunnels with a subdomain (`<tunnel>.<node>.<domain>`) need a wildcard DNS name and certificate per
  node and are not set up by the server yet.
* **Uploads** are sent to the node holding the client, which keeps the file in its upload directory while the client
  fetches it.
* A command's live output reaches the UI through the API node that started it; the result is also saved by the node
  holding the client, so a broken peer connection loses only the live view.
* A node that only accepts devices and holds no other database rights (a `device` database role) needs `EXECUTE` on
  the four registry functions.

Some state stays on the node that created it, so the load balancer must send a user's requests to the same API
node (session affinity) for:

* two-factor login: the code sent at login is known only to the node that sent it;
* login attempts and bans: each node counts failed logins on its own;
* device pairing codes;
* live job output in the UI (websockets) and uploads in progress.

## Organizations and platform operators

A tenant is an organization. **Platform operators** manage the organizations of a server: they are the
administrators of the reserved system organization `_system`, which `rportd migrate` creates. Operators log in with
the normal login. Inside `_system` they get only their own account, the operator users and groups, the `_system`
audit log and the operator API; they can't see or act in another organization through the rest of the API. No device
can belong to `_system`.

After upgrading, run `rportd migrate --grant-to <application role>` once more: the application role needs `EXECUTE`
on the functions through which operators change organizations (it can still only read the `tenants` table).

Create the first operator, then manage organizations with `rportd org` (`--dsn` is the schema owner or the
application role; default `[storage] dsn`):

```shell
rportd org add-admin _system ops@example.com -c /etc/rport/rportd.conf   # prints a one-time password
rportd org add acme "Acme Corp" --device-limit 50 -c /etc/rport/rportd.conf
rportd org add-admin acme alice@example.com -c /etc/rport/rportd.conf
rportd org list -c /etc/rport/rportd.conf                                # --json for JSON
rportd org set-limit acme 100 -c /etc/rport/rportd.conf                  # or "unlimited"
rportd org suspend acme -c /etc/rport/rportd.conf
rportd org activate acme -c /etc/rport/rportd.conf
```

* `add-admin` adds a login to the organization's Administrators group. A new login gets a generated password,
  printed once, that must be changed at the first login; a login that already has an account (a member of other
  organizations) is invited and joins with its own password when it accepts (see [Members of several organizations](#members-of-several-organizations)).
* `suspend` takes effect on the next request: the organization's users can't log in, and their sessions and API
  tokens stop working. `activate` undoes it. Organizations are not deleted.
* The device limit is the most devices an organization may have (unlimited when not set), enforced when devices
  connect (see `max_clients` above).

The same actions are in the **operator API** under `/api/v1/operator` (see the API reference, tag *Operator*), for
administrators of `_system` only: other organizations get 404, other `_system` users 403, and servers on SQLite
have no operator API.

| Method | Path | Does |
|---|---|---|
| `GET` | `/operator/organizations` | list organizations (id, name, status, created, device count, device limit, user count) |
| `POST` | `/operator/organizations` | create: `{"id": "acme", "name": "Acme Corp", "device_limit": 50}` |
| `GET` | `/operator/organizations/{id}` | one organization |
| `POST` | `/operator/organizations/{id}/suspend` | suspend |
| `POST` | `/operator/organizations/{id}/activate` | reactivate |
| `PUT` | `/operator/organizations/{id}/device-limit` | `{"device_limit": 100}` (`null`: unlimited) |
| `POST` | `/operator/organizations/{id}/admins` | `{"username": "alice@example.com"}`; returns the generated password of a new login once, or invites an existing account |
| `GET` | `/operator/organizations/{id}/members` | the members, their groups there and how many organizations each belongs to |

Every change, from the API or the CLI, is written to the audit log of `_system` (application
`operator.organization` or `operator.organization.admin`; the CLI as user `cli:<system user>`), whether or not
the audit log is enabled in the configuration. `GET /api/v1/status` reports `capabilities.operator: true` to
operators.

### Members of several organizations

A login (normally an email address) is one **account** with one password, password expiry, two-factor (TOTP)
secret and 2FA delivery address, and can be a **member** of several organizations, with its own permission groups
in each. An organization's administrators are the members of its Administrators group, as before.

* **Login.** A login of one organization sees no change. A login of several that names no organization gets, once
  its password is verified and before any 2FA, `"token": null` and `organizations` (id and name of each); the
  client repeats the login with `?organization=acme` (or `"organization": "acme"` in the body). That response adds
  `organization` (the one the token is for) and `organizations`. A login naming an organization it isn't a member
  of is refused like a wrong password.
* **Basic auth** with a password (sent with each request) names no organization: it works only for a login of one
  organization. **API tokens** belong to the organization they were created in, and a request authenticating with
  one goes there.
* **Switching.** `GET /api/v1/me/organizations` lists the login's active organizations (`current` marks the one of
  the request); `POST /api/v1/me/organization` with `{"organization": "acme"}` returns a session token for that
  organization. The old token keeps working in its own organization. Only session tokens switch; there is no new
  2FA, since the 2FA settings are the account's.
* **Inviting.** An administrator adds a login with `POST /api/v1/organization/members`
  (`{"username": "carol@example.com", "groups": ["Viewers"]}`). A new login gets an account with a generated
  password, returned once, that must be changed at the first login, and is a member at once. A login that already
  has an account gets a **pending invitation** (`"pending": true`): its owner sees it in `GET /api/v1/me/invitations`
  and accepts it with `POST /api/v1/me/invitations/acme` (a session token, not an API token) or declines it with
  `DELETE`. Administrators list their pending invitations with `GET /api/v1/organization/invitations` and withdraw
  one with `DELETE /api/v1/organization/invitations/{login}`. Operators adding an existing account as an
  administrator (`add-admin`) invite it the same way.
* **Removing.** `DELETE /api/v1/users/{user_id}` removes the membership: its sessions and API tokens in this
  organization stop working, its other memberships stay, and the account (with its pending invitations) goes with
  its last membership.
* **What an administrator can change.** The groups of any member. The password, its expiry, the 2FA address, the
  TOTP secret and the login name of a member of other organizations too belong to its owner (`PUT /api/v1/me`, the
  login's `new_password`, the TOTP routes): an administrator gets 403. For a member of one organization alone
  nothing changes.
* A suspended organization drops out of its members' lists, logins and switches.
* Whoever holds an account's password owns it: there is no proof of email ownership yet, so the response of an
  invitation says whether the login already had an account (`new_account`).

`GET /api/v1/status` reports `capabilities.organizations: true` on Postgres servers. None of this exists on SQLite.
`rportd migrate` turns the existing users into accounts (each login was in one organization so far). After
upgrading, run `rportd migrate --grant-to <application role>` once more: the application role needs `EXECUTE` on
the account and invitation functions (it can read only the accounts of its organization's members). Until then,
password changes and invitations fail with a message naming the fix.

## Usage metering

With `[metering]` on, every node counts what each organization uses, per UTC hour, and the API shows it to the
organization's administrators and to platform operators. Off by default; refused on SQLite.

```toml
[metering]
  enabled = true
  flush_interval = "60s"          # how often each process writes its counts (5 s to 30 min)
  # hourly_retention = "2160h"    # hourly usage and per-device daily transfer, default 90 days (at least 10)
  # daily_retention = "18600h"    # daily usage and device-days, default 25 months (at least hourly_retention)
```

After upgrading, run `rportd migrate --grant-to <application role>` (and `--grant-device-to <device role>` for
device processes) once more: both roles need `EXECUTE` on the function usage is written through (`ingest_usage`),
the application role also on the operators' summary functions. A device process with metering on refuses to start
until its role has it (one without metering starts as before). Days are rolled up and pruned by whichever node does
the cluster upkeep, metering on or not.

What is counted:

| Metric | Counted where |
|---|---|
| `tunnel_bytes_to_user`, `tunnel_bytes_to_device` | the tunnel's copy loop on the node holding the tunnel, both directions, while the connection runs (port tunnels, tunnels behind the built-in HTTP proxy, browser RDP/VNC through it, UDP datagrams) |
| `upload_bytes_to_devices` | the bytes each device reads of an uploaded file (a file sent to 100 devices counts 100 times; a transfer that stops half way counts half) |
| `api_bytes_out` | the bytes of API responses, websockets included, of requests resolved to the organization (failed logins included; not the UI's static files, not `_system`) |
| `jobs_command`, `jobs_script`, `jobs_library` | jobs a device started (a job for several devices counts once per device) |
| `device_seconds`, `devices_peak` | the devices connected to each node, sampled every 15 seconds: device-seconds per hour (the hour's average is device-seconds / 3600) and the most at once |
| device-days | the devices that had a session on each UTC day (`usage_device_days`): a device counts once a day, however many nodes saw it |
| `monitoring_rows` | the measurements stored, counted once a day by the cluster upkeep |

Each process keeps its counts in memory and writes them every `flush_interval`, one item per organization with a
unique key (node, process start, organization, flush number). A device process puts them in its outbox (kind
`usage`, never dropped when an organization's outbox is full) and the outbox ships them; other nodes call
`ingest_usage` directly. `ingest_usage` claims the item's key and adds its deltas in the same transaction, so
retries add nothing twice; it rejects items with unknown metrics, values out of range, hours not on the hour, more
than an hour ahead or more than 7 days old, and reserved organizations (suspended ones are kept). Usage keys are
kept 8 days.

Tables: `usage_hourly (tenant_id, hour, metric, value)`; `usage_daily`, rolled up from the hours of days that ended
more than two days ago (sums; the highest value for `devices_peak` and `monitoring_rows`) and recomputed for a week
while late data may arrive; `usage_device_days (tenant_id, day, client_id)`; `usage_client_daily (tenant_id, day,
client_id, metric, value)` for the transfer of each device. The upkeep (rollups, the monitoring count and the
retention) runs with the cluster upkeep, on one node at a time.

Reading it:

| Method | Path | For |
|---|---|---|
| `GET` | `/usage?from&to&granularity=hour\|day\|month` | administrators of an organization, their own organization: periods, totals and the 10 devices with the most transfer |
| `GET` | `/operator/organizations/{id}/usage?from&to&granularity` | operators, one organization's report |
| `GET` | `/operator/usage?month=YYYY-MM` (or `from&to`, `organization`, `metric`) | operators, every organization's totals, heaviest first (by `transfer_bytes` unless `metric` names another) |

`from` and `to` are RFC 3339 times or dates (UTC); the default is the current month. Hourly reports cover at most
31 days and only the hourly retention; day and month reports use whole UTC days. Each period has the counters, the
transfer (`transfer_bytes` = tunnel bytes both ways + upload bytes), `jobs_run`, `device_hours`, `devices_avg`,
`devices_peak` and, by day or month, `device_days`, `devices_billable` (the average of the daily device counts over
the days passed, rounded up) and `devices_peak_day`. `GET /api/v1/status` reports `capabilities.usage: true`. With
metering on, each tunnel in the API also shows `bytes: {to_user, to_device}`, its totals since it opened (live on
the node holding the tunnel; on other nodes as of the holder's last report of the client).

Accuracy:

* Bytes are payload bytes: TLS, websocket and SSH framing (some 5–10 %) are not counted. Through the built-in HTTP
  proxy the bytes counted are those between the proxy and the device.
* A process that dies loses at most one flush interval of counts (a minute) plus what its outbox did not ship;
  losses only under-count.
* `devices_peak` is the most devices one node saw at once: with several nodes accepting devices it is the highest
  node's count, not the cluster's (device-days and `device_seconds` add up across nodes correctly).
* Hours come from each node's clock; the count of the seconds before a sample is attributed to the sample's hour.

## Billing plans and limits

With `[billing]` on (it needs `[metering]`), every organization has a plan and a billing status, each calendar month
(UTC) is closed with frozen quantities, and transfer limits apply. There is no payment provider yet
(`provider = "none"`): plans, statuses, periods and limits work without one. Off by default; refused on SQLite.

```toml
[billing]
  enabled = true
  provider = "none"
  plans_file = "/etc/rport/plans.toml"   # private: names, prices, allowances, rates
  overage_review_above = 100.00          # hold a closed month with more overage (USD) for an operator's approval
  # implausible_growth_factor = 20       # don't cap on an hour this many times the month's average hour
```

After upgrading, run `rportd migrate --grant-to <application role>` and `--grant-device-to <device role>` again: the
application role needs `EXECUTE` on the billing functions (it can't write the billing tables directly), the device
role on `device_transfer_policy()`. A device process with billing on refuses to start without it.

When billing is turned on (and for organizations created later without a plan), every organization without a
billing record gets one on the free plan, paying, billing starting then, but with no plan chosen
(`plan_unassigned`): it is metered and shown, without transfer limits or rates, and its device limit is left alone.
Once an operator sets its plan (`rportd org set-plan`), the plan's limits apply and the device limit
(`tenants.max_clients`) follows the plan; a device limit an operator sets by hand afterwards (`rportd org
set-limit`) is kept until the plan changes.

### The plans file

All numbers below are placeholders. Sizes are `B`, `KiB` … `TiB` (or `KB` … `TB`); rates are bits per second
(`kbit`, `Mbit`, `Gbit`); prices are USD. Unknown keys are refused.

```toml
free_plan = "free"            # organizations without a record are on it; it has no price and overage = "cap"

[trial]                       # what a trial without a payment method is capped at; then the free plan
  plan = "team"
  days = 14
  max_clients = 25
  transfer = "25GiB"

[[plan]]
  id = "free"
  name = "Free"
  max_clients = 10            # tenants.max_clients follows the plan
  transfer_base = "10GiB"
  overage = "cap"             # the hard cap applies at hard_cap percent (default 100)
  tunnel_rate_limit = "2Mbit" # each tunnel connection, each direction
  throttle_rate = "256kbit"   # past the hard cap

[[plan]]
  id = "team"
  name = "Team"
  base_price = 49.00          # per month, includes included_devices
  included_devices = 50
  price_per_device = 0.80     # each billable device above them
  transfer_base = "250GiB"    # allowance = base + per device × billable devices
  transfer_per_device = "5GiB"
  overage = "bill"            # past the allowance: overage_price_per_gib; no hard cap unless a spending limit
  overage_price_per_gib = 0.20
  soft_cap = 80               # percent: the administrators are notified
  provider_prices = { base = "price_…", devices = "price_…", overage = "price_…" }
```

### Billing status

| Status | Charged | Notes |
|---|---|---|
| `paying` | yes | the default; on the free plan that is nothing |
| `trial` | no, until `trial_ends_at` | then `paying` on the same plan with a payment method on file, otherwise `paying` on the free plan; a trial without a payment method is capped at the `[trial]` devices and transfer |
| `exempt` | never | needs a reason; `exempt_limits = "plan"` (default) keeps the plan's limits, `"none"` lifts them; never sent to a payment provider |
| `comped` | no, until `comped_until` (the last free day) | then as a trial that ends |

Operators set plans and statuses with the API or the CLI; each change is written to the audit logs of `_system`
and of the organization (application `billing`):

```shell
rportd org set-plan acme team
rportd org set-billing acme trial --trial-ends 2026-12-08
rportd org set-billing partner exempt --reason "reseller partner" --exempt-limits none
rportd org set-billing acme comped --comped-until 2027-03-31 --reason "launch customer"
rportd org lift-cap acme --until 2026-12-01          # --end, --extra-transfer 50GiB, --clear-fail-open; the rest is kept
rportd org billing [acme]                            # accounts, or one with its closed periods
rportd org approve-period acme 2026-11               # release a held period
```

### Limits

Every 5 minutes one node (lease `billing-upkeep`) computes each organization's transfer this month (tunnel bytes
both ways and upload bytes delivered) against its allowance and writes the state:

| State | When | Effect |
|---|---|---|
| `normal` | below the soft cap | none |
| `soft` | at `soft_cap` percent | a notification to the administrators, `soft_cap_reached` in `GET /billing` |
| `over` | past 100% on a plan that bills overage | a notification; overage accrues |
| `capped` | the hard cap: `cap` plans (Free, trials without a payment method) at `hard_cap` percent, or a plan that bills overage once the organization's spending limit is reached | a notification; tunnels are throttled to `throttle_rate`, new tunnels and uploads are refused with `403 ERR_CODE_TRANSFER_LIMIT` |

Device sessions are never closed and existing tunnels never cut: commands, scripts, schedules, monitoring and
updates keep working. Every process reads the states once a minute through `device_transfer_policy()` and applies
the plan's `tunnel_rate_limit` to each tunnel connection (a token bucket per direction; with tunnels on several
nodes the rate applies on each). The api process refuses tunnels and uploads, and the process holding the device
refuses them again. One notification per kind per month is sent (email, through the notifications); trials and
comped periods without a payment method are announced 14 and 3 days before they end.

It fails open: the hard cap is not applied while the newest usage of any organization is more than 2 hours old
(metering stalled) or when an organization's last hour moved more than `implausible_growth_factor` times its
average hour (at least 10 GiB); the state is `over` and `fail_open` says why. Once a month failed open it stays open
for the rest of the month (the spike would otherwise become part of the average) until an operator checked it and
clears it (`rportd org lift-cap ID --clear-fail-open`); it is logged as an error and the administrators of
`_system` with an email address are notified once per organization and month. A process that can't read the states keeps the last ones for an hour, then applies only the per-tunnel
rates; a cap the upkeep wrote more than an hour ago is ignored. An operator can lift the cap until a time or raise
the month's allowance; an organization's administrators can set a spending limit (`PUT /billing/spending-limit`).

### Billing periods

Two hours after a month ends the upkeep closes it for every organization (`billing_periods`, kept detached when an
organization is deleted): billable devices (the average of the daily device counts over the days billed, rounded
up) and the peak day, transfer per kind, the allowance and the overage (in whole GiB, rounded up, on plans that
bill it), and the amounts at the plan's prices. The first month is prorated from the day billing started, and a
trial or comped period ends the free part. Usage arriving after the close is shown but never billed. States:
`ready` (to be charged), `held` (overage above `overage_review_above`, until an operator approves it),
`not_charged` (trial or comped the whole month, or nothing to charge), `never_charged` (exempt), `reported` (set by
the payment provider). A month is closed with the status and plan that applied at its end (a change made after it
ended, before the close, is not applied to it); that plan applies to the whole month. An organization that was
exempt and became paying during the month is billed from that day; one that became comped or exempt during the
month is not charged for it. Setting a trial or comped organization to paying ends its free time at once.

| Method | Path | For |
|---|---|---|
| `GET` | `/billing` | administrators: plan, status, allowance, used, transfer state, soft-cap flag |
| `GET` | `/billing/periods` | administrators: the closed periods |
| `PUT` | `/billing/spending-limit` | administrators: `{"spending_limit": 50.00}` (USD per month, `null` removes it) |
| `GET` | `/operator/billing`, `/operator/billing/periods?state=held` | operators: every organization |
| `GET` | `/operator/organizations/{id}/billing`, `…/billing/periods` | operators: one organization |
| `PUT` | `/operator/organizations/{id}/billing/plan`, `…/status`, `…/override` | operators |
| `POST` | `/operator/organizations/{id}/billing/periods/{YYYY-MM}/approve` | operators: release a held period |

`GET /api/v1/status` reports `capabilities.billing: true`.

## Developing

`go run ./server/storage/postgres/pgtest/devdb` starts a throwaway Postgres without Docker and prints a `[storage]`
section for it. The Postgres tests run with `make test-postgres`, or with `RPORT_TEST_POSTGRES_URL` set to a
superuser URL. Without that variable they are skipped.
