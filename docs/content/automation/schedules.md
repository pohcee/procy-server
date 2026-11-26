---
title: "Schedules"
weight: 30
slug: schedules
---
{{< toc >}}

A schedule runs a command, a script or a [library entry](/docs/automation/scripts/#library-entries) on a set of
devices at fixed times, written as a cron expression. Use it for recurring work such as OS updates, client updates
or clean-ups. This page applies to hosted Procy and to self-hosted servers.

A scheduled run works like a run from the Execute page: the devices need remote commands (and, for scripts, remote
scripts) enabled, see [running commands](/docs/automation/command-execution/#before-you-start). Your user group needs
the **scheduler** permission (see [permissions](/docs/hosted/permissions-model/)).

## Create a schedule

Open **Schedules** in the menu and click **New schedule**.

1. **Schedule name**, for example "Nightly cleanup".
2. **Cron expression**: when to run, see [cron expressions](#cron-expressions).
3. **Targets**: **Devices**, **Groups** or **Tags**, as on the Execute page. Groups and tags are resolved at every
   run, so devices added to a group later are included.
4. **What to run**:
   * **Code**: type a command or script (or use **Insert from library** to start from an entry), and set
     **Interpreter**, **Working directory**, **Timeout (seconds, 0 = server default)** and sudo. With **Run as** on
     Auto, a single full-path command (such as `/usr/bin/uptime`) runs as a command, anything else as a script.
   * **Library entry**: pick an entry. The form shows its **Active version** and the OSes it is **Implemented for**.
     Each run uses the entry's active version at that moment, and each device runs the implementation for its OS;
     the entry sets the interpreter, working directory, timeout and sudo.
5. Options:
   * **One device at a time**: run the devices one after another instead of all at once.
   * **Abort on error**: with one device at a time, stop at the first device that fails.
6. Save.

To schedule code for a single device, write it on the device's **Run** tab and click **Schedule**. A device's
**Schedules** tab lists the schedules that target it.

{{< hint type=tip >}}
To keep a fleet up to date, schedule the built-in **OS updates** and **Client update** entries. Each is one entry
with a Linux and a Windows implementation, so a single schedule covers both. To roll out in steps, make one schedule
per group (for example a test group first, the rest a day later).
{{< /hint >}}

## Cron expressions

The expression has five fields: minute, hour, day of month, month and day of week.

| Expression | Runs |
| --- | --- |
| `*/15 * * * *` | every 15 minutes |
| `0 3 * * *` | every day at 03:00 |
| `30 2 * * 0` | Sundays at 02:30 |
| `0 6 1 * *` | the first of each month at 06:00 |
| `@daily`, `@hourly`, `@weekly`, `@monthly` | at the start of each day, hour, week or month |
| `@every 6h` | every 6 hours |

Times are in the server's time zone. Hosted Procy runs in UTC.

A schedule doesn't start a new run while its previous run is still going; that run is skipped. For a library
entry, "still going" is judged by the longest timeout among the entry's implementations.

## Watch the results

The Schedules page shows each schedule's last run: **Succeeded**, **Partly failed**, **Failed**, **Running**,
**Skipped** or **No devices**, with how many devices succeeded. Click the history icon for **Run history**: every run,
newest first; open one for each device's result and output. Scheduled runs also appear in the run history on the
Execute page, marked as scheduled, with a link to the schedule.

If a library entry a schedule runs has no implementation for any of its targets, the run is recorded as failed; it is
never silently skipped. An entry can't be deleted while a schedule runs it.

## Schedules through the API

The examples use an [API token](/docs/api-and-cli/api-authentication/) and hosted Procy:

```shell
export PROCY=https://procy.sh/api/v1
export CREDS='you@example.com:<api-token>'
```

Run a library entry every Sunday at 02:30 on two groups:

```shell
curl -s -u "$CREDS" -X POST "$PROCY/schedules" \
  -H 'Content-Type: application/json' \
  --data-raw '{
    "name": "Weekly OS updates",
    "schedule": "30 2 * * 0",
    "type": "library",
    "library_entry_id": "4943d682-7874-4f7a-999c-b4ff5493fc3f",
    "group_ids": ["linux-servers", "windows-servers"],
    "execute_concurrently": true,
    "abort_on_error": false
  }' | jq
```

Fields:

`name`, `schedule` (required)
: The name and the cron expression.

`type` (required)
: `command`, `script` or `library`.

`command`, `script`, `library_entry_id`
: What to run, matching the type. `script` is base64-encoded.

`client_ids`, `group_ids`, `tags`
: The targets, as for [multi-device commands](/docs/automation/command-execution/#several-devices).

`interpreter`, `cwd`, `is_sudo`, `timeout_sec`
: For `command` and `script` only; a library entry brings its own.

`execute_concurrently`, `abort_on_error`
: As for multi-device commands.

`overlaps`
: `true` lets a new run start while the previous one is still going. Default `false`.

`GET /schedules` lists schedules (with `filter[...]` and `sort`), `GET`, `PUT` and `DELETE /schedules/{id}` read,
replace and remove one. A schedule's runs are multi-jobs: `GET /commands?filter[schedule_id]=<id>&sort=-started_at`.

## Moving schedules to another server

**Self-hosted only.** When you move a standalone server to Postgres with `rportd import`, schedules come along. Each
tenant has its own copies of the built-in entries under their own IDs, so the import points schedules that run a
built-in entry (such as **OS updates**) at the tenant's copy; your own entries keep their IDs. See
[move a standalone server to Postgres](/docs/self-hosting/postgres/#move-a-standalone-server-to-postgres).
