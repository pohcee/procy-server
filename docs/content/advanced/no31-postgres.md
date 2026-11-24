---
title: "Running the server on Postgres (preview)"
weight: 31
slug: postgres
---
{{< toc >}}

{{< hint type=caution title="Preview" >}}
**Preview.** Postgres storage is new. It is the first step towards clusters of servers sharing one database.
For now, run **one** server per database. Without a `[storage]` section the server uses its SQLite files, as it
always has.
{{< /hint >}}

## What changes

With `[storage] backend = "postgres"` the server keeps all its data in a Postgres database instead of the SQLite
files in `data_dir`. That includes users, API tokens, sessions, client credentials, clients, jobs, schedules, the
library, the vault, the audit log, monitoring data and the notification log. The data directory still holds the
server key, the generated JWT secret (when `jwt_secret` isn't set), uploads and other runtime files.

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
```

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
  ## "passphrase" (default), "kms" or "local"
  key_provider = "local"
  local_key_file = "/etc/rport/vault.key"
  ## for "kms": kms_key = "projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key>"
```

* `passphrase`: the vault is set up and unlocked with a passphrase, as on SQLite. It is locked again after each
  restart.
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

With `key_provider = "kms"` or `"local"`, the vault values are decrypted with the old vault passphrase and encrypted
again with the tenant's new key. Pass the passphrase in `RPORT_VAULT_PASSPHRASE`, or with
`--vault-passphrase-stdin` on the first line of stdin. With the passphrase vault, the values are copied as they
are, and the old passphrase unlocks the vault.

## Tenants

`rportd tenant add ID [NAME] --dsn <owner URL>` adds another tenant. Its users log in to the same server, and each
login lands in its tenant. In this preview, devices connect only to the `default` tenant, and schedules and
clean-ups run only there.

## Developing

`go run ./server/storage/postgres/pgtest/devdb` starts a throwaway Postgres without Docker and prints a `[storage]`
section for it. The Postgres tests run with `make test-postgres`, or with `RPORT_TEST_POSTGRES_URL` set to a
superuser URL. Without that variable they are skipped.
