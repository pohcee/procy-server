---
title: "procy-cli"
weight: 20
slug: cli
---
{{< toc >}}

procy-cli is a set of `procy-*` shell commands for working with devices from your terminal: list them, open SSH
sessions, copy and sync files, mount folders and open tunnels, all through the Procy API. It works with hosted Procy
and with self-hosted servers. The source is at [github.com/pohcee/procy-cli](https://github.com/pohcee/procy-cli).

## Install

You need `bash`, `curl`, `jq`, `ssh`, `scp`, `rsync`, `sshfs` and `column` (on Debian and Ubuntu, `column` is in
`bsdmainutils`). Then:

```bash
git clone https://github.com/pohcee/procy-cli.git
cd procy-cli
./install.sh
```

The installer checks the prerequisites, links the `procy-*` commands into `~/.local/bin` and adds shell completion.
`./uninstall.sh` removes them again.

Create an [API token](/docs/api-and-cli/api-authentication/) (scope **Read + write**, since the commands create
tunnels) and set these in your shell profile:

```bash
export PROCY_HOST=procy.sh                          # the server; the API is https://$PROCY_HOST/api/v1
export PROCY_CREDENTIALS='you@example.com:<api-token>'
```

For a self-hosted server, set `PROCY_HOST` to its host name. The server must be reachable over HTTPS.

{{< hint type=note >}}
The commands were called `rport-*` before. `rport-clients`, `rport-ssh` and the others are still installed as
wrappers that run the matching `procy-*` command, and `RPORT_HOST` / `RPORT_CREDENTIALS` are still read when the
`PROCY_*` variables are not set. Move to the new names; the old ones will be removed in a future major release.
{{< /hint >}}

## How it finds and reaches devices

Commands take a device by its **name**, which must match exactly one device. To reach a device, a command opens (or
reuses) an [on-demand tunnel](/docs/remote-access/on-demand-tunnels/) to the device's SSH port, allowed only from your
current public IP address and closed after 5 idle minutes. Your SSH client then connects through it.

The SSH username comes from the [vault](/docs/automation/vault/): create an entry for the device with the key
`ssh-user` and the username as its value. Authenticate to SSH as usual, for example with your SSH key.

## Commands

All commands print help with `-h` or `--help`.

### procy-clients

List devices with their status, host name and number of tunnels.

```text
$ procy-clients
CLIENT_NAME   CLIENT_ID                              STATUS   HOSTNAME    TUNNELS
client-01     e0668077-d9b8-40aa-b5e6-420f38b05637   online   10.2.20.10  1
client-02     e0668077-d9b8-40aa-b5e6-420f38b05638   offline  10.2.20.11  0
```

* `--status=online` or `--status=offline` (`-o` is short for online) filters by status.
* `--json` prints the raw API response, for scripts.

### procy-status

Print the server's status as JSON (`GET /status`).

### procy-ssh

Open an SSH session on a device, or run one command there:

```text
$ procy-ssh client-01
$ procy-ssh client-01 "ls -l /tmp"
```

`-v` prints what it does, for troubleshooting. Host key checks are skipped, because the tunnel's host and port are
reused for different devices.

### procy-scp

Copy files to or from a device. Write the remote side as `<device name>:<path>`:

```text
$ procy-scp local-file.txt client-01:/tmp/remote-file.txt
$ procy-scp client-01:/var/log/syslog ./
```

### procy-rsync

Sync files or folders with rsync over SSH. `-d` deletes files at the destination that aren't in the source
(`rsync --delete`):

```text
$ procy-rsync ./site/ client-01:/var/www/site/
$ procy-rsync -d client-01:/etc/nginx/ ./nginx-backup/
```

### procy-sshfs

Mount a folder of a device locally with SSHFS:

```text
$ procy-sshfs client-01:/home/admin /mnt/client-01
```

### procy-tunnel

Open (or reuse) a tunnel and print how to reach it, for use in your own scripts. The second argument is the device
port (default `22`):

```text
$ procy-tunnel client-01
admin|usc1-1.connect.procy.sh|20417
$ procy-tunnel client-01 3389
admin|usc1-1.connect.procy.sh|20533
```

The output is `<ssh-user>|<host>|<port>`. Give a third argument to forward through the device to another host on its
network instead; the output is then `<host>|<port>` and the tunnel closes after 60 idle minutes:

```text
$ procy-tunnel client-01 80 192.168.1.50
usc1-1.connect.procy.sh|20871
```

### procy-metadata

Print all [vault](/docs/automation/vault/) entries of a device as one JSON object:

```text
$ procy-metadata client-01
{
  "ssh-user": "admin"
}
```

## Troubleshooting

* **Authentication failed (HTTP 401):** check `PROCY_CREDENTIALS` (username and token). The commands stop at the
  first `401`, because repeated failures get you temporarily banned.
* **Rate limited (HTTP 429):** too many failed attempts; wait before trying again.
* **Unable to find exact match for client:** the name matches no device, or several. Check with `procy-clients`.
* **Client is not connected:** the device is offline.
* **Failed to look up 'ssh-user' in vault:** add the `ssh-user` entry for the device.
