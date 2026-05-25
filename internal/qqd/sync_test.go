package qqd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSyncUploadLocalHostUsesPlainRsync pins the regression where syncUpload
// always wrapped rsync in `-e 'ssh ...'` and addressed the destination as
// `user@local:/path`, which made ssh choke on the empty user and exit 255.
// For host == "local", rsync should run without -e and write to the local
// path directly.
func TestSyncUploadLocalHostUsesPlainRsync(t *testing.T) {
	tmp, err := os.MkdirTemp("", "qqd-sync-local-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	defer os.RemoveAll(tmp)

	cfg := ProjectConfig{
		Sync:         "upload",
		InvocationWD: tmp,
	}
	eff := EffectiveTarget{
		Target: TargetConfig{
			Name:    "alpha",
			Host:    "local",
			RepoDir: filepath.Join(tmp, "remote"),
		},
		Services: map[string]ServiceConfig{},
	}

	localExec := newMockExecutor("local")
	targetExec := newMockExecutor("target")
	app := &App{
		ExecFactory: mockFactory{
			local:   localExec,
			targets: map[string]*mockExecutor{"alpha": targetExec},
		},
		Stdout: io.Discard,
	}

	if err := app.syncUpload(context.Background(), cfg, eff, nil, targetExec); err != nil {
		t.Fatalf("syncUpload: %v", err)
	}

	if len(localExec.commands) == 0 {
		t.Fatal("expected rsync to run on the local executor")
	}
	rsyncCmd := localExec.commands[len(localExec.commands)-1]
	if !strings.HasPrefix(rsyncCmd, "rsync -az --delete") {
		t.Fatalf("expected rsync prefix, got: %s", rsyncCmd)
	}
	if strings.Contains(rsyncCmd, "-e '") || strings.Contains(rsyncCmd, "-e \"") {
		t.Fatalf("local-host upload must not wrap ssh via -e: %s", rsyncCmd)
	}
	if strings.Contains(rsyncCmd, "@local:") {
		t.Fatalf("local-host upload must not use user@local: destination: %s", rsyncCmd)
	}
	if !strings.Contains(rsyncCmd, eff.Target.RepoDir) {
		t.Fatalf("rsync destination should reference repo dir %q, got: %s", eff.Target.RepoDir, rsyncCmd)
	}
}

// TestSyncUploadRemoteHostStillUsesSSH guards the SSH path so the local-host
// branch above does not accidentally take over for real remote targets.
func TestSyncUploadRemoteHostStillUsesSSH(t *testing.T) {
	tmp, err := os.MkdirTemp("", "qqd-sync-remote-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	defer os.RemoveAll(tmp)

	cfg := ProjectConfig{
		Sync:         "upload",
		InvocationWD: tmp,
	}
	eff := EffectiveTarget{
		Target: TargetConfig{
			Name:    "prod",
			Host:    "192.0.2.30",
			User:    "deploy",
			SSHKey:  "~/.ssh/demo",
			RepoDir: "/srv/app",
		},
		Services: map[string]ServiceConfig{},
	}

	localExec := newMockExecutor("local")
	targetExec := newMockExecutor("target")
	app := &App{
		ExecFactory: mockFactory{
			local:   localExec,
			targets: map[string]*mockExecutor{"prod": targetExec},
		},
		Stdout: io.Discard,
	}

	if err := app.syncUpload(context.Background(), cfg, eff, nil, targetExec); err != nil {
		t.Fatalf("syncUpload: %v", err)
	}
	if len(localExec.commands) == 0 {
		t.Fatal("expected rsync to run on the local executor")
	}
	rsyncCmd := localExec.commands[len(localExec.commands)-1]
	if !strings.Contains(rsyncCmd, "-e 'ssh ") {
		t.Fatalf("remote upload must wrap rsync with -e ssh: %s", rsyncCmd)
	}
	if !strings.Contains(rsyncCmd, "deploy@192.0.2.30:/srv/app/") {
		t.Fatalf("remote upload should target deploy@192.0.2.30:/srv/app/, got: %s", rsyncCmd)
	}
}
