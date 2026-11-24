#!/bin/sh
#
# Procy client update supervisor. Written and started by the update script, detached from the rport
# client (systemd-run or setsid), because restarting the client kills everything it started.
#
# It installs the staged client, restarts the service and checks that the new client connects to the
# server. If it does not, it restores the previous binary and config and restarts again, so a failed
# update never leaves the machine unreachable.
#
# Settings come from the environment or from the file given as $1:
#   STAGE          root-only work dir holding previous/ (backup) and new/ (the staged client)
#   RPORT_BIN      path of the binary the service runs
#   CONFIG_FILE    rport.conf
#   LOG_FILE       client log file ("" = read the systemd journal)
#   FROM_VERSION, TO_VERSION
#   CHANGE         what is changed, for the messages (default "$FROM_VERSION to $TO_VERSION")
#   PKG_KIND       tarball (default), deb or rpm; deb/rpm install $STAGE/new/rport.<kind>
#   HEALTH_TIMEOUT seconds to wait for a connection (default 120)
#   STABLE_SECS    seconds the client must keep running after connecting (default 20)
#   SERVICE_CMD    optional command taking start|stop|status instead of the service manager
#   HANDOVER_DELAY seconds to wait before stopping the client, so it can report the result of the
#                  command that started the update (default 5)
#
# Result: $STAGE/status.json ({"status": running|success|rolled_back|rollback_failed, ...}) and
# $STAGE/update.log.
#
# shellcheck disable=SC1090
[ -n "$1" ] && . "$1"
set -u
HEALTH_TIMEOUT=${HEALTH_TIMEOUT:-120}
STABLE_SECS=${STABLE_SECS:-20}
PKG_KIND=${PKG_KIND:-tarball}
SERVICE_CMD=${SERVICE_CMD:-}
LOG_FILE=${LOG_FILE:-}
HANDOVER_DELAY=${HANDOVER_DELAY:-5}
CHANGE=${CHANGE:-"$FROM_VERSION to $TO_VERSION"}
STARTED=$(date -u +%Y-%m-%dT%H:%M:%SZ)
ULOG="$STAGE/update.log"

log() {
    echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) $*" >>"$ULOG"
    command -v logger >/dev/null 2>&1 && logger -t rport-update "$*"
    true
}

# status <state> <message>; messages must not contain double quotes or backslashes
status() {
    finished=""
    [ "$1" != running ] && finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    cat >"$STAGE/status.json.tmp" <<EOF
{"status": "$1", "from": "$FROM_VERSION", "to": "$TO_VERSION", "started": "$STARTED", "finished": "$finished", "message": "$2"}
EOF
    chmod 0644 "$STAGE/status.json.tmp"
    mv -f "$STAGE/status.json.tmp" "$STAGE/status.json"
}

svc() {
    if [ -n "$SERVICE_CMD" ]; then
        "$SERVICE_CMD" "$1"
    elif command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
        case "$1" in
        status) systemctl is-active --quiet rport ;;
        *) systemctl "$1" rport ;;
        esac
    elif [ -x /etc/init.d/rport ]; then
        case "$1" in
        status) pidof rport >/dev/null 2>&1 ;;
        *) /etc/init.d/rport "$1" ;;
        esac
    else
        case "$1" in
        status) pidof rport >/dev/null 2>&1 ;;
        *) service rport "$1" ;;
        esac
    fi
}

log_size() {
    if [ -n "$LOG_FILE" ] && [ -f "$LOG_FILE" ]; then
        wc -c <"$LOG_FILE" | tr -d ' '
    else
        echo 0
    fi
}

# True if the client logged a connection to the server after log offset $1 / time $2.
connected_since() {
    if [ -n "$LOG_FILE" ] && [ -f "$LOG_FILE" ]; then
        size=$(log_size)
        if [ "$size" -lt "$1" ]; then
            # rotated or truncated: the whole file is new
            grep -q "Connected to " "$LOG_FILE"
        else
            tail -c +"$(($1 + 1))" "$LOG_FILE" | grep -q "Connected to "
        fi
    else
        journalctl -u rport --since "@$2" -o cat 2>/dev/null | grep -q "Connected to "
    fi
}

# wait_healthy <log offset> <start time>: the service runs, connects within HEALTH_TIMEOUT and
# keeps running for STABLE_SECS after that.
wait_healthy() {
    waited=0
    while [ "$waited" -lt "$HEALTH_TIMEOUT" ]; do
        if connected_since "$1" "$2"; then
            sleep "$STABLE_SECS"
            if svc status; then
                return 0
            fi
            HEALTH_ERROR="the client connected but stopped within ${STABLE_SECS}s"
            return 1
        fi
        sleep 1
        waited=$((waited + 1))
    done
    if svc status; then
        HEALTH_ERROR="the client is running but did not connect to the server within ${HEALTH_TIMEOUT}s"
    else
        HEALTH_ERROR="the client did not start or did not connect to the server within ${HEALTH_TIMEOUT}s"
    fi
    return 1
}

install_new() {
    case "$PKG_KIND" in
    deb) DEBIAN_FRONTEND=noninteractive apt-get --yes -o Dpkg::Options::="--force-confold" install "$STAGE/new/rport.deb" >>"$ULOG" 2>&1 ;;
    rpm) rpm -U --force "$STAGE/new/rport.rpm" >>"$ULOG" 2>&1 ;;
    *)
        cp "$STAGE/new/rport" "$RPORT_BIN.new" && chmod 0755 "$RPORT_BIN.new" && mv -f "$RPORT_BIN.new" "$RPORT_BIN"
        ;;
    esac
}

restore_previous() {
    # A package install may have moved the binary; put the previous one back where it was.
    cp "$STAGE/previous/rport" "$RPORT_BIN.rollback" && chmod 0755 "$RPORT_BIN.rollback" && mv -f "$RPORT_BIN.rollback" "$RPORT_BIN"
    cp "$STAGE/previous/rport.conf" "$CONFIG_FILE"
    if [ -f "$STAGE/previous/rport.service" ]; then
        cp "$STAGE/previous/rport.service" /etc/systemd/system/rport.service
        systemctl daemon-reload
    fi
}

recent_output() {
    if [ -n "$LOG_FILE" ] && [ -f "$LOG_FILE" ]; then
        tail -c +"$(($1 + 1))" "$LOG_FILE" | tail -n 20
    else
        journalctl -u rport --since "@$2" -n 20 --no-pager 2>/dev/null
    fi
}

log "update started: $CHANGE ($PKG_KIND)"
status running "updating $CHANGE"

if [ "$HANDOVER_DELAY" -gt 0 ]; then
    log "waiting ${HANDOVER_DELAY}s so the client can report the result of the update command"
    sleep "$HANDOVER_DELAY"
fi
offset=$(log_size)
t0=$(date +%s)
svc stop >>"$ULOG" 2>&1 || log "stopping the client failed, continuing"
if ! install_new; then
    log "installing $TO_VERSION failed"
    restore_previous
    svc start >>"$ULOG" 2>&1
    status rolled_back "installing $TO_VERSION failed; kept $FROM_VERSION"
    exit 1
fi
svc start >>"$ULOG" 2>&1 || log "starting the new client returned an error"
HEALTH_ERROR=""
if wait_healthy "$offset" "$t0"; then
    log "update to $TO_VERSION succeeded: the client is connected"
    status success "updated $CHANGE; the client is connected"
    exit 0
fi

log "update to $TO_VERSION failed: $HEALTH_ERROR. Last client output:"
recent_output "$offset" "$t0" >>"$ULOG" 2>&1
FIRST_ERROR=$HEALTH_ERROR
log "rolling back to $FROM_VERSION"
svc stop >>"$ULOG" 2>&1
restore_previous
offset=$(log_size)
t0=$(date +%s)
svc start >>"$ULOG" 2>&1
if wait_healthy "$offset" "$t0"; then
    log "rollback to $FROM_VERSION succeeded: the client is connected"
    status rolled_back "$TO_VERSION failed ($FIRST_ERROR); rolled back to $FROM_VERSION, which is connected"
    exit 1
fi
log "rollback to $FROM_VERSION did not reconnect: $HEALTH_ERROR"
status rollback_failed "$TO_VERSION failed ($FIRST_ERROR) and $FROM_VERSION did not reconnect after the rollback: $HEALTH_ERROR"
exit 2
