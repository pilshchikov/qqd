package qqd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
)

type mockExecutor struct {
	mu            sync.Mutex
	id            string
	commands      []string
	existingImage map[string]bool
	imageIDs      map[string]string // tag -> image ID
	imageUsers    map[string]string // tag -> image Config.User
	buildCounter  int               // incremented on each build to generate new IDs
	copyFromCalls [][2]string
	copyToCalls   [][2]string
	failAll       bool
	failCmds      map[string]int           // cmd substring -> remaining fail count (decremented on each match)
	healthStatus  map[string]string        // container name -> health status
	files         map[string]string        // path -> content (simulates remote filesystem)
	unitStates    map[string]string        // unit name -> state (overrides default "active")
	stdoutFor     map[string]string        // cmd substring -> canned stdout
	containers    map[string]containerSnap // direct-mode: containers seen via `podman run -d --name X`
}

type containerSnap struct {
	image  string
	labels map[string]string
}

func newMockExecutor(id string) *mockExecutor {
	return &mockExecutor{
		id:            id,
		existingImage: map[string]bool{},
		imageIDs:      map[string]string{},
		imageUsers:    map[string]string{},
		failCmds:      map[string]int{},
		healthStatus:  map[string]string{},
		stdoutFor:     map[string]string{},
		files:         map[string]string{},
		containers:    map[string]containerSnap{},
	}
}

func TestAnnotateVolumeOwnership(t *testing.T) {
	exec := newMockExecutor("local")
	exec.imageUsers["ghcr.io/acme/app:1"] = "1000:1000"
	exec.imageUsers["docker.io/library/postgres:16"] = "root"

	app := &App{Runtime: PodmanRuntime{}}
	services := map[string]ServiceConfig{
		"app": {
			Image:   "ghcr.io/acme/app:1",
			Volumes: []string{"/srv/app:/app/data"},
		},
		"db": {
			Image:   "docker.io/library/postgres:16",
			Volumes: []string{"/srv/db:/var/lib/postgresql/data"},
		},
		"worker": {
			Image:   "ghcr.io/acme/worker:1",
			User:    "1001:1001",
			Volumes: []string{"/srv/worker:/data"},
		},
		"stateless": {
			Image: "ghcr.io/acme/stateless:1",
		},
	}

	got := app.annotateVolumeOwnership(context.Background(), exec, services)
	if !got["app"].volumeNeedsU {
		t.Fatal("image USER with non-root uid should enable :U")
	}
	if got["db"].volumeNeedsU {
		t.Fatal("root image USER should not enable :U")
	}
	if !got["worker"].volumeNeedsU {
		t.Fatal("explicit non-root service user should enable :U")
	}
	if got["stateless"].volumeNeedsU {
		t.Fatal("services without volumes do not need :U annotation")
	}
}

func (m *mockExecutor) RunStream(ctx context.Context, cmd string, _ io.Writer) error {
	_, err := m.Run(ctx, cmd)
	return err
}

func (m *mockExecutor) Run(_ context.Context, cmd string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commands = append(m.commands, cmd)
	if m.failAll {
		return "", errors.New("executor unavailable")
	}
	for pattern, count := range m.failCmds {
		if count > 0 && strings.Contains(cmd, pattern) {
			m.failCmds[pattern]--
			if m.failCmds[pattern] <= 0 {
				delete(m.failCmds, pattern)
			}
			return "", fmt.Errorf("simulated failure: %s", pattern)
		}
	}
	for pattern, out := range m.stdoutFor {
		if strings.Contains(cmd, pattern) {
			return out, nil
		}
	}
	if strings.Contains(cmd, "podman inspect --format '{{.State.Health.Status}}'") {
		for cname, status := range m.healthStatus {
			if strings.Contains(cmd, cname) {
				return status + "\n", nil
			}
		}
		return "starting\n", nil
	}
	// Docker image existence check: docker image inspect ... >/dev/null 2>&1
	if strings.Contains(cmd, "docker image inspect") && strings.Contains(cmd, ">/dev/null 2>&1") {
		for image := range m.existingImage {
			if strings.Contains(cmd, image) {
				return "", nil
			}
		}
		return "", errors.New("missing image")
	}
	// Image config user lookup: podman/docker image inspect --format '{{.Config.User}}'
	if strings.Contains(cmd, "image inspect --format '{{.Config.User}}'") {
		for tag, user := range m.imageUsers {
			if strings.Contains(cmd, tag) {
				return user + "\n", nil
			}
		}
		return "\n", nil
	}
	// Image ID lookup: podman/docker image inspect --format '{{.Id}}'
	if strings.Contains(cmd, "podman image inspect") || strings.Contains(cmd, "docker image inspect") {
		for tag, id := range m.imageIDs {
			if strings.Contains(cmd, tag) {
				return id + "\n", nil
			}
		}
		return "", errors.New("image not found")
	}
	// Podman image existence check
	if strings.Contains(cmd, "podman image exists") {
		for image := range m.existingImage {
			if strings.Contains(cmd, image) {
				return "", nil
			}
		}
		return "", errors.New("missing image")
	}
	if strings.Contains(cmd, "podman build") || strings.Contains(cmd, "docker build") {
		m.buildCounter++
		rest := cmd
		for {
			img := extractBetween(rest, "-t '", "'")
			if img == "" {
				break
			}
			m.existingImage[img] = true
			m.imageIDs[img] = fmt.Sprintf("sha256:build%d", m.buildCounter)
			rest = rest[strings.Index(rest, "-t '")+4:]
		}
		return "", nil
	}
	if strings.Contains(cmd, "podman pull") || strings.Contains(cmd, "docker pull") {
		for _, prefix := range []string{"podman pull '", "docker pull '"} {
			img := extractBetween(cmd, prefix, "'")
			if img != "" {
				m.existingImage[img] = true
				return "", nil
			}
		}
		return "", nil
	}
	if strings.Contains(cmd, "podman load -i") || strings.Contains(cmd, "docker load -i") {
		return "", nil
	}
	if strings.Contains(cmd, "podman save") {
		return "", nil
	}
	// Direct-mode: podman run -d --name X --label qqd.* ... <image>
	// Track the container so subsequent inspect/ps queries reflect it.
	if strings.Contains(cmd, " run -d ") && strings.HasPrefix(cmd, "podman ") {
		name := extractBetween(cmd, "--name '", "'")
		image := mockExtractTrailingImage(cmd)
		labels := mockExtractLabels(cmd)
		if name != "" {
			m.containers[name] = containerSnap{image: image, labels: labels}
		}
		return "", nil
	}
	// Direct-mode: podman rm -f <name> 2>/dev/null || true
	if strings.HasPrefix(cmd, "podman rm -f ") {
		name := extractBetween(cmd, "rm -f '", "'")
		if name == "" {
			name = strings.TrimSuffix(strings.Fields(cmd)[3], "'")
		}
		delete(m.containers, name)
		return "", nil
	}
	// Direct-mode: podman stop <name>
	if strings.HasPrefix(cmd, "podman stop ") {
		// Stop doesn't remove the container; we leave the snap in place.
		return "", nil
	}
	// Direct-mode: podman inspect <name> --format '{{json .}}' 2>/dev/null || true
	if strings.HasPrefix(cmd, "podman inspect ") && strings.Contains(cmd, "{{json .}}") {
		name := extractBetween(cmd, "inspect '", "'")
		snap, ok := m.containers[name]
		if !ok {
			return "", nil
		}
		labels := "{"
		first := true
		for k, v := range snap.labels {
			if !first {
				labels += ","
			}
			first = false
			labels += fmt.Sprintf("%q:%q", k, v)
		}
		labels += "}"
		// Emit a "running" container so waitContainerActive sees it as active.
		// Wrap in array since the parser handles both array and object shapes.
		return fmt.Sprintf(`[{"Name":"/%s","State":{"Status":"running","ExitCode":0,"StartedAt":"2026-04-25T10:00:00Z","Health":{"Status":"healthy"}},"Config":{"Image":%q,"Labels":%s}}]`, name, snap.image, labels), nil
	}
	// Direct-mode: podman ps --all --filter label=qqd.project='X' --format '{{.Names}}'
	if strings.HasPrefix(cmd, "podman ps ") && strings.Contains(cmd, "--filter label=qqd.project=") {
		project := extractBetween(cmd, "qqd.project='", "'")
		var names []string
		for n, snap := range m.containers {
			if snap.labels["qqd.project"] == project {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		return strings.Join(names, "\n"), nil
	}
	// Direct-mode: top-level `podman network create|rm <name>` - accept
	// silently. Anchored to HasPrefix so it doesn't match heredocs that happen
	// to embed a podman network command inside a unit file body.
	if strings.HasPrefix(cmd, "podman network ") {
		return "", nil
	}
	// Direct-mode: probe systemctl with `command -v systemctl`. Default is success.
	if strings.HasPrefix(cmd, "command -v systemctl") {
		return "", nil
	}
	// Direct-mode: $HOME probe used by App.homeDirFor to expand "~/..."
	// volume mounts. Tests get a stable fake home so assertions don't
	// depend on the developer machine running them.
	if strings.HasPrefix(cmd, "printf %s \"$HOME\"") {
		return "/home/testuser", nil
	}
	// Handle file writes (heredoc), including atomic writes (heredoc + mv)
	// Supports "cat > path <<'DELIM'", "cat > tmp <<'DELIM' && mv tmp path"
	// and "sudo sh -c 'cat > path' <<'DELIM'" for any qqd heredoc delimiter.
	if hdrEnd := strings.Index(cmd, "\n"); hdrEnd > 0 && strings.Contains(cmd[:hdrEnd], " <<'") {
		header := cmd[:hdrEnd]
		dStart := strings.Index(header, " <<'") + len(" <<'")
		dEnd := strings.Index(header[dStart:], "'")
		if dEnd > 0 {
			delim := header[dStart : dStart+dEnd]
			var path string
			switch {
			case strings.HasPrefix(header, "cat > "):
				rest := header[len("cat > "):]
				path = rest[:strings.Index(rest, " <<'")]
			case strings.Contains(header, "sh -c 'cat > "):
				rest := header[strings.Index(header, "sh -c 'cat > ")+len("sh -c 'cat > "):]
				if end := strings.Index(rest, "'"); end > 0 {
					path = rest[:end]
				}
			}
			if path != "" {
				body := cmd[hdrEnd+1:]
				body = strings.TrimSuffix(body, delim)
				m.files[path] = body
				// Atomic write: "... && mv <tmp> <dst>" on the command line.
				if mvIdx := strings.Index(header, "&& mv "); mvIdx >= 0 {
					mvParts := strings.Fields(header[mvIdx+len("&& "):])
					if len(mvParts) == 3 && mvParts[0] == "mv" {
						m.files[mvParts[2]] = m.files[mvParts[1]]
						delete(m.files, mvParts[1])
					}
				}
				return "", nil
			}
		}
	}
	// Ownership markers: grep -H '^# qqd-project=' <dir>/*
	if strings.HasPrefix(cmd, "grep -H '^# qqd-project=' ") {
		dir := strings.TrimSuffix(cmd[len("grep -H '^# qqd-project=' "):], "/* 2>/dev/null || true")
		var lines []string
		for path, content := range m.files {
			if !strings.HasPrefix(path, dir+"/") || strings.Contains(path[len(dir)+1:], "/") {
				continue
			}
			for _, line := range strings.Split(content, "\n") {
				if strings.HasPrefix(line, "# qqd-project=") {
					lines = append(lines, path+":"+line)
					break
				}
			}
		}
		sort.Strings(lines)
		return strings.Join(lines, "\n"), nil
	}
	// Handle sha256sum over one or more quoted paths (TLS fingerprinting)
	if strings.HasPrefix(cmd, "sha256sum ") {
		var lines []string
		for _, path := range sortedKeys(m.files) {
			if !strings.Contains(cmd, path) {
				continue
			}
			sum := sha256.Sum256([]byte(m.files[path]))
			lines = append(lines, fmt.Sprintf("%x  %s", sum, path))
		}
		return strings.Join(lines, "\n"), nil
	}
	// Handle file reads
	if strings.HasPrefix(cmd, "cat ") && strings.Contains(cmd, " 2>/dev/null || true") {
		path := cmd[len("cat "):strings.Index(cmd, " 2>/dev/null || true")]
		if content, ok := m.files[path]; ok {
			return content, nil
		}
		return "", nil
	}
	// Handle ls -1 for directory listing (used by stale quadlet detection)
	if strings.HasPrefix(cmd, "ls -1 ") && strings.HasSuffix(cmd, " 2>/dev/null || true") {
		dir := cmd[len("ls -1 ") : len(cmd)-len(" 2>/dev/null || true")]
		var listing []string
		for path := range m.files {
			if strings.HasPrefix(path, dir+"/") {
				name := path[len(dir)+1:]
				if !strings.Contains(name, "/") {
					listing = append(listing, name)
				}
			}
		}
		sort.Strings(listing)
		return strings.Join(listing, "\n"), nil
	}
	// Handle rm -f / rm -rf for file removal (supports simple * globs)
	// Also handles "sudo rm -f ..." prefix
	rmCmd := cmd
	if strings.HasPrefix(rmCmd, "sudo ") {
		rmCmd = rmCmd[len("sudo "):]
	}
	if strings.HasPrefix(rmCmd, "rm -f ") || strings.HasPrefix(rmCmd, "rm -rf ") {
		fields := strings.Fields(rmCmd)
		for _, part := range fields[2:] {
			path := strings.ReplaceAll(strings.ReplaceAll(part, "'", ""), "\"", "")
			if strings.Contains(path, "*") {
				// Simple glob: expand against known files
				prefix := path[:strings.Index(path, "*")]
				suffix := path[strings.Index(path, "*")+1:]
				for f := range m.files {
					if strings.HasPrefix(f, prefix) && strings.HasSuffix(f, suffix) {
						delete(m.files, f)
					}
				}
			} else {
				delete(m.files, path)
			}
		}
		return "", nil
	}
	// Handle test -f for file existence checks
	if strings.HasPrefix(cmd, "test -f ") {
		path := strings.TrimPrefix(cmd, "test -f ")
		path = strings.ReplaceAll(strings.ReplaceAll(path, "'", ""), "\"", "")
		if _, ok := m.files[path]; ok {
			return "", nil
		}
		return "", errors.New("file not found")
	}
	// Handle systemctl restart (both --user and sudo): flip unit state to active
	if strings.Contains(cmd, "systemctl") && strings.Contains(cmd, "restart") {
		for unit := range m.unitStates {
			if strings.Contains(cmd, unit) {
				m.unitStates[unit] = "active"
			}
		}
		return "", nil
	}
	// Handle systemctl is-active with unit state overrides (both --user and sudo)
	if strings.Contains(cmd, "systemctl") && strings.Contains(cmd, "is-active") {
		for unit, state := range m.unitStates {
			if strings.Contains(cmd, unit) {
				if state != "active" {
					if strings.Contains(cmd, "|| true") {
						return state + "\n", nil
					}
					return state + "\n", fmt.Errorf("unit %s is %s", unit, state)
				}
				return "active\n", nil
			}
		}
	}
	return "active\n", nil
}

func extractBetween(s, left, right string) string {
	start := strings.Index(s, left)
	if start == -1 {
		return ""
	}
	start += len(left)
	end := strings.Index(s[start:], right)
	if end == -1 {
		return ""
	}
	return s[start : start+end]
}

// mockExtractTrailingImage pulls the image argument out of a podman
// `run -d ... <image>` command. The image is the last shell-quoted token
// before any trailing command args. We approximate by taking the last
// single-quoted token in the string.
func mockExtractTrailingImage(cmd string) string {
	last := -1
	for i := len(cmd) - 1; i >= 0; i-- {
		if cmd[i] == '\'' {
			if last == -1 {
				last = i
				continue
			}
			return cmd[i+1 : last]
		}
	}
	return ""
}

// mockExtractLabels pulls all `--label k=v` pairs out of a run command. v
// is single-quoted in the qqd-emitted command line.
func mockExtractLabels(cmd string) map[string]string {
	out := map[string]string{}
	rest := cmd
	for {
		idx := strings.Index(rest, "--label ")
		if idx < 0 {
			break
		}
		rest = rest[idx+len("--label "):]
		eq := strings.Index(rest, "=")
		if eq < 0 {
			break
		}
		key := rest[:eq]
		rest = rest[eq+1:]
		if !strings.HasPrefix(rest, "'") {
			continue
		}
		end := strings.Index(rest[1:], "'")
		if end < 0 {
			break
		}
		out[key] = rest[1 : 1+end]
		rest = rest[1+end+1:]
	}
	return out
}

func (m *mockExecutor) CopyFrom(_ context.Context, remotePath, localPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.copyFromCalls = append(m.copyFromCalls, [2]string{remotePath, localPath})
	return nil
}

func (m *mockExecutor) CopyTo(_ context.Context, localPath, remotePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.copyToCalls = append(m.copyToCalls, [2]string{localPath, remotePath})
	return nil
}

func (m *mockExecutor) Close() error { return nil }
func (m *mockExecutor) ID() string   { return m.id }

type mockFactory struct {
	local     *mockExecutor
	targets   map[string]*mockExecutor
	buildHost map[string]*mockExecutor
}

func (f mockFactory) Local() Executor {
	return f.local
}

func (f mockFactory) ForTarget(t TargetConfig) (Executor, error) {
	return f.targets[t.Name], nil
}

func (f mockFactory) ForBuildHost(b BuildConfig) (Executor, error) {
	return f.buildHost[b.Host], nil
}

func TestDeployLocalBuildAndPull(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:   "report",
		Repo:   "git@github.com:acme/report.git",
		Branch: "master",
		Build: BuildConfig{
			Strategy: "local",
			CPU:      2,
			Memory:   "2g",
		},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/report/server:1.44",
				Dockerfile: "backend/server/Dockerfile",
			},
			"db": {
				Image: "postgres:16.1",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/report/repo",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "podman build --cpu-period=100000 --cpu-quota=200000 --memory='2g'") {
		t.Fatalf("build command missing limits:\n%s", cmds)
	}
	if !strings.Contains(cmds, "cd '/home/centos/report/repo' && podman build") {
		t.Fatalf("build command should cd then build:\n%s", cmds)
	}
	if !strings.Contains(cmds, "podman pull 'postgres:16.1'") {
		t.Fatalf("third-party image should be pulled:\n%s", cmds)
	}
	if !strings.Contains(cmds, "systemctl --user daemon-reload") {
		t.Fatalf("daemon-reload should be called:\n%s", cmds)
	}
}

func TestDeployBuildHostDirect(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	buildExec := newMockExecutor("build-host")
	cfg := ProjectConfig{
		Name:   "report",
		Repo:   "git@github.com:acme/report.git",
		Branch: "master",
		Build: BuildConfig{
			Strategy: "build-host",
			Host:     "192.0.2.21",
			User:     "builder",
			RepoDir:  "/home/builder/report/repo",
			Delivery: "direct",
		},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/report/server:1.44",
				Dockerfile: "backend/server/Dockerfile",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/report/repo",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			local:     newMockExecutor("local"),
			targets:   map[string]*mockExecutor{"main": targetExec},
			buildHost: map[string]*mockExecutor{"192.0.2.21": buildExec},
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", []string{"server"}, false); err != nil {
		t.Fatalf("Deploy build-host direct failed: %v", err)
	}
	cmdsBuild := strings.Join(buildExec.commands, "\n")
	cmdsTarget := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmdsBuild, "podman save 'ghcr.io/acme/report/server:1.44'") {
		t.Fatalf("build host should save image archive:\n%s", cmdsBuild)
	}
	if !strings.Contains(cmdsTarget, "podman load -i '/tmp/qqd-report-server.tar'") {
		t.Fatalf("target should load image archive:\n%s", cmdsTarget)
	}
	if len(buildExec.copyFromCalls) == 0 || len(targetExec.copyToCalls) == 0 {
		t.Fatalf("direct delivery should copy through local machine")
	}
}

func TestDeploySkipsRestartWhenImageUnchanged(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["postgres:16.1"] = true
	// Pre-populate image ID — build will produce same ID (cache hit)
	targetExec.imageIDs["ghcr.io/acme/report/server:1.44"] = "sha256:build1"
	targetExec.buildCounter = 0 // next build will produce sha256:build1
	cfg := ProjectConfig{
		Name:   "report",
		Repo:   "git@github.com:acme/report.git",
		Branch: "master",
		Build:  BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/report/server:1.44",
				Dockerfile: "backend/server/Dockerfile",
			},
			"db": {
				Image: "postgres:16.1",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/report/repo",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "podman build") {
		t.Fatalf("should build when image doesn't exist:\n%s", cmds)
	}
	if strings.Contains(cmds, "podman pull") {
		t.Fatalf("should not pull when third-party image exists:\n%s", cmds)
	}
	// No restart when image unchanged
	if strings.Contains(cmds, "systemctl --user restart") {
		t.Fatalf("should not restart when image unchanged:\n%s", cmds)
	}
	if !strings.Contains(cmds, "systemctl --user start") {
		t.Fatalf("should still start units:\n%s", cmds)
	}
}

func TestDeployRestartsWhenImageChanged(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["postgres:16.1"] = true
	// Pre-populate with an OLD ID — build will produce a different one
	targetExec.imageIDs["ghcr.io/acme/report/server:1.44"] = "sha256:old-image-id"
	cfg := ProjectConfig{
		Name:   "report",
		Repo:   "git@github.com:acme/report.git",
		Branch: "master",
		Build:  BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/report/server:1.44",
				Dockerfile: "backend/server/Dockerfile",
			},
			"db": {
				Image: "postgres:16.1",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/report/repo",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "systemctl --user restart 'report-server.service'") {
		t.Fatalf("should restart service when image changed:\n%s", cmds)
	}
}

func TestDeployRollingRestartWithReplicas(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	// Pre-populate with OLD ID
	targetExec.imageIDs["ghcr.io/acme/server:1.0"] = "sha256:old-id"
	// Set all replicas as healthy so rolling restart completes
	targetExec.healthStatus["proj-server-1"] = "healthy"
	targetExec.healthStatus["proj-server-2"] = "healthy"
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/server:1.0",
				Dockerfile: "Dockerfile",
				Replicas:   2,
				Health:     HealthConfig{Path: "/api/health", Port: 8080},
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/api/": "server:8080"}},
				}},
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy with replicas failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	// Should restart replicas individually
	if !strings.Contains(cmds, "systemctl --user restart 'proj-server-1.service'") {
		t.Fatalf("should restart replica 1:\n%s", cmds)
	}
	if !strings.Contains(cmds, "systemctl --user restart 'proj-server-2.service'") {
		t.Fatalf("should restart replica 2:\n%s", cmds)
	}
	// Should check health after each restart
	if !strings.Contains(cmds, "podman inspect --format '{{.State.Health.Status}}' 'proj-server-1'") {
		t.Fatalf("should check health of replica 1:\n%s", cmds)
	}
	// Should write traefik.yml and dynamic config
	if !strings.Contains(cmds, "~/.config/qqd/proj/traefik.yml") {
		t.Fatalf("should write traefik.yml:\n%s", cmds)
	}
	if !strings.Contains(cmds, "~/.config/qqd/proj/dynamic/routes.yml") {
		t.Fatalf("should write dynamic routes.yml:\n%s", cmds)
	}
}

func TestDeployLocalTarget(t *testing.T) {
	localExec := newMockExecutor("dev")
	cfg := ProjectConfig{
		Name:   "myapp",
		Repo:   "git@github.com:acme/myapp.git",
		Branch: "master",
		Build:  BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"app": {
				Image:      "myapp:1.0",
				Dockerfile: "Dockerfile",
			},
		},
		Targets: map[string]TargetConfig{
			"dev": {
				Name:    "dev",
				Host:    "local",
				RepoDir: "/tmp/myapp/repo",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"dev": localExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "dev", nil, false); err != nil {
		t.Fatalf("Deploy to local target failed: %v", err)
	}
	cmds := strings.Join(localExec.commands, "\n")
	if !strings.Contains(cmds, "podman build") {
		t.Fatalf("should build image locally:\n%s", cmds)
	}
	if !strings.Contains(cmds, "systemctl --user start") {
		t.Fatalf("should enable units locally:\n%s", cmds)
	}
}

func TestTargetOrder(t *testing.T) {
	cfg := ProjectConfig{
		Targets: map[string]TargetConfig{
			"b": {Name: "b"},
			"a": {Name: "a"},
		},
	}
	got := targetOrder(cfg, "")
	want := []string{"a", "b"}
	sort.Strings(got)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("targetOrder mismatch: %#v", got)
		}
	}
}

func TestDefaultExecFactoryLocalTarget(t *testing.T) {
	factory := DefaultExecFactory{}
	exec, err := factory.ForTarget(TargetConfig{Host: "local", RepoDir: "/tmp/test"})
	if err != nil {
		t.Fatalf("ForTarget local should not error: %v", err)
	}
	if _, ok := exec.(LocalExecutor); !ok {
		t.Fatalf("ForTarget with host=local should return LocalExecutor, got %T", exec)
	}
	exec, err = factory.ForBuildHost(BuildConfig{Host: "local"})
	if err != nil {
		t.Fatalf("ForBuildHost local should not error: %v", err)
	}
	if _, ok := exec.(LocalExecutor); !ok {
		t.Fatalf("ForBuildHost with host=local should return LocalExecutor, got %T", exec)
	}
}

func TestStatusReturnsErrorOnConnectivityFailure(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.failAll = true
	cfg := ProjectConfig{
		Name: "report",
		Services: map[string]ServiceConfig{
			"server": {Image: "ghcr.io/acme/report/server:1.44"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/report/repo",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	err := app.Status(context.Background(), cfg, "main")
	if err == nil {
		t.Fatalf("Status should fail when target is unreachable")
	}
	if !strings.Contains(err.Error(), "target main") {
		t.Fatalf("Status error should include target name, got: %v", err)
	}
}

func TestBuildImageCommandNoRunTag(t *testing.T) {
	svc := ServiceConfig{
		Image:      "ghcr.io/acme/server:1.0",
		Dockerfile: "Dockerfile",
	}
	cmd := buildImageCommand("/repo", svc, BuildConfig{})
	if !strings.Contains(cmd, "-t 'ghcr.io/acme/server:1.0'") {
		t.Fatalf("should tag with svc.Image:\n%s", cmd)
	}
	// Should only have one -t (no run tag)
	if strings.Count(cmd, "-t ") != 1 {
		t.Fatalf("should have exactly one -t tag:\n%s", cmd)
	}
}

func TestDeployRestartsOnConfigChange(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["postgres:16.1"] = true
	cfg := ProjectConfig{
		Name:   "report",
		Repo:   "git@github.com:acme/report.git",
		Branch: "master",
		Build:  BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/report/server:1.44",
				Dockerfile: "backend/server/Dockerfile",
			},
			"db": {
				Image: "postgres:16.1",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/report/repo",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	// First deploy — writes quadlet files to mock filesystem
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("First deploy failed: %v", err)
	}
	// Reset command history and build counter so image stays the same
	targetExec.commands = nil
	targetExec.buildCounter = 0

	// Second deploy with changed config (new env var) but same image
	cfg2 := cfg
	cfg2.Services = map[string]ServiceConfig{
		"server": {
			Image:      "ghcr.io/acme/report/server:1.44",
			Dockerfile: "backend/server/Dockerfile",
			Env:        map[string]string{"NEW_VAR": "new_value"},
		},
		"db": {
			Image: "postgres:16.1",
		},
	}
	if err := app.Deploy(context.Background(), cfg2, "main", nil, false); err != nil {
		t.Fatalf("Second deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	// Should restart server service due to quadlet config change
	if !strings.Contains(cmds, "systemctl --user restart 'report-server.service'") {
		t.Fatalf("should restart service when quadlet config changed:\n%s", cmds)
	}
}

func TestDeployRemovesStaleQuadletsReplicatedToNonReplicated(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.healthStatus["proj-server-1"] = "healthy"
	targetExec.healthStatus["proj-server-2"] = "healthy"
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/server:1.0",
				Dockerfile: "Dockerfile",
				Replicas:   2,
				Health:     HealthConfig{Path: "/api/health", Port: 8080},
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/api/": "server:8080"}},
				}},
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	// First deploy: replicated (2 replicas)
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("First deploy failed: %v", err)
	}
	// Verify replica files exist in mock filesystem
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-server-1.container"]; !ok {
		t.Fatal("replica 1 container file should exist after first deploy")
	}
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-server-2.container"]; !ok {
		t.Fatal("replica 2 container file should exist after first deploy")
	}

	// Second deploy: switch to non-replicated (exposed → zero-downtime slot)
	targetExec.commands = nil
	targetExec.buildCounter = 0
	cfg2 := cfg
	cfg2.Services = map[string]ServiceConfig{
		"server": {
			Image:      "ghcr.io/acme/server:1.0",
			Dockerfile: "Dockerfile",
		},
	}
	cfg2.Targets = map[string]TargetConfig{
		"main": {
			Name:    "main",
			Host:    "192.0.2.20",
			User:    "centos",
			RepoDir: "/home/centos/proj/repo",
			Expose: ExposeConfig{Entries: []ExposeEntry{
				{HostPort: 80, Routes: map[string]string{"/api/": "server:8080"}},
			}},
		},
	}
	if err := app.Deploy(context.Background(), cfg2, "main", nil, false); err != nil {
		t.Fatalf("Second deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	// Should stop and remove stale replica units
	if !strings.Contains(cmds, "systemctl --user stop 'proj-server-1.service' 2>/dev/null || true") {
		t.Fatalf("should stop stale replica 1 unit:\n%s", cmds)
	}
	if !strings.Contains(cmds, "systemctl --user stop 'proj-server-2.service' 2>/dev/null || true") {
		t.Fatalf("should stop stale replica 2 unit:\n%s", cmds)
	}
	// With no active slot, the first slot-eligible deploy writes a standard quadlet.
	// Slot files are created on subsequent image changes, not on the initial transition.
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-server.container"]; !ok {
		t.Fatal("standard container file should exist after second deploy (no active slot yet)")
	}
}

func TestDeployRemovesStaleQuadletsNonReplicatedToReplicated(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/server:1.0",
				Dockerfile: "Dockerfile",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
				}},
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	// First deploy: non-replicated + exposed → zero-downtime slot (creates slot container)
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("First deploy failed: %v", err)
	}
	// Slot deploy creates a hash-based slotted container file, not the standard one
	serverHash := slotHash("ghcr.io/acme/server:1.0")
	slotFile := fmt.Sprintf("~/.config/containers/systemd/proj-server-%s.container", serverHash)
	if _, ok := targetExec.files[slotFile]; !ok {
		t.Fatalf("slot container file %s should exist after first deploy", slotFile)
	}

	// Second deploy: switch to replicated (2 replicas)
	targetExec.commands = nil
	targetExec.buildCounter = 0
	targetExec.healthStatus["proj-server-1"] = "healthy"
	targetExec.healthStatus["proj-server-2"] = "healthy"
	cfg2 := cfg
	cfg2.Services = map[string]ServiceConfig{
		"server": {
			Image:      "ghcr.io/acme/server:1.0",
			Dockerfile: "Dockerfile",
			Replicas:   2,
			Health:     HealthConfig{Path: "/api/health", Port: 8080},
		},
	}
	cfg2.Targets = map[string]TargetConfig{
		"main": {
			Name:    "main",
			Host:    "192.0.2.20",
			User:    "centos",
			RepoDir: "/home/centos/proj/repo",
			Expose: ExposeConfig{Entries: []ExposeEntry{
				{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
			}},
		},
	}
	if err := app.Deploy(context.Background(), cfg2, "main", nil, false); err != nil {
		t.Fatalf("Second deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	// Should stop and remove the old hash-based slot unit
	expectedStop := fmt.Sprintf("systemctl --user stop 'proj-server-%s.service' 2>/dev/null || true", serverHash)
	if !strings.Contains(cmds, expectedStop) {
		t.Fatalf("should stop stale slot unit:\n%s", cmds)
	}
	// Replica files should exist
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-server-1.container"]; !ok {
		t.Fatal("replica 1 container file should exist after second deploy")
	}
}

func TestDeployRemovesStaleQuadletsOnReplicaDecrease(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.healthStatus["proj-server-1"] = "healthy"
	targetExec.healthStatus["proj-server-2"] = "healthy"
	targetExec.healthStatus["proj-server-3"] = "healthy"
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/server:1.0",
				Dockerfile: "Dockerfile",
				Replicas:   3,
				Health:     HealthConfig{Path: "/api/health", Port: 8080},
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/api/": "server:8080"}},
				}},
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	// First deploy: 3 replicas
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("First deploy failed: %v", err)
	}

	// Second deploy: reduce to 2 replicas
	targetExec.commands = nil
	targetExec.buildCounter = 0
	cfg2 := cfg
	cfg2.Services = map[string]ServiceConfig{
		"server": {
			Image:      "ghcr.io/acme/server:1.0",
			Dockerfile: "Dockerfile",
			Replicas:   2,
			Health:     HealthConfig{Path: "/api/health", Port: 8080},
		},
	}
	cfg2.Targets = map[string]TargetConfig{
		"main": {
			Name:    "main",
			Host:    "192.0.2.20",
			User:    "centos",
			RepoDir: "/home/centos/proj/repo",
			Expose: ExposeConfig{Entries: []ExposeEntry{
				{HostPort: 80, Routes: map[string]string{"/api/": "server:8080"}},
			}},
		},
	}
	if err := app.Deploy(context.Background(), cfg2, "main", nil, false); err != nil {
		t.Fatalf("Second deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	// Should stop and remove only the excess replica
	if !strings.Contains(cmds, "systemctl --user stop 'proj-server-3.service' 2>/dev/null || true") {
		t.Fatalf("should stop stale replica 3 unit:\n%s", cmds)
	}
	// Should NOT remove replicas 1 and 2
	if strings.Contains(cmds, "rm -f ~/.config/containers/systemd/'proj-server-1.container'") {
		t.Fatalf("should NOT remove replica 1:\n%s", cmds)
	}
	if strings.Contains(cmds, "rm -f ~/.config/containers/systemd/'proj-server-2.container'") {
		t.Fatalf("should NOT remove replica 2:\n%s", cmds)
	}
	// Replica 3 file should be gone from filesystem
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-server-3.container"]; ok {
		t.Fatal("replica 3 container file should be removed after second deploy")
	}
	// Replicas 1 and 2 should still exist
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-server-1.container"]; !ok {
		t.Fatal("replica 1 container file should still exist")
	}
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-server-2.container"]; !ok {
		t.Fatal("replica 2 container file should still exist")
	}
}

func TestAllContainerUnitsWithReplicas(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {
			Image:    "ghcr.io/acme/server:1.0",
			Replicas: 2,
		},
		"db": {
			Image: "postgres:16.1",
		},
	}
	units := allContainerUnits("proj", services)
	want := []string{
		"proj-db.service",
		"proj-server-1.service",
		"proj-server-2.service",
	}
	if len(units) != len(want) {
		t.Fatalf("expected %d units, got %d: %v", len(want), len(units), units)
	}
	for i, w := range want {
		if units[i] != w {
			t.Fatalf("unit[%d] = %q, want %q", i, units[i], w)
		}
	}
}

func TestDeployWritesTraefikConfig(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "server:1.0",
				Dockerfile: "Dockerfile",
			},
			"db": {
				Image: "postgres:16.1",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "local",
				RepoDir: "/tmp/proj",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/api/": "server:8080"}},
					{HostPort: 5432, Target: "db:5432"},
				}},
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	// Verify traefik.yml was written
	traefikYml, ok := targetExec.files["~/.config/qqd/proj/traefik.yml"]
	if !ok {
		t.Fatal("traefik.yml should be written")
	}
	if !strings.Contains(traefikYml, "web-80:") {
		t.Fatalf("traefik.yml should contain web-80 entrypoint:\n%s", traefikYml)
	}
	if !strings.Contains(traefikYml, "tcp-5432:") {
		t.Fatalf("traefik.yml should contain tcp-5432 entrypoint:\n%s", traefikYml)
	}
	// Verify dynamic routes.yml was written
	routesYml, ok := targetExec.files["~/.config/qqd/proj/dynamic/routes.yml"]
	if !ok {
		t.Fatal("dynamic/routes.yml should be written")
	}
	if !strings.Contains(routesYml, "http:") {
		t.Fatalf("routes.yml should contain http section:\n%s", routesYml)
	}
	if !strings.Contains(routesYml, "tcp:") {
		t.Fatalf("routes.yml should contain tcp section:\n%s", routesYml)
	}
}

func TestDeployUploadSkipsWhenAllImagesExist(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	localExec := newMockExecutor("local")
	// Pre-populate all images as existing
	targetExec.existingImage["ghcr.io/acme/server:1.0"] = true
	targetExec.existingImage["postgres:16.1"] = true
	cfg := ProjectConfig{
		Name:         "proj",
		Repo:         "git@github.com:acme/proj.git",
		Branch:       "master",
		Sync:         "upload",
		InvocationWD: "/local/proj",
		Build:        BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/server:1.0",
				Dockerfile: "Dockerfile",
			},
			"db": {
				Image: "postgres:16.1",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   localExec,
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	// rsync runs on the local executor — verify it was NOT called
	localCmds := strings.Join(localExec.commands, "\n")
	if strings.Contains(localCmds, "rsync") {
		t.Fatalf("should NOT rsync when all images exist:\n%s", localCmds)
	}
}

func TestDeployFullRemovesDeletedService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/server:1.0",
				Dockerfile: "Dockerfile",
			},
			"worker": {
				Image:      "ghcr.io/acme/worker:1.0",
				Dockerfile: "Dockerfile.worker",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	// First deploy: both services (full deploy, no service args)
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("First deploy failed: %v", err)
	}
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-server.container"]; !ok {
		t.Fatal("server container file should exist after first deploy")
	}
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-worker.container"]; !ok {
		t.Fatal("worker container file should exist after first deploy")
	}

	// Second deploy: remove worker from config (full deploy, no service args)
	targetExec.commands = nil
	targetExec.buildCounter = 0
	cfg2 := cfg
	cfg2.Services = map[string]ServiceConfig{
		"server": {
			Image:      "ghcr.io/acme/server:1.0",
			Dockerfile: "Dockerfile",
		},
	}
	if err := app.Deploy(context.Background(), cfg2, "main", nil, false); err != nil {
		t.Fatalf("Second deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	// Worker quadlet should be stopped and removed
	if !strings.Contains(cmds, "systemctl --user stop 'proj-worker.service' 2>/dev/null || true") {
		t.Fatalf("should stop removed worker service:\n%s", cmds)
	}
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-worker.container"]; ok {
		t.Fatal("worker container file should be removed after second deploy")
	}
	// Server should still exist
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-server.container"]; !ok {
		t.Fatal("server container file should still exist after second deploy")
	}
}

func TestDeployPartialLeavesDeletedService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/server:1.0",
				Dockerfile: "Dockerfile",
			},
			"worker": {
				Image:      "ghcr.io/acme/worker:1.0",
				Dockerfile: "Dockerfile.worker",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	// First deploy: both services
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("First deploy failed: %v", err)
	}

	// Second deploy: partial deploy targeting only server ("qqd deploy server")
	targetExec.commands = nil
	targetExec.buildCounter = 0
	if err := app.Deploy(context.Background(), cfg, "main", []string{"server"}, false); err != nil {
		t.Fatalf("Partial deploy failed: %v", err)
	}
	// Worker quadlet should NOT be touched during partial deploy
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-worker.container"]; !ok {
		t.Fatal("worker container file should still exist after partial deploy")
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if strings.Contains(cmds, "systemctl --user stop 'proj-worker.service'") {
		t.Fatalf("should NOT stop worker during partial deploy:\n%s", cmds)
	}
}

func TestDeployUploadOnlyContextDirs(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	localExec := newMockExecutor("local")
	cfg := ProjectConfig{
		Name:         "proj",
		Repo:         "git@github.com:acme/proj.git",
		Branch:       "master",
		Sync:         "upload",
		InvocationWD: "/local/proj",
		Build:        BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/server:1.0",
				Dockerfile: "backend/server/Dockerfile",
				Context:    "backend/server",
			},
			"worker": {
				Image:      "ghcr.io/acme/worker:1.0",
				Dockerfile: "backend/worker/Dockerfile",
				Context:    "backend/worker",
			},
			"db": {
				Image: "postgres:16.1",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   localExec,
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	localCmds := strings.Join(localExec.commands, "\n")
	// Should rsync each context dir individually, not the whole project
	if !strings.Contains(localCmds, "/local/proj/backend/server/") {
		t.Fatalf("should rsync server context dir:\n%s", localCmds)
	}
	if !strings.Contains(localCmds, "/local/proj/backend/worker/") {
		t.Fatalf("should rsync worker context dir:\n%s", localCmds)
	}
	if !strings.Contains(localCmds, "centos@192.0.2.20:/home/centos/proj/repo/backend/server/") {
		t.Fatalf("should target server context on remote:\n%s", localCmds)
	}
	if !strings.Contains(localCmds, "centos@192.0.2.20:/home/centos/proj/repo/backend/worker/") {
		t.Fatalf("should target worker context on remote:\n%s", localCmds)
	}
	// Should NOT upload the full project root
	rsyncCount := strings.Count(localCmds, "rsync")
	if rsyncCount != 2 {
		t.Fatalf("expected 2 rsync calls (one per context), got %d:\n%s", rsyncCount, localCmds)
	}
	// Should clean up only the context dirs, not the whole repo
	targetCmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(targetCmds, "rm -rf '/home/centos/proj/repo/backend/server'") {
		t.Fatalf("should clean up server context dir:\n%s", targetCmds)
	}
	if !strings.Contains(targetCmds, "rm -rf '/home/centos/proj/repo/backend/worker'") {
		t.Fatalf("should clean up worker context dir:\n%s", targetCmds)
	}
	if strings.Contains(targetCmds, "rm -rf '/home/centos/proj/repo'\n") {
		t.Fatalf("should NOT clean up the entire repo dir:\n%s", targetCmds)
	}
}

func TestDeployUploadFallsBackWithoutContext(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	localExec := newMockExecutor("local")
	cfg := ProjectConfig{
		Name:         "proj",
		Repo:         "git@github.com:acme/proj.git",
		Branch:       "master",
		Sync:         "upload",
		InvocationWD: "/local/proj",
		Build:        BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/server:1.0",
				Dockerfile: "backend/server/Dockerfile",
				Context:    "backend/server",
			},
			"worker": {
				Image:      "ghcr.io/acme/worker:1.0",
				Dockerfile: "Dockerfile.worker",
				// No context — requires full upload
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   localExec,
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	localCmds := strings.Join(localExec.commands, "\n")
	// Should upload the full project root since worker has no context
	if !strings.Contains(localCmds, "/local/proj/") {
		t.Fatalf("should rsync full project dir:\n%s", localCmds)
	}
	if !strings.Contains(localCmds, "centos@192.0.2.20:/home/centos/proj/repo/") {
		t.Fatalf("should target full repo dir on remote:\n%s", localCmds)
	}
	rsyncCount := strings.Count(localCmds, "rsync")
	if rsyncCount != 1 {
		t.Fatalf("expected 1 rsync call (full upload), got %d:\n%s", rsyncCount, localCmds)
	}
}

func TestDeployUploadAndCleanup(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	localExec := newMockExecutor("local")
	// server image does NOT exist — needs build, so upload is required
	cfg := ProjectConfig{
		Name:         "proj",
		Repo:         "git@github.com:acme/proj.git",
		Branch:       "master",
		Sync:         "upload",
		InvocationWD: "/local/proj",
		Build:        BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/server:1.0",
				Dockerfile: "Dockerfile",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   localExec,
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	// rsync should have been called on the local executor
	localCmds := strings.Join(localExec.commands, "\n")
	if !strings.Contains(localCmds, "rsync") {
		t.Fatalf("should rsync when image is missing:\n%s", localCmds)
	}
	// Cleanup: rm -rf should have been called on the target executor
	targetCmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(targetCmds, "rm -rf '/home/centos/proj/repo'") {
		t.Fatalf("should clean up uploaded source after build:\n%s", targetCmds)
	}
}

func TestDeployUploadUsesStrictHostKeyCheckingByDefault(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	localExec := newMockExecutor("local")
	cfg := ProjectConfig{
		Name:         "proj",
		Repo:         "git@github.com:acme/proj.git",
		Branch:       "master",
		Sync:         "upload",
		InvocationWD: "/local/proj",
		Build:        BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/server:1.0",
				Dockerfile: "Dockerfile",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   localExec,
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	localCmds := strings.Join(localExec.commands, "\n")
	if !strings.Contains(localCmds, "StrictHostKeyChecking=yes") {
		t.Fatalf("upload mode should keep strict host key checking by default:\n%s", localCmds)
	}
	if strings.Contains(localCmds, "StrictHostKeyChecking=no") {
		t.Fatalf("upload mode should not disable host key checking by default:\n%s", localCmds)
	}
}

func TestSlotDeployExposedNonReplicated(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	// Pre-populate with OLD image ID so deploy detects a change
	targetExec.imageIDs["ghcr.io/acme/server:1.0"] = "sha256:old-id"
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/server:1.0",
				Dockerfile: "Dockerfile",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
				}},
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")

	// Hash-based slot quadlet should be written
	expectedHash := slotHash("ghcr.io/acme/server:1.0")
	slotFile := fmt.Sprintf("~/.config/containers/systemd/proj-server-%s.container", expectedHash)
	if _, ok := targetExec.files[slotFile]; !ok {
		t.Fatalf("slot container file %s should exist", slotFile)
	}
	slotContent := targetExec.files[slotFile]
	expectedCName := fmt.Sprintf("ContainerName=proj-server-%s", expectedHash)
	if !strings.Contains(slotContent, expectedCName) {
		t.Fatalf("slot should have correct container name:\n%s", slotContent)
	}
	if !strings.Contains(slotContent, "Network=proj.network:alias=proj-server") {
		t.Fatalf("slot should have network alias for DNS:\n%s", slotContent)
	}

	// Standard (non-slotted) container should NOT exist
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-server.container"]; ok {
		t.Fatal("standard container file should NOT exist for slot-deployed service")
	}

	// Slot unit should have been started
	expectedUnit := fmt.Sprintf("systemctl --user start 'proj-server-%s.service'", expectedHash)
	if !strings.Contains(cmds, expectedUnit) {
		t.Fatalf("should start slot unit:\n%s", cmds)
	}

	// Traefik dynamic config should reference the slot container name
	routesYml, ok := targetExec.files["~/.config/qqd/proj/dynamic/routes.yml"]
	if !ok {
		t.Fatal("routes.yml should be written")
	}
	expectedSlotName := fmt.Sprintf("proj-server-%s", expectedHash)
	if !strings.Contains(routesYml, expectedSlotName) {
		t.Fatalf("routes.yml should reference slot name %s:\n%s", expectedSlotName, routesYml)
	}
}

func TestRollingRestartWithDrainExposedReplicated(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	// Pre-populate with OLD image ID
	targetExec.imageIDs["ghcr.io/acme/server:1.0"] = "sha256:old-id"
	targetExec.healthStatus["proj-server-1"] = "healthy"
	targetExec.healthStatus["proj-server-2"] = "healthy"
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/server:1.0",
				Dockerfile: "Dockerfile",
				Replicas:   2,
				Health:     HealthConfig{Path: "/api/health", Port: 8080},
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/api/": "server:8080"}},
				}},
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")

	// Both replicas should be restarted individually
	if !strings.Contains(cmds, "systemctl --user restart 'proj-server-1.service'") {
		t.Fatalf("should restart replica 1:\n%s", cmds)
	}
	if !strings.Contains(cmds, "systemctl --user restart 'proj-server-2.service'") {
		t.Fatalf("should restart replica 2:\n%s", cmds)
	}

	// routes.yml should be written multiple times (drain exclusion + inclusion)
	// Count the number of routes.yml writes
	routesWrites := strings.Count(cmds, "~/.config/qqd/proj/dynamic/routes.yml")
	// At least: initial write + 2 excludes + 2 includes = 5
	if routesWrites < 5 {
		t.Fatalf("routes.yml should be written at least 5 times (initial + 2 exclude + 2 include), got %d writes:\n%s", routesWrites, cmds)
	}

	// Health checks should be performed after each restart
	if !strings.Contains(cmds, "podman inspect --format '{{.State.Health.Status}}' 'proj-server-1'") {
		t.Fatalf("should check health of replica 1:\n%s", cmds)
	}
	if !strings.Contains(cmds, "podman inspect --format '{{.State.Health.Status}}' 'proj-server-2'") {
		t.Fatalf("should check health of replica 2:\n%s", cmds)
	}
}

func TestDirectRestartNonExposed(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	// Pre-populate with OLD image ID so deploy detects a change
	targetExec.imageIDs["ghcr.io/acme/server:1.0"] = "sha256:old-id"
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/server:1.0",
				Dockerfile: "Dockerfile",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
				// No Expose — service is not exposed
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")

	// Should use simple systemctl restart (not slot-based deploy)
	if !strings.Contains(cmds, "systemctl --user restart 'proj-server.service'") {
		t.Fatalf("non-exposed service should restart in place:\n%s", cmds)
	}

	// Standard (non-slotted) container should exist
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-server.container"]; !ok {
		t.Fatal("standard container file should exist for non-exposed service")
	}

	// No slot files should exist
	slotFile := fmt.Sprintf("~/.config/containers/systemd/proj-server-%s.container", slotHash("ghcr.io/acme/server:1.0"))
	if _, ok := targetExec.files[slotFile]; ok {
		t.Fatal("slot file should NOT exist for non-exposed service")
	}

	// No routes.yml should be written (no expose config)
	if _, ok := targetExec.files["~/.config/qqd/proj/dynamic/routes.yml"]; ok {
		t.Fatal("routes.yml should NOT be written when no expose config")
	}
}

// TestDeployDependsOnSlottedService verifies that when service A depends on
// service B and B has an active slot, A's quadlet references the
// slot unit (e.g. After=proj-server-a1b2c3d4.service) instead of the non-existent
// standard unit (proj-server.service).
func TestDeployDependsOnSlottedService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	serverSvc := ServiceConfig{Image: "ghcr.io/acme/server:1.0"}
	// server already deployed as hash slot — simulate existing slot quadlet
	serverHash := slotHash("ghcr.io/acme/server:1.0")
	slotFile := fmt.Sprintf("~/.config/containers/systemd/proj-server-%s.container", serverHash)
	targetExec.files[slotFile] = renderExpectedSlotContent("proj", "server", serverHash, serverSvc, map[string]string{"server": serverHash}, PodmanRuntime{})
	// Pre-populate images so ensureImages doesn't detect changes
	targetExec.existingImage["ghcr.io/acme/server:1.0"] = true
	targetExec.existingImage["ghcr.io/acme/mcp:1.0"] = true
	// mcp has no changes (same image), but depends on server
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": serverSvc,
			"mcp":    {Image: "ghcr.io/acme/mcp:1.0", DependsOn: []string{"server"}},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
				}},
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	// mcp quadlet should reference the hash slot unit, NOT the standard unit
	mcpQuadlet, ok := targetExec.files["~/.config/containers/systemd/proj-mcp.container"]
	if !ok {
		t.Fatal("mcp container file should exist")
	}
	expectedAfter := fmt.Sprintf("After=proj-server-%s.service", serverHash)
	if !strings.Contains(mcpQuadlet, expectedAfter) {
		t.Fatalf("mcp quadlet should depend on slot unit (%s):\n%s", expectedAfter, mcpQuadlet)
	}
	expectedRequires := fmt.Sprintf("Requires=proj-server-%s.service", serverHash)
	if !strings.Contains(mcpQuadlet, expectedRequires) {
		t.Fatalf("mcp quadlet should require slot unit (%s):\n%s", expectedRequires, mcpQuadlet)
	}
	// Should NOT reference the standard unit
	if strings.Contains(mcpQuadlet, "After=proj-server.service") {
		t.Fatalf("mcp quadlet should NOT reference standard unit:\n%s", mcpQuadlet)
	}
}

// TestSlotDeployRewritesSlottedDependentQuadlet verifies the fix for the
// cascade-stop bug: when service A is slot-deployed and a dependent service B
// is itself slotted, A's slotDeploy must rewrite B's *slot* quadlet file (not
// the non-existent standard proj-B.container) so systemd doesn't cascade the
// stop of A's old slot into B when the old unit is removed.
func TestSlotDeployRewritesSlottedDependentQuadlet(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	oldServerImage := "ghcr.io/acme/server:1.0"
	newServerImage := "ghcr.io/acme/server:2.0"
	mcpImage := "ghcr.io/acme/mcp:1.0"
	oldServerHash := slotHash(oldServerImage)
	newServerHash := slotHash(newServerImage)
	mcpHash := slotHash(mcpImage)

	serverSlotFile := fmt.Sprintf("~/.config/containers/systemd/proj-server-%s.container", oldServerHash)
	mcpSlotFile := fmt.Sprintf("~/.config/containers/systemd/proj-mcp-%s.container", mcpHash)

	// Seed the existing slot files: old server slot and mcp slot pointing at it.
	oldServerSvc := ServiceConfig{Image: oldServerImage, Dockerfile: "Dockerfile"}
	mcpSvc := ServiceConfig{Image: mcpImage, DependsOn: []string{"server"}}
	existingSlots := map[string]string{"server": oldServerHash, "mcp": mcpHash}
	targetExec.files[serverSlotFile] = renderExpectedSlotContent("proj", "server", oldServerHash, oldServerSvc, existingSlots, PodmanRuntime{})
	targetExec.files[mcpSlotFile] = renderExpectedSlotContent("proj", "mcp", mcpHash, mcpSvc, existingSlots, PodmanRuntime{})

	// Server image is about to change; mcp image is unchanged.
	targetExec.existingImage[oldServerImage] = true
	targetExec.existingImage[mcpImage] = true
	targetExec.imageIDs[oldServerImage] = "sha256:old-server"
	targetExec.imageIDs[mcpImage] = "sha256:mcp"

	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {Image: newServerImage, Dockerfile: "Dockerfile"},
			"mcp":    mcpSvc,
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{
						"/mcp": "mcp:8989",
						"/":    "server:8080",
					}},
				}},
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	// The new server slot must exist and the old must be gone.
	newServerSlotFile := fmt.Sprintf("~/.config/containers/systemd/proj-server-%s.container", newServerHash)
	if _, ok := targetExec.files[newServerSlotFile]; !ok {
		t.Fatalf("new server slot %s not written; files: %v", newServerSlotFile, keysOf(targetExec.files))
	}
	if _, ok := targetExec.files[serverSlotFile]; ok {
		t.Fatalf("old server slot %s should be removed", serverSlotFile)
	}

	// mcp's slot file must still exist and now reference the NEW server slot.
	updatedMcp, ok := targetExec.files[mcpSlotFile]
	if !ok {
		t.Fatalf("mcp slot file %s should still exist; files: %v", mcpSlotFile, keysOf(targetExec.files))
	}
	expectedNewReq := fmt.Sprintf("Requires=proj-server-%s.service", newServerHash)
	if !strings.Contains(updatedMcp, expectedNewReq) {
		t.Fatalf("mcp slot quadlet should Require new server slot (%s):\n%s", expectedNewReq, updatedMcp)
	}
	oldReq := fmt.Sprintf("proj-server-%s.service", oldServerHash)
	if strings.Contains(updatedMcp, oldReq) {
		t.Fatalf("mcp slot quadlet should NOT still reference old server slot (%s):\n%s", oldReq, updatedMcp)
	}

	// Standard proj-mcp.container must not have been written as a side-effect.
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-mcp.container"]; ok {
		t.Fatal("standard mcp container file should not exist for slotted service")
	}
}

func TestSlotHash(t *testing.T) {
	h := slotHash("ghcr.io/acme/server:1.0")
	if len(h) != 8 {
		t.Fatalf("expected 8 hex chars, got %d: %s", len(h), h)
	}
	// Deterministic: same input → same output
	if h2 := slotHash("ghcr.io/acme/server:1.0"); h != h2 {
		t.Fatalf("expected deterministic hash, got %s and %s", h, h2)
	}
	// Different inputs → different hashes
	if h3 := slotHash("ghcr.io/acme/server:2.0"); h == h3 {
		t.Fatalf("different images should produce different hashes")
	}
}

func TestDetectActiveSlotHash(t *testing.T) {
	exec := newMockExecutor("target")
	hash := slotHash("ghcr.io/acme/server:1.0")
	exec.files[fmt.Sprintf("~/.config/containers/systemd/proj-server-%s.container", hash)] = "[Container]\n"
	ctx := context.Background()
	slot := detectActiveSlot(ctx, exec, "proj", "server", "~/.config/containers/systemd", ".container", "systemctl --user")
	if slot != hash {
		t.Fatalf("expected slot %q, got %q", hash, slot)
	}
}

func TestDetectActiveSlotSkipsReplicas(t *testing.T) {
	exec := newMockExecutor("target")
	exec.files["~/.config/containers/systemd/proj-server-1.container"] = "[Container]\n"
	exec.files["~/.config/containers/systemd/proj-server-2.container"] = "[Container]\n"
	ctx := context.Background()
	slot := detectActiveSlot(ctx, exec, "proj", "server", "~/.config/containers/systemd", ".container", "systemctl --user")
	if slot != "" {
		t.Fatalf("expected empty slot (replicas only), got %q", slot)
	}
}

func TestIsSlotFile(t *testing.T) {
	bgSvcs := map[string]bool{"server": true}

	if !isSlotFile("proj", "proj-server-a1b2c3d4.container", ".container", bgSvcs) {
		t.Fatal("hash-based slot should be detected")
	}
	if isSlotFile("proj", "proj-server-1.container", ".container", bgSvcs) {
		t.Fatal("replica should NOT be detected as slot")
	}
	if isSlotFile("proj", "proj-server.container", ".container", bgSvcs) {
		t.Fatal("standard name should NOT be detected as slot")
	}
	if isSlotFile("proj", "proj-other-a1b2c3d4.container", ".container", bgSvcs) {
		t.Fatal("different service should NOT be detected as slot")
	}
	if isSlotFile("proj", "proj-server-a1b2c3d4.container", ".container", nil) {
		t.Fatal("should return false with no slotted services")
	}
}

// TestIsValidSlotHash tests the slot hash format validator.
func TestIsValidSlotHash(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"a1b2c3d4", true},    // valid 8 hex chars
		{"00000000", true},    // all zeros
		{"abcdef01", true},    // all lowercase hex
		{"12345678", true},    // all digits
		{"a1b2c3", false},     // too short (6 chars)
		{"a1b2c3d4e5", false}, // too long (10 chars)
		{"A1B2C3D4", false},   // uppercase hex not valid
		{"a1b2g3d4", false},   // 'g' is not hex
		{"", false},           // empty
		{"api-a1b2", false},   // contains dash
	}
	for _, tt := range tests {
		if got := isValidSlotHash(tt.input); got != tt.valid {
			t.Errorf("isValidSlotHash(%q) = %v, want %v", tt.input, got, tt.valid)
		}
	}
}

// TestIsSlotFileServiceNamePrefixCollision verifies that a file for service
// "web-api" is NOT incorrectly detected as a slot file for service "web".
func TestIsSlotFileServiceNamePrefixCollision(t *testing.T) {
	slottedSvcs := map[string]bool{"web": true}
	// "web-api" service with a slot hash should NOT match "web"
	hash := slotHash("some-image:1.0")
	filename := fmt.Sprintf("proj-web-api-%s.container", hash)
	if isSlotFile("proj", filename, ".container", slottedSvcs) {
		t.Fatal("proj-web-api-<hash>.container should NOT be detected as slot file for service 'web'")
	}

	webHash := slotHash("ghcr.io/acme/web:1.0")
	webFilename := fmt.Sprintf("proj-web-%s.container", webHash)
	if !isSlotFile("proj", webFilename, ".container", slottedSvcs) {
		t.Fatal("proj-web-<hash>.container should be detected as slot file for 'web'")
	}

	bothSlotted := map[string]bool{"web": true, "web-api": true}
	if !isSlotFile("proj", filename, ".container", bothSlotted) {
		t.Fatal("proj-web-api-<hash>.container should be detected for 'web-api'")
	}
}

// TestDetectActiveSlotPrefixCollision verifies detectActiveSlot doesn't match
// a different service whose name has the same prefix.
func TestDetectActiveSlotPrefixCollision(t *testing.T) {
	exec := newMockExecutor("target")
	apiHash := slotHash("ghcr.io/acme/web-api:1.0")
	// Only "web-api" has a slot file; "web" does not
	exec.files[fmt.Sprintf("~/.config/containers/systemd/proj-web-api-%s.container", apiHash)] = "[Container]\n"
	ctx := context.Background()

	// Detecting slot for "web" should return "" (no slot for web)
	slot := detectActiveSlot(ctx, exec, "proj", "web", "~/.config/containers/systemd", ".container", "systemctl --user")
	if slot != "" {
		t.Fatalf("detectActiveSlot for 'web' should return empty when only 'web-api' has slot, got %q", slot)
	}

	slot = detectActiveSlot(ctx, exec, "proj", "web-api", "~/.config/containers/systemd", ".container", "systemctl --user")
	if slot != apiHash {
		t.Fatalf("detectActiveSlot for 'web-api' = %q, want %q", slot, apiHash)
	}
}

// TestDetectActiveSlotFromListingPrefixCollision verifies the listing-based
// variant also handles prefix collisions correctly.
func TestDetectActiveSlotFromListingPrefixCollision(t *testing.T) {
	apiHash := slotHash("ghcr.io/acme/web-api:1.0")
	listing := fmt.Sprintf("proj-web-api-%s.container\nproj.network\n", apiHash)

	slot := detectActiveSlotFromListing("proj", "web", listing, ".container")
	if slot != "" {
		t.Fatalf("detectActiveSlotFromListing for 'web' should return empty, got %q", slot)
	}

	slot = detectActiveSlotFromListing("proj", "web-api", listing, ".container")
	if slot != apiHash {
		t.Fatalf("detectActiveSlotFromListing for 'web-api' = %q, want %q", slot, apiHash)
	}
}

func TestClean(t *testing.T) {
	customExec := &cleanMockExecutor{
		mockExecutor: newMockExecutor("target-main"),
		psOutput:     "",
		imagesOutput: "",
	}
	cfg := ProjectConfig{
		Name: "myapp",
		Services: map[string]ServiceConfig{
			"server": {Image: "myapp-server:1.45"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/myapp/repo",
			},
		},
	}
	app := &App{
		ExecFactory: cleanMockFactory{exec: customExec},
		Stdout:      io.Discard,
		DrainWait:   -1,
	}
	if err := app.Clean(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Clean failed: %v", err)
	}
	cmds := strings.Join(customExec.commands, "\n")

	// Should list stopped containers (ownership is decided per name, not by a
	// "<project>-" filter that also matches another project's containers)
	if !strings.Contains(cmds, "podman ps -a --filter status=exited --filter status=created --format '{{.Names}}'") {
		t.Fatalf("should list stopped containers:\n%s", cmds)
	}
	// Should list project images by repository reference
	if !strings.Contains(cmds, "podman images --filter reference='myapp-server' --format '{{.Repository}}:{{.Tag}}'") {
		t.Fatalf("should list project images by reference:\n%s", cmds)
	}
	// Should prune dangling images
	if !strings.Contains(cmds, "podman image prune -f") {
		t.Fatalf("should prune dangling images:\n%s", cmds)
	}
	// Should NOT run rm or rmi when nothing was listed
	if strings.Contains(cmds, "podman rm") {
		t.Fatalf("should not rm when no containers listed:\n%s", cmds)
	}
	if strings.Contains(cmds, "podman rmi") {
		t.Fatalf("should not rmi when no stale images listed:\n%s", cmds)
	}
}

func TestCleanRemovesListedContainersAndImages(t *testing.T) {
	// We need a custom executor that returns container/image lists
	customExec := &cleanMockExecutor{
		mockExecutor: newMockExecutor("target-main"),
		psOutput:     "myapp-server-1\nmyapp-server-2",
		imagesOutput: "myapp-server:1.44\nmyapp-server:1.45",
	}
	cfg := ProjectConfig{
		Name: "myapp",
		Services: map[string]ServiceConfig{
			"server": {Image: "myapp-server:1.45"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/myapp/repo",
			},
		},
	}
	app := &App{
		ExecFactory: cleanMockFactory{exec: customExec},
		Stdout:      io.Discard,
		DrainWait:   -1,
	}
	if err := app.Clean(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Clean failed: %v", err)
	}
	cmds := strings.Join(customExec.commands, "\n")

	// Should remove the listed containers
	if !strings.Contains(cmds, "podman rm -f") || !strings.Contains(cmds, "myapp-server-1") {
		t.Fatalf("should remove listed containers:\n%s", cmds)
	}
	// Should remove only stale images (1.44), NOT the active one (1.45)
	if !strings.Contains(cmds, "podman rmi") || !strings.Contains(cmds, "myapp-server:1.44") {
		t.Fatalf("should remove stale images:\n%s", cmds)
	}
	if strings.Contains(cmds, "'myapp-server:1.45'") && strings.Contains(cmds, "podman rmi") {
		// Find the rmi command and check it doesn't contain the active image
		for _, cmd := range customExec.commands {
			if strings.HasPrefix(cmd, "podman rmi") && strings.Contains(cmd, "myapp-server:1.45") {
				t.Fatalf("should NOT remove active image myapp-server:1.45:\n%s", cmds)
			}
		}
	}
}

func TestCleanSkipsAllWhenOnlyActiveImages(t *testing.T) {
	customExec := &cleanMockExecutor{
		mockExecutor: newMockExecutor("target-main"),
		psOutput:     "",
		imagesOutput: "myapp-server:1.45",
	}
	cfg := ProjectConfig{
		Name: "myapp",
		Services: map[string]ServiceConfig{
			"server": {Image: "myapp-server:1.45"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/myapp/repo",
			},
		},
	}
	app := &App{
		ExecFactory: cleanMockFactory{exec: customExec},
		Stdout:      io.Discard,
		DrainWait:   -1,
	}
	if err := app.Clean(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Clean failed: %v", err)
	}
	cmds := strings.Join(customExec.commands, "\n")
	if strings.Contains(cmds, "podman rmi") {
		t.Fatalf("should NOT run podman rmi when only active images exist:\n%s", cmds)
	}
}

// cleanMockExecutor extends mockExecutor to return specific output for podman ps/images.
type cleanMockExecutor struct {
	*mockExecutor
	psOutput     string
	imagesOutput string
}

func (m *cleanMockExecutor) Run(_ context.Context, cmd string) (string, error) {
	m.commands = append(m.commands, cmd)
	if strings.Contains(cmd, "podman ps") && strings.Contains(cmd, "--filter") {
		return m.psOutput + "\n", nil
	}
	if strings.Contains(cmd, "podman images") && strings.Contains(cmd, "--filter reference=") {
		return m.imagesOutput + "\n", nil
	}
	// Everything else (release listing/reads, rm, rmi, prune) behaves like the
	// shared mock so tests can seed release records via files.
	return m.mockExecutor.Run(context.Background(), cmd)
}

func (m *cleanMockExecutor) RunStream(_ context.Context, cmd string, _ io.Writer) error {
	_, err := m.Run(context.Background(), cmd)
	return err
}

func (m *cleanMockExecutor) CopyFrom(_ context.Context, remotePath, localPath string) error {
	return nil
}

func (m *cleanMockExecutor) CopyTo(_ context.Context, localPath, remotePath string) error {
	return nil
}

func (m *cleanMockExecutor) Close() error { return nil }
func (m *cleanMockExecutor) ID() string   { return m.mockExecutor.id }

type cleanMockFactory struct {
	exec *cleanMockExecutor
}

func (f cleanMockFactory) Local() Executor                            { return newMockExecutor("local") }
func (f cleanMockFactory) ForTarget(_ TargetConfig) (Executor, error) { return f.exec, nil }
func (f cleanMockFactory) ForBuildHost(_ BuildConfig) (Executor, error) {
	return newMockExecutor("build"), nil
}

// --- Edge-case tests for failed/inactive services and slot recovery ---

// TestDeployRestartsFailedSlottedService verifies that when a slotted service
// (HTTP-exposed, non-replicated) has a failed/inactive slot unit with the same
// image tag, deploy detects the failure and restarts the slot unit.
func TestDeployRestartsFailedSlottedService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	frontendSvc := ServiceConfig{
		Image:      "ghcr.io/acme/frontend:1.0",
		Dockerfile: "Dockerfile",
		Health:     HealthConfig{Path: "/", Port: 80},
	}
	frontendHash := slotHash("ghcr.io/acme/frontend:1.0")
	slotFile := fmt.Sprintf("~/.config/containers/systemd/proj-frontend-%s.container", frontendHash)
	// Use real rendered content so reconciliation only triggers from unit state, not content mismatch.
	targetExec.files[slotFile] = renderExpectedSlotContent("proj", "frontend", frontendHash, frontendSvc, map[string]string{"frontend": frontendHash}, PodmanRuntime{})
	targetExec.existingImage["ghcr.io/acme/frontend:1.0"] = true
	targetExec.existingImage["postgres:16.1"] = true
	// Mark the slot unit as failed — mock flips to active on restart
	slotUnit := fmt.Sprintf("proj-frontend-%s.service", frontendHash)
	targetExec.unitStates = map[string]string{
		slotUnit: "failed",
	}
	// Health check needs to report healthy after restart
	targetExec.healthStatus[fmt.Sprintf("proj-frontend-%s", frontendHash)] = "healthy"

	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"frontend": frontendSvc,
			"db":       {Image: "postgres:16.1"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "frontend:80"}},
				}},
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}

	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")

	// Must issue restart for the failed slot unit
	if !strings.Contains(cmds, fmt.Sprintf("systemctl --user restart '%s'", slotUnit)) {
		t.Fatalf("deploy should restart failed slot unit:\n%s", cmds)
	}
	// After restart, state should be active (mock flips on restart)
	if targetExec.unitStates[slotUnit] != "active" {
		t.Fatalf("slot unit should be active after restart, got: %s", targetExec.unitStates[slotUnit])
	}
}

// TestDeploySkipsActiveSlottedService verifies that deploy does NOT restart
// a slotted service that is already active with the same image.
func TestDeploySkipsActiveSlottedService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	frontendSvc := ServiceConfig{
		Image:      "ghcr.io/acme/frontend:1.0",
		Dockerfile: "Dockerfile",
		Health:     HealthConfig{Path: "/", Port: 80},
	}
	frontendHash := slotHash("ghcr.io/acme/frontend:1.0")
	slotFile := fmt.Sprintf("~/.config/containers/systemd/proj-frontend-%s.container", frontendHash)
	// Use real rendered content so content comparison passes — skip should work.
	targetExec.files[slotFile] = renderExpectedSlotContent("proj", "frontend", frontendHash, frontendSvc, map[string]string{"frontend": frontendHash}, PodmanRuntime{})
	targetExec.existingImage["ghcr.io/acme/frontend:1.0"] = true
	targetExec.existingImage["postgres:16.1"] = true
	// Slot is active — should NOT be restarted
	slotUnit := fmt.Sprintf("proj-frontend-%s.service", frontendHash)
	targetExec.unitStates = map[string]string{
		slotUnit: "active",
	}

	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"frontend": frontendSvc,
			"db":       {Image: "postgres:16.1"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "frontend:80"}},
				}},
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}

	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")

	// Should NOT restart active slot
	if strings.Contains(cmds, fmt.Sprintf("systemctl --user restart '%s'", slotUnit)) {
		t.Fatalf("deploy should NOT restart active slot unit:\n%s", cmds)
	}
}

func TestDeployReconcilesActiveSlottedServiceWhenDependencyRewriteChanges(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	serverHash := slotHash("ghcr.io/acme/server:1.0")
	mcpHash := slotHash("ghcr.io/acme/mcp:1.0")
	serverSlotFile := fmt.Sprintf("~/.config/containers/systemd/proj-server-%s.container", serverHash)
	mcpSlotFile := fmt.Sprintf("~/.config/containers/systemd/proj-mcp-%s.container", mcpHash)

	serverSvc := ServiceConfig{Image: "ghcr.io/acme/server:1.0"}
	mcpSvc := ServiceConfig{Image: "ghcr.io/acme/mcp:1.0", DependsOn: []string{"server"}}

	targetExec.files[serverSlotFile] = renderContainerWithSlot("proj", "server", serverHash, serverSvc)
	// Old content without slot dependency rewrite — deploy should reconcile this.
	targetExec.files[mcpSlotFile] = renderContainerWithSlot("proj", "mcp", mcpHash, mcpSvc)
	targetExec.existingImage["ghcr.io/acme/server:1.0"] = true
	targetExec.existingImage["ghcr.io/acme/mcp:1.0"] = true

	serverUnit := fmt.Sprintf("proj-server-%s.service", serverHash)
	mcpUnit := fmt.Sprintf("proj-mcp-%s.service", mcpHash)
	targetExec.unitStates = map[string]string{
		serverUnit: "active",
		mcpUnit:    "active",
	}

	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": serverSvc,
			"mcp":    mcpSvc,
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "server:8080", "/mcp/": "mcp:8081"}},
				}},
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}

	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, fmt.Sprintf("systemctl --user restart '%s'", mcpUnit)) {
		t.Fatalf("deploy should restart active slot when its rewritten content changes:\n%s", cmds)
	}
	want := renderExpectedSlotContent("proj", "mcp", mcpHash, mcpSvc, map[string]string{
		"server": serverHash,
		"mcp":    mcpHash,
	}, PodmanRuntime{})
	if strings.TrimSpace(targetExec.files[mcpSlotFile]) != strings.TrimSpace(want) {
		t.Fatalf("mcp slot file should be reconciled to rewritten content:\nwant:\n%s\ngot:\n%s", want, targetExec.files[mcpSlotFile])
	}
}

// TestDeployRestartsInactiveSlottedService verifies that "inactive" (not just
// "failed") slot units are also restarted.
func TestDeployRestartsInactiveSlottedService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	frontendSvc := ServiceConfig{Image: "ghcr.io/acme/frontend:1.0", Dockerfile: "Dockerfile", Health: HealthConfig{Path: "/", Port: 80}}
	hash := slotHash("ghcr.io/acme/frontend:1.0")
	slotFile := fmt.Sprintf("~/.config/containers/systemd/proj-frontend-%s.container", hash)
	// Use real rendered content so reconciliation triggers only from unit state.
	targetExec.files[slotFile] = renderExpectedSlotContent("proj", "frontend", hash, frontendSvc, map[string]string{"frontend": hash}, PodmanRuntime{})
	targetExec.existingImage["ghcr.io/acme/frontend:1.0"] = true
	slotUnit := fmt.Sprintf("proj-frontend-%s.service", hash)
	targetExec.unitStates = map[string]string{slotUnit: "inactive"}
	// Health check needs to report healthy after restart
	targetExec.healthStatus[fmt.Sprintf("proj-frontend-%s", hash)] = "healthy"

	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"frontend": frontendSvc,
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{{HostPort: 80, Routes: map[string]string{"/": "frontend:80"}}}}},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, fmt.Sprintf("systemctl --user restart '%s'", slotUnit)) {
		t.Fatalf("should restart inactive slot unit:\n%s", cmds)
	}
}

// TestDeployMultipleSlotsMixedStates verifies that with multiple slotted services,
// only the failed ones are restarted while active ones are left alone.
func TestDeployMultipleSlotsMixedStates(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	webSvc := ServiceConfig{Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"}
	apiSvc := ServiceConfig{Image: "ghcr.io/acme/api:2.0", Dockerfile: "Dockerfile"}
	webHash := slotHash("ghcr.io/acme/web:1.0")
	apiHash := slotHash("ghcr.io/acme/api:2.0")
	activeSlots := map[string]string{"web": webHash, "api": apiHash}
	webSlotFile := fmt.Sprintf("~/.config/containers/systemd/proj-web-%s.container", webHash)
	apiSlotFile := fmt.Sprintf("~/.config/containers/systemd/proj-api-%s.container", apiHash)
	targetExec.files[webSlotFile] = renderExpectedSlotContent("proj", "web", webHash, webSvc, activeSlots, PodmanRuntime{})
	targetExec.files[apiSlotFile] = renderExpectedSlotContent("proj", "api", apiHash, apiSvc, activeSlots, PodmanRuntime{})
	targetExec.existingImage["ghcr.io/acme/web:1.0"] = true
	targetExec.existingImage["ghcr.io/acme/api:2.0"] = true

	webUnit := fmt.Sprintf("proj-web-%s.service", webHash)
	apiUnit := fmt.Sprintf("proj-api-%s.service", apiHash)
	targetExec.unitStates = map[string]string{
		webUnit: "failed", // web is down
		apiUnit: "active", // api is fine
	}

	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": webSvc,
			"api": apiSvc,
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "web:80", "/api/": "api:8080"}},
				}}},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")

	// web (failed) should be restarted
	if !strings.Contains(cmds, fmt.Sprintf("systemctl --user restart '%s'", webUnit)) {
		t.Fatalf("should restart failed web slot:\n%s", cmds)
	}
	// api (active) should NOT be restarted
	if strings.Contains(cmds, fmt.Sprintf("systemctl --user restart '%s'", apiUnit)) {
		t.Fatalf("should NOT restart active api slot:\n%s", cmds)
	}
}

// TestDeployPartialWithFailedSlot verifies that a partial deploy targeting
// a specific service detects and restarts its failed slot.
func TestDeployPartialWithFailedSlot(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	webSvc := ServiceConfig{Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"}
	webHash := slotHash("ghcr.io/acme/web:1.0")
	slotFile := fmt.Sprintf("~/.config/containers/systemd/proj-web-%s.container", webHash)
	targetExec.files[slotFile] = renderExpectedSlotContent("proj", "web", webHash, webSvc, map[string]string{"web": webHash}, PodmanRuntime{})
	targetExec.existingImage["ghcr.io/acme/web:1.0"] = true
	targetExec.existingImage["postgres:16"] = true

	webUnit := fmt.Sprintf("proj-web-%s.service", webHash)
	targetExec.unitStates = map[string]string{webUnit: "failed"}

	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": webSvc,
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "web:80"}},
				}}},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	// Partial deploy: only "web"
	if err := app.Deploy(context.Background(), cfg, "main", []string{"web"}, false); err != nil {
		t.Fatalf("Partial deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, fmt.Sprintf("systemctl --user restart '%s'", webUnit)) {
		t.Fatalf("partial deploy should restart failed web slot:\n%s", cmds)
	}
}

// TestDeployAfterDestroyRecreatesEverything verifies that deploy works correctly
// on a clean target (no existing quadlet files or slots) after a destroy.
func TestDeployAfterDestroyRecreatesEverything(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	// No files, no images — simulates post-destroy state
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "web:80"}},
				}}},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy after destroy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")

	// Should build the image (no pre-existing image)
	if !strings.Contains(cmds, "podman build") {
		t.Fatalf("should build image on clean target:\n%s", cmds)
	}
	// Should pull db image
	if !strings.Contains(cmds, "podman pull 'postgres:16'") {
		t.Fatalf("should pull db image:\n%s", cmds)
	}
	// db should have standard quadlet
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-db.container"]; !ok {
		t.Fatal("should write db quadlet file")
	}
	// web is HTTP-exposed → first deploy builds it (changedImages), then slot-deploys.
	// Final state: slot file exists, standard file removed.
	webHash := slotHash("ghcr.io/acme/web:1.0")
	webSlotFile := fmt.Sprintf("~/.config/containers/systemd/proj-web-%s.container", webHash)
	if _, ok := targetExec.files[webSlotFile]; !ok {
		t.Fatalf("should write web slot quadlet, files: %v", keysOf(targetExec.files))
	}
	// Should start units
	if !strings.Contains(cmds, "systemctl --user start") {
		t.Fatalf("should start units:\n%s", cmds)
	}
}

// TestDeployImageChangePlusFailedSlot verifies that when a slotted service has
// both a failed unit AND an image change, deploy creates a new slot (not just restart).
func TestDeployImageChangePlusFailedSlot(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	oldHash := slotHash("ghcr.io/acme/web:1.0")
	slotFile := fmt.Sprintf("~/.config/containers/systemd/proj-web-%s.container", oldHash)
	targetExec.files[slotFile] = fmt.Sprintf("[Container]\nContainerName=proj-web-%s\nImage=ghcr.io/acme/web:1.0\n", oldHash)
	// Old image exists, new image doesn't → will be built
	targetExec.existingImage["ghcr.io/acme/web:1.0"] = true

	oldUnit := fmt.Sprintf("proj-web-%s.service", oldHash)
	targetExec.unitStates = map[string]string{oldUnit: "failed"}

	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:2.0", Dockerfile: "Dockerfile"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "web:80"}},
				}}},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	// New slot should exist (different hash since image changed)
	newHash := slotHash("ghcr.io/acme/web:2.0")
	newSlotFile := fmt.Sprintf("~/.config/containers/systemd/proj-web-%s.container", newHash)
	if _, ok := targetExec.files[newSlotFile]; !ok {
		t.Fatalf("should create new slot file for new image, existing files: %v", keysOf(targetExec.files))
	}
	// Old slot should be cleaned up
	if _, ok := targetExec.files[slotFile]; ok {
		t.Fatal("old slot file should be removed")
	}
	// Should NOT just restart (should do full slot deploy)
	cmds := strings.Join(targetExec.commands, "\n")
	if strings.Contains(cmds, fmt.Sprintf("systemctl --user restart '%s'", oldUnit)) {
		t.Fatalf("should do slot deploy (not restart) when image changes:\n%s", cmds)
	}
}

// TestRollbackSlottedService verifies that rollback restores the previous release
// and reinstalls quadlet files, restarting services.
func TestRollbackSlottedService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["ghcr.io/acme/web:1.0"] = true
	targetExec.existingImage["ghcr.io/acme/web:2.0"] = true
	hash := slotHash("ghcr.io/acme/web:2.0")
	slotFile := fmt.Sprintf("~/.config/containers/systemd/proj-web-%s.container", hash)
	targetExec.files[slotFile] = "[Container]\nImage=ghcr.io/acme/web:2.0\n"

	cfg := ProjectConfig{
		Name:  "proj",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:2.0", Dockerfile: "Dockerfile"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "web:80"}},
				}}},
		},
	}
	// Pre-populate release history
	ctx := context.Background()
	saveRelease(ctx, targetExec, "proj", Release{
		ID: "20260101-100000", Timestamp: "2026-01-01T10:00:00Z",
		Services: map[string]string{"web": "ghcr.io/acme/web:1.0"},
	})
	saveRelease(ctx, targetExec, "proj", Release{
		ID: "20260101-110000", Timestamp: "2026-01-01T11:00:00Z",
		Services: map[string]string{"web": "ghcr.io/acme/web:2.0"},
	})
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Rollback(ctx, cfg, "main", "web"); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")

	// Should have performed installAndStart (which includes daemon-reload and restart)
	if !strings.Contains(cmds, "systemctl --user daemon-reload") {
		t.Fatalf("rollback should trigger daemon-reload:\n%s", cmds)
	}
}

// TestRollbackNonSlottedService verifies rollback restores the previous release
// for a non-slotted (standard) service.
func TestRollbackNonSlottedService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["postgres:15"] = true
	targetExec.existingImage["postgres:16"] = true
	cfg := ProjectConfig{
		Name:  "proj",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"db": {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	// Pre-populate release history: previous had postgres:15, current has postgres:16
	ctx := context.Background()
	saveRelease(ctx, targetExec, "proj", Release{
		ID: "20260101-100000", Timestamp: "2026-01-01T10:00:00Z",
		Services: map[string]string{"db": "postgres:15"},
	})
	saveRelease(ctx, targetExec, "proj", Release{
		ID: "20260101-110000", Timestamp: "2026-01-01T11:00:00Z",
		Services: map[string]string{"db": "postgres:16"},
	})
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Rollback(ctx, cfg, "main", "db"); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}
	// Verify rollback saved a new release with the previous image
	releases, _ := listReleases(ctx, targetExec, "proj")
	if len(releases) < 3 {
		t.Fatalf("expected at least 3 releases after rollback, got %d", len(releases))
	}
	if releases[0].Services["db"] != "postgres:15" {
		t.Fatalf("rollback release db image = %q, want postgres:15", releases[0].Services["db"])
	}
}

func TestRollbackSingleServiceDoesNotRestartUnchangedServices(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["ghcr.io/acme/web:1.0"] = true
	targetExec.existingImage["ghcr.io/acme/web:2.0"] = true
	targetExec.existingImage["ghcr.io/acme/worker:3.0"] = true
	ctx := context.Background()

	cfg := ProjectConfig{
		Name:  "proj",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web":    {Image: "ghcr.io/acme/web:2.0"},
			"worker": {Image: "ghcr.io/acme/worker:3.0"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	saveRelease(ctx, targetExec, "proj", Release{
		ID:        "20260101-100000",
		Timestamp: "2026-01-01T10:00:00Z",
		Services: map[string]string{
			"web":    "ghcr.io/acme/web:1.0",
			"worker": "ghcr.io/acme/worker:3.0",
		},
	})
	saveRelease(ctx, targetExec, "proj", Release{
		ID:        "20260101-110000",
		Timestamp: "2026-01-01T11:00:00Z",
		Services: map[string]string{
			"web":    "ghcr.io/acme/web:2.0",
			"worker": "ghcr.io/acme/worker:3.0",
		},
	})

	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard,
		DrainWait:   -1,
	}
	if err := app.Rollback(ctx, cfg, "main", "web"); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	cmds := strings.Join(targetExec.commands, "\n")
	if strings.Contains(cmds, "systemctl --user restart 'proj-worker.service'") {
		t.Fatalf("worker should not be restarted during single-service rollback:\n%s", cmds)
	}
	if strings.Contains(cmds, "podman pull 'ghcr.io/acme/worker:3.0'") {
		t.Fatalf("worker image should not be re-pulled during single-service rollback:\n%s", cmds)
	}
}

// TestDeployDetectsNonActiveStandardService verifies that deploy returns
// an error when a non-slotted service fails to start (verify step catches it).
func TestDeployDetectsNonActiveStandardService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["postgres:16"] = true
	targetExec.unitStates = map[string]string{
		"proj-db.service": "failed",
	}
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"db": {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	err := app.Deploy(context.Background(), cfg, "main", nil, false)
	if err == nil {
		t.Fatal("Deploy should fail when standard service unit is not active")
	}
	if !strings.Contains(err.Error(), "proj-db.service") {
		t.Fatalf("error should mention the failed unit, got: %v", err)
	}
}

// TestDeploySlottedServiceWithDepsFailedSlot verifies that when service A
// depends on slotted service B and B's slot is failed, deploy restarts B
// and A's quadlet still references B's slot unit.
func TestDeploySlottedServiceWithDepsFailedSlot(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	webSvc := ServiceConfig{Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"}
	webHash := slotHash("ghcr.io/acme/web:1.0")
	webSlotFile := fmt.Sprintf("~/.config/containers/systemd/proj-web-%s.container", webHash)
	targetExec.files[webSlotFile] = renderExpectedSlotContent("proj", "web", webHash, webSvc, map[string]string{"web": webHash}, PodmanRuntime{})
	targetExec.existingImage["ghcr.io/acme/web:1.0"] = true
	targetExec.existingImage["docker.io/library/alpine:3.20"] = true

	webUnit := fmt.Sprintf("proj-web-%s.service", webHash)
	targetExec.unitStates = map[string]string{webUnit: "failed"}

	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web":    webSvc,
			"worker": {Image: "docker.io/library/alpine:3.20", Command: []string{"sleep", "infinity"}, DependsOn: []string{"web"}},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "web:80"}},
				}}},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")

	// web slot should be restarted
	if !strings.Contains(cmds, fmt.Sprintf("systemctl --user restart '%s'", webUnit)) {
		t.Fatalf("should restart failed web slot:\n%s", cmds)
	}

	// worker quadlet should reference web's slot unit
	workerQuadlet, ok := targetExec.files["~/.config/containers/systemd/proj-worker.container"]
	if !ok {
		t.Fatal("worker quadlet should exist")
	}
	expectedDep := fmt.Sprintf("After=proj-web-%s.service", webHash)
	if !strings.Contains(workerQuadlet, expectedDep) {
		t.Fatalf("worker quadlet should depend on web slot unit (%s):\n%s", expectedDep, workerQuadlet)
	}
}

// TestDestroyThenDeploy verifies full lifecycle: init → destroy → deploy.
// After destroy, no slots/files exist. Deploy should create everything fresh.
func TestDestroyThenDeploy(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "web:80"}},
				}}},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}

	// Init (firstInit=true: no slot deploy, standard quadlets)
	if err := app.Init(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-web.container"]; !ok {
		t.Fatal("web quadlet should exist after init")
	}

	// Destroy — glob in rm should remove all proj-*.container files
	if err := app.Destroy(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Destroy failed: %v", err)
	}
	// All project container files should be gone
	for path := range targetExec.files {
		if strings.Contains(path, "containers/systemd/proj-") && strings.HasSuffix(path, ".container") {
			t.Fatalf("container file %s should be removed after destroy", path)
		}
	}

	// Deploy — images still exist from init, should skip build.
	// Since images already exist, nothing is "changed" → web gets standard quadlet (no slot deploy).
	targetExec.commands = nil
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy after destroy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")

	// db should have standard quadlet
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-db.container"]; !ok {
		t.Fatal("db quadlet should be recreated after deploy")
	}
	// web also gets standard quadlet (images exist → not changed → no slot deploy)
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-web.container"]; !ok {
		t.Fatalf("web quadlet should be recreated after deploy, files: %v", keysOf(targetExec.files))
	}
	// Should start units
	if !strings.Contains(cmds, "systemctl --user start") {
		t.Fatalf("should start units after destroy+deploy:\n%s", cmds)
	}
}

// TestCleanThenDeployRebuildsImages verifies that after clean removes images,
// deploy correctly rebuilds/re-pulls them.
func TestCleanThenDeployRebuildsImages(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["ghcr.io/acme/web:1.0"] = true
	targetExec.existingImage["postgres:16"] = true

	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}

	// Deploy — images exist, no build needed
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("First deploy failed: %v", err)
	}

	// Simulate clean: remove all images
	targetExec.existingImage = map[string]bool{}
	targetExec.imageIDs = map[string]string{}
	targetExec.commands = nil

	// Deploy again — should rebuild/re-pull
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy after clean failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "podman build") {
		t.Fatalf("should rebuild web image after clean:\n%s", cmds)
	}
	if !strings.Contains(cmds, "podman pull 'postgres:16'") {
		t.Fatalf("should re-pull db image after clean:\n%s", cmds)
	}
}

// TestStopThenDeployDoesNotLeaveServiceDown verifies that after stopping services,
// deploy brings them back up (systemctl start).
func TestStopThenDeployDoesNotLeaveServiceDown(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["ghcr.io/acme/web:1.0"] = true
	targetExec.existingImage["postgres:16"] = true

	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}

	// Init
	if err := app.Init(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Stop
	if err := app.Stop(context.Background(), cfg, "main", nil); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "systemctl --user stop") {
		t.Fatalf("Stop should issue systemctl stop:\n%s", cmds)
	}

	// Deploy — should start units again
	targetExec.commands = nil
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy after stop failed: %v", err)
	}
	cmds = strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "systemctl --user start") {
		t.Fatalf("Deploy after stop should start units:\n%s", cmds)
	}
}

// TestDeployIdempotentNoRestart verifies that deploying twice with the same
// config does not restart any services on the second deploy.
func TestDeployIdempotentNoRestart(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}

	// First deploy
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("First deploy failed: %v", err)
	}

	// Reset command log, keep images/files
	targetExec.commands = nil
	targetExec.buildCounter = 0

	// Second deploy — should be no-op (no restart)
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Second deploy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if strings.Contains(cmds, "systemctl --user restart") {
		t.Fatalf("idempotent deploy should NOT restart anything:\n%s", cmds)
	}
}

// ---------------------------------------------------------------------------
// Init command tests
// ---------------------------------------------------------------------------

// TestInitCreatesQuadletsAndStartsUnits verifies basic init flow.
func TestInitCreatesQuadletsAndStartsUnits(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Init(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	// Quadlet files should exist
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-web.container"]; !ok {
		t.Fatal("web quadlet missing after init")
	}
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-db.container"]; !ok {
		t.Fatal("db quadlet missing after init")
	}
	if _, ok := targetExec.files["~/.config/containers/systemd/proj.network"]; !ok {
		t.Fatal("network file missing after init")
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "podman build") {
		t.Fatal("init should build images")
	}
	if !strings.Contains(cmds, "podman pull") {
		t.Fatal("init should pull third-party images")
	}
	if !strings.Contains(cmds, "systemctl --user start") {
		t.Fatal("init should start units")
	}
}

// TestInitPartialServices verifies init with a subset of services.
func TestInitPartialServices(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
			"db":  {Image: "postgres:16"},
			"api": {Image: "ghcr.io/acme/api:2.0", Dockerfile: "api/Dockerfile"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	// Only init "db"
	if err := app.Init(context.Background(), cfg, "main", []string{"db"}, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-db.container"]; !ok {
		t.Fatal("db quadlet should exist")
	}
	// web and api should NOT be created
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-web.container"]; ok {
		t.Fatal("web quadlet should NOT exist when not in service list")
	}
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-api.container"]; ok {
		t.Fatal("api quadlet should NOT exist when not in service list")
	}
}

// TestInitRebuildForcesImageRebuild verifies --rebuild flag builds even if image exists.
func TestInitRebuildForcesImageRebuild(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["ghcr.io/acme/web:1.0"] = true // already exists
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Init(context.Background(), cfg, "main", nil, true); err != nil {
		t.Fatalf("Init with rebuild failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "podman build") {
		t.Fatal("init --rebuild should rebuild even when image exists")
	}
}

// TestInitMultipleTargets verifies init runs on all targets when no -t specified.
func TestInitMultipleTargets(t *testing.T) {
	exec1 := newMockExecutor("target-a")
	exec2 := newMockExecutor("target-b")
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
		},
		Targets: map[string]TargetConfig{
			"a": {Name: "a", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
			"b": {Name: "b", Host: "192.0.2.11", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"a": exec1, "b": exec2}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Init(context.Background(), cfg, "", nil, false); err != nil {
		t.Fatalf("Init all targets failed: %v", err)
	}
	if len(exec1.commands) == 0 {
		t.Fatal("target a should have received commands")
	}
	if len(exec2.commands) == 0 {
		t.Fatal("target b should have received commands")
	}
}

// ---------------------------------------------------------------------------
// Build command tests
// ---------------------------------------------------------------------------

// TestBuildOnlyBuildsImages verifies build doesn't install quadlets or start services.
func TestBuildOnlyBuildsImages(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Build(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	// Should build/pull
	if !strings.Contains(cmds, "podman build") {
		t.Fatal("build should build images")
	}
	// Should NOT install quadlets or start services
	if strings.Contains(cmds, "systemctl --user start") {
		t.Fatal("build should NOT start services")
	}
	if strings.Contains(cmds, "systemctl --user daemon-reload") {
		t.Fatal("build should NOT reload systemd")
	}
	// No quadlet files written
	for path := range targetExec.files {
		if strings.HasSuffix(path, ".container") {
			t.Fatalf("build should NOT write quadlet files, found: %s", path)
		}
	}
}

// TestBuildSkipsExistingImages verifies build skips images that already exist.
func TestBuildSkipsExistingImages(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["ghcr.io/acme/web:1.0"] = true
	targetExec.existingImage["postgres:16"] = true
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Build(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if strings.Contains(cmds, "podman build") {
		t.Fatal("build should skip images that already exist")
	}
	if strings.Contains(cmds, "podman pull") {
		t.Fatal("build should skip pulling images that already exist")
	}
}

// TestBuildRebuildForces verifies --rebuild forces build even when image exists.
func TestBuildRebuildForces(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["ghcr.io/acme/web:1.0"] = true
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Build(context.Background(), cfg, "main", nil, true); err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "podman build") {
		t.Fatal("build --rebuild should force rebuild")
	}
}

// TestBuildPartialServices verifies build with a subset of services.
func TestBuildPartialServices(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
			"api": {Image: "ghcr.io/acme/api:2.0", Dockerfile: "api/Dockerfile"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	// Only build "web"
	if err := app.Build(context.Background(), cfg, "main", []string{"web"}, false); err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "podman build") {
		t.Fatal("should build web image")
	}
	// Should not pull postgres (not in the service list)
	if strings.Contains(cmds, "podman pull 'postgres:16'") {
		t.Fatal("should not pull db image when not in service list")
	}
}

// ---------------------------------------------------------------------------
// Status command tests
// ---------------------------------------------------------------------------

// TestStatusOutputsServiceStates verifies status outputs service information.
func TestStatusOutputsServiceStates(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	var buf strings.Builder
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      &buf, DrainWait: -1,
	}
	if err := app.Status(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "target=main") {
		t.Fatalf("status should print target header, got:\n%s", out)
	}
	if !strings.Contains(out, "db") {
		t.Fatalf("status should list db service, got:\n%s", out)
	}
	if !strings.Contains(out, "web") {
		t.Fatalf("status should list web service, got:\n%s", out)
	}
}

// TestStatusWithReplicatedServices verifies replicas are shown individually.
func TestStatusWithReplicatedServices(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"worker": {Image: "worker:1.0", Replicas: 3},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	var buf strings.Builder
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      &buf, DrainWait: -1,
	}
	if err := app.Status(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	out := buf.String()
	for i := 1; i <= 3; i++ {
		label := fmt.Sprintf("worker/%d", i)
		if !strings.Contains(out, label) {
			t.Fatalf("status should show replica %d, got:\n%s", i, out)
		}
	}
}

// TestStatusWithSlottedService verifies status detects slot from ls listing.
func TestStatusWithSlottedService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	hash := slotHash("ghcr.io/acme/web:1.0")
	targetExec.files[fmt.Sprintf("~/.config/containers/systemd/proj-web-%s.container", hash)] = "slot content"
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "web:80"}},
				}}},
		},
	}
	var buf strings.Builder
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      &buf, DrainWait: -1,
	}
	if err := app.Status(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	// Status should query the slot unit, not the base unit
	slotUnit := fmt.Sprintf("proj-web-%s.service", hash)
	if !strings.Contains(cmds, slotUnit) {
		t.Fatalf("status should query slot unit %s, commands:\n%s", slotUnit, cmds)
	}
}

// TestStatusMultipleTargets verifies status iterates all targets.
func TestStatusMultipleTargets(t *testing.T) {
	exec1 := newMockExecutor("target-a")
	exec2 := newMockExecutor("target-b")
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"a": {Name: "a", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
			"b": {Name: "b", Host: "192.0.2.11", User: "u", RepoDir: "/repo"},
		},
	}
	var buf strings.Builder
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"a": exec1, "b": exec2}, local: newMockExecutor("local")},
		Stdout:      &buf, DrainWait: -1,
	}
	if err := app.Status(context.Background(), cfg, ""); err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "target=a") || !strings.Contains(out, "target=b") {
		t.Fatalf("status should show both targets, got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Logs command tests
// ---------------------------------------------------------------------------

// TestLogsSingleService verifies logs issues podman logs for a single service.
func TestLogsSingleService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:1.0"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Logs(context.Background(), cfg, "main", []string{"web"}); err != nil {
		t.Fatalf("Logs failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "podman logs") {
		t.Fatalf("logs should run podman logs, commands:\n%s", cmds)
	}
	if !strings.Contains(cmds, "proj-web") {
		t.Fatalf("logs should reference correct container, commands:\n%s", cmds)
	}
}

// TestLogsAllServices verifies logs without service args streams all services.
func TestLogsAllServices(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:1.0"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Logs(context.Background(), cfg, "main", nil); err != nil {
		t.Fatalf("Logs failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "proj-db") {
		t.Fatalf("logs should include db container, commands:\n%s", cmds)
	}
	if !strings.Contains(cmds, "proj-web") {
		t.Fatalf("logs should include web container, commands:\n%s", cmds)
	}
}

// TestLogsSlottedService verifies logs resolves slot container correctly.
func TestLogsSlottedService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	hash := slotHash("ghcr.io/acme/web:1.0")
	targetExec.files[fmt.Sprintf("~/.config/containers/systemd/proj-web-%s.container", hash)] = "slot"
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Logs(context.Background(), cfg, "main", []string{"web"}); err != nil {
		t.Fatalf("Logs failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	slotContainer := fmt.Sprintf("proj-web-%s", hash)
	if !strings.Contains(cmds, slotContainer) {
		t.Fatalf("logs should use slot container %s, commands:\n%s", slotContainer, cmds)
	}
}

// TestLogsReplicatedService verifies logs for replicated service streams all replicas.
func TestLogsReplicatedService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"worker": {Image: "worker:1.0", Replicas: 3},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Logs(context.Background(), cfg, "main", []string{"worker"}); err != nil {
		t.Fatalf("Logs failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	// All 3 replicas should be streamed
	for i := 1; i <= 3; i++ {
		expected := fmt.Sprintf("proj-worker-%d", i)
		if !strings.Contains(cmds, expected) {
			t.Fatalf("logs should include replica %s, commands:\n%s", expected, cmds)
		}
	}
}

// ---------------------------------------------------------------------------
// Stop command tests
// ---------------------------------------------------------------------------

// TestStopAllServices verifies stop issues systemctl stop for all services.
func TestStopAllServices(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:1.0"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Stop(context.Background(), cfg, "main", nil); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "systemctl --user stop") {
		t.Fatal("stop should issue systemctl stop")
	}
	if !strings.Contains(cmds, "proj-web.service") {
		t.Fatal("stop should include web service")
	}
	if !strings.Contains(cmds, "proj-db.service") {
		t.Fatal("stop should include db service")
	}
}

// TestStopPartialServices verifies stop only stops requested services.
func TestStopPartialServices(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:1.0"},
			"db":  {Image: "postgres:16"},
			"api": {Image: "api:1.0"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Stop(context.Background(), cfg, "main", []string{"web"}); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "proj-web.service") {
		t.Fatal("stop should include web")
	}
	if strings.Contains(cmds, "proj-db.service") {
		t.Fatal("stop should NOT include db when not requested")
	}
}

// TestStopWithProxy verifies stop includes proxy unit when services are exposed.
func TestStopWithProxy(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "web:80"}},
				}}},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Stop(context.Background(), cfg, "main", nil); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "proj-proxy.service") {
		t.Fatal("stop should include proxy service when expose is configured")
	}
}

// ---------------------------------------------------------------------------
// Start command tests
// ---------------------------------------------------------------------------

// TestStartAllServices verifies start issues systemctl start for all services.
func TestStartAllServices(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:1.0"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Start(context.Background(), cfg, "main", nil); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "systemctl --user start") {
		t.Fatal("start should issue systemctl start")
	}
	// Start also starts the network unit
	if !strings.Contains(cmds, "proj-network.service") {
		t.Fatal("start should include network unit")
	}
}

// TestStartPartialServices verifies start only starts requested services.
func TestStartPartialServices(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:1.0"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Start(context.Background(), cfg, "main", []string{"db"}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "proj-db.service") {
		t.Fatal("start should include db")
	}
	if strings.Contains(cmds, "proj-web.service") {
		t.Fatal("start should NOT include web when not requested")
	}
}

// TestStartIncludesNetworkUnit verifies start always includes network unit.
func TestStartIncludesNetworkUnit(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Start(context.Background(), cfg, "main", nil); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "proj-network.service") {
		t.Fatalf("start should always include network unit, commands:\n%s", cmds)
	}
}

// ---------------------------------------------------------------------------
// Destroy command tests
// ---------------------------------------------------------------------------

// TestDestroyRemovesQuadletsAndReloads verifies destroy removes all files and reloads.
func TestDestroyRemovesQuadletsAndReloads(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	// Pre-populate some files
	targetExec.files["~/.config/containers/systemd/proj-web.container"] = "web"
	targetExec.files["~/.config/containers/systemd/proj-db.container"] = "db"
	targetExec.files["~/.config/containers/systemd/proj.network"] = "net"
	targetExec.files["~/.config/qqd/proj/traefik.yml"] = "traefik"
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:1.0"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Destroy(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Destroy failed: %v", err)
	}
	// All container files should be gone
	for path := range targetExec.files {
		if strings.HasSuffix(path, ".container") {
			t.Fatalf("container file %s should be removed", path)
		}
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "systemctl --user stop") {
		t.Fatal("destroy should stop units")
	}
	if !strings.Contains(cmds, "systemctl --user disable") {
		t.Fatal("destroy should disable units")
	}
	if !strings.Contains(cmds, "systemctl --user daemon-reload") {
		t.Fatal("destroy should reload daemon")
	}
}

// TestDestroyWithProxy verifies destroy also cleans proxy when expose is present.
func TestDestroyWithProxy(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "web:80"}},
				}}},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Destroy(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Destroy failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "proj-proxy.service") {
		t.Fatal("destroy should include proxy unit when expose is configured")
	}
	if !strings.Contains(cmds, "rm -rf ~/.config/qqd/") {
		t.Fatal("destroy should clean up traefik config")
	}
}

// TestDestroyMultipleTargets verifies destroy runs on all targets.
func TestDestroyMultipleTargets(t *testing.T) {
	exec1 := newMockExecutor("target-a")
	exec2 := newMockExecutor("target-b")
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"a": {Name: "a", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
			"b": {Name: "b", Host: "192.0.2.11", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"a": exec1, "b": exec2}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Destroy(context.Background(), cfg, ""); err != nil {
		t.Fatalf("Destroy failed: %v", err)
	}
	if len(exec1.commands) == 0 {
		t.Fatal("target a should receive destroy commands")
	}
	if len(exec2.commands) == 0 {
		t.Fatal("target b should receive destroy commands")
	}
}

// ---------------------------------------------------------------------------
// Clean command edge cases
// ---------------------------------------------------------------------------

// TestCleanNoContainersNoImages verifies clean works with nothing to remove.
func TestCleanNoContainersNoImages(t *testing.T) {
	me := &cleanMockExecutor{mockExecutor: newMockExecutor("main")}
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: cleanMockFactory{exec: me},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Clean(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Clean with nothing to remove should not error: %v", err)
	}
	cmds := strings.Join(me.commands, "\n")
	// Should still run prune
	if !strings.Contains(cmds, "podman image prune -f") {
		t.Fatal("clean should always prune dangling images")
	}
}

// TestCleanMultipleTargets verifies clean runs on all targets.
func TestCleanMultipleTargets(t *testing.T) {
	// Use regular mockExecutor since cleanMockFactory only supports single target
	exec1 := newMockExecutor("target-a")
	exec2 := newMockExecutor("target-b")
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"a": {Name: "a", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
			"b": {Name: "b", Host: "192.0.2.11", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"a": exec1, "b": exec2}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Clean(context.Background(), cfg, ""); err != nil {
		t.Fatalf("Clean failed: %v", err)
	}
	if len(exec1.commands) == 0 {
		t.Fatal("target a should receive clean commands")
	}
	if len(exec2.commands) == 0 {
		t.Fatal("target b should receive clean commands")
	}
}

// ---------------------------------------------------------------------------
// Rollback command tests
// ---------------------------------------------------------------------------

// TestRollbackMultipleTargets verifies rollback on all targets.
func TestRollbackMultipleTargets(t *testing.T) {
	exec1 := newMockExecutor("target-a")
	exec2 := newMockExecutor("target-b")
	exec1.existingImage["web:1.0"] = true
	exec1.existingImage["web:2.0"] = true
	exec2.existingImage["web:1.0"] = true
	exec2.existingImage["web:2.0"] = true
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:2.0"},
		},
		Targets: map[string]TargetConfig{
			"a": {Name: "a", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
			"b": {Name: "b", Host: "192.0.2.11", User: "u", RepoDir: "/repo"},
		},
	}
	// Pre-populate release history on both targets
	ctx := context.Background()
	for _, e := range []*mockExecutor{exec1, exec2} {
		saveRelease(ctx, e, "proj", Release{
			ID: "20260101-100000", Timestamp: "2026-01-01T10:00:00Z",
			Services: map[string]string{"web": "web:1.0"},
		})
		saveRelease(ctx, e, "proj", Release{
			ID: "20260101-110000", Timestamp: "2026-01-01T11:00:00Z",
			Services: map[string]string{"web": "web:2.0"},
		})
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"a": exec1, "b": exec2}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Rollback(ctx, cfg, "", "web"); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}
	for _, e := range []*mockExecutor{exec1, exec2} {
		cmds := strings.Join(e.commands, "\n")
		if !strings.Contains(cmds, "systemctl --user daemon-reload") {
			t.Fatalf("rollback should trigger daemon-reload on each target, commands:\n%s", cmds)
		}
	}
}

// TestRollbackReplicatedService verifies rollback restores previous release
// for a replicated service.
func TestRollbackReplicatedService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["worker:1.0"] = true
	targetExec.existingImage["worker:2.0"] = true
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"worker": {Image: "worker:2.0", Replicas: 3},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	// Pre-populate release history
	ctx := context.Background()
	saveRelease(ctx, targetExec, "proj", Release{
		ID: "20260101-100000", Timestamp: "2026-01-01T10:00:00Z",
		Services: map[string]string{"worker": "worker:1.0"},
	})
	saveRelease(ctx, targetExec, "proj", Release{
		ID: "20260101-110000", Timestamp: "2026-01-01T11:00:00Z",
		Services: map[string]string{"worker": "worker:2.0"},
	})
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Rollback(ctx, cfg, "main", "worker"); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	// Replicated services should reference replica units
	if !strings.Contains(cmds, "proj-worker-1.service") {
		t.Fatalf("rollback for replicated service should reference replica unit, commands:\n%s", cmds)
	}
	// Verify rollback release has the old image
	releases, _ := listReleases(ctx, targetExec, "proj")
	if releases[0].Services["worker"] != "worker:1.0" {
		t.Fatalf("rollback release worker image = %q, want worker:1.0", releases[0].Services["worker"])
	}
}

// ---------------------------------------------------------------------------
// Cross-command lifecycle tests (additional edge cases)
// ---------------------------------------------------------------------------

// TestInitThenStopThenStart verifies full lifecycle: init → stop → start.
func TestInitThenStopThenStart(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	// Init
	if err := app.Init(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	// Stop
	targetExec.commands = nil
	if err := app.Stop(context.Background(), cfg, "main", nil); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "systemctl --user stop") {
		t.Fatal("stop should issue systemctl stop")
	}
	// Start
	targetExec.commands = nil
	if err := app.Start(context.Background(), cfg, "main", nil); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	cmds = strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "systemctl --user start") {
		t.Fatal("start should issue systemctl start")
	}
}

// TestDeployThenBuildNewVersion verifies build after deploy with updated image.
func TestDeployThenBuildNewVersion(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	// Deploy v1.0
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	// Now bump to v1.1 and build only
	cfg.Services["web"] = ServiceConfig{Image: "ghcr.io/acme/web:1.1", Dockerfile: "Dockerfile"}
	targetExec.commands = nil
	if err := app.Build(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Build v1.1 failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "podman build") {
		t.Fatal("build should build the new version")
	}
	if !strings.Contains(cmds, "web:1.1") {
		t.Fatal("build should reference the new tag")
	}
}

// TestDeployWithSlottedThenRollback verifies rollback after slot deploy.
func TestDeployWithSlottedThenRollback(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["ghcr.io/acme/web:0.9"] = true
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "web:80"}},
				}}},
		},
	}
	// Pre-populate a previous release so rollback has something to roll back to
	ctx := context.Background()
	saveRelease(ctx, targetExec, "proj", Release{
		ID: "20260101-090000", Timestamp: "2026-01-01T09:00:00Z",
		Services: map[string]string{"web": "ghcr.io/acme/web:0.9", "db": "postgres:16"},
	})
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	// Deploy (web gets slot-deployed because it's exposed and image is new)
	if err := app.Deploy(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	hash := slotHash("ghcr.io/acme/web:1.0")
	slotFile := fmt.Sprintf("~/.config/containers/systemd/proj-web-%s.container", hash)
	if _, ok := targetExec.files[slotFile]; !ok {
		t.Fatal("web should have slot file after deploy")
	}
	// Rollback should reinstall with previous images
	targetExec.commands = nil
	if err := app.Rollback(ctx, cfg, "main", "web"); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	// Rollback should trigger daemon-reload (installAndStart)
	if !strings.Contains(cmds, "systemctl --user daemon-reload") {
		t.Fatalf("rollback should trigger daemon-reload, commands:\n%s", cmds)
	}
}

// TestStopSlottedService verifies stop resolves slot unit correctly.
func TestStopSlottedService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	hash := slotHash("ghcr.io/acme/web:1.0")
	targetExec.files[fmt.Sprintf("~/.config/containers/systemd/proj-web-%s.container", hash)] = "slot"
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Stop(context.Background(), cfg, "main", nil); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	slotUnit := fmt.Sprintf("proj-web-%s.service", hash)
	if !strings.Contains(cmds, slotUnit) {
		t.Fatalf("stop should use slot unit %s, commands:\n%s", slotUnit, cmds)
	}
}

// TestStartSlottedService verifies start resolves slot unit correctly.
func TestStartSlottedService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	hash := slotHash("ghcr.io/acme/web:1.0")
	targetExec.files[fmt.Sprintf("~/.config/containers/systemd/proj-web-%s.container", hash)] = "slot"
	cfg := ProjectConfig{
		Name: "proj",
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Start(context.Background(), cfg, "main", nil); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	cmds := strings.Join(targetExec.commands, "\n")
	slotUnit := fmt.Sprintf("proj-web-%s.service", hash)
	if !strings.Contains(cmds, slotUnit) {
		t.Fatalf("start should use slot unit %s, commands:\n%s", slotUnit, cmds)
	}
}

// TestDeployReplicaCountChange verifies adding replicas triggers restart.
func TestDeployReplicaCountChange(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["worker:1.0"] = true
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"worker": {Image: "worker:1.0", Replicas: 2},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	// First deploy with 2 replicas
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-worker-1.container"]; !ok {
		t.Fatal("worker-1 quadlet should exist")
	}
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-worker-2.container"]; !ok {
		t.Fatal("worker-2 quadlet should exist")
	}
	// Now deploy with 3 replicas
	cfg.Services["worker"] = ServiceConfig{Image: "worker:1.0", Replicas: 3}
	targetExec.commands = nil
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy with 3 replicas failed: %v", err)
	}
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-worker-3.container"]; !ok {
		t.Fatal("worker-3 quadlet should be created")
	}
}

// TestDeployWithExposeCreatesProxy verifies that expose config creates proxy files.
func TestDeployWithExposeCreatesProxy(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "web:80"}},
				}}},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-proxy.container"]; !ok {
		t.Fatal("proxy quadlet should exist with expose config")
	}
	if _, ok := targetExec.files["~/.config/qqd/proj/traefik.yml"]; !ok {
		t.Fatal("traefik config should exist with expose config")
	}
	if _, ok := targetExec.files["~/.config/qqd/proj/dynamic/routes.yml"]; !ok {
		t.Fatal("routes config should exist with expose config")
	}
}

// TestDeployWithoutExposeNoProxy verifies no proxy files when no expose config.
func TestDeployWithoutExposeNoProxy(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0", Dockerfile: "Dockerfile"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	if _, ok := targetExec.files["~/.config/containers/systemd/proj-proxy.container"]; ok {
		t.Fatal("proxy quadlet should NOT exist without expose config")
	}
}

// TestCleanStaleSlotsRemovesOldSlots verifies that cleanStaleSlots finds and removes
// quadlet files for slots that don't match the currently active slot, then stops
// their systemd units.
func TestCleanStaleSlotsRemovesOldSlots(t *testing.T) {
	targetExec := newMockExecutor("target")
	qdDir := "~/.config/containers/systemd"

	// Simulate 3 slot versions: old1, old2 are stale; current is active
	oldHash1 := "a1b2c3d4"
	oldHash2 := "e5f6a7b8"
	currentHash := slotHash("ghcr.io/acme/server:2.0")

	targetExec.files[fmt.Sprintf("%s/proj-server-%s.container", qdDir, oldHash1)] = "[Container]\nImage=old1\n"
	targetExec.files[fmt.Sprintf("%s/proj-server-%s.container", qdDir, oldHash2)] = "[Container]\nImage=old2\n"
	targetExec.files[fmt.Sprintf("%s/proj-server-%s.container", qdDir, currentHash)] = "[Container]\nImage=ghcr.io/acme/server:2.0\n"
	// Also keep the network file (should not be touched)
	targetExec.files[fmt.Sprintf("%s/proj.network", qdDir)] = "[Network]\n"

	app := &App{Stdout: io.Discard}
	slottedSvcs := map[string]bool{"server": true}
	activeSlots := map[string]string{"server": currentHash}

	app.cleanStaleSlots(context.Background(), "proj", qdDir, slottedSvcs, activeSlots, targetExec)

	cmds := strings.Join(targetExec.commands, "\n")

	// Stale files should be removed
	if _, ok := targetExec.files[fmt.Sprintf("%s/proj-server-%s.container", qdDir, oldHash1)]; ok {
		t.Fatal("stale slot a1b2c3d4 file should be removed")
	}
	if _, ok := targetExec.files[fmt.Sprintf("%s/proj-server-%s.container", qdDir, oldHash2)]; ok {
		t.Fatal("stale slot e5f6a7b8 file should be removed")
	}

	// Active slot file should still exist
	if _, ok := targetExec.files[fmt.Sprintf("%s/proj-server-%s.container", qdDir, currentHash)]; !ok {
		t.Fatal("active slot file should NOT be removed")
	}

	// Network file should still exist
	if _, ok := targetExec.files[fmt.Sprintf("%s/proj.network", qdDir)]; !ok {
		t.Fatal("network file should NOT be touched")
	}

	// Should have daemon-reloaded
	if !strings.Contains(cmds, "systemctl --user daemon-reload") {
		t.Fatalf("should daemon-reload after removing stale files:\n%s", cmds)
	}

	// Should have stopped stale units
	if !strings.Contains(cmds, fmt.Sprintf("systemctl --user stop 'proj-server-%s.service'", oldHash1)) {
		t.Fatalf("should stop stale unit %s:\n%s", oldHash1, cmds)
	}
	if !strings.Contains(cmds, fmt.Sprintf("systemctl --user stop 'proj-server-%s.service'", oldHash2)) {
		t.Fatalf("should stop stale unit %s:\n%s", oldHash2, cmds)
	}
}

// TestCleanStaleSlotsRemovesOrphanedContainers verifies that cleanStaleSlots
// finds and removes running containers that have no corresponding quadlet file.
func TestCleanStaleSlotsRemovesOrphanedContainers(t *testing.T) {
	targetExec := newMockExecutor("target")
	qdDir := "~/.config/containers/systemd"

	currentHash := slotHash("ghcr.io/acme/server:2.0")
	orphanHash := "deadbeef"

	// Only active slot quadlet exists (no quadlet for orphan)
	targetExec.files[fmt.Sprintf("%s/proj-server-%s.container", qdDir, currentHash)] = "[Container]\n"

	// Mock podman ps to return both the active container and an orphaned one
	// We need to override the default Run behavior for podman ps commands.
	// The mock executor returns "active\n" for unrecognized commands, so we need
	// to inject a podman ps response. We do this by adding a special file that
	// the cleanStaleSlots podman ps command would match.
	// Actually, cleanStaleSlots uses: podman ps -a --filter name='proj-server-' --format '{{.Names}}'
	// The default mockExecutor returns "active\n" for this. We need a custom mock.

	// Use a wrapper that intercepts podman ps
	wrapper := &podmanPsMockExecutor{
		mockExecutor: targetExec,
		psResponses: map[string]string{
			"proj-server-": fmt.Sprintf("proj-server-%s\nproj-server-%s", currentHash, orphanHash),
		},
	}

	app := &App{Stdout: io.Discard}
	slottedSvcs := map[string]bool{"server": true}
	activeSlots := map[string]string{"server": currentHash}

	app.cleanStaleSlots(context.Background(), "proj", qdDir, slottedSvcs, activeSlots, wrapper)

	cmds := strings.Join(wrapper.commands, "\n")

	// Should stop and rm the orphaned container
	if !strings.Contains(cmds, fmt.Sprintf("podman stop 'proj-server-%s'", orphanHash)) {
		t.Fatalf("should stop orphaned container:\n%s", cmds)
	}
	if !strings.Contains(cmds, fmt.Sprintf("podman rm 'proj-server-%s'", orphanHash)) {
		t.Fatalf("should rm orphaned container:\n%s", cmds)
	}

	// Should NOT stop or rm the active container
	if strings.Contains(cmds, fmt.Sprintf("podman stop 'proj-server-%s'", currentHash)) {
		t.Fatalf("should NOT stop active container:\n%s", cmds)
	}
}

// TestCleanStaleSlotsNoopWhenClean verifies that cleanStaleSlots is a no-op when
// there are no stale files or orphaned containers.
func TestCleanStaleSlotsNoopWhenClean(t *testing.T) {
	targetExec := newMockExecutor("target")
	qdDir := "~/.config/containers/systemd"

	currentHash := slotHash("ghcr.io/acme/server:1.0")
	targetExec.files[fmt.Sprintf("%s/proj-server-%s.container", qdDir, currentHash)] = "[Container]\n"
	targetExec.files[fmt.Sprintf("%s/proj.network", qdDir)] = "[Network]\n"

	// Mock podman ps to return only the active container
	wrapper := &podmanPsMockExecutor{
		mockExecutor: targetExec,
		psResponses: map[string]string{
			"proj-server-": fmt.Sprintf("proj-server-%s", currentHash),
		},
	}

	app := &App{Stdout: io.Discard}
	slottedSvcs := map[string]bool{"server": true}
	activeSlots := map[string]string{"server": currentHash}

	app.cleanStaleSlots(context.Background(), "proj", qdDir, slottedSvcs, activeSlots, wrapper)

	cmds := strings.Join(wrapper.commands, "\n")

	// Should NOT daemon-reload (no stale files removed)
	daemonReloadCount := strings.Count(cmds, "systemctl --user daemon-reload")
	if daemonReloadCount > 0 {
		t.Fatalf("should not daemon-reload when no stale files:\n%s", cmds)
	}
	// Should NOT stop/rm any containers
	if strings.Contains(cmds, "podman stop") {
		t.Fatalf("should not stop any containers:\n%s", cmds)
	}
	if strings.Contains(cmds, "podman rm") {
		t.Fatalf("should not rm any containers:\n%s", cmds)
	}
}

// TestDeployMultipleSlotVersionsCleanedUp verifies that deploying several image
// versions in sequence leaves only the latest slot and cleans up all previous ones.
func TestDeployMultipleSlotVersionsCleanedUp(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	baseCfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/acme/server:1.0",
				Dockerfile: "Dockerfile",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
				}},
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}

	qdDir := "~/.config/containers/systemd"

	// Deploy v1.0
	if err := app.Deploy(context.Background(), baseCfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy v1.0 failed: %v", err)
	}
	hash1 := slotHash("ghcr.io/acme/server:1.0")
	if _, ok := targetExec.files[fmt.Sprintf("%s/proj-server-%s.container", qdDir, hash1)]; !ok {
		t.Fatalf("v1.0 slot file should exist after first deploy")
	}

	// Deploy v2.0
	targetExec.commands = nil
	targetExec.buildCounter = 0
	cfg2 := baseCfg
	cfg2.Services = map[string]ServiceConfig{
		"server": {Image: "ghcr.io/acme/server:2.0", Dockerfile: "Dockerfile"},
	}
	if err := app.Deploy(context.Background(), cfg2, "main", nil, false); err != nil {
		t.Fatalf("Deploy v2.0 failed: %v", err)
	}
	hash2 := slotHash("ghcr.io/acme/server:2.0")
	// v2.0 slot should exist
	if _, ok := targetExec.files[fmt.Sprintf("%s/proj-server-%s.container", qdDir, hash2)]; !ok {
		t.Fatalf("v2.0 slot file should exist after second deploy")
	}
	// v1.0 slot should be cleaned up
	if _, ok := targetExec.files[fmt.Sprintf("%s/proj-server-%s.container", qdDir, hash1)]; ok {
		t.Fatalf("v1.0 slot file should be removed after second deploy")
	}

	// Deploy v3.0
	targetExec.commands = nil
	targetExec.buildCounter = 0
	cfg3 := baseCfg
	cfg3.Services = map[string]ServiceConfig{
		"server": {Image: "ghcr.io/acme/server:3.0", Dockerfile: "Dockerfile"},
	}
	if err := app.Deploy(context.Background(), cfg3, "main", nil, false); err != nil {
		t.Fatalf("Deploy v3.0 failed: %v", err)
	}
	hash3 := slotHash("ghcr.io/acme/server:3.0")
	// v3.0 slot should exist
	if _, ok := targetExec.files[fmt.Sprintf("%s/proj-server-%s.container", qdDir, hash3)]; !ok {
		t.Fatalf("v3.0 slot file should exist after third deploy")
	}
	// Both v1.0 and v2.0 should be gone
	if _, ok := targetExec.files[fmt.Sprintf("%s/proj-server-%s.container", qdDir, hash1)]; ok {
		t.Fatalf("v1.0 slot file should be removed after third deploy")
	}
	if _, ok := targetExec.files[fmt.Sprintf("%s/proj-server-%s.container", qdDir, hash2)]; ok {
		t.Fatalf("v2.0 slot file should be removed after third deploy")
	}

	// Only 1 slot file should exist for "server"
	serverSlotCount := 0
	for f := range targetExec.files {
		if strings.HasPrefix(f, qdDir+"/proj-server-") && strings.HasSuffix(f, ".container") {
			serverSlotCount++
		}
	}
	if serverSlotCount != 1 {
		var names []string
		for f := range targetExec.files {
			if strings.HasPrefix(f, qdDir+"/proj-server-") {
				names = append(names, f)
			}
		}
		t.Fatalf("expected exactly 1 server slot file, got %d: %v", serverSlotCount, names)
	}
}

// TestDeployMultilineEnvInQuadlet verifies that services with multiline JSON env
// values (like GCP service account keys) produce properly escaped quadlet files.
func TestDeployMultilineEnvInQuadlet(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	gcpJSON := "{\n  \"type\": \"service_account\",\n  \"project_id\": \"myproject\",\n  \"private_key_id\": \"key123\",\n  \"client_email\": \"user@domain.iam.gserviceaccount.com\",\n  \"client_x509_cert_url\": \"https://www.googleapis.com/robot/v1/metadata/x509/user%40domain.iam.gserviceaccount.com\"\n}"
	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "server:1.0",
				Dockerfile: "Dockerfile",
				Env: map[string]string{
					"GCP_KEY": gcpJSON,
					"SIMPLE":  "hello",
				},
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/proj/repo",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Deploy(context.Background(), cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	// Read the generated quadlet from mock filesystem
	quadlet, ok := targetExec.files["~/.config/containers/systemd/proj-server.container"]
	if !ok {
		t.Fatal("server container file should exist")
	}

	// Multiline JSON value must be quoted with escaped newlines and quotes
	if !strings.Contains(quadlet, `Environment="GCP_KEY=`) {
		t.Fatalf("multiline env should use quoted form:\n%s", quadlet)
	}
	// Must NOT contain raw newlines inside the Environment= line
	for _, line := range strings.Split(quadlet, "\n") {
		if strings.HasPrefix(line, "Environment=") && strings.Contains(line, "GCP_KEY") {
			// This line must be a single line (no raw newlines inside the value)
			if strings.Contains(line, "\n") {
				t.Fatal("Environment=GCP_KEY line should not contain raw newlines")
			}
			// Should contain escaped newlines
			if !strings.Contains(line, `\n`) {
				t.Fatalf("should contain escaped newlines (\\n):\n%s", line)
			}
			// Should contain escaped quotes
			if !strings.Contains(line, `\"`) {
				t.Fatalf("should contain escaped quotes (\\\"):\n%s", line)
			}
			// Should contain %% for %40 (URL encoded @)
			if !strings.Contains(line, "%%40") {
				t.Fatalf("should contain %%%% for %%40 (systemd percent escaping):\n%s", line)
			}
			break
		}
	}

	// Simple env should be unquoted
	if !strings.Contains(quadlet, "Environment=SIMPLE=hello") {
		t.Fatalf("simple env should be unquoted:\n%s", quadlet)
	}
}

// podmanPsMockExecutor wraps mockExecutor to intercept podman ps commands.
type podmanPsMockExecutor struct {
	*mockExecutor
	psResponses map[string]string // filter prefix -> response
}

func (m *podmanPsMockExecutor) Run(ctx context.Context, cmd string) (string, error) {
	m.commands = append(m.commands, cmd)
	if strings.Contains(cmd, "podman ps -a --filter name=") {
		for prefix, response := range m.psResponses {
			if strings.Contains(cmd, prefix) {
				return response + "\n", nil
			}
		}
		return "\n", nil
	}
	return m.mockExecutor.Run(ctx, cmd)
}

func (m *podmanPsMockExecutor) RunStream(ctx context.Context, cmd string, w io.Writer) error {
	_, err := m.Run(ctx, cmd)
	return err
}

func (m *podmanPsMockExecutor) CopyFrom(ctx context.Context, r, l string) error {
	return m.mockExecutor.CopyFrom(ctx, r, l)
}

func (m *podmanPsMockExecutor) CopyTo(ctx context.Context, l, r string) error {
	return m.mockExecutor.CopyTo(ctx, l, r)
}

func (m *podmanPsMockExecutor) Close() error { return nil }
func (m *podmanPsMockExecutor) ID() string   { return m.mockExecutor.ID() }

// keysOf returns sorted keys of a string map (test helper).
func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestDeployAutoRollback(t *testing.T) {
	oldImage := "ghcr.io/acme/app/server:1.44"
	newImage := "ghcr.io/acme/app/server:1.45"
	targetExec := newMockExecutor("target-main")

	// Make the batch unit start fail once (deploy fails, rollback succeeds)
	targetExec.failCmds = map[string]int{
		"systemctl --user start ": 1,
	}

	// Pre-populate a previous release with old image
	relJSON := fmt.Sprintf(`{"id":"20260417-100000.000","timestamp":"2026-04-17T10:00:00Z","services":{"server":"%s"}}`, oldImage)
	targetExec.files["~/.config/qqd/app/releases/20260417-100000.000.json"] = relJSON

	// Old image exists on target
	targetExec.existingImage[oldImage] = true

	cfg := ProjectConfig{
		Name:   "app",
		Repo:   "git@github.com:acme/app.git",
		Branch: "main",
		Build:  BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      newImage,
				Dockerfile: "Dockerfile",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.10",
				User:    "deploy",
				RepoDir: "/home/deploy/app/repo",
			},
		},
	}

	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}

	err := app.Deploy(context.Background(), cfg, "main", nil, false)
	if err == nil {
		t.Fatal("expected deploy to fail")
	}

	// After auto-rollback, the quadlet should contain the old image
	qdDir := "~/.config/containers/systemd"
	quadletPath := fmt.Sprintf("%s/app-server.container", qdDir)
	content := targetExec.files[quadletPath]

	if !strings.Contains(content, oldImage) {
		t.Fatalf("expected quadlet to contain old image %s after rollback, got:\n%s", oldImage, content)
	}
	if strings.Contains(content, newImage) {
		t.Fatalf("expected quadlet NOT to contain new image after rollback, got:\n%s", content)
	}

	// Verify restart was issued during rollback
	cmds := strings.Join(targetExec.commands, "\n")
	if !strings.Contains(cmds, "systemctl --user restart 'app-server.service'") {
		t.Fatalf("expected restart command for rollback, got:\n%s", cmds)
	}
}

func TestDeployAutoRollbackNoRelease(t *testing.T) {
	newImage := "ghcr.io/acme/app/server:1.45"
	targetExec := newMockExecutor("target-main")

	// Make unit start fail
	targetExec.failCmds = map[string]int{
		"systemctl --user start ": 1,
	}

	// NO previous release on target

	cfg := ProjectConfig{
		Name:   "app",
		Repo:   "git@github.com:acme/app.git",
		Branch: "main",
		Build:  BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      newImage,
				Dockerfile: "Dockerfile",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.10",
				User:    "deploy",
				RepoDir: "/home/deploy/app/repo",
			},
		},
	}

	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}

	err := app.Deploy(context.Background(), cfg, "main", nil, false)
	if err == nil {
		t.Fatal("expected deploy to fail")
	}

	// With no release, rollback is skipped. The quadlet should still have the new image
	// (written during the failed deploy, not overwritten by rollback).
	qdDir := "~/.config/containers/systemd"
	quadletPath := fmt.Sprintf("%s/app-server.container", qdDir)
	content := targetExec.files[quadletPath]

	if !strings.Contains(content, newImage) {
		t.Fatalf("with no release to rollback to, quadlet should still have new image, got:\n%s", content)
	}
}

func TestDeployAutoRollbackSlotBased(t *testing.T) {
	oldImage := "ghcr.io/acme/app/server:1.44"
	newImage := "ghcr.io/acme/app/server:1.45"
	newHash := slotHash(newImage)
	oldHash := slotHash(oldImage)
	targetExec := newMockExecutor("target-main")

	// Fail the new slot's start command (once, so rollback's slot succeeds)
	newSlotUnit := fmt.Sprintf("app-server-%s.service", newHash)
	targetExec.failCmds = map[string]int{
		fmt.Sprintf("start '%s'", newSlotUnit): 1,
	}

	// Pre-populate a previous release
	relJSON := fmt.Sprintf(`{"id":"20260417-100000.000","timestamp":"2026-04-17T10:00:00Z","services":{"server":"%s"}}`, oldImage)
	targetExec.files["~/.config/qqd/app/releases/20260417-100000.000.json"] = relJSON

	// Old image exists on target
	targetExec.existingImage[oldImage] = true

	cfg := ProjectConfig{
		Name:   "app",
		Repo:   "git@github.com:acme/app.git",
		Branch: "main",
		Build:  BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      newImage,
				Dockerfile: "Dockerfile",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.10",
				User:    "deploy",
				RepoDir: "/home/deploy/app/repo",
				Expose: ExposeConfig{
					Entries: []ExposeEntry{
						{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
					},
				},
			},
		},
	}

	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}

	err := app.Deploy(context.Background(), cfg, "main", nil, false)
	if err == nil {
		t.Fatal("expected deploy to fail")
	}

	qdDir := "~/.config/containers/systemd"

	// The new slot quadlet should have been cleaned up by slotDeploy
	newSlotQuadlet := fmt.Sprintf("%s/app-server-%s.container", qdDir, newHash)
	if _, exists := targetExec.files[newSlotQuadlet]; exists {
		t.Fatalf("new slot quadlet %s should have been cleaned up", newSlotQuadlet)
	}

	// After rollback, the old slot quadlet should exist
	oldSlotQuadlet := fmt.Sprintf("%s/app-server-%s.container", qdDir, oldHash)
	content, exists := targetExec.files[oldSlotQuadlet]
	if !exists {
		t.Fatalf("expected rollback slot quadlet %s to exist, files: %v", oldSlotQuadlet, keysOf(targetExec.files))
	}
	if !strings.Contains(content, oldImage) {
		t.Fatalf("rollback slot quadlet should contain old image, got:\n%s", content)
	}

	// Verify old slot was started during rollback
	cmds := strings.Join(targetExec.commands, "\n")
	oldSlotStartCmd := fmt.Sprintf("start 'app-server-%s.service'", oldHash)
	if !strings.Contains(cmds, oldSlotStartCmd) {
		t.Fatalf("expected old slot start command, got:\n%s", cmds)
	}
}

// rollbackDepsFixture builds a two-service project ("server" plus "mcp", which
// depends on it) where both are HTTP-exposed and therefore slot-deployed, both
// images move forward, and the forward deploy fails its final verification —
// forcing an auto-rollback that has to move both services back a slot.
type rollbackDepsFixture struct {
	exec                     *mockExecutor
	cfg                      ProjectConfig
	app                      *App
	oldServerHash, newServer string
	oldMcpHash, newMcpHash   string
}

func newRollbackDepsFixture(t *testing.T) rollbackDepsFixture {
	t.Helper()
	const (
		oldServerImage = "ghcr.io/acme/proj/server:1.0"
		newServerImage = "ghcr.io/acme/proj/server:2.0"
		oldMcpImage    = "ghcr.io/acme/proj/mcp:1.0"
		newMcpImage    = "ghcr.io/acme/proj/mcp:2.0"
	)
	f := rollbackDepsFixture{
		exec:          newMockExecutor("target-main"),
		oldServerHash: slotHash(oldServerImage),
		newServer:     slotHash(newServerImage),
		oldMcpHash:    slotHash(oldMcpImage),
		newMcpHash:    slotHash(newMcpImage),
	}

	oldServerSvc := ServiceConfig{Image: oldServerImage, Dockerfile: "Dockerfile"}
	oldMcpSvc := ServiceConfig{Image: oldMcpImage, Dockerfile: "Dockerfile.mcp", DependsOn: []string{"server"}}
	existing := map[string]string{"server": f.oldServerHash, "mcp": f.oldMcpHash}
	f.exec.files[fmt.Sprintf("%s/proj-server-%s.container", testQdDir, f.oldServerHash)] =
		renderExpectedSlotContent("proj", "server", f.oldServerHash, oldServerSvc, existing, PodmanRuntime{})
	f.exec.files[fmt.Sprintf("%s/proj-mcp-%s.container", testQdDir, f.oldMcpHash)] =
		renderExpectedSlotContent("proj", "mcp", f.oldMcpHash, oldMcpSvc, existing, PodmanRuntime{})

	f.exec.files["~/.config/qqd/proj/releases/20260417-100000.000.json"] = fmt.Sprintf(
		`{"id":"20260417-100000.000","timestamp":"2026-04-17T10:00:00Z","services":{"server":%q,"mcp":%q}}`,
		oldServerImage, oldMcpImage)

	// Only the previous images exist, so both services build (and count as
	// changed) on the forward deploy and are pullable on the way back.
	for _, img := range []string{oldServerImage, oldMcpImage} {
		f.exec.existingImage[img] = true
		f.exec.imageIDs[img] = "sha256:" + slotHash(img)
	}

	// The new mcp slot starts but never becomes active: the forward deploy gets
	// all the way to verification and fails there, exactly as a dependent
	// stopped by a systemd Requires= cascade would.
	f.exec.unitStates = map[string]string{
		fmt.Sprintf("proj-mcp-%s.service", f.newMcpHash): "inactive",
	}

	f.cfg = ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {Image: newServerImage, Dockerfile: "Dockerfile"},
			"mcp":    {Image: newMcpImage, Dockerfile: "Dockerfile.mcp", DependsOn: []string{"server"}},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name: "main", Host: "192.0.2.30", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/mcp": "mcp:8989", "/": "server:8080"}},
				}},
			},
		},
	}
	f.app = &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": f.exec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}
	return f
}

// TestAutoRollbackRepointsDependentQuadletsAtRestoredSlot covers the failure
// mode where a rollback moved both a service and its dependent back a slot, but
// left the dependent's quadlet requiring the forward attempt's slot — which the
// rollback then removed. systemd cascaded the stop into the dependent and
// afterwards refused to start it, because the unit it required no longer existed.
//
// After a rollback every dependent must reference a unit that exists.
func TestAutoRollbackRepointsDependentQuadletsAtRestoredSlot(t *testing.T) {
	f := newRollbackDepsFixture(t)

	if err := f.app.Deploy(context.Background(), f.cfg, "main", nil, false); err == nil {
		t.Fatal("expected the deploy to fail so the auto-rollback runs")
	}

	restoredMcp := fmt.Sprintf("%s/proj-mcp-%s.container", testQdDir, f.oldMcpHash)
	mcpQuadlet, ok := f.exec.files[restoredMcp]
	if !ok {
		t.Fatalf("rollback should restore the mcp slot quadlet %s; files: %v", restoredMcp, keysOf(f.exec.files))
	}
	restoredServerUnit := fmt.Sprintf("proj-server-%s.service", f.oldServerHash)
	if !strings.Contains(mcpQuadlet, "Requires="+restoredServerUnit) {
		t.Fatalf("mcp must require the restored server slot (%s):\n%s", restoredServerUnit, mcpQuadlet)
	}
	abandonedServerUnit := fmt.Sprintf("proj-server-%s.service", f.newServer)
	if strings.Contains(mcpQuadlet, abandonedServerUnit) {
		t.Fatalf("mcp must not reference the torn-down server slot (%s):\n%s", abandonedServerUnit, mcpQuadlet)
	}

	// The unit mcp now requires has to actually exist on the target.
	restoredServerQuadlet := fmt.Sprintf("%s/proj-server-%s.container", testQdDir, f.oldServerHash)
	if _, ok := f.exec.files[restoredServerQuadlet]; !ok {
		t.Fatalf("mcp requires %s but its quadlet is missing; files: %v", restoredServerUnit, keysOf(f.exec.files))
	}
	if _, ok := f.exec.files[fmt.Sprintf("%s/proj-server-%s.container", testQdDir, f.newServer)]; ok {
		t.Fatal("the abandoned server slot quadlet should have been removed by the rollback")
	}

	// The rewrite has to be visible to systemd before the units are (re)started.
	cmds := f.exec.commands
	writeIdx, reloadIdx := -1, -1
	for i, c := range cmds {
		switch {
		case writeIdx == -1 && strings.HasPrefix(c, "cat > "+restoredMcp+" ") && strings.Contains(c, restoredServerUnit):
			writeIdx = i
		case writeIdx != -1 && reloadIdx == -1 && strings.Contains(c, "daemon-reload"):
			reloadIdx = i
		}
	}
	if writeIdx == -1 || reloadIdx == -1 {
		t.Fatalf("expected the repointed mcp quadlet to be written and followed by a daemon-reload:\n%s", strings.Join(cmds, "\n"))
	}
}

// TestAutoRollbackErrorNamesRestoredStateNotAbandonedSlot covers a successful
// rollback being reported with the failed forward attempt's slot unit — a unit
// that no longer exists on the target, has no journal, and sends the operator
// looking for a container that was never left behind.
func TestAutoRollbackErrorNamesRestoredStateNotAbandonedSlot(t *testing.T) {
	f := newRollbackDepsFixture(t)

	err := f.app.Deploy(context.Background(), f.cfg, "main", nil, false)
	if err == nil {
		t.Fatal("expected the deploy to fail so the auto-rollback runs")
	}
	if !strings.Contains(err.Error(), "rolled back to release 20260417-100000.000") {
		t.Fatalf("a completed rollback must be reported as such, got: %v", err)
	}
	for _, abandoned := range []string{f.newMcpHash, f.newServer} {
		if strings.Contains(err.Error(), abandoned) {
			t.Fatalf("error names slot %s, which the rollback tore down: %v", abandoned, err)
		}
	}
}

// TestVerifySlottedUnitNamesCurrentSlot covers the verification pass reporting a
// slot name from the map it computed before the pass instead of the slot the
// service actually occupies now.
func TestVerifySlottedUnitNamesCurrentSlot(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	oldImage := "ghcr.io/acme/proj/server:1.0"
	newImage := "ghcr.io/acme/proj/server:2.0"
	oldHash, newHash := slotHash(oldImage), slotHash(newImage)

	oldSvc := ServiceConfig{Image: oldImage, Dockerfile: "Dockerfile"}
	targetExec.files[fmt.Sprintf("%s/proj-server-%s.container", testQdDir, oldHash)] =
		renderExpectedSlotContent("proj", "server", oldHash, oldSvc, map[string]string{"server": oldHash}, PodmanRuntime{})
	targetExec.existingImage[oldImage] = true
	targetExec.imageIDs[oldImage] = "sha256:old"
	// The new slot starts but never goes active, and there is no release to roll
	// back to, so the deploy error is the raw verification failure.
	targetExec.unitStates = map[string]string{
		fmt.Sprintf("proj-server-%s.service", newHash): "inactive",
	}

	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {Image: newImage, Dockerfile: "Dockerfile"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name: "main", Host: "192.0.2.31", User: "u", RepoDir: "/repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
				}},
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{targets: map[string]*mockExecutor{"main": targetExec}, local: newMockExecutor("local")},
		Stdout:      io.Discard, DrainWait: -1,
	}

	err := app.Deploy(context.Background(), cfg, "main", nil, false)
	if err == nil {
		t.Fatal("expected the deploy to fail verification")
	}
	if !strings.Contains(err.Error(), newHash) {
		t.Fatalf("verification must name the slot in effect (%s), got: %v", newHash, err)
	}
	if strings.Contains(err.Error(), oldHash) {
		t.Fatalf("verification must not name the slot the service left (%s), got: %v", oldHash, err)
	}
}

// TestDiagnoseUnitFallsBackToJournalWhenContainerReaped covers slot units running
// `podman run --rm`: by the time a failure is diagnosed the container is gone, so
// `podman logs` returns "no such container" and the only evidence left is the
// unit journal. Printing the runtime's own error instead loses the cause of the
// failure for good.
func TestDiagnoseUnitFallsBackToJournalWhenContainerReaped(t *testing.T) {
	exec := newMockExecutor("target-main")
	unit, cname := "proj-svc-a1b2c3d4.service", "proj-svc-a1b2c3d4"
	exec.unitStates = map[string]string{unit: "inactive"}
	exec.stdoutFor["podman logs"] = `Error: no container with name or ID "` + cname + `" found: no such container`
	exec.stdoutFor["journalctl --user -u"] = "svc: listen tcp :8080: bind: address already in use"

	out := &strings.Builder{}
	app := &App{Runtime: PodmanRuntime{}, Stdout: out}
	app.diagnoseUnit(context.Background(), exec, "svc", unit, cname, ServiceConfig{})

	if !strings.Contains(out.String(), "address already in use") {
		t.Fatalf("expected the unit journal as fallback evidence, got:\n%s", out)
	}
	if strings.Contains(out.String(), "no such container") {
		t.Fatalf("the runtime's 'container is gone' error is not diagnostics:\n%s", out)
	}
}

// TestDiagnoseUnitPrefersContainerLogs keeps the journal fallback from displacing
// real container output when the container is still around.
func TestDiagnoseUnitPrefersContainerLogs(t *testing.T) {
	exec := newMockExecutor("target-main")
	unit, cname := "proj-svc-a1b2c3d4.service", "proj-svc-a1b2c3d4"
	exec.unitStates = map[string]string{unit: "failed"}
	exec.stdoutFor["podman logs"] = "panic: config: missing DATABASE_URL"

	out := &strings.Builder{}
	app := &App{Runtime: PodmanRuntime{}, Stdout: out}
	app.diagnoseUnit(context.Background(), exec, "svc", unit, cname, ServiceConfig{})

	if !strings.Contains(out.String(), "missing DATABASE_URL") {
		t.Fatalf("expected container logs, got:\n%s", out)
	}
	for _, c := range exec.commands {
		if strings.Contains(c, "journalctl") {
			t.Fatalf("journal is only a fallback; container logs were available:\n%s", strings.Join(exec.commands, "\n"))
		}
	}
}

// TestSlotDeployCapturesFailureBeforeContainerIsReaped covers diagnostics for a
// slot that never became ready: the evidence has to be collected before the unit
// is stopped, because `--rm` removes the container with it.
func TestSlotDeployCapturesFailureBeforeContainerIsReaped(t *testing.T) {
	exec := newMockExecutor("target-main")
	image := "ghcr.io/acme/proj/server:2.0"
	hash := slotHash(image)
	unit := fmt.Sprintf("proj-server-%s.service", hash)
	exec.failCmds = map[string]int{fmt.Sprintf("start '%s'", unit): 1}
	exec.stdoutFor["podman logs"] = "server: exiting: bad config"

	out := &strings.Builder{}
	app := &App{Runtime: PodmanRuntime{}, Stdout: out, DrainWait: -1}
	cfg := ProjectConfig{Name: "proj", Services: map[string]ServiceConfig{"server": {Image: image}}}
	eff := EffectiveTarget{Target: TargetConfig{Name: "main"}, Services: cfg.Services}
	err := app.slotDeploy(context.Background(), cfg, eff, exec, "server", cfg.Services["server"], cfg.Services, map[string]string{})
	if err == nil {
		t.Fatal("expected the slot start to fail")
	}
	if !strings.Contains(out.String(), "bad config") {
		t.Fatalf("expected the failing container's output to be captured:\n%s", out)
	}
	var logsIdx, stopIdx = -1, -1
	for i, c := range exec.commands {
		if logsIdx == -1 && strings.Contains(c, "podman logs") {
			logsIdx = i
		}
		if stopIdx == -1 && strings.Contains(c, "stop '"+unit+"'") {
			stopIdx = i
		}
	}
	if logsIdx == -1 || stopIdx == -1 || logsIdx > stopIdx {
		t.Fatalf("logs must be captured before the unit is stopped (logs=%d stop=%d):\n%s", logsIdx, stopIdx, strings.Join(exec.commands, "\n"))
	}
}
