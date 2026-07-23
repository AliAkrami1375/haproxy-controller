#!/usr/bin/env bash
#
# Install HAProxy Controller and its systemd services.
#
# Powered by Ebdaa.me - https://ebdaa.me
#
# Usage:
#   sudo ./scripts/install.sh                  # build and install everything
#   sudo CONTROL_PORT=9443 ./scripts/install.sh
#
set -euo pipefail

PREFIX="${PREFIX:-/usr/local}"
CONFIG_DIR="${CONFIG_DIR:-/etc/haproxy-controller}"
DATA_DIR="${DATA_DIR:-/var/lib/haproxy-controller}"
HAPROXY_DIR="${HAPROXY_DIR:-/etc/haproxy}"
CONTROL_PORT="${CONTROL_PORT:-9000}"
CONTROL_ADDR="${CONTROL_ADDR:-0.0.0.0}"
HAPROXY_BIN="${HAPROXY_BIN:-/usr/local/sbin/haproxy}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m==>\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m==>\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "This script must run as root."

# ---------------------------------------------------------------- binary
if [ -x "${ROOT}/bin/haproxy-controller" ]; then
  log "Using the prebuilt binary at bin/haproxy-controller"
  BINARY="${ROOT}/bin/haproxy-controller"
else
  command -v go >/dev/null 2>&1 || die "Go is required to build. Install Go 1.22+ or provide bin/haproxy-controller."
  log "Building the controller"
  ( cd "$ROOT" && make build )
  BINARY="${ROOT}/bin/haproxy-controller"
fi

install -d -m 0755 "${PREFIX}/bin"
install -m 0755 "$BINARY" "${PREFIX}/bin/haproxy-controller"
log "Installed ${PREFIX}/bin/haproxy-controller"

# ------------------------------------------------------------ directories
install -d -m 0750 "$CONFIG_DIR"
install -d -m 0750 "$DATA_DIR" "${DATA_DIR}/backups"
install -d -m 0755 "$HAPROXY_DIR" "${HAPROXY_DIR}/errors" "${HAPROXY_DIR}/maps"
install -d -m 0700 "${HAPROXY_DIR}/certs"

# ---------------------------------------------------------- configuration
CONFIG_FILE="${CONFIG_DIR}/controller.json"
if [ -f "$CONFIG_FILE" ]; then
  log "Keeping the existing ${CONFIG_FILE}"
else
  log "Writing ${CONFIG_FILE}"
  cat > "$CONFIG_FILE" <<JSON
{
  "listen_addr": "${CONTROL_ADDR}",
  "listen_port": ${CONTROL_PORT},
  "tls_enabled": false,
  "tls_cert": "",
  "tls_key": "",
  "data_dir": "${DATA_DIR}",
  "db_path": "${DATA_DIR}/controller.db",
  "haproxy_bin": "${HAPROXY_BIN}",
  "config_path": "${HAPROXY_DIR}/haproxy.cfg",
  "config_dir": "${HAPROXY_DIR}",
  "error_pages_dir": "${HAPROXY_DIR}/errors",
  "certs_dir": "${HAPROXY_DIR}/certs",
  "maps_dir": "${HAPROXY_DIR}/maps",
  "backup_dir": "${DATA_DIR}/backups",
  "runtime_socket": "/run/haproxy/admin.sock",
  "reload_command": "systemctl reload haproxy-managed",
  "restart_command": "systemctl restart haproxy-managed",
  "status_command": "systemctl is-active haproxy-managed",
  "session_ttl_minutes": 720,
  "log_level": "info"
}
JSON
  # The file holds the session secret once the controller starts.
  chmod 0600 "$CONFIG_FILE"
fi

# --------------------------------------------------- minimal bootstrap cfg
# HAProxy must have a valid file before its unit will start. The controller
# overwrites this on the first apply.
if [ ! -f "${HAPROXY_DIR}/haproxy.cfg" ]; then
  log "Writing a minimal bootstrap ${HAPROXY_DIR}/haproxy.cfg"
  cat > "${HAPROXY_DIR}/haproxy.cfg" <<'CFG'
# Bootstrap configuration. HAProxy Controller replaces this on the first apply.
# Powered by Ebdaa.me - https://ebdaa.me
global
    log /dev/log local0
    maxconn 20000
    user haproxy
    group haproxy
    stats socket /run/haproxy/admin.sock mode 660 level admin
    stats timeout 30s

defaults
    mode http
    log global
    option httplog
    option dontlognull
    timeout connect 5s
    timeout client 50s
    timeout server 50s
CFG
fi

# ------------------------------------------------------------- services
if command -v systemctl >/dev/null 2>&1; then
  log "Installing systemd units"
  install -m 0644 "${ROOT}/systemd/haproxy-managed.service"    /etc/systemd/system/
  install -m 0644 "${ROOT}/systemd/haproxy-controller.service" /etc/systemd/system/
  systemctl daemon-reload

  if [ ! -x "$HAPROXY_BIN" ]; then
    warn "HAProxy is not installed at ${HAPROXY_BIN}."
    warn "Run scripts/build-haproxy.sh first, then: systemctl enable --now haproxy-managed"
  else
    systemctl enable haproxy-managed >/dev/null 2>&1 || true
    systemctl restart haproxy-managed || warn "haproxy-managed failed to start; check: journalctl -u haproxy-managed"
  fi

  systemctl enable haproxy-controller >/dev/null 2>&1 || true
  systemctl restart haproxy-controller
  sleep 2

  echo
  if systemctl is-active --quiet haproxy-controller; then
    log "The control panel is running."
  else
    warn "The control panel did not start. Check: journalctl -u haproxy-controller -n 50"
  fi
else
  warn "systemd was not found. Start the controller yourself:"
  warn "  ${PREFIX}/bin/haproxy-controller -config ${CONFIG_FILE}"
fi

# ---------------------------------------------------------------- summary
IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
[ -n "$IP" ] || IP="<server-ip>"

cat <<BANNER

====================================================================
  HAProxy Controller is installed
====================================================================
  Panel      : http://${IP}:${CONTROL_PORT}/
  Config     : ${CONFIG_FILE}
  Database   : ${DATA_DIR}/controller.db
  HAProxy    : ${HAPROXY_BIN}

  The first-run administrator password is printed once in the log:
      journalctl -u haproxy-controller | grep -A6 'initial administrator'

  Powered by Ebdaa.me - https://ebdaa.me
====================================================================

BANNER
