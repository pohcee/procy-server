---
title: Procy
draft: false
---

![Procy: the clever raccoon sneaking a secure tunnel under a firewall to a server](/docs/images/procy-hero.svg)

Procy gives you remote access to and management of your servers, routers and workstations, without VPNs, jump hosts
or port forwards. A small client (`rport`) on each device dials out to the Procy server (`rportd`) and keeps the
connection open. Through it you can see your fleet, run commands and scripts, schedule jobs, open SSH, remote desktop
and web tunnels from the browser, and keep shared secrets in a vault.

![How Procy works: admins and users reach the Procy server over HTTPS; clients on private networks dial out to it through their firewall, and tunnels ride that connection](/docs/images/procy-principle.svg)

Every connection starts on the device's side. Nothing on the device's network has to accept incoming connections.

## Choose your path

**Hosted Procy.** We run the server for you at [procy.sh](https://procy.sh). Sign up with Google, get a free trial,
invite your team and add devices in a few minutes.
{{< button relref="/hosted/getting-started" >}}Get started on procy.sh{{< /button >}}

**Self-hosted.** Run the same open source server on your own machine, with SQLite or Postgres, and as several nodes if
you need them.
{{< button relref="/self-hosting/quick-start" >}}Self-hosting quick start{{< /button >}}

Both run the same software. Most pages apply to both. Pages and sections that only apply to one of them say so at the
top.

## What's in these docs

| Section | What it covers |
| --- | --- |
| [Hosted Procy](/docs/hosted/) | Signing up, organizations, members, permissions, billing and the firewall rules for procy.sh. |
| [Devices](/docs/devices/) | Installing and pairing the client, client groups, tags and labels, update status and monitoring. |
| [Tunnels and remote access](/docs/remote-access/) | SSH, RDP, VNC and web tunnels, on demand or permanent, and the browser proxies. |
| [Commands and automation](/docs/automation/) | Commands, scripts and the script library, schedules, sending files and the vault. |
| [API and CLI](/docs/api-and-cli/) | API tokens and authentication, the REST API and the `procy` command line. |
| [Self-hosting](/docs/self-hosting/) | Installing, securing and scaling your own server: HTTPS, Postgres, sign-in with Google, 2FA. |

The REST API is described in the [API reference](/docs/api/).

{{< hint type=note >}}
The programs, packages and config files still use the `rport` names: `rportd` (server), `rport` (client),
`/etc/rport/rportd.conf` and `/etc/rport/rport.conf`.
{{< /hint >}}

## Help and feedback

- **Bugs and feature requests:** [open an issue](https://github.com/pohcee/procy-server/issues) on GitHub.
- **Questions:** ask in the [discussions](https://github.com/pohcee/procy-server/discussions).
- **Hosted Procy support:** [support@procy.sh](mailto:support@procy.sh).
- **Security reports:** [security@procy.sh](mailto:security@procy.sh). Please don't open a public issue.

## Why Procy?

Every project needs a mascot, and ours is a raccoon: good with its hands, busy at night, and very good at getting into
places it was never meant to reach. That is the job of a remote tunnel.

The first name on the table was **Cannie**. It sounds like *canny*, which is exactly what a raccoon is, and it also
sounds like *can*, the garbage can every raccoon knows how to open. Unfortunately, half the internet had the same idea.

So we went to Latin. The raccoon's genus is *Procyon*, and *callidus* means clever or skilled: ***Procyon callidus***,
the clever raccoon. As a bonus, Procyon is also the brightest star in *Canis Minor*, so a little of Cannie lives on in
the night sky.

*Procyon callidus* is a mouthful, so it became **Procy** (say "PRO-see"). It sounds a lot like *proxy*, which for a
tunnel and remote access tool we decided is a feature, not a bug.

## License and credits

Procy is open source under the [GNU Affero General Public License v3.0](https://github.com/pohcee/procy-server/blob/main/LICENSE)
(AGPL-3.0-only). If you run a modified version as a network service, you must offer its source code to that
service's users.

Procy continues the open source rport and OpenRPort projects. Their code was released under the MIT License and is
included under its original terms. The notice is in
[LICENSES/MIT-rport-openrport.txt](https://github.com/pohcee/procy-server/blob/main/LICENSES/MIT-rport-openrport.txt),
and the [NOTICE](https://github.com/pohcee/procy-server/blob/main/NOTICE) file has the details. Thanks to
[jpillora/chisel](https://github.com/jpillora/chisel) for the groundwork of the tunnels.
