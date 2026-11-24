#!/bin/sh
# Procy server switch: points the client at the server's connect URLs ([server] url), checks it reconnects
# and restores the previous rport.conf otherwise; result /var/lib/rport-update/status.json
set -e
curl -fsS {{server_url}}/pairing/update -o /tmp/rport-update.sh
sh /tmp/rport-update.sh -S
rm -f /tmp/rport-update.sh
