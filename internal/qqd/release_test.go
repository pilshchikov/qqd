package qqd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNewRelease(t *testing.T) {
	services := map[string]ServiceConfig{
		"web":    {Image: "ghcr.io/acme/web:1.0"},
		"worker": {Image: "ghcr.io/acme/worker:2.3"},
	}
	rel := newRelease(services)
	if rel.Services["web"] != "ghcr.io/acme/web:1.0" {
		t.Fatalf("web image = %q, want ghcr.io/acme/web:1.0", rel.Services["web"])
	}
	if rel.Services["worker"] != "ghcr.io/acme/worker:2.3" {
		t.Fatalf("worker image = %q, want ghcr.io/acme/worker:2.3", rel.Services["worker"])
	}
	if len(rel.Services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(rel.Services))
	}
	if rel.ID == "" {
		t.Fatal("release ID should not be empty")
	}
	if rel.Timestamp == "" {
		t.Fatal("release timestamp should not be empty")
	}
}

func TestReleaseIDFormat(t *testing.T) {
	id := releaseID()
	re := regexp.MustCompile(`^\d{8}-\d{6}\.\d{3}$`)
	if !re.MatchString(id) {
		t.Fatalf("release ID %q does not match YYYYMMDD-HHMMSS.mmm format", id)
	}
	// Verify it parses back to a valid time
	_, err := time.Parse("20060102-150405.000", id)
	if err != nil {
		t.Fatalf("release ID %q is not a valid timestamp: %v", id, err)
	}
}

func TestSaveAndListReleases(t *testing.T) {
	exec := newMockExecutor("test")
	ctx := context.Background()
	project := "myapp"

	// Save 3 releases with distinct IDs
	ids := []string{"20260101-100000", "20260101-100100", "20260101-100200"}
	for _, id := range ids {
		rel := Release{
			ID:        id,
			Timestamp: "2026-01-01T10:00:00Z",
			Services:  map[string]string{"web": "img:" + id},
		}
		if err := saveRelease(ctx, exec, project, rel); err != nil {
			t.Fatalf("saveRelease(%s) failed: %v", id, err)
		}
	}

	// List and verify order (newest first)
	releases, err := listReleases(ctx, exec, project)
	if err != nil {
		t.Fatalf("listReleases failed: %v", err)
	}
	if len(releases) != 3 {
		t.Fatalf("expected 3 releases, got %d", len(releases))
	}
	// Newest first
	if releases[0].ID != "20260101-100200" {
		t.Fatalf("releases[0].ID = %q, want 20260101-100200", releases[0].ID)
	}
	if releases[1].ID != "20260101-100100" {
		t.Fatalf("releases[1].ID = %q, want 20260101-100100", releases[1].ID)
	}
	if releases[2].ID != "20260101-100000" {
		t.Fatalf("releases[2].ID = %q, want 20260101-100000", releases[2].ID)
	}
	// Verify content
	if releases[0].Services["web"] != "img:20260101-100200" {
		t.Fatalf("releases[0] web image = %q", releases[0].Services["web"])
	}
}

func TestPreviousRelease(t *testing.T) {
	exec := newMockExecutor("test")
	ctx := context.Background()
	project := "myapp"

	// Save 3 releases
	for _, id := range []string{"20260101-100000", "20260101-100100", "20260101-100200"} {
		rel := Release{
			ID:        id,
			Timestamp: "2026-01-01T10:00:00Z",
			Services:  map[string]string{"web": "img:" + id},
		}
		if err := saveRelease(ctx, exec, project, rel); err != nil {
			t.Fatalf("saveRelease(%s) failed: %v", id, err)
		}
	}

	prev, err := previousRelease(ctx, exec, project)
	if err != nil {
		t.Fatalf("previousRelease failed: %v", err)
	}
	// Should return the second-newest
	if prev.ID != "20260101-100100" {
		t.Fatalf("previous release ID = %q, want 20260101-100100", prev.ID)
	}
}

func TestPreviousReleaseNotEnough(t *testing.T) {
	exec := newMockExecutor("test")
	ctx := context.Background()
	project := "myapp"

	// No releases
	_, err := previousRelease(ctx, exec, project)
	if err == nil {
		t.Fatal("expected error with 0 releases")
	}
	if !strings.Contains(err.Error(), "no previous release available") {
		t.Fatalf("wrong error: %v", err)
	}

	// Only 1 release
	rel := Release{
		ID:        "20260101-100000",
		Timestamp: "2026-01-01T10:00:00Z",
		Services:  map[string]string{"web": "img:1"},
	}
	if err := saveRelease(ctx, exec, project, rel); err != nil {
		t.Fatalf("saveRelease failed: %v", err)
	}
	_, err = previousRelease(ctx, exec, project)
	if err == nil {
		t.Fatal("expected error with 1 release")
	}
	if !strings.Contains(err.Error(), "no previous release available (found 1 releases)") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestTrimReleases(t *testing.T) {
	exec := newMockExecutor("test")
	ctx := context.Background()
	project := "myapp"

	// Save 12 releases
	for i := 0; i < 12; i++ {
		rel := Release{
			ID:        fmt.Sprintf("20260101-%06d", i),
			Timestamp: "2026-01-01T10:00:00Z",
			Services:  map[string]string{"web": fmt.Sprintf("img:%d", i)},
		}
		if err := saveRelease(ctx, exec, project, rel); err != nil {
			t.Fatalf("saveRelease(%d) failed: %v", i, err)
		}
	}

	// Verify 12 exist
	releases, _ := listReleases(ctx, exec, project)
	if len(releases) != 12 {
		t.Fatalf("expected 12 releases before trim, got %d", len(releases))
	}

	// Trim
	trimReleases(ctx, exec, project)

	// Verify only 10 remain (the newest 10)
	releases, _ = listReleases(ctx, exec, project)
	if len(releases) != 10 {
		t.Fatalf("expected 10 releases after trim, got %d", len(releases))
	}

	// The oldest 2 should be gone (IDs 000000 and 000001)
	for _, rel := range releases {
		if rel.ID == "20260101-000000" || rel.ID == "20260101-000001" {
			t.Fatalf("release %s should have been trimmed", rel.ID)
		}
	}
	// The newest should still be there
	if releases[0].ID != "20260101-000011" {
		t.Fatalf("newest release = %q, want 20260101-000011", releases[0].ID)
	}
}

func TestRollbackToPreviousRelease(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["ghcr.io/acme/web:1.0"] = true
	targetExec.existingImage["ghcr.io/acme/web:2.0"] = true
	targetExec.existingImage["postgres:16.1"] = true
	ctx := context.Background()

	cfg := ProjectConfig{
		Name:   "myapp",
		Repo:   "git@github.com:acme/app.git",
		Branch: "master",
		Build:  BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:2.0", Dockerfile: "Dockerfile"},
			"db":  {Image: "postgres:16.1"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/app/repo",
			},
		},
	}

	// Pre-populate two release records: v1 and v2
	rel1 := Release{
		ID:        "20260101-100000",
		Timestamp: "2026-01-01T10:00:00Z",
		Services:  map[string]string{"web": "ghcr.io/acme/web:1.0", "db": "postgres:16.1"},
	}
	rel2 := Release{
		ID:        "20260101-110000",
		Timestamp: "2026-01-01T11:00:00Z",
		Services:  map[string]string{"web": "ghcr.io/acme/web:2.0", "db": "postgres:16.1"},
	}
	saveRelease(ctx, targetExec, "myapp", rel1)
	saveRelease(ctx, targetExec, "myapp", rel2)

	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}

	// Full rollback
	if err := app.Rollback(ctx, cfg, "main", ""); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	// Verify a new release was saved with the rolled-back images
	releases, err := listReleases(ctx, targetExec, "myapp")
	if err != nil {
		t.Fatalf("listReleases failed: %v", err)
	}
	// Should have 3 releases now (rel1, rel2, and the rollback)
	if len(releases) < 3 {
		t.Fatalf("expected at least 3 releases after rollback, got %d", len(releases))
	}
	// The newest release should have the v1 web image
	newest := releases[0]
	if newest.Services["web"] != "ghcr.io/acme/web:1.0" {
		t.Fatalf("rollback release web image = %q, want ghcr.io/acme/web:1.0", newest.Services["web"])
	}
	if newest.Services["db"] != "postgres:16.1" {
		t.Fatalf("rollback release db image = %q, want postgres:16.1", newest.Services["db"])
	}
}

func TestRollbackSingleService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["ghcr.io/acme/web:1.0"] = true
	targetExec.existingImage["ghcr.io/acme/web:2.0"] = true
	targetExec.existingImage["ghcr.io/acme/worker:3.0"] = true
	ctx := context.Background()

	cfg := ProjectConfig{
		Name:   "myapp",
		Repo:   "git@github.com:acme/app.git",
		Branch: "master",
		Build:  BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web":    {Image: "ghcr.io/acme/web:2.0", Dockerfile: "Dockerfile"},
			"worker": {Image: "ghcr.io/acme/worker:3.0", Dockerfile: "Dockerfile.worker"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/app/repo",
			},
		},
	}

	// Two releases: v1 had web:1.0 + worker:3.0, v2 has web:2.0 + worker:3.0
	rel1 := Release{
		ID:        "20260101-100000",
		Timestamp: "2026-01-01T10:00:00Z",
		Services:  map[string]string{"web": "ghcr.io/acme/web:1.0", "worker": "ghcr.io/acme/worker:3.0"},
	}
	rel2 := Release{
		ID:        "20260101-110000",
		Timestamp: "2026-01-01T11:00:00Z",
		Services:  map[string]string{"web": "ghcr.io/acme/web:2.0", "worker": "ghcr.io/acme/worker:3.0"},
	}
	saveRelease(ctx, targetExec, "myapp", rel1)
	saveRelease(ctx, targetExec, "myapp", rel2)

	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}

	// Single service rollback for "web" only
	if err := app.Rollback(ctx, cfg, "main", "web"); err != nil {
		t.Fatalf("Rollback single service failed: %v", err)
	}

	// Verify the rollback release has web:1.0 but worker:3.0 (unchanged)
	releases, _ := listReleases(ctx, targetExec, "myapp")
	newest := releases[0]
	if newest.Services["web"] != "ghcr.io/acme/web:1.0" {
		t.Fatalf("rollback release web image = %q, want ghcr.io/acme/web:1.0", newest.Services["web"])
	}
	if newest.Services["worker"] != "ghcr.io/acme/worker:3.0" {
		t.Fatalf("rollback release worker image = %q, want ghcr.io/acme/worker:3.0", newest.Services["worker"])
	}
}

func TestRollbackNoHistory(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	ctx := context.Background()

	cfg := ProjectConfig{
		Name:   "myapp",
		Repo:   "git@github.com:acme/app.git",
		Branch: "master",
		Build:  BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/app/repo",
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

	err := app.Rollback(ctx, cfg, "main", "")
	if err == nil {
		t.Fatal("expected error when no releases exist")
	}
	if !strings.Contains(err.Error(), "no previous release available") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestHistoryOutput(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	ctx := context.Background()

	cfg := ProjectConfig{
		Name:   "myapp",
		Repo:   "git@github.com:acme/app.git",
		Branch: "master",
		Build:  BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/app/repo",
			},
		},
	}

	// Save two releases
	for _, id := range []string{"20260101-100000", "20260101-110000"} {
		rel := Release{
			ID:        id,
			Timestamp: "2026-01-01T10:00:00Z",
			Services:  map[string]string{"web": "ghcr.io/acme/web:" + id},
		}
		saveRelease(ctx, targetExec, "myapp", rel)
	}

	var out bytes.Buffer
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    &out,
		DrainWait: -1,
	}

	if err := app.History(ctx, cfg, "main"); err != nil {
		t.Fatalf("History failed: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "target=main") {
		t.Fatalf("history output missing target=main:\n%s", got)
	}
	if !strings.Contains(got, "releases=2") {
		t.Fatalf("history output missing releases=2:\n%s", got)
	}
	if !strings.Contains(got, "20260101-110000") {
		t.Fatalf("history output missing release ID 20260101-110000:\n%s", got)
	}
	if !strings.Contains(got, "20260101-100000") {
		t.Fatalf("history output missing release ID 20260101-100000:\n%s", got)
	}
	if !strings.Contains(got, "web") {
		t.Fatalf("history output missing service name 'web':\n%s", got)
	}
}

func TestHistoryCommandDispatch(t *testing.T) {
	// Write a minimal config file
	td := t.TempDir()
	confPath := filepath.Join(td, "qd.conf")
	confContent := `
name = myapp
repo = "git@github.com:acme/app.git"
branch = master
services {
  web {
    image = "ghcr.io/acme/web:1.0"
  }
}
targets {
  main {
    host = "192.0.2.20"
    user = centos
    repo_dir = "/home/centos/app/repo"
  }
}
`
	if err := os.WriteFile(confPath, []byte(confContent), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// The history command will try to SSH to the target, so it will fail,
	// but we just need to verify it dispatches correctly (doesn't return usage error).
	var out bytes.Buffer
	err := Execute([]string{"history", "-c", confPath, "-t", "main"}, &out)
	// It will fail because we can't actually SSH, but it should NOT be a usage error
	if err != nil && strings.Contains(err.Error(), "usage:") {
		t.Fatalf("history should be recognized as a valid command, got usage error: %v", err)
	}
}

func TestHistoryHelpFlag(t *testing.T) {
	var out bytes.Buffer
	if err := Execute([]string{"history", "--help"}, &out); err != nil {
		t.Fatalf("history --help failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "usage: qqd history") {
		t.Fatalf("history help not shown: %q", got)
	}
}

func TestRollbackAcceptsZeroOrOneService(t *testing.T) {
	// Verify rollback rejects more than one service
	var out bytes.Buffer
	td := t.TempDir()
	confPath := filepath.Join(td, "qd.conf")
	confContent := `
name = myapp
repo = "git@github.com:acme/app.git"
branch = master
services {
  web { image = "ghcr.io/acme/web:1.0" }
  worker { image = "ghcr.io/acme/worker:1.0" }
}
targets {
  main { host = "192.0.2.20"; user = centos; repo_dir = "/home/centos/app/repo" }
}
`
	if err := os.WriteFile(confPath, []byte(confContent), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := Execute([]string{"rollback", "-c", confPath, "-t", "main", "web", "worker"}, &out)
	if err == nil {
		t.Fatal("expected error when passing two services to rollback")
	}
	if !strings.Contains(err.Error(), "rollback accepts at most one service") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestDeploySavesRelease(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["postgres:16.1"] = true
	ctx := context.Background()

	cfg := ProjectConfig{
		Name:   "myapp",
		Repo:   "git@github.com:acme/app.git",
		Branch: "master",
		Build:  BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"db": {Image: "postgres:16.1"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    "192.0.2.20",
				User:    "centos",
				RepoDir: "/home/centos/app/repo",
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

	if err := app.Deploy(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	// Verify a release was saved
	releases, err := listReleases(ctx, targetExec, "myapp")
	if err != nil {
		t.Fatalf("listReleases failed: %v", err)
	}
	if len(releases) != 1 {
		t.Fatalf("expected 1 release after deploy, got %d", len(releases))
	}
	if releases[0].Services["db"] != "postgres:16.1" {
		t.Fatalf("release db image = %q, want postgres:16.1", releases[0].Services["db"])
	}
}

func TestDeployPartialMergesReleaseState(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.existingImage["ghcr.io/acme/server:1.1"] = true
	targetExec.existingImage["ghcr.io/acme/worker:1.0"] = true
	ctx := context.Background()

	saveRelease(ctx, targetExec, "proj", Release{
		ID:        "20260101-100000",
		Timestamp: "2026-01-01T10:00:00Z",
		Services: map[string]string{
			"server": "ghcr.io/acme/server:1.0",
			"worker": "ghcr.io/acme/worker:1.0",
		},
	})

	cfg := ProjectConfig{
		Name:  "proj",
		Repo:  "git@github.com:acme/proj.git",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"server": {Image: "ghcr.io/acme/server:1.1"},
			"worker": {Image: "ghcr.io/acme/worker:1.0"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "u", RepoDir: "/repo"},
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

	if err := app.Deploy(ctx, cfg, "main", []string{"server"}, false); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	releases, err := listReleases(ctx, targetExec, "proj")
	if err != nil {
		t.Fatalf("listReleases failed: %v", err)
	}
	if len(releases) < 2 {
		t.Fatalf("expected at least 2 releases, got %d", len(releases))
	}
	if releases[0].Services["server"] != "ghcr.io/acme/server:1.1" {
		t.Fatalf("new release server image = %q, want ghcr.io/acme/server:1.1", releases[0].Services["server"])
	}
	if releases[0].Services["worker"] != "ghcr.io/acme/worker:1.0" {
		t.Fatalf("new release worker image = %q, want ghcr.io/acme/worker:1.0", releases[0].Services["worker"])
	}
}

func TestSaveReleaseJSON(t *testing.T) {
	exec := newMockExecutor("test")
	ctx := context.Background()

	rel := Release{
		ID:        "20260301-120000",
		Timestamp: "2026-03-01T12:00:00Z",
		Services:  map[string]string{"web": "ghcr.io/acme/web:1.0"},
	}
	if err := saveRelease(ctx, exec, "myapp", rel); err != nil {
		t.Fatalf("saveRelease failed: %v", err)
	}

	// Read the saved file and verify it's valid JSON
	path := "~/.config/qqd/myapp/releases/20260301-120000.json"
	content, ok := exec.files[path]
	if !ok {
		t.Fatalf("release file not found at %s", path)
	}
	var parsed Release
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		t.Fatalf("saved release is not valid JSON: %v\ncontent: %s", err, content)
	}
	if parsed.ID != "20260301-120000" {
		t.Fatalf("parsed ID = %q, want 20260301-120000", parsed.ID)
	}
}
