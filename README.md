<!-- markdownlint-disable -->
![Procy: the clever raccoon sneaking a secure tunnel under a firewall to a server](https://raw.githubusercontent.com/pohcee/openrport/main/docs/static/images/procy-hero.svg)

> Procy continues the open-source rport project, originally built by CloudRadar and carried on by the community as
> [OpenRPort](https://github.com/openrport/openrport) after rport moved to a commercial licence. The programs, packages and config files still
> use the `rport` names (`rportd`, `rport`, `/etc/rport/rportd.conf`); they will be renamed in a later release.

## At a glance

<!-- markdownlint-restore -->

Procy helps you manage remote servers without the hassle of VPNs, chained SSH connections, jump hosts, or
commercial tools like TeamViewer and its clones.

Procy consists of a server (`rportd`) and a client (`rport`) that establish permanent or on-demand secure tunnels to
devices inside protected intranets behind a firewall.

All operating systems provide secure and well-established mechanisms for remote management, SSH and Remote Desktop
being the most widely used. Procy makes them accessible easily and securely.

**Is Procy a replacement for TeamViewer?**
Yes and no. It depends on your needs.
TeamViewer and a couple of similar products are focused on giving access to a remote graphical desktop bypassing the
Remote Desktop implementation of Microsoft. They fall short in a heterogeneous environment where access to headless
Linux machines is needed. But they are without alternatives for Windows Home Editions.
Apart from remote management, they offer supplementary services like video conferences, desktop sharing, screen
mirroring, or spontaneous remote assistance for desktop users.

**Goal of Procy**
Procy focuses only on remote management of those operating systems where an existing login mechanism can be used.
It can be used for Linux and Windows, but also appliances and IoT devices providing a web-based configuration.
From a technological perspective, [Ngrok](https://ngrok.com/) and [openport.io](https://openport.io) are similar
products. Procy differs from them in many aspects.

* Procy is 100% open source. Client and Server. Remote management is a matter of trust and security. Procy is fully transparent.
* Procy comes with a [web UI](https://github.com/pohcee/openrport-ui) that makes managing remote systems easy.
* Procy is made for all operating systems with native and small binaries. No need for Python or similar heavyweights.
* Procy lets you self-host the server.
* Procy lets clients wait in standby mode without an active tunnel. Tunnels can be requested on demand by the user remotely.

**Supported operating systems**
For the client almost all operating systems are supported, and we provide binaries for a variety of Linux architectures
and Microsoft Windows.
Also, the server can run on any operating system supported by the golang compiler. At the moment we provide server
binaries only for Linux X64 because this is the ideal platform for running it securely and cost-effective.

![How Procy works: admins and users reach the Procy server over HTTPS; clients on private networks dial out to it through their firewall, and tunnels ride that connection](https://raw.githubusercontent.com/pohcee/openrport/main/docs/static/images/procy-principle.svg)

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

## Install

Download the server and client packages from the [latest release](https://github.com/pohcee/openrport/releases/latest),
then follow the [quick start](https://procy.sh/docs/get-started/quick-start/).

### Client pairing

Pairing (the "Install client" one-liners in the web UI) is built into `rportd`. Enable it in `/etc/rport/rportd.conf`:

```text
[pairing]
  enabled = true
  url = "https://procy.example.com"
  downloads_url = "https://procy.example.com/downloads"
```

and restart the server: `sudo systemctl restart rportd.service`. See
[pairing clients](https://procy.sh/docs/get-started/client-pairing/) for how to serve the client packages and all options.

## 📖 Documentation

The [documentation](https://procy.sh/docs/) covers installation, configuration, secure operation and the concepts behind
Procy. The REST API is described in the [API reference](https://procy.sh/docs/api/).

## Feedback and Help

**We need your feedback**.
Our vision is to establish Procy as a serious alternative to all the black box software for remote management.
To make it a success, please share your feedback.

### Report bugs

If you encounter errors while installing or using Procy, please let us know.
[File an issue report](https://github.com/pohcee/openrport/issues) here on GitHub.

### Ask question

If you have difficulties installing or using Procy, don't hesitate to ask us anything. Often questions give us a hint
on how to improve the documentation. Do not use issue reports for asking questions.
Use the [discussion forum](https://github.com/pohcee/openrport/discussions) instead.

### Positive Feedback

Please share positive feedback also. Give us a star. Write a review. Share our project page on your social media.
Contribute to the [discussion](https://github.com/pohcee/openrport/discussions). Is Procy suitable for your
needs? What is missing?

### Stay tuned

Click on the Watch button in the top right corner of the [Repository Page](https://github.com/pohcee/openrport).
This way you won't miss any updates and the upcoming features.

## Credits

* Thanks to [jpillora/chisel](https://github.com/jpillora/chisel) for the great groundwork of tunnels

## Versioning model

Procy uses `<major>.<minor>.<buildnumber>` version pattern for compatibility with a maximum number of package managers.

## License

Procy is licensed under the [GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0-only). If you run a
modified version as a network service, you must offer its source code to that service's users.

Procy builds on rport and OpenRPort, which were released under the MIT License; that code is included under its
original terms, and the notice is kept in [LICENSES/MIT-rport-openrport.txt](LICENSES/MIT-rport-openrport.txt).
See [NOTICE](NOTICE).
