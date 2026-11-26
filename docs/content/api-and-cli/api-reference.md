---
title: "API reference"
weight: 30
slug: api-reference
---

Everything the web interface does goes through the Procy REST API, so you can automate it too. The full reference of
endpoints, parameters and responses is generated from the OpenAPI description:

{{< button href="/docs/api/" >}}Open the API reference{{< /button >}}

## Basics

**Base URL**
: `https://procy.sh/api/v1` on hosted Procy. On a self-hosted server, `https://<your server>/api/v1`.

**Authentication**
: HTTP basic authentication with your username and an [API token](/docs/api-and-cli/api-authentication/). On hosted
  Procy the username is the email address you sign in with. A token acts in the organization it was created in.

**Format**
: Requests and responses are JSON, except file uploads (multipart form). Results are in `data`, lists also carry
  `meta.count`; errors are a list in `errors`, each with `code`, `title` and `detail`.

**Lists**
: Most list endpoints take `sort=<field>` (`-<field>` for descending), `filter[<field>]=<value>` (comma-separate
  values to match any of them) and `page[limit]` / `page[offset]`. With curl, pass `-g` so the brackets are sent as
  they are.

**Live output**
: Commands, scripts, library entries and uploads also have WebSocket endpoints under `/api/v1/ws/` that stream
  results as devices report them.

**Limits**
: Request bodies are limited to 10 KB (file uploads to 10 MB) by default. After several failed sign-ins the server
  answers `429` for a while.

```shell
curl -s -u 'you@example.com:<api-token>' https://procy.sh/api/v1/clients | jq '.data[].name'
```

## Where to start

| Task | Guide |
| --- | --- |
| Open, list and close tunnels | [Managing tunnels](/docs/remote-access/managing-tunnels/#manage-tunnels-through-the-api) |
| Run commands on devices | [Running commands](/docs/automation/command-execution/#run-through-the-api) |
| Library entries and scripts | [Scripts and the library](/docs/automation/scripts/#library-entries-through-the-api) |
| Schedules | [Schedules](/docs/automation/schedules/#schedules-through-the-api) |
| Send files | [Sending files](/docs/automation/file-reception/#send-a-file-through-the-api) |
| Vault entries | [Vault](/docs/automation/vault/#use-the-vault-through-the-api) |
| From the shell | [procy-cli](/docs/api-and-cli/cli/) |
