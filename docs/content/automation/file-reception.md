---
title: "Sending files"
weight: 40
slug: file-reception
aliases:
- /advanced/file-reception/
- /docs/no18-file-reception.html
---
{{< toc >}}

Copy a file from your computer to one device or many through the Procy server, for example to distribute software
or configuration files. This works on hosted Procy and on self-hosted servers.

{{< mermaid class="text-center">}}
flowchart TD
    1[Your browser, procy-cli or curl]-->|upload over HTTPS|2[Procy server]
    2-->|SFTP over the device's connection|3[Device]
    2-->|SFTP over the device's connection|4[Device]
    2-->|SFTP over the device's connection|5[Device]
{{< /mermaid >}}

You upload the file once. The server keeps it in a temporary folder and tells each device about it over the
connection the device already holds. Each device downloads it with SFTP over that connection, checks its MD5
checksum, sets the mode and owner if asked, and moves it to the destination. The server then deletes its copy.

## Before you start

* The device must accept files. When you install a client from the web interface, tick **Enable file transfer**
  (installer option `-r`). To let the device write anywhere and change file owners on Linux, also tick **Enable file
  transfer sudo rights** (`-b`); see [where files may go](#where-files-may-go).
* Your user group needs the **uploads** permission (see [permissions](/docs/hosted/permissions-model/)).
* Files are limited to 10 MB by default (`[api] max_filepush_size` on self-hosted servers). On hosted Procy, uploads
  count toward your organization's transfer, and new uploads are refused at the
  [hard transfer limit](/docs/hosted/billing-and-usage/#when-you-reach-the-transfer-allowance).

## Send a file from the web interface

Open the device's page, tab **Files**, **Upload a file**:

1. **Local file**: choose the file.
2. **Remote path**: the folder and file name on the device, such as `/tmp/test.txt` or `C:\Windows\temp\test.txt`.
3. Optionally:
   * **Force overwrite**: replace the file if it already exists. Without it, an existing file is left alone.
   * **Sync**: if the file exists, replace it only when its content differs, and apply the mode and owner below.
   * **Mode (optional)**, **User (optional)**, **Group (optional)** (Unix only): the file's permissions, such as
     `0644`, and owner. By default the file belongs to the user and group that run the Procy client.
4. Upload.

## Send a file through the API

`POST /files` takes a multipart form (not JSON). The file goes in the `upload` part; the other parts set the targets
and options:

```shell
curl -s -u 'you@example.com:<api-token>' -X POST https://procy.sh/api/v1/files \
  -F 'upload=@./nginx.conf' \
  -F 'client_id=89C4AB76-D90A-555C-85BF-9F8770A3036F' \
  -F 'client_id=b4f795ef-718f-4f69-8f6f-b304b38a904f' \
  -F 'dest=/etc/nginx/nginx.conf' \
  -F 'sync=true' \
  -F 'mode=0644' \
  -F 'user=root' \
  -F 'group=root'
```

`upload` (required)
: The file.

`dest` (required)
: The absolute path, including the file name, on the device. Paths under `/proc/`, `/sys/`, `/dev/` and `/run/`
  are refused.

`client_id`, `group_id`
: A device or [client group](/docs/devices/client-groups/) to send to. Repeat the part for more.

`tags`
: Instead of IDs, send to devices by tag, as JSON: `{"tags": ["linux"], "operator": "AND"}`.

`force`
: `true` overwrites an existing file. Default `false`: an existing file is skipped.

`sync`
: `true`: if the file exists, overwrite it only when the MD5 checksums differ, and apply `mode`, `user` and `group` to
  it. A missing file is created as usual.

`mode` (Unix only)
: The file mode in octal, such as `0644`. Default `0764`.

`user`, `group` (Unix only)
: The owner and group. Default: the user that runs the Procy client and its group.

The server answers once it has received the file. That doesn't mean every device has it yet.

### Follow the results

Each device reports success or failure. To follow them live, open the WebSocket `/api/v1/ws/uploads`; it delivers
messages for all uploads that happen while it is open, for example:

```json
{
  "client_id": "89C4AB76-D90A-555C-85BF-9F8770A3036F",
  "uuid": "482ae29e-d372-4d21-8cb4-58d75482b7e1",
  "filepath": "/etc/nginx/nginx.conf",
  "size": 17118,
  "message": "file successfully copied to destination",
  "status": "success"
}
```

`uuid` matches the ID the upload request returned. Errors come in the same form with `"status": "error"` and the
reason in `message`. The server doesn't keep these messages, but the **Audit log** records each upload and its
result per device.

**Self-hosted:** to try the WebSocket in a browser, set `enable_ws_test_endpoints = true` in `[server]`, restart
`rportd` and open `/api/v1/test/uploads/ui`.

## Where files may go

On Linux the Procy client runs as the unprivileged `rport` user, so it can write only where that user may. Moving a
file into a protected folder or changing its owner needs sudo rules. The installer option **Enable file transfer
sudo rights** (`-b`) creates these:

```text
# /etc/sudoers.d/rport-filereception
# Lets the rport client change the owner of files it received from the server
rport ALL=NOPASSWD: /usr/bin/chown * /var/lib/rport/filepush/*_rport_filepush
# Lets the rport client move received files to any folder
rport ALL=NOPASSWD: /usr/bin/mv /var/lib/rport/filepush/*_rport_filepush *
```

Narrow them if the device should only receive files in some folders.

Whatever the sudo rules allow, the client refuses to write to the folders in `[file-reception] protected` of its
`rport.conf`. The list holds [glob patterns](https://en.wikipedia.org/wiki/Glob_(programming)) and applies to the
folder and to the file path:

```toml
[file-reception]
  ## Receive files pushed by the server.
  enabled = true
  ## Linux default
  protected = ['/bin', '/sbin', '/boot', '/usr/bin', '/usr/sbin', '/dev', '/lib*', '/run']
  ## Windows default
  #protected = ['C:\Windows\', 'C:\ProgramData']
```

For example, `/bin` refuses `/bin/yourfile.txt` but not `/etc/bin/yourfile.txt`, and `C:\Windows\*.exe` refuses
`C:\Windows\program.exe` but not `C:\Windows\program.txt`.

To stop a device from receiving files, set `[file-reception] enabled = false` and restart the client.
