package qqd

import (
	"context"
	"fmt"
	"strings"
)

// systemdLifecycle is the systemd-backed Lifecycle implementation. It is a
// thin adapter over systemctl and Quadlet rendering so systemd and direct
// backends share the Lifecycle interface where the orchestration path needs it.
type systemdLifecycle struct {
	rt ContainerRuntime
}

func (l systemdLifecycle) Name() string { return "systemd" }

func (l systemdLifecycle) sctl() string {
	return l.rt.SystemctlPrefix()
}

func (l systemdLifecycle) sudoPrefix() string {
	return ""
}

// Install renders a unit file from the spec, writes it to the runtime's
// UnitDir, and starts it via systemctl. Idempotent: an existing unit with
// the same name is overwritten.
func (l systemdLifecycle) Install(ctx context.Context, exec Executor, spec ContainerSpec) error {
	cfg := serviceConfigFromSpec(spec)
	var content string
	switch {
	case spec.Replica > 0 && containerNameSuggestsReplica(spec):
		content = l.rt.RenderReplicaContainer(spec.Project, spec.Service, spec.Replica, cfg)
	default:
		content = l.rt.RenderContainer(spec.Project, spec.Service, cfg)
	}
	dir := l.rt.UnitDir()
	name := containerFileNameForSpec(l.rt, spec)
	if err := writeUnitFile(ctx, exec, l.sudoPrefix(), dir, name, content); err != nil {
		return err
	}
	if _, err := exec.Run(ctx, l.sctl()+" daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}
	if _, err := exec.Run(ctx, fmt.Sprintf("%s start %s", l.sctl(), shellQuote(name))); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	return nil
}

func (l systemdLifecycle) Reload(ctx context.Context, exec Executor, _ string) error {
	_, err := exec.Run(ctx, l.sctl()+" daemon-reload")
	return err
}

func (l systemdLifecycle) Start(ctx context.Context, exec Executor, name string) error {
	_, err := exec.Run(ctx, fmt.Sprintf("%s start %s", l.sctl(), shellQuote(name)))
	return err
}

func (l systemdLifecycle) Stop(ctx context.Context, exec Executor, name string) error {
	_, err := exec.Run(ctx, fmt.Sprintf("%s stop %s 2>/dev/null || true", l.sctl(), shellQuote(name)))
	return err
}

func (l systemdLifecycle) Remove(ctx context.Context, exec Executor, name string) error {
	if _, err := exec.Run(ctx, fmt.Sprintf("%s stop %s 2>/dev/null || true", l.sctl(), shellQuote(name))); err != nil {
		return err
	}
	dir := l.rt.UnitDir()
	if _, err := exec.Run(ctx, fmt.Sprintf("%srm -f %s/%s", l.sudoPrefix(), dir, shellQuote(name))); err != nil {
		return err
	}
	_, err := exec.Run(ctx, l.sctl()+" daemon-reload")
	return err
}

func (l systemdLifecycle) Status(ctx context.Context, exec Executor, name string) (UnitStatus, error) {
	out, err := exec.Run(ctx, fmt.Sprintf("%s is-active %s 2>/dev/null || true", l.sctl(), shellQuote(name)))
	state := strings.TrimSpace(out)
	if state == "" {
		state = "missing"
	}
	return UnitStatus{Name: name, State: normalizeSystemdState(state), RawDetail: state}, err
}

func (l systemdLifecycle) List(ctx context.Context, exec Executor, project string) ([]UnitStatus, error) {
	dir := l.rt.UnitDir()
	ext := l.rt.UnitExtension()
	cmd := fmt.Sprintf("ls -1 %s 2>/dev/null | grep -E '^%s-.*%s$' || true",
		dir, project, strings.TrimPrefix(ext, "."))
	out, err := exec.Run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var statuses []UnitStatus
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		st, _ := l.Status(ctx, exec, line)
		st.Name = line
		statuses = append(statuses, st)
	}
	return statuses, nil
}

func (l systemdLifecycle) Rename(ctx context.Context, exec Executor, from, to string) error {
	dir := l.rt.UnitDir()
	mv := fmt.Sprintf("%smv %s/%s %s/%s", l.sudoPrefix(), dir, shellQuote(from), dir, shellQuote(to))
	if _, err := exec.Run(ctx, mv); err != nil {
		return err
	}
	if _, err := exec.Run(ctx, l.sctl()+" daemon-reload"); err != nil {
		return err
	}
	_, err := exec.Run(ctx, fmt.Sprintf("%s restart %s", l.sctl(), shellQuote(to)))
	return err
}

// normalizeSystemdState maps `systemctl is-active` output to UnitStatus.State.
func normalizeSystemdState(s string) string {
	switch s {
	case "active":
		return "active"
	case "activating":
		return "deploying"
	case "inactive", "deactivating":
		return "inactive"
	case "failed":
		return "failed"
	case "missing":
		return "missing"
	default:
		return "unknown"
	}
}

// serviceConfigFromSpec rebuilds the ServiceConfig shape that the Quadlet
// renderers expect.
func serviceConfigFromSpec(spec ContainerSpec) ServiceConfig {
	return ServiceConfig{
		Image:     spec.Image,
		User:      spec.User,
		Command:   append([]string(nil), spec.Command...),
		DependsOn: append([]string(nil), spec.DependsOn...),
		Volumes:   append([]string(nil), spec.Volumes...),
		Env:       cloneStringMap(spec.Env),
		Replicas:  spec.Replica,
		Health:    spec.Health,
		Resources: spec.Resources,
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func containerNameSuggestsReplica(spec ContainerSpec) bool {
	suffix := fmt.Sprintf("-%d", spec.Replica)
	return spec.Replica > 1 || strings.HasSuffix(spec.Container, suffix)
}

func containerFileNameForSpec(rt ContainerRuntime, spec ContainerSpec) string {
	if containerNameSuggestsReplica(spec) {
		return rt.ReplicaFileName(spec.Project, spec.Service, spec.Replica)
	}
	return rt.ContainerFileName(spec.Project, spec.Service)
}

// writeUnitFile drops content into <dir>/<name>, using sudo when needed.
func writeUnitFile(ctx context.Context, exec Executor, sudo, dir, name, content string) error {
	if _, err := exec.Run(ctx, fmt.Sprintf("%smkdir -p %s", sudo, dir)); err != nil {
		return err
	}
	path := fmt.Sprintf("%s/%s", dir, name)
	heredoc := fmt.Sprintf("%ssh -c 'cat > %s' <<'QD_EOF'\n%sQD_EOF", sudo, path, content)
	if sudo == "" {
		heredoc = fmt.Sprintf("cat > %s <<'QD_EOF'\n%sQD_EOF", path, content)
	}
	_, err := exec.Run(ctx, heredoc)
	return err
}
