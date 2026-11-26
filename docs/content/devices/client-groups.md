---
title: "Client groups"
weight: 30
slug: client-groups
aliases:
- /get-started/client-groups/
  - /docs/no04-clients-groups.html
  - /docs/get-started/no04-client-groups.md
---
{{< toc >}}

A client group is a named set of devices. Use groups to run commands and schedules on many devices at once, to filter
the device list, and to [give user groups access](/docs/hosted/permissions-model/#per-client-group) to many devices
at once. A group can list devices by ID or select them by rules, such as all Linux devices with a tag. Devices that
match a rule join the group by themselves. This page applies to hosted Procy and to self-hosted servers.

## Manage groups

Administrators see the groups under **Settings → Client groups**, with their **Allowed user groups**. **Add group**
creates a group with an ID, a description and its allowed user groups. On the **Devices** page, the group picker
filters the list by group.

Which devices belong to a group is defined by its `params`, which you set through the API (below).

## Rules

The `params` of a group decide which devices belong to it. Each parameter takes a list of values:

- an exact value, ignoring case: `"client_id": ["web-01", "web-02"]` selects these two devices;
- a value with `*` wildcards, ignoring case: `"os_family": ["linux*", "*win*"]` selects devices whose OS family starts
  with `linux` or contains `win`.

A device matches a parameter if it matches any of its values. With several parameters, a device must match all of
them:

```json
"params": {
  "tag": ["QA", "my-tag*"],
  "os_family": ["linux*", "ubuntu*"]
}
```

selects devices that have the tag `QA` or a tag starting with `my-tag`, **and** whose OS family starts with `linux`
or `ubuntu`. For a parameter with several values on the device, such as tags or IP addresses, one matching value is
enough.

To require several tags at once, use `and`:

```json
"params": {
  "tag": { "and": ["Linux", "Datacenter 3"] }
}
```

`or` works the same way and is the default for a plain list.

The parameters are `client_id`, `name`, `hostname`, `os`, `os_arch`, `os_family`, `os_kernel`, `ipv4`, `ipv6`,
`tag`, `version` (client version), `address`, `client_auth_id` (the credentials the device connects with) and
`connection_state`.

A group's `client_ids` lists the devices that belong to it. It's read-only.

## Manage groups through the API

The `/client-groups` endpoints create, change, list and delete groups. Changing a group needs an administrator.
Replace `https://procy.sh` with your own server's address if you self-host, and see
[API authentication](/docs/api-and-cli/api-authentication/) for tokens.

### Create a group

```shell
curl -s -u admin@example.com:$TOKEN -X POST https://procy.sh/api/v1/client-groups \
  -H 'Content-Type: application/json' \
  --data-raw '{
    "id": "qa-linux",
    "description": "Linux devices of the QA team",
    "params": {
      "tag": ["QA"],
      "os_family": ["linux*", "ubuntu*"]
    },
    "allowed_user_groups": ["QA"]
  }'
```

### Change a group

A change replaces the description and the allowed user groups. Leave out `params` to keep the group's rules as they
are, or send `"params": {}` to remove them. This change sets all three:

```shell
curl -s -u admin@example.com:$TOKEN -X PUT https://procy.sh/api/v1/client-groups/qa-linux \
  -H 'Content-Type: application/json' \
  --data-raw '{
    "id": "qa-linux",
    "description": "Linux devices of the QA team",
    "params": {
      "tag": ["QA", "my-tag*"],
      "os_family": ["linux*", "ubuntu*"]
    },
    "allowed_user_groups": ["QA"]
  }'
```

### Show a group

```shell
curl -s -u admin@example.com:$TOKEN https://procy.sh/api/v1/client-groups/qa-linux
```

```json
{
  "data": {
    "id": "qa-linux",
    "description": "Linux devices of the QA team",
    "params": {
      "os_family": ["linux*", "ubuntu*"],
      "tag": ["QA", "my-tag*"]
    },
    "allowed_user_groups": ["QA"],
    "client_ids": ["qa-ubuntu-16", "qa-ubuntu-20"]
  }
}
```

The answer lists every parameter; the ones not set are `null` (left out above). `GET /api/v1/client-groups` lists
all groups.

### Delete a group

```shell
curl -s -u admin@example.com:$TOKEN -X DELETE https://procy.sh/api/v1/client-groups/qa-linux
```

The devices themselves aren't affected.
