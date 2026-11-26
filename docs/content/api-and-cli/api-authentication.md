---
title: "API tokens and authentication"
weight: 10
slug: api-authentication
aliases:
- /get-started/api-authentication/
- /docs/no02-api-auth.html
- /docs/content/get-started/no02-api-auth.md
---
{{< toc >}}

Scripts and integrations call the Procy API with a personal **API token**. This page shows how to create one, how
to call the API with it, and the other ways to authenticate. It applies to hosted Procy and to self-hosted servers.
For where a self-hosted server keeps its users, two-factor sign-in and delegated authentication, see
[user authentication](/docs/self-hosting/user-authentication/).

## Create an API token

1. In the web interface, open **Settings → API tokens** and click **Add API token**.
2. Enter a **Name**, such as "backup script".
3. Choose a **Scope**:

   | Scope | Allows |
   | --- | --- |
   | **Read** | `GET` requests only, no WebSockets. |
   | **Read + write** | Everything you can do yourself. |
   | **Clients auth (admin)** | Only the client credential endpoints (`/clients-auth`). Administrators only. |

4. Optionally set **Expires at**. Without it, the token is valid until you delete it.
5. Copy the token from **Token created**. It is shown only once.

The token looks like `Ab3dE6gH_8f14e45f-ceea-467f-a0a6-1e6f2c7b3d9a`. The part before the underscore is its
**prefix**, which the token list shows so you can tell tokens apart. **Edit** changes a token's name and expiry;
**Delete** revokes it at once.

A token acts as you, with your permissions, in the organization you created it in. If you belong to several
organizations, create a token in each one you want to script. Removing you from an organization revokes your tokens
there.

API tokens keep working when you sign in with Google or another identity provider, when your organization requires
that sign-in, and when you use two-factor sign-in: they are not passwords.

## Call the API

Send your username and the token with HTTP basic authentication. On hosted Procy your username is the email address
you sign in with, and the API is at `https://procy.sh/api/v1`:

```shell
export PROCY=https://procy.sh/api/v1
export CREDS='you@example.com:Ab3dE6gH_8f14e45f-ceea-467f-a0a6-1e6f2c7b3d9a'

curl -s -u "$CREDS" "$PROCY/me" | jq
curl -s -u "$CREDS" "$PROCY/clients" | jq '.data[] | {id, name, connection_state}'
```

On a self-hosted server, use its address, for example `https://procy.example.com/api/v1`.

The API answers in JSON; [jq](https://jqlang.github.io/jq/) makes it readable. Results are in `data`; errors come
as a list in `errors`, each with a `code`, `title` and `detail`. A wrong or expired token gives `401`. A request
the token's scope doesn't allow is refused with "the scope of the provided token is not authorized for this
operation". After several failed attempts, the server answers `429` for a while, so a script should stop at the first
`401` instead of retrying.

See the [API reference](/docs/api-and-cli/api-reference/) for the endpoints, and
[procy-cli](/docs/api-and-cli/cli/) for ready-made commands built on them.

{{< hint type=important >}}
Treat a token like a password: keep it out of scripts in version control (read it from an environment variable or a
secrets store), give it the smallest scope that works, and set an expiry for tokens you hand to other systems.
{{< /hint >}}

## Manage tokens through the API

| Method and path | What it does |
| --- | --- |
| `GET /me/tokens` | List your tokens (prefix, name, scope, created, expires; never the secret). |
| `POST /me/tokens` | Create a token: `{"name": "...", "scope": "read", "expires_at": "2027-01-01T00:00:00Z"}`. The response holds the token, once. |
| `PUT /me/tokens/{prefix}` | Change the `name` and `expires_at`. |
| `DELETE /me/tokens/{prefix}` | Revoke the token. |

## Session tokens

The web interface uses short-lived session tokens (JWTs). You can get one for a script too, by signing in to
`/login` with your **password** and sending the token as a bearer token:

```shell
TOKEN=$(curl -s -u admin:$PASSWORD "https://procy.example.com/api/v1/login?token-lifetime=3600" | jq -r .data.token)
curl -s -H "Authorization: Bearer $TOKEN" https://procy.example.com/api/v1/clients | jq
```

`token-lifetime` is in seconds: 10 minutes by default, at most 90 days. `DELETE /logout` ends the session.

This needs a password, so it is mainly for self-hosted servers. On hosted Procy you sign in with Google and have no
password; use an API token.

With two-factor sign-in on, `/login` returns a token that is good only for `/verify-2fa`. Send the code there with
that token to get a full session token; see
[two-factor authentication](/docs/self-hosting/user-authentication/#two-factor-auth).

## Basic authentication with a password

A self-hosted server also accepts your username and password on every request, the same way as a token:

```shell
curl -s -u admin:$PASSWORD https://procy.example.com/api/v1/clients | jq
```

This stops working when two-factor sign-in is on, when your organization requires sign-in through an identity
provider, and for logins that belong to more than one organization. Use an API token in those cases (and preferably
always), so the script doesn't hold your password.

**Self-hosted:** session tokens are signed with `[api] jwt_secret`. If you don't set it, a random secret is created
on first start and saved as `rportd-jwt-secret.txt` in the data directory. Servers that must accept each other's
tokens need the same secret (at least 16 characters, for example `openssl rand -hex 32`).
