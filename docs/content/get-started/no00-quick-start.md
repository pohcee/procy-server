---
title: 'Quick start'
weight: 0
slug: quick-start
---

{{< toc >}}

This page walks you through a manual installation of the Procy server (`rportd`) and a first client (`rport`). It
also shows what happens behind the scenes, so it's a good read even if you later install clients with
[pairing](/docs/get-started/client-pairing/) or run everything from the web UI.

![Quick start: install the server, start it and copy the fingerprint, connect a client, then SSH through the tunnel](https://raw.githubusercontent.com/pohcee/openrport/main/docs/static/images/procy-quick-start.svg)

## Get the binaries

Download the server and client from the [latest release](https://github.com/pohcee/openrport/releases/latest). Each
release has `tar.gz` archives for Linux, macOS and other platforms, a `zip` for Windows, and DEB and RPM packages.

To build from source (Linux or macOS), run `make build`. The `rport` and `rportd` binaries appear in the current
directory. `make docker-goreleaser` builds the binaries for all supported platforms into `./dist`.

## Try it without installing

To see Procy work, run the server from any unprivileged account:

```shell
VERSION=$(curl -fsSL https://api.github.com/repos/pohcee/openrport/releases/latest | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
curl -LO "https://github.com/pohcee/openrport/releases/download/${VERSION}/rportd_${VERSION}_linux_x86_64.tar.gz"
tar vxzf rportd_${VERSION}_linux_x86_64.tar.gz rportd
KEY=$(openssl rand -hex 18)
CLIENT_PASSWORD=$(openssl rand -hex 12)
./rportd --log-level info --data-dir /var/tmp/ --key $KEY --auth client1:$CLIENT_PASSWORD
```

`rportd` listens for client connections on port 8080. It writes its fingerprint to `/var/tmp/rportd-fingerprint.txt`;
clients use it to check they are talking to your server.

Then, on another machine, connect a client and map port 2222 of the server to port 22 of the client:

```shell
./rport --auth client1:<CLIENT_PASSWORD> --fingerprint <FINGERPRINT> <SERVER_IP>:8080 2222:22
```

Now `ssh -p 2222 <SERVER_IP>` lands on the client. Leave out the server port (`./rport ... 22`) and the server picks
a free one for you. See `./rportd --help` and `./rport --help` for all options.

## Install the server

Use a machine on the public internet, ideally with a DNS name. Below it's called `procy.example.com`.

{{< hint type=warning >}}
**Don't run the server as root.** It doesn't need root rights, and `rportd` refuses to start as root unless you set
`allow_root`. Use an unprivileged user, usually `rport`.
{{< /hint >}}

```bash
VERSION=$(curl -fsSL https://api.github.com/repos/pohcee/openrport/releases/latest | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
curl -LO "https://github.com/pohcee/openrport/releases/download/${VERSION}/rportd_${VERSION}_linux_x86_64.tar.gz"
sudo tar vxzf rportd_${VERSION}_linux_x86_64.tar.gz -C /usr/local/bin/ rportd
sudo useradd -d /var/lib/rport -m -U -r -s /bin/false rport
sudo mkdir /etc/rport/
sudo mkdir /var/log/rport/
sudo chown rport /var/log/rport/
sudo tar vxzf rportd_${VERSION}_linux_x86_64.tar.gz -C /etc/rport/ rportd.example.conf
sudo cp /etc/rport/rportd.example.conf /etc/rport/rportd.conf
```

### Set your own secrets

`rportd` refuses to start with the placeholders and sample values from `rportd.example.conf` (`<YOUR_SEED>`,
`admin:foobaz`, `clientAuth1:1234`, `<YOUR_SECRET>`). Open `/etc/rport/rportd.conf` and set:

* `key_seed` in `[server]`: a random string the server's key pair is derived from. Clients pin the resulting
  fingerprint, so keep it and never change it. Generate one with `openssl rand -hex 18`.
* Client credentials in `[server]`, for example `auth = "client1:<a strong password>"`. See
  [client authentication](/docs/get-started/client-authentication/) for per-client credentials.
* API credentials in `[api]`, for example `auth = "admin:<a strong password>"`. The example config enables the API
  on port 3000, and `rportd` won't start it without authentication. See
  [API authentication](/docs/get-started/api-authentication/) for multi-user setups.

To set the `key_seed` from the command line:

```shell
KEY_SEED=$(openssl rand -hex 18)
sudo sed -i "s/key_seed = .*/key_seed = \"${KEY_SEED}\"/g" /etc/rport/rportd.conf
```

You don't need to set `jwt_secret`. If it's empty, `rportd` generates one on the first start and saves it as
`rportd-jwt-secret.txt` in the data directory, so logins survive restarts.

### Check that it starts

Switch to the `rport` user and start the server in the foreground:

```shell
sudo -u rport -s /bin/bash
rportd -c /etc/rport/rportd.conf --log-level info
```

Watch the log with `tail -f /var/log/rport/rportd.log`. Copy the fingerprint from
`/var/lib/rport/rportd-fingerprint.txt`; you need it for your clients.

### Run it with systemd

If all works, stop `rportd` and register it with systemd:

```shell
sudo rportd --service install --service-user rport --config /etc/rport/rportd.conf
sudo systemctl start rportd
sudo systemctl enable rportd # start on boot
```

This creates `/etc/systemd/system/rportd.service`.

{{< hint type=note title="Is the connection encrypted?" >}}
Yes, always. Clients talk to the server over SSH inside HTTP, using the server's ECDSA key pair. The fingerprint
makes sure a client only connects to your server: without it, a man in the middle could pose as your server and
take over your tunnels. You can put a TLS reverse proxy in front to get SSH over HTTPS.
{{< /hint >}}

## Connect a client

### Try it on the command line

On the client, here `client1.example.com`, install the binary:

```shell
VERSION=$(curl -fsSL https://api.github.com/repos/pohcee/openrport/releases/latest | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
curl -LO "https://github.com/pohcee/openrport/releases/download/${VERSION}/rport_${VERSION}_linux_x86_64.tar.gz"
sudo tar vxzf rport_${VERSION}_linux_x86_64.tar.gz -C /usr/local/bin/ rport
```

Create a tunnel that forwards port 2222 of `procy.example.com` to port 22 of the client:

```shell
rport --auth client1:<CLIENT_PASSWORD> --fingerprint <FINGERPRINT> --data-dir=/tmp procy.example.com:8080 2222:0.0.0.0:22
```

The server log confirms the new tunnel. Now `ssh -p 2222 procy.example.com` takes you to the client, even though it
sits behind a firewall.

### Run a Linux client with systemd

For a permanent installation:

```shell
VERSION=$(curl -fsSL https://api.github.com/repos/pohcee/openrport/releases/latest | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
curl -LO "https://github.com/pohcee/openrport/releases/download/${VERSION}/rport_${VERSION}_linux_x86_64.tar.gz"
sudo tar vxzf rport_${VERSION}_linux_x86_64.tar.gz -C /usr/local/bin/ rport
sudo useradd -d /var/lib/rport -U -m -r -s /bin/false rport
sudo mkdir /etc/rport/
sudo mkdir /var/log/rport/
sudo chown rport /var/log/rport/
sudo tar vxzf rport_${VERSION}_linux_x86_64.tar.gz -C /etc/rport/ rport.example.conf
sudo cp /etc/rport/rport.example.conf /etc/rport/rport.conf
sudo rport --service install --service-user rport --config /etc/rport/rport.conf
```

Edit `/etc/rport/rport.conf`. A minimal configuration looks like this:

```text
[client]
server = "procy.example.com:8080"
fingerprint = "<FINGERPRINT>"
auth = "client1:<CLIENT_PASSWORD>"
remotes = ['2222:22']
```

`remotes` sets up a permanent tunnel: port 22 (SSH) of the client becomes available on port 2222 of the server. Leave
it out to connect without a tunnel and open tunnels [on demand](/docs/get-started/on-demand-tunnels/).

Start the client and, optionally, enable it on boot:

```shell
sudo systemctl start rport
sudo systemctl enable rport
```

### Run a Windows client

Download `rport_<version>_windows_x86_64.zip` from the [latest release](https://github.com/pohcee/openrport/releases/latest)
and extract it to `C:\Program Files\rport`. Rename `rport.example.conf` to `rport.conf` and edit it. A minimal
configuration:

```text
[client]
server = "procy.example.com:8080"
fingerprint = "<FINGERPRINT>"
auth = "client1:<CLIENT_PASSWORD>"
remotes = ['3300:3389']
```

This makes port 3389 (Remote Desktop) of the client available on port 3300 of the server.

Test the connection from a command prompt with administrative rights:

```shell
cd "C:\Program Files\rport"
rport.exe -c rport.conf
```

If there are no errors, try a Remote Desktop connection to `procy.example.com:3300`. Then stop the client with
CTRL-C, and register and start it as a service:

```shell
rport.exe --service install -c rport.conf
sc query rport
sc start rport
```

The service starts automatically on boot. To change that, run `sc config rport start=disabled`.

### Other operating systems

See [install and run a Procy client](/docs/get-started/install-and-run-a-client/) for macOS. To install clients
with a one-liner generated by the server, use [pairing](/docs/get-started/client-pairing/).

## Configuration files

Both programs read a config file given with `--config` (or `-c`):

```shell
rportd -c /etc/rport/rportd.conf
rport -c /etc/rport/rport.conf
```

The commented examples `rportd.example.conf` ([view online](https://github.com/pohcee/openrport/blob/main/rportd.example.conf))
and `rport.example.conf` ([view online](https://github.com/pohcee/openrport/blob/main/rport.example.conf)) are in the
release archives. Command line options and environment variables override values from the config file.

## Next steps

* Install the [web UI](/docs/get-started/web-ui/) and [enable HTTPS](/docs/get-started/securing-rportd-with-https/).
* Let clients wait without a tunnel and open [tunnels on demand](/docs/get-started/on-demand-tunnels/).
* Read how to [secure the server](/docs/advanced/securing-the-server/).
