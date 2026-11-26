---
title: "Billing and usage"
weight: 50
slug: billing-and-usage
---
{{< toc >}}

This page explains hosted Procy's plans, the free trial, how you pay, and what the Usage page shows. It's for
organization administrators on procy.sh. Only members of **Administrators** see the **Billing** and **Usage** pages.
Self-hosted servers have no billing unless their operator sets it up, see
[Payments with Stripe](/docs/self-hosting/payment-provider/).

{{< hint type=note title="Preview" >}}
Hosted Procy is in preview. Plans and prices may change.
{{< /hint >}}

## Plans

The current plans and prices are on the [procy.sh](https://procy.sh) home page. A plan sets:

- a **monthly price**, which includes a number of devices, plus a price for each device above that;
- the most devices you can register;
- a **transfer allowance**: how much data tunnels and file uploads may move each month;
- what happens past the allowance: on some plans you pay for the extra transfer (**overage**), on others transfer is
  capped.

The trial includes every feature: devices, commands, scripts, tunnels, schedules, the vault and team members.

### What counts as a device

A device counts for a day (UTC) if it was connected at any moment that day. Devices that are registered but never
connect don't count. Your **billable devices** for the month are the average of the daily counts, rounded up, so a
device that is on for only part of the month costs only part of the month.

### What counts as transfer

Transfer is what your tunnels carry in both directions, plus the files you upload to devices. Commands, scripts,
schedules, monitoring and the web interface itself don't count toward it.

## The free trial

Every new organization starts with a free trial. You don't need a payment method to start. The billing page shows how
many days are left, and a banner reminds administrators 14 days and 3 days before it ends.

When the trial ends:

- with a payment method on file, the organization continues on its plan and is billed monthly;
- without one, it moves to the free plan, with its lower limits. Nothing is deleted. Devices beyond the free plan's
  limit can't connect until you choose a plan.

## Pay for Procy

Payments go through [Stripe](https://stripe.com). Procy never sees your card details.

Open **Billing** in the navigation. From there you can:

- **Add payment method**: opens Stripe's checkout. Once it completes, your subscription starts.
- **Change plan**: enter the plan's ID from the price list, for example `team`. The plan and its limits change at
  once.
- **Manage payment**: opens Stripe's customer portal. Update your card, see and download invoices, or cancel.

You're billed monthly. The base price is charged in advance. Extra devices and overage for a month are invoiced at
the start of the next month. Each month is closed two hours after it ends, and its quantities are frozen: usage that
arrives later shows on the Usage page but is never billed.

If you cancel, the organization moves to the free plan at the end of the billing period. Nothing is deleted.

### Spending limit

On a plan that bills overage, you can set a **Spending limit** on the Billing page: the most overage you want to pay
in a month. Reaching it works like a transfer cap (below). Leave it empty for no limit.

## When you reach the transfer allowance

| Where you are | What happens |
| --- | --- |
| 80% of the allowance | Administrators see a banner. Nothing is limited. |
| 100%, on a plan with overage | Administrators see a banner. Overage starts to count. Nothing is limited. |
| The cap (capped plan, or spending limit reached) | New tunnels and uploads are refused until the next month. Open tunnels stay up but are slowed down. |

Whatever happens, **devices stay connected**. Commands, scripts, schedules and monitoring keep working, and open
tunnels are never cut. The limits reset at the start of each month (UTC). To go on right away, raise your spending
limit or change to a bigger plan.

## If a payment fails

Stripe retries the payment. Procy emails the administrators and shows them a banner.

1. **The first 14 days:** everything keeps working. Use **Manage payment** to pay.
2. **The next 14 days:** the organization is **read-only**. You can sign in and see everything, but changes, new
   jobs, tunnels and uploads are refused, and schedules pause. Devices stay connected, and open tunnels stay up.
3. **After that:** the organization may be suspended. Nobody can sign in, and devices can't connect.

Paying the overdue invoice restores everything at once.

## The Usage page

**Usage** in the navigation shows what your organization used in a month. Pick the month at the top. It's updated
every few minutes.

- **Transfer**: tunnel and upload transfer, against your allowance.
- **Billable devices**: the billable count so far, with the peak day and the most devices connected at once.
- **Jobs run** and **Uploads to devices**.
- **Daily transfer**: transfer per UTC day, split into data to you, data to devices, and uploads.
- **Top devices by transfer**: the devices that moved the most this month. Use it to find what used up the
  allowance.
- **Also metered**: device-days, connected device-hours and monitoring measurements. These aren't billed.

Transfer is counted in payload bytes. The overhead of encryption and protocols isn't counted.
