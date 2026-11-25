---
title: "Payments with Stripe (preview)"
weight: 32
slug: payment-provider
---
{{< toc >}}

{{< hint type=caution title="Preview" >}}
**Preview.** Only for servers on Postgres with `[metering]` and `[billing]` enabled (see
[Running the server on Postgres](../postgres/)). A standalone server, and a cluster without `[billing] provider =
"stripe"`, has none of this: no routes, no jobs, no tables read.
{{< /hint >}}

With `[billing] provider = "stripe"`, organizations on a paid plan pay through Stripe. The server's own usage tables
stay the source of truth: when a month closes (two hours after it ends), its quantities are frozen in
`billing_periods`, and the server reports them to Stripe, which turns them into an invoice, collects the money and
computes sales tax. Exempt organizations are metered like any other but never sent to Stripe.

## Configuration

```toml
[billing]
  enabled = true
  provider = "stripe"
  plans_file = "/etc/rportd/plans.toml"
  stripe_secret_key_file = "/run/secrets/stripe-key"        # secret or restricted key
  stripe_webhook_secret_file = "/run/secrets/stripe-webhook" # the endpoint's signing secret, whsec_…
  return_url = "https://procy.example.com/billing"           # checkout and the portal return here
  # stripe_automatic_tax = true                              # Stripe Tax in checkout (default on)
  # grace_days = 14                                          # full service after a failed payment
  # read_only_days = 14                                      # then read-only, then suspension proposed
```

Secrets are read only from files (a secret manager's mount in a hosted deployment), never from the configuration
file, the database or the UI, and never logged. After upgrading, run `rportd migrate --grant-to <application role>`
once more (migration 1500).

## What to set up in Stripe

rportd creates customers, checkout sessions, portal sessions and meter events. It never creates products, prices or
meters; those are yours:

1. **Meters**, one per metric, aggregation *sum*, default payload keys (`stripe_customer_id`, `value`): extra devices
   (billable devices above the plan's included ones) and overage GiB. All plans can share them.
2. **Prices** per plan: a licensed monthly base price, and metered prices for extra devices and overage backed by the
   meters. Put their ids in the plans file: `provider_prices = { base = "price_…", devices = "price_…", overage =
   "price_…" }`.
3. **A webhook endpoint** at `https://<server>/api/v1/billing/webhook` with the events
   `checkout.session.completed`, `customer.subscription.created`, `customer.subscription.updated`,
   `customer.subscription.deleted`, `customer.subscription.trial_will_end`, `invoice.paid` and
   `invoice.payment_failed`. Pin it to an API version of the same release train as the server's stripe-go
   (`2026-10-28.endive` and later `.endive` versions): events of another train are refused (400) and retried by
   Stripe.
4. **Invoice finalization grace period** (Billing → Invoice settings): usage counts on a subscription's monthly
   invoice only if it arrives before the invoice finalizes, by default one hour after the month ends. The server
   closes the month two hours after it ends and reports within the next 15 minutes, so set a rule for subscription
   cycle invoices with metered prices to **at least 6 hours** (up to 72). Without it, usage is recorded in Stripe but
   not invoiced.
5. **Stripe Tax** registrations for the US and Canada, and the **customer portal** (payment methods, invoices, tax
   ids, cancellation; plan changes go through the server so that limits change with the price).

## How it works

* **Customers and checkout.** An organization's administrator chooses a plan (`POST /api/v1/billing/checkout`); the
  server creates the Stripe customer (metadata `procy_org_id`; never for exempt organizations) and a Checkout
  Session in subscription mode, its billing cycle anchored to the 1st of next month (UTC) with the partial first
  month prorated, Stripe Tax on, the billing address and tax ids collected. With a live subscription, a new plan
  replaces the subscription's prices at once instead.
* **Usage in arrears.** For every closed period ready to be charged, one meter event per organization and metric —
  extra devices and overage GiB — with the identifier `procy:<organization>:<metric>:<YYYY-MM-01>`, timestamped in
  the period's last second. Quantities of 0 are not sent. Held periods (overage above `overage_review_above`) wait
  for an operator. When every quantity is sent the period becomes `reported`.
* **Reconciliation.** Daily, each sent report is compared with Stripe's meter event summary for its period:
  equal is `confirmed`; nothing at all after 6 hours is sent again with the same identifier; anything else is a
  `mismatch` for operators, never corrected automatically. Stripe deduplicates identifiers for only about 24 hours,
  so a failed send older than that is sent again only if Stripe's summary shows nothing.
* **Webhooks.** No session authentication: the `Stripe-Signature` header is verified (HMAC-SHA256 over the
  timestamp and the raw body, 5 minutes' tolerance) before anything is parsed, and a request that fails gets 400 and
  is counted. Each event id is claimed in `billing_events` in the same transaction that applies it, so a replay
  changes nothing and a failure is retried by Stripe. The organization is found through the server's own customer
  mapping; an organization named in the event's metadata must agree or the event is ignored. Older subscription
  events arriving after newer ones are ignored.
* **Dunning.** A failed payment starts a grace period with full service and an email to the organization's
  administrators. After `grace_days` the organization is **read-only**: users log in and see everything, but every
  change is refused with 403 `ERR_CODE_BILLING_READ_ONLY` (no new jobs, tunnels, uploads or configuration changes;
  scheduled runs pause). Devices stay connected and open tunnels stay up; the portal keeps working, since paying is
  how it ends. After `read_only_days` more, **suspension is proposed** to operators, who confirm it. A successful
  payment restores everything at once. A canceled subscription moves the organization to the free plan, never to
  suspension.

The background jobs run on one node at a time (cluster leases `billing-report` every 15 minutes,
`billing-reconcile` daily, `billing-dunning` hourly).

## API

| Method | Path | For |
|---|---|---|
| `POST` | `/billing/webhook` | Stripe (signature-verified, no session) |
| `GET` | `/billing/payment` | an organization's administrators: subscription, payment method, overdue state |
| `GET` | `/billing/invoices?limit` | an organization's administrators: the latest invoices, with links to Stripe's pages |
| `POST` | `/billing/portal` | an organization's administrators: a link to Stripe's customer portal |
| `POST` | `/billing/checkout` `{"plan": "starter"}` | an organization's administrators: a checkout link, or the plan changed in place |
| `GET` | `/operator/billing/provider/accounts` | operators: every organization's customer, subscription and dunning state (`suspension_proposed`) |
| `GET` | `/operator/billing/provider/reports?period=YYYY-MM` | operators: the usage reported and its reconciliation |
| `GET` | `/operator/billing/provider/stats` | operators: this node's webhook and reporting counters |
| `POST` | `/operator/billing/provider/accounts/{id}/restore` | operators: out of read-only without a payment |
| `POST` | `/operator/billing/provider/accounts/{id}/confirm-suspension` | operators: suspend an organization whose suspension is proposed |

Plans, statuses, the current period's usage against the allowance and the closed periods are in `GET /billing` and
the operator billing routes of the plans (see `[billing]` in the Postgres page). Every webhook-driven change is
written to the audit log with the actor `billing:stripe` and the event id; operator actions with the operator.

## Tables (migration 1500)

`billing_provider_accounts` (customer, subscription, dunning state per organization), `billing_events` (webhook
event ids), `billing_usage_reports` (one row per organization, metric and period), `billing_dunning_notices` (one
email per kind and overdue episode). The application role reads them under row-level security and changes them only
through the migration's functions.
