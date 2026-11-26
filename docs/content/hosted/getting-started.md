---
title: "Getting started on procy.sh"
weight: 10
slug: getting-started
---
{{< toc >}}

This page takes you from nothing to a first connected device on hosted Procy at [procy.sh](https://procy.sh): sign up,
wait for your organization to be reviewed, add a device, and invite your team. If you run your own server, start with
the [self-hosting quick start](/docs/self-hosting/quick-start/) instead.

{{< hint type=note title="Preview" >}}
Hosted Procy is in preview. Features and plans may change.
{{< /hint >}}

## 1. Sign up

1. Open [procy.sh](https://procy.sh) and choose **Start free trial**.
2. Enter your **Organization name**. This is usually your company or team.
3. Choose **Continue with Google**. Google confirms your email address, so no confirmation link is sent. This works
   with Gmail and Google Workspace addresses.

   If you can't use a Google account, enter your **Work email** instead and choose **Create organization**. We email
   you a link to confirm the address and set a password. The link works once and expires after a day. If nothing
   arrives, check your spam folder.

You become the organization's first administrator. Every organization starts with a free trial, and no payment
method is needed to start. See [Billing and usage](/docs/hosted/billing-and-usage/) for what happens when the trial
ends.

{{< hint type=tip >}}
Sign in with Google if you can. You don't have to manage a password, and your administrator can later
[require Google sign-in](/docs/hosted/organizations/#require-google-sign-in) for the whole organization.
{{< /hint >}}

## 2. Wait for the review

A person reviews each new organization before it opens. This keeps abuse off the service. We email you when your
organization is ready. Until then you can't sign in to it.

When the email arrives, go to [procy.sh](https://procy.sh), choose **Sign in**, and sign in the same way you signed up.

## 3. Add your first device

Before you start, check that the device can reach Procy. Everything is outbound, but strict firewalls and proxies
may need the rules in [Network requirements](/docs/hosted/network-requirements/).

1. In the web interface, open **Devices** and choose **Add device**.
2. Keep the generated ID and password, or enter an ID that means something to you. Choose **Add Client Access**.
3. The **Install client** dialog opens. Keep the connect URL at `https://connect.procy.sh` and choose the installer
   options you want. See [Installer options](/docs/devices/install-and-run-a-client/#installer-options).
4. Copy the Linux or Windows command and run it on the device: on Linux as root, and on Windows in PowerShell as
   Administrator.

After a few seconds the device shows up on the **Devices** page. Select it to see its details, run a command, or
open a tunnel.

The install command contains a one-time code that expires after an hour. If it expires, open **Settings → Client access**,
find the device's ID and choose **Install client** in its actions menu for a new command. For macOS and other systems the installer
doesn't cover, see [Install and run a Procy client](/docs/devices/install-and-run-a-client/).

## 4. Invite your team

1. Open **Settings → Users** and choose **Invite member**.
2. Enter the person's email address and choose their [user groups](/docs/hosted/permissions-model/). Members of
   **Administrators** can do everything. Give other people a group that grants only what they need.
3. Tell them to sign in at [procy.sh](https://procy.sh).

Someone who already has a Procy account gets an invitation to accept after they sign in. A new address gets an account
right away. See [Organizations and members](/docs/hosted/organizations/) for the details.

## Next steps

- Group your devices and control who can reach them: [Client groups](/docs/devices/client-groups/) and the
  [permissions model](/docs/hosted/permissions-model/).
- Reach SSH, remote desktop or a web page on a device: [Managing tunnels](/docs/remote-access/managing-tunnels/).
- Run a command on many devices at once: [Command execution](/docs/automation/command-execution/).
- Automate Procy from scripts: [API authentication](/docs/api-and-cli/api-authentication/).
