package qqd

import (
	"context"
	"strings"
	"testing"
)

func TestDirectLifecycleInstallEmitsPodmanRun(t *testing.T) {
	exec := newMockExecutor("local")
	lc := directLifecycle{rt: PodmanRuntime{}}

	spec := ContainerSpec{
		Project:    "obs",
		Service:    "api",
		Replica:    1,
		Container:  "obs-api",
		Network:    "obs",
		Image:      "ghcr.io/acme/api:v2",
		Role:       "app",
		DeployID:   "rel-42",
		ConfigHash: "abc123",
		Env:        map[string]string{"PORT": "8080"},
		Volumes:    []string{"/srv/data:/data:rw,U"},
		Health:     HealthConfig{Path: "/health", Port: 8080},
	}
	if err := lc.Install(context.Background(), exec, spec); err != nil {
		t.Fatalf("Install: %v", err)
	}

	all := strings.Join(exec.commands, "\n")
	wantSubstrings := []string{
		"podman rm -f 'obs-api'",
		"podman network create 'obs'",
		"podman run -d --restart=always --name 'obs-api'",
		"--network 'obs'",
		"-e PORT='8080'",
		"--health-cmd 'curl -sf http://localhost:8080/health || wget -q -O /dev/null http://localhost:8080/health || exit 1'",
		"--label qqd.project='obs'",
		"--label qqd.service='api'",
		"--label qqd.deploy_id='rel-42'",
		"'ghcr.io/acme/api:v2'",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(all, s) {
			t.Errorf("missing %q in commands:\n%s", s, all)
		}
	}
	if !strings.Contains(all, "-v '/srv/data:/data:rw,U'") {
		t.Errorf("podman volume should keep :U flag:\n%s", all)
	}
}

func TestDirectLifecyclePodmanInstallUsesAlwaysRestart(t *testing.T) {
	exec := newMockExecutor("local")
	lc := directLifecycle{rt: PodmanRuntime{}}
	spec := ContainerSpec{
		Project:   "obs",
		Service:   "api",
		Replica:   1,
		Container: "obs-api",
		Network:   "obs",
		Image:     "ghcr.io/acme/api:v2",
		Role:      "app",
		DeployID:  "rel-1",
	}
	if err := lc.Install(context.Background(), exec, spec); err != nil {
		t.Fatalf("Install: %v", err)
	}
	all := strings.Join(exec.commands, "\n")
	if !strings.Contains(all, "podman run -d --restart=always --name 'obs-api'") {
		t.Fatalf("podman should use --restart=always:\n%s", all)
	}
}

func TestDirectLifecycleStopAndRemove(t *testing.T) {
	exec := newMockExecutor("local")
	lc := directLifecycle{rt: PodmanRuntime{}}
	if err := lc.Stop(context.Background(), exec, "obs-api"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := lc.Remove(context.Background(), exec, "obs-api"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	all := strings.Join(exec.commands, "\n")
	for _, want := range []string{"podman stop 'obs-api'", "podman rm -f 'obs-api'"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in commands:\n%s", want, all)
		}
	}
}

func TestDirectLifecycleListFiltersByLabel(t *testing.T) {
	exec := newMockExecutor("local")
	lc := directLifecycle{rt: PodmanRuntime{}}
	if _, err := lc.List(context.Background(), exec, "obs"); err != nil {
		t.Fatalf("List: %v", err)
	}
	all := strings.Join(exec.commands, "\n")
	if !strings.Contains(all, "podman ps --all --filter label=qqd.project='obs'") {
		t.Fatalf("List must filter by qqd.project label:\n%s", all)
	}
}

func TestParseInspectOneFromArray(t *testing.T) {
	raw := `[{
  "Name": "/obs-api",
  "State": {"Status": "running", "ExitCode": 0, "StartedAt": "2026-04-25T10:00:00Z", "Health": {"Status": "healthy"}},
  "Config": {"Image": "ghcr.io/acme/api:v2", "Labels": {"qqd.project":"obs","qqd.service":"api","qqd.replica":"1","qqd.role":"app","qqd.deploy_id":"rel-42","qqd.image":"ghcr.io/acme/api:v2"}}
}]`
	st := parseInspectOne(raw)
	if st.State != "active" {
		t.Errorf("state: got %q want active", st.State)
	}
	if st.Project != "obs" || st.Service != "api" || st.Replica != 1 {
		t.Errorf("labels not picked up: %+v", st)
	}
	if st.DeployID != "rel-42" {
		t.Errorf("deploy_id: got %q", st.DeployID)
	}
	if st.Health != "healthy" {
		t.Errorf("health: got %q", st.Health)
	}
	if st.Name != "obs-api" {
		t.Errorf("name should strip leading slash, got %q", st.Name)
	}
}

func TestParseInspectOneFromPodman(t *testing.T) {
	// podman inspect --format '{{json .}}' returns a single object, not an array.
	raw := `{"Name":"obs-api","State":{"Status":"exited","ExitCode":137,"StartedAt":"2026-04-20T08:00:00Z"},"Config":{"Image":"ghcr.io/acme/api:v1","Labels":{"qqd.project":"obs"}}}`
	st := parseInspectOne(raw)
	if st.State != "inactive" {
		t.Errorf("state: got %q want inactive", st.State)
	}
	if st.ExitCode != 137 {
		t.Errorf("exit_code: got %d want 137", st.ExitCode)
	}
}

func TestChooseLifecycleSelection(t *testing.T) {
	cases := []struct {
		setting  string
		probeOK  bool
		wantBack string
		wantWhy  string
	}{
		{"systemd", true, "systemd", "config"},
		{"systemd", false, "systemd", "config"},
		{"direct", true, "direct", "config"},
		{"direct", false, "direct", "config"},
		{"", true, "systemd", "auto: systemctl present"},
		{"", false, "direct", "auto: systemctl missing"},
		{"auto", true, "systemd", "auto: systemctl present"},
		{"auto", false, "direct", "auto: systemctl missing"},
		{"AUTO", false, "direct", "auto: systemctl missing"},
	}
	for _, tc := range cases {
		got := chooseLifecycle(tc.setting, PodmanRuntime{}, tc.probeOK)
		if got.Backend != tc.wantBack || got.Reason != tc.wantWhy {
			t.Errorf("setting=%q probe=%v -> backend=%q reason=%q; want %q / %q",
				tc.setting, tc.probeOK, got.Backend, got.Reason, tc.wantBack, tc.wantWhy)
		}
		if got.Selected == nil {
			t.Errorf("setting=%q: nil Selected", tc.setting)
		}
		if got.Selected != nil && got.Selected.Name() != tc.wantBack {
			t.Errorf("setting=%q: backend Name() = %q; want %q", tc.setting, got.Selected.Name(), tc.wantBack)
		}
	}
}

func TestProbeSystemctlMissing(t *testing.T) {
	exec := newMockExecutor("local")
	exec.failCmds = map[string]int{"command -v systemctl": 1}
	if probeSystemctl(context.Background(), exec) {
		t.Fatal("probe should report false when the command fails")
	}
}

func TestProbeSystemctlPresent(t *testing.T) {
	exec := newMockExecutor("local")
	if !probeSystemctl(context.Background(), exec) {
		t.Fatal("probe should report true when the command succeeds (mock returns no error by default)")
	}
}

func TestAppLifecycleForExplicitSetting(t *testing.T) {
	app := &App{}
	exec := newMockExecutor("local")
	sel := app.lifecycleFor(context.Background(), TargetConfig{Name: "alpha", Lifecycle: "direct"}, exec)
	if sel.Backend != "direct" || sel.Reason != "config" {
		t.Fatalf("explicit direct should not probe; got backend=%q reason=%q", sel.Backend, sel.Reason)
	}
	// No probe happened.
	for _, c := range exec.commands {
		if strings.Contains(c, "command -v systemctl") {
			t.Errorf("explicit setting must not probe systemctl, but ran: %s", c)
		}
	}
	// Cached: a second call for the same target hits no executor at all.
	exec2 := newMockExecutor("local")
	again := app.lifecycleFor(context.Background(), TargetConfig{Name: "alpha", Lifecycle: "direct"}, exec2)
	if again.Backend != "direct" {
		t.Fatalf("cached lookup mismatch: %+v", again)
	}
	if len(exec2.commands) != 0 {
		t.Errorf("cached call must not touch the executor; got %v", exec2.commands)
	}
}

func TestAppLifecycleForAutoProbes(t *testing.T) {
	app := &App{}
	// Empty (== auto) and probe succeeds → systemd.
	exec := newMockExecutor("local")
	sel := app.lifecycleFor(context.Background(), TargetConfig{Name: "beta"}, exec)
	if sel.Backend != "systemd" {
		t.Fatalf("auto with probe success should pick systemd; got %q (reason %q)", sel.Backend, sel.Reason)
	}
	probed := false
	for _, c := range exec.commands {
		if strings.Contains(c, "command -v systemctl") {
			probed = true
			break
		}
	}
	if !probed {
		t.Fatal("auto must probe systemctl")
	}

	// auto + probe fails → direct.
	app2 := &App{}
	exec3 := newMockExecutor("local")
	exec3.failCmds = map[string]int{"command -v systemctl": 1}
	sel = app2.lifecycleFor(context.Background(), TargetConfig{Name: "gamma", Lifecycle: "auto"}, exec3)
	if sel.Backend != "direct" {
		t.Fatalf("auto with probe failure should pick direct; got %q", sel.Backend)
	}
}

func TestDirectModeDeployRunsContainersNotSystemctl(t *testing.T) {
	cfg := ProjectConfig{
		Name:    "obs",
		Repo:    "git@example:obs.git",
		Runtime: "podman",
		Sync:    "upload",
		Services: map[string]ServiceConfig{
			"web": {Image: "nginx:1.27", Health: HealthConfig{Path: "/", Port: 80}},
		},
		Targets: map[string]TargetConfig{
			"alpha": {
				Name:      "alpha",
				Host:      "local",
				RepoDir:   "/tmp/obs",
				Services:  []string{"web"},
				Lifecycle: "direct",
				Expose: ExposeConfig{
					Entries: []ExposeEntry{{HostPort: 8080, Routes: map[string]string{"/": "web:80"}}},
				},
			},
		},
	}
	targetExec := newMockExecutor("alpha")
	app := &App{
		ExecFactory: mockFactory{
			local:   newMockExecutor("local"),
			targets: map[string]*mockExecutor{"alpha": targetExec},
		},
		Stdout:    discardWriter{},
		NoLock:    true,
		Runtime:   PodmanRuntime{},
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "alpha", nil, false); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	all := strings.Join(targetExec.commands, "\n")
	// Direct mode must not touch systemctl or write quadlets / unit files.
	for _, banned := range []string{
		"systemctl",
		"daemon-reload",
		".container",              // podman quadlet extension
		"obs-alpha-web.service",   // systemd unit name shape
		"obs-alpha-proxy.service", // proxy systemd unit
		"/etc/systemd/system",
		"/.config/containers/systemd",
	} {
		if strings.Contains(all, banned) {
			t.Errorf("direct deploy must not run %q; got commands:\n%s", banned, all)
		}
	}
	// Container names are target-scoped: <project>-<target>-<service>.
	if !strings.Contains(all, "podman run -d --restart=always --name 'obs-alpha-web'") {
		t.Errorf("expected podman run for obs-alpha-web; got:\n%s", all)
	}
	// Network alias preserves the legacy <project>-<service> DNS so user
	// envs that reference services by short name still resolve.
	if !strings.Contains(all, "--network-alias 'obs-web'") {
		t.Errorf("expected --network-alias 'obs-web'; got:\n%s", all)
	}
	if !strings.Contains(all, "--label qqd.target='alpha'") {
		t.Errorf("expected qqd.target label; got:\n%s", all)
	}
	if !strings.Contains(all, "--label qqd.service='web'") {
		t.Errorf("expected qqd.service label for web; got:\n%s", all)
	}
	// Proxy container is also target-scoped.
	if !strings.Contains(all, "podman run -d --restart=always --name 'obs-alpha-proxy'") {
		t.Errorf("expected target-scoped proxy container; got:\n%s", all)
	}
	// Network is target-scoped.
	if !strings.Contains(all, "podman network create 'obs-alpha'") {
		t.Errorf("expected target-scoped network 'obs-alpha'; got:\n%s", all)
	}
}

func TestDirectModeDestroyRemovesLabeledContainers(t *testing.T) {
	cfg := ProjectConfig{
		Name:    "obs",
		Repo:    "git@example:obs.git",
		Runtime: "podman",
		Services: map[string]ServiceConfig{
			"web": {Image: "nginx:1.27"},
		},
		Targets: map[string]TargetConfig{
			"alpha": {
				Name:      "alpha",
				Host:      "local",
				RepoDir:   "/tmp/obs",
				Services:  []string{"web"},
				Lifecycle: "direct",
			},
		},
	}
	targetExec := newMockExecutor("alpha")
	// We don't pre-seed ps output; the mock returns "" by default which means
	// directLifecycle.List sees zero containers. Destroy should still clean
	// the proxy config dir and not touch systemctl. The test below pins those
	// behaviors and the qqd ps --filter call.
	app := &App{
		ExecFactory: mockFactory{
			local:   newMockExecutor("local"),
			targets: map[string]*mockExecutor{"alpha": targetExec},
		},
		Stdout:  discardWriter{},
		NoLock:  true,
		Runtime: PodmanRuntime{},
	}
	if err := app.Destroy(context.Background(), cfg, "alpha"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	all := strings.Join(targetExec.commands, "\n")
	if strings.Contains(all, "systemctl") {
		t.Errorf("direct destroy must not run systemctl:\n%s", all)
	}
	// Destroy is target-scoped: lists containers labeled with both
	// qqd.project and qqd.target so destroying alpha never touches bravo.
	if !strings.Contains(all, "podman ps --all --filter label=qqd.project='obs' --filter label=qqd.target='alpha'") {
		t.Errorf("destroy should list project containers via project+target labels; got:\n%s", all)
	}
	if !strings.Contains(all, "rm -rf ~/.config/qqd/obs/alpha") {
		t.Errorf("destroy should clean target-scoped proxy config dir ~/.config/qqd/obs/alpha; got:\n%s", all)
	}
}

// discardWriter is used to suppress test output without pulling in io/ioutil.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestDirectModeMultiTargetSameDaemonNoCollision pins the regression where
// two targets sharing one runtime daemon (e.g. macOS local: alpha + bravo
// both deploying to the same Podman machine) trampled each other. Container names
// must include the target so they coexist; the sweep must filter by
// qqd.target so deploying bravo never removes alpha's containers; the
// destroy must filter by qqd.target so destroying bravo never affects
// alpha.
func TestDirectModeMultiTargetSameDaemonNoCollision(t *testing.T) {
	cfg := ProjectConfig{
		Name:    "obs",
		Repo:    "git@example:obs.git",
		Runtime: "podman",
		Sync:    "upload",
		Services: map[string]ServiceConfig{
			"web": {Image: "nginx:1.27"},
			"db":  {Image: "postgres:16.4"},
			"api": {Image: "ghcr.io/x/api:v1"},
		},
		Targets: map[string]TargetConfig{
			"alpha": {
				Name:      "alpha",
				Host:      "local",
				RepoDir:   "/tmp/obs-alpha",
				Services:  []string{"web", "db", "api"},
				Lifecycle: "direct",
			},
			"bravo": {
				Name:      "bravo",
				Host:      "local",
				RepoDir:   "/tmp/obs-bravo",
				Services:  []string{"api"},
				Lifecycle: "direct",
			},
		},
	}
	alphaExec := newMockExecutor("alpha")
	bravoExec := newMockExecutor("bravo")
	app := &App{
		ExecFactory: mockFactory{
			local: newMockExecutor("local"),
			targets: map[string]*mockExecutor{
				"alpha": alphaExec,
				"bravo": bravoExec,
			},
		},
		Stdout:    discardWriter{},
		NoLock:    true,
		Runtime:   PodmanRuntime{},
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "alpha", nil, false); err != nil {
		t.Fatalf("Deploy alpha: %v", err)
	}
	// Start a fresh app for bravo so the lifecycle cache mirrors a real
	// second invocation. (In production, two consecutive `qqd deploy` runs
	// each start a new App; this models that.)
	app2 := &App{
		ExecFactory: mockFactory{
			local: newMockExecutor("local"),
			targets: map[string]*mockExecutor{
				"alpha": alphaExec,
				"bravo": bravoExec,
			},
		},
		Stdout:    discardWriter{},
		NoLock:    true,
		Runtime:   PodmanRuntime{},
		DrainWait: -1,
	}
	if err := app2.Deploy(context.Background(), cfg, "bravo", nil, false); err != nil {
		t.Fatalf("Deploy bravo: %v", err)
	}

	// alpha's containers must still exist (the deploy happens against
	// alphaExec, the bravo deploy against bravoExec - in production they'd
	// be the same Podman machine, but we assert the *commands*: bravo's
	// install must use target-scoped names so they don't collide with
	// alpha's, and bravo's sweep must be target-scoped so it never tries
	// to podman rm -f any of alpha's containers).
	bravoCmds := strings.Join(bravoExec.commands, "\n")
	for _, banned := range []string{
		"podman rm -f 'obs-alpha-web'",
		"podman rm -f 'obs-alpha-db'",
		"podman rm -f 'obs-alpha-api'",
		"podman rm -f 'obs-alpha-proxy'",
	} {
		if strings.Contains(bravoCmds, banned) {
			t.Errorf("bravo deploy must not touch alpha's containers; ran: %s", banned)
		}
	}
	// bravo's containers must use bravo-scoped names.
	if !strings.Contains(bravoCmds, "podman run -d --restart=always --name 'obs-bravo-api'") {
		t.Errorf("bravo's api should be named obs-bravo-api; got commands:\n%s", bravoCmds)
	}
	// bravo's sweep must enumerate all project containers (so legacy
	// orphans without qqd.target get caught) - the in-memory filter then
	// skips containers belonging to other targets.
	if !strings.Contains(bravoCmds, "podman ps --all --filter label=qqd.project='obs' --format '{{.Names}}'") {
		t.Errorf("bravo sweep must list all project containers (to catch legacy orphans); got:\n%s", bravoCmds)
	}
}

func TestDirectModeExpandsTildeInVolumeMounts(t *testing.T) {
	cfg := ProjectConfig{
		Name:    "obs",
		Repo:    "git@example:obs.git",
		Runtime: "podman",
		Sync:    "upload",
		Services: map[string]ServiceConfig{
			"data": {
				Image:   "alpine:3",
				Volumes: []string{"~/state/obs:/data:rw"},
			},
		},
		Targets: map[string]TargetConfig{
			"alpha": {
				Name:      "alpha",
				Host:      "local",
				RepoDir:   "/tmp/obs",
				Services:  []string{"data"},
				Lifecycle: "direct",
			},
		},
	}
	targetExec := newMockExecutor("alpha")
	app := &App{
		ExecFactory: mockFactory{
			local:   newMockExecutor("local"),
			targets: map[string]*mockExecutor{"alpha": targetExec},
		},
		Stdout:    discardWriter{},
		NoLock:    true,
		Runtime:   PodmanRuntime{},
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "alpha", nil, false); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	all := strings.Join(targetExec.commands, "\n")
	// Mock's printf %s "$HOME" returns "" (no special handler). The
	// fallback uses os.UserHomeDir(), which on the dev host is a real
	// absolute path. Either way, the literal "~" must NOT survive into
	// the podman run argument.
	if strings.Contains(all, "-v '~/state/obs:/data:rw'") {
		t.Fatalf("podman -v must not contain unexpanded ~; got:\n%s", all)
	}
	// And it must contain a -v with /data:rw on the right side.
	if !strings.Contains(all, ":/data:rw'") {
		t.Fatalf("expected expanded -v ending in :/data:rw; got:\n%s", all)
	}
}

// TestDirectModeProxyConfigIsTargetScoped pins the regression where two
// targets sharing one host wrote their proxy routes to the same
// ~/.config/qqd/<project>/dynamic/routes.yml, so whichever deployed
// second silently overwrote the first's routes (and the first target's
// proxy began serving the second target's entrypoints, returning 404 for
// every request to the first's exposed ports).
func TestDirectModeProxyConfigIsTargetScoped(t *testing.T) {
	cfg := ProjectConfig{
		Name:    "obs",
		Repo:    "git@example:obs.git",
		Runtime: "podman",
		Sync:    "upload",
		Services: map[string]ServiceConfig{
			"web": {Image: "nginx:1.27"},
		},
		Targets: map[string]TargetConfig{
			"alpha": {
				Name:      "alpha",
				Host:      "local",
				RepoDir:   "/tmp/obs-alpha",
				Services:  []string{"web"},
				Lifecycle: "direct",
				Expose: ExposeConfig{
					Entries: []ExposeEntry{{HostPort: 8080, Routes: map[string]string{"/": "web:80"}}},
				},
			},
		},
	}
	targetExec := newMockExecutor("alpha")
	app := &App{
		ExecFactory: mockFactory{
			local:   newMockExecutor("local"),
			targets: map[string]*mockExecutor{"alpha": targetExec},
		},
		Stdout:    discardWriter{},
		NoLock:    true,
		Runtime:   PodmanRuntime{},
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "alpha", nil, false); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	all := strings.Join(targetExec.commands, "\n")
	// Heredoc paths use ~ (shell expands at write time). Mount paths are
	// pre-expanded to /home/testuser by the mock $HOME probe.
	for _, want := range []string{
		// Static config goes to the target-scoped dir.
		"cat > ~/.config/qqd/obs/alpha/traefik.yml",
		// Dynamic dir is target-scoped.
		"mkdir -p ~/.config/qqd/obs/alpha/dynamic",
		// Proxy mounts the target-scoped paths (expanded against $HOME).
		"-v '/home/testuser/.config/qqd/obs/alpha/traefik.yml:/etc/traefik/traefik.yml:ro'",
		"-v '/home/testuser/.config/qqd/obs/alpha/dynamic:/etc/traefik/dynamic:ro'",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in commands; full output:\n%s", want, all)
		}
	}
	// The project-only (un-target-scoped) paths must NOT appear.
	for _, banned := range []string{
		"cat > ~/.config/qqd/obs/dynamic",
		"cat > ~/.config/qqd/obs/traefik.yml",
		"-v '/home/testuser/.config/qqd/obs/traefik.yml",
		"-v '/home/testuser/.config/qqd/obs/dynamic",
	} {
		if strings.Contains(all, banned) {
			t.Errorf("direct mode must not use the project-only proxy config path %q; got:\n%s", banned, all)
		}
	}
}

func TestDirectModeSweepsLegacyOrphansBeforeInstall(t *testing.T) {
	cfg := ProjectConfig{
		Name:    "obs",
		Repo:    "git@example:obs.git",
		Runtime: "podman",
		Sync:    "upload",
		Services: map[string]ServiceConfig{
			"web": {Image: "nginx:1.27"},
		},
		Targets: map[string]TargetConfig{
			"alpha": {
				Name:      "alpha",
				Host:      "local",
				RepoDir:   "/tmp/obs",
				Services:  []string{"web"},
				Lifecycle: "direct",
				Expose: ExposeConfig{
					Entries: []ExposeEntry{{HostPort: 8080, Routes: map[string]string{"/": "web:80"}}},
				},
			},
		},
	}
	targetExec := newMockExecutor("alpha")
	// Pre-seed a legacy container: same project, no qqd.target. Mirrors
	// the upgrade scenario where a previous qqd version created a
	// non-target-scoped container that's still bound to the proxy port.
	targetExec.containers["obs-proxy"] = containerSnap{
		image: "docker.io/library/traefik:v3.6",
		labels: map[string]string{
			"qqd.project": "obs",
			"qqd.role":    "proxy",
			// qqd.target intentionally absent: this is a pre-target-scoped orphan.
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			local:   newMockExecutor("local"),
			targets: map[string]*mockExecutor{"alpha": targetExec},
		},
		Stdout:    discardWriter{},
		NoLock:    true,
		Runtime:   PodmanRuntime{},
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "alpha", nil, false); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	all := strings.Join(targetExec.commands, "\n")
	// The legacy orphan must be removed (so its port is freed).
	if !strings.Contains(all, "podman rm -f 'obs-proxy'") {
		t.Errorf("legacy orphan obs-proxy must be swept; got:\n%s", all)
	}
	// And the sweep must happen BEFORE the new proxy install, otherwise
	// the new proxy can't bind the port.
	rmIdx := strings.Index(all, "podman rm -f 'obs-proxy'")
	installIdx := strings.Index(all, "podman run -d --restart=always --name 'obs-alpha-proxy'")
	if rmIdx < 0 || installIdx < 0 || rmIdx > installIdx {
		t.Errorf("orphan sweep must precede proxy install (rm idx=%d, install idx=%d):\n%s", rmIdx, installIdx, all)
	}
}

func TestSystemdLifecycleInstallWritesUnitAndStarts(t *testing.T) {
	exec := newMockExecutor("local")
	lc := systemdLifecycle{rt: PodmanRuntime{}}
	spec := ContainerSpec{
		Project:   "obs",
		Service:   "api",
		Replica:   1,
		Container: "obs-api",
		Image:     "ghcr.io/acme/api:v1",
		Role:      "app",
	}
	if err := lc.Install(context.Background(), exec, spec); err != nil {
		t.Fatalf("Install: %v", err)
	}
	all := strings.Join(exec.commands, "\n")
	for _, want := range []string{
		"mkdir -p ~/.config/containers/systemd",
		"cat > ~/.config/containers/systemd/obs-api.container",
		"systemctl --user daemon-reload",
		"systemctl --user start 'obs-api.container'",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in commands:\n%s", want, all)
		}
	}
}
