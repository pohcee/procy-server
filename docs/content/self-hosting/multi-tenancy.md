---
title: "Organizations"
weight: 70
slug: multi-tenancy
aliases:
- /get-started/multi-tenancy/
- /docs/no11-multi-tenancy.html
---
{{< toc >}}

Serve several separate customers or teams (organizations) from one self-hosted Procy installation. On
[procy.sh](https://procy.sh) every account already belongs to an organization, and
[organizations and members](/docs/hosted/organizations/) explains how to manage yours; this page is for running
such a server yourself.

There are two ways:

* **Organizations on Postgres.** One server (or [cluster](/docs/self-hosting/postgres/#run-several-nodes)) on
  Postgres serves many organizations behind one URL for the UI and one for devices. Platform operators create and
  suspend organizations; each organization's administrators manage their own users and devices. This is what
  procy.sh runs.
* **Separate instances.** Several isolated `rportd` processes on one host, each with its own config, data directory
  and ports. Simple, and works on SQLite, but nothing is shared: every instance has its own URL, users and fingerprint.

## Separate instances

Store this systemd template as `/etc/systemd/system/rportd@.service` (the `@` matters):

```text
[Unit]
Description=Procy server instance %i
ConditionFileIsExecutable=/usr/local/bin/rportd

[Service]
ExecStart=/usr/local/bin/rportd -c /etc/rport/instances/rportd.%i.conf
LimitNOFILE=1048576
User=rport
Restart=always
RestartSec=120

[Install]
WantedBy=multi-user.target
```

Put one config file per instance in `/etc/rport/instances/`, each with its own `data_dir`, log file, client listener
and API ports. Start an instance with `systemctl enable --now rportd@<name>`.

## Organizations on Postgres

Set up [Postgres storage](/docs/self-hosting/postgres/) first. On SQLite none of this exists.

Every organization is a tenant: its users, devices, jobs, vault and audit log are kept apart by the server and by
row-level security in Postgres. All organizations' devices connect to the same client listener. Client auth ids are
unique across the server, so the id a device logs in with tells the server its organization; creating a credential
whose id another organization uses fails (HTTP 409).

### Platform operators

Platform operators manage the organizations. They are the administrators of the reserved organization `_system`,
which `rportd migrate` creates. In `_system` they see only their own account, the operators, the `_system` audit log
and the operator console; they can't see devices or data of other organizations. No device can belong to `_system`.

Operators sign in with Google only, so turn on [Sign in with Google](/docs/self-hosting/oidc/) first. Then add the
first operator by their Google email address:

```shell
rportd org add-admin _system ops@example.com -c /etc/rport/rportd.conf
```

With `[oidc]` on, the operator gets no password at all. They open the UI, choose **Sign in with Google**, and find the
**Operator console**: an overview, the organizations with their members, devices, usage and activity, pending signups
and invitations, deletions, platform usage and billing, the servers of a cluster, and the operators' audit log.

**Servers** lists the nodes of a [cluster](/docs/self-hosting/postgres/#run-several-nodes) that hold device
connections: each node's status from its heartbeat (*live* up to a minute, *stale* up to a day, *gone* after that,
still listed at the bottom), version, role, devices online and their organizations, connects and disconnects of the
last 24 hours, and its traffic today and over the period. A node's page adds a daily chart, the organizations on it,
its devices and its recent connections. Traffic is counted per node with [usage
metering](/docs/self-hosting/payment-provider/) on, from the release that added this view; older traffic isn't
attributed to a node. A node on an older release shows no version or role until it is upgraded.

### Manage organizations

From the CLI (`--dsn` is the schema owner or the application role; default `[storage] dsn`):

```shell
rportd org add acme "Acme Corp" --device-limit 50 -c /etc/rport/rportd.conf
rportd org add-admin acme alice@example.com -c /etc/rport/rportd.conf
rportd org list -c /etc/rport/rportd.conf                     # --json for JSON
rportd org set-limit acme 100 -c /etc/rport/rportd.conf       # or "unlimited"
rportd org suspend acme -c /etc/rport/rportd.conf
rportd org activate acme -c /etc/rport/rportd.conf
rportd org remove-admin acme alice@example.com -c /etc/rport/rportd.conf
```

* `add-admin` adds a login to the organization's Administrators group. A new login gets a generated password,
  printed once, that must be changed at the first login, unless it has to sign in with Google (operators, and
  organizations that require Google): then it gets none. A login that already has an account is invited, and becomes
  an administrator when it accepts.
* `remove-admin` removes the membership, with its sessions and API tokens there, and the account too when it was its
  last organization. The last administrator stays unless you add `--force`.
* `suspend` applies on the next request: the organization's users can't sign in, their sessions and API tokens stop
  working, and its devices are refused when they connect (connected ones stay until they reconnect). `activate` undoes
  it.

The same actions are in the operator console and the operator API under `/api/v1/operator` (tag *Operator* in the
[API reference](/docs/api/)). Only operators may use it: other organizations get 404, other `_system` users 403.
Every change, from the API or the CLI, is written to the `_system` audit log.

The console's views are **metadata only**: counts, usage, names, device versions and operating systems, audit log
applications, actions, users and addresses. Never device contents, command output, files, vault values, device
addresses or tags, or audit request and response bodies.

### Device limits

An organization's device limit is the most devices it may have (unlimited when not set). It counts the devices that
are connected or were connected within `keep_disconnected_clients`, as the device list shows them, across all nodes.
A device the organization already has always reconnects; a new one is refused once the limit is reached. The device
logs `connection refused: the organization's limit of 10 clients is reached` and retries after its
`max_retry_interval`, but not sooner than a minute. With [billing plans](/docs/self-hosting/payment-provider/) the
limit follows the organization's plan.

### Members of several organizations

A login (normally an email address) is one **account** with one password, two-factor settings and Google link, and can
be a **member** of several organizations, with its own user groups in each. An organization's administrators are the
members of its Administrators group.

* **Signing in.** A member of several organizations picks one after the password (or Google), before any 2FA code.
  The UI shows an organization switcher; switching needs no new 2FA.
* **Inviting.** An administrator adds a member on the **Users** page (API: `POST /api/v1/organization/members`). A
  new login gets an account at once. An existing account gets a pending invitation, which its owner accepts or
  declines on the **Invitations** page.
* **Removing.** Removing a member ends its sessions and API tokens in this organization; its other memberships stay.
* **Ownership.** For a member of several organizations, the password, 2FA settings and login name belong to its
  owner: an administrator can change only its groups.
* **API tokens** belong to the organization they were created in. HTTP basic auth with a password works only for a
  login of a single organization.

`GET /api/v1/status` reports `capabilities.organizations: true` on Postgres servers.

## Self-service signup

With `[signup]` on, visitors create their own organization and become its first administrator. Off by default;
Postgres only. Turning it off closes signup without touching existing organizations.

```toml
[signup]
  enabled = true
  public_url = "https://procy.example.com"   # the web UI the links in the emails point to
  require_approval = true                    # hold verified signups for an operator
  # notify_email = "support@example.com"     # told "New signup waiting" when a signup awaits approval
  captcha = "turnstile"                      # or "hcaptcha", or "" (none)
  captcha_secret_file = "/run/secrets/turnstile"
  captcha_site_key = "0x4AAAA..."            # public, given to the UI
  blocked_domains_file = "/etc/rport/disposable-domains.txt"
  # allowed_domains_file = ""                # only these domains (private servers)
  # check_mx = false                         # refuse domains without a mail server
  max_per_ip_per_day = 3                     # a negative value turns a limit off
  max_per_domain_per_day = 10
  max_per_email_per_day = 3
  # unlimited_domains = [...]               # no per-domain limit (default: large shared mail providers)
  # token_ttl = "24h"                        # how long a verification link is valid
  # deletion_hold = "720h"                   # how long a deleted organization is kept suspended
  # mail = "smtp"                            # "log": only log the emails and links (development)
  # email_notice = "Preview: features and plans may change."  # added to every signup email
```

The emails go through [`[smtp]`](/docs/self-hosting/2fa-messaging/#smtp).

How a signup works:

1. The visitor enters an email address and an organization name. The captcha, the domain lists and the daily limits
   (counted in the database, so they hold across nodes) are checked, then a single-use link is emailed. The answer is
   the same whatever the address, so the form doesn't reveal who has an account.
2. The visitor opens the link and chooses a password. The organization is created with an id made from its name and a
   random suffix (`acme-corp-x7k2m9`), and the visitor becomes its administrator. With
   [billing](/docs/self-hosting/payment-provider/) on, it starts on the trial of the plans file, or the free plan.
3. With `require_approval`, the signup waits in the operators' **Signups** queue instead. The queue shows how the email
   was proven, whether its domain has a mail server, whether it is a free mail provider, and earlier signups or denials
   of the same email or domain. Approving mails a "you're in" email; denying mails the operator's reason, if given.

With [`[oidc]`](/docs/self-hosting/oidc/) on, the signup page also offers **Continue with Google**: the Google
account's verified email replaces the emailed link (Gmail and Google Workspace addresses only), and the account signs
in with Google and has no password.

Every step is written to the `_system` audit log. Signups expire after `token_ttl`; verified ones waiting for approval
after 30 days.

**Email confirmation.** Signup confirms the account's email. Accounts an administrator creates by inviting a new login
start unconfirmed, and members confirm their address from their profile. While `[signup]` is on, an unconfirmed
account can't be invited into further organizations or accept invitations, and a signup for its address takes it
over: proving the mailbox is the stronger claim to an email login.

### Leaving

An organization's administrators can, on the **Organization** page or through the API:

* **Export** the configuration as JSON (`GET /api/v1/organization/export`): users and groups, clients and client
  groups, stored tunnels, schedules and library entries. No passwords, TOTP secrets, API tokens, client credentials or
  vault values.
* **Delete** the organization (`POST /api/v1/organization/deletion`). It is suspended at once, its administrators get
  an email with a link to cancel, and after `deletion_hold` it is deleted with all its data; its vault key is
  destroyed and its id is never given out again. An operator who reactivates the organization during the hold calls
  the deletion off. Operators can start, cancel or expedite deletions in the console.
