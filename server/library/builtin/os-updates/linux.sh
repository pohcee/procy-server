#!/bin/sh
# Procy OS updates (Linux): installs all pending OS updates and restarts the host if they need it.
# Works alongside automatic updates (unattended-upgrades, dnf-automatic): it waits for their lock and
# installs whatever is still pending. Package holds are respected; local config files are kept.
#   apt (Debian, Ubuntu; full upgrade on Proxmox VE), dnf/yum (RHEL family), zypper (SUSE).
# Restart: when an update needs one, the host restarts 1 minute after this script reports. A host that
# runs the Procy server (rportd) is never restarted automatically. Set REBOOT=never to skip restarts.
# Result: /var/lib/procy-os-updates/last.json
REBOOT=${REBOOT:-auto}
STATE_DIR=/var/lib/procy-os-updates
LOCK_WAIT=600

started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
out=$(mktemp)
trap 'rm -f "$out"' EXIT

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*"; }

result() { # <status> <updated count> <reboot: yes|no|pending> <message>
  mkdir -p "$STATE_DIR"
  esc=$(printf '%s' "$4" | sed 's/\\/\\\\/g; s/"/\\"/g')
  printf '{"status":"%s","updated":%s,"reboot":"%s","started":"%s","finished":"%s","message":"%s"}\n' \
    "$1" "$2" "$3" "$started" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$esc" > "$STATE_DIR/last.json"
  echo "RESULT: $1 - $4"
}

fail() {
  result failed 0 no "$1"
  exit 1
}

[ "$(id -u)" -eq 0 ] || fail "needs root: run this with sudo"

in_container() {
  command -v systemd-detect-virt >/dev/null 2>&1 && systemd-detect-virt -cq
}

# A newer kernel is installed than the one running, or the OS says a restart is needed.
reboot_needed() {
  [ -f /var/run/reboot-required ] && return 0
  case $pm in
    dnf|yum)
      if command -v needs-restarting >/dev/null 2>&1; then
        needs-restarting -r >/dev/null 2>&1 || return 0
      fi
      return 1 ;;
    zypper)
      zypper needs-rebooting >/dev/null 2>&1; [ $? -eq 102 ] && return 0
      return 1 ;;
  esac
  in_container && return 1
  newest=$(ls /lib/modules 2>/dev/null | sort -V | tail -1)
  [ -n "$newest" ] && [ "$newest" != "$(uname -r)" ]
}

runs_procy_server() {
  command -v systemctl >/dev/null 2>&1 && systemctl is-active -q rportd 2>/dev/null
}

# Restarts services that still use replaced libraries (needrestart, Debian/Ubuntu), after this script
# has reported, so the rport client restarting doesn't cut off the result.
restart_services_later() {
  command -v needrestart >/dev/null 2>&1 || return 0
  if command -v systemd-run >/dev/null 2>&1; then
    systemd-run --quiet --on-active=60 --unit "procy-needrestart-$(date +%s)" needrestart -r a -q >/dev/null 2>&1 &&
      log "services that use updated libraries restart in 1 minute"
  fi
}

boot_space_ok() {
  mountpoint -q /boot 2>/dev/null || return 0
  free_kb=$(df -Pk /boot | awk 'NR==2{print $4}')
  [ "${free_kb:-0}" -ge 204800 ]
}

if command -v apt-get >/dev/null 2>&1; then pm=apt
elif command -v dnf >/dev/null 2>&1; then pm=dnf
elif command -v yum >/dev/null 2>&1; then pm=yum
elif command -v zypper >/dev/null 2>&1; then pm=zypper
else fail "no supported package manager (apt, dnf, yum, zypper)"
fi
. /etc/os-release 2>/dev/null
log "OS updates on $(hostname) (${PRETTY_NAME:-unknown OS}, $pm)"
boot_space_ok || fail "less than 200 MB free in /boot; remove old kernels first"

updated=0
case $pm in
  apt)
    export DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1 APT_LISTCHANGES_FRONTEND=none
    APT="apt-get -q -y -o DPkg::Lock::Timeout=$LOCK_WAIT -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold"
    # Finish an install that was interrupted earlier.
    dpkg --configure -a >"$out" 2>&1 || { cat "$out"; fail "dpkg --configure -a failed"; }
    n=0
    until apt-get -q -o DPkg::Lock::Timeout=$LOCK_WAIT update >"$out" 2>&1; do
      n=$((n + 1))
      if [ $n -ge 3 ]; then cat "$out"; fail "apt-get update failed (see above)"; fi
      log "apt-get update failed, retrying in 60s"; sleep 60
    done
    grep -E '^(W|E):' "$out"
    if command -v pveversion >/dev/null 2>&1; then
      mode=dist-upgrade # Proxmox VE requires a full upgrade
    else
      mode="upgrade --with-new-pkgs"
    fi
    pending=$(apt-get -s $mode 2>/dev/null | grep -c '^Inst ')
    log "$pending package(s) to update ($mode)"
    if [ "$pending" -gt 0 ]; then
      apt-get -s $mode 2>/dev/null | awk '/^Inst /{print "  " $2}' | head -100
      # shellcheck disable=SC2086
      $APT $mode >"$out" 2>&1 || { tail -40 "$out"; fail "apt-get $mode failed (see above)"; }
      grep -E '^[0-9]+ upgraded' "$out"
      updated=$pending
    fi
    held=$(apt-mark showhold 2>/dev/null | tr '\n' ' ')
    [ -n "$held" ] && log "held (not updated): $held"
    ;;
  dnf|yum)
    pending=$($pm -q check-update 2>/dev/null | awk 'NF==3' | wc -l)
    log "$pending package(s) to update"
    if [ "$pending" -gt 0 ]; then
      $pm -y upgrade >"$out" 2>&1 || { tail -40 "$out"; fail "$pm upgrade failed (see above)"; }
      tail -5 "$out"
      updated=$pending
    fi
    ;;
  zypper)
    zypper -n --gpg-auto-import-keys refresh >"$out" 2>&1 || { tail -20 "$out"; fail "zypper refresh failed"; }
    pending=$(zypper -n -q list-updates 2>/dev/null | grep -c '^v ')
    log "$pending package(s) to update"
    if [ "$pending" -gt 0 ]; then
      zypper -n update --auto-agree-with-licenses >"$out" 2>&1 || { tail -40 "$out"; fail "zypper update failed (see above)"; }
      updated=$pending
    fi
    ;;
esac

if reboot_needed; then
  if [ "$REBOOT" = never ]; then
    result success "$updated" pending "updated $updated package(s); a restart is needed (REBOOT=never)"
  elif runs_procy_server; then
    result success "$updated" pending "updated $updated package(s); a restart is needed but this host runs the Procy server, so restart it by hand"
  else
    shutdown -r +1 "Procy OS updates: restarting to finish installing updates" >/dev/null 2>&1 ||
      fail "updated $updated package(s) but scheduling the restart failed"
    result success "$updated" yes "updated $updated package(s); restarting in 1 minute"
  fi
else
  restart_services_later
  result success "$updated" no "updated $updated package(s); no restart needed"
fi
exit 0
