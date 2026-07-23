# HAProxy Controller

Complete control of HAProxy two ways: a professional **web control panel** and
a built-in **AI assistant** you can talk to in plain language. Your whole load
balancer configuration lives in a local SQLite database; the controller renders
a real `haproxy.cfg` from it, validates that file with HAProxy itself, and
applies it with one button — rolling back automatically if HAProxy does not
come back up.

Every part of the configuration can be built either by hand in the UI or by
asking the assistant, which drives the same validated, staged workflow through
a guided agent.

HAProxy's own source ships inside this repository and is compiled into the
same image, so there is one service, one container, and nothing to pull from
anywhere else.

**Powered by [Ebdaa.me](https://ebdaa.me)**

![Dashboard](docs/screenshots/dashboard.png)

---

## Contents

- [Quick start](#quick-start)
- [What you can control](#what-you-can-control)
- [The AI assistant](#the-ai-assistant)
- [The screens](#the-screens)
- [How a change reaches production](#how-a-change-reaches-production)
- [Configuration reference](#configuration-reference)
- [The HAProxy source](#the-haproxy-source)
- [Install on a host with systemd](#install-on-a-host-with-systemd)
- [Configuring the assistant](#configuring-the-assistant)
- [Security](#security)
- [Architecture](#architecture)
- [Development](#development)
- [Troubleshooting](#troubleshooting)

---

## Quick start

```bash
docker compose up -d --build
```

The build compiles HAProxy from `third_party/haproxy` and the controller from
this tree. First boot prints the administrator password exactly once:

```bash
docker logs haproxy-controller | grep -A6 'initial administrator'
```

Open `http://127.0.0.1:9000/`, sign in, and change the password when prompted.

<p align="center">
  <img src="docs/screenshots/login.png" alt="Sign in" width="520">
</p>

Without compose:

```bash
docker build -t haproxy-controller .

docker run -d --name haproxy-controller \
  -p 127.0.0.1:9000:9000 -p 80:80 -p 443:443 \
  -v hc-data:/var/lib/haproxy-controller \
  -v hc-etc:/etc/haproxy \
  -v hc-conf:/etc/haproxy-controller \
  haproxy-controller
```

`9000` is the control panel. Publish whatever ports your frontends bind — the
`80` and `443` mappings are only a starting point. A listener that is not
published on the host is not reachable from outside the container.

### Volumes

| Path | Holds |
| --- | --- |
| `/var/lib/haproxy-controller` | SQLite database, config versions, backups |
| `/etc/haproxy` | generated `haproxy.cfg`, certificates, error pages |
| `/etc/haproxy-controller` | controller settings, including the session secret |

Destroying and recreating the container keeps every account, certificate and
applied version as long as those volumes are reused.

---

## What you can control

Everything HAProxy does, from the browser:

- **Frontends** — listeners, TLS, HTTP options, HSTS, per-IP rate limiting,
  the built-in stats page
- **Backends** — load balancing algorithm, health checks, session persistence,
  timeouts, per-server weights and flags
- **Domains** — host-based routing with exact, subdomain, wildcard or regex
  matching, optional path prefixes, redirects and forced HTTPS
- **Certificates** — paste or upload PEM, validated on save, expiry tracked
- **Error pages** — authored and previewed in the browser, grouped into
  `http-errors` sections
- **Global and defaults** — process, logging, runtime socket and TLS defaults
- **Snippets** — anything the structured editors do not cover: `resolvers`,
  `peers`, `userlist`, `cache`, standalone `listen` sections, raw directives
- **Live status and control** — read from HAProxy's runtime socket; drain or
  re-enable a server without a reload
- **Versions** — every apply recorded, with one-click rollback
- **Users and audit** — three roles, and a log of every state-changing action
- **AI assistant** — build and change any of the above by chatting in plain language

---

## The AI assistant

The assistant turns a plain-language request into a correct HAProxy
configuration, using the same tools and the same safety model as the web UI.
It is designed as a **staged, self-validating agent** — not a text generator
that hands you a config file to paste. It never writes raw `haproxy.cfg`; it
operates the controller's own structured configuration and lets the panel
render and validate the result.

![The assistant](docs/screenshots/assistant.png)

### How it works — the waterfall

Each turn runs as an explicit pipeline, and every step is shown to you:

1. **Understand.** It reads the request and asks a single clarifying question
   only when the answer would change the result.
2. **Gather context.** It inspects the current configuration (frontends,
   backends, domains, certificates) so it builds on what exists and never
   duplicates a name.
3. **Build.** It makes the change with the smallest correct set of steps, in
   dependency order — backends and servers before the frontends and routes that
   reference them.
4. **Validate.** It renders the full configuration and runs HAProxy's own
   checker (`haproxy -c`).
5. **Repair.** If validation fails, it reads the diagnostics, fixes the cause,
   and validates again — looping until the configuration is valid.
6. **Report.** It summarises exactly what it changed and reminds you that the
   change is staged.

The controller enforces this even if a weaker model forgets to: if the agent
made changes but never validated, the controller validates itself before
reporting, so you are never told a broken configuration is ready.

Crucially, the assistant only ever **stages** changes — exactly like editing in
the UI. Nothing goes live until you open **Review & apply** and press Save &
Apply, which itself validates and rolls back on failure. Every change the
assistant makes is written to the audit log under your account, so its work is
fully traceable and reversible.

### What it can do

The agent has structured tools for backends and servers, frontends and
listeners, TLS, host-based routing, ACLs and ordered rules, branded error
pages, and raw snippets — plus read tools to inspect the current state and a
validate tool. Ask for things like:

> Create an HTTPS site for `www.example.com` load balancing across
> `10.0.0.11:8080` and `10.0.0.12:8080`, with health checks and forced HTTPS.

> Add an API backend for `api.example.com` using leastconn with two servers,
> and route `api.example.com/api` to it.

> Rate limit the public frontend to 100 requests per 10 seconds per client IP
> and add a branded 429 page.

It runs entirely on **free, tool-capable models** through
[OpenRouter](https://openrouter.ai) — no paid API is required. See
[Configuring the assistant](#configuring-the-assistant) to connect it.

---

## The screens

### Live status

Read straight from HAProxy's runtime socket: per-proxy counters, per-server
health and check results.

![Live status](docs/screenshots/status.png)

### Frontends

Each frontend lists what it binds and where unmatched traffic goes.

![Frontends](docs/screenshots/frontends.png)

The editor covers listeners, TLS, HSTS, rate limiting, the stats page, ACLs and
ordered rules. Rules are evaluated top to bottom and can be reordered.

![Frontend editor](docs/screenshots/frontend-edit.png)

### Backends

![Backends](docs/screenshots/backends.png)

Health checks use HAProxy's modern `http-check` syntax. Servers can be drained
or returned to service live, without a reload.

![Backend editor](docs/screenshots/backend-edit.png)

### Domains

Each entry becomes an ACL plus a routing or redirect rule on its frontend.

![Domains](docs/screenshots/domains.png)

### Certificates

PEM material is validated on save — the key must actually match the
certificate — and expiry is surfaced before it bites.

![Certificates](docs/screenshots/certificates.png)

### Error pages

Grouped into `http-errors` sections and attached to any frontend, backend or
the defaults section. Each page is written as a complete raw HTTP response,
so it is served even when every backend is down.

![Error pages](docs/screenshots/error-pages.png)

![Error page editor](docs/screenshots/error-page-edit.png)

### Global and defaults

![Global settings](docs/screenshots/global.png)

![Defaults](docs/screenshots/defaults.png)

### Snippets

For everything the structured editors do not model.

![Snippets](docs/screenshots/snippets.png)

### Review and apply

The exact file that will be written, already checked by HAProxy. Nothing you
edit is live until you press **Save & Apply**.

![Review and apply](docs/screenshots/config-review.png)

### Versions

![Versions](docs/screenshots/versions.png)

### Users

![Users](docs/screenshots/users.png)

### Settings

![Settings](docs/screenshots/settings.png)

### Audit log

![Audit log](docs/screenshots/audit.png)

---

## How a change reaches production

Nothing you edit takes effect until you apply it.

1. **Render** the configuration from the database.
2. **Stage** error pages and certificates into a scratch directory and run
   `haproxy -c` against them. The live tree is untouched at this point, so a
   broken configuration cannot reach your load balancer.
3. **Back up** the current `haproxy.cfg`.
4. **Write** error pages, certificates, then the configuration itself — each
   atomically, via a temporary file and a rename.
5. **Reload** HAProxy and confirm a *new worker is actually serving*.
6. **Roll back** to the previous file and reload again if it is not.

Step 5 is stricter than it sounds. A HAProxy reload can look successful while
the new worker dies on startup — a bind address that does not exist on this
host, a port already taken, an unreadable certificate. The master survives and
the *previous* worker keeps serving, so the change silently never happens. The
controller watches the master CLI until a genuinely new worker has taken over
and survived, and treats anything else as a failed apply.

Every attempt, successful or not, is recorded under **Versions**.

---

## Configuration reference

Process settings live in `/etc/haproxy-controller/controller.json`.
Precedence: defaults → config file → environment → command-line flags.

| Key | Environment | Default | Purpose |
| --- | --- | --- | --- |
| `listen_addr` | `HC_LISTEN_ADDR` | `0.0.0.0` | Panel bind address |
| `listen_port` | `HC_LISTEN_PORT` | `9000` | Panel port |
| `tls_enabled` | `HC_TLS_ENABLED` | `false` | Serve the panel over HTTPS |
| `tls_cert` / `tls_key` | `HC_TLS_CERT` / `HC_TLS_KEY` | — | Panel TLS key pair |
| `db_path` | `HC_DB_PATH` | `/var/lib/haproxy-controller/controller.db` | SQLite database |
| `haproxy_bin` | `HC_HAPROXY_BIN` | `/usr/local/sbin/haproxy` | Binary used to validate and run |
| `config_path` | `HC_HAPROXY_CONFIG` | `/etc/haproxy/haproxy.cfg` | File that gets written |
| `error_pages_dir` | — | `/etc/haproxy/errors` | Where error pages are written |
| `certs_dir` | — | `/etc/haproxy/certs` | Where certificates are written |
| `runtime_socket` | `HC_RUNTIME_SOCKET` | `/run/haproxy/admin.sock` | Stats socket for live status |
| `master_socket` | `HC_MASTER_SOCKET` | `/run/haproxy/master.sock` | `supervised` mode: master CLI |
| `process_mode` | `HC_PROCESS_MODE` | `command` | `command` (init system) or `supervised` (run HAProxy as a child) |
| `reload_command` | `HC_RELOAD_COMMAND` | `systemctl reload haproxy-managed` | `command` mode only |
| `status_command` | — | `systemctl is-active haproxy-managed` | `command` mode only |
| `session_ttl_minutes` | — | `720` | Session lifetime |

Flags: `-config`, `-listen`, `-port`, `-data-dir`, `-haproxy-bin`,
`-haproxy-config`, `-process-mode`, `-version`, `-healthcheck`.

Most of these are also editable from the Settings page. The assistant's
settings (enabled, model, and the encrypted API key) are stored in the database
and managed under Settings → Assistant, not in this file.

### Process modes

`supervised` (what the container uses) runs HAProxy as a direct child of the
controller in master-worker mode. Applying a configuration signals the master
to reload; no init system is involved. The process tree is:

```
PID 1  haproxy-controller
 └─ PID n  haproxy (master)
     └─ PID m  haproxy (worker)
```

Stopping the container sends `SIGTERM` to the controller, which soft-stops
HAProxy and waits for in-flight requests before exiting.

`command` (the default for a host install) drives HAProxy through the
configured reload, restart and status commands — normally systemd.

---

## The HAProxy source

`third_party/haproxy` holds the upstream HAProxy source, committed verbatim.
Both the Docker build and the host build compile that directory, so neither
downloads HAProxy. `third_party/haproxy/VENDOR.md` records the exact upstream
commit it came from.

Currently vendored: **HAProxy 3.2.21** — the 3.2 long-term-support branch with
all published fixes, commit `dbe43be3798e`.

To move to a different release, re-vendor and commit the result:

```bash
HAPROXY_REF=v3.2.22 ./scripts/vendor-haproxy.sh    # or: make vendor-haproxy
```

The script defaults to `git.haproxy.org/git/haproxy-3.2.git`, the per-branch
repository that carries patched stable releases. `github.com/haproxy/haproxy.git`
mirrors HAProxy's development tree and tags only `.0` releases, so it is not
the default; point at it explicitly for a development build:

```bash
HAPROXY_REPO=https://github.com/haproxy/haproxy.git \
HAPROXY_REF=v3.4.0 ./scripts/vendor-haproxy.sh
```

Vendoring is the only step that touches the network, and only when you change
HAProxy version.

---

## Install on a host with systemd

Requires Linux with systemd, and Go 1.22+ to build.

```bash
sudo ./scripts/build-haproxy.sh   # compile the vendored HAProxy
sudo ./scripts/install.sh         # install the controller and its services
```

`build-haproxy.sh` compiles `third_party/haproxy` out of tree and installs it
to `/usr/local/sbin/haproxy`. It downloads nothing.

Pick the control port at install time:

```bash
sudo CONTROL_PORT=9443 CONTROL_ADDR=127.0.0.1 ./scripts/install.sh
```

The first-run password appears once in the service log:

```bash
journalctl -u haproxy-controller | grep -A6 'initial administrator'
```

---

## Configuring the assistant

The assistant uses [OpenRouter](https://openrouter.ai), which offers a range of
capable models for free. An administrator connects it once under
**Settings → Assistant**.

![Assistant settings](docs/screenshots/ai-settings.png)

1. Create a free API key at `openrouter.ai/keys`.
2. In the panel, open **Settings → Assistant**, paste the key, and enable the
   assistant. The key is stored **encrypted** with the controller's session
   secret — never in plaintext.
3. Click **Load free models** to fetch the current list of free,
   **tool-capable** models directly from OpenRouter, and pick one. Tool support
   is required: the agent works by calling the configuration tools, so a model
   that cannot call tools will not work.
4. Use **Test connection** to confirm the key and model respond.

Operators and administrators can then use the **Assistant** page. Viewers
cannot, since they have read-only access.

A few notes:

- **Model quality varies.** Free models differ in how reliably they follow the
  staged workflow. If one struggles with a complex request, pick a larger free
  model or break the request into smaller steps. The controller's own
  validation and rollback protect you regardless of the model.
- **Rate limits.** Free models are rate limited by OpenRouter. If you hit a
  limit, wait a moment or switch models.
- **Privacy.** Requests include a summary of your configuration (names,
  addresses, ports) so the agent can work. They are sent to OpenRouter and the
  selected model provider. Do not enable the assistant if that is not
  acceptable for your environment.

---

## Security

- Passwords are hashed with bcrypt at cost 12; only the hash is stored.
- Session cookies are `HttpOnly`, `SameSite=Lax`, `Secure` under TLS, and
  `__Host-` prefixed when the panel serves HTTPS. Only a SHA-256 of the
  session token is persisted.
- Every state-changing request requires a per-session CSRF token and an
  `Origin` header matching the panel.
- Five consecutive failed sign-ins lock an account for fifteen minutes.
  Failures are indistinguishable and uniformly delayed.
- A Content-Security-Policy of `default-src 'none'`; the panel loads nothing
  from outside itself.
- Changing a password or a role invalidates that user's other sessions.
- The last active administrator cannot be deleted, demoted or disabled.
- Private keys are written mode `0600` into a `0700` directory.
- Values that become configuration directives are rejected if they contain
  newlines, so a form field cannot inject unrelated directives.
- The panel refuses to hand HAProxy a port the panel itself is listening on.
- The assistant's OpenRouter API key is stored encrypted (AES-256-GCM) with a
  key derived from the session secret, and is only ever shown masked.
- The assistant can only reach operators and administrators, only stages
  changes, and records every change it makes to the audit log.

**Do not expose the panel directly to the internet.** Bind it to `127.0.0.1`
and reach it over a VPN or SSH tunnel, or enable TLS and restrict access at
the network layer.

---

## Architecture

```
cmd/haproxy-controller/     entry point
internal/
  config/                   process configuration
  db/                       SQLite schema and migrations
  store/                    data access and validation
  hap/
    render.go               config generation
    render_proxy.go         frontend/backend rendering
    validate.go             haproxy -c
    deploy.go               stage, apply, roll back
    runtime.go              stats socket client
    process.go              init-system process manager
    supervisor.go           in-container process manager
  ai/
    openrouter.go           OpenRouter chat + model listing
    agent.go                the staged, self-validating agent loop
    tools.go                the agent's configuration tools
    crypto.go               API-key encryption at rest
  web/                      handlers, templates, assets
third_party/haproxy/        vendored HAProxy source (see VENDOR.md)
scripts/
  vendor-haproxy.sh         refresh the vendored source
  build-haproxy.sh          compile and install it
  install.sh                install the controller and services
docker/entrypoint.sh        container bootstrap
Dockerfile                  single self-contained image
systemd/                    unit files
```

The controller is a single static binary (`CGO_ENABLED=0`, pure-Go SQLite) with
templates and assets embedded, so it has no runtime dependencies of its own.

---

## Development

```bash
make build           # build bin/haproxy-controller
make test            # run the test suite
make check           # format, vet and test
make docker          # build the self-contained image
make vendor-haproxy  # refresh the vendored HAProxy source
make help            # list targets
```

---

## Troubleshooting

**The panel says HAProxy is not running.** On a fresh install that is normal:
HAProxy will not start without at least one listener. Add a frontend with a
bind and apply.

**Live statistics are unavailable.** The `stats socket` path in the Global
section must match `runtime_socket` under Settings. In the container both
default to `/run/haproxy/admin.sock`.

**An apply failed with "could not start a worker".** HAProxy accepted the file
but could not start on it — usually a bind address that does not exist on this
host, a port already in use, or an unreadable certificate. The previous
configuration was restored automatically; the reason is in the container log:

```bash
docker logs haproxy-controller | grep haproxy
```

**A TLS listener fails to start.** A `crt <dir>/` reference needs at least one
PEM in the certificate directory. The container generates a placeholder on
first boot; add a real certificate under **Certificates**.

---

## Licensing

HAProxy Controller (everything outside `third_party/`) is released under the
**MIT license** — see [LICENSE](LICENSE).

`third_party/haproxy/` is an unmodified copy of the **HAProxy** source, licensed
separately under **GPL-2.0** (with an LGPL-2.1 exception for some bundled
libraries); see `third_party/haproxy/LICENSE`. The controller runs HAProxy as a
separate process rather than linking it, so the two are an aggregate of
independently licensed works. If you redistribute a build, you are also
redistributing HAProxy and should keep its license and source available, which
this repository already does.

---

Powered by **[Ebdaa.me](https://ebdaa.me)**
