package qqd

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
)

// Manifest is the machine-readable, AI-agent-friendly description of every
// surface qqd exposes: commands, flags, config schema, output formats, and
// the known pitfalls users / agents need to know about.
//
// `qqd manifest` builds the most drift-prone pieces at runtime and keeps the
// rest in small registries that are exercised by tests:
//
//   - commandSpecs() (docsgen.go)  for command names, summaries, usage, details
//   - commonFlagRegistry()         for the flags parseCommonOpts accepts
//   - reflection over the tagged   for the YAML/JSON/HOCON config schema
//     ProjectConfig / ServiceConfig
//     / TargetConfig / BuildConfig
//     / TLSConfig / HealthConfig /
//     HooksConfig / ResourceConfig
//     / ServiceOverride structs
//   - conceptRegistry() / pitfallRegistry()
//     for guidance text with no runtime struct counterpart
//
// Adding a new field to a config struct with a `qqd:` tag automatically makes
// it appear in `qqd manifest`. Adding a new command to commandSpecs() does the
// same; command-specific flag metadata and guidance registries still need an
// intentional update.
type Manifest struct {
	Tool          ToolMeta         `json:"tool"`
	Concepts      []Concept        `json:"concepts"`
	CommonFlags   []Flag           `json:"common_flags"`
	Commands      []Command        `json:"commands"`
	ConfigSchema  []ConfigSection  `json:"config_schema"`
	OutputFormats []OutputFormat   `json:"output_formats"`
	ExitCodes     []ExitCode       `json:"exit_codes"`
	Lifecycles    []LifecycleEntry `json:"lifecycle_backends"`
	Proxies       []ProxyEntry     `json:"proxy_providers"`
	Pitfalls      []Pitfall        `json:"pitfalls"`
}

// ToolMeta identifies the binary and provides links agents can fetch.
type ToolMeta struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Commit      string   `json:"commit,omitempty"`
	BuildTime   string   `json:"build_time,omitempty"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Repo        string   `json:"repo"`
	Docs        []DocRef `json:"docs"`
}

// DocRef points at a Markdown doc shipped with the project.
type DocRef struct {
	Title string `json:"title"`
	Path  string `json:"path"`
	About string `json:"about"`
}

// Concept is a short labelled note about a piece of the model.
type Concept struct {
	Name  string `json:"name"`
	Body  string `json:"body"`
	SeeIn string `json:"see_in,omitempty"`
}

// Command describes one CLI verb.
type Command struct {
	Name            string   `json:"name"`
	Summary         string   `json:"summary"`
	Usage           string   `json:"usage"`
	Details         []string `json:"details,omitempty"`
	UsesCommonFlags bool     `json:"uses_common_flags"`
	ExtraFlags      []Flag   `json:"extra_flags,omitempty"`
}

// Flag is a named option (short or long).
type Flag struct {
	Name       string `json:"name"`
	Short      string `json:"short,omitempty"`
	Type       string `json:"type"`
	Default    string `json:"default,omitempty"`
	Repeatable bool   `json:"repeatable,omitempty"`
	About      string `json:"about"`
}

// ConfigSection is one block of the YAML/HOCON/JSON schema.
type ConfigSection struct {
	Name   string  `json:"name"`
	Path   string  `json:"path"`
	About  string  `json:"about"`
	Fields []Field `json:"fields"`
}

// Field is one key inside a ConfigSection.
type Field struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    string `json:"required"` // "yes", "no", "conditional"
	Default     string `json:"default,omitempty"`
	Description string `json:"description"`
}

// OutputFormat documents structured output (e.g. status --output json).
type OutputFormat struct {
	Command string `json:"command"`
	Flag    string `json:"flag"`
	Format  string `json:"format"`
	Shape   string `json:"shape,omitempty"`
	About   string `json:"about"`
}

// ExitCode documents process exit semantics.
type ExitCode struct {
	Code    int    `json:"code"`
	Meaning string `json:"meaning"`
}

// LifecycleEntry describes a lifecycle backend.
type LifecycleEntry struct {
	Name  string `json:"name"`
	About string `json:"about"`
}

// ProxyEntry describes a reverse-proxy provider.
type ProxyEntry struct {
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
	About        string   `json:"about"`
}

// Pitfall is a known gotcha. Each lists symptom, cause, and fix.
type Pitfall struct {
	Topic   string `json:"topic"`
	Symptom string `json:"symptom"`
	Cause   string `json:"cause"`
	Fix     string `json:"fix"`
	SeeIn   string `json:"see_in,omitempty"`
}

// buildManifest assembles the full manifest from the live registries.
func buildManifest() Manifest {
	return Manifest{
		Tool:          buildToolMeta(),
		Concepts:      conceptRegistry(),
		CommonFlags:   commonFlagRegistry(),
		Commands:      buildCommandManifest(),
		ConfigSchema:  buildConfigSchema(),
		OutputFormats: outputFormatRegistry(),
		ExitCodes:     exitCodeRegistry(),
		Lifecycles:    lifecycleRegistry(),
		Proxies:       proxyRegistry(),
		Pitfalls:      pitfallRegistry(),
	}
}

func buildToolMeta() ToolMeta {
	return ToolMeta{
		Name:      "qqd",
		Version:   versionString(),
		Commit:    buildCommit,
		BuildTime: buildTime,
		Summary:   "Deploy containerized apps to your own VMs over SSH. One config, one command.",
		Description: "qqd is a single-binary CLI that manages Podman services on Linux (or local macOS via podman machine). " +
			"It generates Quadlet units, runs blue-green or rolling deploys where the service shape allows, configures " +
			"a Traefik or Caddy reverse proxy, and tracks release history for rollback. No daemon, no Kubernetes. " +
			"qqd connects over SSH from your machine and exits after each invocation.",
		Repo: "https://github.com/pilshchikov/qqd",
		Docs: []DocRef{
			{Title: "README", Path: "README.md", About: "Project overview and 60-second local demo."},
			{Title: "Setup Guide", Path: "docs/setup.md", About: "Target host prerequisites, SSH known_hosts, RHEL 8 / rootless podman fixes."},
			{Title: "Configuration Reference", Path: "docs/configuration.md", About: "Every config field with type, default, required-ness."},
			{Title: "Command Reference", Path: "docs/commands.md", About: "Detailed flag and behavior reference per command."},
			{Title: "CLI Reference (auto-generated)", Path: "docs/cli-reference.md", About: "Generated by `qqd docs`. Checked by CI for drift."},
			{Title: "Lifecycle Backends", Path: "docs/lifecycle.md", About: "systemd vs direct lifecycle and how `auto` selects between them."},
			{Title: "Safety Model", Path: "docs/safety-model.md", About: "Guarantees, failure modes, recovery, concurrent-deploy lock."},
			{Title: "Zero-Downtime", Path: "docs/zero-downtime.md", About: "Blue-green / rolling strategy selection rules."},
			{Title: "Limitations", Path: "docs/limitations.md", About: "What qqd does not do."},
			{Title: "Proxy: Caddy", Path: "docs/proxy-caddy.md", About: "Caddy provider parity with Traefik and what is unsupported."},
			{Title: "Production Checklist", Path: "docs/production-checklist.md", About: "Pre-flight checklist before pointing qqd at a production target."},
			{Title: "Integration Tests", Path: "docs/integration-tests.md", About: "How to run the opt-in integration suite."},
			{Title: "Claim Matrix", Path: "docs/claims.md", About: "Each top-line claim with its evidence."},
		},
	}
}

// commonFlagRegistry is the single source of truth for the flags
// parseCommonOpts accepts. The TestCommonFlagsRegistryMatchesParser unit test
// pins both ends together so adding a new flag to parseCommonOpts without
// updating this registry breaks the build, not production.
func commonFlagRegistry() []Flag {
	return []Flag{
		{Name: "--config", Short: "-c", Type: "path", Repeatable: true, About: "Config file. Repeat to layer overlays (-c app.yaml -c secrets.yaml). Required for almost every command."},
		{Name: "--target", Short: "-t", Type: "string", About: "Run against this target only. Without -t, the command runs against every target in the config."},
		{Name: "--rebuild", Type: "bool", About: "Force-rebuild images even if they already exist on the target."},
		{Name: "--approve", Type: "bool", About: "Skip the interactive plan-confirmation prompt. Use in CI/automation."},
		{Name: "--dry-run", Type: "bool", About: "Show the plan without executing any changes."},
		{Name: "--no-build", Type: "bool", About: "Skip building services that have a dockerfile (still pulls, updates config, restarts)."},
		{Name: "--config-only", Type: "bool", About: "Skip source sync and image build. Only update config, env, expose, then restart."},
		{Name: "--force-unlock", Type: "bool", About: "Take the per-target deploy lock even if another holder is recorded. Only safe when you are certain no other deploy is in progress."},
	}
}

// buildCommandManifest derives the command list from commandSpecs() (the
// existing in-binary registry used by `qqd help` and `qqd docs`). No
// duplication: edits to commandSpecs() flow into the manifest automatically.
// Per-command flag metadata (whether it accepts the common flags + any
// command-specific flags) comes from commandUsesCommonFlags / commandExtraFlags
// in this file; tests pin those to the actual parsers.
func buildCommandManifest() []Command {
	specs := commandSpecs()
	out := make([]Command, 0, len(specs))
	for _, s := range specs {
		out = append(out, Command{
			Name:            s.Name,
			Summary:         s.Summary,
			Usage:           s.Usage,
			Details:         s.Details,
			UsesCommonFlags: commandUsesCommonFlags(s.Name),
			ExtraFlags:      commandExtraFlags(s.Name),
		})
	}
	return out
}

// commandUsesCommonFlags reports whether a command's parser is a parseCommonOpts
// caller. Derived from the dispatcher's known set; kept here so adding a new
// command in commandSpecs() defaults to "no" until you explicitly opt in.
func commandUsesCommonFlags(name string) bool {
	switch name {
	case "init", "deploy", "build", "logs", "rollback", "history",
		"stop", "start", "destroy", "clean", "doctor", "update":
		return true
	default:
		return false
	}
}

// commandExtraFlags returns flags specific to a command that aren't in the
// common set. The runtime parsers stay hand-rolled; this is the metadata.
// Cross-checked by TestCommandExtraFlagsMatchParsers.
func commandExtraFlags(name string) []Flag {
	switch name {
	case "plan":
		return []Flag{{Name: "--output", Type: "string", Default: "text", About: "Use `json` for machine output. JSON shape includes risks[] with level info|warn|danger."}}
	case "status":
		return []Flag{{Name: "--output", Type: "string", Default: "text", About: "Use `json` for machine output."}}
	case "convert":
		return []Flag{
			{Name: "-c", Type: "path", About: "Input config."},
			{Name: "-o", Type: "path", About: "Output file. Without -o, prints to stdout."},
			{Name: "--format", Type: "string", About: "Output format (yaml | json | hocon). Auto-detected from -o extension."},
		}
	case "import":
		return []Flag{
			{Name: "-f", Type: "path", About: "Path to docker-compose.yaml (required)."},
			{Name: "--env", Type: "path", About: "Optional .env file. ${VAR:-default} expansions are honored."},
			{Name: "--format", Type: "string", Default: "yaml", About: "Output format (yaml | json | hocon). Auto-detected from -o extension."},
			{Name: "--host", Type: "string", About: "Target host for the generated config."},
			{Name: "--user", Type: "string", About: "SSH user for the generated target."},
			{Name: "--ssh-key", Type: "path", About: "SSH key path for the generated target."},
			{Name: "-o", Type: "path", About: "Write the generated config here."},
		}
	case "migrate":
		return []Flag{
			{Name: "-c", Type: "path", Repeatable: true, About: "qqd config the stack will be migrated to."},
			{Name: "--from", Type: "string", About: "Source: `compose` or `swarm`."},
			{Name: "--to", Type: "string", Default: "podman", About: "Target runtime. Only `podman` is supported."},
			{Name: "--stack", Type: "string", About: "Compose/swarm stack name (defaults to project name)."},
			{Name: "--dry-run", Type: "bool", About: "Print every destructive action without executing."},
			{Name: "--yes", Short: "-y", Type: "bool", About: "Skip destructive-action confirmation. Only after a clean --dry-run."},
		}
	case "validate":
		return []Flag{{Name: "-c", Type: "path", Repeatable: true, About: "Config file."}}
	case "docs":
		return []Flag{
			{Name: "--format", Type: "string", Default: "markdown", About: "Documentation format."},
			{Name: "-o", Type: "path", About: "Write to file. Without -o, prints to stdout."},
		}
	case "manifest":
		return []Flag{
			{Name: "--format", Type: "string", Default: "json", About: "Output format (json or md)."},
			{Name: "-o", Short: "--output", Type: "path", About: "Write to file. Without -o, prints to stdout."},
		}
	default:
		return nil
	}
}

// buildConfigSchema walks the tagged config structs via reflection and emits
// one ConfigSection per struct. Adding a new field to ProjectConfig /
// ServiceConfig / TargetConfig (etc.) with a `qqd:` tag makes it appear in
// `qqd manifest` and `qqd docs config` automatically — no edits to this
// function required.
func buildConfigSchema() []ConfigSection {
	type entry struct {
		name  string
		path  string
		about string
		t     reflect.Type
	}
	// Order matches the order an agent would learn the schema: top-level
	// project, then the building blocks it references.
	registry := []entry{
		{"project", "(top-level)", "Top-level keys at the root of the config file.", reflect.TypeOf(ProjectConfig{})},
		{"service", "services.<name>", "One container definition. Repeated per service.", reflect.TypeOf(ServiceConfig{})},
		{"target", "targets.<name>", "One deployment destination host. Repeated per target.", reflect.TypeOf(TargetConfig{})},
		{"build", "build (project) or targets.<name>.build", "Build strategy and resource hints.", reflect.TypeOf(BuildConfig{})},
		{"health", "services.<name>.health", "Health check endpoint inside a service.", reflect.TypeOf(HealthConfig{})},
		{"resources", "services.<name>.resources", "Container resource limits.", reflect.TypeOf(ResourceConfig{})},
		{"hooks", "hooks (project) or services.<name>.hooks", "Inline shell snippets that run at deployment / build boundaries.", reflect.TypeOf(HooksConfig{})},
		{"tls", "targets.<name>.expose.<port>.tls", "TLS termination subobject under an HTTP expose entry.", reflect.TypeOf(TLSConfig{})},
		{"service_override", "targets.<name>.overrides.<service>", "Per-target overrides for a single service.", reflect.TypeOf(ServiceOverride{})},
	}
	out := make([]ConfigSection, 0, len(registry)+1)
	for _, e := range registry {
		out = append(out, ConfigSection{
			Name:   e.name,
			Path:   e.path,
			About:  e.about,
			Fields: fieldsFromStruct(e.t),
		})
	}
	// `expose` is not a simple struct (top-level keys are port numbers); document it as prose.
	out = append(out, ConfigSection{
		Name:  "expose",
		Path:  "targets.<name>.expose",
		About: "Reverse-proxy entries. Top-level keys are host port numbers (plus the special key `dashboard`). Cannot be expressed as a single Go struct.",
		Fields: []Field{
			{Name: "<port> (string value)", Type: "string", Required: "no", Description: "TCP passthrough. e.g. `5432: \"db:5432\"`. Traefik only (Caddy rejects raw TCP at validate)."},
			{Name: "<port> (object value)", Type: "object", Required: "no", Description: "HTTP routing block: map of path prefix → \"<service>:<port>\". Optional `tls` subobject for TLS termination."},
			{Name: "dashboard", Type: "integer", Required: "no", Description: "Host port to publish the proxy dashboard on."},
		},
	})
	return out
}

// fieldsFromStruct iterates a struct's fields and returns those with a
// `qqd:` tag, parsing the tag into structured metadata. Fields without the
// tag (or tagged `qqd:"-"`) are treated as internal and skipped.
func fieldsFromStruct(t reflect.Type) []Field {
	if t.Kind() != reflect.Struct {
		return nil
	}
	var out []Field
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		tag := sf.Tag.Get("qqd")
		if tag == "" || tag == "-" {
			continue
		}
		meta := parseQQDTag(tag)
		if meta.key == "" {
			continue
		}
		fieldType := meta.typeOverride
		if fieldType == "" {
			fieldType = goKindToConfigType(sf.Type)
		}
		req := meta.required
		if req == "" {
			req = "no"
		}
		out = append(out, Field{
			Name:        meta.key,
			Type:        fieldType,
			Required:    req,
			Default:     meta.defaultVal,
			Description: meta.desc,
		})
	}
	return out
}

// qqdTagMeta is the parsed form of a `qqd:` struct tag.
type qqdTagMeta struct {
	key          string
	required     string
	defaultVal   string
	typeOverride string
	desc         string
}

// parseQQDTag splits a tag like
//
//	qqd:"key=name;required=yes;default=main;type=string;desc=Free text"
//
// into its components. Semicolons split fields; an equals separates key from
// value. Descriptions may contain commas and embedded quotes (the runtime
// just trims surrounding whitespace), but they may NOT contain semicolons.
func parseQQDTag(tag string) qqdTagMeta {
	var m qqdTagMeta
	for _, part := range strings.Split(tag, ";") {
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(part[:eq])
		v := strings.TrimSpace(part[eq+1:])
		switch k {
		case "key":
			m.key = v
		case "required":
			m.required = v
		case "default":
			m.defaultVal = v
		case "type":
			m.typeOverride = v
		case "desc":
			m.desc = v
		}
	}
	return m
}

// goKindToConfigType maps a Go field type to the YAML/HOCON-facing label.
// Authors can override with `type=…` in the qqd tag when the inferred label
// is wrong (e.g. multi-format fields like `command` accept string OR array).
func goKindToConfigType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array of " + goKindToConfigType(t.Elem())
	case reflect.Map:
		return "map " + goKindToConfigType(t.Key()) + " → " + goKindToConfigType(t.Elem())
	case reflect.Struct:
		return "object"
	case reflect.Ptr:
		return goKindToConfigType(t.Elem())
	default:
		return t.String()
	}
}

func conceptRegistry() []Concept {
	return []Concept{
		{Name: "config-formats", Body: "qqd accepts YAML (.yaml/.yml), JSON (.json), and HOCON (.conf/.hocon). The parser is chosen by extension. Multiple -c flags layer: later files deep-merge over earlier ones. The first -c file's directory is the base for all relative paths in the config (env_file, ssh_key, dockerfile, context, file::refs).", SeeIn: "docs/configuration.md"},
		{Name: "host-local", Body: "Set `host: local` on a target to run on your machine (no SSH). Useful for development, smoke testing, or single-machine deploys. Local targets do not require `user`.", SeeIn: "docs/setup.md"},
		{Name: "pure-image-pull", Body: "If no service has a `dockerfile` or `context`, qqd does not need a `repo` or `repo_dir` — source is never synced. This is the simplest possible deploy shape: a few images pulled from a registry and wired up through the proxy.", SeeIn: "docs/configuration.md"},
		{Name: "lifecycle-auto", Body: "Targets default to `lifecycle: auto`. qqd probes for a usable user systemd session (`systemctl --user`) and uses Quadlet/systemd when available, otherwise falls back to `direct` (podman run --restart=... with qqd.* labels). Set `lifecycle: systemd` or `lifecycle: direct` to pin the choice.", SeeIn: "docs/lifecycle.md"},
		{Name: "strategy-auto-selection", Body: "qqd picks a deploy strategy automatically per service when an image changes: HTTP-exposed non-replicated → blue-green slot; exposed + replicated → rolling-with-drain; replicated + health check → rolling restart with health gating; otherwise → direct restart. You do not configure this.", SeeIn: "docs/zero-downtime.md"},
		{Name: "inter-service-dns", Body: "Containers are named <project>-<service> (e.g. my-app-db). Use the project-prefixed name in env vars: DB_HOST=my-app-db (NOT just \"db\"). This is different from Docker Compose where bare service names work. Replicated containers are <project>-<service>-N.", SeeIn: "docs/configuration.md"},
		{Name: "variable-expansion", Body: "${VAR} in service fields expands from target.env first, then OS environment. Values with the suffixes _TOKEN, _SECRET, _PASSWORD, _KEY are redacted in `qqd plan` output.", SeeIn: "docs/configuration.md"},
		{Name: "deploy-lock", Body: "qqd holds a per-target deploy lock for the duration of init/deploy/build/destroy/rollback/migrate. Concurrent invocations against the same target are refused; use --force-unlock only when you know no other deploy is running.", SeeIn: "docs/safety-model.md"},
		{Name: "release-history-rollback", Body: "Each successful deploy is recorded as a release on the target. `qqd rollback` restores the previous release's images and proxy config (NOT volume data). On a failed health check, qqd auto-rolls back to the previous release when one exists.", SeeIn: "docs/safety-model.md"},
	}
}

func outputFormatRegistry() []OutputFormat {
	return []OutputFormat{
		{Command: "status", Flag: "--output json", Format: "JSON",
			Shape: `{ "project", "targets": [ { "name", "host", "services": [ { "name", "state", "image", "started_at", "uptime_seconds" } ] } ] }`,
			About: "Stable shape for CI / dashboards."},
		{Command: "plan", Flag: "--output json", Format: "JSON",
			Shape: `{ "project", "runtime", "proxy", "sync", "mode", "targets": [...], "risks": [ { "level": "info|warn|danger", "code", "message", "target?", "service?" } ] }`,
			About: "Use risks[level=='danger'] as a CI gate."},
		{Command: "manifest", Flag: "(default)", Format: "JSON",
			Shape: "See `Manifest` type in this document. Stable across patch releases.",
			About: "Use this as the agent's entry point."},
		{Command: "manifest", Flag: "--format md", Format: "Markdown",
			About: "Self-contained brief tuned for LLM consumption."},
	}
}

func exitCodeRegistry() []ExitCode {
	return []ExitCode{
		{Code: 0, Meaning: "success"},
		{Code: 1, Meaning: "qqd or runtime error (config parse, ssh, podman, validation, lock-held, etc.). Read stderr for the specific cause."},
	}
}

func lifecycleRegistry() []LifecycleEntry {
	return []LifecycleEntry{
		{Name: "systemd", About: "Generates Podman Quadlet .container files under ~/.config/containers/systemd/. systemd manages start/restart/dependencies. Survives reboot when user lingering is enabled."},
		{Name: "direct", About: "Runs `podman run --restart=always` with qqd.* labels. No systemd unit files. Survives the user session only as long as podman keeps the container alive; depends on cgroup delegation."},
		{Name: "auto", About: "Probes the target for a usable `systemctl --user` session. Picks `systemd` when found, `direct` otherwise. Default for all targets."},
	}
}

func proxyRegistry() []ProxyEntry {
	return []ProxyEntry{
		{Name: "traefik", Capabilities: []string{"http", "tls", "tcp"}, About: "Default provider. Traefik v3.6. Supports HTTP routing, TLS termination, and raw TCP passthrough."},
		{Name: "caddy", Capabilities: []string{"http", "tls"}, About: "Caddy v2. HTTP + TLS only. `qqd validate` rejects any expose that combines `proxy: caddy` with a raw TCP entry."},
	}
}

func pitfallRegistry() []Pitfall {
	return []Pitfall{
		{
			Topic:   "SSH host key mismatch (Go client)",
			Symptom: "qqd reports `SSH connect …: ssh: handshake failed: knownhosts: key mismatch` even though `ssh -p … <host>` works fine from the shell.",
			Cause:   "The Go SSH client may negotiate a different host-key algorithm (ecdsa/rsa) than the one your known_hosts cached (often only ed25519, via `accept-new` or `ssh-keyscan`). The library treats algorithm mismatch as a key conflict.",
			Fix:     "Cache all three host-key algorithms for the target. See `docs/setup.md` → SSH known_hosts.",
			SeeIn:   "docs/setup.md#ssh-known_hosts-strict-host-key-checking",
		},
		{
			Topic:   "Stale bare-hostname entry in known_hosts",
			Symptom: "`knownhosts: key mismatch` for a host you have never connected to via Go.",
			Cause:   "An old `localhost ssh-ed25519 …` (no port, no brackets) line from earlier tooling. The Go library matches by hostname even when a port-qualified entry also exists.",
			Fix:     "`grep -nE '^localhost ' ~/.ssh/known_hosts` then `ssh-keygen -R localhost`.",
			SeeIn:   "docs/setup.md#ssh-known_hosts-strict-host-key-checking",
		},
		{
			Topic:   "User services die after SSH disconnect",
			Symptom: "`qqd init` succeeds; containers stop ~seconds after the SSH session closes.",
			Cause:   "User systemd manager (`user@<uid>.service`) exits with the last login session unless lingering is enabled.",
			Fix:     "`sudo loginctl enable-linger <username>` on the target.",
			SeeIn:   "docs/setup.md#enable-lingering",
		},
		{
			Topic:   "Rootless podman + cgroups v1 hybrid (RHEL 8)",
			Symptom: "Service shows `Active: failed (Result: exit-code) … status=126`; logs contain `mkdir /sys/fs/cgroup/pids/.../session.scope/runtime: permission denied`.",
			Cause:   "RHEL 8 defaults to hybrid cgroups v1 + v2. Rootless podman can't manage v1 cgroups under the user session.",
			Fix:     "Boot with unified v2: `sudo grubby --update-kernel=ALL --args=\"systemd.unified_cgroup_hierarchy=1\"` and reboot.",
			SeeIn:   "docs/setup.md#rootless-podman-on-rhel-8--older-hosts",
		},
		{
			Topic:   "runc too old for cgroups v2 rootless",
			Symptom: "After switching to v2: `runc create failed: … openat2 …/pids.max: no such file or directory`.",
			Cause:   "RHEL 8's bundled runc predates cgroups v2 rootless support.",
			Fix:     "`sudo dnf install -y crun`, then set `runtime = \"crun\"` in `~/.config/containers/containers.conf` on the target.",
			SeeIn:   "docs/setup.md#rootless-podman-on-rhel-8--older-hosts",
		},
		{
			Topic:   "Controllers not delegated to user manager",
			Symptom: "After installing crun: `crun: the requested cgroup controller 'pids' is not available`.",
			Cause:   "By default `user@<uid>.service` gets no controllers in cgroups v2 mode on RHEL 8.",
			Fix:     "Drop in `/etc/systemd/system/user@.service.d/delegate.conf` with `[Service]\\nDelegate=memory pids cpu io`, then `daemon-reload` + `systemctl restart user@<uid>.service`.",
			SeeIn:   "docs/setup.md#rootless-podman-on-rhel-8--older-hosts",
		},
		{
			Topic:   "Bare service name in env (Docker-Compose habit)",
			Symptom: "App can't resolve `db` / `redis` / etc. — DNS NXDOMAIN.",
			Cause:   "qqd container names are `<project>-<service>` (e.g. `my-app-db`). Compose's bare-name DNS is not replicated.",
			Fix:     "Use the project-prefixed name in env: `DB_HOST=my-app-db`. `qqd import` rewrites these automatically.",
			SeeIn:   "docs/configuration.md",
		},
		{
			Topic:   "Short image names rejected on target",
			Symptom: "Pull fails with `short-name resolution enforced`.",
			Cause:   "Podman in non-interactive (SSH) mode rejects ambiguous short names.",
			Fix:     "Use fully-qualified images: `docker.io/library/postgres:16.1`, `ghcr.io/org/repo/app:1.0`.",
			SeeIn:   "docs/setup.md#image-naming",
		},
		{
			Topic:   "Caddy + raw TCP",
			Symptom: "`qqd validate` errors with a message about caddy + TCP.",
			Cause:   "Caddy provider supports only HTTP + TLS. Raw TCP passthrough is Traefik-only.",
			Fix:     "Either switch the target's `proxy` to `traefik` or remove the TCP expose entry.",
			SeeIn:   "docs/proxy-caddy.md",
		},
		{
			Topic:   "Rollback does not restore volume data",
			Symptom: "`qqd rollback` restores images and proxy config; data the new release wrote into volumes is still there.",
			Cause:   "Rollback is image+config scoped by design. Persistent volume contents are out of scope.",
			Fix:     "Take an out-of-band snapshot (DB dump, FS snapshot) before destructive migrations.",
			SeeIn:   "docs/limitations.md#rollback-scope",
		},
	}
}

// Build-time version metadata. Wired up by the cmd/qqd/main package via
// SetBuildInfo so the internal package can read what goreleaser stamped.
var (
	buildVersion = "dev"
	buildCommit  = ""
	buildTime    = ""
)

// SetBuildInfo lets cmd/qqd/main pass the ldflag-stamped version metadata in
// at startup so commands like `manifest` and any future telemetry can report
// it accurately.
func SetBuildInfo(version, commit, builtAt string) {
	if version != "" {
		buildVersion = version
	}
	buildCommit = commit
	buildTime = builtAt
}

// versionString centralizes the displayed version (build-time stamped or "dev").
func versionString() string {
	if buildVersion != "" {
		return buildVersion
	}
	return "dev"
}

// renderManifestJSON marshals the manifest as pretty-printed JSON.
func renderManifestJSON(m Manifest) (string, error) {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

// renderManifestMarkdown emits a self-contained agent brief in Markdown.
func renderManifestMarkdown(m Manifest) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("# %s — Agent Brief\n\n", m.Tool.Name)
	w("**Version:** %s\n", m.Tool.Version)
	if m.Tool.Commit != "" {
		w("**Commit:** %s\n", m.Tool.Commit)
	}
	if m.Tool.BuildTime != "" {
		w("**Built:** %s\n", m.Tool.BuildTime)
	}
	w("\n%s\n\n", m.Tool.Description)
	w("Repo: %s\n\n", m.Tool.Repo)

	w("## Where to read more\n\n")
	for _, d := range m.Tool.Docs {
		w("- `%s` — %s\n", d.Path, d.About)
	}
	w("\n")

	w("## Concepts\n\n")
	for _, c := range m.Concepts {
		w("- **%s.** %s", c.Name, c.Body)
		if c.SeeIn != "" {
			w(" _(see `%s`)_", c.SeeIn)
		}
		w("\n")
	}
	w("\n")

	w("## Common flags\n\n")
	w("Accepted by every command marked `uses_common_flags`.\n\n")
	w("| Flag | Short | Type | Default | Description |\n")
	w("|------|-------|------|---------|-------------|\n")
	for _, f := range m.CommonFlags {
		w("| `%s` | %s | %s | %s | %s |\n", f.Name, codeIfSet(f.Short), mdCell(f.Type), mdCell(escapeOrDash(f.Default)), mdCell(f.About))
	}
	w("\n")

	w("## Commands\n\n")
	for _, c := range m.Commands {
		w("### `%s` — %s\n\n", c.Name, c.Summary)
		w("```\n%s\n```\n\n", c.Usage)
		if c.UsesCommonFlags {
			w("Accepts the common flags above.\n\n")
		}
		if len(c.ExtraFlags) > 0 {
			w("**Flags**\n\n")
			for _, f := range c.ExtraFlags {
				short := ""
				if f.Short != "" {
					short = fmt.Sprintf(" / `%s`", f.Short)
				}
				dflt := ""
				if f.Default != "" {
					dflt = fmt.Sprintf(" _(default `%s`)_", f.Default)
				}
				w("- `%s`%s (%s)%s — %s\n", f.Name, short, f.Type, dflt, f.About)
			}
			w("\n")
		}
		if len(c.Details) > 0 {
			for _, d := range c.Details {
				w("> %s\n", d)
			}
			w("\n")
		}
	}

	w("## Config schema\n\n")
	for _, sec := range m.ConfigSchema {
		w("### `%s` (`%s`)\n\n", sec.Name, sec.Path)
		w("%s\n\n", sec.About)
		w("| Field | Type | Required | Default | Description |\n")
		w("|-------|------|----------|---------|-------------|\n")
		for _, f := range sec.Fields {
			w("| `%s` | %s | %s | %s | %s |\n", f.Name, mdCell(f.Type), mdCell(f.Required), mdCell(escapeOrDash(f.Default)), mdCell(f.Description))
		}
		w("\n")
	}

	w("## Output formats\n\n")
	w("| Command | Flag | Format | Shape |\n")
	w("|---------|------|--------|-------|\n")
	for _, o := range m.OutputFormats {
		w("| %s | `%s` | %s | %s |\n", mdCell(o.Command), mdCell(o.Flag), mdCell(o.Format), mdCell(o.Shape))
	}
	w("\n")

	w("## Exit codes\n\n")
	for _, ec := range m.ExitCodes {
		w("- `%d` — %s\n", ec.Code, ec.Meaning)
	}
	w("\n")

	w("## Lifecycle backends\n\n")
	for _, lb := range m.Lifecycles {
		w("- **%s** — %s\n", lb.Name, lb.About)
	}
	w("\n")

	w("## Proxy providers\n\n")
	for _, p := range m.Proxies {
		w("- **%s** (`%s`) — %s\n", p.Name, strings.Join(p.Capabilities, ", "), p.About)
	}
	w("\n")

	w("## Pitfalls\n\n")
	w("If a deploy fails, scan this list before opening an issue.\n\n")
	for _, p := range m.Pitfalls {
		w("### %s\n\n", p.Topic)
		w("- **Symptom:** %s\n", p.Symptom)
		w("- **Cause:** %s\n", p.Cause)
		w("- **Fix:** %s\n", p.Fix)
		if p.SeeIn != "" {
			w("- **See:** `%s`\n", p.SeeIn)
		}
		w("\n")
	}

	return b.String()
}

func codeIfSet(s string) string {
	if s == "" {
		return ""
	}
	return "`" + s + "`"
}

func escapeOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func mdCell(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", `\|`)
	return s
}

// manifestOptions holds parsed flags for `qqd manifest`.
type manifestOptions struct {
	Format string
	Output string
}

// parseManifestArgs parses `qqd manifest` flags.
func parseManifestArgs(args []string) (manifestOptions, error) {
	opts := manifestOptions{Format: "json"}
	fs := flag.NewFlagSet("manifest", flag.ContinueOnError)
	fs.SetOutput(ioDiscard{})
	fs.StringVar(&opts.Format, "format", "json", "output format (json or md)")
	fs.StringVar(&opts.Output, "o", "", "write output to file")
	fs.StringVar(&opts.Output, "output", "", "write output to file")
	if err := fs.Parse(args); err != nil {
		return manifestOptions{}, err
	}
	if positional := fs.Args(); len(positional) > 0 {
		return manifestOptions{}, fmt.Errorf("manifest does not accept positional args: %s", strings.Join(positional, " "))
	}
	switch strings.ToLower(opts.Format) {
	case "json":
		opts.Format = "json"
	case "md", "markdown":
		opts.Format = "md"
	default:
		return manifestOptions{}, fmt.Errorf("unsupported manifest format %q (supported: json, md)", opts.Format)
	}
	return opts, nil
}

// runManifestCommand renders the manifest and writes to stdout or a file.
func runManifestCommand(opts manifestOptions, invocationWD string, out io.Writer) error {
	m := buildManifest()
	var content string
	switch opts.Format {
	case "json":
		s, err := renderManifestJSON(m)
		if err != nil {
			return err
		}
		content = s
	case "md":
		content = renderManifestMarkdown(m)
	default:
		return errors.New("unsupported manifest format")
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
	_, err = fmt.Fprintf(out, "wrote %s manifest to %s\n", opts.Format, path)
	return err
}
