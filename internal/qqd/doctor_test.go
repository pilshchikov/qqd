package qqd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// doctorMockExecutor implements Executor with configurable responses for doctor checks.
type doctorMockExecutor struct {
	id        string
	responses map[string]mockResponse
}

type mockResponse struct {
	output string
	err    error
}

func newDoctorMockExecutor(id string) *doctorMockExecutor {
	return &doctorMockExecutor{
		id:        id,
		responses: map[string]mockResponse{},
	}
}

func (m *doctorMockExecutor) Run(_ context.Context, cmd string) (string, error) {
	// Match by checking if the command contains a known key.
	for key, resp := range m.responses {
		if strings.Contains(cmd, key) {
			return resp.output, resp.err
		}
	}
	// Default: succeed with empty output.
	return "", nil
}

func (m *doctorMockExecutor) RunStream(ctx context.Context, cmd string, w io.Writer) error {
	out, err := m.Run(ctx, cmd)
	if out != "" {
		w.Write([]byte(out))
	}
	return err
}

func (m *doctorMockExecutor) CopyFrom(_ context.Context, _, _ string) error { return nil }
func (m *doctorMockExecutor) CopyTo(_ context.Context, _, _ string) error   { return nil }
func (m *doctorMockExecutor) Close() error                                  { return nil }
func (m *doctorMockExecutor) ID() string                                    { return m.id }

// doctorMockFactory implements ExecFactory for doctor tests.
type doctorMockFactory struct {
	executors map[string]*doctorMockExecutor
	failFor   map[string]error // target name -> error on ForTarget
}

func (f *doctorMockFactory) Local() Executor {
	return &doctorMockExecutor{id: "local"}
}

func (f *doctorMockFactory) ForTarget(t TargetConfig) (Executor, error) {
	if f.failFor != nil {
		if err, ok := f.failFor[t.Name]; ok {
			return nil, err
		}
	}
	if exec, ok := f.executors[t.Name]; ok {
		return exec, nil
	}
	return &doctorMockExecutor{id: t.Name}, nil
}

func (f *doctorMockFactory) ForBuildHost(_ BuildConfig) (Executor, error) {
	return &doctorMockExecutor{id: "build"}, nil
}

func baseDoctorConfig() ProjectConfig {
	return ProjectConfig{
		Name: "test-proj",
		Repo: "https://github.com/test/repo.git",
		Services: map[string]ServiceConfig{
			"server": {Image: "ghcr.io/test/server:1.0"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.40",
				User:    "deploy",
				RepoDir: "/home/deploy/repo",
			},
		},
	}
}

func healthyDoctorExecutor(id string) *doctorMockExecutor {
	exec := newDoctorMockExecutor(id)
	exec.responses["echo ok"] = mockResponse{output: "ok\n"}
	exec.responses["podman --version"] = mockResponse{output: "podman version 4.9.3\n"}
	exec.responses["systemctl --user is-system-running"] = mockResponse{output: "running\n"}
	exec.responses["test -d"] = mockResponse{output: ""}
	exec.responses["df -h"] = mockResponse{output: "/dev/sda1  50G  23G  28G  45% /\n"}
	exec.responses["loginctl"] = mockResponse{output: "Linger=yes\n"}
	return exec
}

func TestDoctorAllOK(t *testing.T) {
	exec := healthyDoctorExecutor("main")
	cfg := baseDoctorConfig()

	app := &App{
		ExecFactory: &doctorMockFactory{
			executors: map[string]*doctorMockExecutor{"main": exec},
		},
		Stdout: io.Discard,
	}
	if err := app.Doctor(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Doctor failed: %v", err)
	}
}

func TestDoctorAllOKOutput(t *testing.T) {
	exec := healthyDoctorExecutor("main")
	cfg := baseDoctorConfig()

	var out bytes.Buffer
	app := &App{
		ExecFactory: &doctorMockFactory{
			executors: map[string]*doctorMockExecutor{"main": exec},
		},
		Stdout: &out,
	}
	if err := app.Doctor(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Doctor failed: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "ssh connectivity:") {
		t.Fatalf("output should contain ssh connectivity check, got:\n%s", got)
	}
	if !strings.Contains(got, "podman:") {
		t.Fatalf("output should contain podman check, got:\n%s", got)
	}
	if !strings.Contains(got, "podman version 4.9.3") {
		t.Fatalf("output should contain podman version, got:\n%s", got)
	}
	if !strings.Contains(got, "45% used") {
		t.Fatalf("output should contain disk usage, got:\n%s", got)
	}
	if !strings.Contains(got, "target=main") {
		t.Fatalf("output should contain target name, got:\n%s", got)
	}
}

func TestDoctorPodmanMissing(t *testing.T) {
	exec := healthyDoctorExecutor("main")
	exec.responses["podman --version"] = mockResponse{
		output: "",
		err:    errors.New("command not found: podman"),
	}
	cfg := baseDoctorConfig()

	var out bytes.Buffer
	app := &App{
		ExecFactory: &doctorMockFactory{
			executors: map[string]*doctorMockExecutor{"main": exec},
		},
		Stdout: &out,
	}
	err := app.Doctor(context.Background(), cfg, "main")
	if err == nil {
		t.Fatal("Doctor should return error when podman is missing")
	}

	got := out.String()
	if !strings.Contains(got, "not installed") {
		t.Fatalf("output should indicate podman not installed, got:\n%s", got)
	}
}

func TestDoctorDiskSpaceWarning(t *testing.T) {
	exec := healthyDoctorExecutor("main")
	exec.responses["df -h"] = mockResponse{output: "/dev/sda1  50G  48G  2G  95% /\n"}
	cfg := baseDoctorConfig()

	var out bytes.Buffer
	app := &App{
		ExecFactory: &doctorMockFactory{
			executors: map[string]*doctorMockExecutor{"main": exec},
		},
		Stdout: &out,
	}
	if err := app.Doctor(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Doctor failed: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "95% used") {
		t.Fatalf("output should contain 95%% disk usage, got:\n%s", got)
	}
	if !strings.Contains(got, "warning") {
		t.Fatalf("output should contain warning for high disk usage, got:\n%s", got)
	}
}

func TestDoctorLingeringNotEnabled(t *testing.T) {
	exec := healthyDoctorExecutor("main")
	exec.responses["loginctl"] = mockResponse{output: "Linger=no\n"}
	cfg := baseDoctorConfig()

	var out bytes.Buffer
	app := &App{
		ExecFactory: &doctorMockFactory{
			executors: map[string]*doctorMockExecutor{"main": exec},
		},
		Stdout: &out,
	}
	if err := app.Doctor(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Doctor failed: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "not enabled") {
		t.Fatalf("output should indicate lingering not enabled, got:\n%s", got)
	}
	if !strings.Contains(got, "services may stop on logout") {
		t.Fatalf("output should explain lingering consequence, got:\n%s", got)
	}
}

func TestDoctorSSHFails(t *testing.T) {
	cfg := baseDoctorConfig()

	var out bytes.Buffer
	app := &App{
		ExecFactory: &doctorMockFactory{
			executors: map[string]*doctorMockExecutor{},
			failFor:   map[string]error{"main": errors.New("SSH connect failed: connection refused")},
		},
		Stdout: &out,
	}
	err := app.Doctor(context.Background(), cfg, "main")
	if err == nil {
		t.Fatal("Doctor should return error when SSH fails")
	}

	got := out.String()
	if !strings.Contains(got, "ssh connectivity:") {
		t.Fatalf("output should contain ssh connectivity error, got:\n%s", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("output should contain connection error detail, got:\n%s", got)
	}
}

func TestDoctorCommandDispatch(t *testing.T) {
	cfgContent := `
name = "test-proj"
repo = "https://github.com/test/repo.git"
services {
    server {
        image = "ghcr.io/test/server:1.0"
    }
}
targets {
    main {
        host = "local"
        user = "test"
        repo_dir = "/tmp/test"
    }
}
`
	cfgPath := writeTempConfig(t, cfgContent)
	var out bytes.Buffer
	err := Execute([]string{"doctor", "-c", cfgPath}, &out)
	// Doctor may return an error if local checks fail (no podman etc.),
	// but it should NOT be a usage error.
	if err != nil && strings.Contains(err.Error(), "usage:") {
		t.Fatalf("doctor should be recognized as a valid command, got usage error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "local checks") {
		t.Fatalf("output should contain local checks section, got:\n%s", got)
	}
	if !strings.Contains(got, "target=main") {
		t.Fatalf("output should contain target name, got:\n%s", got)
	}
}

func TestDoctorHelpFlag(t *testing.T) {
	var out bytes.Buffer
	if err := Execute([]string{"doctor", "--help"}, &out); err != nil {
		t.Fatalf("doctor --help failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "usage: qqd doctor") {
		t.Fatalf("doctor help should show usage, got: %q", got)
	}
}

func TestDoctorMultipleTargets(t *testing.T) {
	cfg := ProjectConfig{
		Name: "test-proj",
		Repo: "https://github.com/test/repo.git",
		Services: map[string]ServiceConfig{
			"server": {Image: "ghcr.io/test/server:1.0"},
		},
		Targets: map[string]TargetConfig{
			"staging": {
				Name:    "staging",
				Host:    "192.0.2.10",
				User:    "deploy",
				RepoDir: "/home/deploy/repo",
			},
			"prod": {
				Name:    "prod",
				Host:    "192.0.2.11",
				User:    "deploy",
				RepoDir: "/home/deploy/repo",
			},
		},
	}

	stagingExec := healthyDoctorExecutor("staging")
	prodExec := healthyDoctorExecutor("prod")

	var out bytes.Buffer
	app := &App{
		ExecFactory: &doctorMockFactory{
			executors: map[string]*doctorMockExecutor{
				"staging": stagingExec,
				"prod":    prodExec,
			},
		},
		Stdout: &out,
	}
	if err := app.Doctor(context.Background(), cfg, ""); err != nil {
		t.Fatalf("Doctor with multiple targets failed: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "target=staging") && !strings.Contains(got, "target=prod") {
		t.Fatalf("output should contain both targets, got:\n%s", got)
	}
}

func TestDoctorCheckIndependence(t *testing.T) {
	// Verify that one check failing doesn't prevent others from running.
	exec := newDoctorMockExecutor("main")
	exec.responses["echo ok"] = mockResponse{output: "ok\n"}
	exec.responses["podman --version"] = mockResponse{err: errors.New("not found")}
	exec.responses["systemctl --user is-system-running"] = mockResponse{output: "offline\n"}
	exec.responses["test -d"] = mockResponse{err: errors.New("not found")}
	exec.responses["df -h"] = mockResponse{err: errors.New("command failed")}
	exec.responses["loginctl"] = mockResponse{output: "Linger=no\n"}

	cfg := baseDoctorConfig()
	var out bytes.Buffer
	app := &App{
		ExecFactory: &doctorMockFactory{
			executors: map[string]*doctorMockExecutor{"main": exec},
		},
		Stdout: &out,
	}
	err := app.Doctor(context.Background(), cfg, "main")
	if err == nil {
		t.Fatal("Doctor should return error when checks have errors")
	}

	got := out.String()
	// All six checks should appear in the output even though most fail.
	checks := []string{"ssh connectivity:", "podman:", "systemd user:", "unit directory:", "disk space:", "lingering:"}
	for _, check := range checks {
		if !strings.Contains(got, check) {
			t.Fatalf("output should contain %q check even with failures, got:\n%s", check, got)
		}
	}
}

func TestDoctorReturnsErrorOnFailure(t *testing.T) {
	exec := healthyDoctorExecutor("main")
	exec.responses["podman --version"] = mockResponse{
		output: "",
		err:    errors.New("command not found: podman"),
	}
	cfg := baseDoctorConfig()

	app := &App{
		ExecFactory: &doctorMockFactory{
			executors: map[string]*doctorMockExecutor{"main": exec},
		},
		Stdout: io.Discard,
	}
	err := app.Doctor(context.Background(), cfg, "main")
	if err == nil {
		t.Fatal("Doctor should return error when checks have errors")
	}
	if !strings.Contains(err.Error(), "doctor found errors") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDoctorSystemdDegraded(t *testing.T) {
	exec := healthyDoctorExecutor("main")
	exec.responses["systemctl --user is-system-running"] = mockResponse{output: "degraded\n"}
	cfg := baseDoctorConfig()

	var out bytes.Buffer
	app := &App{
		ExecFactory: &doctorMockFactory{
			executors: map[string]*doctorMockExecutor{"main": exec},
		},
		Stdout: &out,
	}
	if err := app.Doctor(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Doctor should not return error for degraded systemd: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "some units failed") {
		t.Fatalf("output should mention degraded state, got:\n%s", got)
	}
}
