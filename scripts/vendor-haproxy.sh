#!/usr/bin/env bash
#
# Vendor the HAProxy source tree into third_party/haproxy.
#
# The build compiles that directory directly, so building this project never
# clones HAProxy from the network. Run this script only when you want to move
# to a different HAProxy release, then commit the result.
#
# Powered by Ebdaa.me - https://ebdaa.me
#
# Usage:
#   ./scripts/vendor-haproxy.sh                       # re-vendor the pinned release
#   HAPROXY_REF=v3.2.22 ./scripts/vendor-haproxy.sh   # move to another release
#   HAPROXY_REPO=https://github.com/haproxy/haproxy.git \
#     HAPROXY_REF=v3.4.0 ./scripts/vendor-haproxy.sh
#
set -euo pipefail

# The per-branch repository carries the patched stable releases. The GitHub
# mirror only tags .0 releases, so it is not the default here.
HAPROXY_REPO="${HAPROXY_REPO:-https://git.haproxy.org/git/haproxy-3.2.git}"
HAPROXY_REF="${HAPROXY_REF:-v3.2.21}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# NOT "vendor/": Go reserves that directory name at a module root and would
# switch the toolchain into vendored-dependency mode.
DEST="${ROOT}/third_party/haproxy"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m==>\033[0m %s\n' "$*" >&2; exit 1; }

command -v git >/dev/null 2>&1 || die "git is required."

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

log "Fetching ${HAPROXY_REF} from ${HAPROXY_REPO}"
git init -q "${TMP}/src"
git -C "${TMP}/src" remote add origin "$HAPROXY_REPO"
git -C "${TMP}/src" fetch --depth 1 --quiet origin "$HAPROXY_REF" \
  || die "Ref ${HAPROXY_REF} not found in ${HAPROXY_REPO}"
git -C "${TMP}/src" checkout --quiet --detach FETCH_HEAD

COMMIT="$(git -C "${TMP}/src" rev-parse HEAD)"
COMMIT_DATE="$(git -C "${TMP}/src" log -1 --format=%cs)"
VERSION="$(cat "${TMP}/src/VERSION" 2>/dev/null || true)"
[ -n "$VERSION" ] || VERSION="$HAPROXY_REF"

# Drop git metadata: the source is committed here as plain files, not as a
# nested repository or submodule.
rm -rf "${TMP}/src/.git"

# Trim what the build never touches. The regression and fuzzing suites are the
# bulk of the tree and are not needed to compile or run HAProxy.
for extra in reg-tests tests scripts/build-vtest.sh .github .gitignore .cirrus.yml; do
  rm -rf "${TMP}/src/${extra}"
done

log "Replacing ${DEST}"
rm -rf "$DEST"
mkdir -p "$(dirname "$DEST")"
mv "${TMP}/src" "$DEST"

cat > "${DEST}/VENDOR.md" <<META
# Vendored HAProxy source

This directory is upstream HAProxy, committed verbatim so that building this
project requires no network access to fetch it. Do not edit these files: any
local change would be lost the next time the tree is re-vendored.

| | |
| --- | --- |
| Upstream repository | \`${HAPROXY_REPO}\` |
| Reference | \`${HAPROXY_REF}\` |
| Commit | \`${COMMIT}\` |
| Commit date | ${COMMIT_DATE} |
| Version | ${VERSION} |
| Vendored on | $(date -u +%Y-%m-%d) |

HAProxy is distributed under the GPL v2 (with an LGPL v2.1 exception for
its included libraries); see \`LICENSE\` and \`doc/\` in this directory.

To move to a different release:

\`\`\`bash
HAPROXY_REF=v3.2.22 ./scripts/vendor-haproxy.sh
\`\`\`

The regression and fuzzing test suites are removed during vendoring because
they are not needed to build or run HAProxy.

Powered by [Ebdaa.me](https://ebdaa.me)
META

log "Vendored HAProxy ${VERSION} (${COMMIT:0:12}, ${COMMIT_DATE})"
log "Location: third_party/haproxy  ($(du -sh "$DEST" | cut -f1))"
