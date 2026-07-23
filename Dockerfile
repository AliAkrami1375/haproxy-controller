# syntax=docker/dockerfile:1.7
#
# HAProxy Controller - a single self-contained image.
#
# Both halves are compiled from source that lives in this repository: HAProxy
# from third_party/haproxy, the controller from cmd/ and internal/. The build
# clones nothing and the runtime needs no init system, no sidecar and no
# database server -- the controller supervises HAProxy as its own child and
# keeps all state in SQLite.
#
# Powered by Ebdaa.me - https://ebdaa.me

# ---------------------------------------------------------------------------
# Stage 1: compile the vendored HAProxy source
# ---------------------------------------------------------------------------
FROM debian:trixie-slim AS haproxy-builder

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
        build-essential pkg-config \
        libssl-dev libpcre2-dev zlib1g-dev liblua5.4-dev

# The HAProxy source ships with this repository. See third_party/haproxy/VENDOR.md
# for its upstream origin and commit, and scripts/vendor-haproxy.sh to move to
# a different release.
COPY third_party/haproxy /usr/src/haproxy
WORKDIR /usr/src/haproxy

# USE_SYSTEMD is deliberately omitted: the controller supervises HAProxy in
# master-worker mode, so there is no service manager to notify.
RUN set -eux; \
    LUA_INC="$(pkg-config --cflags-only-I lua5.4 | sed 's/^-I//')"; \
    LUA_LIB="$(pkg-config --libs-only-L lua5.4 | tr -d ' ') $(pkg-config --libs-only-l lua5.4)"; \
    make -j "$(nproc)" \
        TARGET=linux-glibc \
        USE_OPENSSL=1 \
        USE_PCRE2=1 USE_PCRE2_JIT=1 \
        USE_ZLIB=1 \
        USE_LUA=1 LUA_INC="${LUA_INC}" LUA_LIB="${LUA_LIB}" \
        USE_THREAD=1 \
        USE_PROMEX=1 \
        USE_GETADDRINFO=1; \
    make install-bin PREFIX=/usr/local DESTDIR=/out; \
    /out/usr/local/sbin/haproxy -v

# ---------------------------------------------------------------------------
# Stage 2: compile the controller
# ---------------------------------------------------------------------------
FROM golang:1.26-trixie AS controller-builder

ARG VERSION=docker

WORKDIR /src

# Dependencies first so a source-only change reuses the module cache.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Fully static: the SQLite driver is pure Go, so the binary carries no
# runtime library dependencies at all.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/haproxy-controller ./cmd/haproxy-controller

# ---------------------------------------------------------------------------
# Stage 3: runtime
# ---------------------------------------------------------------------------
FROM debian:trixie-slim AS runtime

ARG VERSION=docker

LABEL org.opencontainers.image.title="HAProxy Controller" \
      org.opencontainers.image.description="Web control panel for HAProxy, with HAProxy built from source in the same image" \
      org.opencontainers.image.vendor="Ebdaa.me" \
      org.opencontainers.image.url="https://ebdaa.me" \
      org.opencontainers.image.source="https://ebdaa.me" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="MIT"

# Only the shared libraries HAProxy links against, plus openssl for generating
# the placeholder certificate on first boot. No build tools ship in this layer.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
        libssl3 libpcre2-8-0 zlib1g liblua5.4-0 \
        openssl ca-certificates tzdata \
 && groupadd --system haproxy \
 && useradd --system --gid haproxy --no-create-home \
            --home-dir /var/lib/haproxy --shell /usr/sbin/nologin haproxy

COPY --from=haproxy-builder /out/usr/local/sbin/haproxy /usr/local/sbin/haproxy
COPY --from=controller-builder /out/haproxy-controller  /usr/local/bin/haproxy-controller
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh

RUN chmod +x /usr/local/bin/entrypoint.sh \
 && mkdir -p /etc/haproxy/errors /etc/haproxy/maps /etc/haproxy-controller \
             /var/lib/haproxy /var/lib/haproxy-controller/backups /run/haproxy \
 && mkdir -p /etc/haproxy/certs && chmod 700 /etc/haproxy/certs \
 && chown -R haproxy:haproxy /var/lib/haproxy /run/haproxy \
 && haproxy -v

ENV HC_CONFIG=/etc/haproxy-controller/controller.json \
    HC_PROCESS_MODE=supervised \
    HC_LISTEN_ADDR=0.0.0.0 \
    HC_LISTEN_PORT=9000 \
    HC_HAPROXY_BIN=/usr/local/sbin/haproxy \
    HC_HAPROXY_CONFIG=/etc/haproxy/haproxy.cfg \
    HC_RUNTIME_SOCKET=/run/haproxy/admin.sock \
    HC_MASTER_SOCKET=/run/haproxy/master.sock \
    HC_DATA_DIR=/var/lib/haproxy-controller \
    HC_LOG_LEVEL=info

# State that must outlive the container: the database, the generated
# configuration, certificates and error pages.
VOLUME ["/var/lib/haproxy-controller", "/etc/haproxy", "/etc/haproxy-controller"]

# 9000 is the control panel; 80 and 443 are the usual HAProxy listeners.
# Publish whatever ports your frontends actually bind.
EXPOSE 9000 80 443

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD /usr/local/bin/haproxy-controller -healthcheck || exit 1

# The controller runs as PID 1: it reaps HAProxy, forwards shutdown signals
# and stops it gracefully on the way out.
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["haproxy-controller"]
