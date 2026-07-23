#!/usr/bin/env bash
#
# Build and install HAProxy from the source vendored in this repository.
#
# The source lives in third_party/haproxy and is compiled as-is, so this
# script never downloads HAProxy. To move to a different release, run
# scripts/vendor-haproxy.sh and commit the result.
#
# Powered by Ebdaa.me - https://ebdaa.me
#
# Usage:
#   sudo ./scripts/build-haproxy.sh
#   sudo PREFIX=/opt/haproxy ./scripts/build-haproxy.sh
#   sudo SKIP_DEPS=1 ./scripts/build-haproxy.sh     # dependencies already present
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC_DIR="${SRC_DIR:-${ROOT}/third_party/haproxy}"
PREFIX="${PREFIX:-/usr/local}"
BUILD_DIR="${BUILD_DIR:-/usr/local/src/haproxy-build}"
JOBS="${JOBS:-$(nproc 2>/dev/null || echo 2)}"
SKIP_DEPS="${SKIP_DEPS:-0}"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m==>\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m==>\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "This script must run as root (it installs into ${PREFIX})."

# ---------------------------------------------------------------- packages
install_deps() {
  [ "$SKIP_DEPS" = "1" ] && { log "Skipping dependency installation."; return; }

  if command -v apt-get >/dev/null 2>&1; then
    log "Installing build dependencies with apt-get"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y --no-install-recommends \
      build-essential ca-certificates pkg-config \
      libssl-dev libpcre2-dev zlib1g-dev libsystemd-dev liblua5.4-dev
  elif command -v dnf >/dev/null 2>&1; then
    log "Installing build dependencies with dnf"
    dnf install -y gcc make pkgconf-pkg-config openssl-devel pcre2-devel zlib-devel systemd-devel lua-devel
  elif command -v yum >/dev/null 2>&1; then
    log "Installing build dependencies with yum"
    yum install -y gcc make pkgconfig openssl-devel pcre2-devel zlib-devel systemd-devel lua-devel
  elif command -v apk >/dev/null 2>&1; then
    log "Installing build dependencies with apk"
    apk add --no-cache build-base pkgconf openssl-dev pcre2-dev zlib-dev lua5.4-dev linux-headers
  else
    warn "No supported package manager found. Install the build dependencies yourself:"
    warn "  a C toolchain, OpenSSL, PCRE2, zlib, Lua 5.4 and systemd headers."
  fi
}

# ------------------------------------------------------------- lua probe
# HAProxy needs both the Lua headers and its pkg-config name, which differs
# between distributions.
detect_lua() {
  local candidate inc
  for candidate in lua5.4 lua-5.4 lua54 lua5.3 lua-5.3 lua; do
    pkg-config --exists "$candidate" 2>/dev/null || continue

    # HAProxy needs the directory that actually contains lauxlib.h. On Debian
    # that is /usr/include/lua5.4, which only --cflags reveals; the
    # includedir variable points one level too high.
    inc="$(pkg-config --cflags-only-I "$candidate" 2>/dev/null | tr ' ' '\n' | sed -n 's/^-I//p' | head -1)"
    [ -n "$inc" ] || inc="$(pkg-config --variable=includedir "$candidate" 2>/dev/null || true)"

    if [ ! -f "${inc}/lauxlib.h" ]; then
      # Fall back to searching the usual locations.
      inc="$(find /usr/include /usr/local/include -maxdepth 2 -name lauxlib.h -printf '%h\n' 2>/dev/null | head -1)"
    fi
    [ -n "$inc" ] && [ -f "${inc}/lauxlib.h" ] || continue

    LUA_PKG="$candidate"
    LUA_INC="$inc"
    LUA_LIB="$(pkg-config --libs-only-L "$candidate" 2>/dev/null | tr -d ' ') $(pkg-config --libs-only-l "$candidate" 2>/dev/null)"
    LUA_LIB="$(echo "$LUA_LIB" | sed 's/^ *//;s/ *$//')"
    return 0
  done
  return 1
}

install_deps

# ----------------------------------------------------------------- source
[ -f "${SRC_DIR}/Makefile" ] || die "No HAProxy source at ${SRC_DIR}. Run scripts/vendor-haproxy.sh first."

VERSION="$(cat "${SRC_DIR}/VERSION" 2>/dev/null || true)"
log "Using the vendored HAProxy source (${VERSION:-unknown version})"
[ -f "${SRC_DIR}/VENDOR.md" ] && sed -n 's/^| Commit | `\(.*\)` |$/    commit \1/p' "${SRC_DIR}/VENDOR.md"

# Build out of tree so the committed source stays pristine.
log "Copying the source to ${BUILD_DIR}"
rm -rf "${BUILD_DIR}/haproxy"
mkdir -p "$BUILD_DIR"
cp -a "$SRC_DIR" "${BUILD_DIR}/haproxy"
cd "${BUILD_DIR}/haproxy"
make clean >/dev/null 2>&1 || true

# ---------------------------------------------------------------- build
MAKE_ARGS=(
  TARGET=linux-glibc
  USE_OPENSSL=1
  USE_PCRE2=1
  USE_PCRE2_JIT=1
  USE_ZLIB=1
  USE_THREAD=1
  USE_PROMEX=1        # built-in Prometheus exporter
  USE_GETADDRINFO=1
)

if pkg-config --exists libsystemd 2>/dev/null; then
  MAKE_ARGS+=(USE_SYSTEMD=1)
  log "systemd notify support enabled"
else
  warn "libsystemd not found; building without USE_SYSTEMD (the unit uses Type=exec)"
fi

if detect_lua; then
  MAKE_ARGS+=(USE_LUA=1)
  [ -n "${LUA_INC:-}" ] && MAKE_ARGS+=("LUA_INC=${LUA_INC}")
  [ -n "${LUA_LIB:-}" ] && MAKE_ARGS+=("LUA_LIB=${LUA_LIB}")
  log "Lua support enabled via pkg-config ${LUA_PKG}"
else
  warn "Lua headers not found; building without USE_LUA"
fi

log "Building HAProxy ${VERSION:-} with ${JOBS} job(s)"
make -j"${JOBS}" "${MAKE_ARGS[@]}"

log "Installing into ${PREFIX}"
make install-bin install-man PREFIX="$PREFIX"

# ------------------------------------------------------- runtime plumbing
if ! id -u haproxy >/dev/null 2>&1; then
  log "Creating the haproxy system user"
  groupadd --system haproxy 2>/dev/null || true
  useradd --system --gid haproxy --no-create-home \
          --home-dir /var/lib/haproxy --shell /usr/sbin/nologin haproxy 2>/dev/null || true
fi

install -d -m 0755 -o haproxy -g haproxy /var/lib/haproxy
install -d -m 0755 /etc/haproxy /etc/haproxy/errors /etc/haproxy/maps
install -d -m 0700 /etc/haproxy/certs
install -d -m 0755 -o haproxy -g haproxy /run/haproxy

# The certificate directory must never be empty: HAProxy fails to start when a
# `crt <dir>/` reference resolves to nothing.
if [ -z "$(find /etc/haproxy/certs -maxdepth 1 -name '*.pem' -print -quit 2>/dev/null)" ]; then
  log "Generating a placeholder self-signed certificate"
  openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
    -keyout /tmp/hc-placeholder.key -out /tmp/hc-placeholder.crt \
    -subj "/CN=haproxy-controller-placeholder" >/dev/null 2>&1
  cat /tmp/hc-placeholder.key /tmp/hc-placeholder.crt > /etc/haproxy/certs/000-placeholder.pem
  chmod 600 /etc/haproxy/certs/000-placeholder.pem
  rm -f /tmp/hc-placeholder.key /tmp/hc-placeholder.crt
fi

BIN="${PREFIX}/sbin/haproxy"
log "Installed: $("$BIN" -v | head -1)"
log "Binary   : ${BIN}"
echo
log "Next: run scripts/install.sh to install the controller and its services."
echo "     Powered by Ebdaa.me - https://ebdaa.me"
