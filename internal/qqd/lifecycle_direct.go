package qqd

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// directLifecycle drives containers via `podman run` directly,
// without writing systemd unit files. Survival across host reboots relies on
// Podman's --restart policy, not systemd.
//
// State that systemd was tracking (which containers belong to which project,
// which version is current, which release was last deployed) lives in qqd.*
// container labels and ~/.qqd/state/<project>.json.
type directLifecycle struct {
	rt ContainerRuntime
}

func (l directLifecycle) Name() string { return "direct" }

func (l directLifecycle) cmd() string { return l.rt.Cmd() }

// restartPolicy mirrors what the systemd path provides via Restart=always.
func (l directLifecycle) restartPolicy() string {
	return "always"
}

// Install creates and starts the container described by spec. If a container
// with the same name already exists it is removed first (atomic replace).
func (l directLifecycle) Install(ctx context.Context, exec Executor, spec ContainerSpec) error {
	if spec.Container == "" {
		return fmt.Errorf("install: ContainerSpec.Container is required")
	}
	// Atomic replace: stop + rm any existing container with this name.
	_, _ = exec.Run(ctx, fmt.Sprintf("%s rm -f %s 2>/dev/null || true", l.cmd(), shellQuote(spec.Container)))

	// Best-effort: ensure the project network exists.
	network := spec.Network
	if network == "" {
		network = spec.Project
	}
	if network != "" {
		_, _ = exec.Run(ctx, fmt.Sprintf("%s network create %s 2>/dev/null || true", l.cmd(), shellQuote(network)))
	}

	parts := []string{l.cmd(), "run", "-d", fmt.Sprintf("--restart=%s", l.restartPolicy())}
	parts = append(parts, fmt.Sprintf("--name %s", shellQuote(spec.Container)))
	if network != "" {
		parts = append(parts, fmt.Sprintf("--network %s", shellQuote(network)))
	}
	for _, a := range spec.Aliases {
		if a == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("--network-alias %s", shellQuote(a)))
	}
	for _, p := range spec.Ports {
		parts = append(parts, fmt.Sprintf("-p %s", p))
	}
	if spec.User != "" {
		parts = append(parts, fmt.Sprintf("--user %s", shellQuote(spec.User)))
	}
	for _, key := range sortedKeys(spec.Env) {
		parts = append(parts, fmt.Sprintf("-e %s=%s", key, shellQuote(spec.Env[key])))
	}
	for _, vol := range spec.Volumes {
		if spec.Role == "app" {
			vol = ensureVolumeFlags(vol, spec.VolumeNeedsU)
		}
		parts = append(parts, fmt.Sprintf("-v %s", shellQuote(vol)))
	}
	if spec.Health.Path != "" && spec.Health.Port != 0 {
		// Probe via curl OR wget. curl is missing from python:slim and a
		// few other base images; wget is missing from minimal Debian. Try
		// both so the auto-generated healthcheck works on the typical
		// images people actually use.
		probe := fmt.Sprintf(
			"curl -sf http://localhost:%d%s || wget -q -O /dev/null http://localhost:%d%s || exit 1",
			spec.Health.Port, spec.Health.Path,
			spec.Health.Port, spec.Health.Path,
		)
		parts = append(parts,
			fmt.Sprintf("--health-cmd %s", shellQuote(probe)),
			"--health-interval=10s",
			"--health-timeout=5s",
			"--health-retries=3",
			"--health-start-period=30s",
		)
	}
	if spec.Resources.CPUs != "" {
		parts = append(parts, fmt.Sprintf("--cpus=%s", spec.Resources.CPUs))
	}
	if spec.Resources.Memory != "" {
		parts = append(parts, fmt.Sprintf("--memory=%s", spec.Resources.Memory))
	}
	parts = append(parts, spec.QqdLabels()...)
	parts = append(parts, shellQuote(spec.Image))
	if len(spec.Command) > 0 {
		for _, c := range spec.Command {
			parts = append(parts, shellQuote(c))
		}
	}
	_, err := exec.Run(ctx, strings.Join(parts, " "))
	return err
}

// Reload is a no-op in direct mode. systemd's daemon-reload has no analog.
func (l directLifecycle) Reload(_ context.Context, _ Executor, _ string) error {
	return nil
}

func (l directLifecycle) Start(ctx context.Context, exec Executor, name string) error {
	_, err := exec.Run(ctx, fmt.Sprintf("%s start %s", l.cmd(), shellQuote(name)))
	return err
}

func (l directLifecycle) Stop(ctx context.Context, exec Executor, name string) error {
	_, err := exec.Run(ctx, fmt.Sprintf("%s stop %s 2>/dev/null || true", l.cmd(), shellQuote(name)))
	return err
}

func (l directLifecycle) Remove(ctx context.Context, exec Executor, name string) error {
	_, err := exec.Run(ctx, fmt.Sprintf("%s rm -f %s 2>/dev/null || true", l.cmd(), shellQuote(name)))
	return err
}

// Status inspects a single container.
func (l directLifecycle) Status(ctx context.Context, exec Executor, name string) (UnitStatus, error) {
	out, err := exec.Run(ctx, fmt.Sprintf("%s inspect %s --format '{{json .}}' 2>/dev/null || true", l.cmd(), shellQuote(name)))
	if err != nil {
		return UnitStatus{Name: name, State: "unknown"}, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return UnitStatus{Name: name, State: "missing"}, nil
	}
	st := parseInspectOne(out)
	st.Name = name
	return st, nil
}

// List returns every container labeled with qqd.project=<project>. Results
// include the per-container qqd.target / qqd.service labels so callers can
// further scope by target.
func (l directLifecycle) List(ctx context.Context, exec Executor, project string) ([]UnitStatus, error) {
	cmd := fmt.Sprintf("%s ps --all --filter label=qqd.project=%s --format '{{.Names}}'",
		l.cmd(), shellQuote(project))
	out, err := exec.Run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var statuses []UnitStatus
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		st, _ := l.Status(ctx, exec, name)
		statuses = append(statuses, st)
	}
	return statuses, nil
}

// listForTarget is the same as List but additionally filters server-side by
// qqd.target label so multiple targets sharing one runtime daemon don't see
// each other's containers.
func (l directLifecycle) listForTarget(ctx context.Context, exec Executor, project, target string) ([]UnitStatus, error) {
	cmd := fmt.Sprintf("%s ps --all --filter label=qqd.project=%s --filter label=qqd.target=%s --format '{{.Names}}'",
		l.cmd(), shellQuote(project), shellQuote(target))
	out, err := exec.Run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var statuses []UnitStatus
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		st, _ := l.Status(ctx, exec, name)
		statuses = append(statuses, st)
	}
	return statuses, nil
}

// Rename swaps a container's name. Used by blue-green: spin up <svc>-next,
// health-check, then rename(svc, svc-old) + rename(svc-next, svc) + rm svc-old.
// Podman supports `rename` while the container is running.
func (l directLifecycle) Rename(ctx context.Context, exec Executor, from, to string) error {
	_, err := exec.Run(ctx, fmt.Sprintf("%s rename %s %s", l.cmd(), shellQuote(from), shellQuote(to)))
	return err
}

// inspectPayload is a minimal subset of `podman inspect` output that
// directLifecycle needs. Anything we don't decode is ignored.
type inspectPayload struct {
	Name  string `json:"Name"`
	State struct {
		Status    string `json:"Status"`
		ExitCode  int    `json:"ExitCode"`
		StartedAt string `json:"StartedAt"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func parseInspectOne(raw string) UnitStatus {
	// Keep array handling for compatibility with older output shapes and tests.
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "[") {
		var arr []inspectPayload
		if err := json.Unmarshal([]byte(raw), &arr); err != nil || len(arr) == 0 {
			return UnitStatus{State: "unknown", RawDetail: raw}
		}
		return inspectToStatus(arr[0])
	}
	var one inspectPayload
	if err := json.Unmarshal([]byte(raw), &one); err != nil {
		return UnitStatus{State: "unknown", RawDetail: raw}
	}
	return inspectToStatus(one)
}

func inspectToStatus(p inspectPayload) UnitStatus {
	st := UnitStatus{
		Name:      strings.TrimPrefix(p.Name, "/"),
		State:     normalizeDockerState(p.State.Status),
		Image:     p.Config.Image,
		ExitCode:  p.State.ExitCode,
		RawDetail: p.State.Status,
	}
	if p.State.Health != nil {
		st.Health = strings.ToLower(p.State.Health.Status)
	}
	if labels := p.Config.Labels; labels != nil {
		st.Project = labels["qqd.project"]
		st.Target = labels["qqd.target"]
		st.Service = labels["qqd.service"]
		st.Role = labels["qqd.role"]
		st.DeployID = labels["qqd.deploy_id"]
		if r, err := strconv.Atoi(labels["qqd.replica"]); err == nil {
			st.Replica = r
		}
		if labels["qqd.image"] != "" {
			st.Image = labels["qqd.image"]
		}
	}
	if p.State.StartedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, p.State.StartedAt); err == nil && !t.IsZero() {
			d := time.Since(t).Seconds()
			if d > 0 {
				st.UptimeS = int64(d)
			}
		}
	}
	return st
}

// normalizeDockerState maps inspect .State.Status to UnitStatus.State.
//
//	running, restarting → "active"
//	created, paused     → "deploying"
//	exited, dead, removing → "inactive" or "failed" depending on ExitCode (caller knows)
func normalizeDockerState(s string) string {
	switch strings.ToLower(s) {
	case "running":
		return "active"
	case "restarting", "created":
		return "deploying"
	case "paused":
		return "inactive"
	case "exited":
		return "inactive"
	case "dead":
		return "failed"
	case "removing":
		return "deploying"
	default:
		return "unknown"
	}
}
