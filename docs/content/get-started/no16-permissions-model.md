---
title: "Permissions Model"
weight: 16
slug: "permissions-model"
---
{{< toc >}}

![The permissions model: administrators can do everything; other users need a function permission from one of their user groups and access to the client, per client or per client group](https://raw.githubusercontent.com/pohcee/openrport/main/docs/static/images/procy-permissions.svg)

## Admin users

Members of the 'Administrators' user group bypass the below permission model completely. They have full access to all
hosts and all functions.

## User group permissions aka. function permissions

A user who isn't an administrator has no rights in Procy until at least one of their user groups grants permissions.
Procy has the following functions:

* tunnels
* scripts
* commands
* vault
* scheduler
* monitoring
* uploads
* auditlog

The permissions are stored in the `group_details` table of
your [API access database](/docs/get-started/api-authentication/#database). They are managed through
the [update user groups API endpoint](https://procy.sh/docs/api/).

{{< hint type=important title="Deny by default" >}}
Without a `group_details` table (`auth_group_details_table`), for example with a JSON `auth_file` or a single user,
permissions can't be evaluated. In that case only members of `Administrators` can use these functions; other users
get "403 Forbidden". `rportd` logs a notice about this at startup. To give other users rights, store users in a
[database](/docs/get-started/api-authentication/#database) with a `group_details` table.
{{< /hint >}}

In addition to one of the above function permissions, client permissions are needed. The function permissions define
only what a user can do, not on which clients they can do it.

{{< hint type=caution title="Permissions are additive only">}} There is no option to revoke a permission. Once a user
group has a permission, you cannot revoke it through a second user group. {{< /hint >}}

## Client permissions

A user with function permissions also needs access to at least one client to use Procy.

### Per client

You can grant access to a single client to one or many user groups. The allowed user groups are stored on the clients'
table inside the `details` object. The so-called client ACLs are managed through
the [client ACL API endpoint](https://procy.sh/docs/api/).

### Client group permissions

You can grant access to a client group to one or many user groups. This makes managing access rights effective and
flexible.

Client group access is managed through
the [client groups API endpoint](https://procy.sh/docs/api/). The underlying data is stored on
the `client_groups` database. See [client groups](/docs/get-started/client-groups/).

{{< hint type=caution title="Permissions are additive only">}} There is no option to revoke a permission. If permission
is granted to client group A, you cannot deny access to client group B if it is a subset of A.  
{{< /hint >}}
