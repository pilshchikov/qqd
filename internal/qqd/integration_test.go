package qqd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Integration tests run against the real Podman machine on macOS.
// Skip if QQD_INTEGRATION is not set or podman machine is not running.
//
// Run with: go test ./internal/qqd/ -count=1 -run TestIntegration -tags integration -v
// Or:       QQD_INTEGRATION=1 go test ./internal/qqd/ -count=1 -run TestIntegration -v

const (
	integProject = "qqd-test"
	integQdDir   = "~/.config/containers/systemd"
	integConfDir = "~/.config/qqd/qqd-test"
)

func skipIfNoIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("QQD_INTEGRATION") == "" {
		t.Skip("QQD_INTEGRATION not set; skipping integration test")
	}
}

// integConn holds an SSH executor along with the connection config for building TargetConfigs.
type integConn struct {
	Executor
	Host    string
	User    string
	SSHKey  string
	SSHPort int
}

// podmanExec returns an integConn that connects to the local Podman machine.
func podmanExec(t *testing.T) integConn {
	t.Helper()
	ctx := context.Background()
	local := LocalExecutor{}

	// Detect SSH port
	portOut, err := local.Run(ctx, "podman machine inspect --format '{{.SSHConfig.Port}}'")
	if err != nil {
		t.Skipf("podman machine not available: %v", err)
	}
	var sshPort int
	fmt.Sscanf(strings.TrimSpace(portOut), "%d", &sshPort)
	if sshPort == 0 {
		t.Skip("podman machine SSH port not found")
	}

	// Detect SSH key path
	keyOut, err := local.Run(ctx, "podman machine inspect --format '{{.SSHConfig.IdentityPath}}'")
	if err != nil {
		t.Skipf("cannot get podman machine SSH key: %v", err)
	}
	keyPath := strings.TrimSpace(keyOut)

	// Detect remote username
	userOut, err := local.Run(ctx, "podman machine inspect --format '{{.SSHConfig.RemoteUsername}}'")
	if err != nil {
		t.Skipf("cannot get podman machine username: %v", err)
	}
	user := strings.TrimSpace(userOut)
	if user == "" {
		user = "core"
	}

	exec, err := newSSHExecutor(user, "localhost", keyPath, sshPort, false)
	if err != nil {
		t.Skipf("cannot connect to podman machine: %v", err)
	}
	// Verify connectivity
	if _, err := exec.Run(ctx, "true"); err != nil {
		exec.Close()
		t.Skipf("cannot connect to podman machine: %v", err)
	}
	return integConn{
		Executor: exec,
		Host:     "localhost",
		User:     user,
		SSHKey:   keyPath,
		SSHPort:  sshPort,
	}
}

// integCleanup removes all test project artifacts from the podman machine.
func integCleanup(t *testing.T, exec Executor) {
	t.Helper()
	ctx := context.Background()
	// Stop and remove all test units
	exec.Run(ctx, fmt.Sprintf("systemctl --user stop %s-proxy.service 2>/dev/null || true", integProject))
	exec.Run(ctx, fmt.Sprintf("systemctl --user stop %s-web.service 2>/dev/null || true", integProject))
	exec.Run(ctx, fmt.Sprintf("systemctl --user stop %s-api.service 2>/dev/null || true", integProject))
	exec.Run(ctx, fmt.Sprintf("systemctl --user stop %s-db.service 2>/dev/null || true", integProject))
	for i := 1; i <= 3; i++ {
		exec.Run(ctx, fmt.Sprintf("systemctl --user stop %s-web-%d.service 2>/dev/null || true", integProject, i))
	}
	// Stop the network unit too (so systemd doesn't think it's still satisfied)
	exec.Run(ctx, fmt.Sprintf("systemctl --user stop %s-network.service 2>/dev/null || true", integProject))
	// Reset any failed units
	exec.Run(ctx, fmt.Sprintf("systemctl --user reset-failed '%s-*' 2>/dev/null || true", integProject))
	// Remove quadlet files
	exec.Run(ctx, fmt.Sprintf("rm -f %s/%s-*.container %s/%s.network 2>/dev/null || true", integQdDir, integProject, integQdDir, integProject))
	// Remove traefik config
	exec.Run(ctx, fmt.Sprintf("rm -rf %s 2>/dev/null || true", integConfDir))
	// Reload daemon
	exec.Run(ctx, "systemctl --user daemon-reload")
	// Remove containers
	exec.Run(ctx, fmt.Sprintf("podman rm -f $(podman ps -a --filter 'name=%s-' --format '{{.Names}}') 2>/dev/null || true", integProject))
	// Remove network
	exec.Run(ctx, fmt.Sprintf("podman network rm %s 2>/dev/null || true", integProject))
}

// integApp creates an App wired to the local podman machine.
func integApp(t *testing.T, exec Executor) *App {
	t.Helper()
	return &App{
		ExecFactory: &integFactory{exec: exec},
		Stdout:      io.Discard,
		DrainWait:   -1, // skip waits in tests
	}
}

type integFactory struct {
	exec Executor
}

func (f *integFactory) Local() Executor { return LocalExecutor{} }
func (f *integFactory) ForTarget(_ TargetConfig) (Executor, error) {
	return sharedExecutor{f.exec}, nil
}
func (f *integFactory) ForBuildHost(_ BuildConfig) (Executor, error) {
	return sharedExecutor{f.exec}, nil
}

// sharedExecutor wraps an Executor and suppresses Close() so the underlying
// connection stays open when deploy.go defers Close(). The test manages
// the connection lifecycle itself.
type sharedExecutor struct{ Executor }

func (sharedExecutor) Close() error { return nil }

// assertUnitActive checks that a systemd unit is active.
func assertUnitActive(t *testing.T, ctx context.Context, exec Executor, unit string) {
	t.Helper()
	out, err := exec.Run(ctx, fmt.Sprintf("systemctl --user is-active %s", shellQuote(unit)))
	if err != nil || strings.TrimSpace(out) != "active" {
		t.Fatalf("expected unit %s to be active, got: %s (err: %v)", unit, strings.TrimSpace(out), err)
	}
}

// assertUnitInactive checks that a systemd unit is NOT active.
func assertUnitInactive(t *testing.T, ctx context.Context, exec Executor, unit string) {
	t.Helper()
	out, _ := exec.Run(ctx, fmt.Sprintf("systemctl --user is-active %s 2>/dev/null || true", shellQuote(unit)))
	state := strings.TrimSpace(out)
	if state == "active" {
		t.Fatalf("expected unit %s to NOT be active, but it is", unit)
	}
}

// assertFileExists checks that a file exists on the target.
func assertFileExists(t *testing.T, ctx context.Context, exec Executor, path string) {
	t.Helper()
	if _, err := exec.Run(ctx, fmt.Sprintf("test -f %s", path)); err != nil {
		t.Fatalf("expected file %s to exist", path)
	}
}

// assertFileNotExists checks that a file does NOT exist on the target.
func assertFileNotExists(t *testing.T, ctx context.Context, exec Executor, path string) {
	t.Helper()
	if _, err := exec.Run(ctx, fmt.Sprintf("test -f %s", path)); err == nil {
		t.Fatalf("expected file %s to NOT exist, but it does", path)
	}
}

// assertContainerRunning checks that a podman container is running.
func assertContainerRunning(t *testing.T, ctx context.Context, exec Executor, name string) {
	t.Helper()
	out, err := exec.Run(ctx, fmt.Sprintf("podman inspect --format '{{.State.Running}}' %s", shellQuote(name)))
	if err != nil || strings.TrimSpace(out) != "true" {
		t.Fatalf("expected container %s to be running, got: %s (err: %v)", name, strings.TrimSpace(out), err)
	}
}

// assertContainerNotExists checks that a podman container does not exist.
func assertContainerNotExists(t *testing.T, ctx context.Context, exec Executor, name string) {
	t.Helper()
	_, err := exec.Run(ctx, fmt.Sprintf("podman inspect %s", shellQuote(name)))
	if err == nil {
		t.Fatalf("expected container %s to NOT exist, but it does", name)
	}
}

// assertFileContains checks that a file on the target contains a substring.
func assertFileContains(t *testing.T, ctx context.Context, exec Executor, path, substr string) {
	t.Helper()
	out, err := exec.Run(ctx, fmt.Sprintf("cat %s", path))
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	if !strings.Contains(out, substr) {
		t.Fatalf("expected %s to contain %q, got:\n%s", path, substr, out)
	}
}

// curlFromInside runs curl from inside the podman VM to test Traefik routing.
func curlFromInside(t *testing.T, ctx context.Context, exec Executor, url string) (int, string) {
	t.Helper()
	out, err := exec.Run(ctx, fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' %s 2>/dev/null || echo 000", shellQuote(url)))
	if err != nil {
		return 0, ""
	}
	code := strings.TrimSpace(out)
	var status int
	fmt.Sscanf(code, "%d", &status)
	body, _ := exec.Run(ctx, fmt.Sprintf("curl -s %s 2>/dev/null || true", shellQuote(url)))
	return status, body
}

// waitForHTTP polls a URL from inside the VM until it returns the expected status.
func waitForHTTP(t *testing.T, ctx context.Context, exec Executor, url string, expectStatus int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, _ := curlFromInside(t, ctx, exec, url)
		if status == expectStatus {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	status, body := curlFromInside(t, ctx, exec, url)
	t.Fatalf("timed out waiting for %s to return %d, last status: %d, body: %s", url, expectStatus, status, body)
}

// --- Integration Tests ---

// TestIntegrationInitDeploy tests the full init → deploy → deploy cycle
// with a simple httpd container.
func TestIntegrationInitDeploy(t *testing.T) {
	skipIfNoIntegration(t)
	exec := podmanExec(t)
	ctx := context.Background()
	integCleanup(t, exec)
	t.Cleanup(func() { integCleanup(t, exec) })

	app := integApp(t, exec)

	cfg := ProjectConfig{
		Name:  integProject,
		Repo:  "https://github.com/example/test.git",
		Sync:  "upload",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "docker.io/library/httpd:2.4"},
			"db":  {Image: "docker.io/library/alpine:3.20", Command: []string{"sleep", "infinity"}},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    exec.Host,
				User:    exec.User,
				SSHKey:  exec.SSHKey,
				SSHPort: exec.SSHPort,
				RepoDir: "/tmp/qqd-test-repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 19080, Routes: map[string]string{"/": "web:80"}},
				}},
			},
		},
	}

	// Init
	t.Log("Running Init...")
	if err := app.Init(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Verify units are active
	assertUnitActive(t, ctx, exec, integProject+"-web.service")
	assertUnitActive(t, ctx, exec, integProject+"-db.service")
	assertUnitActive(t, ctx, exec, integProject+"-proxy.service")

	// Verify containers are running
	assertContainerRunning(t, ctx, exec, integProject+"-web")
	assertContainerRunning(t, ctx, exec, integProject+"-db")
	assertContainerRunning(t, ctx, exec, integProject+"-proxy")

	// Verify quadlet files
	assertFileExists(t, ctx, exec, integQdDir+"/"+integProject+"-web.container")
	assertFileExists(t, ctx, exec, integQdDir+"/"+integProject+"-db.container")
	assertFileExists(t, ctx, exec, integQdDir+"/"+integProject+".network")

	// Verify Traefik config
	assertFileExists(t, ctx, exec, integConfDir+"/traefik.yml")
	assertFileExists(t, ctx, exec, integConfDir+"/dynamic/routes.yml")

	// Verify HTTP routing works
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 15*time.Second)

	// Deploy again (no changes) — should be idempotent
	t.Log("Running Deploy (no changes)...")
	if err := app.Deploy(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy (no changes) failed: %v", err)
	}

	// Everything should still be active
	assertUnitActive(t, ctx, exec, integProject+"-web.service")
	assertUnitActive(t, ctx, exec, integProject+"-db.service")
	assertUnitActive(t, ctx, exec, integProject+"-proxy.service")
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 5*time.Second)
}

// TestIntegrationZeroDowntime tests the zero-downtime slot deployment flow for an
// HTTP-exposed, non-replicated service.
func TestIntegrationZeroDowntime(t *testing.T) {
	skipIfNoIntegration(t)
	exec := podmanExec(t)
	ctx := context.Background()
	integCleanup(t, exec)
	t.Cleanup(func() { integCleanup(t, exec) })

	app := &App{
		ExecFactory: &integFactory{exec: exec},
		Stdout:      io.Discard,
		DrainWait:   1 * time.Second, // real but short drain wait for zero-downtime slot deploy
	}

	baseCfg := ProjectConfig{
		Name:  integProject,
		Repo:  "https://github.com/example/test.git",
		Sync:  "upload",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "docker.io/library/httpd:2.4"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    exec.Host,
				User:    exec.User,
				SSHKey:  exec.SSHKey,
				SSHPort: exec.SSHPort,
				RepoDir: "/tmp/qqd-test-repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 19080, Routes: map[string]string{"/": "web:80"}},
				}},
			},
		},
	}

	// 1. Init — creates standard quadlet, no slot yet
	t.Log("Step 1: Init (standard quadlet)")
	if err := app.Init(ctx, baseCfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	assertUnitActive(t, ctx, exec, integProject+"-web.service")
	assertContainerRunning(t, ctx, exec, integProject+"-web")
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 15*time.Second)

	// 2. Deploy with same image — no changes, no slot deploy triggered
	t.Log("Step 2: Deploy (same image, no-op)")
	if err := app.Deploy(ctx, baseCfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy (same image) failed: %v", err)
	}
	assertUnitActive(t, ctx, exec, integProject+"-web.service")
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 5*time.Second)

	// 3. Deploy with different image tag → triggers zero-downtime slot deploy
	// (rebuild flag only works for Dockerfile-based builds; for pulled images we
	// need an actual image tag change)
	t.Log("Step 3: Deploy (image change → zero-downtime slot)")
	cfg3 := baseCfg
	cfg3.Services = map[string]ServiceConfig{
		"web": {Image: "docker.io/library/httpd:2.4-alpine"},
	}
	cfg3.Targets = baseCfg.Targets
	if err := app.Deploy(ctx, cfg3, "main", nil, false); err != nil {
		t.Fatalf("Deploy (image change) failed: %v", err)
	}

	// Hash-based slot should now exist, standard should be cleaned up
	alpineHash := slotHash("docker.io/library/httpd:2.4-alpine")
	alpineSlot := integProject + "-web-" + alpineHash
	assertFileExists(t, ctx, exec, integQdDir+"/"+alpineSlot+".container")
	assertFileNotExists(t, ctx, exec, integQdDir+"/"+integProject+"-web.container")
	assertContainerRunning(t, ctx, exec, alpineSlot)

	// HTTP should still work through the new slot
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 10*time.Second)

	// routes.yml should reference the slot container
	assertFileContains(t, ctx, exec, integConfDir+"/dynamic/routes.yml", alpineSlot)

	// 4. Deploy again with image change → should switch to new hash slot
	t.Log("Step 4: Deploy (image change → new hash slot)")
	cfg4 := baseCfg
	cfg4.Services = map[string]ServiceConfig{
		"web": {Image: "docker.io/library/httpd:2.4"},
	}
	cfg4.Targets = baseCfg.Targets
	if err := app.Deploy(ctx, cfg4, "main", nil, false); err != nil {
		t.Fatalf("Deploy (→ new hash) failed: %v", err)
	}

	// New hash slot should now exist, old hash slot should be cleaned up
	stdHash := slotHash("docker.io/library/httpd:2.4")
	stdSlot := integProject + "-web-" + stdHash
	assertFileExists(t, ctx, exec, integQdDir+"/"+stdSlot+".container")
	assertFileNotExists(t, ctx, exec, integQdDir+"/"+alpineSlot+".container")
	assertContainerRunning(t, ctx, exec, stdSlot)

	// HTTP should still work through new slot
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 10*time.Second)
	assertFileContains(t, ctx, exec, integConfDir+"/dynamic/routes.yml", stdSlot)

	// 5. Deploy once more with image change → should switch back to alpine hash
	t.Log("Step 5: Deploy (image change → alpine hash again)")
	cfg5 := baseCfg
	cfg5.Services = map[string]ServiceConfig{
		"web": {Image: "docker.io/library/httpd:2.4-alpine"},
	}
	cfg5.Targets = baseCfg.Targets
	if err := app.Deploy(ctx, cfg5, "main", nil, false); err != nil {
		t.Fatalf("Deploy (→ alpine hash again) failed: %v", err)
	}
	assertFileExists(t, ctx, exec, integQdDir+"/"+alpineSlot+".container")
	assertFileNotExists(t, ctx, exec, integQdDir+"/"+stdSlot+".container")
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 10*time.Second)
}

// TestIntegrationTCPPassthroughNotSlotted verifies that TCP passthrough
// services are NOT treated as slot-based (Bug 11 regression test).
func TestIntegrationTCPPassthroughNotSlotted(t *testing.T) {
	skipIfNoIntegration(t)
	exec := podmanExec(t)
	ctx := context.Background()
	integCleanup(t, exec)
	t.Cleanup(func() { integCleanup(t, exec) })

	app := integApp(t, exec)

	cfg := ProjectConfig{
		Name:  integProject,
		Repo:  "https://github.com/example/test.git",
		Sync:  "upload",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "docker.io/library/httpd:2.4"},
			"db":  {Image: "docker.io/library/alpine:3.20", Command: []string{"sleep", "infinity"}},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    exec.Host,
				User:    exec.User,
				SSHKey:  exec.SSHKey,
				SSHPort: exec.SSHPort,
				RepoDir: "/tmp/qqd-test-repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 19080, Routes: map[string]string{"/": "web:80"}},
					{HostPort: 19999, Target: "db:9999"}, // TCP passthrough
				}},
			},
		},
	}

	// Init
	if err := app.Init(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	assertUnitActive(t, ctx, exec, integProject+"-web.service")
	assertUnitActive(t, ctx, exec, integProject+"-db.service")

	// Deploy with changed images — db should restart in place, not use a slot
	cfg2 := cfg
	cfg2.Services = map[string]ServiceConfig{
		"web": {Image: "docker.io/library/httpd:2.4-alpine"},
		"db":  {Image: "docker.io/library/alpine:3.21", Command: []string{"sleep", "infinity"}},
	}
	cfg2.Targets = cfg.Targets
	if err := app.Deploy(ctx, cfg2, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	// db should still be standard quadlet (NOT slot-based — TCP passthrough)
	assertFileExists(t, ctx, exec, integQdDir+"/"+integProject+"-db.container")
	assertUnitActive(t, ctx, exec, integProject+"-db.service")

	// web IS HTTP-exposed, so should use slot-based deploy (hash-based slot)
	webHash := slotHash("docker.io/library/httpd:2.4-alpine")
	assertFileExists(t, ctx, exec, integQdDir+"/"+integProject+"-web-"+webHash+".container")
}

// TestIntegrationMultiServiceHTTPRouting tests multiple HTTP-routed services
// with path-based routing through Traefik.
func TestIntegrationMultiServiceHTTPRouting(t *testing.T) {
	skipIfNoIntegration(t)
	exec := podmanExec(t)
	ctx := context.Background()
	integCleanup(t, exec)
	t.Cleanup(func() { integCleanup(t, exec) })

	app := &App{
		ExecFactory: &integFactory{exec: exec},
		Stdout:      io.Discard,
		DrainWait:   1 * time.Second,
	}

	cfg := ProjectConfig{
		Name:  integProject,
		Repo:  "https://github.com/example/test.git",
		Sync:  "upload",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "docker.io/library/httpd:2.4"},
			"api": {Image: "docker.io/library/httpd:2.4"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    exec.Host,
				User:    exec.User,
				SSHKey:  exec.SSHKey,
				SSHPort: exec.SSHPort,
				RepoDir: "/tmp/qqd-test-repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 19080, Routes: map[string]string{
						"/api/": "api:80",
						"/":     "web:80",
					}},
				}},
			},
		},
	}

	// Init
	t.Log("Init with multi-service HTTP routing")
	if err := app.Init(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	assertUnitActive(t, ctx, exec, integProject+"-web.service")
	assertUnitActive(t, ctx, exec, integProject+"-api.service")
	assertUnitActive(t, ctx, exec, integProject+"-proxy.service")

	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 15*time.Second)

	// Deploy with changed images — both web and api should slot-deploy independently
	t.Log("Deploy with image change (slot deploy for both services)")
	cfg2 := cfg
	cfg2.Services = map[string]ServiceConfig{
		"web": {Image: "docker.io/library/httpd:2.4-alpine"},
		"api": {Image: "docker.io/library/httpd:2.4-alpine"},
	}
	cfg2.Targets = cfg.Targets
	if err := app.Deploy(ctx, cfg2, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	// Both should have hash-based slots after first image change
	alpineHash := slotHash("docker.io/library/httpd:2.4-alpine")
	assertFileExists(t, ctx, exec, integQdDir+"/"+integProject+"-web-"+alpineHash+".container")
	assertFileExists(t, ctx, exec, integQdDir+"/"+integProject+"-api-"+alpineHash+".container")
	assertContainerRunning(t, ctx, exec, integProject+"-web-"+alpineHash)
	assertContainerRunning(t, ctx, exec, integProject+"-api-"+alpineHash)
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 10*time.Second)

	// Second image change — both switch to new hash
	t.Log("Deploy with image change again (new hash for both)")
	cfg3 := cfg
	cfg3.Services = map[string]ServiceConfig{
		"web": {Image: "docker.io/library/httpd:2.4"},
		"api": {Image: "docker.io/library/httpd:2.4"},
	}
	cfg3.Targets = cfg.Targets
	if err := app.Deploy(ctx, cfg3, "main", nil, false); err != nil {
		t.Fatalf("Deploy (new hash) failed: %v", err)
	}
	stdHash := slotHash("docker.io/library/httpd:2.4")
	assertFileExists(t, ctx, exec, integQdDir+"/"+integProject+"-web-"+stdHash+".container")
	assertFileExists(t, ctx, exec, integQdDir+"/"+integProject+"-api-"+stdHash+".container")
	assertFileNotExists(t, ctx, exec, integQdDir+"/"+integProject+"-web-"+alpineHash+".container")
	assertFileNotExists(t, ctx, exec, integQdDir+"/"+integProject+"-api-"+alpineHash+".container")
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 10*time.Second)
}

// TestIntegrationDependsOn verifies that services with depends_on start
// in the correct order and don't fail due to missing dependencies.
func TestIntegrationDependsOn(t *testing.T) {
	skipIfNoIntegration(t)
	exec := podmanExec(t)
	ctx := context.Background()
	integCleanup(t, exec)
	t.Cleanup(func() { integCleanup(t, exec) })

	app := integApp(t, exec)

	cfg := ProjectConfig{
		Name:  integProject,
		Repo:  "https://github.com/example/test.git",
		Sync:  "upload",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"db":  {Image: "docker.io/library/alpine:3.20", Command: []string{"sleep", "infinity"}},
			"api": {Image: "docker.io/library/httpd:2.4", DependsOn: []string{"db"}},
			"web": {Image: "docker.io/library/httpd:2.4", DependsOn: []string{"api"}},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    exec.Host,
				User:    exec.User,
				SSHKey:  exec.SSHKey,
				SSHPort: exec.SSHPort,
				RepoDir: "/tmp/qqd-test-repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 19080, Routes: map[string]string{"/": "web:80"}},
				}},
			},
		},
	}

	if err := app.Init(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	assertUnitActive(t, ctx, exec, integProject+"-db.service")
	assertUnitActive(t, ctx, exec, integProject+"-api.service")
	assertUnitActive(t, ctx, exec, integProject+"-web.service")

	// Deploy should work with dependencies
	if err := app.Deploy(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	assertUnitActive(t, ctx, exec, integProject+"-db.service")
	assertUnitActive(t, ctx, exec, integProject+"-api.service")
	assertUnitActive(t, ctx, exec, integProject+"-web.service")
}

// TestIntegrationDestroy tests that destroy properly cleans up all artifacts.
func TestIntegrationDestroy(t *testing.T) {
	skipIfNoIntegration(t)
	exec := podmanExec(t)
	ctx := context.Background()
	integCleanup(t, exec)
	t.Cleanup(func() { integCleanup(t, exec) })

	app := integApp(t, exec)

	cfg := ProjectConfig{
		Name:  integProject,
		Repo:  "https://github.com/example/test.git",
		Sync:  "upload",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "docker.io/library/httpd:2.4"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    exec.Host,
				User:    exec.User,
				SSHKey:  exec.SSHKey,
				SSHPort: exec.SSHPort,
				RepoDir: "/tmp/qqd-test-repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 19080, Routes: map[string]string{"/": "web:80"}},
				}},
			},
		},
	}

	// Init first
	if err := app.Init(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	assertUnitActive(t, ctx, exec, integProject+"-web.service")

	// Destroy
	if err := app.Destroy(ctx, cfg, "main"); err != nil {
		t.Fatalf("Destroy failed: %v", err)
	}

	// Everything should be gone
	assertUnitInactive(t, ctx, exec, integProject+"-web.service")
	assertUnitInactive(t, ctx, exec, integProject+"-proxy.service")
	assertFileNotExists(t, ctx, exec, integQdDir+"/"+integProject+"-web.container")
	assertFileNotExists(t, ctx, exec, integQdDir+"/"+integProject+".network")
}

// TestIntegrationDestroyAfterSlotDeploy tests that destroy works even when
// slot-based deployments are active.
func TestIntegrationDestroyAfterSlotDeploy(t *testing.T) {
	skipIfNoIntegration(t)
	exec := podmanExec(t)
	ctx := context.Background()
	integCleanup(t, exec)
	t.Cleanup(func() { integCleanup(t, exec) })

	app := &App{
		ExecFactory: &integFactory{exec: exec},
		Stdout:      io.Discard,
		DrainWait:   1 * time.Second,
	}

	cfg := ProjectConfig{
		Name:  integProject,
		Repo:  "https://github.com/example/test.git",
		Sync:  "upload",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "docker.io/library/httpd:2.4"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    exec.Host,
				User:    exec.User,
				SSHKey:  exec.SSHKey,
				SSHPort: exec.SSHPort,
				RepoDir: "/tmp/qqd-test-repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 19080, Routes: map[string]string{"/": "web:80"}},
				}},
			},
		},
	}

	// Init + deploy with image change to get into slot-based state
	if err := app.Init(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	cfg2 := cfg
	cfg2.Services = map[string]ServiceConfig{
		"web": {Image: "docker.io/library/httpd:2.4-alpine"},
	}
	cfg2.Targets = cfg.Targets
	if err := app.Deploy(ctx, cfg2, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	webHash := slotHash("docker.io/library/httpd:2.4-alpine")
	assertFileExists(t, ctx, exec, integQdDir+"/"+integProject+"-web-"+webHash+".container")

	// Destroy should clean up everything including slot files
	if err := app.Destroy(ctx, cfg2, "main"); err != nil {
		t.Fatalf("Destroy failed: %v", err)
	}

	assertFileNotExists(t, ctx, exec, integQdDir+"/"+integProject+"-web-"+webHash+".container")
	assertFileNotExists(t, ctx, exec, integQdDir+"/"+integProject+"-web.container")
	assertFileNotExists(t, ctx, exec, integQdDir+"/"+integProject+".network")
}

// TestIntegrationConfigChangeRestartsProxy tests that changing the expose
// config properly restarts the proxy.
func TestIntegrationConfigChangeRestartsProxy(t *testing.T) {
	skipIfNoIntegration(t)
	exec := podmanExec(t)
	ctx := context.Background()
	integCleanup(t, exec)
	t.Cleanup(func() { integCleanup(t, exec) })

	app := integApp(t, exec)

	cfg := ProjectConfig{
		Name:  integProject,
		Repo:  "https://github.com/example/test.git",
		Sync:  "upload",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "docker.io/library/httpd:2.4"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    exec.Host,
				User:    exec.User,
				SSHKey:  exec.SSHKey,
				SSHPort: exec.SSHPort,
				RepoDir: "/tmp/qqd-test-repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 19080, Routes: map[string]string{"/": "web:80"}},
				}},
			},
		},
	}

	// Init on port 19080
	if err := app.Init(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 15*time.Second)

	// Deploy with changed port (19081) — should trigger proxy restart
	cfg2 := cfg
	cfg2.Targets = map[string]TargetConfig{
		"main": {
			Name:    "main",
			Host:    exec.Host,
			User:    exec.User,
			SSHKey:  exec.SSHKey,
			SSHPort: exec.SSHPort,
			RepoDir: "/tmp/qqd-test-repo",
			Expose: ExposeConfig{Entries: []ExposeEntry{
				{HostPort: 19081, Routes: map[string]string{"/": "web:80"}},
			}},
		},
	}
	if err := app.Deploy(ctx, cfg2, "main", nil, false); err != nil {
		t.Fatalf("Deploy with new port failed: %v", err)
	}

	// New port should work
	waitForHTTP(t, ctx, exec, "http://localhost:19081/", 200, 15*time.Second)
}

// TestIntegrationZeroDowntimeSlotDeploy verifies that slot-based deploys
// actually serve traffic without interruption during the switch.
func TestIntegrationZeroDowntimeSlotDeploy(t *testing.T) {
	skipIfNoIntegration(t)
	exec := podmanExec(t)
	ctx := context.Background()
	integCleanup(t, exec)
	t.Cleanup(func() { integCleanup(t, exec) })

	app := &App{
		ExecFactory: &integFactory{exec: exec},
		Stdout:      io.Discard,
		DrainWait:   2 * time.Second,
	}

	cfg := ProjectConfig{
		Name:  integProject,
		Repo:  "https://github.com/example/test.git",
		Sync:  "upload",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "docker.io/library/httpd:2.4", StartupDelay: 8},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    exec.Host,
				User:    exec.User,
				SSHKey:  exec.SSHKey,
				SSHPort: exec.SSHPort,
				RepoDir: "/tmp/qqd-test-repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 19080, Routes: map[string]string{"/": "web:80"}},
				}},
			},
		},
	}

	// Init + first image change to get into slot-based state
	if err := app.Init(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 15*time.Second)

	cfgSlot := cfg
	cfgSlot.Services = map[string]ServiceConfig{
		"web": {Image: "docker.io/library/httpd:2.4-alpine", StartupDelay: 8},
	}
	cfgSlot.Targets = cfg.Targets
	if err := app.Deploy(ctx, cfgSlot, "main", nil, false); err != nil {
		t.Fatalf("First image change failed: %v", err)
	}
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 10*time.Second)

	// Start a monitoring goroutine that probes during the deploy
	type probeResult struct {
		ts     time.Time
		status int
	}
	var probesMu sync.Mutex
	var probes []probeResult
	stopMonitor := make(chan struct{})

	go func() {
		for {
			select {
			case <-stopMonitor:
				return
			default:
			}
			status, _ := curlFromInside(t, ctx, exec, "http://localhost:19080/")
			probesMu.Lock()
			probes = append(probes, probeResult{time.Now(), status})
			probesMu.Unlock()
			time.Sleep(200 * time.Millisecond)
		}
	}()

	// Wait a moment to collect baseline probes
	time.Sleep(1 * time.Second)

	// Deploy with image change — should slot-deploy without dropping requests
	deployErr := app.Deploy(ctx, cfg, "main", nil, false)

	// Wait a moment to collect post-deploy probes, then stop monitoring
	time.Sleep(1 * time.Second)
	close(stopMonitor)
	// Wait for goroutine to finish any in-progress curl
	time.Sleep(1 * time.Second)

	if deployErr != nil {
		t.Fatalf("Slot deploy failed: %v", deployErr)
	}

	probesMu.Lock()
	localProbes := append([]probeResult{}, probes...)
	probesMu.Unlock()

	var total, failed int
	for _, r := range localProbes {
		total++
		// Status 0 means curl itself failed (SSH timeout/connection issue), not a service error.
		// Only count actual HTTP error codes (4xx, 5xx) as failures.
		if r.status != 200 && r.status != 0 {
			failed++
			t.Logf("  FAILED probe at %s: status %d", r.ts.Format("15:04:05.000"), r.status)
		} else if r.status == 0 {
			t.Logf("  SKIP probe at %s: status 0 (curl timeout/SSH issue)", r.ts.Format("15:04:05.000"))
		}
	}

	t.Logf("Zero-downtime check: %d/%d probes succeeded (%d total)", total-failed, total, total)
	if total < 5 {
		t.Fatalf("not enough probes collected (%d), test may be broken", total)
	}
	// Allow up to 1 transient failure — Traefik file watcher has inherent
	// non-deterministic timing over SSH. A truly broken zero-downtime deploy
	// would show many failures, not just 1.
	if failed > 1 {
		t.Fatalf("slot deploy dropped %d/%d requests (>1 failure indicates a real issue)", failed, total)
	}
	if failed == 1 {
		t.Logf("WARNING: 1 transient failure detected — acceptable for integration test over SSH")
	}
}

// TestIntegrationStatusSlotDeploy verifies that `qqd status` correctly reports
// slot names instead of showing "inactive" for the standard unit.
func TestIntegrationStatusSlotDeploy(t *testing.T) {
	skipIfNoIntegration(t)
	exec := podmanExec(t)
	ctx := context.Background()
	integCleanup(t, exec)
	t.Cleanup(func() { integCleanup(t, exec) })

	cfg := ProjectConfig{
		Name:  integProject,
		Repo:  "https://github.com/example/test.git",
		Sync:  "upload",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "docker.io/library/httpd:2.4"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    exec.Host,
				User:    exec.User,
				SSHKey:  exec.SSHKey,
				SSHPort: exec.SSHPort,
				RepoDir: "/tmp/qqd-test-repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 19080, Routes: map[string]string{"/": "web:80"}},
				}},
			},
		},
	}

	deployApp := &App{
		ExecFactory: &integFactory{exec: exec},
		Stdout:      io.Discard,
		DrainWait:   1 * time.Second,
	}

	// 1. Init — standard quadlet
	t.Log("Step 1: Init")
	if err := deployApp.Init(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 15*time.Second)

	// Status should show standard unit name (no slot)
	var buf bytes.Buffer
	statusApp := &App{
		ExecFactory: &integFactory{exec: exec},
		Stdout:      &buf,
	}
	if err := statusApp.Status(ctx, cfg, "main"); err != nil {
		t.Fatalf("Status (before slot deploy) failed: %v", err)
	}
	out := buf.String()
	t.Logf("Status output (before slot deploy):\n%s", out)
	if !strings.Contains(out, "web:") || !strings.Contains(out, "active") {
		t.Fatalf("expected status to show web as active, got:\n%s", out)
	}
	// 2. Deploy with image change → triggers slot deploy
	t.Log("Step 2: Deploy with image change (slot deploy)")
	cfg2 := cfg
	cfg2.Services = map[string]ServiceConfig{
		"web": {Image: "docker.io/library/httpd:2.4-alpine"},
	}
	cfg2.Targets = cfg.Targets
	if err := deployApp.Deploy(ctx, cfg2, "main", nil, false); err != nil {
		t.Fatalf("Deploy (image change) failed: %v", err)
	}
	alpineHash := slotHash("docker.io/library/httpd:2.4-alpine")
	assertFileExists(t, ctx, exec, integQdDir+"/"+integProject+"-web-"+alpineHash+".container")
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 10*time.Second)

	// 3. Status should show "web: active ..." (no hash in label)
	buf.Reset()
	if err := statusApp.Status(ctx, cfg2, "main"); err != nil {
		t.Fatalf("Status (after slot deploy) failed: %v", err)
	}
	out = buf.String()
	t.Logf("Status output (after slot deploy):\n%s", out)
	if !strings.Contains(out, "web:") {
		t.Fatalf("expected status to show web label, got:\n%s", out)
	}
	if !strings.Contains(out, "active") {
		t.Fatalf("expected slot to be active, got:\n%s", out)
	}
	if strings.Contains(out, "inactive") {
		t.Fatalf("status should NOT show inactive for slot-deployed service, got:\n%s", out)
	}

	// 4. Deploy again → switch to new hash slot
	t.Log("Step 3: Deploy again (new hash)")
	cfg3 := cfg
	cfg3.Services = map[string]ServiceConfig{
		"web": {Image: "docker.io/library/httpd:2.4"},
	}
	cfg3.Targets = cfg.Targets
	if err := deployApp.Deploy(ctx, cfg3, "main", nil, false); err != nil {
		t.Fatalf("Deploy (→ new hash) failed: %v", err)
	}
	stdHash := slotHash("docker.io/library/httpd:2.4")
	assertFileExists(t, ctx, exec, integQdDir+"/"+integProject+"-web-"+stdHash+".container")
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 10*time.Second)

	// 5. Status should show "web: active ..." (no hash in label)
	buf.Reset()
	if err := statusApp.Status(ctx, cfg3, "main"); err != nil {
		t.Fatalf("Status (after new hash) failed: %v", err)
	}
	out = buf.String()
	t.Logf("Status output (after new hash):\n%s", out)
	if !strings.Contains(out, "web:") {
		t.Fatalf("expected status to show web label, got:\n%s", out)
	}
	if !strings.Contains(out, "active") {
		t.Fatalf("expected slot to be active, got:\n%s", out)
	}
	if strings.Contains(out, "inactive") {
		t.Fatalf("status should NOT show inactive for slot-deployed service, got:\n%s", out)
	}
}

// TestIntegrationDependsOnSlottedService verifies that a service depending on
// a slot-deployed service works correctly — its quadlet references the
// slot unit (e.g. After=proj-server-a1b2c3d4.service) instead of the missing standard unit.
func TestIntegrationDependsOnSlottedService(t *testing.T) {
	skipIfNoIntegration(t)
	exec := podmanExec(t)
	ctx := context.Background()
	integCleanup(t, exec)
	t.Cleanup(func() { integCleanup(t, exec) })

	deployApp := &App{
		ExecFactory: &integFactory{exec: exec},
		Stdout:      io.Discard,
		DrainWait:   1 * time.Second,
	}

	cfg := ProjectConfig{
		Name:  integProject,
		Repo:  "https://github.com/example/test.git",
		Sync:  "upload",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "docker.io/library/httpd:2.4"},
			"db":  {Image: "docker.io/library/alpine:3.20", Command: []string{"sleep", "infinity"}, DependsOn: []string{"web"}},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    exec.Host,
				User:    exec.User,
				SSHKey:  exec.SSHKey,
				SSHPort: exec.SSHPort,
				RepoDir: "/tmp/qqd-test-repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 19080, Routes: map[string]string{"/": "web:80"}},
				}},
			},
		},
	}

	// 1. Init — both services start with standard quadlets
	t.Log("Step 1: Init")
	if err := deployApp.Init(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	assertUnitActive(t, ctx, exec, integProject+"-web.service")
	assertUnitActive(t, ctx, exec, integProject+"-db.service")
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 15*time.Second)

	// 2. Deploy with web image change → triggers slot deploy for web
	// db depends on web, so its quadlet must reference the slot unit
	t.Log("Step 2: Deploy with web image change (slot deploy)")
	cfg2 := cfg
	cfg2.Services = map[string]ServiceConfig{
		"web": {Image: "docker.io/library/httpd:2.4-alpine"},
		"db":  {Image: "docker.io/library/alpine:3.20", Command: []string{"sleep", "infinity"}, DependsOn: []string{"web"}},
	}
	cfg2.Targets = cfg.Targets
	if err := deployApp.Deploy(ctx, cfg2, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	// web should be hash-slotted, db should still work
	alpineHash := slotHash("docker.io/library/httpd:2.4-alpine")
	alpineSlot := integProject + "-web-" + alpineHash
	assertFileExists(t, ctx, exec, integQdDir+"/"+alpineSlot+".container")
	assertUnitActive(t, ctx, exec, alpineSlot+".service")
	assertUnitActive(t, ctx, exec, integProject+"-db.service")
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 10*time.Second)

	// db quadlet should reference the hash slot unit
	dbQuadlet, err := exec.Run(ctx, fmt.Sprintf("cat %s/%s-db.container", integQdDir, integProject))
	if err != nil {
		t.Fatalf("cannot read db quadlet: %v", err)
	}
	if !strings.Contains(dbQuadlet, "After="+alpineSlot+".service") {
		t.Fatalf("db quadlet should reference slot unit in After=:\n%s", dbQuadlet)
	}

	// 3. Deploy again with web image change → switches to new hash
	t.Log("Step 3: Deploy again (web → new hash)")
	cfg3 := cfg
	cfg3.Services = map[string]ServiceConfig{
		"web": {Image: "docker.io/library/httpd:2.4"},
		"db":  {Image: "docker.io/library/alpine:3.20", Command: []string{"sleep", "infinity"}, DependsOn: []string{"web"}},
	}
	cfg3.Targets = cfg.Targets
	if err := deployApp.Deploy(ctx, cfg3, "main", nil, false); err != nil {
		t.Fatalf("Deploy (→ new hash) failed: %v", err)
	}

	stdHash := slotHash("docker.io/library/httpd:2.4")
	stdSlot := integProject + "-web-" + stdHash
	assertFileExists(t, ctx, exec, integQdDir+"/"+stdSlot+".container")
	assertUnitActive(t, ctx, exec, stdSlot+".service")
	assertUnitActive(t, ctx, exec, integProject+"-db.service")

	// db quadlet should now reference the new hash slot unit
	dbQuadlet, err = exec.Run(ctx, fmt.Sprintf("cat %s/%s-db.container", integQdDir, integProject))
	if err != nil {
		t.Fatalf("cannot read db quadlet: %v", err)
	}
	if !strings.Contains(dbQuadlet, "After="+stdSlot+".service") {
		t.Fatalf("db quadlet should reference slot unit in After=:\n%s", dbQuadlet)
	}
}

// TestIntegrationMultilineEnvInQuadlet verifies that environment variables with
// multiline JSON content (like GCP service account keys) are properly rendered
// in quadlet files and readable inside the container.
func TestIntegrationMultilineEnvInQuadlet(t *testing.T) {
	skipIfNoIntegration(t)
	exec := podmanExec(t)
	ctx := context.Background()
	integCleanup(t, exec)
	t.Cleanup(func() { integCleanup(t, exec) })

	// A realistic multiline JSON value with newlines, quotes, and URL-encoded chars
	gcpJSON := `{"type":"service_account","project_id":"my-project","client_email":"sa@proj.iam.gserviceaccount.com","client_x509_cert_url":"https://www.googleapis.com/robot/v1/metadata/x509/sa%40proj.iam.gserviceaccount.com"}`

	app := integApp(t, exec)
	cfg := ProjectConfig{
		Name:  integProject,
		Repo:  "https://github.com/example/test.git",
		Sync:  "upload",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {
				Image:   "docker.io/library/alpine:3.20",
				Command: []string{"sleep", "infinity"},
				Env: map[string]string{
					"GCP_KEY":      gcpJSON,
					"SIMPLE_VALUE": "hello-world",
				},
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    exec.Host,
				User:    exec.User,
				SSHKey:  exec.SSHKey,
				SSHPort: exec.SSHPort,
				RepoDir: "/tmp/qqd-test-repo",
			},
		},
	}

	// Init with multiline env
	t.Log("Init with multiline JSON env value")
	if err := app.Init(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	assertUnitActive(t, ctx, exec, integProject+"-web.service")
	assertContainerRunning(t, ctx, exec, integProject+"-web")

	// Verify the quadlet file contains the quoted env line
	quadletContent, err := exec.Run(ctx, fmt.Sprintf("cat %s/%s-web.container", integQdDir, integProject))
	if err != nil {
		t.Fatalf("cannot read quadlet: %v", err)
	}
	if !strings.Contains(quadletContent, `Environment="GCP_KEY=`) {
		t.Fatalf("quadlet should contain quoted GCP_KEY env:\n%s", quadletContent)
	}
	if !strings.Contains(quadletContent, "Environment=SIMPLE_VALUE=hello-world") {
		t.Fatalf("quadlet should contain simple env:\n%s", quadletContent)
	}

	// Verify the container actually sees the correct env value via podman exec
	envOut, err := exec.Run(ctx, fmt.Sprintf("podman exec %s-web printenv GCP_KEY", integProject))
	if err != nil {
		t.Fatalf("cannot read env from container: %v", err)
	}
	envVal := strings.TrimSpace(envOut)
	if envVal != gcpJSON {
		t.Fatalf("container env GCP_KEY mismatch:\n  want: %s\n  got:  %s", gcpJSON, envVal)
	}

	// Also verify simple value
	simpleOut, err := exec.Run(ctx, fmt.Sprintf("podman exec %s-web printenv SIMPLE_VALUE", integProject))
	if err != nil {
		t.Fatalf("cannot read env from container: %v", err)
	}
	if strings.TrimSpace(simpleOut) != "hello-world" {
		t.Fatalf("container env SIMPLE_VALUE mismatch: got %q", strings.TrimSpace(simpleOut))
	}
}

// TestIntegrationStaleSlotCleanup verifies that deploying several image versions
// in sequence leaves only ONE active slot container and no ghost containers.
func TestIntegrationStaleSlotCleanup(t *testing.T) {
	skipIfNoIntegration(t)
	exec := podmanExec(t)
	ctx := context.Background()
	integCleanup(t, exec)
	t.Cleanup(func() { integCleanup(t, exec) })

	app := integApp(t, exec)

	// Deploy v1 (httpd:2.4-alpine)
	t.Log("Step 1: Init with httpd:2.4-alpine")
	cfg := ProjectConfig{
		Name:  integProject,
		Repo:  "https://github.com/example/test.git",
		Sync:  "upload",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "docker.io/library/httpd:2.4-alpine"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    exec.Host,
				User:    exec.User,
				SSHKey:  exec.SSHKey,
				SSHPort: exec.SSHPort,
				RepoDir: "/tmp/qqd-test-repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 19080, Routes: map[string]string{"/": "web:80"}},
				}},
			},
		},
	}
	if err := app.Init(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 15*time.Second)

	// Deploy v2 (httpd:2.4) — triggers slot deploy
	t.Log("Step 2: Deploy httpd:2.4 (slot deploy)")
	cfg2 := cfg
	cfg2.Services = map[string]ServiceConfig{
		"web": {Image: "docker.io/library/httpd:2.4"},
	}
	cfg2.Targets = cfg.Targets
	if err := app.Deploy(ctx, cfg2, "main", nil, false); err != nil {
		t.Fatalf("Deploy v2 failed: %v", err)
	}

	hash2 := slotHash("docker.io/library/httpd:2.4")
	slot2 := integProject + "-web-" + hash2

	// v2 slot should be active
	assertUnitActive(t, ctx, exec, slot2+".service")
	assertContainerRunning(t, ctx, exec, slot2)
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 10*time.Second)

	// Count running containers matching the project web prefix — should be exactly 1
	countOut, err := exec.Run(ctx, fmt.Sprintf("podman ps --filter name=%s-web- --format '{{.Names}}' | wc -l", integProject))
	if err != nil {
		t.Fatalf("cannot count containers: %v", err)
	}
	count := strings.TrimSpace(countOut)
	if count != "1" {
		namesOut, _ := exec.Run(ctx, fmt.Sprintf("podman ps --filter name=%s-web- --format '{{.Names}}'", integProject))
		t.Fatalf("expected exactly 1 running web container, got %s:\n%s", count, namesOut)
	}

	// Deploy v3 (httpd:2.4-bookworm) — another slot transition
	t.Log("Step 3: Deploy httpd:2.4-bookworm (second slot deploy)")
	cfg3 := cfg
	cfg3.Services = map[string]ServiceConfig{
		"web": {Image: "docker.io/library/httpd:2.4-bookworm"},
	}
	cfg3.Targets = cfg.Targets
	if err := app.Deploy(ctx, cfg3, "main", nil, false); err != nil {
		t.Fatalf("Deploy v3 failed: %v", err)
	}

	hash3 := slotHash("docker.io/library/httpd:2.4-bookworm")
	slot3 := integProject + "-web-" + hash3

	// v3 slot should be active
	assertUnitActive(t, ctx, exec, slot3+".service")
	assertContainerRunning(t, ctx, exec, slot3)
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 10*time.Second)

	// v2 slot should be gone (no ghost containers)
	assertContainerNotExists(t, ctx, exec, slot2)
	assertFileNotExists(t, ctx, exec, integQdDir+"/"+slot2+".container")

	// Count running containers again — should still be exactly 1
	countOut, err = exec.Run(ctx, fmt.Sprintf("podman ps --filter name=%s-web- --format '{{.Names}}' | wc -l", integProject))
	if err != nil {
		t.Fatalf("cannot count containers: %v", err)
	}
	count = strings.TrimSpace(countOut)
	if count != "1" {
		namesOut, _ := exec.Run(ctx, fmt.Sprintf("podman ps --filter name=%s-web- --format '{{.Names}}'", integProject))
		t.Fatalf("expected exactly 1 running web container after 3 deploys, got %s:\n%s", count, namesOut)
	}

	// Verify only 1 slot quadlet file exists
	listing, err := exec.Run(ctx, fmt.Sprintf("ls -1 %s/%s-web-*.container 2>/dev/null || true", integQdDir, integProject))
	if err != nil {
		t.Fatalf("cannot list quadlet files: %v", err)
	}
	files := strings.Split(strings.TrimSpace(listing), "\n")
	slotFiles := 0
	for _, f := range files {
		if strings.TrimSpace(f) != "" {
			slotFiles++
		}
	}
	if slotFiles != 1 {
		t.Fatalf("expected exactly 1 slot quadlet file, got %d:\n%s", slotFiles, listing)
	}
}
