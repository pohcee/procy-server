---
title: "Sign in with Google (OpenID Connect)"
weight: 80
slug: oidc
aliases:
- /advanced/oidc/
---
{{< toc >}}

Let users of a self-hosted server sign in with Google (or another OpenID Connect provider) instead of, or next to, a
password. On [procy.sh](https://procy.sh) Google sign-in is already on. With `[oidc]` enabled, the login page shows a
**Sign in with Google** button next to the password form. The server never sees a Google password: Google signs the
user in, and the server verifies the ID token it gets back.

## How accounts are matched

* **A Google sign-in never creates an account.** It signs in to an existing login whose username is the Google
  account's verified email address (case is ignored). Users still come from an administrator, an invitation or
  [self-service signup](/docs/self-hosting/multi-tenancy/#self-service-signup).
* **Only addresses Google manages can be linked:** Gmail and Google Workspace accounts. A consumer Google account
  registered with another address (which its holder may no longer control) is refused (`unmanaged_email`); its owner
  signs in with the password.
* **The first sign-in pins the Google account** (issuer and subject) to the login. Afterwards the login is found by the
  pin, even if the Google email changes, and a different Google account that later claims the same address is
  refused. An administrator removes the pin with **Unlink Google account** on the **Users** page
  (`DELETE /api/v1/users/{user}/oidc-identity`) when someone changes Google accounts. Renaming a login removes it too.
* **No second factor from the server.** Google enforces its own 2-Step Verification, so a Google sign-in asks for no
  TOTP or 2FA code. To require a second factor for every member, enforce 2-Step Verification in your Google Workspace.
* A member of several organizations gets the same organization picker as after a password. The session is the same as
  after a password login (token, lifetime, permissions).

## Who may sign in how

On a standalone server (SQLite) every user may use both a password and Google. On a server with
`[storage] backend = "postgres"`, each member signs in with a **password**, with **Google**, or **both** (the
default):

* **Platform operators** (members of `_system`) sign in with Google only. A password login is refused even when the
  account has a password, and `rportd org add-admin _system <email>` creates operators without one.
* **An organization can require Google sign-in.** Its administrators turn it on with **Require Google sign-in** on the
  **Organization** page (API: `PUT /api/v1/organization/sign-in`; operators:
  `PUT /api/v1/operator/organizations/{id}/sign-in`). Password logins then get 403 `oidc_required`, password sessions
  end at their next request (on other nodes within 15 seconds), and new members are created without a password. An
  administrator can turn it on only while signed in with Google, so they can't lock themselves out.
* **Per member.** An administrator sets a member's method on the **Users** page (API: `sign_in` in
  `POST /api/v1/users` and `PUT /api/v1/users/{user}`). Members change their own on their profile
  (`PUT /api/v1/me/sign-in`); "Google only" needs one Google sign-in first.
* **API tokens** are not passwords: they keep working.

The policy applies only while `[oidc]` is enabled, so a provider outage or a configuration mistake can be fixed by
turning `[oidc]` off. Accounts created without a password still can't sign in with one: an empty password never
matches.

## Create the Google OAuth client

1. In the [Google Cloud console](https://console.cloud.google.com/), pick a project, open **APIs & Services → OAuth
   consent screen**, and configure it: user type **External** (or **Internal** to allow only your Workspace), the app
   name, a support email, and the scopes `openid`, `email` and `profile` only.
2. Open **APIs & Services → Credentials → Create credentials → OAuth client ID**, application type **Web
   application**.
3. Under **Authorized redirect URIs** add the server's callback, exactly: `https://<api domain>/api/v1/oidc/callback`.
   Google allows no wildcards: every domain the API is reached under needs its own entry.
4. Save, and copy the client ID and secret. Store the secret in a file only the user `rportd` runs as can read, for
   example `/etc/rport/oidc-client-secret` with mode `0640`, owner `root`, group `rport`.

## Configure the server

```toml
[oidc]
  enabled = true
  ## The provider. Default: Google.
  #issuer = "https://accounts.google.com"
  client_id = "1234-abc.apps.googleusercontent.com"
  ## The client secret is read from this file only, never from the config itself.
  client_secret_file = "/etc/rport/oidc-client-secret"
  ## The redirect URI registered with the provider.
  redirect_url = "https://procy.example.com/api/v1/oidc/callback"
  ## The web UI a sign-in returns to (its page /auth/oidc). Default: the scheme and host of redirect_url.
  #ui_url = "https://procy.example.com"
  ## Only these Google Workspace domains (hd claim); with another provider, the email domain.
  #allowed_domains = ["example.com"]
  ## The label of the button on the login page.
  #button_label = "Sign in with Google"
```

The server reads the secret file at start-up and refuses to start when it is missing or empty. The provider's
discovery document and keys are fetched on the first sign-in, so the server starts even when the provider can't be
reached. `redirect_url` and `issuer` must be `https` (plain `http` only for `localhost` and loopback addresses).

In a [cluster](/docs/self-hosting/postgres/#run-several-nodes), give every API process the same `[oidc]` settings. A
sign-in may start on one node and come back to another: pending sign-ins are kept in the database.

## How a sign-in works

1. The button sends the browser to `GET /api/v1/oidc/login`. The server stores a pending sign-in (random state, nonce
   and PKCE verifier; valid for 10 minutes, used once), sets a cookie that binds it to this browser, and redirects to
   the provider.
2. The provider sends the browser back to `GET /api/v1/oidc/callback`. The server checks the cookie, redeems the code
   with the PKCE verifier, and verifies the ID token: signature, issuer, audience (the client ID), expiry and nonce.
   The email must be verified, and `allowed_domains` applies.
3. The server redirects to the UI's page `/auth/oidc` with a one-time sign-in code in the URL fragment (valid for 5
   minutes), or with an error. The page trades the code for a session at `POST /api/v1/oidc/token`. Only the browser
   that started the sign-in can do that (the cookie goes with it), so nobody can sign someone else in to their own
   account with a link.

Failed Google sign-ins don't count toward the password-guessing bans; only made-up sign-in codes at
`POST /api/v1/oidc/token` count against the IP address. Every Google sign-in, failed or successful, is written to the
audit log (application `auth.oidc`); failures that belong to no organization go to the platform operators' audit log.

## Managing operators

```shell
# add a platform operator: no password; they sign in with Google as ops@example.com
rportd org add-admin _system ops@example.com -c /etc/rport/rportd.conf
# remove one (sessions and API tokens end; the last one stays unless --force)
rportd org remove-admin _system ops@example.com -c /etc/rport/rportd.conf
```

See [organizations](/docs/self-hosting/multi-tenancy/) for everything else operators do.
