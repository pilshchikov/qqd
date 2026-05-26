# Command Reference

This is the curated, prose-style command reference. For the **authoritative** listing of every command, flag, and exact usage line - generated from the binary and checked by CI on every change - see [CLI Reference](cli-reference.md).

## Common Flags

| Flag | Description |
|------|-------------|
| `-c <config>` | Config file (required, repeatable). Each subsequent `-c` is deep-merged on top |
| `-t <target>` | Target name; omit if config has only one target |
| `--rebuild` | Force rebuilding images even when they already exist (init, deploy, build, update) |
| `--approve` | Skip the interactive confirmation prompt (deploy, update) |
| `--no-build` | Skip all image builds (deploy) |
| `--config-only` | Update config and restart only, no sync or build (deploy) |
| `--dry-run` | Show the plan without executing any changes (deploy, migrate) |
| `--yes`, `-y` | Skip the destructive-action confirmation prompt (migrate) |
| `--output json` | Machine-readable JSON output (plan, status) |
| `--force-unlock` | Take the deploy lock even if another holder is recorded on the target (init, deploy, build, rollback, destroy). Only safe if you are sure no other deploy is running |
| `-h`, `--help` | Show help for the command |

## Commands

### plan

Show deployment plan without executing. Displays per-target breakdown of services, images, build actions, deployment modes (standard, slot-based, replicated), environment variables, and risks (`info`/`warn`/`danger`: missing health checks, mutable image tags, services with volumes, Caddy TCP passthrough, etc.). Secret values are automatically redacted.

Supports `--output json` for CI/automation. The JSON shape is `{ project, runtime, proxy, sync, mode, targets[], risks[] }`. CI gates can fail when any risk has level `"danger"`:

```bash
qqd plan -c app.yaml --output json | jq -e '.risks | map(select(.level=="danger")) | length == 0'
```

For exact usage syntax, run `qqd help plan` or see [cli-reference.md](cli-reference.md).

### init

First-time setup on target(s): clone repo, create dirs, build/pull images, install units, start services. Acquires the deploy lock; pass `--force-unlock` to override a stale lock.

Pipeline:
```
SSH to target
  |-- mkdir -p <repo_dir>
  |-- git clone <repo> (if not cloned yet)
  |-- git fetch --all && git reset --hard origin/<branch>
  |-- mkdir -p <dirs> (target directories)
  |-- for each service:
  |     image missing + has dockerfile → build
  |     image missing + no dockerfile  → pull
  |     image exists                   → skip
  |-- write unit files to systemd directory
  |-- systemctl daemon-reload
  |-- systemctl start <all units>
  '-- verify: systemctl is-active <each unit>
```

### deploy

Idempotent deploy: build/pull only missing images, apply changed services. Acquires the deploy lock; pass `--force-unlock` to override a stale lock.

Shows a plan and asks for confirmation before proceeding. Use `--approve` to skip. Use `--dry-run` to show the plan without executing any changes. Use `--no-build` to skip all image builds. Use `--config-only` to update config and restart services without syncing or building.

Each deploy saves a release record for rollback.

Full deploy (no service args) removes services deleted from the config. Partial deploy (e.g. `qqd deploy server`) leaves other services untouched.

Pipeline:
```
SSH to target
  |-- sync source (git clone/fetch or rsync)
  |-- mkdir -p <dirs>
  |-- for each service:
  |     image exists → skip
  |     has dockerfile → build → mark changed
  |     no dockerfile  → pull  → mark changed
  |-- write unit files, daemon-reload
  |-- remove stale unit files (full deploy: all removed services)
  |-- detect config changes (unit files + proxy container)
  |-- systemctl start <all units>
  |-- systemctl restart <changed units only>
  '-- verify: systemctl is-active <each unit>
```

### build

Build/pull images only, without deploying or restarting. Acquires the deploy lock; pass `--force-unlock` to override a stale lock.

No unit files are written, no services are restarted.

### update

Bump or set image version(s), then deploy updated services. Shows a plan and asks for confirmation. Use `--approve` to skip.

Without service args: auto-bumps all services with version tags (increments rightmost number: `1.44` → `1.45`, `0.1-b7` → `0.1-b8`).

With service args: bumps or sets versions for listed services.

```bash
qqd update -c app.yaml server              # auto-increment
qqd update -c app.yaml server=2.0          # explicit version
qqd update -c app.yaml server frontend     # multiple services
```

After bumping, deploys only the updated services.

### status

Show service state, image, creation time, and uptime per target.

Output format: `image:tag (YYYY-MM-DD HH:MM:SS UTC, up Xh Ym)`

Blue-green services show the active slot: `server (blue): active ...`

Use `--output json` for machine-readable JSON output (structured as `StatusResult` with per-target service details).

Fails fast if SSH connectivity to the target fails.

### logs

Stream container logs.

```
qqd logs -c <config> [-c <overlay>...] [-t <target>] [services...]
```

Without service args, streams all services. Replicated services stream all replicas. Multiple containers are prefixed with the container name.

### rollback

Restore the previous release on the target. Acquires the deploy lock; pass `--force-unlock` to override a stale lock.

Without a service name, rolls back all services to the previous release. With a service name, rolls back only that service. Previous release images are pulled if not already present on the target. Unit files are rewritten to match the restored images, and only changed services are restarted.

The rollback itself is saved as a new release in the history.

#### Auto-rollback on failure

When `deploy` or `update` fails during the install/restart phase, qqd automatically attempts to roll back to the latest successful release. If a previous release exists, qqd restores the old images (pulling them if needed) and re-installs the previous configuration. If no previous release is available or the rollback itself fails, the original error is reported. Auto-rollback is skipped when the user interrupts the deploy (Ctrl+C).

### history

Show deployment release history per target. Lists recent releases stored on each target with release ID, timestamp, and service-to-image mappings. The most recent release is marked. Up to 10 releases are kept per target.

### stop

Stop service units (preserves Quadlet files and images).

### start

Start previously stopped service units.

### destroy

Stop/disable units and remove generated unit files. Images are NOT removed. Acquires the deploy lock; pass `--force-unlock` to override a stale lock.

### clean

Remove project containers and unused images from targets.

### doctor

Check target environment for common problems. Read-only — does not take the deploy lock.

Runs diagnostic checks on each target:
- SSH connectivity
- Podman availability and version
- systemd user session status
- Quadlet unit directory exists
- Disk space usage (warns above 90%)
- User lingering enabled (required for services to persist after logout)

Each check runs independently - one failure doesn't prevent others. Returns non-zero exit code if any check reports an error.

### validate

Check config for errors without deploying. Read-only and offline — does not connect to any target.

Validates the configuration for semantic errors:
- `depends_on` references to undefined services
- Circular dependency detection
- Port range validation (1-65535)
- TLS configuration completeness (port, certs_dir, server_name)
- Health check port inference (error if port can't be inferred from expose routes)
- Build strategy requirements (build-host needs host/user, github-actions forbids dockerfile)
- Mutable image tag warnings (`:latest`, `:main`, etc.)
- **`proxy: caddy` + raw TCP expose entry → hard error.** Caddy's built-in `reverse_proxy` is HTTP-only; `qqd` will not deploy a known-broken config. See [docs/proxy-caddy.md](proxy-caddy.md).

Exits with error if any `error:` level issues are found. Warnings are printed but don't cause failure.

### man

Open the installed manual page.

### import

Convert a Docker Compose file to a qqd config. Reads a `docker-compose.yaml` and generates an equivalent qqd config file. Supports service images, build contexts, environment variables, volumes, ports, `depends_on`, and command overrides. Variables from `--env` are expanded with `${VAR:-default}` syntax. Output format is auto-detected from the output extension, or set with `--format`.

```bash
qqd import -f docker-compose.yaml --host 192.0.2.10 --user ec2-user -o app.yaml
qqd deploy -c app.yaml
```

### migrate

Migrate a running deployment from another system to qqd. **Run with `--dry-run` first** on any production-like target — the command performs host-global operations (see [safety-model.md](safety-model.md)). Confirmation is required by default; pass `--yes`/`-y` only after a clean dry-run.

Supported `--from` values:
- `compose` - migrate from a running Docker Compose stack
- `swarm` - migrate from a running Docker Swarm stack (`--stack <name>` required)

If `--to` is omitted, it defaults to `podman`, the only supported destination.

### convert

Convert a qqd config between formats. Reads a qqd config file and writes it in the specified output format. Format is auto-detected from the output extension, or set with `--format`. Without `-o`, prints to stdout.

### docs

Generate CLI documentation to stdout or a file. The default output is the same Markdown that ships as [docs/cli-reference.md](cli-reference.md). CI re-runs this generator on every change and fails on drift, so this file is the canonical CLI listing.

```bash
qqd docs                      # print to stdout
qqd docs -o docs/CLI.md       # write to file
qqd docs config               # full configuration reference
```

## Help

```bash
qqd --help                    # global help
qqd help deploy               # command help
qqd deploy --help             # also works
qqd man                       # full man page
man qqd                       # also works
```
