---
title: 'Quick start'
weight: 10
slug: quick-start
aliases:
- /get-started/quick-start/
---

{{< toc >}}

Install the Procy server (`rportd`) on your own machine and connect a first client (`rport`) by hand. This page is
for self-hosting; on [procy.sh](https://procy.sh) the server runs for you and you start at
[installing a client](/docs/devices/install-and-run-a-client/).

Doing it once by hand shows what happens behind the scenes. Later you'll usually add clients with
[pairing](/docs/devices/client-pairing/) from the web UI.

![Quick start: install the server, start it and copy the fingerprint, connect a client, then SSH through the tunnel](/docs/images/procy-quick-start.svg)

## Get the binaries

Download from the [latest release](https://github.com/pohcee/procy-server/releases/latest):

* the server: `rportd_<version>_linux_x86_64.tar.gz`;
* the client: `tar.gz` archives for Linux, macOS and FreeBSD on many architectures, a `zip` and an `msi` for Windows,
  and DEB and RPM packages.

To build from source, run `make build`; the `rport` and `rportd` binaries appear in the current directory.

The commands below look up the latest version first:

```shell
VERSION=$(curl -fsSL https://api.github.com/repos/pohcee/procy-server/releases/latest \
  | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
```

## Try it without installing

To see Procy work, run the server from any unprivileged account:

```shell
curl -LO "https://github.com/pohcee/procy-server/releases/download/${VERSION}/rportd_${VERSION}_linux_x86_64.tar.gz"
tar vxzf rportd_${VERSION}_linux_x86_64.tar.gz rportd
KEY=$(openssl rand -hex 18)
CLIENT_PASSWORD=$(openssl rand -hex 12)
./rportd --log-level info --data-dir /var/tmp/ --key $KEY --auth client1:$CLIENT_PASSWORD
```

`rportd` listens for clients on port 8080 and writes its fingerprint to `/var/tmp/rportd-fingerprint.txt`. Clients
use the fingerprint to check they are talking to your server.

On another machine, connect a client and map port 2222 of the server to port 22 of the client:

```shell
./rport --auth client1:<CLIENT_PASSWORD> --fingerprint <FINGERPRINT> <SERVER_IP>:8080 2222:22
```

Now `ssh -p 2222 <SERVER_IP>` lands on the client. Leave out the server port (`./rport ... 22`) and the server picks
a free one. `./rportd --help` and `./rport --help` list all options.

## Install the server

Use a machine on the public internet, ideally with a DNS name. Below it's called `procy.example.com`.

{{< hint type=caution >}}
**Don't run the server as root.** It doesn't need root rights, and `rportd` refuses to start as root unless you set
`allow_root`. Use an unprivileged user, usually `rport`.
{{< /hint >}}

```shell
curl -LO "https://github.com/pohcee/procy-server/releases/download/${VERSION}/rportd_${VERSION}_linux_x86_64.tar.gz"
sudo tar vxzf rportd_${VERSION}_linux_x86_64.tar.gz -C /usr/local/bin/ rportd
sudo useradd -d /var/lib/rport -m -U -r -s /bin/false rport
sudo mkdir /etc/rport/ /var/log/rport/
sudo chown rport /var/log/rport/
sudo tar vxzf rportd_${VERSION}_linux_x86_64.tar.gz -C /etc/rport/ rportd.example.conf
sudo cp /etc/rport/rportd.example.conf /etc/rport/rportd.conf
```

### Set your own secrets

`rportd` refuses to start with the placeholders and sample values of `rportd.example.conf` (`<YOUR_SEED>`,
`<YOUR_SECRET>`, `admin:foobaz`, `clientAuth1:1234`). Open `/etc/rport/rportd.conf` and set:

* `key_seed` in `[server]`: a random string the server's key pair is derived from. Clients pin the resulting
  fingerprint, so keep it and never change it.
* Client credentials in `[server]`, for example `auth = "client1:<a strong password>"`. For a credential per client,
  see [client authentication](/docs/self-hosting/client-authentication/).
* API credentials in `[api]`, for example `auth = "admin:<a strong password>"`. The example config turns the API on at
  port 3000, and `rportd` won't start it without authentication. For several users, see
  [API authentication](/docs/api-and-cli/api-authentication/).

To set `key_seed` from the command line:

```shell
KEY_SEED=$(openssl rand -hex 18)
sudo sed -i "s/key_seed = .*/key_seed = \"${KEY_SEED}\"/g" /etc/rport/rportd.conf
```

Leave `jwt_secret` empty: `rportd` generates one on the first start and saves it as `rportd-jwt-secret.txt` in the
data directory, so logins survive restarts.

### Check that it starts

Start the server in the foreground as the `rport` user:

```shell
sudo -u rport rportd -c /etc/rport/rportd.conf --log-level info
```

Watch the log with `tail -f /var/log/rport/rportd.log`, and copy the fingerprint from
`/var/lib/rport/rportd-fingerprint.txt`: your clients need it.

### Run it with systemd

Stop `rportd` and register it as a service:

```shell
sudo rportd --service install --service-user rport --config /etc/rport/rportd.conf
sudo systemctl enable --now rportd
```

This creates `/etc/systemd/system/rportd.service`.

{{< hint type=note title="Is the connection encrypted?" >}}
Always. Clients talk to the server over SSH inside HTTP, using the server's ECDSA key pair. The fingerprint makes sure
a client only connects to your server: without it, a man in the middle could pose as your server and take over your
tunnels. Put a TLS reverse proxy in front to get SSH over HTTPS.
{{< /hint >}}

## Connect a client

### Try it on the command line

On the client, here `client1.example.com`, install the binary:

```shell
curl -LO "https://github.com/pohcee/procy-server/releases/download/${VERSION}/rport_${VERSION}_linux_x86_64.tar.gz"
sudo tar vxzf rport_${VERSION}_linux_x86_64.tar.gz -C /usr/local/bin/ rport
```

Forward port 2222 of `procy.example.com` to port 22 of the client:

```shell
rport --auth client1:<CLIENT_PASSWORD> --fingerprint <FINGERPRINT> --data-dir=/tmp \
  procy.example.com:8080 2222:0.0.0.0:22
```

The server log shows the new tunnel. `ssh -p 2222 procy.example.com` now takes you to the client, even though it sits
behind a firewall.

### Run a Linux client with systemd

```shell
sudo tar vxzf rport_${VERSION}_linux_x86_64.tar.gz -C /usr/local/bin/ rport
sudo useradd -d /var/lib/rport -U -m -r -s /bin/false rport
sudo mkdir /etc/rport/ /var/log/rport/
sudo chown rport /var/log/rport/
sudo tar vxzf rport_${VERSION}_linux_x86_64.tar.gz -C /etc/rport/ rport.example.conf
sudo cp /etc/rport/rport.example.conf /etc/rport/rport.conf
sudo rport --service install --service-user rport --config /etc/rport/rport.conf
```

Edit `/etc/rport/rport.conf`. A minimal configuration:

```text
[client]
server = "procy.example.com:8080"
fingerprint = "<FINGERPRINT>"
auth = "client1:<CLIENT_PASSWORD>"
remotes = ['2222:22']
```

`remotes` sets up a permanent tunnel: port 22 of the client becomes port 2222 of the server. Leave it out to connect
without a tunnel and open tunnels [on demand](/docs/remote-access/on-demand-tunnels/).

```shell
sudo systemctl enable --now rport
```

### Run a Windows client

Download `rport_<version>_windows_x86_64.zip` from the
[latest release](https://github.com/pohcee/procy-server/releases/latest) and extract it to `C:\Program Files\rport`.
Rename `rport.example.conf` to `rport.conf` and edit it:

```text
[client]
server = "procy.example.com:8080"
fingerprint = "<FINGERPRINT>"
auth = "client1:<CLIENT_PASSWORD>"
remotes = ['3300:3389']
```

This makes port 3389 (Remote Desktop) of the client available on port 3300 of the server. Test it from a command
prompt with administrative rights:

```shell
cd "C:\Program Files\rport"
rport.exe -c rport.conf
```

If there are no errors, try a Remote Desktop connection to `procy.example.com:3300`. Then stop the client with
CTRL-C, and register and start it as a service:

```shell
rport.exe --service install -c rport.conf
sc start rport
```

The service starts on boot. To change that, run `sc config rport start=disabled`.

### Other systems

[Install and run a client](/docs/devices/install-and-run-a-client/) covers macOS and the other installers.

## Configuration files

Both programs read the config file given with `--config` (or `-c`). The commented examples
[`rportd.example.conf`](https://github.com/pohcee/procy-server/blob/main/rportd.example.conf) and
[`rport.example.conf`](https://github.com/pohcee/procy-server/blob/main/rport.example.conf) are in the release
archives and list every setting. Command line options and environment variables override the config file.

## Next steps

* Install the [web UI](/docs/self-hosting/web-ui/) and [turn on HTTPS](/docs/self-hosting/securing-rportd-with-https/).
* Read how to [secure the server](/docs/self-hosting/securing-the-server/).
* Let clients wait without a tunnel and open [tunnels on demand](/docs/remote-access/on-demand-tunnels/).
* To run several servers, or several organizations on one, move to [Postgres](/docs/self-hosting/postgres/).
