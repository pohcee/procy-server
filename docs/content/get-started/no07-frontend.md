---
title: "Web UI"
weight: 07
slug: web-ui
aliases:
  - /docs/no07-frontend.html
  - /docs/content/get-started/no07-frontend.md
  - /get-started/rport-frontend/
---
{{< toc >}}
Procy comes with a web-based user interface, distributed as a separate bundle. Like the server and client, it is open
source: the code is in the [procy-ui repository](https://github.com/pohcee/procy-ui) and released under the
GNU Affero General Public License v3.

## Installing the web UI

The web UI comes as a bundle of static HTML, JavaScript and assets. It does not require any server-side scripting
support; `rportd` serves it from its doc root.

The web UI is served on the API address, so the API must listen on an address your browser can reach, for example
`0.0.0.0:3000`. Make sure you have the below options enabled in `[api]` section of the `rportd.conf`.

```text
[api]
  address = "0.0.0.0:3000"  
  doc_root = "/var/lib/rport/docroot"
```

{{< hint type=warning >}}
Usually you run rportd and the web UI on a public server directly exposed to the internet. Running the API and
serving the web UI on unencrypted HTTP is dangerous. Always use HTTPS. The built-in web server supports HTTPS.
To quickly generate certificates, follow [this guide](/docs/get-started/securing-rportd-with-https/).
{{< /hint >}}

* Create the doc root folder. Usually `/var/lib/rport/docroot` is used.
* Download the latest release of the web UI from [GitHub](https://github.com/pohcee/procy-ui/releases/latest)
  (`openrport-frontend-<version>.zip`).
* Unpack to the doc root folder.

```bash
sudo mkdir /var/lib/rport/docroot
cd /var/lib/rport/docroot
UI_TAG=$(curl -fsSL https://api.github.com/repos/pohcee/procy-ui/releases/latest | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
sudo curl -fsSL -o procy-ui.zip \
  "https://github.com/pohcee/procy-ui/releases/download/${UI_TAG}/openrport-frontend-${UI_TAG#v}.zip"
sudo unzip -qq procy-ui.zip && sudo rm -f procy-ui.zip
cd ~
sudo chown -R rport:rport /var/lib/rport/docroot
```

Now open the API-URL in a browser. Log in with a username and password specified for the
[API authentication](/docs/get-started/api-authentication/).

You are done.
