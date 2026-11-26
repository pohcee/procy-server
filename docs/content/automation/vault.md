---
title: "Vault"
weight: 50
slug: vault
aliases:
- /get-started/vault/
- /docs/no13-vault.html
---
{{< toc >}}

The vault stores passwords, keys and notes on the server, encrypted, so your team can share them without copying
them around. An entry can belong to one device or be global, and can be limited to a user group. This page applies
to hosted Procy and to self-hosted servers; only [unlocking](#self-hosted-initialize-unlock-and-lock) differs.

* **Hosted Procy:** the vault is ready to use. Each organization's entries are encrypted with the organization's own
  key; there is no passphrase, and the vault is never locked.
* **Self-hosted:** an administrator sets a passphrase once, and unlocks the vault with it after every restart of the
  server (unless the server uses a key provider, see below).

Your user group needs the **vault** permission (see [permissions](/docs/hosted/permissions-model/)).

## Use the vault in the web interface

Open **Settings → Vault**. Entries are grouped into global entries and entries per device.

Click **Add entry** and fill in:

**Key**
: A name for the entry, such as `ssh-user` or `router admin password`.

**Type**
: `secret` (default), `text`, `string` or `markdown`. The type tells the interface how to show the value.

**Client ID (optional)**
: The device the entry belongs to. Leave it empty for a global entry.

**Required user group (optional)**
: If set, only members of this user group can read, change or delete the value.

**Value**
: The secret itself. Only this field is encrypted; the key, client ID and the other fields are stored in clear
  text, so don't put secrets in them.

Use the menu next to an entry to view, edit or delete it.

{{< hint type=tip >}}
[procy-cli](/docs/api-and-cli/cli/) reads per-device entries: `procy-ssh` and `procy-tunnel` take the SSH user from
the device's `ssh-user` entry, and `procy-metadata` prints all of a device's entries.
{{< /hint >}}

## Use the vault through the API

The examples use an [API token](/docs/api-and-cli/api-authentication/) and hosted Procy:

```shell
export PROCY=https://procy.sh/api/v1
export CREDS='you@example.com:<api-token>'
```

Add an entry:

```shell
curl -s -u "$CREDS" -X POST "$PROCY/vault" \
  -H 'Content-Type: application/json' \
  --data-raw '{
    "client_id": "client123",
    "required_group": "",
    "key": "ssh-user",
    "value": "admin",
    "type": "string"
  }' | jq
```

The response holds the new entry's `id`.

List entries (without their values):

```shell
curl -s -g -u "$CREDS" "$PROCY/vault?filter[client_id]=client123&sort=key" | jq
```

Read one entry with its decrypted value:

```shell
curl -s -u "$CREDS" "$PROCY/vault/5" | jq
```

```json
{
  "data": {
    "id": 5,
    "client_id": "client123",
    "required_group": "",
    "key": "ssh-user",
    "value": "admin",
    "type": "string",
    "created_at": "2026-11-25T09:46:07Z",
    "created_by": "you@example.com",
    "updated_at": "2026-11-25T09:46:07Z",
    "updated_by": "you@example.com"
  }
}
```

Change an entry with `PUT /vault/{id}` and the full body (all fields, as when adding it), and delete one with
`DELETE /vault/{id}`.

Lists can be sorted and filtered by `id`, `client_id`, `created_by`, `created_at` and `key`, for example
`?sort=-created_at` (newest first) or `?filter[key]=one,two` (either key). With curl, pass `-g` so the brackets are
not treated as patterns.

## Back up the vault

If the vault's key is lost (on self-hosted servers: the passphrase), its values can't be read. Keep a backup of the
values, for example with this script. It needs an API token of a member of a group that can read every entry.

```bash
#!/bin/sh
set -e
PROCY=https://procy.sh/api/v1
CREDS='you@example.com:<api-token>'
DIR=./vault-backup
mkdir -p "$DIR"
for ID in $(curl -sf -u "$CREDS" "$PROCY/vault" | jq -r '.data[].id'); do
  curl -sf -u "$CREDS" "$PROCY/vault/$ID" -o "$DIR/$ID.json"
done
tar czf vault-backup.tar.gz "$DIR"
# remove the clear-text copies
find "$DIR" -type f -exec shred -u {} \;
rm -rf "$DIR"
```

The archive holds the values in clear text. Store it encrypted.

## Self-hosted: initialize, unlock and lock

**Self-hosted only.** On a standalone server the vault is protected by a passphrase of 4 to 32 bytes. The server keeps
the passphrase only in memory, so the vault is **locked** after every restart until an administrator unlocks it. If the
passphrase is lost, the values are lost with it.

In the web interface, **Settings → Vault** shows **Initialize the vault** the first time (enter and repeat the
passphrase), **Unlock the vault** when it is locked, and **Lock vault** while it is unlocked. Through the API
(administrators only):

```shell
# status: "init" is "uninitialized" or "setup-completed", "status" is "locked" or "unlocked"
curl -s -u admin:$PASSWORD https://procy.example.com/api/v1/vault-admin | jq

# initialize, once
curl -s -u admin:$PASSWORD -X POST https://procy.example.com/api/v1/vault-admin/init \
  -H 'Content-Type: application/json' --data-raw '{"password": "<VAULT_PASSPHRASE>"}'

# unlock after a restart
curl -s -u admin:$PASSWORD -X POST https://procy.example.com/api/v1/vault-admin/sesame \
  -H 'Content-Type: application/json' --data-raw '{"password": "<VAULT_PASSPHRASE>"}'

# lock
curl -s -u admin:$PASSWORD -X DELETE https://procy.example.com/api/v1/vault-admin/sesame
```

Back up the vault database in the server's data directory along with the passphrase.

A server on Postgres can instead protect the vault with a key provider, `[vault] key_provider = "kms"` (Cloud KMS)
or `"local"` (a key file): each tenant gets its own key, there is no passphrase, and the vault is never locked. The
web interface then hides the passphrase forms. See [the vault on Postgres](/docs/self-hosting/postgres/#the-vault).
