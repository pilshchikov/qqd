package qqd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeHooks(t *testing.T) {
	raw := map[string]any{
		"pre_deploy":  "echo pre",
		"post_deploy": "echo post",
		"pre_build":   "echo prebuild",
		"post_build":  "echo postbuild",
	}
	h := decodeHooks(raw)
	if h.PreDeploy != "echo pre" {
		t.Fatalf("PreDeploy: got %q", h.PreDeploy)
	}
	if h.PostDeploy != "echo post" {
		t.Fatalf("PostDeploy: got %q", h.PostDeploy)
	}
	if h.PreBuild != "echo prebuild" {
		t.Fatalf("PreBuild: got %q", h.PreBuild)
	}
	if h.PostBuild != "echo postbuild" {
		t.Fatalf("PostBuild: got %q", h.PostBuild)
	}
}

func TestDecodeHooksEmpty(t *testing.T) {
	h := decodeHooks(nil)
	if h != (HooksConfig{}) {
		t.Fatalf("nil input should produce empty HooksConfig, got %+v", h)
	}
	h = decodeHooks("not a map")
	if h != (HooksConfig{}) {
		t.Fatalf("non-map input should produce empty HooksConfig, got %+v", h)
	}
}

func TestProjectHooksPreDeploy(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:   "myapp",
		Repo:   "git@github.com:test/repo.git",
		Branch: "main",
		Hooks: HooksConfig{
			PreDeploy: "echo 'starting deploy'",
		},
		Services: map[string]ServiceConfig{
			"server": {
				Image: "ghcr.io/test/server:1.0",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "local",
				RepoDir: "/opt/app",
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
	cmds := targetExec.commands
	// Find the pre_deploy hook command
	hookIdx := -1
	for i, cmd := range cmds {
		if cmd == "echo 'starting deploy'" {
			hookIdx = i
			break
		}
	}
	if hookIdx == -1 {
		t.Fatalf("pre_deploy hook command not found in executed commands:\n%s", strings.Join(cmds, "\n"))
	}
	// Verify the hook runs before any project-modifying work. The deploy's repo
	// dir mkdir is the first such step. Lock-related mkdirs under ~/.qqd/locks
	// run before everything else by design and are excluded.
	for i, cmd := range cmds {
		if i >= hookIdx {
			break
		}
		if strings.Contains(cmd, "mkdir") && strings.Contains(cmd, "/opt/app") {
			t.Fatalf("pre_deploy hook should run before repo dir mkdir, but mkdir was at index %d and hook at %d", i, hookIdx)
		}
	}
}

func TestProjectHooksPostDeploy(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:   "myapp",
		Repo:   "git@github.com:test/repo.git",
		Branch: "main",
		Hooks: HooksConfig{
			PostDeploy: "echo 'deploy complete'",
		},
		Services: map[string]ServiceConfig{
			"server": {
				Image: "ghcr.io/test/server:1.0",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "local",
				RepoDir: "/opt/app",
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
	cmds := targetExec.commands
	hookIdx := -1
	for i, cmd := range cmds {
		if cmd == "echo 'deploy complete'" {
			hookIdx = i
			break
		}
	}
	if hookIdx == -1 {
		t.Fatalf("post_deploy hook command not found in executed commands:\n%s", strings.Join(cmds, "\n"))
	}
	// Verify it runs after daemon-reload (which is one of the last deploy steps)
	daemonReloadIdx := -1
	for i, cmd := range cmds {
		if strings.Contains(cmd, "systemctl --user daemon-reload") {
			daemonReloadIdx = i
		}
	}
	if daemonReloadIdx == -1 {
		t.Fatalf("daemon-reload not found in commands")
	}
	if hookIdx < daemonReloadIdx {
		t.Fatalf("post_deploy hook (idx %d) should run after daemon-reload (idx %d)", hookIdx, daemonReloadIdx)
	}
}

func TestProjectHooksPreBuild(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:   "myapp",
		Repo:   "git@github.com:test/repo.git",
		Branch: "main",
		Hooks: HooksConfig{
			PreBuild: "echo 'project pre build'",
		},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/test/server:1.0",
				Dockerfile: "Dockerfile",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "local",
				RepoDir: "/opt/app",
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
	cmds := targetExec.commands
	hookIdx := -1
	buildIdx := -1
	for i, cmd := range cmds {
		if cmd == "echo 'project pre build'" {
			hookIdx = i
		}
		if strings.Contains(cmd, "podman build") {
			buildIdx = i
		}
	}
	if hookIdx == -1 {
		t.Fatalf("project pre_build hook not found in commands:\n%s", strings.Join(cmds, "\n"))
	}
	if buildIdx == -1 {
		t.Fatalf("podman build not found in commands:\n%s", strings.Join(cmds, "\n"))
	}
	if hookIdx > buildIdx {
		t.Fatalf("project pre_build hook (idx %d) should run before podman build (idx %d)", hookIdx, buildIdx)
	}
}

func TestServiceHooksPreBuild(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:   "myapp",
		Repo:   "git@github.com:test/repo.git",
		Branch: "main",
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/test/server:1.0",
				Dockerfile: "Dockerfile",
				Hooks: HooksConfig{
					PreBuild: "echo 'pre build server'",
				},
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "local",
				RepoDir: "/opt/app",
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
	cmds := targetExec.commands
	hookIdx := -1
	buildIdx := -1
	for i, cmd := range cmds {
		if cmd == "echo 'pre build server'" {
			hookIdx = i
		}
		if strings.Contains(cmd, "podman build") {
			buildIdx = i
		}
	}
	if hookIdx == -1 {
		t.Fatalf("pre_build hook not found in commands:\n%s", strings.Join(cmds, "\n"))
	}
	if buildIdx == -1 {
		t.Fatalf("podman build not found in commands:\n%s", strings.Join(cmds, "\n"))
	}
	if hookIdx > buildIdx {
		t.Fatalf("pre_build hook (idx %d) should run before podman build (idx %d)", hookIdx, buildIdx)
	}
}

func TestServiceHooksPostBuild(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:   "myapp",
		Repo:   "git@github.com:test/repo.git",
		Branch: "main",
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/test/server:1.0",
				Dockerfile: "Dockerfile",
				Hooks: HooksConfig{
					PostBuild: "echo 'post build server'",
				},
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "local",
				RepoDir: "/opt/app",
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
	cmds := targetExec.commands
	hookIdx := -1
	buildIdx := -1
	for i, cmd := range cmds {
		if cmd == "echo 'post build server'" {
			hookIdx = i
		}
		if strings.Contains(cmd, "podman build") {
			buildIdx = i
		}
	}
	if hookIdx == -1 {
		t.Fatalf("post_build hook not found in commands:\n%s", strings.Join(cmds, "\n"))
	}
	if buildIdx == -1 {
		t.Fatalf("podman build not found in commands:\n%s", strings.Join(cmds, "\n"))
	}
	if hookIdx < buildIdx {
		t.Fatalf("post_build hook (idx %d) should run after podman build (idx %d)", hookIdx, buildIdx)
	}
}

func TestHookFailureStopsDeploy(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:   "myapp",
		Repo:   "git@github.com:test/repo.git",
		Branch: "main",
		Hooks: HooksConfig{
			PreDeploy: "exit 1",
		},
		Services: map[string]ServiceConfig{
			"server": {
				Image: "ghcr.io/test/server:1.0",
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "local",
				RepoDir: "/opt/app",
			},
		},
	}

	// Make the executor fail on "exit 1"
	failExec := &hookFailExecutor{mock: targetExec, failCmd: "exit 1"}
	app := &App{
		ExecFactory: hookFailFactory{
			targets: map[string]Executor{"main": failExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	err := app.Deploy(context.Background(), cfg, "main", nil, false)
	if err == nil {
		t.Fatalf("Deploy should fail when pre_deploy hook fails")
	}
	if !strings.Contains(err.Error(), "hook pre_deploy project") {
		t.Fatalf("error should mention hook phase, got: %v", err)
	}
	// Verify no further commands were executed after the failed hook
	for _, cmd := range targetExec.commands {
		if strings.Contains(cmd, "podman") || strings.Contains(cmd, "systemctl") {
			t.Fatalf("no deploy commands should run after hook failure, but found: %s", cmd)
		}
	}
}

// hookFailExecutor wraps a mockExecutor but fails on a specific command.
type hookFailExecutor struct {
	mock    *mockExecutor
	failCmd string
}

func (e *hookFailExecutor) Run(ctx context.Context, cmd string) (string, error) {
	if cmd == e.failCmd {
		return "", errors.New("command failed")
	}
	return e.mock.Run(ctx, cmd)
}
func (e *hookFailExecutor) RunStream(ctx context.Context, cmd string, w io.Writer) error {
	if cmd == e.failCmd {
		return errors.New("command failed")
	}
	return e.mock.RunStream(ctx, cmd, w)
}
func (e *hookFailExecutor) CopyFrom(ctx context.Context, r, l string) error {
	return e.mock.CopyFrom(ctx, r, l)
}
func (e *hookFailExecutor) CopyTo(ctx context.Context, l, r string) error {
	return e.mock.CopyTo(ctx, l, r)
}
func (e *hookFailExecutor) Close() error { return nil }
func (e *hookFailExecutor) ID() string   { return e.mock.ID() }

type hookFailFactory struct {
	targets map[string]Executor
	local   Executor
}

func (f hookFailFactory) Local() Executor                              { return f.local }
func (f hookFailFactory) ForTarget(t TargetConfig) (Executor, error)   { return f.targets[t.Name], nil }
func (f hookFailFactory) ForBuildHost(b BuildConfig) (Executor, error) { return nil, nil }

func TestServiceHooksBuildHostStrategy(t *testing.T) {
	buildExec := newMockExecutor("build-host")
	targetExec := newMockExecutor("target-main")
	// Image does not exist on target, so build path is taken
	targetExec.existingImage["ghcr.io/test/server:1.0"] = false

	cfg := ProjectConfig{
		Name:   "myapp",
		Repo:   "git@github.com:test/repo.git",
		Branch: "main",
		Build: BuildConfig{
			Strategy: "build-host",
			Host:     "build.example.com",
			User:     "builder",
			RepoDir:  "/opt/builds",
			Delivery: "direct",
		},
		Services: map[string]ServiceConfig{
			"server": {
				Image:      "ghcr.io/test/server:1.0",
				Dockerfile: "Dockerfile",
				Hooks: HooksConfig{
					PreBuild:  "echo 'pre build on build host'",
					PostBuild: "echo 'post build on build host'",
				},
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.10",
				User:    "deploy",
				RepoDir: "/opt/app",
			},
		},
	}

	app := &App{
		ExecFactory: mockFactory{
			targets:   map[string]*mockExecutor{"main": targetExec},
			buildHost: map[string]*mockExecutor{"build.example.com": buildExec},
			local:     newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}

	if err := app.Build(context.Background(), cfg, "main", nil, true); err != nil {
		t.Fatalf("Build with build-host strategy failed: %v", err)
	}

	// Check that hooks were executed on the build executor
	buildCmds := buildExec.commands
	preBuildFound := false
	postBuildFound := false
	for _, cmd := range buildCmds {
		if cmd == "echo 'pre build on build host'" {
			preBuildFound = true
		}
		if cmd == "echo 'post build on build host'" {
			postBuildFound = true
		}
	}
	if !preBuildFound {
		t.Fatalf("pre_build hook not found on build executor commands:\n%s", strings.Join(buildCmds, "\n"))
	}
	if !postBuildFound {
		t.Fatalf("post_build hook not found on build executor commands:\n%s", strings.Join(buildCmds, "\n"))
	}
}

func TestServiceHooksGitHubActionsStrategy(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	localExec := newMockExecutor("local")

	cfg := ProjectConfig{
		Name:   "myapp",
		Repo:   "git@github.com:test/repo.git",
		Branch: "main",
		Build: BuildConfig{
			Strategy: "github-actions",
			Repo:     "test/repo",
			Workflow: "build.yml",
		},
		Services: map[string]ServiceConfig{
			"server": {
				Image: "ghcr.io/test/server:1.0",
				Hooks: HooksConfig{
					PreBuild:  "echo 'pre build on target'",
					PostBuild: "echo 'post build on target'",
				},
			},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.10",
				User:    "deploy",
				RepoDir: "/opt/app",
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
		t.Fatalf("Deploy with github-actions strategy failed: %v", err)
	}

	// Check that hooks were executed on the target executor
	targetCmds := targetExec.commands
	preBuildFound := false
	postBuildFound := false
	for _, cmd := range targetCmds {
		if cmd == "echo 'pre build on target'" {
			preBuildFound = true
		}
		if cmd == "echo 'post build on target'" {
			postBuildFound = true
		}
	}
	if !preBuildFound {
		t.Fatalf("pre_build hook not found on target executor commands:\n%s", strings.Join(targetCmds, "\n"))
	}
	if !postBuildFound {
		t.Fatalf("post_build hook not found on target executor commands:\n%s", strings.Join(targetCmds, "\n"))
	}
}

func TestHookConfigParsing(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "myapp"
repo = "https://github.com/test/repo.git"
hooks {
    pre_deploy = "echo 'starting deploy'"
    post_deploy = "echo 'deploy complete'"
}
services {
    server {
        image = "ghcr.io/test/server:1.0"
        hooks {
            pre_build = "echo 'building server'"
            post_build = "echo 'server built'"
        }
    }
}
targets {
    main {
        host = "local"
        repo_dir = "/opt/app"
    }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadProjectConfig([]string{cfgPath}, td)
	if err != nil {
		t.Fatalf("loadProjectConfig failed: %v", err)
	}
	// Verify project-level hooks
	if cfg.Hooks.PreDeploy != "echo 'starting deploy'" {
		t.Fatalf("project PreDeploy: got %q", cfg.Hooks.PreDeploy)
	}
	if cfg.Hooks.PostDeploy != "echo 'deploy complete'" {
		t.Fatalf("project PostDeploy: got %q", cfg.Hooks.PostDeploy)
	}
	// Verify service-level hooks
	server := cfg.Services["server"]
	if server.Hooks.PreBuild != "echo 'building server'" {
		t.Fatalf("server PreBuild: got %q", server.Hooks.PreBuild)
	}
	if server.Hooks.PostBuild != "echo 'server built'" {
		t.Fatalf("server PostBuild: got %q", server.Hooks.PostBuild)
	}
}
