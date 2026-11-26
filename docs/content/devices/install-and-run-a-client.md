---
title: "Add a device"
weight: 10
slug: install-and-run-a-client
aliases:
- /get-started/install-and-run-a-client/
- /get-started/install-and-run-rport-client/
- /docs/no05-client-on-other-os.html
---
{{< toc >}}

A device joins Procy when the Procy client (`rport`) runs on it and connects to your server. This page shows how to
install the client: with the one-line installer from the web interface on Linux and Windows, or by hand on macOS
and other systems. It applies to hosted Procy and to self-hosted servers.

## With the installer (Linux and Windows)

The web interface creates the device's credentials and gives you a one-line command that installs and configures the
client. This is called [pairing](/docs/devices/client-pairing/). On hosted Procy it works out of the box. On a
self-hosted server, [enable pairing](/docs/devices/client-pairing/#enable-pairing-self-hosted) first.

1. Open **Devices** and choose **Add device**.
2. Enter an **ID** for the device, or keep the generated one, and a **Password** of at least 12 characters (letters,
   digits and `_ + - . :`). Choose **Add Client Access**.
3. In the **Install client** dialog, pick the connect URL the device should use. On hosted Procy, keep
   `https://connect.procy.sh`.
4. Choose the [installer options](#installer-options).
5. Copy the command for the device's system and run it on the device.

On Linux, as root or with `sudo`:

```shell
curl https://procy.sh/pairing/<code> > rport-installer.sh
sudo sh rport-installer.sh -x -r
```

On Windows, in PowerShell as Administrator:

```powershell
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$url="https://procy.sh/pairing/<code>"
Invoke-WebRequest -Uri $url -OutFile "rport-installer.ps1"
powershell -ExecutionPolicy Bypass -File .\rport-installer.ps1 -x -r
```

The installer downloads and checks the client, writes its configuration, installs it as a service and starts it. A
few seconds later the device shows up on the **Devices** page.

The command contains a code that works once and expires after an hour. For a new code, open **Settings → Client
access**, find the device's ID and choose **Install client** in its actions menu.

{{< hint type=note >}}
Each set of credentials is meant for one device. To add another device, choose **Add device** again.
{{< /hint >}}

### Installer options

| Option | Flag | What it does |
| --- | --- | --- |
| Enable remote script and command | `-x` | Lets Procy run any [command or script](/docs/automation/command-execution/) on the device. Without it, remote commands are off. |
| Give remote scripts and command sudo rights | `-s` | Linux only. Lets the client's user run anything with `sudo` (`/etc/sudoers.d/rport-all-cmd`). |
| Enable file transfer | `-r` | Lets you [send files](/docs/automation/file-reception/) to the device. |
| Enable file transfer sudo rights | `-b` | Linux only. Lets received files be written anywhere. Needs `-r`. |
| Install Tacoscript along with the RPort client | `-i` | Also installs Tacoscript, a script interpreter. |

{{< hint type=note >}}
On Linux, the installer currently ignores `-r` and `-b`: file reception is on, without sudo rights, after any install.
The [update script](/docs/devices/client-pairing/#updating-clients) applies both flags. On Windows, file reception
is on only with `-r`.
{{< /hint >}}

The Linux installer also takes `-g <tag>` (add a tag), `-a <user>` (run the client as another user than `rport`),
`-l` (SELinux), `-d` (a random ID instead of `/etc/machine-id`) and `-u` (uninstall). Run `sh rport-installer.sh -h`
for the full list.

### What the installer sets up

| | Linux | Windows |
| --- | --- | --- |
| Program | `/usr/local/bin/rport` | `C:\Program Files\rport\rport.exe` |
| Configuration | `/etc/rport/rport.conf` | `C:\Program Files\rport\rport.conf` |
| Log | `/var/log/rport/rport.log` | `C:\Program Files\rport\rport.log` |
| Service | `rport` (systemd or OpenRC) | `rport` |
| Runs as | the `rport` user | Local System |

It also writes an [attributes file](/docs/devices/attributes/) with the device's city and country as labels, and on
Debian, Ubuntu and SUSE the sudo rules the client needs to read the [update status](/docs/devices/supervising-update-status/).

To keep clients up to date, see [Updating clients](/docs/devices/client-pairing/#updating-clients).

## By hand (macOS and other systems)

The installer covers Linux and Windows. On macOS, FreeBSD or a system without the installer, install the client by
hand. You need:

- the **connect URL**: `https://connect.procy.sh` on hosted Procy, or your server's `[server] url`;
- the server's **fingerprint**, shown under **Settings → Server info**;
- a **client ID and password**: create them under **Settings → Client access** with **Add access**.

Client packages for every system are on the [releases page](https://github.com/pohcee/procy-server/releases/latest).

### macOS

Download the client and create its directories:

```shell
VERSION=$(curl -fsSL https://api.github.com/repos/pohcee/procy-server/releases/latest \
  | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
curl -L -o rport-mac.tar.gz \
  "https://github.com/pohcee/procy-server/releases/download/${VERSION}/rport_${VERSION}_darwin_$(uname -m).tar.gz"
test -e /usr/local/bin/ || sudo mkdir /usr/local/bin
sudo tar xzf rport-mac.tar.gz -C /usr/local/bin/ rport
rport --version
sudo mkdir -p /etc/rport /private/var/log/rport /private/var/lib/rport
sudo tar xzf rport-mac.tar.gz -C /etc/rport rport.example.conf
sudo mv /etc/rport/rport.example.conf /etc/rport/rport.conf
```

Set the server, credentials and fingerprint in the configuration:

```shell
CONNECT_URL="https://connect.procy.sh"
FINGERPRINT="<FINGERPRINT>"
CLIENT_ID="<CLIENT_ID>"
PASSWORD="<CLIENT_PASSWORD>"
CONFIG_FILE=/etc/rport/rport.conf
sudo sed -i '' "s|#*server = .*|server = \"${CONNECT_URL}\"|g" "$CONFIG_FILE"
sudo sed -i '' "s/#*auth = .*/auth = \"${CLIENT_ID}:${PASSWORD}\"/g" "$CONFIG_FILE"
sudo sed -i '' "s/#*fingerprint = .*/fingerprint = \"${FINGERPRINT}\"/g" "$CONFIG_FILE"
sudo sed -i '' "s/#*log_file = .*C.*Program Files.*/""/g" "$CONFIG_FILE"
sudo sed -i '' "s/#*log_file = /log_file = /g" "$CONFIG_FILE"
```

Test it in the foreground first:

```shell
sudo rport -c /etc/rport/rport.conf
```

It prints nothing when it works, and the device shows up in the web interface. Stop it with Ctrl+C.

The client should run as its own user, not as root and not as the built-in `daemon` user. Create a system user
`rport`:

```shell
cat <<"EOF" | sudo bash
set -e
username="rport"
if id ${username} >/dev/null 2>&1; then echo "User ${username} exists"; exit 0; fi
for (( uid = 500;; --uid )); do
  if ! id -u $uid &>/dev/null && ! dscl /Local/Default -ls Groups gid | grep -q [^0-9]$uid\$; then
    dscl /Local/Default -create Groups/_$username
    dscl /Local/Default -create Groups/_$username Password \*
    dscl /Local/Default -create Groups/_$username PrimaryGroupID $uid
    dscl /Local/Default -create Groups/_$username RealName "Procy client"
    dscl /Local/Default -create Groups/_$username RecordName _$username $username
    dscl /Local/Default -create Users/_$username
    dscl /Local/Default -create Users/_$username NFSHomeDirectory /private/var/lib/rport
    dscl /Local/Default -create Users/_$username Password \*
    dscl /Local/Default -create Users/_$username PrimaryGroupID $uid
    dscl /Local/Default -create Users/_$username RealName "Procy client"
    dscl /Local/Default -create Users/_$username RecordName _$username $username
    dscl /Local/Default -create Users/_$username UniqueID $uid
    dscl /Local/Default -create Users/_$username UserShell /bin/sh
    dscl /Local/Default -delete /Users/_$username AuthenticationAuthority
    dscl /Local/Default -delete /Users/_$username PasswordPolicyOptions
    break
  fi
done
chown rport /private/var/log/rport /private/var/lib/rport
EOF
```

Install and start the service:

```shell
sudo rport --service install --service-user rport -c /etc/rport/rport.conf
sudo rport --service start
sudo launchctl list | grep "rport$"
```

The first column shows the process ID when the client runs, for example `9942 0 rport`. A `-` instead means the
service is installed but the client can't start. The usual cause is that the `rport` user can't write to the log
directory. To see the error, set `StandardOutPath` and `StandardErrorPath` in `/Library/LaunchDaemons/rport.plist` to
files under `/tmp`, then reload:

```shell
sudo launchctl unload /Library/LaunchDaemons/rport.plist
sudo launchctl load /Library/LaunchDaemons/rport.plist
sudo launchctl start rport
```

The service starts at boot.

### Other systems

On other systems, download the archive for your system and architecture from the
[releases page](https://github.com/pohcee/procy-server/releases/latest), copy `rport` to a directory on the path, and
start from the included `rport.example.conf`: set `server`, `auth` and `fingerprint` in the `[client]` section.
`rport --service install -c <config file>` registers it as a service where the system supports that.

## Check the connection

If the device doesn't show up:

- Look at the client's log (see the table above).
- Check that the device can reach the server. For hosted Procy, see
  [Network requirements](/docs/hosted/network-requirements/#checking-from-a-device).
- Check that the credentials still exist under **Settings → Client access**, and that no other device uses them.

## Remove a device

1. On the device, uninstall the client: on Linux run the installer with `-u`; on other systems run
   `rport --service uninstall` and delete the files.
2. In the web interface, delete the device and its credentials under **Settings → Client access**. A device whose
   credentials still exist can connect again.
