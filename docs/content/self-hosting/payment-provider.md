---
title: "Usage, plans and payments (preview)"
weight: 100
slug: payment-provider
aliases:
- /advanced/payment-provider/
---
{{< toc >}}

Meter what each organization uses, put organizations on plans with limits, and collect payments through Stripe, on a
self-hosted server with [organizations](/docs/self-hosting/multi-tenancy/). This is how [procy.sh](https://procy.sh)
bills (for its plans, see [billing and usage](/docs/hosted/billing-and-usage/)); a server for your own team
doesn't need it.

{{< hint type=caution title="Preview" >}}
Only on [Postgres](/docs/self-hosting/postgres/). Each part is off by default and builds on the one before:
`[metering]` counts usage, `[billing]` (needs `[metering]`) adds plans, limits and monthly periods, and
`[billing] provider = "stripe"` adds payments. A server without them has no billing routes, jobs or tables in use.
{{< /hint >}}

After turning any of them on, run `rportd migrate --grant-to <application role> --grant-device-to <device role>` once
more: the roles need rights on the functions usage and billing are written through. A process whose role lacks them
refuses to start.

In a [cluster](/docs/self-hosting/postgres/#run-several-nodes), set `[metering]` and `[billing]` on every process,
device processes included: they count tunnel bytes and enforce transfer limits. The background jobs run on one node at
a time.

## Usage metering

```toml
[metering]
  enabled = true
  flush_interval = "60s"          # how often each process writes its counts (5 s to 30 min)
  # hourly_retention = "2160h"    # hourly usage and per-device daily transfer, default 90 days (at least 10)
  # daily_retention = "18600h"    # daily usage and device-days, default 25 months (at least hourly_retention)
```

Every process counts per organization and UTC hour:

| Metric | What |
| --- | --- |
| `tunnel_bytes_to_user`, `tunnel_bytes_to_device` | tunnel traffic, both ways (port, HTTP proxy, RDP/VNC, UDP) |
| `upload_bytes_to_devices` | the bytes each device reads of an upload (sent to 100 devices: counted 100 times) |
| `api_bytes_out` | API responses of the organization's requests, websockets included; not static files |
| `jobs_command`, `jobs_script`, `jobs_library` | jobs a device started (a job for several devices counts once per device) |
| `device_seconds`, `devices_peak` | connected devices, sampled every 15 s: device-seconds per hour and the most at once |
| device-days | the devices that had a session on each UTC day, once per device however many nodes saw it |
| `monitoring_rows` | the measurements stored, counted once a day |

Each process writes its counts every `flush_interval`, with a unique key, so retries never count twice. Device
processes ship them through their outbox.

Administrators see their organization's usage on the **Usage** page (`GET /api/v1/usage?from&to&granularity=
hour|day|month`); operators see every organization's in the console (`GET /api/v1/operator/usage`, and
`/operator/organizations/{id}/usage`). With metering on, each tunnel in the API also shows its bytes so far.

How accurate it is:

* Bytes are payload bytes: TLS, websocket and SSH framing (some 5–10 %) are not counted.
* A process that dies loses at most one flush interval of counts plus what its outbox didn't ship. Losses only
  under-count.
* `devices_peak` is the highest count of one node, not of the cluster; device-days and `device_seconds` add up across
  nodes correctly.

## Plans and limits

```toml
[billing]
  enabled = true
  provider = "none"                      # plans, limits and periods without payments
  plans_file = "/etc/rport/plans.toml"   # private: names, prices, allowances, rates
  overage_review_above = 100.00          # hold a closed month with more overage (USD) for an operator
  # implausible_growth_factor = 20       # don't cap on an hour this many times the month's average hour
  # egress_cost_per_gib = 0.13           # for the operators' estimated egress cost
```

When billing is turned on, every organization without a billing record gets one on the free plan, but with no plan
chosen: it is metered and shown, without transfer limits, and its device limit is left alone. Once an operator sets
its plan, the plan's limits apply and the device limit follows the plan. A device limit set by hand afterwards
(`rportd org set-limit`) is kept until the plan changes. New organizations from signup start on the trial.

### The plans file

The numbers are placeholders. Sizes are `B`, `KiB` … `TiB` (or `KB` … `TB`), rates are bits per second (`kbit`,
`Mbit`, `Gbit`), prices are USD. Unknown keys are refused.

```toml
free_plan = "free"            # organizations without a record are on it; no price, overage = "cap"

[trial]                       # a new organization's trial; capped like this until a payment method is on file
  plan = "team"
  days = 14
  max_clients = 25
  transfer = "25GiB"

[[plan]]
  id = "free"
  name = "Free"
  max_clients = 10            # the device limit follows the plan
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
  provider_prices = { base = "price_…", devices = "price_…", overage = "price_…" }   # Stripe only
```

The public plan catalog for a home page is `GET /api/v1/plans` (no provider ids).

### Billing status

| Status | Charged | Notes |
| --- | --- | --- |
| `paying` | yes | the default; on the free plan that is nothing |
| `trial` | no, until `trial_ends_at` | then paying on the same plan with a payment method, else the free plan |
| `exempt` | never | needs a reason; `exempt_limits = "plan"` keeps the plan's limits, `"none"` lifts them |
| `comped` | no, until `comped_until` (the last free day) | then as a trial that ends |

Operators set plans and statuses in the console or the CLI; each change is written to the audit logs of `_system` and
of the organization:

```shell
rportd org set-plan acme team
rportd org set-billing acme trial --trial-ends 2026-12-08
rportd org set-billing partner exempt --reason "reseller partner" --exempt-limits none
rportd org set-billing acme comped --comped-until 2027-03-31 --reason "launch customer"
rportd org lift-cap acme --until 2026-12-01      # or --end, --extra-transfer 50GiB, --clear-fail-open
rportd org billing [acme]                        # every organization, or one with its closed periods
rportd org approve-period acme 2026-11           # release a held period
```

### Transfer limits

Every 5 minutes one node compares each organization's transfer this month (tunnel bytes both ways plus upload bytes)
with its allowance:

| State | When | Effect |
| --- | --- | --- |
| `normal` | below the soft cap | none |
| `soft` | at `soft_cap` percent | the administrators are notified |
| `over` | past 100 % on a plan that bills overage | notified; overage accrues |
| `capped` | at the hard cap | notified; tunnels throttled to `throttle_rate`; new tunnels and uploads get 403 `ERR_CODE_TRANSFER_LIMIT` |

The hard cap is `hard_cap` percent on `cap` plans (Free, and trials without a payment method), and the organization's
spending limit on plans that bill overage. Administrators set a spending limit on the **Billing** page
(`PUT /api/v1/billing/spending-limit`).

Device sessions are never closed and open tunnels never cut: commands, scripts, schedules, monitoring and updates keep
working. Each tunnel connection is also held to the plan's `tunnel_rate_limit`, per direction and per node.
Notifications are emailed once per kind and month; trials and comped periods without a payment method are announced
14 and 3 days before they end.

**It fails open.** The hard cap isn't applied while metering is stalled (an organization's newest usage is more than
2 hours old), or when an organization's last hour moved more than `implausible_growth_factor` times its average hour
(at least 10 GiB). The state is then `over`, with `fail_open` saying why. A month that failed open stays open until an
operator has checked it and runs `rportd org lift-cap <id> --clear-fail-open`; the operators are emailed once per
organization and month. A process that can't read the limits keeps the last ones for an hour, then applies only the
per-tunnel rates.

### Billing periods

Two hours after a month ends (UTC), it is closed for every organization with frozen quantities: billable devices (the
average of the daily device counts, rounded up) and the peak day, transfer by kind, the allowance and the overage (in
whole GiB, rounded up), and the amounts at the plan's prices. The first month is prorated from the day billing
started. Usage that arrives after the close is shown but never billed. A month is closed with the status and plan
that applied at its end.

| Period state | Meaning |
| --- | --- |
| `ready` | to be charged |
| `held` | overage above `overage_review_above`; waits for an operator's approval |
| `not_charged` | trial or comped all month, or nothing to charge |
| `never_charged` | exempt |
| `reported` | sent to the payment provider |

Administrators see their plan, usage against the allowance and closed periods on the **Billing** page
(`GET /api/v1/billing`, `/billing/periods`). Operators see every organization's in the console
(`/api/v1/operator/billing…`). A deleted organization's billing records are kept, detached, for accounting.

## Payments with Stripe

With `provider = "stripe"`, organizations on a paid plan pay through Stripe. The server's own usage stays the source of
truth: it reports each closed month to Stripe, and Stripe turns it into an invoice, collects the money and computes
sales tax. Exempt organizations are never sent to Stripe.

```toml
[billing]
  enabled = true
  provider = "stripe"
  plans_file = "/etc/rport/plans.toml"
  stripe_secret_key_file = "/run/secrets/stripe-key"          # secret or restricted key
  stripe_webhook_secret_file = "/run/secrets/stripe-webhook"   # the endpoint's signing secret, whsec_…
  return_url = "https://procy.example.com/billing"             # checkout and the portal return here
  # stripe_automatic_tax = true                                # Stripe Tax in checkout (default on)
  # grace_days = 14                                            # full service after a failed payment
  # read_only_days = 14                                        # then read-only, then suspension proposed
```

Secrets are read only from files, never from the config file, the database or the UI, and never logged. In a cluster
only the api processes need the Stripe settings; device processes keep `provider = "none"`.

### Set up Stripe

The server creates customers, checkout sessions, portal sessions and meter events. It never creates products, prices
or meters; set those up yourself:

1. **Meters**, one per metric, aggregation *sum*, default payload keys (`stripe_customer_id`, `value`): extra devices
   (billable devices above the plan's included ones) and overage GiB. All plans can share them.
2. **Prices** per plan: a licensed monthly base price, and metered prices for extra devices and overage, backed by the
   meters. Put their ids in the plan's `provider_prices`.
3. **A webhook endpoint** at `https://<server>/api/v1/billing/webhook` with the events `checkout.session.completed`,
   `customer.subscription.created`, `customer.subscription.updated`, `customer.subscription.deleted`,
   `customer.subscription.trial_will_end`, `invoice.paid` and `invoice.payment_failed`. Pin it to an API version of
   the server's release train (`2026-10-28.endive` or a later `.endive` version): events of another train are refused
   with 400 and retried by Stripe.
4. **An invoice finalization grace period** (Billing → Invoice settings) of **at least 6 hours** (up to 72) for
   subscription cycle invoices with metered prices. Usage counts on a monthly invoice only if it arrives before the
   invoice finalizes, by default one hour after the month ends; the server closes the month two hours after it ends
   and reports within the next 15 minutes. Without it, usage is recorded in Stripe but not invoiced.
5. **Stripe Tax** registrations for where you collect tax, and the **customer portal** (payment methods, invoices, tax
   ids, cancellation). Plan changes go through the server, so limits change with the price.

### How it works

* **Checkout.** An organization's administrator chooses a plan on the **Billing** page. The server creates the Stripe
  customer and a subscription checkout, its billing cycle anchored to the 1st of next month (UTC) with the partial
  first month prorated, Stripe Tax on, billing address and tax ids collected. With a live subscription, a new plan
  replaces the subscription's prices at once.
* **Usage in arrears.** For each closed period ready to be charged, one meter event per organization and metric
  (extra devices, overage GiB) with the identifier `procy:<organization>:<metric>:<YYYY-MM-01>`. Zero quantities are
  not sent; held periods wait for an operator.
* **Reconciliation.** Daily, each report is compared with Stripe's meter event summary: equal is `confirmed`; nothing
  after 6 hours is sent again with the same identifier; anything else is a `mismatch` for operators, never corrected
  automatically.
* **Webhooks** carry no session: the `Stripe-Signature` header is verified (5 minutes' tolerance) before anything is
  parsed. Each event id is recorded in the same transaction that applies it, so a replay changes nothing and a failure
  is retried by Stripe. Out-of-order subscription events are ignored. Changes are audited with the actor
  `billing:stripe` and the event id.
* **Dunning.** A failed payment starts `grace_days` of full service and emails the organization's administrators.
  Then the organization is **read-only**: users sign in and see everything, but every change is refused with 403
  `ERR_CODE_BILLING_READ_ONLY` (no new jobs, tunnels, uploads or configuration changes; scheduled runs pause).
  Devices stay connected, open tunnels stay up, and the customer portal keeps working. After `read_only_days` more,
  **suspension is proposed** to operators, who confirm it. A successful payment restores everything at once. A
  canceled subscription moves the organization to the free plan, never to suspension.

### API

| Method | Path | For |
| --- | --- | --- |
| `POST` | `/billing/webhook` | Stripe (signature-verified, no session) |
| `GET` | `/billing/payment` | administrators: subscription, payment method, overdue state |
| `GET` | `/billing/invoices?limit` | administrators: the latest invoices, with links to Stripe's pages |
| `POST` | `/billing/portal` | administrators: a link to Stripe's customer portal |
| `POST` | `/billing/checkout` `{"plan": "team"}` | administrators: a checkout link, or the plan changed in place |
| `GET` | `/operator/billing/provider/accounts` | operators: every organization's customer, subscription and dunning state |
| `GET` | `/operator/billing/provider/reports?period=YYYY-MM` | operators: the usage reported and its reconciliation |
| `GET` | `/operator/billing/provider/stats` | operators: this node's webhook and reporting counters |
| `POST` | `/operator/billing/provider/accounts/{id}/restore` | operators: out of read-only without a payment |
| `POST` | `/operator/billing/provider/accounts/{id}/confirm-suspension` | operators: suspend an organization whose suspension is proposed |
