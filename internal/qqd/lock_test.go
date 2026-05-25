package qqd

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// All lock tests reuse the project-wide mockExecutor (defined in deploy_test.go).
// We control branching via failCmds for the contention test.

func TestAcquireLockFirstHolderSucceeds(t *testing.T) {
	exec := newMockExecutor("target")
	release, err := acquireLock(context.Background(), exec, "myapp", "prod", "deploy", false)
	if err != nil {
		t.Fatalf("first acquire should succeed, got: %v", err)
	}
	if release == nil {
		t.Fatal("expected release function, got nil")
	}
	// Releasing should not panic and should run a `rm -rf` for the lock dir.
	release()
	found := false
	for _, c := range exec.commands {
		if strings.HasPrefix(c, "rm -rf") && strings.Contains(c, "myapp.lock") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("release should issue rm -rf on the lock dir; commands were:\n%s",
			strings.Join(exec.commands, "\n"))
	}
}

func TestAcquireLockSecondHolderBlocked(t *testing.T) {
	exec := newMockExecutor("target")
	// Pre-seed the lock dir so the second mkdir fails.
	exec.failCmds = map[string]int{"mkdir " + projectLockDir("myapp") + " 2>/dev/null": 1}
	exec.files = map[string]string{
		projectLockDir("myapp") + "/meta": "command:    deploy\nproject:    myapp\ntarget:     prod\npid:        1234\nlocal_user: alice\nlocal_host: laptop\nstarted_at: 2026-04-25T12:00:00Z\n",
	}
	_, err := acquireLock(context.Background(), exec, "myapp", "prod", "deploy", false)
	if err == nil {
		t.Fatal("expected lock contention error")
	}
	var le *LockedError
	if !errors.As(err, &le) {
		t.Fatalf("expected *LockedError, got %T: %v", err, err)
	}
	if le.Project != "myapp" || le.Target != "prod" {
		t.Fatalf("unexpected metadata: %+v", le)
	}
	if !strings.Contains(le.Error(), "deploy lock held") {
		t.Fatalf("error should mention 'deploy lock held', got: %s", le.Error())
	}
	if !strings.Contains(le.Error(), "--force-unlock") {
		t.Fatalf("error should suggest --force-unlock, got: %s", le.Error())
	}
}

func TestAcquireLockForceUnlockClearsBeforeMkdir(t *testing.T) {
	// With force=true, acquireLock should issue an `rm -rf <lockdir>` before
	// attempting the atomic mkdir. We don't need to simulate a real prior
	// holder; we only need to verify the *sequence* of remote commands.
	exec := newMockExecutor("target")
	release, err := acquireLock(context.Background(), exec, "myapp", "prod", "deploy", true)
	if err != nil {
		t.Fatalf("force-unlock acquire should succeed on a free lock, got: %v", err)
	}
	defer release()
	rmIdx, mkIdx := -1, -1
	for i, c := range exec.commands {
		if rmIdx == -1 && strings.HasPrefix(c, "rm -rf") && strings.Contains(c, "myapp.lock") {
			rmIdx = i
		}
		if strings.HasPrefix(c, "mkdir ") && strings.Contains(c, "myapp.lock") && !strings.Contains(c, "-p") {
			mkIdx = i
		}
	}
	if rmIdx == -1 {
		t.Fatalf("force-unlock should issue rm -rf on the lock dir; commands:\n%s", strings.Join(exec.commands, "\n"))
	}
	if mkIdx == -1 {
		t.Fatalf("force-unlock should still attempt mkdir after rm; commands:\n%s", strings.Join(exec.commands, "\n"))
	}
	if rmIdx >= mkIdx {
		t.Fatalf("force rm should run before mkdir; rmIdx=%d mkIdx=%d", rmIdx, mkIdx)
	}
}

func TestParseLockMetaRoundTrip(t *testing.T) {
	in := buildLockMeta("deploy", "myapp", "prod")
	in.PID = 999
	in.LocalUser = "alice"
	in.LocalHost = "laptop"
	out := parseLockMeta(formatLockMeta(in))
	if out.Command != in.Command || out.Project != in.Project || out.Target != in.Target {
		t.Fatalf("round-trip mismatch: in=%+v out=%+v", in, out)
	}
	if out.PID != in.PID || out.LocalUser != in.LocalUser || out.LocalHost != in.LocalHost {
		t.Fatalf("round-trip identity mismatch: in=%+v out=%+v", in, out)
	}
}

func TestSanitizeLockName(t *testing.T) {
	cases := map[string]string{
		"":            "_unnamed",
		"my-app":      "my-app",
		"my_app":      "my_app",
		"My App!":     "My_App_",
		"../etc/pwd":  "___etc_pwd",
		"a/b/c":       "a_b_c",
		"with space":  "with_space",
		"hello-world": "hello-world",
	}
	for in, want := range cases {
		if got := sanitizeLockName(in); got != want {
			t.Errorf("sanitizeLockName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProjectLockDirIsUnderLockBase(t *testing.T) {
	d := projectLockDir("myapp")
	if !strings.HasPrefix(d, lockBaseDir+"/") {
		t.Fatalf("lock dir should be under %s, got %s", lockBaseDir, d)
	}
	if !strings.HasSuffix(d, ".lock") {
		t.Fatalf("lock dir should end in .lock, got %s", d)
	}
}

func TestNoLockSkipsAcquisition(t *testing.T) {
	exec := newMockExecutor("target")
	app := &App{NoLock: true, ExecFactory: nil, Stdout: nilWriter{}}
	called := false
	err := app.withTargetLock(context.Background(), exec, "myapp", "prod", "deploy", func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("NoLock should skip cleanly, got: %v", err)
	}
	if !called {
		t.Fatal("body fn should have been called")
	}
	for _, c := range exec.commands {
		if strings.Contains(c, ".qqd/locks") {
			t.Fatalf("NoLock should not touch the lock dir, but ran: %s", c)
		}
	}
}

type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) { return len(p), nil }
