---
title: "Permissions model"
weight: 40
slug: "permissions-model"
aliases:
- /get-started/permissions-model/
---
{{< toc >}}

This page explains who can do what in Procy. It applies to hosted Procy and to self-hosted servers. A user needs two
things to work on a device: a **permission** for the function (for example tunnels) from one of their user groups, and
**access to the device**, given per device or through a client group. Administrators need neither.

![The permissions model: administrators can do everything; other users need a function permission from one of their user groups and access to the client, per client or per client group](/docs/images/procy-permissions.svg)

## Administrators

Members of the **Administrators** user group can do everything, on every device. Only administrators manage users,
user groups, client access, client groups, billing, usage and the organization settings.

## Function permissions

A user who isn't an administrator can do nothing until one of their user groups grants permissions. You set them under
**Settings → User groups**, or with the `/user-groups` API (see [Users and user groups](/docs/hosted/users/)).

| Permission | Lets the user |
| --- | --- |
| `commands` | run commands and scripts on devices, and use the script library |
| `tunnels` | open and close tunnels |
| `scheduler` | create and change schedules |
| `uploads` | send files to devices |
| `vault` | read and use the vault |
| `monitoring` | see monitoring data |
| `auditlog` | read the audit log |

A user gets every permission of every group they're in. Permissions only add up: a second group can't take away
what a first one grants.

## Device access

Function permissions say what a user can do, not on which devices. A user also needs access to the device. There are
two ways to give it, and they add up.

### Per device

Open the device and set **Allowed user groups** on its Inventory tab. Members of those groups can reach the device.
Through the API, send the whole list to `POST /api/v1/clients/{client_id}/acl`:

```json
{ "allowed_user_groups": ["Operators", "Support"] }
```

### Per client group

A [client group](/docs/devices/client-groups/) is a named set of devices, chosen by ID or by rules such as tags or the
operating system. Give user groups access to the client group, and its members can reach every device in it,
including devices that join the group later. This is usually the easier way to manage access for many devices.

Set the allowed user groups when you create or edit a client group under **Settings → Client groups**, or in the
`allowed_user_groups` field of the `/client-groups` API.

A device with no allowed user groups, and in no client group that allows any, can be reached by administrators only.

## Example

You want your support team to open remote desktop sessions on customer workstations, but not run commands.

1. Create a user group `Support` with only the `tunnels` permission.
2. Create a client group `workstations` for the devices tagged `workstation`, and allow `Support` on it.
3. Add the support people to `Support`.

They can now open tunnels to the workstations, and nothing else.

## Self-hosted: where permissions are stored

On a self-hosted server, group permissions are stored in the `group_details` table of the
[user database](/docs/self-hosting/user-authentication/#database) (`auth_group_details_table`).

{{< hint type=important title="Deny by default" >}}
Without a `group_details` table, for example with a JSON `auth_file` or a single static user, permissions can't be
evaluated. Then only members of `Administrators` can use these functions, and other users get "403 Forbidden".
`rportd` logs a notice about this at startup.
{{< /hint >}}
