---
title: "Users"
weight: 12
slug: users
aliases:
  - /docs/no12-users.html
---
{{< toc >}}
Procy API users are stored in a JSON file or a database, as described in
[API authentication](/docs/get-started/api-authentication/#storing-credentials-managing-users).

You can manage users with the [REST API](https://procy.sh/docs/api/).

## API Limitations

Before using the [User Management API](https://procy.sh/docs/api/), you should provide at least one user
belonging `Administrators` group either in a JSON file or DB.

If `rportd` is started with a [single static user](/docs/get-started/api-authentication/#hardcoded-single-user), the
user management API can't be used.

If `rportd` is started with JSON file credentials, changes to the users list won't be refreshed until `rportd` is restarted
since there is a [limitation](/docs/get-started/api-authentication/#user-file).

## API Usage

The `/users` endpoints allow you to create, update, delete and list users and add or remove users to/from groups.

As listed in the API docs Users are defined by `username`

### Create

Passwords are hashed before they are stored. They need at least 14 characters (`password_min_length`) and must pass a
check against common words and patterns (`password_zxcvbn_minscore`).

```shell
curl -X POST 'http://localhost:3000/api/v1/users' \
-u admin:$PASSWORD \
-H 'Content-Type: application/json' \
--data-raw '{
    "username": "user1",
    "password": "<NEW_PASSWORD>",
    "groups":
    [
        "Users",
        "Administrators"
    ]
}'
```

### Update

You can provide any parameters that you want to change.

```shell
curl -X PUT 'http://localhost:3000/api/v1/users/user1' \
-u admin:$PASSWORD \
-H 'Content-Type: application/json' \
--data-raw '{
    "password": "<NEW_PASSWORD>",
    "groups":
    [
        "Users"
    ]
}'
```

This will change password and remove user from Administrators group. To add user to a new group, you should provide all current user groups + a new one e.g.

```json
{
    "groups":
    [
        "Users",
        "Administrators",
        "New Group"
    ]
}
```

Please note that all changes to the user affecting credentials will have an immediate effect in most cases disregard if
you use JWT or basic password auth (e.g. deletion user from Administrators group), so you should use this API carefully.
If you change a password, user will still be able to login with an old JWT token, so the change will work till the next login.

### List all users

```shell
curl -s -u admin:$PASSWORD http://localhost:3000/api/v1/users
```

```json
{
    "data": [
        {
            "username": "admin",
            "groups": [
                "Administrators",
                "Users"
            ]
        },
        {
            "username": "root",
            "groups": [
                "Users"
            ]
        }
    ]
}
```

Because of security consideration, users list won't return hashed passwords.

### Delete

```shell
curl -u admin:$PASSWORD -X DELETE 'http://localhost:3000/api/v1/users/user1'
```

Deleting a user also revokes their API sessions and API tokens right away.
