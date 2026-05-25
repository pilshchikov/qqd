# qqd

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/pilshchikov/qqd)](go.mod)
[![CI](https://github.com/pilshchikov/qqd/actions/workflows/ci.yml/badge.svg)](https://github.com/pilshchikov/qqd/actions/workflows/ci.yml)

Deploy containerized apps to your own VMs over SSH. `qqd` manages Podman services through systemd or direct Podman runs, routes traffic through Traefik or Caddy, and supports health-aware rollouts where the service topology allows it. No Kubernetes.

One config file. One command. Your services are live.

```
qqd deploy -c app.yaml
```

That command is the deploy flow, not just a container restart:

```text
qqd deploy -c app.yaml
    |
    SSH into target
    |-- sync source (git or rsync)
    |-- build/pull only missing images
    |-- generate unit files, write only changed ones
    |-- start all units (idempotent)
    |-- restart only changed services:
    |     HTTP-exposed       → blue-green (proxy switch)
    |     replicated         → rolling restart (one at a time)
    |     with health check  → wait healthy before next
    |     otherwise          → direct restart
    '-- verify all units active
```

> **Production readiness.** `qqd` is built for VM-based deployments and is still pre-1.0. The [Claim Matrix](docs/claims.md) shows what is covered by CI, opt-in integration tests, or unit tests, and the [Safety Model](docs/safety-model.md) documents rollout rules, failure modes, and recovery.

## Who is this for

- **Solo devs and small teams** deploying to VMs who don't want Kubernetes but still want rollback, health checks, and zero-downtime deploys where eligible
- **Multi-cloud setups** - backend on AWS, file service near your data, frontend on a different region - all from one config, one command
- **Anyone replacing** manual SSH + Docker Compose workflows, Ansible deploy playbooks, or looking for a lighter alternative to Kamal

## Why qqd

- **Single binary** - one Go binary on your machine, no local runtime dependencies. The target needs SSH, Podman, and shell tooling - see [Setup](docs/setup.md). Two lifecycle backends: systemd (when present) or direct (`podman run --restart=...` with `qqd.*` labels). Auto-selects per target so the same config works on a Linux VM, a macOS laptop with Podman Machine, or a CI runner - see [Lifecycle Backends](docs/lifecycle.md)
- **Zero-downtime strategies** - blue-green and rolling deploys for HTTP-exposed and replicated services with health checks. Other shapes get a direct restart - see [Safety Model](docs/safety-model.md) for the exact rules
- **YAML, JSON, or HOCON config** - auto-detected by file extension. The YAML parser is a small custom subset (no anchors/multiline scalars/tags); see [YAML Subset](docs/yaml-subset.md) for what's supported
- **Fully open-source stack** - Podman + optional systemd + Traefik/Caddy
- **No daemon** - qqd runs locally, SSHs into targets, does its job, exits. Per-target deploy lock prevents concurrent runs from racing
- **Podman-focused** - rootless, daemonless Podman by default. Docker and Compose are supported only as migration/import sources into Podman
- **Real rollback** - auto-rollback on health-check failure, release history tracked per deploy, one-command manual rollback. Restores the image and proxy config; does NOT restore volume data - see [Limitations](docs/limitations.md#rollback-scope)
- **Safety first** - `plan` (with risk surfacing and JSON output), `validate`, `doctor`, `--dry-run` before you touch production. `migrate` requires explicit confirmation for destructive ops

## What changes with qqd

**Without qqd** - you SSH into each server, pull images, edit compose files, restart containers, check if they're healthy, configure Nginx, hope nothing breaks during the restart, and repeat for every target. Rollback means "SSH in and figure it out."

**With qqd** - you write one config file describing your services and targets. Then:

```bash
qqd deploy -c app.yaml              # deploy everything; zero downtime where eligible
qqd rollback -c app.yaml            # broke something? one command to restore
qqd status -c app.yaml              # see what's running across all targets
```

qqd handles SSH, image builds, Podman service management, proxy routing, health checks, blue-green switching, and release tracking. You describe what you want - qqd figures out the safest supported path for that service shape.

## How It Works

On Linux hosts with systemd, qqd generates Podman [Quadlet](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html) `.container` files from your config and installs them on the target. That gives you automatic restarts, boot startup, structured logging via journalctl, and per-project network isolation with DNS.

On targets without a usable systemd session, qqd can fall back to a direct lifecycle that runs `podman run --restart=always` with `qqd.*` labels. This keeps local Podman Machine, minimal Linux, and CI targets usable, with different reboot and zero-downtime tradeoffs documented in [Lifecycle Backends](docs/lifecycle.md).

When you define an `expose` block, qqd automatically manages a reverse proxy container on the
target for HTTP path routing, TLS termination, load balancing across replicas, and the traffic
switching that makes blue-green zero-downtime deploys possible.

Two proxy providers are built in: [Traefik](https://traefik.io/traefik/) v3.6 (default) and
[Caddy](https://caddyserver.com/) v2. Set `proxy: caddy` in your config to switch. **Capabilities
differ by provider**: Traefik supports HTTP, TLS, and raw TCP passthrough; Caddy supports HTTP and
TLS only — `qqd validate` rejects any config that combines `proxy: caddy` with a raw TCP expose
entry. See [docs/proxy-caddy.md](docs/proxy-caddy.md). The proxy layer is pluggable via a
`ProxyProvider` interface - additional proxies can be added by implementing it. You don't install
or configure the proxy yourself - qqd pulls the image and generates all config automatically.
If your services don't need external traffic routing
(no `expose` block), no proxy is used at all.

## Zero-Downtime Deployments

When a service's image changes, qqd picks the right strategy automatically - no configuration needed:

| Service type | Strategy |
|---|---|
| HTTP-exposed, non-replicated | **Blue-green**: start new slot, switch proxy traffic, stop old |
| Exposed, replicated | **Rolling with drain**: remove from proxy, restart, wait healthy, add back |
| Replicated + health check | **Rolling restart**: one at a time, wait healthy before next |
| Everything else | **Direct restart** |

This is automatic. You don't configure a deploy strategy - qqd picks the right one based on your service's `expose` and `replicas` settings.

## Install

**From GitHub releases** (any machine or CI):

```bash
curl -fsSL https://raw.githubusercontent.com/pilshchikov/qqd/main/install.sh | sh
```

The installer downloads the latest GitHub release binary for your OS/architecture, verifies it against `checksums.txt`, and installs it as `qqd`.

Options:

```bash
# Install to a specific directory
curl -fsSL https://raw.githubusercontent.com/pilshchikov/qqd/main/install.sh | sh -s -- -d /usr/local/bin

# Install a specific version
curl -fsSL https://raw.githubusercontent.com/pilshchikov/qqd/main/install.sh | sh -s -- -v v2026.05.24.42
```

**In a Dockerfile:**

```dockerfile
RUN curl -fsSL https://raw.githubusercontent.com/pilshchikov/qqd/main/install.sh | sh -s -- -d /usr/local/bin
```

**With Go** (requires Go 1.24+):

```bash
go install github.com/pilshchikov/qqd/cmd/qqd@latest
```

**From source** (includes man page):

```bash
make install
```

`install.sh` is the public release installer. Source builds use the Makefile.

## 60-second local demo

Try `qqd` against your local Podman without a remote VM. This deploys nginx and exposes it on `localhost:18080` - one config, one command, no SSH.

```bash
cat > nginx-local.yaml <<'EOF'
name: nginx-local
services:
  web:
    image: docker.io/library/nginx:1
targets:
  local:
    host: local
    expose:
      18080:
        "/": "web:80"
EOF

qqd init -c nginx-local.yaml
curl http://localhost:18080
qqd destroy -c nginx-local.yaml -t local
```

If that works end-to-end, your local qqd and Podman setup is working. Continue to the full Quick Start below for an SSH target.

## Quick Start

**1. Write a config** (YAML, JSON, or HOCON - auto-detected by extension):

```yaml
# app.yaml
name: my-app
repo: "https://github.com/org/my-app.git"
branch: main

services:
  db:
    image: "docker.io/library/postgres:16.1"
    volumes:
      - "${DB_PATH}:/var/lib/postgresql/data:z,U"
    env:
      POSTGRES_PASSWORD: "${PG_PASSWORD}"
  server:
    image: "ghcr.io/org/my-app/server:1.0"
    dockerfile: backend/Dockerfile
    context: backend
    replicas: 2
    health:
      path: /api/health
      port: 8080
    depends_on:
      - db
    env:
      DB_HOST: my-app-db

targets:
  prod:
    host: "192.0.2.10"
    user: ec2-user
    ssh_key: "~/.ssh/my-key.pem"
    repo_dir: /home/ec2-user/my-app
    dirs:
      - "${DB_PATH}"
    env:
      DB_PATH: /home/ec2-user/pg-data
      PG_PASSWORD: ""
    expose:
      80:
        "/api/": "server:8080"
        "/": "server:8080"
      5432: "db:5432"
```

**2. Add secrets** (not committed to git - can be any format):

```yaml
# secrets.yaml
targets:
  prod:
    env:
      PG_PASSWORD: my-secret-password
```

**3. Deploy:**

```bash
qqd init -c app.yaml -c secrets.yaml      # first time
qqd deploy -c app.yaml -c secrets.yaml     # subsequent deploys
qqd update -c app.yaml -c secrets.yaml     # bump versions and redeploy
```

## Commands

| Command | Description |
|---------|-------------|
| `plan` | Show deployment plan without executing |
| `init` | First-time setup: clone repo, build images, install Quadlet, start services |
| `deploy` | Idempotent: build/pull only missing images, restart only changed services |
| `build` | Build/pull images only, no deploy |
| `update` | Bump image version(s) in config, then deploy updated services |
| `status` | Show service state, image, and uptime per target |
| `logs` | Stream container logs (all services/replicas, prefixed output) |
| `rollback` | Restore previous release (or rollback a single service) |
| `history` | Show deployment release history per target |
| `stop` | Stop service units |
| `start` | Start service units |
| `destroy` | Stop/disable units and remove Quadlet files |
| `clean` | Remove project containers and unused images |
| `doctor` | Check target environment for common problems |
| `validate` | Check config for errors without deploying |
| `import` | Convert a Docker Compose file to qqd config |
| `migrate` | Migrate a running Compose or Swarm stack to Podman-backed qqd |
| `convert` | Convert config between formats (HOCON, YAML, JSON) |
| `man` | Open the man page |
| `docs` | Generate CLI documentation (Markdown) |
| `manifest` | Emit the full qqd surface (commands, flags, config schema, pitfalls) as JSON or Markdown for AI agents and tooling |

```bash
qqd plan -c app.yaml                       # preview what deploy would do
qqd deploy -c app.yaml                     # deploy all services to all targets
qqd deploy -c app.yaml --dry-run           # show plan, don't execute
qqd deploy -c app.yaml -t prod server      # deploy one service to one target
qqd update -c app.yaml server              # auto-bump server version
qqd update -c app.yaml server=2.0          # set explicit version
qqd status -c app.yaml                     # check what's running
qqd status -c app.yaml --output json       # machine-readable JSON output
qqd logs -c app.yaml server                # stream logs
qqd rollback -c app.yaml                   # restore previous release
qqd rollback -c app.yaml server            # rollback only one service
qqd history -c app.yaml                    # show release history
qqd doctor -c app.yaml                     # diagnose target issues
qqd validate -c app.yaml                   # check config for errors
```

## Compared to

| | qqd | Kamal | Compose + SSH | Ansible |
|---|---|---|---|---|
| **Zero-downtime** | HTTP blue-green + rolling where eligible | Rolling | Manual | Manual |
| **Config format** | YAML, JSON, or HOCON | YAML only | YAML only | YAML only |
| **Multi-target** | One config - different services, env, routing per target | Separate files per env | Single host | Inventory |
| **Multi-service** | Path-based routing | Hostname-based | Native | You build it |
| **Rollback** | Image/config rollback (auto on failed health + manual); no data rollback | Manual | None | Manual |
| **Pre-built images** | Yes | No (build only) | Yes | Yes |
| **Proxy** | Traefik (HTTP+TLS+TCP) or Caddy (HTTP+TLS); pluggable | Kamal Proxy (built in) | None | You build it |

**vs [Kamal](https://kamal-deploy.org/)** - the closest alternative; both deploy over SSH and include downtime-reduction strategies.

- qqd uses rootless, daemonless Podman
- qqd defines all targets in one config with per-target services, env, and routing - deploy backend to AWS, file services near data sources, frontend to edge, all with one `qqd deploy`. Kamal uses separate config files per environment
- qqd picks deploy strategy automatically per service (blue-green, rolling, or direct). Kamal uses a single rolling approach
- qqd routes multiple services via path rules (`/api/ → backend`, `/ → frontend`). Kamal routes by hostname (one app per domain)
- qqd supports build on target, build on a dedicated host, or pull pre-built images from registry. Kamal always builds from source
- qqd proxy is pluggable (Traefik or Caddy built in). Kamal uses its own [Kamal Proxy](https://github.com/basecamp/kamal-proxy) (MIT-licensed, not pluggable)

**vs Compose + SSH** - Compose defines services but doesn't deploy them.

- No zero-downtime restarts, no health-aware rollouts, no rollback
- No reverse proxy - you manage Nginx/Traefik yourself
- qqd adds all of that with one command over SSH

**vs Ansible** - Ansible can do anything, but you write everything yourself.

- Playbooks, roles, Jinja2 templates, inventory files - steep learning curve
- Health checks, rollback, proxy config - all manual
- qqd gives you all of that from one config file

## Key Features

**Config & secrets:**
- Multi-format config (YAML, JSON, HOCON) - auto-detected, mixable across overlays
- Secrets overlay - `-c secrets.yaml` deep-merges on top of the base config
- Env files - `env_file: ".env"` or `env_file: [".env", ".env.prod"]` with last-wins precedence
- File references - `KEY: "file::secrets/key.json"` reads local files at deploy time
- Secret redaction - plan output masks `*_TOKEN`, `*_SECRET`, `*_PASSWORD`, `*_KEY` values

**Deploy & routing:**
- Replicas - `replicas: 2` produces load-balanced containers with rolling restarts
- Reverse proxy - per-target `expose` block with HTTP routing and TLS for both built-in providers; raw TCP passthrough is supported on Traefik only (Caddy rejects it at `validate`). Pluggable via `ProxyProvider`
- Health checks - `health: { path: "/health", port: 8080 }` enables readiness-aware deploys
- Resource limits - `resources: { cpus: "2", memory: "1g" }`
- Deploy hooks - project and per-service `pre_deploy`, `post_deploy`, `pre_build`, `post_build`
- Build strategies - build on target (default), on a dedicated build host, or pull pre-built images (pluggable via `BuildStrategy`)

**Operations:**
- Release history - each deploy saves a record; auto-rollback on failure, `rollback` for manual restore
- Plan & dry-run - `qqd plan` and `--dry-run` preview changes before applying
- Validation - `qqd validate` catches config errors before deploy
- Diagnostics - `qqd doctor` checks SSH, container runtime, systemd, disk space
- JSON output - `qqd status --output json` for CI/automation
- Local targets - `host: "local"` runs on the local machine (no SSH)
- Upload sync - `sync: "upload"` rsyncs local files instead of git clone
- Approval flow - `--approve` to skip confirmation prompt (for CI/scripts)
- Strict SSH - host key verification by default, `insecure_host_key: true` to opt out

## Requirements

**Your machine:** SSH client

**Target host:** Podman 4.0+, git, bash. systemd is recommended for production targets; qqd can use direct Podman runs when systemd is unavailable.

**Managed automatically:** [Traefik](https://traefik.io/traefik/) v3.6 or [Caddy](https://caddyserver.com/) v2 - pulled and configured by qqd when an `expose` block is present. No manual install needed.

```bash
# Install Podman
sudo dnf install -y podman git          # RHEL/AlmaLinux
sudo apt install -y podman git          # Ubuntu/Debian
```

## Podman Runtime

qqd deploys with Podman. You can omit `runtime` or set `runtime: "podman"` for clarity.
`runtime: "docker"` is rejected for deploys. To bring a running Docker Compose
or Swarm stack onto qqd, use `qqd migrate --from compose` (or `--from swarm`)
with `--dry-run` first to preview the destructive actions.

## For AI agents and tooling

If you're an AI agent (or any program) that wants to drive qqd, run `qqd manifest`. It prints one structured document covering every command, flag, config field, output format, lifecycle backend, and known pitfall:

```bash
qqd manifest                     # JSON (default) — stable across patch releases
qqd manifest --format md         # self-contained Markdown brief
qqd manifest | jq '.commands[] | {name, summary}'
```

The manifest is generated from the command registry, the common/per-command flag registries, and `qqd:`-tagged config structs via reflection. Guidance sections such as concepts and pitfalls are curated in code and covered by tests.

## Documentation

| Document | Description |
|----------|-------------|
| [Configuration Reference](docs/configuration.md) | Full config format: services, targets, expose, secrets, env, volumes |
| [Zero-Downtime Deploys](docs/zero-downtime.md) | Blue-green, rolling restarts, health checks, startup delay |
| [Command Reference](docs/commands.md) | Detailed usage, flags, and examples for every command |
| [CLI Reference](docs/cli-reference.md) | Auto-generated from the binary; checked by CI for drift |
| [Setup Guide](docs/setup.md) | Target host setup, macOS Podman Machine, SELinux, DNS, ports |
| [Operations Guide](docs/operations.md) | Manual operations, diagnostics, and troubleshooting on targets |
| [Safety Model](docs/safety-model.md) | What `qqd` guarantees, failure modes, recovery, concurrent deploys |
| [Limitations](docs/limitations.md) | What `qqd` does not do |
| [YAML Subset](docs/yaml-subset.md) | Exactly which YAML features are supported (and which are rejected) |
| [Caddy Proxy](docs/proxy-caddy.md) | Caddy provider model, parity with Traefik, and what is not supported (e.g. raw TCP) |
| [Production Checklist](docs/production-checklist.md) | Step-by-step checklist before pointing qqd at a production target |
| [Integration Tests](docs/integration-tests.md) | How to run the opt-in integration suite (`QQD_INTEGRATION=1`), what it covers, what's still missing |
| [Claim Matrix](docs/claims.md) | Each top-line claim and its current evidence |

## Project Layout

```
cmd/qqd/main.go          CLI entrypoint
install.sh               GitHub release installer
Makefile                 local build/test/install/release targets
.github/workflows/
  ci.yml                 build + test + vet + docs drift on PRs
  release.yml            automatic GitHub releases on main/master push
docs/                    documentation (configuration, commands, setup, operations, zero-downtime)
man/qqd.1                manual page

internal/qqd/
  cli.go                 command routing and argument parsing
  config.go              config loading, format detection, target resolution, secrets overlay
  configformat.go        JSON and YAML parsers, format auto-detection
  hocon.go               HOCON parser
  types.go               config types (services, targets, hooks, env_file, build)
  deploy.go              core deploy orchestration (init, deploy, build)
  images.go              image build/pull strategies (local, build-host, github-actions)
  sync.go                source sync (git clone/fetch, rsync upload)
  slot.go                blue-green zero-downtime slot deployment
  restart.go             restart strategies (rolling, drain, health wait)
  lifecycle.go           status, logs, rollback, history, stop, start, destroy, clean
  release.go             release history (save, list, rollback, trim)
  quadlet.go             Quadlet file rendering (containers, replicas, network)
  proxy.go               ProxyProvider interface + Traefik implementation
  caddy.go               Caddy proxy provider implementation
  update.go              version bump and config rewrite
  validate.go            config validation (deps, ports, health, build strategy, TLS)
  doctor.go              target diagnostics (SSH, podman, systemd, disk, lingering)
  runtime.go             ContainerRuntime interface + Podman implementation
  executor.go            local + SSH command executors
  helpers.go             deploy helpers (container names, units, file ops)
  util.go                shared utilities (merge, sort, shell quoting, var expansion)
  envfile.go             .env file parser
  redact.go              secret value redaction for plan output
  json.go                JSON status output types
  docsgen.go             CLI help, documentation, and embedded config reference
  color.go               terminal color output
  spinner.go             progress spinner
```

## Test

```bash
go test ./internal/qqd/ -count=1
go test ./internal/qqd/ -count=1 -race
```

## CI / Release

GitHub Actions workflows automate testing and releases:

- **CI** (`.github/workflows/ci.yml`) - runs on pull requests to `main` or `master`. Builds the binary, runs the full test suite with race detection, runs `go vet`, and checks generated CLI docs for drift.
- **Release** (`.github/workflows/release.yml`) - on every push to `main` or `master`, creates a GitHub release named `vYYYY.MM.DD.<run_number>`, builds linux/darwin x amd64/arm64 binaries, writes `checksums.txt`, and marks it as the latest release.

Release assets are direct executables:

```text
qqd_darwin_amd64
qqd_darwin_arm64
qqd_linux_amd64
qqd_linux_arm64
checksums.txt
```

Release versions are generated by GitHub Actions. For example, the 42nd workflow run on May 24, 2026 publishes:

```text
v2026.05.24.42
```
