---
title: "Organizations and members"
weight: 20
slug: organizations
---
{{< toc >}}

On hosted Procy, everything you manage belongs to an organization: its devices, users, groups, tunnels, schedules,
scripts and vault. This page explains members, roles, invitations and how members sign in. It's for organization
administrators on procy.sh. A self-hosted server with [several tenants](/docs/self-hosting/multi-tenancy/) works the
same way.

## Organizations

An organization has a name and an ID. You choose the name when you sign up. The ID is generated from it, for example
`acme-k3x9`, and appears in the organization switcher. Organizations are fully separated: a member of one sees
nothing of another.

One account (one login, usually your email address) can belong to several organizations. If yours does, you pick the
organization when you sign in, and you can switch with the organization switcher at the top of the navigation.

## Members and roles

The members of an organization are listed under **Settings → Users**. What each member can do depends on their
**user groups**:

- **Administrators** can do everything in the organization, on every device. They also manage members, billing,
  usage and the organization settings.
- Any other group grants only the permissions you give it under **Settings → User groups**, on the devices you
  allow it to reach.

The [permissions model](/docs/hosted/permissions-model/) explains how groups, permissions and device access fit
together. A member's groups apply to this organization only. In another organization the same account can have
different groups.

To change a member's groups, choose **Edit** in the member's actions menu. To take someone out of the organization,
choose **Remove from organization**. They can no longer sign in to it, and their sessions and API tokens there stop
working. If their account belongs to other organizations, it stays a member there.

A member who also belongs to another organization owns their own password, two-factor settings and login name. You
can change only their groups here. The list marks these members with **Also in other organizations**.

## Invite a member

1. Open **Settings → Users** and choose **Invite member**.
2. Enter their **Email or login**. Use the email address they sign in with.
3. Pick their user groups.
4. Choose **Invite**.

What happens next depends on whether the address already has a Procy account:

- **New address.** An account is created and is a member right away. If the organization
  [requires Google sign-in](#require-google-sign-in), the account has no password: the person signs in with the
  Google account of that address. Otherwise the dialog shows a one-time password. Pass it on safely. It is shown
  only once, and the person must change it at their first sign-in. If it's lost, remove the member and invite them
  again.
- **Existing account.** The person gets an invitation. They become a member when they accept it. Until then it's
  listed under **Pending invitations** on the Users page, where you can withdraw it.

Procy doesn't email invitations. Tell the person to sign in at [procy.sh](https://procy.sh).

### Accept an invitation

If another organization invites you, an **Invitations** link appears in your user menu. On the Invitations page,
choose **Accept** or **Decline**. Accepting makes you a member with the groups of the invitation. Your password and
two-factor settings stay the same. You then switch to the new organization with the organization switcher.

To accept an invitation, your account's email address must be confirmed. If it isn't, the Invitations page and your
**Profile** show how to confirm it. A Google sign-in confirms it too.

## How members sign in

Members can sign in with a password, with Google, or both. A Google account is linked to a login the first time
someone signs in with Google using the login's email address. The Users list shows **Google linked** for those
members.

Each member chooses how they sign in under **Settings → Profile → How you sign in**: **Password and Google**,
**Password only** or **Google only**. "Google only" is available once a Google account is linked. Administrators can
also set a member's sign-in method when they edit the member.

If someone links the wrong Google account, choose **Unlink Google account** in the member's actions menu. Their next
Google sign-in links one again.

Procy doesn't ask for its own two-factor code after a Google sign-in. Google's own 2-Step Verification protects
those sign-ins. Google Workspace administrators can enforce it for their domain.

### Require Google sign-in

Administrators can make Google the only way into the organization. This is the recommended setting: nobody has a
password to lose or reuse.

1. Open **Settings → Organization**.
2. Under **Sign-in**, turn on **Require Google sign-in** and confirm.

From then on:

- Members sign in with the Google account of their email address. Passwords don't work in this organization.
- Password sessions end at once, and members with only a password can't sign in until they use Google.
- New members you invite get no password.
- [API tokens](/docs/api-and-cli/api-authentication/) keep working.

The setting belongs to the organization. If a member also belongs to another organization that allows passwords, they
can still use their password there.

## Export your organization

Under **Settings → Organization**, **Download export** gives you the organization's configuration as JSON: users and
their groups, user groups, devices, device groups, stored tunnels, schedules and the script library.

The export contains no secrets: no passwords, two-factor secrets, API tokens, device credentials or vault values.
Each export is recorded in the platform's audit log.

## Delete your organization

Under **Settings → Organization**, **Delete this organization** asks for the organization ID to confirm. Then:

- The organization is suspended at once. Nobody can sign in, and its devices stop connecting.
- Billing stops, and any subscription is canceled.
- Every administrator with an email address gets a link to cancel the deletion within 30 days.
- After 30 days everything is deleted for good, and the organization's vault key is destroyed. The ID is never given
  out again.

Download an export first if you want to keep the configuration. To cancel, use the link in the email before the
deletion date.
