---
title: "Web UI"
weight: 20
slug: web-ui
aliases:
- /get-started/web-ui/
- /docs/no07-frontend.html
- /docs/content/get-started/no07-frontend.md
- /get-started/rport-frontend/
---
{{< toc >}}

Install the Procy web UI on your own server. This page is for self-hosting only: on [procy.sh](https://procy.sh) the
UI is already there.

The UI is a bundle of static HTML, JavaScript and assets, released separately from the server. `rportd` serves it from
its `doc_root` on the API address, so the UI and the API share one URL and need no other web server. Like the server,
it is open source under the AGPL-3.0: the code is in the [procy-ui repository](https://github.com/pohcee/procy-ui).

## Install it

1. In the `[api]` section of `rportd.conf`, listen on an address your browser can reach and set the doc root:

   ```text
   [api]
     address = "0.0.0.0:3000"
     doc_root = "/var/lib/rport/docroot"
   ```

2. Download the latest `procy-ui-<version>.zip` from the
   [UI releases](https://github.com/pohcee/procy-ui/releases/latest) and unpack it into the doc root:

   ```shell
   sudo mkdir -p /var/lib/rport/docroot
   cd /var/lib/rport/docroot
   UI_TAG=$(curl -fsSL https://api.github.com/repos/pohcee/procy-ui/releases/latest \
     | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
   sudo curl -fsSL -o procy-ui.zip \
     "https://github.com/pohcee/procy-ui/releases/download/${UI_TAG}/procy-ui-${UI_TAG#v}.zip"
   sudo unzip -qq procy-ui.zip && sudo rm -f procy-ui.zip
   cd ~
   sudo chown -R rport:rport /var/lib/rport/docroot
   ```

3. Restart `rportd`, open the API URL in a browser and log in with an
   [API user](/docs/api-and-cli/api-authentication/).

{{< hint type=caution >}}
The server usually faces the internet. Never serve the API and the UI over plain HTTP:
[turn on HTTPS](/docs/self-hosting/securing-rportd-with-https/) first.
{{< /hint >}}

## Update it

Unpack a newer release over the doc root the same way; no restart is needed. Remove the old files first if you want a
clean copy. Keep the UI and the server on matching releases: a new UI may call API routes an older server doesn't have.

## What shows up

The UI asks the server what it supports (`GET /api/v1/status`) and shows only those pages. A standalone server on
SQLite shows devices, tunnels, commands, scripts, schedules, the library, the vault, users and the audit log. A server
on [Postgres](/docs/self-hosting/postgres/) can also show organizations, members and invitations, usage, billing and
the operator console, depending on what you turn on. With [Sign in with Google](/docs/self-hosting/oidc/) the login
page shows a Google button next to the password form.
