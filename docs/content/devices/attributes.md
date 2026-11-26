---
title: "Tags and labels"
weight: 40
slug: attributes
aliases:
- /advanced/attributes/
  - /docs/no11-attributes.html
---
{{< toc >}}

Tags and labels describe your devices so you can find, filter and group them. This page shows how to set them in the
web interface, on the device and through the API. It applies to hosted Procy and to self-hosted servers.

- **Tags** are single words: `["win", "server", "vm"]`.
- **Labels** are key-value pairs: `{"country": "France", "city": "Lille", "datacenter": "OVH"}`.

Both are stored on the device, in its **attributes file**, and the device sends them to the server when it connects.
Use them in [client group](/docs/devices/client-groups/) rules, for example to give a team access to every device
tagged `workstation`.

## In the web interface

Open the device, go to the **Inventory** tab and choose **Edit** on the **Tags & labels** card. You can add, change
and remove tags and labels there. The device must be online, because the change is written to the device's
attributes file.

Devices installed with the [installer](/docs/devices/install-and-run-a-client/) already have an attributes file, with
their city and country as labels when the installer could look them up.

## On the device

The attributes file is a JSON file set with `attributes_file_path` in the `[client]` section of `rport.conf`. The
installer uses `/var/lib/rport/client_attributes.json` on Linux and `C:\Program Files\rport\client_attributes.json`
on Windows.

```toml
[client]
  attributes_file_path = "/var/lib/rport/client_attributes.json"
```

```json
{
  "tags": ["win", "server", "vm"],
  "labels": {
    "country": "France",
    "city": "Lille",
    "datacenter": "OVH"
  }
}
```

The client reads the file when it starts. After you edit it by hand, restart the client.

For changes from the web interface or the API to work, the client's user must be able to write the file.

Without an attributes file you can set tags, but not labels, with `tags = ['win', 'server']` in the `[client]`
section of `rport.conf`. These tags can't be changed remotely, and `attributes_file_path` takes precedence when both
are set.

## Through the API

Send the complete new tags and labels to `PUT /api/v1/clients/{client_id}/attributes`. Partial updates aren't
supported:

```shell
curl -s -u admin@example.com:$TOKEN -X PUT https://procy.sh/api/v1/clients/web-01/attributes \
  -H 'Content-Type: application/json' \
  --data-raw '{"tags": ["linux", "web"], "labels": {"city": "Lille"}}'
```

The device must be connected and have a writable attributes file. Otherwise the server refuses the change.

## Filtering

Filter the device list by tags and labels with `filter[tags]` and `filter[labels]`. Labels are matched as
`key: value`. `*` works as a wildcard:

```text
GET /api/v1/clients?filter[tags]=server
GET /api/v1/clients?filter[tags]=ser*
GET /api/v1/clients?filter[tags]=and(Linux,Datacenter 4)
GET /api/v1/clients?filter[labels]=city: Cologne
GET /api/v1/clients?filter[labels]=*: Cologne
```

URL-encode spaces as `%20`. `GET /api/v1/client-tags` lists every tag with the devices that have it. See the
[API reference](/docs/api/) for all filters.
