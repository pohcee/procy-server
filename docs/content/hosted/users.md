---
title: "Users and user groups"
weight: 30
slug: users
aliases:
- /get-started/users/
  - /docs/no12-users.html
---
{{< toc >}}

Users are the people who sign in to Procy. This page shows how to manage users, their groups and their sessions in the
web interface and through the API. It applies to hosted Procy and to self-hosted servers. On hosted Procy, users are
the members of your organization: for inviting people and how they sign in, see
[Organizations and members](/docs/hosted/organizations/).

## In the web interface

Administrators manage users under **Settings**:

- **Users** lists everyone who can sign in, with their groups. On hosted Procy you add people with **Invite member**;
  on a self-hosted server with one organization, with **Add user**. The actions menu of each user has **Edit**,
  **Manage sessions** (see and end their sign-ins), **Unlink Google account** (when one is linked) and **Remove from
  organization** or **Delete**.
- **User groups** lists the groups and the permissions each one grants. A user gets every permission of every group
  they're in. See the [permissions model](/docs/hosted/permissions-model/).

Each user manages their own password, two-factor authentication and sign-in method under **Settings → Profile**.

Removing or deleting a user ends their sessions and revokes their API tokens at once.

## Passwords

Where users have passwords, the server hashes them before storing them. A new password needs at least 14 characters
and must pass a check against common words and patterns. On a self-hosted server, `password_min_length` and
`password_zxcvbn_minscore` in the `[api]` section change these rules.

Accounts that [sign in with Google only](/docs/hosted/organizations/#require-google-sign-in) need no password.

## Through the API

The `/users` and `/user-groups` endpoints manage users and groups. They need an administrator. The
[API reference](/docs/api/) lists every field. The examples use an [API token](/docs/api-and-cli/api-authentication/);
replace `https://procy.sh` with your own server's address if you self-host.

### List users

```shell
curl -s -u admin@example.com:$TOKEN https://procy.sh/api/v1/users
```

```json
{
  "data": [
    { "username": "admin@example.com", "groups": ["Administrators"] },
    { "username": "ops@example.com", "groups": ["Operators"] }
  ]
}
```

The list never contains password hashes.

### Create a user

```shell
curl -s -u admin@example.com:$TOKEN -X POST https://procy.sh/api/v1/users \
  -H 'Content-Type: application/json' \
  --data-raw '{
    "username": "ops@example.com",
    "password": "<NEW_PASSWORD>",
    "groups": ["Operators"]
  }'
```

On hosted Procy, prefer **Invite member** in the web interface: it also handles people who already have an account in
another organization.

### Change a user

Send only the fields you want to change. `groups` replaces the whole list, so include every group the user should
stay in:

```shell
curl -s -u admin@example.com:$TOKEN -X PUT https://procy.sh/api/v1/users/ops@example.com \
  -H 'Content-Type: application/json' \
  --data-raw '{ "groups": ["Operators", "Support"] }'
```

Group changes take effect at once, also for sessions that are already open. A password change takes effect at the
user's next sign-in: a session that is already open keeps working until it ends. To end it now, delete the user's
sessions (`DELETE /api/v1/users/{user}/sessions`) or use **Manage sessions** in the web interface.

### Delete a user

```shell
curl -s -u admin@example.com:$TOKEN -X DELETE https://procy.sh/api/v1/users/ops@example.com
```

On hosted Procy this removes the user from your organization only.

### Set a group's permissions

```shell
curl -s -u admin@example.com:$TOKEN -X PUT https://procy.sh/api/v1/user-groups/Operators \
  -H 'Content-Type: application/json' \
  --data-raw '{ "permissions": { "commands": true, "tunnels": true, "monitoring": true } }'
```

## Self-hosted: where users are stored

On a self-hosted server, users live in a database, a JSON file or a single static user, as described in
[API authentication](/docs/api-and-cli/api-authentication/). This changes what you can do:

- **Database:** everything on this page works. Group permissions need the `group_details` table
  (`auth_group_details_table`).
- **JSON file (`auth_file`):** the user API works, but changes made outside the API are read only when `rportd`
  restarts. Group permissions can't be stored, so only `Administrators` can use most functions.
- **Single static user (`auth`):** there's no user management.

Create at least one user in the `Administrators` group before you use the user API.
