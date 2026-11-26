---
title: "Email and 2FA messages"
weight: 90
slug: 2fa-messaging
aliases:
- /get-started/2fa-messaging/
- /docs/no15-messaging.html
- /docs/content/get-started/no15-messaging.md
---
{{< toc >}}

Set up how a self-hosted server sends messages: email through SMTP, push messages through Pushover, or your own
script or web service. On [procy.sh](https://procy.sh) this is done for you.

The server sends messages for:

* [two-factor authentication](/docs/self-hosting/user-authentication/#two-factor-auth): the verification code sent at
  login, through the method chosen with `two_fa_token_delivery` in `[api]`;
* on [Postgres](/docs/self-hosting/postgres/): signup and email-confirmation links,
  [deletion notices](/docs/self-hosting/multi-tenancy/#leaving) and [billing notices](/docs/self-hosting/payment-provider/),
  always by email.

## SMTP

```text
[smtp]
  server = 'smtp.example.com:587'
  sender_email = 'procy@example.com'
  auth_username = 'john.doe'
  auth_password = 'secret'
  secure = false
```

`server`
: The SMTP server and port, separated by a colon. Required.

`sender_email`
: The sender address of the server's emails. Required.

`auth_username`, `auth_password`
: Credentials, if the server needs them.

`secure`
: `true` for implicit TLS (usually port 465). With `false`, STARTTLS is used whenever the server offers it (usually
  port 587).

`rportd` connects to the SMTP server at start-up to check the settings, so it must be reachable then.

To send 2FA codes by email, set `two_fa_token_delivery = 'smtp'` in `[api]`.

## Pushover

[Pushover](https://pushover.net) delivers push messages to phones and desktops.

1. [Sign up](https://pushover.net/signup) for an account, and [create an application](https://pushover.net/apps/build)
   to get an API token.
2. Add both to `rportd.conf`:

    ```text
    [pushover]
      api_token = "afapzrcv5801jeaw71b4odjyn1m2e5"
      user_key = "pgcjszdyures33k4m4e12e9ggc1syo"
    ```

3. Set `two_fa_token_delivery = 'pushover'` in `[api]`.
4. Receive the messages with one of the [Pushover apps](https://pushover.net/clients).

## Script

Send 2FA codes with your own script, for example through Telegram or another messenger. In `[api]`, set
`two_fa_token_delivery` to the full path of an executable:

```text
two_fa_token_delivery = '/usr/local/bin/2fa-sender'
```

The code and the recipient are passed in environment variables. To see them, create `/usr/local/bin/2fa-sender`,
make it executable with `chmod +x`, and log in:

```bash
#!/bin/bash
date > /tmp/2fa-sender.txt
printenv >> /tmp/2fa-sender.txt
```

```text
RPORT_2FA_SENDTO=email@example.com
RPORT_2FA_TOKEN_TTL=600
RPORT_2FA_TOKEN=7SM7j2
RPORT_2FA_USER_AGENT='Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:99.0) Gecko/20100101 Firefox/99.0'
RPORT_2FA_REMOTE_ADDRESS=::1
```

`RPORT_2FA_SENDTO` is the user's 2FA address (the `two_fa_send_to` field of the user store). If your script needs a
particular format, make the API check it when users change it: `two_fa_send_to_type = 'email'` accepts only email
addresses, and `two_fa_send_to_type = 'regex'` with `two_fa_send_to_regex` accepts what matches the expression.

If the script exits with a code other than `0`, the login request gets HTTP 500 with the script's STDERR output.

{{< hint type=tip >}}
When the script hands the code to a web service with curl, use `curl -f`: curl then exits non-zero on any HTTP error,
and the server reports the failure.
{{< /hint >}}

### Telegram example

[Create a bot](https://core.telegram.org/bots#6-botfather) first and copy its token. Users enter their Telegram chat id
as their 2FA address.

```bash
#!/bin/sh
BOT_TOKEN="<YOUR_BOT_TOKEN>"
URL="https://api.telegram.org/bot${BOT_TOKEN}/sendMessage"
curl -fs -X POST $URL \
  -d chat_id=$RPORT_2FA_SENDTO \
  -d text="Your Procy 2FA token: $RPORT_2FA_TOKEN (valid for $RPORT_2FA_TOKEN_TTL seconds)"
```

## URL

Send 2FA codes to your own web service. In `[api]`, set `two_fa_token_delivery` to its URL; `base_url` must be set
too:

```text
two_fa_token_delivery = 'https://2fa-sender.example.com/send'
```

The server makes a form-encoded `POST` with the fields `email` (the user's 2FA address, which must be an email
address), `token`, `ttl` (seconds), `user_agent`, `remote_address` and `url` (the server's `base_url`). Any answer
other than 2xx fails the login.
