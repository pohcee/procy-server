---
title: 'Client authentication'
weight: 50
slug: client-authentication
aliases:
- /get-started/client-authentication/
- /docs/no03-client-auth.html
- /docs/get-started/no03-client-auth.md
---

{{< toc >}}

Choose where a self-hosted server keeps the credentials clients log in with: a client auth id and a password. On
[procy.sh](https://procy.sh), and on any server on [Postgres](/docs/self-hosting/postgres/), credentials live in the
database and you manage them on the **Client access** page of the web UI or through the API; you can skip this page.

A standalone server reads client credentials from one of three sources:

1. a single credential in the config file;
2. a JSON file;
3. a database table.

Use only one; you can't mix them. With a file or a table you can manage credentials through the API and the UI.

## A single credential

{{< hint type=caution >}}
Not for production. If all clients share one credential, you can't lock out a single client, and if the password
leaks you must reconfigure every client.
{{< /hint >}}

In the `[server]` section of `rportd.conf`:

```text
auth = "rport:a-strong-password12345"
```

`rportd` refuses to start with the sample credential `clientAuth1:1234` from `rportd.example.conf`. Restart
`rportd` to apply the change.

A client passes the credential in one of three ways; the first one found wins:

1. the command line: `--auth rport:a-strong-password12345`;
2. its config file, in the `[client]` section: `auth = "rport:a-strong-password12345"`;
3. the environment variable `RPORT_AUTH`.

## A file

For one credential per client, create a JSON file, for example `/var/lib/rport/client-auth.json`:

```json
{
    "client1": "yienei5Ch-quoo3Eiv",
    "client2": "ieRi1Noo2-Thaech4u",
    "client3": "Oow8ooqu-ahGh2iet"
}
```

and point to it in the `[server]` section:

```text
auth_file = "/var/lib/rport/client-auth.json"
```

`rportd` reads the file only at start-up; edits made while it runs have no effect until a restart. To manage the
credentials through the API and the UI, make the file writable by the `rport` user
(`chown rport /var/lib/rport/client-auth.json`).

## Using a database table

Set up the database connection in the `[database]` section of `rportd.conf` first. MySQL/MariaDB and SQLite are
supported; the [example config](https://github.com/pohcee/procy-server/blob/main/rportd.example.conf) explains the
settings. Create the table yourself:

{{< tabs "create-table" >}}
{{< tab "MySQL/MariaDB" >}}

```sql
CREATE TABLE `clients_auth` (
  `id` varchar(100) PRIMARY KEY,
  `password` varchar(100) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8;
```

{{< /tab >}}
{{< tab "SQLite" >}}

```sql
CREATE TABLE `clients_auth` (
  `id` varchar(100) PRIMARY KEY,
  `password` varchar(100) NOT NULL
);
```

{{< /tab >}}
{{< /tabs >}}

Then name the table in the `[server]` section and restart `rportd`:

```text
auth_table = "clients_auth"
```

## On Postgres

On a server with `[storage] backend = "postgres"` the `auth`, `auth_file` and `auth_table` settings are refused:
credentials are stored in Postgres, as argon2id hashes. When you create one, the password is shown once (leave it
empty and the server generates one); afterwards nobody can read it, not even an administrator. Client auth ids are
unique across all organizations on the server, because the id tells the server which organization a client belongs
to. [Moving to Postgres](/docs/self-hosting/postgres/#move-a-standalone-server-to-postgres) keeps the existing ids and
passwords, so clients carry on without changes.

## Manage credentials through the API

The `/clients-auth` endpoint of the [API](/docs/api/) lists, adds and deletes credentials (administrators only). It
isn't available with a single credential.

List the credentials (standalone servers show the passwords, Postgres servers don't):

```shell
curl -s -u admin:$PASSWORD https://procy.example.com/api/v1/clients-auth | jq
```

Add one:

```shell
curl -X POST https://procy.example.com/api/v1/clients-auth \
  -u admin:$PASSWORD \
  -H 'Content-Type: application/json' \
  --data-raw '{"id": "client4", "password": "<CLIENT_PASSWORD>"}'
```

Ids and passwords need at least 3 characters. The API supports filtering and pagination.

If a third-party tool writes the file or the table directly, turn the API's write access off in `[server]`; writes then
get HTTP 403:

```text
auth_write = false
```
