---
title: "Sign in with Google (OpenID Connect)"
weight: 33
slug: oidc
---
{{< toc >}}

## What it does

With `[oidc]` enabled the login page shows a **Sign in with Google** button next to the password form. Any OpenID
Connect provider works; Google is the default issuer. The server never stores a Google password: Google signs the
user in and the server verifies the ID token it gets back.

* A Google sign-in never creates an account. It signs in to an existing login whose username is the Google account's
  verified email address (case is ignored). New users still come from an administrator or from
  [self-service signup](../postgres/).
* At the first Google sign-in the Google account (issuer and subject) is **pinned** to the login. Afterwards the
  login is found by the pin, even when the Google email changes, and a different Google account that later claims
  the same address is refused. An administrator can remove the pin (`DELETE /api/v1/users/{user}/oidc-identity`,
  or **Unlink Google account** in the web UI) when someone changes Google accounts. Renaming a login removes its pin.
* A Google sign-in asks for no second factor from the server (no TOTP, no 2FA code): Google enforces its own 2-Step
  Verification. Organizations that want a second factor for every member should enforce 2-Step Verification in
  their Google Workspace.
* After a Google sign-in, a login that belongs to several organizations gets the same organization picker as after
  a password.
* The session is the same as after a password login (same token, lifetime and permissions). It also records that it
  signed in through Google.

## Who may sign in how

Each member of an organization signs in with a **password**, through **Google**, or **both** (the default for
everyone, existing users included). On a server with `[storage] backend = "postgres"`:

* **Platform operators** (the members of the `_system` organization) sign in through Google only. A password login
  is refused even when the account has a password, `rportd org add-admin _system <email>` creates an operator
  without any password, and the API gives operators none.
* An organization's administrators (or a platform operator) can **require Google sign-in** for the whole
  organization (web UI: Organization page; API: `PUT /api/v1/organization/sign-in`, operators
  `PUT /api/v1/operator/organizations/{id}/sign-in`). Its members can no longer use their password: password logins
  get 403 with the error code `oidc_required`, and password sessions end at their next request (on other servers of
  a cluster within 15 seconds). New members of such an organization are created without a password.
* An administrator sets a member's method on the Users page (API: `sign_in` in `POST /api/v1/users` and
  `PUT /api/v1/users/{user}`). Members can change their own method on their profile (`PUT /api/v1/me/sign-in`);
  "Google only" needs one Google sign-in first, so nobody locks themselves out.
* API tokens are not passwords: they keep working.

On a standalone server (SQLite) every user may use both; there are no per-user or per-organization settings.

The policy applies only while `[oidc]` is enabled, so a provider outage or a configuration mistake can be fixed by
turning `[oidc]` off. Accounts created without a password still can't sign in with one: an empty password never
matches.

## Create the Google OAuth client

1. In the [Google Cloud console](https://console.cloud.google.com/), pick the project, open **APIs & Services →
   OAuth consent screen**, and configure it: user type **External** (or **Internal** to allow only your Workspace),
   the app name, a support email, and the scopes `openid`, `email` and `profile` only.
2. Open **APIs & Services → Credentials → Create credentials → OAuth client ID**, application type **Web
   application**.
3. Under **Authorized redirect URIs** add the callback of the server, exactly:
   `https://<api domain>/api/v1/oidc/callback`. Google allows no wildcards: every domain the API is reached under
   needs its own entry (or its own client).
4. Save, and copy the client ID and the client secret. Store the secret in a file readable only by the user rportd
   runs as, for example `/etc/rport/oidc-client-secret` with mode `0640`, owner `root`, group `rport`.

## Configure the server

```toml
[oidc]
  enabled = true
  ## The provider. Default Google.
  #issuer = "https://accounts.google.com"
  client_id = "1234-abc.apps.googleusercontent.com"
  ## The client secret is read from this file only (never from the config itself).
  client_secret_file = "/etc/rport/oidc-client-secret"
  ## The redirect URI registered with the provider: https://<api domain>/api/v1/oidc/callback
  redirect_url = "https://rport.example.com/api/v1/oidc/callback"
  ## The web UI a sign-in returns to (its page /auth/oidc). Default: the scheme and host of redirect_url.
  #ui_url = "https://rport.example.com"
  ## Allow only these domains: the Google Workspace domain of the account (hd claim), else its email domain.
  #allowed_domains = ["example.com"]
  ## The label of the button on the login page.
  #button_label = "Sign in with Google"
```

The server reads the secret file at start-up and refuses to start when it is missing or empty. The issuer's
discovery document and keys are fetched on the first sign-in, so the server starts even when the provider can't be
reached. `redirect_url` and `issuer` must be https (plain http only for `localhost` and loopback addresses).

In a cluster every API server needs the same `[oidc]` settings. A sign-in may start on one server and come back to
another: the pending sign-ins are kept in the database.

## How a sign-in works

1. The button sends the browser to `GET /api/v1/oidc/login`. The server stores a pending sign-in (a random state,
   nonce and PKCE verifier; valid for 10 minutes and used once), sets a cookie that binds it to this browser, and
   redirects to Google.
2. Google sends the browser back to `GET /api/v1/oidc/callback`. The server takes the pending sign-in, checks the
   cookie, redeems the code with the PKCE verifier, and verifies the ID token: its signature (the issuer's keys), the
   issuer, the audience (the client ID), the expiry and the nonce. The email must be verified by Google, and
   `allowed_domains` applies.
3. The server redirects to the web UI's page `/auth/oidc` with a one-time sign-in code in the URL fragment (valid
   for 5 minutes), or with an error. The page trades the code for a session at `POST /api/v1/oidc/token`.

Failed Google sign-ins don't count toward the ban of an IP address or user for wrong passwords; only made-up
sign-in codes at `POST /api/v1/oidc/token` count for the IP address. Every Google sign-in, failed or successful, is
written to the audit log (application `auth.oidc`); failures that belong to no organization yet go to the audit log
of the platform operators.

## Managing operators

```shell
# add a platform operator: no password; they sign in with Google as ops@example.com
rportd org add-admin _system ops@example.com -c /etc/rport/rportd.conf
# remove one (sessions and API tokens end; the last administrator stays unless --force)
rportd org remove-admin _system ops@example.com -c /etc/rport/rportd.conf
```

`rportd org remove-admin` works for every organization: it removes the administrator's membership, and the account
too when it was its last organization.
