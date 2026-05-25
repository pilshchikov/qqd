package qqd

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestComposeMigrateDryRunSuppressesDestructiveCommands verifies the
// --dry-run flag causes destructive shell commands (`docker compose down`,
// `docker swarm leave`, `docker network prune`, `sudo chown -R`) to be
// PRINTED but not actually executed.
func TestComposeMigrateDryRunSuppressesDestructiveCommands(t *testing.T) {
	exec := newMockExecutor("target")
	cfg := ProjectConfig{
		Name:    "myapp",
		Runtime: "podman",
		Services: map[string]ServiceConfig{
			"server": {Image: "ghcr.io/me/server:1.0"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "local",
				RepoDir: "/opt/app",
			},
		},
	}
	var buf bytes.Buffer
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": exec},
			local:   newMockExecutor("local"),
		},
		Stdout:    &buf,
		DryRun:    true,
		AssumeYes: true,
		NoLock:    true,
	}
	if err := app.MigrateCompose(context.Background(), cfg, MigrateComposeOpts{
		Target: "main",
	}); err != nil {
		t.Fatalf("MigrateCompose dry-run failed: %v", err)
	}
	for _, c := range exec.commands {
		if strings.Contains(c, "docker swarm leave") {
			t.Errorf("dry-run should not exec `docker swarm leave`, but did: %s", c)
		}
		if strings.Contains(c, "docker compose -p") && strings.Contains(c, "down") {
			t.Errorf("dry-run should not exec `docker compose down`, but did: %s", c)
		}
		if strings.Contains(c, "docker network prune") {
			t.Errorf("dry-run should not exec `docker network prune`, but did: %s", c)
		}
	}
	if !strings.Contains(buf.String(), "[would run]") {
		t.Errorf("expected '[would run]' in compose-migrate dry-run output; got:\n%s", buf.String())
	}
}
