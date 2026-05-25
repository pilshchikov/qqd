package qqd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostKeyCallbackForPathRequiresKnownHostsByDefault(t *testing.T) {
	td := t.TempDir()
	path := filepath.Join(td, "missing_known_hosts")
	_, err := hostKeyCallbackForPath(path, false)
	if err == nil {
		t.Fatal("expected missing known_hosts to fail in strict mode")
	}
	if !strings.Contains(err.Error(), "known_hosts not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHostKeyCallbackForPathAllowsExplicitInsecureMode(t *testing.T) {
	td := t.TempDir()
	path := filepath.Join(td, "missing_known_hosts")
	cb, err := hostKeyCallbackForPath(path, true)
	if err != nil {
		t.Fatalf("insecure mode should not fail: %v", err)
	}
	if cb == nil {
		t.Fatal("expected insecure host key callback")
	}
}

func TestHostKeyCallbackForPathRejectsInvalidKnownHosts(t *testing.T) {
	td := t.TempDir()
	path := filepath.Join(td, "known_hosts")
	if err := os.WriteFile(path, []byte("not-a-valid-known-hosts-line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := hostKeyCallbackForPath(path, false)
	if err == nil {
		t.Fatal("expected invalid known_hosts to fail")
	}
	if !strings.Contains(err.Error(), "parse known_hosts") {
		t.Fatalf("unexpected error: %v", err)
	}
}
