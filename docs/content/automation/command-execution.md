---
title: "Running commands"
weight: 10
slug: command-execution
aliases:
- /get-started/command-execution/
- /docs/no06-command-execution.html
- /docs/get-started/no06-command-execution.md
---
{{< toc >}}

Run a command or a script on one device or many and see each device's output as it arrives. This works on hosted
Procy and on self-hosted servers. The command and its output travel over the connection the device already holds;
no tunnel is needed.

![Running commands: you post a command for a client group; the server creates a job per client; each client checks the command against its own rules, runs it and sends back the result](/docs/images/procy-commands.svg)

Each run is a **multi-job**: the server creates one **job** per device, each device checks the command against its
own rules, runs it and sends back the exit code, standard output and standard error.

## Before you start

* The device must allow remote commands. When you install a client from the web interface, tick **Enable remote
  script and command** (installer option `-x`). Without it, the device refuses commands with "remote commands
  execution is disabled". To run things as root on Linux, also tick **Give remote scripts and command sudo rights**
  (`-s`). See [securing your environment](#securing-your-environment) to change this later.
* Your user group needs the **commands** permission (see [permissions](/docs/hosted/permissions-model/)).

## Run from the web interface

Open **Execute** in the menu to run on several devices, or a device's page, tab **Run**, for that one device.

1. **Targets** (Execute page only): pick **Devices**, **Groups** or **Tags** and add targets. With tags, choose
   whether a device must have **All tags** or **Any tag**. The count next to Targets shows how many devices match.
2. Choose **Code** to type something, or **Library entry** to run a saved [library entry](/docs/automation/scripts/).
3. Type the command or script. **Run as** decides how it is sent:
   * **Auto** (default): a single line runs as a command; several lines, or a first line starting with `#!`, run as
     a script.
   * **Command**: sent as one command line. The device checks it against its command allow-list.
   * **Script**: written to a temporary file on the device and run with the interpreter. Scripts aren't subject to
     the command allow-list.
4. **Options**:
   * **Interpreter**: **Default** (`/bin/sh`, or the script's `#!` line; `cmd.exe` on Windows), **Bash**,
     **PowerShell (Windows)**, **cmd.exe (Windows)** or **Tacoscript**. You can also type an interpreter path or an
     [interpreter alias](/docs/automation/scripts/#customizing-an-interpreter).
   * **Working directory**: where it runs, such as `/var/www`.
   * **Timeout (seconds)**: how long the device watches the command (default 300 in the web interface). After the
     timeout the result is reported as `unknown`, but the command keeps running on the device.
   * **Run with sudo (Unix only)**: runs it through `sudo -n`. The device needs a matching sudo rule.
   * **Abort on error**: stop at the first device that fails, when devices run one after another.
5. Click **Run on _N_ devices** (or **Run** on a device's page).

Results appear per device as they come in, with the status, exit code and output. **Re-run failed** runs the same
thing again on the devices that failed, and **Download output** saves all output to a file. If some devices refused
the run as a command because of their allow-list, the page offers **Run as a script** on them.

**Save to library** keeps the code as a [library entry](/docs/automation/scripts/) (give it a name first). On a
device's Run tab, **Schedule** turns the code into a [schedule](/docs/automation/schedules/) for that device.

Earlier runs are listed under **Recent runs** on the Execute page; open one to see each device's result.

## Run through the API

The examples use an [API token](/docs/api-and-cli/api-authentication/) and hosted Procy:

```shell
export PROCY=https://procy.sh/api/v1
export CREDS='you@example.com:<api-token>'
```

### One device

```shell
CLIENT_ID=my-client
curl -s -u "$CREDS" -X POST "$PROCY/clients/$CLIENT_ID/commands" \
  -H 'Content-Type: application/json' \
  --data-raw '{"command": "/usr/bin/uptime", "timeout_sec": 30}' | jq
```

The response holds the job ID (`jid`). Fetch the result with it:

```shell
JOB_ID=f72b69fd-f418-40c3-ab62-4ce2c2022c58
curl -s -u "$CREDS" "$PROCY/clients/$CLIENT_ID/commands/$JOB_ID" | jq
```

```json
{
  "data": {
    "jid": "f72b69fd-f418-40c3-ab62-4ce2c2022c58",
    "status": "successful",
    "client_id": "my-client",
    "command": "/usr/bin/uptime",
    "interpreter": "/bin/sh",
    "started_at": "2026-11-25T15:30:12.934238782Z",
    "finished_at": "2026-11-25T15:30:12.937267522Z",
    "created_by": "you@example.com",
    "timeout_sec": 30,
    "result": {
      "stdout": " 15:30:12 up 12 days,  3:04,  0 users,  load average: 0.01, 0.03, 0.00\n",
      "stderr": ""
    }
  }
}
```

`GET /clients/{client_id}/commands` lists the device's earlier jobs.

### Several devices

`POST /commands` takes the targets and options in the body:

```shell
curl -s -u "$CREDS" -X POST "$PROCY/commands" \
  -H 'Content-Type: application/json' \
  --data-raw '{
    "command": "/usr/bin/uptime",
    "client_ids": ["client-1", "client-2"],
    "group_ids": ["web-servers"],
    "timeout_sec": 30,
    "execute_concurrently": true
  }' | jq
```

The response holds the multi-job ID. `GET /commands/{job_id}` returns the multi-job with every device's job in
`jobs`; `GET /commands/{job_id}/jobs` lists the jobs alone. `GET /commands` lists earlier multi-jobs.

Body fields:

`command` (required)
: The command line.

`client_ids`, `group_ids`
: Devices and [client groups](/docs/devices/client-groups/) to run on. You can combine them.

`tags`
: Instead of IDs, run on devices by tag: `{"tags": ["linux", "berlin"], "operator": "AND"}` (`AND` or `OR`).
  Don't combine it with `client_ids` or `group_ids`.

`interpreter`, `cwd`, `is_sudo`
: As in the web interface.

`timeout_sec`
: How long each device watches the command. Defaults to the server's `run_remote_cmd_timeout_sec` (60 seconds).

`execute_concurrently`
: `true` runs on all devices at once. The default `false` runs them one after another.

`abort_on_error`
: With sequential runs, stop at the first device that fails (default `true`). It has no effect with
  `execute_concurrently: true`.

To run a script rather than a command, use the script endpoints described under
[scripts and the library](/docs/automation/scripts/#run-a-script-through-the-api). To follow output live, use the
WebSocket endpoint `/api/v1/ws/commands`; see the [API reference](/docs/api/).

## Securing your environment

A command runs as the user the Procy client runs as. On Linux that is the unprivileged `rport` user (never run the
client as root); on Windows the client runs as a service account with administrative rights.

What a device accepts is set in its own `rport.conf`, so a compromised or careless server account can't override it.
The `[remote-commands]` section:

```toml
[remote-commands]
  ## Turn remote commands (and scripts) on or off.
  enabled = true

  ## Commands matching one of these regular expressions are allowed. Use full paths.
  allow = ['^/usr/bin/.*', '^/usr/local/bin/.*', '^C:\\Windows\\System32\\.*']

  ## Commands matching one of these are refused. The default refuses pipes, redirects and command chains.
  deny = ['(\||<|>|;|,|\n|&)']

  ## Which list is applied first.
  order = ['allow', 'deny']

  ## The most output sent back, for stdout and stderr each, in bytes (default 4 MB).
  send_back_limit = 4194304
```

`order` works like this:

* `['allow', 'deny']` (default): a command must match an `allow` entry, and must not match a `deny` entry.
* `['deny', 'allow']`: a command matching a `deny` entry is refused unless it also matches an `allow` entry;
  anything matching neither is allowed. For example `deny = ['.*']` with `allow = ['zip$']` allows only commands
  ending in `zip`.

The installer's **Enable remote script and command** option sets `allow = ['.*']` and `deny = []`, which allows
everything. Narrow it if a device should only run known commands, for example:

```toml
allow = [
  '^/usr/bin/.*',
  '^/usr/local/bin/.*',
  '^sudo -n .*'
]
```

On Windows, for example:

```toml
allow = [
  '^C:\\Windows\\System32\\.*',
  '^C:\\Users\\Administrator\\scripts\\.*\.bat'
]
```

With an allow-list like this, send commands with their full path. Scripts are not checked against the allow-list;
turn them off with `[remote-scripts] enabled = false` if a device should only run allowed commands. Restart the
client after changing `rport.conf`.
