#!/bin/sh
# Procy client update: installs the latest stable client only if newer, checks it reconnects and rolls back
# otherwise; result /var/lib/rport-update/status.json
set -e
curl -fsS {{server_url}}/pairing/update -o /tmp/rport-update.sh
sh /tmp/rport-update.sh -n -m
rm -f /tmp/rport-update.sh
