set -e
#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  download_new_version
#   DESCRIPTION:  Download and verify the latest client from RPORT_DOWNLOAD_BASE; print the path of the new binary
#----------------------------------------------------------------------------------------------------------------------
download_new_version() {
    TEMP=$(mktemp -d)
    DL_FILE="rport_linux_${ARCH}.tar.gz"
    curl -fLSs "${RPORT_DOWNLOAD_BASE}/rport/${RELEASE}/${DL_FILE}" -o "${TEMP}/rport.tar.gz"
    verify_rport_download "${TEMP}/rport.tar.gz" "${DL_FILE}" >&2
    tar xzf "$TEMP/rport.tar.gz" -C "$TEMP" rport
    rm -f "$TEMP/rport.tar.gz"
    echo "$TEMP/rport"
}

#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  install_rport_binary
#   DESCRIPTION:  Replace the binary the service runs: /usr/bin/rport for package installs, else /usr/local/bin/rport.
#                 A DEB-installed client is put on hold so apt does not swap in another build later.
#----------------------------------------------------------------------------------------------------------------------
install_rport_binary() {
    if [ -e /usr/bin/rport ]; then
        RPORT_BIN=/usr/bin/rport
    else
        RPORT_BIN=/usr/local/bin/rport
    fi
    install -m 0755 "$1" "${RPORT_BIN}.new"
    mv -f "${RPORT_BIN}.new" "${RPORT_BIN}"
    throw_info "Installed ${RPORT_BIN}"
    if command -v dpkg >/dev/null 2>&1 && dpkg -s rport >/dev/null 2>&1 && command -v apt-mark >/dev/null 2>&1; then
        apt-mark hold rport >/dev/null && throw_info "Package 'rport' put on hold so apt keeps this version."
    fi
}

current_version() {
    if [ -e /usr/bin/rport ]; then
        /usr/bin/rport --version | awk '{print $2}'
        return 0
    fi
    if [ -e /usr/local/bin/rport ]; then
        /usr/local/bin/rport --version | awk '{print $2}'
        return 0
    fi
    echo "Failed to get current rport version"
    exit 1
}

#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  restart_rport()
#   DESCRIPTION:  The restart of RPort must be detached from this process in case the update
#                 is triggered remotely using rport. The rport client would kill the script otherwise here.
#    PARAMETERS:  none
#       RETURNS:
#----------------------------------------------------------------------------------------------------------------------
restart_rport() {
    if [ -e /etc/init.d/rport ]; then
        RESTART_CMD='/etc/init.d/rport restart'
    else
        RESTART_CMD='systemctl restart rport'
    fi
    if [ "$1" = "background" ]; then
        if command -v at >/dev/null 2>&1; then
            echo "$RESTART_CMD" | at now +1 minute
            throw_info "Restart of rport scheduled via atd."
        else
            nohup sh -c "sleep 10;$RESTART_CMD" >/dev/null 2>&1 &
            throw_info "Restart of rport scheduled via nohup+sleep."
        fi
        return 0
    fi
    throw_info "Restarting RPort using '$RESTART_CMD'"
    $RESTART_CMD
}
#---  FUNCTION  -------------------------------------------------------------------------------------------------------
#          NAME:  update
#   DESCRIPTION:  update to the latest version or exit if not update available
#----------------------------------------------------------------------------------------------------------------------
update() {
  CURRENT_VERSION=$(current_version)
  check_prerequisites
  # Ask (or validate switches) before anything is changed.
  check_scripts
  check_sudo
  cd /tmp

  if [ -n "$PKG_URL" ] && is_debian; then
      abort_on_rport_subprocess
      RESTART_IN="foreground"
      throw_info "Updating DEB from custom URL. Checking current version skipped."
      install_from_deb_download
  elif [ -n "$PKG_URL" ] && is_rhel; then
      abort_on_rport_subprocess
      RESTART_IN="foreground"
      throw_info "Updating RPM from custom URL. Checking current version skipped."
      install_from_rpm_download
  else
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
      # Install from tar.gz
      NEW_VERSION=$(download_new_version)
      TARGET_VERSION=$(${NEW_VERSION} --version | awk '{print $2}')
      throw_info "Updating from ${CURRENT_VERSION} to latest ${RELEASE} ${TARGET_VERSION}"
      install_rport_binary "$NEW_VERSION"
      rm -rf "$(dirname "$NEW_VERSION")"
      RESTART_IN="background"
  fi
  create_sudoers_updates
  detect_interpreters
  enable_monitoring
  enable_lan_monitoring
  enable_file_reception
  insert_watchdog

  [ "$ENABLE_TACOSCRIPT" -eq 1 ] && install_tacoscript
  throw_info "You are now running $(rport --version)"

  restart_rport $RESTART_IN
  if wait_for_rport; then
      finish
      return 0
  fi
  fail
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
#  Update of RPort finished.
#
#  Logs are written to /var/log/rport/rport.log.
#

Thanks for using
   ____                   _____  _____           _
  / __ \                 |  __ \|  __ \         | |
 | |  | |_ __   ___ _ __ | |__) | |__) |__  _ __| |_
 | |  | | '_ \ / _ \ '_ \|  _  /|  ___/ _ \| '__| __|
 | |__| | |_) |  __/ | | | | \ \| |  | (_) | |  | |_
  \____/| .__/ \___|_| |_|_|  \_\_|   \___/|_|   \__|
        | |
        |_|
"
}

fail() {
  systemctl --no-pager status rport
  echo "
#
# -------------!!   ERROR  !!-------------
#
# Update of RPort finished with errors.
#

Try the following to investigate:
1) systemctl rport status

2) tail /var/log/rport/rport.log

"
  if runs_with_selinux; then
    echo "
3) Check your SELinux settings and create a policy for rport."
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
-z  Download the rport client tar.gz|deb|rpm from the given URL instead of the downloads site.
    See environment variables.
EOF
  exit 0
}

#
# Read the command line options and map to a function call
#
ACTION=update
ENABLE_TACOSCRIPT=1
ENABLE_SUDO=2
RELEASE=stable
ENABLE_SCRIPTS=undef
ENABLE_FILEREC=0
ENABLE_FILEREC_SUDO=0
NO_REPO=0
while getopts "phcuxdsnmrbtz:" opt; do
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
  z) export PKG_URL="${OPTARG}" ;;

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
