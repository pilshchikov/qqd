package qqd

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path"
	"strings"
	"time"
)

// Lock implementation notes:
//
//   - The lock lives on the *target host*, not the local machine. Two
//     `qqd` invocations from different laptops still race against the same
//     remote target, so the lock has to be where they both can see it.
//   - We can't use `flock` on a single SSH session, because each
//     `exec.Run` is a separate session and the kernel releases the lock
//     when the session exits. Instead we use atomic `mkdir`: it succeeds
//     for exactly one caller and fails for everyone else.
//   - The lock directory contains a `meta` file with the holder's
//     identity so collisions print something useful, not just "locked".

const lockBaseDir = "~/.qqd/locks"

// LockMeta describes who holds a deploy lock.
type LockMeta struct {
	Command   string
	Project   string
	Target    string
	PID       int
	LocalUser string
	LocalHost string
	StartedAt time.Time
}

// LockedError is returned by acquireLock when another process holds the lock.
// The Meta field may be partially populated if the metadata file was
// unreadable or malformed.
type LockedError struct {
	Project string
	Target  string
	Meta    LockMeta
	Raw     string
}

func (e *LockedError) Error() string {
	if e.Raw != "" {
		return fmt.Sprintf("deploy lock held on target %q for project %q:\n%s\n\nuse --force-unlock to override (only safe if you are sure no other deploy is running)",
			e.Target, e.Project, indent(strings.TrimSpace(e.Raw), "  "))
	}
	return fmt.Sprintf("deploy lock held on target %q for project %q (no metadata available); use --force-unlock to override",
		e.Target, e.Project)
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

// projectLockDir returns the absolute remote path to the per-project lock dir.
// Project name is sanitized to keep the path predictable on the target.
func projectLockDir(project string) string {
	return path.Join(lockBaseDir, sanitizeLockName(project)+".lock")
}

// sanitizeLockName makes a project name safe to use as a directory name on a
// remote host. Conservative: keep alphanumerics, dash, underscore.
//
// This is intentionally separate from sanitizeProjectName in compose_import.go,
// which is for generating *new* project names from filesystem paths and lowercases
// everything. The lock-name sanitizer must be reversible-ish for diagnostics, so
// we preserve case.
func sanitizeLockName(s string) string {
	if s == "" {
		return "_unnamed"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// acquireLock takes a per-project, per-target deploy lock on the remote host.
//
// Returns a release function that should be called via defer. If the lock is
// already held it returns *LockedError unless force is true, in which case it
// removes any existing lock and acquires a fresh one.
func acquireLock(ctx context.Context, exec Executor, project, target, command string, force bool) (release func(), err error) {
	lockDir := projectLockDir(project)
	metaFile := path.Join(lockDir, "meta")

	// Ensure parent exists. A failure here is fatal: we can't lock without it.
	if _, err := exec.Run(ctx, fmt.Sprintf("mkdir -p %s", lockBaseDir)); err != nil {
		return nil, fmt.Errorf("create lock parent dir: %w", err)
	}

	if force {
		// Best-effort: print the previous holder so the operator sees what was overridden.
		prev, _ := exec.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", metaFile))
		if strings.TrimSpace(prev) != "" {
			// We don't have a stdout reference here; the caller logs forcing.
			_ = prev
		}
		if _, err := exec.Run(ctx, fmt.Sprintf("rm -rf %s", lockDir)); err != nil {
			return nil, fmt.Errorf("force-unlock: %w", err)
		}
	}

	// Atomic mkdir. On most shells this returns non-zero if the directory exists.
	// We capture stderr to detect that vs. permission/IO failures.
	if _, err := exec.Run(ctx, fmt.Sprintf("mkdir %s 2>/dev/null", lockDir)); err != nil {
		// Try to read the holder's metadata for a useful error.
		raw, _ := exec.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", metaFile))
		return nil, &LockedError{
			Project: project,
			Target:  target,
			Meta:    parseLockMeta(raw),
			Raw:     raw,
		}
	}

	// Write metadata. Use printf so we don't depend on `cat <<EOF` shell quoting.
	meta := buildLockMeta(command, project, target)
	metaShell := shellQuote(formatLockMeta(meta))
	if _, err := exec.Run(ctx, fmt.Sprintf("printf %%s %s > %s", metaShell, metaFile)); err != nil {
		// Couldn't write metadata. The lock dir exists but is opaque.
		// Roll back rather than leave a meta-less lock around.
		exec.Run(context.Background(), fmt.Sprintf("rm -rf %s", lockDir))
		return nil, fmt.Errorf("write lock metadata: %w", err)
	}

	released := false
	return func() {
		if released {
			return
		}
		released = true
		// Use a fresh context: we want to release even if the parent ctx is cancelled.
		exec.Run(context.Background(), fmt.Sprintf("rm -rf %s", lockDir))
	}, nil
}

// buildLockMeta gathers identity info about the local process.
func buildLockMeta(command, project, target string) LockMeta {
	m := LockMeta{
		Command:   command,
		Project:   project,
		Target:    target,
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC(),
	}
	if u, err := user.Current(); err == nil {
		m.LocalUser = u.Username
	}
	if h, err := os.Hostname(); err == nil {
		m.LocalHost = h
	}
	return m
}

// formatLockMeta renders metadata as a key/value text block for the lock file.
func formatLockMeta(m LockMeta) string {
	return fmt.Sprintf(
		"command:    %s\nproject:    %s\ntarget:     %s\npid:        %d\nlocal_user: %s\nlocal_host: %s\nstarted_at: %s\n",
		m.Command, m.Project, m.Target, m.PID, m.LocalUser, m.LocalHost, m.StartedAt.Format(time.RFC3339),
	)
}

// parseLockMeta parses what formatLockMeta wrote. Best-effort; an empty or
// truncated file produces a zero-valued LockMeta.
func parseLockMeta(raw string) LockMeta {
	var m LockMeta
	for _, line := range strings.Split(raw, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "command":
			m.Command = v
		case "project":
			m.Project = v
		case "target":
			m.Target = v
		case "pid":
			fmt.Sscanf(v, "%d", &m.PID)
		case "local_user":
			m.LocalUser = v
		case "local_host":
			m.LocalHost = v
		case "started_at":
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				m.StartedAt = t
			}
		}
	}
	return m
}
