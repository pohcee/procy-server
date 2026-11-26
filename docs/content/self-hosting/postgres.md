---
title: "Postgres and clusters (preview)"
weight: 60
slug: postgres
aliases:
- /advanced/postgres/
---
{{< toc >}}

Keep a self-hosted server's data in Postgres instead of SQLite, and run several servers (*nodes*) on one shared
database. This is what [procy.sh](https://procy.sh) runs on. You need it for
[organizations](/docs/self-hosting/multi-tenancy/), self-service signup, [Google-only sign-in
policies](/docs/self-hosting/oidc/), [usage and billing](/docs/self-hosting/payment-provider/) and clustering. A
single server for one team doesn't need any of it: without a `[storage]` section the server keeps using its SQLite
files.

{{< hint type=caution title="Preview" >}}
Postgres storage and clustering are new. Test upgrades on a copy of your database first.
{{< /hint >}}

## What changes

With `[storage] backend = "postgres"` all data lives in Postgres: users, API tokens, sessions, client credentials,
clients, jobs, schedules, the library, the vault, the audit log, monitoring data and notifications. The data directory
still holds runtime files such as uploads (and, on a device process, its outbox).

* **Tenants.** Every row belongs to a tenant (an organization). A server for one organization keeps everything in the
  tenant `default`. Row-level security in Postgres keeps tenants apart, in addition to the server's own checks.
* **Users and client credentials** are stored in Postgres. The `auth`, `auth_file` and `auth_table` settings of
  `[server]`, the `auth`, `auth_file`, `auth_user_table`, `auth_group_table` and `auth_group_details_table` settings
  of `[api]`, and the `[database]` section are refused. Login by proxy header (`auth_header`) and `[plus-oauth]` are
  not supported. Manage users with `rportd user`, `rportd org`, the UI or the API.
* **Client credentials are hashed** (argon2id): the password is shown once when created.
* **Least privilege.** The server connects as an *application role* that doesn't own the tables and isn't a
  superuser; it refuses to start otherwise, because row-level security wouldn't apply. A separate *schema owner* runs
  the migrations.
* **No automatic migrations.** Run `rportd migrate` after each upgrade.
* **Shared secrets.** With the API on, `[api] jwt_secret` is required (it isn't generated into `data_dir`, because
  every node must sign and check tokens with the same secret). Set `[server] key_seed` too: without it the
  fingerprint changes on each start.
* **Vault.** The passphrase vault is refused; use `kms` or `local` keys (see [The vault](#the-vault)).

## Set up the database

The server is tested with Postgres 17. Create a schema owner, an application role and a database:

```sql
CREATE ROLE rport_owner LOGIN PASSWORD '…';
CREATE ROLE rport_app LOGIN PASSWORD '…' NOSUPERUSER NOBYPASSRLS;
CREATE DATABASE rport OWNER rport_owner;
```

If you will run [device processes](#node-roles), also create a device login. It must be a member of no role:

```sql
CREATE ROLE rport_device LOGIN PASSWORD '…' NOSUPERUSER NOBYPASSRLS;
```

Then configure the server:

```toml
[storage]
  backend = "postgres"
  ## The application role. Or dsn_file = "/etc/rport/postgres-dsn", a file holding the URL.
  dsn = "postgres://rport_app:…@db.example.com:5432/rport?sslmode=require"
  ## "all" (default): API and client listener; "api": API only; "device": client listener only.
  role = "all"

[server]
  key_seed = "…"

[api]
  ## Required on Postgres; the same on every node (openssl rand -hex 32).
  jwt_secret = "…"

[vault]
  key_provider = "local"
  local_key_file = "/etc/rport/vault.key"

[cluster]
  ## This node's name among the nodes sharing the database. Default: the hostname.
  #node_id = "node-1"
```

`node_id` has up to 63 letters, digits, `.`, `_` or `-`, starting with a letter or digit. If the hostname isn't a valid
node id (or can't be read), set it, or the server refuses to start.

Create the schema as the owner and grant the roles what they need:

```shell
rportd migrate -c /etc/rport/rportd.conf \
  --dsn 'postgres://rport_owner:…@db.example.com:5432/rport?sslmode=require' \
  --grant-to rport_app --grant-device-to rport_device
```

Leave out `--grant-device-to` if you have no device login. Running `rportd migrate` again says `nothing to migrate`.

Add the first user and start the server:

```shell
rportd user add -c /etc/rport/rportd.conf -u admin -g Administrators
rportd -c /etc/rport/rportd.conf
```

To run several organizations, create them and their administrators with `rportd org` instead; see
[organizations](/docs/self-hosting/multi-tenancy/).

## Upgrades

After installing a new release, run the same `rportd migrate` command, with `--grant-to` and `--grant-device-to`,
before you start the new version. New releases add tables and functions the roles need rights on; a node whose role
lacks them refuses to start (or a feature fails) with a message naming the fix. Running it twice does no harm.

## The vault

```toml
[vault]
  ## "kms" or "local" (required on Postgres)
  key_provider = "local"
  local_key_file = "/etc/rport/vault.key"
  ## for "kms":
  #kms_key = "projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key>"
```

Each tenant gets its own random vault key, stored in Postgres and wrapped by a 32-byte key from a local file
(`local`: hex or base64; create one with `openssl rand -hex 32`) or by a Google Cloud KMS key (`kms`, using the Google
application default credentials, which need encrypt and decrypt rights on the key). The vault needs no passphrase and
can't be locked.

{{< hint type=important >}}
**Back up the wrapping key.** Losing the key file or the KMS key loses every tenant's vault.
{{< /hint >}}

## Move a standalone server to Postgres

`rportd import` copies the data of a **stopped** standalone server into a tenant. It only reads the source, and refuses
a data directory a running server still holds. Import before starting the Postgres servers (or while they're stopped):
logins and vault writes during an import can collide with imported ids.

```shell
rportd import -c /etc/rport/rportd.conf \
  --from /var/lib/rport --from-config /etc/rport/rportd.conf.sqlite \
  --dsn 'postgres://rport_owner:…@db.example.com:5432/rport' \
  --with-history --dry-run
```

* `-c` is the configuration of the Postgres server; `--from` the old data directory; `--from-config` the old
  configuration file. Users and client credentials are read from the old auth settings (auth files, `auth`, or
  `[database]` tables on SQLite).
* `--tenant` (default `default`) is the tenant to import into. With `--dsn` (the schema owner) it is created if
  missing.
* `--with-history` also copies jobs, the audit log, monitoring data and notifications.
* `--dry-run` reads and checks everything and prints the counts, but writes nothing.
* Vault values are decrypted with the old passphrase and encrypted with the tenant's new key. Pass the passphrase in
  `RPORT_VAULT_PASSPHRASE`, or with `--vault-passphrase-stdin` on the first line of stdin.

The import runs in one transaction and prints, for each kind of data, how many rows it read, added, found present and
skipped, with reasons. Running it again adds nothing new. Client ids, credentials, passwords, API tokens and job ids
are kept, so clients and users carry on without changes.

Built-in library entries (such as `client-update` and `os-updates`) exist once per tenant. The import doesn't copy a
built-in the tenant already has; it points the schedules and jobs that use it at the tenant's entry and lists the ids
it mapped. Your own library entries keep their ids.

## Run several nodes

Several nodes can share one database and serve the same users, organizations, devices and data, behind a load
balancer. Nodes don't replicate data to each other: the database is the only shared state, and every node is the same
binary.

### What the nodes share

Give every node:

* the same `[api] jwt_secret`, so a token issued by one node is accepted by the others;
* the same `[server] key_seed`, so devices see the same fingerprint whichever node they reach;
* the same vault wrapping key (the same `kms_key`, or the same key file);
* the same `[server] url`, the public device addresses that pairing hands out;
* the same `purge_disconnected_clients` and `keep_disconnected_clients` (device limits are counted with them);
* the same time zone (`TZ`): schedules fire at the cron time of the node's zone;
* its **own** `[cluster] node_id`. Two nodes with the same id would both claim the same clients.

### Node roles

`[storage] role` sets what a process serves:

| Role | Serves | Database login |
| --- | --- | --- |
| `all` (default) | the API, the UI and the client listener | the application role |
| `api` | the API and the UI; no client listener | the application role |
| `device` | the client listener only (`[api] address` must be empty) | the device role |

A **device process** connects with the narrow device role: it can only call the functions devices need (credential
checks, client records, tunnel rules, the ingest functions), never read users, tokens, sessions or the vault. What
devices report goes into a local outbox (`outbox.db` in its data directory) and is shipped to the database in batches,
so nothing is lost while the database is briefly unreachable. A device process needs no `jwt_secret` and no `[vault]`.
A break-in through the client listener lands in a process that can't read accounts.

Small clusters can run one `all` process per node. For the strongest isolation, run two processes of the same binary
on each node, under different system users with different database logins: an `api` process and a `device` process,
both with the same `node_id`.

### Peer calls

Without peer calls, only **one** node may accept client connections. Run it with `role = "all"` and every other node
with `role = "api"`. The `all` node holds a lease in the database; a second `all` node refuses to start ("node …
accepts the client connections of this database") until the first has been stopped for a minute. API nodes answer
"client is not connected" for work on clients (commands, scripts, tunnels), so send that work to the `all` node.

With peer calls, any number of nodes accept clients, and every API process can work on every client: it looks up the
node holding the client's connection in the database and passes the work on (commands and scripts with live output,
uploads, tunnels, port checks, update status, tags and labels). Nodes call each other over HTTPS with mutual TLS:

```toml
[cluster]
  node_id = "node-1"
  region = "us-central1"            # informational
  ## Where other nodes reach this process (processes with a client listener only).
  peer_address = "10.20.0.5:7443"
  #peer_listen = "0.0.0.0:7443"     # the address to bind, if not peer_address
  trust_domain = "rport"            # default
  ca_file = "/etc/rport/peer/ca.crt"
  cert_file = "/etc/rport/peer/node.crt"
  key_file = "/etc/rport/peer/node.key"
```

Peer calls are on when the certificate files are set. All certificates come from one CA and carry the node id and the
process's roles as URIs `spiffe://<trust_domain>/role/<role>/node/<node_id>`: role `api` for a process that makes
calls, `device` for one that answers them, both for an `all` process. A process answers only callers with the `api`
role, so a device process can't make other nodes run commands, and a caller checks that the answering certificate is
the `device` of the node the database names. Certificate files are re-read when they change, so short-lived
certificates can be rotated without a restart.

Issuing certificates is up to your deployment. For development and tests, `rportd cluster ca` and
`rportd cluster cert` create a CA and node certificates. Also for development only, `peer_secret_file` (a shared
secret of at least 16 characters) with `insecure_dev = true` replaces the certificates: plain HTTP, and anyone with
the secret can pose as any role. Keep the peer port reachable from the cluster's own nodes only.

### An api process and a device process

The two halves of one node, sharing `node_id`, `key_seed` and the CA. The api process:

```toml
[server]
  url = ["https://connect.example.com"]
  key_seed = "…"
  data_dir = "/var/lib/rport-api"

[api]
  address = "0.0.0.0:3000"
  base_url = "https://procy.example.com"
  jwt_secret = "…"
  doc_root = "/opt/procy/docroot"
  trusted_proxies = ["10.0.0.0/8"]

[storage]
  backend = "postgres"
  dsn_file = "/etc/rport/api-dsn"
  role = "api"

[vault]
  key_provider = "kms"
  kms_key = "projects/…/cryptoKeys/…"

[cluster]
  node_id = "node-1"
  ca_file = "/etc/rport/peer/ca.crt"
  cert_file = "/etc/rport/peer/api.crt"
  key_file = "/etc/rport/peer/api.key"
```

The device process:

```toml
[server]
  address = "0.0.0.0:8080"
  url = ["https://connect.example.com"]
  key_seed = "…"
  data_dir = "/var/lib/rport-device"
  ## this node's own public name: tunnels opened here are reached through it
  tunnel_host = "node-1.connect.example.com"
  used_ports = ['20000-30000']

[api]
  address = ""

[storage]
  backend = "postgres"
  dsn_file = "/etc/rport/device-dsn"
  role = "device"

[cluster]
  node_id = "node-1"
  peer_address = "10.20.0.5:7443"
  peer_listen = "0.0.0.0:7443"
  ca_file = "/etc/rport/peer/ca.crt"
  cert_file = "/etc/rport/peer/device.crt"
  key_file = "/etc/rport/peer/device.key"
```

If you turn on [metering or billing](/docs/self-hosting/payment-provider/), set `[metering]` and `[billing]` in both
processes: device processes count tunnel bytes and enforce transfer limits too.

### Load balancer

Use two hostnames, for example `procy.example.com` for the UI and API and `connect.example.com` for devices.

* **UI and API** go to the api (or `all`) processes. Keep them close to the database: an API request makes many
  queries in a row, so an API process one region away is much slower.
* **Devices** go to the device (or `all`) processes of every region. Device connections are long-lived websockets:
  set the backend timeout well above the client keepalive, and drain connections slowly when a node goes away.
  Devices reconnect to another node by themselves.
* **TLS** ends at the load balancer; list its addresses in `trusted_proxies` of the api processes. Behind a load
  balancer all devices seem to come from its addresses, so set `max_failed_login = 0` in `[server]` of the device
  processes, or one device's wrong password bans everyone (see
  [securing the server](/docs/self-hosting/securing-the-server/)).
* **Tunnels** bypass the load balancer: a port tunnel is reached at the `tunnel_host` of the node holding it, plus its
  port, so give each device node its own public name and open its `used_ports` in the firewall. Ports are allocated
  per node. [Tunnels on subdomains](/docs/remote-access/tunnels-on-subdomains/) are not set up per node yet.

Some state stays in the API process that created it. If several API processes serve the UI, the load balancer must
send a user's requests to the same one (session affinity) for:

* two-factor login: the code sent at login is known only to the process that sent it;
* login attempts and bans: each process counts failed logins on its own;
* device pairing codes;
* live job output in the UI (websockets) and uploads in progress.

### How the nodes stay in sync

* **Caches.** Each node caches clients and API sessions. A change on one node reaches the others within about a second
  through a change feed in the database (`NOTIFY` plus polling). A session revoked on one node (logout, password
  change, deleted user) is refused by the others.
* **Background work.** Clean-ups, library pruning, metering roll-ups and billing jobs run on one node at a time: a node
  takes a lease in the database and renews it; when it stops, another takes over within about one and a half
  intervals.
* **Schedules.** Every node knows every schedule. Each run is recorded in the database before it starts, so it starts
  once however many nodes are due to start it. Without peer calls only the `all` node starts runs.
* **Connected devices.** Device processes record each connection in the database and renew it every 20 seconds. A
  device connecting to a second node replaces its older session. When a node dies without saying so, its devices are
  shown disconnected within about a minute.

### Operating a cluster

* **Long transactions hold back every node.** A transaction left open on the database (an idle `psql` session that
  changed something, a long import) delays the change feed until it ends; nodes log a warning. Set
  `idle_in_transaction_session_timeout` on the database, and prefer a database server that doesn't also run long
  transactions for other applications.
* **The change feed keeps three days.** A node stopped for longer (or cut off from the database) reloads what it
  caches.
* **After restoring a dump into another Postgres server**, run `rportd migrate` before starting the nodes: it resets
  the change feed, whose transaction ids come from the old server. Nodes refuse to start until then.
* **When the database is down**, connected devices keep their sessions and tunnels and device processes queue results
  in their outbox. New device connections are refused and retried, and the UI and API return errors.

## Organizations, signup and billing

These need Postgres and have their own pages:

* [Organizations](/docs/self-hosting/multi-tenancy/): platform operators, organizations, members of several
  organizations, device limits and self-service signup.
* [Usage, plans and payments](/docs/self-hosting/payment-provider/): metering, billing plans, transfer limits and
  Stripe.
* [Sign in with Google](/docs/self-hosting/oidc/): required for platform operators.

## Developing

`go run ./server/storage/postgres/pgtest/devdb` starts a throwaway Postgres without Docker and prints a `[storage]`
section for it. Run the Postgres tests with `make test-postgres`, or with `RPORT_TEST_POSTGRES_URL` set to a superuser
URL; without it they are skipped.
