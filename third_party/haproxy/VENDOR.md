# Vendored HAProxy source

This directory is upstream HAProxy, committed verbatim so that building this
project requires no network access to fetch it. Do not edit these files: any
local change would be lost the next time the tree is re-vendored.

| | |
| --- | --- |
| Upstream repository | `https://git.haproxy.org/git/haproxy-3.2.git` |
| Reference | `v3.2.21` |
| Commit | `dbe43be3798e5054bf9fcf4ef281060ffa38a352` |
| Commit date | 2026-07-03 |
| Version | 3.2.21 |
| Vendored on | 2026-07-23 |

HAProxy is distributed under the GPL v2 (with an LGPL v2.1 exception for
its included libraries); see `LICENSE` and `doc/` in this directory.

To move to a different release:

```bash
HAPROXY_REF=v3.2.22 ./scripts/vendor-haproxy.sh
```

The regression and fuzzing test suites are removed during vendoring because
they are not needed to build or run HAProxy.

Powered by [Ebdaa.me](https://ebdaa.me)
