package qqd

import (
	"context"
	"fmt"
	"strings"
)

// Lifecycle abstracts how qqd manages container processes on a target.
//
// Two implementations exist:
//
//   - systemdLifecycle wraps the existing flow: render Quadlet `.container`
//     files, write them under ~/.config/containers/systemd, and use
//     `systemctl --user` to start/stop/inspect.
//   - directLifecycle drives `podman run --restart=always` directly with
//     qqd.* labels. No systemctl.
//
// Both impls produce the same observable outcome for the qqd commands a user
// runs (deploy, status, rollback, destroy, ...). The mechanism differs, the
// result does not.
type Lifecycle interface {
	// Name reports the backend tag for status / plan output.
	Name() string

	// Install creates and starts the container described by spec. Idempotent:
	// if a container with this name already exists, it must be replaced atomically.
	Install(ctx context.Context, exec Executor, spec ContainerSpec) error

	// Reload applies any pending changes (systemd: daemon-reload; direct: no-op).
	Reload(ctx context.Context, exec Executor, project string) error

	// Start ensures the named container is running.
	Start(ctx context.Context, exec Executor, name string) error

	// Stop halts the named container without removing it.
	Stop(ctx context.Context, exec Executor, name string) error

	// Remove stops and removes the named container (and its unit file in
	// systemd mode). No-op if it does not exist.
	Remove(ctx context.Context, exec Executor, name string) error

	// Status reports the current state of one container.
	Status(ctx context.Context, exec Executor, name string) (UnitStatus, error)

	// List returns every qqd-managed container belonging to project.
	List(ctx context.Context, exec Executor, project string) ([]UnitStatus, error)

	// Rename is the atomic rename used by blue-green slot swap. Container
	// must be running before and after; the underlying name changes only.
	Rename(ctx context.Context, exec Executor, from, to string) error
}

// ContainerSpec describes a single container that needs to exist on the
// target. The Lifecycle implementation translates this into either a unit
// file + systemctl call or a direct `podman run` invocation.
type ContainerSpec struct {
	Project     string   // qqd project name
	Target      string   // qqd target name (used for the qqd.target label and direct-mode name scoping)
	Service     string   // logical service name (e.g. "api")
	Replica     int      // 1 for non-replicated; 1..N for replicated
	Container   string   // full container name (e.g. "obs-alpha-api" under direct, "obs-api" under systemd)
	Network     string   // Podman network name
	Aliases     []string // --network-alias entries (e.g. the target-agnostic "<project>-<service>" so existing DNS works)
	Image       string   // image:tag or image@sha256:...
	Env         map[string]string
	Volumes     []string // ["/host:/ctr:opts"]
	Ports       []string // ["host:container"] (only the proxy uses this; app services are wired via expose)
	User        string
	Command     []string
	Health      HealthConfig
	Resources   ResourceConfig
	DependsOn   []string // service names; impl resolves to container names
	Role        string   // "app" or "proxy"
	DeployID    string   // release id
	ConfigHash  string   // sha256 of the effective spec
	ImageDigest string   // resolved image ID at deploy time (sha256:...) or empty
	Runtime     string   // runtime name (informational)
}

// QqdLabels returns the canonical qqd.* label set used by directLifecycle to
// tag containers. Returned in deterministic key order so callers can assemble
// stable command lines.
func (s ContainerSpec) QqdLabels() []string {
	pairs := []struct{ k, v string }{
		{"qqd.project", s.Project},
		{"qqd.target", s.Target},
		{"qqd.service", s.Service},
		{"qqd.replica", fmt.Sprintf("%d", s.Replica)},
		{"qqd.role", s.Role},
		{"qqd.image", s.Image},
		{"qqd.deploy_id", s.DeployID},
		{"qqd.config_hash", s.ConfigHash},
	}
	if s.ImageDigest != "" {
		pairs = append(pairs, struct{ k, v string }{"qqd.image_id", s.ImageDigest})
	}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if p.v == "" {
			continue
		}
		out = append(out, fmt.Sprintf("--label %s=%s", p.k, shellQuote(p.v)))
	}
	return out
}

// UnitStatus is the lifecycle-agnostic status snapshot returned to the rest
// of qqd (status command, auto-rollback decision, doctor).
type UnitStatus struct {
	Name      string // container / unit name
	Project   string
	Target    string
	Service   string
	Replica   int
	Role      string // "app" or "proxy"
	State     string // "active", "inactive", "failed", "deploying", "missing", "unknown"
	Image     string
	DeployID  string
	UptimeS   int64  // seconds since the container started; 0 if unknown
	Health    string // "healthy", "unhealthy", "starting", "" (no healthcheck)
	ExitCode  int    // last exit code if not running; 0 otherwise
	RawDetail string // backend-specific detail, used for diagnostic output
}

// LifecycleSelection records which backend was chosen for a target and why.
// Cached on App for the duration of a single qqd invocation.
type LifecycleSelection struct {
	Backend  string // "systemd" or "direct"
	Reason   string // "config", "auto: systemctl present", "auto: systemctl missing"
	Selected Lifecycle
}

// chooseLifecycle resolves the cfg.Lifecycle field plus an optional probe
// result into a concrete Lifecycle implementation.
//
//   - "systemd" → systemdLifecycle (whatever the runtime says)
//   - "direct"  → directLifecycle
//   - "auto" or "" → if probeOK, systemd; else direct
func chooseLifecycle(setting string, rt ContainerRuntime, probeOK bool) LifecycleSelection {
	setting = strings.ToLower(strings.TrimSpace(setting))
	switch setting {
	case "systemd":
		return LifecycleSelection{
			Backend:  "systemd",
			Reason:   "config",
			Selected: systemdLifecycle{rt: rt},
		}
	case "direct":
		return LifecycleSelection{
			Backend:  "direct",
			Reason:   "config",
			Selected: directLifecycle{rt: rt},
		}
	default: // "" or "auto"
		if probeOK {
			return LifecycleSelection{
				Backend:  "systemd",
				Reason:   "auto: systemctl present",
				Selected: systemdLifecycle{rt: rt},
			}
		}
		return LifecycleSelection{
			Backend:  "direct",
			Reason:   "auto: systemctl missing",
			Selected: directLifecycle{rt: rt},
		}
	}
}

// probeSystemctl runs `command -v systemctl` on the target and returns true
// if systemctl is reachable. Used by chooseLifecycle when the user picks
// "auto" (the default).
func probeSystemctl(ctx context.Context, exec Executor) bool {
	if exec == nil {
		return false
	}
	_, err := exec.Run(ctx, "command -v systemctl >/dev/null 2>&1 && systemctl --version >/dev/null 2>&1")
	return err == nil
}
