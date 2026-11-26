---
title: "Scripts and the library"
weight: 20
slug: scripts
aliases:
- /get-started/scripts/
- /docs/no14-scripts.html
---
{{< toc >}}

The script library keeps the commands and scripts you run often, with a Linux, Windows and macOS version of each, a
full history of changes, and built-in entries maintained by Procy. Run an entry on a mix of devices and each device
runs the version for its OS. This page also covers how scripts run on a device. It applies to hosted Procy and to
self-hosted servers.

## Library entries

A **library entry** has a name, a description, tags and one **implementation** per operating system: Linux, Windows
and macOS. Each implementation has its own code, **Run as** choice (Auto, Command or Script), interpreter, working
directory, timeout and sudo setting. An entry needs at least one implementation.

When you run an entry, each device runs the implementation for its OS. A device whose OS has no implementation is
**skipped**: nothing is sent to it, it shows as skipped in the results, and it doesn't count as a failure. If no
target has a matching implementation, the run is refused.

Open **Script library** in the menu to see all entries, with their OS badges, tags and version. Use the search box to
filter by name, tag or type.

### Create and edit an entry

1. Click **New entry**.
2. Enter a **Name** (unique) and optionally a **Description** and tags.
3. Add a tab for each **Operating system** you support and write its code. Set **Interpreter**, **Working
   directory**, **Timeout (seconds)** and sudo for that OS. **Remove this OS** drops an implementation.
4. Optionally describe the change under **What changed**; it is shown in the history.
5. Save.

You can also save code from the Execute page or a device's Run tab with **Save to library**; it is saved as the
implementation for the OS you choose under **Save as the version for**.

### Versions and history

Every save creates a new **version** (1, 2, 3, ...). Only the newest version, the **active version**, runs; older
versions are kept as read-only history. **History** in the entry editor lists every version with who saved it, when,
and what changed. Pick a version to see its changes or compare it with another one, and **Restore this version** to
make it active again. A restore saves a new version with the old content, so nothing is lost.

If someone else saved the entry while you were editing it, your save is refused and you are shown the newer version,
so you don't overwrite their change by accident.

**Self-hosted:** limit how many versions are kept with the `[library]` section of `rportd.conf`. Both default to `0`
(keep everything); the active version and version 1 are never removed.

```toml
[library]
  ## Keep only the newest N versions of each entry.
  keep_versions = 20
  ## Remove versions older than N days.
  keep_versions_days = 365
```

### Delete and undelete

Deleting an entry hides it but keeps its history. Tick **Show deleted** on the Script library page to see deleted
entries and **Undelete** one. An entry that a [schedule](/docs/automation/schedules/) runs can't be deleted; the
editor lists those schedules under **Used by**, so you can change or remove them first.

### Built-in entries

Procy ships some entries, marked **Built-in**. They are read-only and updated with each Procy release (the history
shows "Updated with Procy _version_"). Click **Duplicate** to make your own editable copy.

| Entry | What it does | Linux | Windows |
| --- | --- | --- | --- |
| **Client update** | Updates the Procy client to the latest stable version if it is newer, and rolls back if the client doesn't reconnect. | sh, sudo | PowerShell |
| **OS updates** | Installs all pending OS updates and restarts the host if needed. | sh, sudo, 30 min | PowerShell, 60 min |
| **Switch server** | Points the client at the server's connect addresses (the first as server, the others as fallbacks), and rolls back if it doesn't reconnect. | sh, sudo | PowerShell |

**OS updates** on Linux works with apt (Debian, Ubuntu, Proxmox VE), dnf or yum (RHEL family) and zypper (SUSE). It
waits for automatic updates that are already running, respects package holds and keeps local configuration files. A
host that runs the Procy server is never restarted automatically. The result is written to
`/var/lib/procy-os-updates/last.json` on the device.

Built-in entries run as scripts and use sudo on Linux, so the device needs **Enable remote script and command** and
**Give remote scripts and command sudo rights** (installer options `-x` and `-s`).

**Self-hosted:** **Client update** and **Switch server** download the client update script from your server's
built-in pairing service, so `[pairing] enabled` must be on and the server needs its public address (`[pairing] url`,
or else `[api] base_url`). Without them, the run is refused with "enable [pairing] and set its url (or api.base_url)".
**Switch server** sets the addresses listed in `[server] url`. See [client pairing](/docs/devices/client-pairing/).

### Run an entry

* **Execute** page or a device's **Run** tab: choose **Library entry**, pick the entry and the targets, and click
  **Run**. The page shows the active version, the OSes it is **Implemented for**, and how the targets split, for
  example "3 Linux, 1 Windows, 1 macOS skipped". Output arrives live, as for typed code.
* **Insert from library** in the code editor copies one implementation into the editor instead, to change it before
  running. Changing the copy doesn't change the entry.
* **Schedules**: a schedule can run an entry; each run uses the entry's active version at that moment. See
  [schedules](/docs/automation/schedules/).

Each job records the entry, the version and the OS implementation that ran.

### Library entries through the API

The examples use an [API token](/docs/api-and-cli/api-authentication/) and hosted Procy:

```shell
export PROCY=https://procy.sh/api/v1
export CREDS='you@example.com:<api-token>'
```

Create an entry:

```shell
curl -s -u "$CREDS" -X POST "$PROCY/library/entries" \
  -H 'Content-Type: application/json' \
  --data-raw '{
    "name": "Disk usage",
    "description": "Shows free space",
    "tags": ["diagnostics"],
    "implementations": {
      "linux":   {"kind": "command", "code": "/usr/bin/df -h", "interpreter": "",
                  "is_sudo": false, "cwd": "", "timeout_sec": 60},
      "windows": {"kind": "script", "code": "Get-PSDrive -PSProvider FileSystem", "interpreter": "powershell",
                  "is_sudo": false, "cwd": "", "timeout_sec": 60}
    }
  }' | jq
```

OS keys are `linux`, `windows` and `darwin` (macOS). `kind` is `auto`, `command` or `script`.

Run it. The body takes the same targets and options as
[multi-device commands](/docs/automation/command-execution/#several-devices) (`client_ids`, `group_ids`, `tags`,
`execute_concurrently`, `abort_on_error`) and returns a multi-job:

```shell
ENTRY_ID=4943d682-7874-4f7a-999c-b4ff5493fc3f
curl -s -u "$CREDS" -X POST "$PROCY/library/entries/$ENTRY_ID/run" \
  -H 'Content-Type: application/json' \
  --data-raw '{"group_ids": ["web-servers"], "execute_concurrently": true}' | jq
```

| Method and path | What it does |
| --- | --- |
| `GET /library/entries` | List entries. Filters `filter[name]`, `filter[tags]`, `filter[os]`, `filter[builtin]`, `filter[deleted]=true`; `sort=name` or `sort=-updated_at`; `page[limit]`, `page[offset]`. |
| `POST /library/entries` | Create an entry (version 1). |
| `GET /library/entries/{id}` | One entry with its implementations. |
| `PUT /library/entries/{id}` | Save a new version. Send the whole entry plus `base_version`, the version you edited; if it isn't the active version any more, the answer is `409 Conflict`. Optional `message`. |
| `DELETE /library/entries/{id}` | Delete. `409` with the schedules that use it, if any. |
| `POST /library/entries/{id}/undelete` | Undelete. |
| `GET /library/entries/{id}/used-by` | The schedules that run the entry. |
| `GET /library/entries/{id}/versions` | The versions (metadata). |
| `GET /library/entries/{id}/versions/{n}` | One version's content. |
| `POST /library/entries/{id}/versions/{n}/restore` | Restore version `n` as a new version (send `base_version`). |
| `POST /library/entries/{id}/run` | Run on devices. |

The older `/library/scripts` and `/library/commands` endpoints still work, as views of entries with one main
implementation, but are deprecated. Use `/library/entries` for new scripts.

## How scripts run on a device

Code runs as a **script** when you choose Script, or when Auto sees several lines or a `#!` line. The device:

1. writes the script to a temporary file with a random name in `{data_dir}/scripts` (on Linux usually
   `/var/lib/rport/scripts`),
2. runs it with the interpreter, for example `sh /var/lib/rport/scripts/f68a779d-....sh`, or
   `powershell -executionpolicy bypass -file C:\...\f68a779d-....ps1` on Windows,
3. deletes the file, whether the script succeeded or not.

### Scripts execution

Scripts need both remote commands and remote scripts enabled in the device's `rport.conf`. The installer option
**Enable remote script and command** (`-x`) turns on both.

```toml
[remote-commands]
  enabled = true

[remote-scripts]
  enabled = true
```

Scripts are not checked against the command allow-list (see
[securing your environment](/docs/automation/command-execution/#securing-your-environment)), so turn
`[remote-scripts]` off on a device that should only run allowed commands.

### Customizing an interpreter

The default interpreter is `/bin/sh` on Linux and macOS (or the interpreter on the script's `#!` line) and `cmd` on
Windows. Other values:

* `powershell` on Windows.
* `tacoscript` on Linux and Windows (see [below](#tacoscript)).
* An absolute path to an interpreter of your choice, such as `/usr/local/bin/zsh` or
  `C:\Program Files\PowerShell\7\pwsh.exe`. On Linux and macOS it must accept `-c`. On Windows, if the path contains
  "powershell" (in any case), `-Noninteractive -executionpolicy bypass -File` is added for you.
* An **alias** defined on the device, in the `[interpreter-aliases]` section of `rport.conf`. The alias may also set the
  input and output encoding:

  ```toml
  [interpreter-aliases]
    pwsh7 = 'C:\Program Files\PowerShell\7\pwsh.exe'
    pwsh7enc = ['C:\Program Files\PowerShell\7\pwsh.exe', 'windows-1252', 'cp437']
    bash = 'C:\Program Files\Git\bin\bash.exe'
  ```

  Then send `pwsh7` as the interpreter.

### Run a script through the API

Send the script base64-encoded. On one device:

```shell
CLIENT_ID=my-client
curl -s -u "$CREDS" -X POST "$PROCY/clients/$CLIENT_ID/scripts" \
  -H 'Content-Type: application/json' \
  --data-raw "{
    \"script\": \"$(base64 -w0 ./check.sh)\",
    \"interpreter\": \"/bin/bash\",
    \"cwd\": \"/tmp\",
    \"is_sudo\": false,
    \"timeout_sec\": 60
  }" | jq
```

The response holds the job ID; fetch the result with
`GET /clients/{client_id}/commands/{job_id}`, as for [commands](/docs/automation/command-execution/#one-device).

On several devices, `POST /scripts` takes the same fields plus `client_ids`, `group_ids` or `tags`,
`execute_concurrently` and `abort_on_error`, and returns a multi-job (`GET /commands/{job_id}`). For live output,
use the WebSocket endpoint `/api/v1/ws/scripts`.

API requests are limited to 10 KB by default (`[api] max_request_bytes` on self-hosted servers). For a longer
script, save it as a library entry and run the entry: that request stays small, however long the script is.

### Tacoscript

[Tacoscript](https://github.com/openrport/tacoscript) runs scripts in a format like SaltStack states on Linux and
Windows, with no other tools needed: conditions on exit codes, files and host facts, package installs, file creation,
dependencies between tasks and variables. Install it on the device (installer option **Install Tacoscript along with
the RPort client**, `-i`) and use `tacoscript` as the interpreter:

```yaml
# Prints the current date.
date command:
  cmd.run:
    - names:
      - date
```
