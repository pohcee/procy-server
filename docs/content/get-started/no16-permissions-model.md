---
title: "Permissions Model"
weight: 16 
slug: "permissions-model"
---
{{< toc >}}

## Admin users

Members of the 'Administrators' user group bypass the below permission model completely. They have full access to all
hosts and all functions.

## User group permissions aka. function permissions

A none-admin user has no effective rights in Procy unless via at least one user group, permissions are granted.
Currently, Procy is subdivided into the following functions:

* tunnels
* scripts
* commands
* vault
* scheduler
* monitoring
* uploads
* auditlog

The permissions are stored on the `group_details` table of
your [API access database](/get-started/api-authentication/#database). They are managed through
the [update user groups API endpoint](https://procy.sh/docs/api/).

Without a `group_details` table (`auth_group_details_table`), for example with a JSON `auth_file` or a single user,
permissions can't be evaluated. In that case only members of `Administrators` can use these functions; other users
are denied.

In addition to one of the above function permissions client permissions are needed. In other words, the function
permissions define only what a user can do, but not on which clients he/she can do it.

{{< hint type=caution title="Permissions are additive only">}} There is no option to revoke a permission. Once a user
group has a permission, you cannot revoke it through a second user group. {{< /hint >}}

## Client permissions

A user that has at least one of the above function permission needs at least access to one client to effectively use
Procy.

### Per client

You can grant access to a single client to one or many user groups. The allowed user groups are stored on the clients'
table inside the `details` object. The so-called client ACLs are managed through
the [client ACL API endpoint](https://procy.sh/docs/api/)

### Client group permissions

You can grant access to a client group to one or many user groups. This makes managing access rights effective and
flexible.

Client group access is managed through
the [client groups API endpoint](https://procy.sh/docs/api/). The underlying data is stored on
the `client_groups` database.

{{< hint type=caution title="Permissions are additive only">}} There is no option to revoke a permission. If permission
is granted to client group A, you cannot deny access to client group B if it is a subset of A.  
{{< /hint >}}
