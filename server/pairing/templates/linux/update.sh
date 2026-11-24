set -e
# Updates are installed by a supervisor running detached from the rport client (see
# update_supervisor.sh). It keeps the previous binary and config, checks that the new client
# connects to the server and rolls back if it does not.
UPDATE_STAGE=/var/lib/rport-update
HEALTH_TIMEOUT=${HEALTH_TIMEOUT:-120}
STABLE_SECS=${STABLE_SECS:-20}

#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  current_binary
#   DESCRIPTION:  Print the path of the binary the service runs: /usr/bin/rport for package installs, else /usr/local/bin/rport
#----------------------------------------------------------------------------------------------------------------------
current_binary() {
    if [ -e /usr/bin/rport ]; then
        echo /usr/bin/rport
    elif [ -e /usr/local/bin/rport ]; then
        echo /usr/local/bin/rport
    else
        throw_fatal "No rport client found in /usr/bin or /usr/local/bin. Nothing to update." >&2
    fi
}

current_version() {
    "$(current_binary)" --version | awk '{print $2}'
}

# The client's log file from rport.conf; empty if it logs to stdout (the systemd journal).
client_log_file() {
    sed -n "s/^[[:space:]]*log_file[[:space:]]*=[[:space:]]*[\"']\([^\"']*\)[\"'].*/\1/p" "$CONFIG_FILE" | head -n 1
}

#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  prepare_stage
#   DESCRIPTION:  Create the root-only work dir and back up the current binary and config, before anything is changed
#----------------------------------------------------------------------------------------------------------------------
prepare_stage() {
    if [ -f "${UPDATE_STAGE}/status.json" ] && grep -q '"status": "running"' "${UPDATE_STAGE}/status.json" &&
        pgrep -f "${UPDATE_STAGE}/supervise.sh" >/dev/null 2>&1; then
        throw_fatal "Another update is running. See ${UPDATE_STAGE}/update.log"
    fi
    mkdir -p "$UPDATE_STAGE"
    chown root:root "$UPDATE_STAGE"
    chmod 0755 "$UPDATE_STAGE"
    rm -rf "${UPDATE_STAGE}/previous" "${UPDATE_STAGE}/new" "${UPDATE_STAGE}/supervise.sh" "${UPDATE_STAGE}/env"
    mkdir -m 0700 "${UPDATE_STAGE}/previous" "${UPDATE_STAGE}/new"
    cp -p "$RPORT_BIN" "${UPDATE_STAGE}/previous/rport"
    cp -p "$CONFIG_FILE" "${UPDATE_STAGE}/previous/rport.conf"
    if [ -f /etc/systemd/system/rport.service ]; then
        cp -p /etc/systemd/system/rport.service "${UPDATE_STAGE}/previous/rport.service"
    fi
    throw_info "Backed up ${RPORT_BIN} (${CURRENT_VERSION}) and ${CONFIG_FILE} to ${UPDATE_STAGE}/previous"
}

#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  stage_download
#   DESCRIPTION:  Download and verify the latest client from RPORT_DOWNLOAD_BASE into the stage
#----------------------------------------------------------------------------------------------------------------------
stage_download() {
    TEMP=$(mktemp -d)
    DL_FILE="rport_linux_${ARCH}.tar.gz"
    curl -fLSs "${RPORT_DOWNLOAD_BASE}/rport/${RELEASE}/${DL_FILE}" -o "${TEMP}/rport.tar.gz"
    verify_rport_download "${TEMP}/rport.tar.gz" "${DL_FILE}"
    tar xzf "$TEMP/rport.tar.gz" -C "${UPDATE_STAGE}/new" rport
    rm -rf "$TEMP"
    PKG_KIND=tarball
}

#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  stage_custom_package
#   DESCRIPTION:  Put the package given with -z (URL or local file; tar.gz, deb or rpm) into the stage.
#                 Its checksum is verified if PKG_SHA256 is set.
#----------------------------------------------------------------------------------------------------------------------
stage_custom_package() {
    case "$PKG_URL" in
    *.tar.gz) PKG_KIND=tarball ;;
    *.deb) PKG_KIND=deb ;;
    *.rpm) PKG_KIND=rpm ;;
    *) throw_fatal "-z must point to a .tar.gz, .deb or .rpm file: $PKG_URL" ;;
    esac
    PKG_DOWNLOAD="${UPDATE_STAGE}/new/download"
    if [ -f "$PKG_URL" ]; then
        cp "$PKG_URL" "$PKG_DOWNLOAD"
    else
        DL_AUTH=""
        if [ -n "${RPORT_INSTALLER_DL_USERNAME:-}" ] && [ -n "${RPORT_INSTALLER_DL_PASSWORD:-}" ]; then
            DL_AUTH="-u ${RPORT_INSTALLER_DL_USERNAME}:${RPORT_INSTALLER_DL_PASSWORD}"
        fi
        throw_info "Downloading ${PKG_URL} ..."
        # shellcheck disable=SC2086
        curl -fLSs ${DL_AUTH} "$PKG_URL" -o "$PKG_DOWNLOAD"
    fi
    if [ -n "${PKG_SHA256:-}" ]; then
        [ "$(sha256sum "$PKG_DOWNLOAD" | awk '{print $1}')" = "$PKG_SHA256" ] || throw_fatal "Checksum mismatch for $PKG_URL"
        throw_info "Checksum of $PKG_URL verified."
    else
        throw_warning "No PKG_SHA256 given; the checksum of $PKG_URL is not verified."
    fi
    case "$PKG_KIND" in
    tarball)
        tar xzf "$PKG_DOWNLOAD" -C "${UPDATE_STAGE}/new" rport
        rm -f "$PKG_DOWNLOAD"
        ;;
    deb)
        mv "$PKG_DOWNLOAD" "${UPDATE_STAGE}/new/rport.deb"
        chmod 0644 "${UPDATE_STAGE}/new/rport.deb"
        TARGET_VERSION=$(dpkg-deb -f "${UPDATE_STAGE}/new/rport.deb" Version)
        ;;
    rpm)
        mv "$PKG_DOWNLOAD" "${UPDATE_STAGE}/new/rport.rpm"
        TARGET_VERSION=$(rpm -qp --qf '%{VERSION}' "${UPDATE_STAGE}/new/rport.rpm")
        ;;
    esac
}

#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  start_supervisor
#   DESCRIPTION:  Start the update supervisor detached from this process: restarting the client kills
#                 everything it started, including this script when it runs as an rport command.
#----------------------------------------------------------------------------------------------------------------------
start_supervisor() {
    cat >"${UPDATE_STAGE}/env" <<EOF
STAGE='${UPDATE_STAGE}'
RPORT_BIN='${RPORT_BIN}'
CONFIG_FILE='${CONFIG_FILE}'
LOG_FILE='$(client_log_file)'
FROM_VERSION='${CURRENT_VERSION}'
TO_VERSION='${TARGET_VERSION}'
CHANGE='${CHANGE:-}'
PKG_KIND='${PKG_KIND}'
HEALTH_TIMEOUT='${HEALTH_TIMEOUT}'
STABLE_SECS='${STABLE_SECS}'
EOF
    write_update_supervisor "${UPDATE_STAGE}/supervise.sh"
    chmod 0600 "${UPDATE_STAGE}/env"
    chmod 0700 "${UPDATE_STAGE}/supervise.sh"
    rm -f "${UPDATE_STAGE}/status.json"
    if command -v systemd-run >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
        UNIT="rport-update-$(date +%s)"
        systemd-run --unit="$UNIT" --collect --quiet /bin/sh "${UPDATE_STAGE}/supervise.sh" "${UPDATE_STAGE}/env" 2>/dev/null ||
            systemd-run --unit="$UNIT" --quiet /bin/sh "${UPDATE_STAGE}/supervise.sh" "${UPDATE_STAGE}/env"
        throw_info "Update supervisor started as systemd unit ${UNIT}."
    else
        setsid /bin/sh "${UPDATE_STAGE}/supervise.sh" "${UPDATE_STAGE}/env" </dev/null >/dev/null 2>&1 &
        throw_info "Update supervisor started in the background."
    fi
}

# Wait for the supervisor and print its result. Returns 0 if the new version is connected.
wait_for_supervisor() {
    limit=$((2 * (HEALTH_TIMEOUT + STABLE_SECS) + 120))
    waited=0
    while [ "$waited" -lt "$limit" ]; do
        if [ -f "${UPDATE_STAGE}/status.json" ] && ! grep -q '"status": "running"' "${UPDATE_STAGE}/status.json"; then
            break
        fi
        sleep 2
        waited=$((waited + 2))
    done
    cat "${UPDATE_STAGE}/status.json" 2>/dev/null || echo "No result yet; see ${UPDATE_STAGE}/update.log"
    grep -q '"status": "success"' "${UPDATE_STAGE}/status.json" 2>/dev/null
}

#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  update
#   DESCRIPTION:  update to the latest version or exit if not update available
#----------------------------------------------------------------------------------------------------------------------
update() {
  check_prerequisites
  RPORT_BIN=$(current_binary)
  CURRENT_VERSION=$(current_version)
  TARGET_VERSION=""
  if [ -z "$PKG_URL" ]; then
      require_download_base
      LATEST_VERSION=$(latest_rport_version || true)
      if [ -z "$LATEST_VERSION" ]; then
          throw_fatal "Could not read the latest version from ${RPORT_DOWNLOAD_BASE}/rport/${RELEASE}/VERSION"
      fi
      if ! version_newer "$LATEST_VERSION" "$CURRENT_VERSION"; then
          throw_info "Nothing to do. RPort is on the latest version ${CURRENT_VERSION}."
          [ "$ENABLE_TACOSCRIPT" -eq 1 ] && install_tacoscript
          exit 0
      fi
  fi
  # Back up before anything is changed, including the config questions below.
  prepare_stage
  check_scripts
  check_sudo
  cd /tmp

  if [ -n "$PKG_URL" ]; then
      throw_info "Updating from ${PKG_URL}. Checking the current version skipped."
      stage_custom_package
  else
      stage_download
  fi
  if [ -z "$TARGET_VERSION" ]; then
      TARGET_VERSION=$("${UPDATE_STAGE}/new/rport" --version | awk '{print $2}')
  fi
  throw_info "Updating from ${CURRENT_VERSION} to ${TARGET_VERSION}"
  if command -v dpkg >/dev/null 2>&1 && dpkg -s rport >/dev/null 2>&1 && command -v apt-mark >/dev/null 2>&1 && [ "$PKG_KIND" = tarball ]; then
      apt-mark hold rport >/dev/null && throw_info "Package 'rport' put on hold so apt keeps this version."
  fi
  create_sudoers_updates
  detect_interpreters
  enable_monitoring
  enable_lan_monitoring
  enable_file_reception
  insert_watchdog
  [ "$ENABLE_TACOSCRIPT" -eq 1 ] && install_tacoscript

  start_supervisor
  if is_rport_subprocess; then
      throw_info "The update runs in the background, because this script was started by the rport client."
      throw_info "The client restarts; if ${TARGET_VERSION} does not connect within ${HEALTH_TIMEOUT}s, ${CURRENT_VERSION} is restored."
      throw_info "Result: ${UPDATE_STAGE}/status.json, log: ${UPDATE_STAGE}/update.log"
      exit 0
  fi
  throw_info "Waiting for the new client to connect (up to ${HEALTH_TIMEOUT}s, then ${STABLE_SECS}s to confirm) ..."
  if wait_for_supervisor; then
      finish
      return 0
  fi
  fail
  exit 1
}

#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  switch_server
#   DESCRIPTION:  Point the client at this server's connect URLs (RPORT_CONNECT_URLS, from [server] url): the first as
#                 server, the others as fallback_servers. Supervised like an update: if the client does not reconnect,
#                 the previous rport.conf is restored.
#----------------------------------------------------------------------------------------------------------------------
switch_server() {
  check_prerequisites
  RPORT_BIN=$(current_binary)
  CURRENT_VERSION=$(current_version)
  # shellcheck disable=SC2086
  set -- ${RPORT_CONNECT_URLS:-}
  if [ $# -eq 0 ]; then
      throw_fatal "No public connect URL configured on the server ([server] url). Nothing changed."
  fi
  case "$*" in
  *"'"*) throw_fatal "Invalid connect URL: $*" ;;
  esac
  # Check the config can be changed before anything else is touched.
  SWITCH_CHECK=$(mktemp)
  cp "$CONFIG_FILE" "$SWITCH_CHECK"
  if ! set_client_servers "$SWITCH_CHECK" "$@"; then
      rm -f "$SWITCH_CHECK"
      throw_fatal "${CONFIG_FILE} can't be changed. Nothing changed."
  fi
  rm -f "$SWITCH_CHECK"
  prepare_stage
  cp -p "$RPORT_BIN" "${UPDATE_STAGE}/new/rport"
  PKG_KIND=tarball
  TARGET_VERSION=$CURRENT_VERSION
  SWITCH_SERVER=$1
  shift
  CHANGE="server to ${SWITCH_SERVER}"
  [ $# -gt 0 ] && CHANGE="${CHANGE} (fallback $*)"
  set_client_servers "$CONFIG_FILE" "$SWITCH_SERVER" "$@"
  throw_info "Switching the ${CHANGE}"

  start_supervisor
  if is_rport_subprocess; then
      throw_info "The switch runs in the background, because this script was started by the rport client."
      throw_info "The client restarts; if it does not connect within ${HEALTH_TIMEOUT}s, the previous config is restored."
      throw_info "Result: ${UPDATE_STAGE}/status.json, log: ${UPDATE_STAGE}/update.log"
      exit 0
  fi
  throw_info "Waiting for the client to connect (up to ${HEALTH_TIMEOUT}s, then ${STABLE_SECS}s to confirm) ..."
  if wait_for_supervisor; then
      throw_info "Server switched: ${CHANGE}"
      return 0
  fi
  fail
  exit 1
}

#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  ask_yes_no
#   DESCRIPTION:  Ask a question and wait for a confirmation
#    PARAMETERS:  Question to be asked
#----------------------------------------------------------------------------------------------------------------------
ask_yes_no() {
  if [ -z "$1" ]; then
    printf "Do you want to proceed?"
  else
    printf "%s" "$1"
  fi
  echo " (y/n)"
  while read -r INPUT; do
    if echo "$INPUT" | grep -q "^[Yy]"; then
      return 0
    elif echo "$INPUT" | grep -q "^[Nn]"; then
      return 1
    fi
    echo "Type (y/n) or abort with Ctrl-C"
  done
}

#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  insert_scripts
#   DESCRIPTION:  Insert the missing remote scripts block
#----------------------------------------------------------------------------------------------------------------------
insert_scripts() {
  echo "[remote-scripts]
  enabled = ${ENABLE_SCRIPTS}
" >>"$CONFIG_FILE"
}

#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  check_scripts
#   DESCRIPTION:  check if scripts can be activated
#----------------------------------------------------------------------------------------------------------------------
check_scripts() {
  if grep -q remote-scripts "$CONFIG_FILE"; then
    return 0
  fi

  if [ "$ENABLE_SCRIPTS" = 'true' ]; then
    if grep -q "\[remote-scripts\]" "$CONFIG_FILE"; then
        insert_scripts
        return 0
      fi

  fi
  if is_terminal; then
    true
  else
    echo 1>&2 "Please use the switches -x/-d to enable or disable script execution."
    help
    # shellcheck disable=SC2317  # Don't warn about unreachable commands in this function
    exit 1
  fi
  if ask_yes_no "Do you want to activate script execution?"; then
    ENABLE_SCRIPTS=true
    insert_scripts
  fi
}

#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  check_sudo
#   DESCRIPTION:  check if user wants to enable sudo
#----------------------------------------------------------------------------------------------------------------------
check_sudo() {
  if [ -e /etc/sudoers.d/rport-all-cmd ]; then
    return 0
  fi
  if [ "$ENABLE_SUDO" -eq 1 ]; then
    create_sudoers_all
    return 0
  fi
  if [ "$ENABLE_SUDO" -eq 0 ]; then
    return 0
  fi
  if is_terminal; then
    true
  else
    echo 1>&2 "Please use the switches -s/-n to enable or disable sudo rights."
    help
    # shellcheck disable=SC2317  # Don't warn about unreachable commands in this function
    exit 1
  fi
  if ask_yes_no "Do you want to activate sudo rights for RPort remote script execution?"; then
    create_sudoers_all
  fi
}

clean_up() {
  true
}

enable_monitoring() {
  if [ "$(version_to_int "$TARGET_VERSION")" -lt 5000 ]; then
    # Version does not handle monitoring yet.
    return 0
  fi
  if grep -q "\[monitoring\]" "$CONFIG_FILE"; then
    echo "Monitoring already enabled."
    return 0
  fi
  cat <<EOF >>"$CONFIG_FILE"
[monitoring]
  ## The rport client can collect and report performance data of the operating system.
  ## Monitoring is enabled by default
  enabled = true
  ## How often (seconds) monitoring data should be collected.
  ## A value below 60 seconds will be overwritten by the hard-coded default of 60 seconds.
  # interval = 60
  ## RPort monitors the fill level of almost all volumes or mount points.
  ## Change the below defaults to include or exclude volumes or mount points from the monitoring.
  #fs_type_include = ['ext3','ext4','xfs','jfs','ntfs','btrfs','hfs','apfs','exfat','smbfs','nfs']
  ## List of excluded mount points or device letters
  #fs_path_exclude = []
  ## Example:
  # fs_path_exclude = ['/mnt/*','h:']
  ## Having fs_path_exclude_recurse = false the specified path
  ## must match a mountpoint or it will be ignored
  ## Having fs_path_exclude_recurse = true the specified path
  ## can be any folder and all mountpoints underneath will be excluded
  #fs_path_exclude_recurse = false
  ## To avoid monitoring of so-called mount binds,
  ## mount points are identified by the path and device name.
  ## Mountpoints pointing to the same device are ignored.
  ## What appears first in /proc/self/mountinfo is considered as the original.
  ## Applies only to Linux
  #fs_identify_mountpoints_by_device = true
  ## RPort monitors all running processes
  ## Process monitoring is enabled by default
  pm_enabled = true
  ## Monitor kernel tasks identified by process group 0
  #pm_enable_kerneltask_monitoring = true
  ## The process list is sorted by PID descending. Only the top N processes are monitored.
  #pm_max_number_monitored_processes = 500
  ## Monitor the bandwidth usage of the following maximum two network cards:
  ## 'net_lan' and 'net_wan'.
  ## You must specify the device name and the maximum speed in Megabits.
  ## On Windows use 'Get-Netadapter' to discover adapter names.
  ## Examples:
  ## net_lan = [ 'eth0' , 1000 ]
  ## net_wan = ['Ethernet0', 1000]
  #net_lan = ['', 1000]
  #net_wan = ['', 1000]
EOF
  echo "Monitoring enabled."
}

insert_watchdog() {
  if [ "$(version_to_int "$TARGET_VERSION")" -lt 8007 ]; then
    # Version does not handle watchdog integration yet.
    echo "Version $TARGET_VERSION does not support watchdog_integration yet"
    return 0
  fi
  if grep -q watchdog_integration "$CONFIG_FILE"; then
    # Watchdog integration already present
    return 0
  else
    WATCHDOG_SNIPPET=$(sed ':a $!{N; ba}; s/\n/\\n/g'<<EOF
  ## Write a state file to {data_dir}/state.json that can be evaluated by external watchdog implementations.
  ## On Linux this also enables the systemd watchdog integration using the systemd notify socket.
  ## Requires max_retry_count = -1 and keep_alive > 0
  ## Disabled by default.
  #watchdog_integration = false
EOF
)
    sed -i "/max_retry_interval/a\\\n${WATCHDOG_SNIPPET}" "$CONFIG_FILE"
  fi
}

#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  finish
#   DESCRIPTION:  print some information
#----------------------------------------------------------------------------------------------------------------------
finish() {
  echo "
#
#  Update of the Procy client (rport) finished: $(rport --version 2>/dev/null) is connected.
#
#  Client log: $(client_log_file), update log: ${UPDATE_STAGE}/update.log
#
"
}

fail() {
  echo "
#
# -------------!!   ERROR  !!-------------
#
# The update did not succeed. The supervisor restores the previous version if the new one does not
# connect; the result above says whether it did.
#
# Investigate with:
#   cat ${UPDATE_STAGE}/update.log
#   systemctl status rport
#   tail /var/log/rport/rport.log
"
  if runs_with_selinux; then
    echo "  Also check your SELinux settings and create a policy for rport."
  fi
}

#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  help
#   DESCRIPTION:  print a help message and exit
#----------------------------------------------------------------------------------------------------------------------
help() {
  cat <<EOF
Usage $0 [OPTION(s)]

Update the current version of RPort to the latest version.

Options:
-h  print this help message
-c  update the rport client, default action
-t  use the latest unstable version (DANGEROUS!)
-u  uninstall the rport client and all configurations and logs
-x  enable script execution in rport.conf
-d  disable script execution in rport.conf
-s  create sudo rules to grant full root access to the rport user
-n  do not change sudo rights (no question asked)
-m  do not install or update tacoscript
-p  No effect; kept for compatibility (packages always come from the downloads site).
-z  Install the rport client tar.gz|deb|rpm from the given URL or local file instead of the downloads site.
    Set PKG_SHA256 to verify its checksum.
-S  Point the client at this server's connect URLs ([server] url): the first as server, the others as
    fallback_servers. Supervised: if the client does not reconnect, the previous rport.conf is restored.

The update is supervised: if the new client does not connect to the server within HEALTH_TIMEOUT
seconds (default 120), the previous binary and config are restored. Result: /var/lib/rport-update/status.json
EOF
  exit 0
}

#
# Read the command line options and map to a function call
#
ACTION=update
PKG_URL=""
ENABLE_TACOSCRIPT=1
ENABLE_SUDO=2
RELEASE=stable
ENABLE_SCRIPTS=undef
ENABLE_FILEREC=0
ENABLE_FILEREC_SUDO=0
NO_REPO=0
while getopts "phcuxdsnmrbtSz:" opt; do
  case "${opt}" in

  h) ACTION=help ;;
  c) ACTION=update ;;
  u) ACTION=uninstall ;;
  x) ENABLE_SCRIPTS=true ;;
  d) ENABLE_SCRIPTS=false ;;
  s) ENABLE_SUDO=1 ;;
  n) ENABLE_SUDO=0 ;;
  t) RELEASE=unstable ;;
  m) ENABLE_TACOSCRIPT=0 ;;
  r) export ENABLE_FILEREC=1 ;;
  b) export ENABLE_FILEREC_SUDO=1 ;;
  p) NO_REPO=1 ;;
  z) PKG_URL="${OPTARG}" ;;
  S) ACTION=switch_server ;;

  \?)
    echo "Option does not exist."
    exit 1
    ;;
  esac # --- end of case ---
done
shift $((OPTIND - 1))
# Packages always come from the downloads site configured on the pairing service;
# -p (no package repository) is kept for compatibility and is always in effect.
NO_REPO=1
$ACTION # Execute the function according to the users decision
