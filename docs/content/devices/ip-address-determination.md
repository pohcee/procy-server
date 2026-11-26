---
title: "External IP address"
weight: 80
slug: "ip-address-determination"
aliases:
- /advanced/ip-address-determination/
---
{{< toc >}}

Procy can show the public IPv4 and IPv6 addresses each device uses on the internet. The client finds them out by
asking an external "what is my IP" service. This works even when the server sits behind a load balancer or reverse
proxy, as on hosted Procy, and only sees the proxy's address. It applies to hosted Procy and to self-hosted servers.

The feature is off by default, for privacy:

- it causes some extra traffic;
- the service you ask can log the requests and so learn where your devices are. The client sends no user agent, so the
  requests don't identify Procy at first sight.

## Turn it on

Set `ip_api_url` in the `[client]` section of `rport.conf` and restart the client:

```toml
[client]
  ip_api_url = "https://api.seeip.org/jsonip"
  ## How often to check, in minutes. Default 30.
  #ip_refresh_min = 30
```

The addresses appear as `ext_ip_addresses` in the device's data in the API. The web interface doesn't show them yet.

## Which services work

Any service that answers both IPv4 and IPv6 requests with JSON like this works:

```json
{"ip": "<IP ADDRESS>"}
```

Other fields in the answer are ignored. These are known to work:

- `https://api.seeip.org/jsonip`
- `https://api64.ipify.org?format=json`

{{< hint type=note title="IPv4 and IPv6" >}}
Check that a service answers over both before you use it: `curl -4 <URL>` and `curl -6 <URL>`.
{{< /hint >}}

## Run your own service

Public services can disappear, or block you after too many requests. To avoid both, run your own on any web server.
This PHP script is all it takes:

```php
<?php
header('Content-Type: application/json; charset=utf-8');
echo json_encode(['ip' => $_SERVER['REMOTE_ADDR']]);
```

To protect it with HTTP basic authentication, put the user and password in the URL:
`ip_api_url = "https://user:password@ip.example.com"`.
