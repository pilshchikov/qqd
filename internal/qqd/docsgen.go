package qqd

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// commandSpec describes one CLI command for help and generated documentation.
type commandSpec struct {
	Name    string
	Usage   string
	Summary string
	Details []string
}

// docsOptions holds configuration for generated documentation output.
type docsOptions struct {
	Format string
	Output string
	Topic  string // "" for CLI commands (default), "config" for configuration reference
}

// commandSpecs returns the canonical command metadata used by help and docs output.
func commandSpecs() []commandSpec {
	return []commandSpec{
		{
			Name:    "plan",
			Usage:   "qqd plan -c <config> [-c <overlay>...] [-t <target>] [--rebuild] [--output json] [services...]",
			Summary: "show deployment plan without executing",
			Details: []string{
				"displays what deploy would do without making any changes",
				"",
				"shows per-target breakdown of:",
				"  - services that will be deployed",
				"  - images and build actions",
				"  - deployment mode (standard, slot-based, replicated)",
				"  - risks (e.g. exposed-no-health, mutable-image-tag, caddy-tcp-passthrough)",
				"",
				"use --output json for machine-readable output. The JSON shape is:",
				"  { project, runtime, proxy, sync, mode, targets[], risks[] }",
				"  where each risk has { level (info|warn|danger), code, message, target?, service? }",
				"",
				"CI gates can fail when any risk has level=\"danger\":",
				"  qqd plan -c app.yaml --output json | jq -e '.risks | map(select(.level==\"danger\")) | length == 0'",
			},
		},
		{
			Name:    "init",
			Usage:   "qqd init -c <config> [-c <overlay>...] [-t <target>] [--rebuild] [--force-unlock] [services...]",
			Summary: "first-time setup on target(s)",
			Details: []string{
				"first-time setup: clone repo, create dirs, build/pull images, install units, start",
				"",
				"pipeline:",
				"  SSH to target",
				"    |-- mkdir -p <repo_dir>",
				"    |-- git clone <repo> (if not cloned yet)",
				"    |-- git fetch --all && git reset --hard origin/<branch>",
				"    |-- mkdir -p <dirs> (target directories)",
				"    |-- for each service:",
				"    |     image missing + has dockerfile --> build",
				"    |     image missing + no dockerfile  --> pull",
				"    |     image exists                   --> skip",
				"    |-- write unit files to systemd directory",
				"    |-- systemctl daemon-reload",
				"    |-- systemctl start <all units>",
				"    '-- verify: systemctl is-active <each unit>",
			},
		},
		{
			Name:    "deploy",
			Usage:   "qqd deploy -c <config> [-c <overlay>...] [-t <target>] [--rebuild] [--no-build] [--approve] [--dry-run] [--config-only] [--force-unlock] [services...]",
			Summary: "idempotent deploy: build/pull missing images and restart changed services",
			Details: []string{
				"idempotent: only builds/pulls missing images, only restarts changed services",
				"",
				"shows a plan and asks for confirmation before proceeding.",
				"use --approve to skip the confirmation prompt (for CI/scripts).",
				"use --dry-run to show the plan without executing any changes.",
				"use --no-build to skip building dockerfile services (still pulls, updates config, restarts).",
				"use --config-only to skip source sync and image build (only update config, env, expose, restart).",
				"use --force-unlock to take the deploy lock even if another holder is recorded on the target",
				"  (only safe if you are sure no other deploy is in progress).",
				"",
				"full deploy (no service args) removes services deleted from the config.",
				"partial deploy (e.g. \"qqd deploy server\") leaves other services untouched.",
				"",
				"pipeline:",
				"  SSH to target",
				"    |-- mkdir -p <repo_dir>",
				"    |-- git clone (if not cloned) or git fetch + reset",
				"    |-- mkdir -p <dirs>",
				"    |-- for each service:",
				"    |     image exists --> skip (no build, no restart)",
				"    |     image missing + has dockerfile --> build --> mark changed",
				"    |     image missing + no dockerfile  --> pull  --> mark changed",
				"    |-- if sync=upload + services have context: upload only context dirs",
				"    |     otherwise: upload/sync full project",
				"    |-- write unit files, daemon-reload",
				"    |-- remove stale unit files (full deploy: all removed services;",
				"    |     partial deploy: only replica/mode changes for targeted services)",
				"    |-- detect config changes (unit files + proxy container)",
				"    |-- systemctl start <all units>",
				"    |-- systemctl restart <changed units only>",
				"    '-- verify: systemctl is-active <each unit>",
				"",
				"  if install/restart fails and a previous release exists,",
				"  qqd auto-rolls back to the last successful release.",
				"",
				"  hooks: pre_deploy and post_deploy run at deployment boundaries",
			},
		},
		{
			Name:    "build",
			Usage:   "qqd build -c <config> [-c <overlay>...] [-t <target>] [--rebuild] [--force-unlock] [services...]",
			Summary: "build/pull images only, no deploy/restart",
			Details: []string{
				"build/pull images without restarting any services",
				"",
				"pipeline:",
				"  SSH to target",
				"    |-- git fetch + reset (sync source)",
				"    |-- mkdir -p <dirs>",
				"    |-- for each service:",
				"    |     image exists --> skip",
				"    |     has dockerfile --> build",
				"    |     no dockerfile  --> pull",
				"    '-- print summary of built/pulled images",
				"",
				"no unit files are written, no services are restarted",
			},
		},
		{
			Name:    "update",
			Usage:   "qqd update -c <config> [-c <overlay>...] [-t <target>] [--rebuild] [--approve] [service[=version]...]",
			Summary: "bump/set image version(s) and redeploy updated services",
			Details: []string{
				"with no services: auto-bumps all buildable services (those with a dockerfile)",
				"with services:    bumps or sets versions for the listed services",
				"",
				"pipeline:",
				"  LOCAL: read config file",
				"    |-- for each service to update:",
				"    |     auto-bump: increment rightmost number (1.44 -> 1.45, 0.1-b7 -> 0.1-b8)",
				"    |     explicit:  set to provided version (server=2.0)",
				"    |-- rewrite image tags in config file in-place",
				"    '-- print updated versions",
				"  then run DEPLOY for the updated services only:",
				"    SSH to target",
				"      |-- git fetch + reset",
				"      |-- build/pull new image versions (old versions still exist, new ones don't)",
				"      |-- write unit files, daemon-reload",
				"      |-- restart updated services",
				"      |-- verify",
				"      '-- on failure: auto-rollback to previous release (if available)",
			},
		},
		{
			Name:    "status",
			Usage:   "qqd status -c <config> [-c <overlay>...] [-t <target>] [--output json]",
			Summary: "show service state/image/uptime on target(s)",
			Details: []string{
				"check connectivity and print service status for each target",
				"",
				"use --output json for machine-readable JSON output",
				"output format: image (YYYY-MM-DD HH:MM:SS UTC, up Xh Ym)",
				"",
				"pipeline:",
				"  SSH to target",
				"    |-- ls <unit directory> (connectivity check + slot detection)",
				"    '-- batched query for all services:",
				"          |-- systemctl is-active <unit>",
				"          '-- <runtime> inspect <container> (image name, creation time)",
			},
		},
		{
			Name:    "logs",
			Usage:   "qqd logs -c <config> [-c <overlay>...] [-t <target>] [services...]",
			Summary: "stream container logs",
			Details: []string{
				"stream last 200 lines and follow new output",
				"without service args, streams all services",
				"replicated services stream all replicas",
				"multiple containers are prefixed with the container name",
				"",
				"pipeline:",
				"  SSH to target",
				"    '-- <runtime> logs --tail 200 -f <container> (per container, in parallel)",
			},
		},
		{
			Name:    "rollback",
			Usage:   "qqd rollback -c <config> [-c <overlay>...] [-t <target>] [--force-unlock] [service]",
			Summary: "restore previous release (or rollback single service)",
			Details: []string{
				"rolls back to the previous release on the target",
				"with a service name, rolls back only that service",
				"without a service name, rolls back all services to the previous release",
				"",
				"the previous release's images are pulled if not already on the target",
				"the rollback is saved as a new release in the history",
				"",
				"note: deploy and update auto-rollback on failure when a previous",
				"release exists. this command is for manual rollback.",
			},
		},
		{
			Name:    "history",
			Usage:   "qqd history -c <config> [-c <overlay>...] [-t <target>]",
			Summary: "show deployment release history on target(s)",
			Details: []string{
				"lists recent releases stored on each target",
				"shows release ID, timestamp, and service images",
				"the most recent release is marked with →",
			},
		},
		{
			Name:    "stop",
			Usage:   "qqd stop -c <config> [-c <overlay>...] [-t <target>] [services...]",
			Summary: "stop service units",
			Details: []string{
				"stop selected or all service units (preserves unit files and images)",
				"",
				"pipeline:",
				"  SSH to target",
				"    '-- systemctl stop <service units>",
			},
		},
		{
			Name:    "start",
			Usage:   "qqd start -c <config> [-c <overlay>...] [-t <target>] [services...]",
			Summary: "start service units",
			Details: []string{
				"start previously stopped service units",
				"",
				"pipeline:",
				"  SSH to target",
				"    |-- systemctl start <network unit>",
				"    '-- systemctl start <service units>",
			},
		},
		{
			Name:    "destroy",
			Usage:   "qqd destroy -c <config> [-c <overlay>...] [-t <target>] [--force-unlock]",
			Summary: "stop/disable units and remove generated unit files",
			Details: []string{
				"fully remove project from target (images are NOT removed)",
				"",
				"pipeline:",
				"  SSH to target",
				"    |-- stop all units (systemctl stop, tolerates missing)",
				"    |-- disable all units (systemctl disable)",
				"    |-- remove unit files",
				"    |-- clean up proxy config (~/.config/qqd/<project>/)",
				"    '-- reload systemd daemon",
			},
		},
		{
			Name:    "clean",
			Usage:   "qqd clean -c <config> [-c <overlay>...] [-t <target>]",
			Summary: "remove project containers and unused images from targets",
			Details: []string{
				"removes stopped containers and stale images associated with the project",
				"",
				"pipeline:",
				"  SSH to target",
				"    |-- list and remove containers matching <project>-*",
				"    |-- list and remove images matching <project>-*",
				"    '-- prune dangling (<none>) images",
			},
		},
		{
			Name:    "doctor",
			Usage:   "qqd doctor -c <config> [-c <overlay>...] [-t <target>]",
			Summary: "check target environment for common problems",
			Details: []string{
				"runs diagnostic checks on deployment targets",
				"",
				"checks:",
				"  - SSH connectivity",
				"  - Podman availability",
				"  - systemd session",
				"  - unit directory exists",
				"  - disk space",
				"  - user lingering (Podman only)",
			},
		},
		{
			Name:    "validate",
			Usage:   "qqd validate -c <config> [-c <overlay>...]",
			Summary: "check config for errors without deploying",
			Details: []string{
				"validates the configuration file for common errors",
				"",
				"checks:",
				"  - service references in depends_on",
				"  - circular dependency detection",
				"  - port range validation",
				"  - TLS configuration completeness",
				"  - health check port inference",
				"  - build strategy requirements",
				"  - mutable image tag warnings",
			},
		},
		{
			Name:    "convert",
			Usage:   "qqd convert -c <input> [-o <output>] [--format yaml|json|hocon]",
			Summary: "convert config between formats (yaml, json, hocon)",
			Details: []string{
				"reads a qqd config file and outputs it in a different format",
				"format is auto-detected from output file extension, or set with --format",
				"without -o, prints to stdout",
			},
		},
		{
			Name:    "migrate",
			Usage:   "qqd migrate -c <config> --from <source> [--to podman] [--stack <name>] [--dry-run] [--yes]",
			Summary: "migrate a running Compose or Swarm stack to Podman-backed qqd",
			Details: []string{
				"--from compose  stop docker-compose stack, deploy with qqd",
				"--from swarm    stop docker swarm stack, leave swarm, deploy with qqd",
				"",
				"--to is optional and must be podman (the only supported runtime)",
				"--stack sets the compose/swarm stack name (defaults to project name)",
				"",
				"--dry-run prints every destructive action without executing it. recommended",
				"          for the FIRST run on any production-like target.",
				"--yes (or -y) skips the destructive-action confirmation prompt. only use",
				"          when you have already validated the migration with --dry-run.",
				"",
				"images are transferred from Docker to Podman automatically",
				"volume ownership is fixed when migrating to rootless podman",
				"",
				"safety notes:",
				"  - swarm migration runs `docker swarm leave --force`. if this node is part",
				"    of a multi-node swarm, the entire swarm is affected, not just this stack.",
				"  - migrating to podman runs `sudo chown -R` on every service volume host path.",
				"    if other workloads use the same paths, they will be affected too.",
				"  - `docker network prune -f` is invoked, which removes ALL unused networks",
				"    on the host, not just those in your project.",
				"",
				"examples:",
				"  qqd migrate -c app.yaml --from compose --stack my-stack --dry-run",
				"  qqd migrate -c app.yaml --from swarm --stack my-stack --to podman --yes",
			},
		},
		{
			Name:    "import",
			Usage:   "qqd import -f <docker-compose.yaml> [--env <.env>] [--format yaml|json|hocon] [--host <host>] [--user <user>] [--ssh-key <key>] [-o <output>]",
			Summary: "generate qqd config from a docker-compose.yaml",
			Details: []string{
				"parses a docker-compose.yaml and generates a qqd config file",
				"supports: services, images, build contexts, volumes, environment, ports, depends_on, commands",
				"env vars from --env file are expanded (${VAR:-default} syntax)",
				"output format is auto-detected from extension, or set with --format (yaml, json, hocon)",
				"",
				"after generating, review the config and deploy:",
				"  qqd import -f docker-compose.yaml --host 192.0.2.10 --user ec2-user -o app.yaml",
				"  qqd deploy -c app.yaml",
			},
		},
		{
			Name:    "man",
			Usage:   "qqd man",
			Summary: "open installed manual page (same as man qqd)",
			Details: []string{
				"opens the qqd manual page with your local `man` command",
			},
		},
		{
			Name:    "docs",
			Usage:   "qqd docs [config] [--format markdown] [-o <path>]",
			Summary: "generate CLI or configuration documentation",
			Details: []string{
				"without arguments: prints CLI command reference",
				"with 'config': prints full configuration reference (all fields, types, defaults)",
				"default format is markdown",
				"by default output is stdout; use -o to write a file",
			},
		},
		{
			Name:    "manifest",
			Usage:   "qqd manifest [--format json|md] [-o <path>]",
			Summary: "emit the full qqd surface (commands, flags, config schema, pitfalls) for AI agents and tooling",
			Details: []string{
				"prints a single structured document covering every command, flag,",
				"config field, output format, lifecycle backend, and known pitfall.",
				"the JSON shape is stable across patch releases — use it as the",
				"entry point for any agent that drives qqd.",
				"",
				"primary sources:",
				"  - commands     <- commandSpecs() in this file",
				"  - common flags <- commonFlagRegistry() in manifest.go",
				"  - config       <- reflection over qqd:-tagged structs in types.go",
				"  - guidance     <- curated registries in manifest.go",
				"",
				"prefer this over `qqd docs` when consuming programmatically.",
			},
		},
	}
}

// commandSpecByName returns metadata for a named command.
func commandSpecByName(name string) (commandSpec, bool) {
	for _, spec := range commandSpecs() {
		if spec.Name == name {
			return spec, true
		}
	}
	return commandSpec{}, false
}

// parseDocsArgs parses the `qqd docs` command options.
func parseDocsArgs(args []string) (docsOptions, error) {
	opts := docsOptions{Format: "markdown"}
	fs := flag.NewFlagSet("docs", flag.ContinueOnError)
	fs.SetOutput(ioDiscard{})
	fs.StringVar(&opts.Format, "format", "markdown", "documentation format")
	fs.StringVar(&opts.Output, "o", "", "write output to file")
	fs.StringVar(&opts.Output, "output", "", "write output to file")
	if err := fs.Parse(args); err != nil {
		return docsOptions{}, err
	}
	positional := fs.Args()
	if len(positional) > 1 {
		return docsOptions{}, fmt.Errorf("docs accepts at most one topic: %s", strings.Join(positional, " "))
	}
	if len(positional) == 1 {
		switch positional[0] {
		case "config":
			opts.Topic = "config"
		default:
			return docsOptions{}, fmt.Errorf("unknown docs topic %q (available: config)", positional[0])
		}
	}
	switch strings.ToLower(opts.Format) {
	case "markdown", "md":
		opts.Format = "markdown"
	default:
		return docsOptions{}, fmt.Errorf("unsupported docs format %q", opts.Format)
	}
	return opts, nil
}

// writeGeneratedDocs renders documentation and writes to stdout or a target file.
func writeGeneratedDocs(opts docsOptions, invocationWD string, out io.Writer) error {
	content, err := generateDocumentation(opts.Format, opts.Topic)
	if err != nil {
		return err
	}
	if opts.Output == "" {
		_, err := io.WriteString(out, content)
		return err
	}
	path, err := resolveLocalPath(invocationWD, opts.Output)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "generated %s documentation: %s\n", opts.Format, path)
	return err
}

// generateDocumentation returns generated docs content for a supported format and topic.
func generateDocumentation(format, topic string) (string, error) {
	switch format {
	case "markdown":
		if topic == "config" {
			return configurationReference, nil
		}
		return generateMarkdownDocumentation(), nil
	default:
		return "", errors.New("unsupported docs format")
	}
}

// configurationReference is the full configuration reference embedded in the binary.
// Generated by: qqd docs config
const configurationReference = `# qqd Configuration Reference

Generated by ` + "`qqd docs config`" + `.

qqd supports three config formats: YAML (.yaml/.yml), JSON (.json), and HOCON (.conf/.hocon).
Format is auto-detected by file extension. You can mix formats across overlays.

    qqd deploy -c app.yaml -c secrets.json

Each subsequent ` + "`-c`" + ` is deep-merged on top of the previous ones - later values override earlier ones.

Convert between formats: qqd convert -c app.yaml -o app.json
Import from docker-compose: qqd import -f docker-compose.yaml -o app.yaml

## Config Formats

YAML example:
    name: my-app
    services:
      web:
        image: "nginx:1.25"
        replicas: 2
        depends_on: ["db"]
        env:
          PORT: "8080"
    targets:
      prod:
        host: "192.0.2.10"
        user: deploy

JSON example:
    { "name": "my-app", "services": { "web": { "image": "nginx:1.25" } } }

HOCON example:
    name = "my-app"
    services { web { image = "nginx:1.25" } }

## Value Types (YAML)

| Type | Syntax | Example |
|------|--------|---------|
| string | key: "value" or key: value | image: "postgres:16" |
| integer | key: 123 | replicas: 2 |
| boolean | key: true / false | insecure_host_key: true |
| array | key: ["a", "b"] or key:\\n  - a\\n  - b | depends_on: ["db", "redis"] |
| object | key:\\n  subkey: value | health:\\n  path: /health\\n  port: 8080 |
| map | key:\\n  k: v | env:\\n  DB_HOST: localhost |

## Project-Level Fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| name | string | yes | — | Project name, prefix for all containers and networks |
| repo | string | conditional | — | Git repository URL. Required when a service has a dockerfile or context (i.e. source must be synced to the target) AND sync is not "upload". Pure image-pull deploys can omit it. |
| branch | string | no | "main" | Git branch to deploy |
| sync | string | no | "git" | "git" or "upload" (rsync local files instead) |
| runtime | string | no | "podman" | Container runtime. Only "podman" is supported; omit this field unless needed for clarity |
| proxy | string | no | "traefik" | Reverse proxy: "traefik" or "caddy" |
| proxy_image | string | no | — | Custom proxy container image (overrides provider default) |
| env_file | string or array | no | — | Path(s) to .env file(s) to load |
| hooks | object | no | — | Project-level deploy hooks |
| build | object | no | — | Build strategy configuration |
| services | object | yes | — | Service definitions |
| targets | object | yes | — | Deployment target definitions |

## Service Fields

Defined inside: services { <name> { ... } }

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| image | string | yes | — | Full image name with tag (e.g. "ghcr.io/org/app:1.0") |
| dockerfile | string | no | — | Path to Dockerfile. Relative paths resolve against the first -c config file's directory. If set, image is built; if absent, pulled |
| context | string | no | — | Build context directory. Relative paths resolve against the first -c config file's directory. With sync=upload, only listed contexts are uploaded |
| replicas | integer | no | 1 | Number of replica containers |
| health | object or string | no | — | Health check: { path = "/health", port = 8080 } or "/health" |
| resources | object | no | — | Resource limits: { cpus = "2", memory = "1g" } |
| depends_on | array of strings | no | [] | Service names to start before this one |
| volumes | array of strings | no | [] | Bind mounts: ["/host:/container:opts"] |
| command | string or array | no | — | Override entrypoint. String = single arg, array = multiple |
| user | string | no | — | Container user (e.g. "1000:1000") |
| startup_delay | integer | no | 5 | Seconds to wait when no health check configured |
| env_file | string | no | — | Path to .env file for this service |
| env | map string→string | no | {} | Environment variables |
| hooks | object | no | — | Per-service hooks: pre_build, post_build (strings) |

## Target Fields

Defined inside: targets { <name> { ... } }

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| host | string | yes | — | IP or hostname. "local" for local execution (no SSH) |
| user | string | yes* | — | SSH user (*not required for local targets) |
| ssh_key | string | no | — | Path to SSH private key. ~ expands to home dir |
| ssh_port | integer | no | 22 | SSH port |
| insecure_host_key | boolean | no | false | Skip SSH known_hosts verification |
| repo_dir | string | conditional | — | Absolute path on target for repo clone/sync. Required whenever sync is set or any service builds from source. Optional for pure image-pull deploys. |
| services | array of strings | no | all | Deploy only these services on this target |
| dirs | array of strings | no | [] | Directories to create before deploy (mkdir -p) |
| env | map string→string | no | {} | Variables for ${VAR} expansion |
| overrides | object | no | — | Per-service env overrides: { svc { env { K = "V" } } } |
| build | object | no | — | Per-target build config, merged with project-level |
| expose | object | no | — | Reverse proxy config (see Expose) |
| lifecycle | string | no | "auto" | "auto" probes systemctl and falls back to "direct"; "systemd" forces unit management; "direct" uses podman run --restart=... with qqd.* labels (no systemctl). See docs/lifecycle.md |

## Build Fields

Defined at project level or inside target: build { ... }

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| strategy | string | no | "local" | "local", "build-host", or "github-actions" |
| host | string | build-host | — | Build server hostname or IP |
| user | string | build-host | — | SSH user on build server |
| ssh_key | string | no | — | SSH key for build server |
| ssh_port | integer | no | 22 | SSH port for build server |
| cpu | integer | no | — | CPU limit for builds |
| memory | string | no | — | Memory limit (e.g. "4g") |

## Hooks Fields

Defined at project level or inside service: hooks { ... }

| Field | Type | Scope | Description |
|-------|------|-------|-------------|
| pre_deploy | string | project | Runs before deployment starts |
| post_deploy | string | project | Runs after deployment completes |
| pre_build | string | project + service | Runs before image build/pull |
| post_build | string | project + service | Runs after image build/pull |

## Expose (Reverse Proxy)

Defined inside target: expose { ... }
Traefik v3.6 (default) or Caddy v2. Set proxy = "caddy" to switch. Pluggable via ProxyProvider interface.

HTTP routing (YAML):
    expose:
      80:
        "/api/": "server:8080"
        "/": "frontend:80"

TCP passthrough:
    expose:
      5432: "db:5432"

TLS termination:
    expose:
      80:
        "/": "server:8080"
        tls:
          port: 443
          certs_dir: /certs
          server_name: example.com

Dashboard:
    expose:
      dashboard: 1111

### TLS Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| port | integer | no | HTTPS listen port (default: 443) |
| certs_dir | string | yes | Host path to certificate directory |
| server_name | string | yes | Domain for SNI and cert lookup |

## Env Files

Project level: env_file = ".env" or env_file = [".env", ".env.prod"]
Service level: env_file = ".env.service"

Precedence: explicit env {} > later env_file > earlier env_file
Format: KEY=value, KEY="quoted", # comments, export KEY=value

## Secrets Overlay

Use multiple -c flags: qqd deploy -c app.yaml -c secrets.yaml
File references: env { KEY = "file::path/to/file.json" }
File contents read at deploy time. ~ expands to local home dir.

## Sync Mode

sync = "git"     — git clone + fetch/reset on target (default)
sync = "upload"  — rsync local files to target (respects .gitignore)

## Variable Expansion

${VAR} in service fields expands from target.env first, then OS environment.

## Inter-Service DNS

IMPORTANT: containers are named <project>-<service> (e.g. my-app-db).
Use the project-prefixed name in env vars: DB_HOST=my-app-db (NOT just "db").
This is different from Docker Compose where bare service names work.
The "qqd import" command rewrites service references automatically.
Replicated: <project>-<service>-N (e.g. my-app-server-1, my-app-server-2)

## Path Resolution

Paths inside the config (ssh_key, env_file, dockerfile, context, file:: refs):
  - Absolute paths (start with /) are used as-is.
  - Relative paths resolve against the directory of the FIRST -c config file,
    NOT the shell cwd. So qqd deploy -c configs/app.yaml behaves identically
    whether you run it from the repo root or from the configs directory.
  - ~ expands to the local home directory.

-c arguments themselves: typed at the prompt, so they resolve from the shell
cwd. Use absolute paths to make them invariant.

Layered configs (-c app.yaml -c overlay.yaml): only the first -c file
defines the base directory; overlays contribute values, not location.

Remote paths (repo_dir, dirs, volume sources): must be absolute, no ~ expansion.
.gitignore for sync=upload: looked up next to the config file, not in cwd.

## Container Runtime

qqd deploys with Podman. The default systemd backend writes Quadlet .container
files under ~/.config/containers/systemd/. The direct backend uses podman run
with qqd.* labels and no systemd unit files.

runtime may be omitted or set to "podman". "docker" is rejected.

## Migration

qqd migrate -c app.yaml --from compose              migrate from docker-compose
qqd migrate -c app.yaml --from swarm --stack name   migrate from docker swarm

Migration stops the source stack, transfers images from Docker to Podman,
fixes volume ownership, and redeploys with qqd.

## Import from Compose

qqd import -f docker-compose.yaml --host 192.0.2.10 --user deploy -o app.yaml

Generates qqd config from docker-compose.yaml. Supports --format yaml|json|hocon.
Expands ${VAR:-default} from --env file. Maps ports to expose config.
`

// generateMarkdownDocumentation creates Markdown reference docs for the CLI.
func generateMarkdownDocumentation() string {
	var b strings.Builder
	b.WriteString("# qqd CLI Reference\n\n")
	b.WriteString("Generated by `qqd docs`.\n\n")
	b.WriteString("## Usage\n\n")
	b.WriteString("```text\n")
	b.WriteString(globalUsage())
	b.WriteString("\n```\n\n")
	b.WriteString("## Commands\n\n")
	for _, spec := range commandSpecs() {
		b.WriteString(fmt.Sprintf("- `%s`: %s\n", spec.Name, spec.Summary))
	}
	b.WriteString("\n")
	for _, spec := range commandSpecs() {
		b.WriteString(fmt.Sprintf("### %s\n\n", spec.Name))
		b.WriteString("```text\n")
		b.WriteString(spec.Usage)
		b.WriteString("\n```\n\n")
		b.WriteString(spec.Summary)
		b.WriteString("\n\n")
		if len(spec.Details) > 0 {
			for _, detail := range spec.Details {
				b.WriteString("- ")
				b.WriteString(detail)
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}
