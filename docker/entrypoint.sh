#!/bin/sh
#
# Container entrypoint for HAProxy Controller.
#
# Prepares the writable state that bind-mounted volumes may arrive empty, then
# hands PID 1 to the controller so it receives shutdown signals directly and
# can stop HAProxy gracefully.
#
# Powered by Ebdaa.me - https://ebdaa.me
set -eu

CONFIG_FILE="${HC_CONFIG:-/etc/haproxy-controller/controller.json}"
HAPROXY_CFG="${HC_HAPROXY_CONFIG:-/etc/haproxy/haproxy.cfg}"
CERTS_DIR="${HC_CERTS_DIR:-/etc/haproxy/certs}"
DATA_DIR="${HC_DATA_DIR:-/var/lib/haproxy-controller}"

log() { printf '%s entrypoint: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }

# ---------------------------------------------------------------- directories
# Volumes mounted from the host start empty and may be owned by root.
mkdir -p "$(dirname "$CONFIG_FILE")" "$(dirname "$HAPROXY_CFG")" \
         "${DATA_DIR}/backups" /etc/haproxy/errors /etc/haproxy/maps \
         /run/haproxy "$CERTS_DIR"
chmod 700 "$CERTS_DIR"
chown -R haproxy:haproxy /run/haproxy /var/lib/haproxy 2>/dev/null || true

# ------------------------------------------------------------ controller.json
# Written once. Afterwards it is the operator's file: the panel persists
# settings changes into it, so it is never regenerated.
if [ ! -f "$CONFIG_FILE" ]; then
  log "creating ${CONFIG_FILE}"
  cat > "$CONFIG_FILE" <<JSON
{
  "listen_addr": "${HC_LISTEN_ADDR:-0.0.0.0}",
  "listen_port": ${HC_LISTEN_PORT:-9000},
  "tls_enabled": false,
  "data_dir": "${DATA_DIR}",
  "db_path": "${DATA_DIR}/controller.db",
  "backup_dir": "${DATA_DIR}/backups",
  "haproxy_bin": "${HC_HAPROXY_BIN:-/usr/local/sbin/haproxy}",
  "config_path": "${HAPROXY_CFG}",
  "config_dir": "/etc/haproxy",
  "error_pages_dir": "/etc/haproxy/errors",
  "certs_dir": "${CERTS_DIR}",
  "maps_dir": "/etc/haproxy/maps",
  "runtime_socket": "${HC_RUNTIME_SOCKET:-/run/haproxy/admin.sock}",
  "master_socket": "${HC_MASTER_SOCKET:-/run/haproxy/master.sock}",
  "pid_file": "/run/haproxy/haproxy.pid",
  "process_mode": "supervised",
  "session_ttl_minutes": ${HC_SESSION_TTL_MINUTES:-720},
  "log_level": "${HC_LOG_LEVEL:-info}"
}
JSON
  chmod 600 "$CONFIG_FILE"
fi

# ------------------------------------------------------- placeholder cert
# A `crt <dir>/` reference fails to load when the directory holds no PEM, so
# a listener with TLS would refuse to start before any certificate is added.
if [ -z "$(find "$CERTS_DIR" -maxdepth 1 -name '*.pem' -print -quit 2>/dev/null)" ]; then
  log "generating a placeholder certificate in ${CERTS_DIR}"
  openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
    -keyout /tmp/ph.key -out /tmp/ph.crt \
    -subj "/CN=haproxy-controller.local" >/dev/null 2>&1
  cat /tmp/ph.key /tmp/ph.crt > "${CERTS_DIR}/000-placeholder.pem"
  chmod 600 "${CERTS_DIR}/000-placeholder.pem"
  rm -f /tmp/ph.key /tmp/ph.crt
fi

# --------------------------------------------------------------- bootstrap
# HAProxy refuses to start without at least one listener, so a fresh install
# has nothing to run until the first frontend is applied. Seeding a minimal
# valid file lets the panel show a healthy service from the very first boot.
if [ ! -f "$HAPROXY_CFG" ]; then
  log "writing a minimal bootstrap ${HAPROXY_CFG}"
  cat > "$HAPROXY_CFG" <<'CFG'
# Bootstrap configuration. HAProxy Controller replaces this on the first apply.
# Powered by Ebdaa.me - https://ebdaa.me
global
    log stdout format raw local0
    maxconn 20000
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

# Replaced as soon as you define your own frontends in the panel.
frontend placeholder
    bind 127.0.0.1:1
    http-request deny deny_status 503
CFG
fi

if [ "${1:-}" = "haproxy-controller" ]; then
  shift
  set -- /usr/local/bin/haproxy-controller "$@"
fi

log "starting ${*}"
exec "$@"
