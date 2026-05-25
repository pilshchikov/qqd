package qqd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocsHaveNoKnownStaleStrings is a cheap grep-as-test for phrases that
// were correct at some point in the past but are wrong today. It exists
// because docs drift faster than code: a flag is added, a default is changed,
// a file is renamed, and the prose lags by months.
//
// Add an entry here whenever you fix a doc contradiction so it can't silently
// come back. Each entry is a substring + a one-line explanation of why it's
// banned. Match is case-sensitive.
func TestDocsHaveNoKnownStaleStrings(t *testing.T) {
	banned := []struct {
		phrase, reason string
	}{
		// The dynamic Caddy config is named "Caddyfile" (Caddyfile-format),
		// not "routes.json". The rename was a Caddy-audit fix.
		{"caddy-routes/routes.json", "Caddy dynamic config is named Caddyfile, not routes.json (post-audit). Update or delete this reference."},
		// Stale CLI usage: import takes -f, convert takes -c. The old
		// positional forms shipped briefly and confused users.
		{"qqd import <docker-compose.yml>", "import requires -f, not a positional arg. Use `qqd import -f <docker-compose.yaml>`."},
		{"qqd convert <input>", "convert requires -c, not a positional arg. Use `qqd convert -c <input>`."},
		// migrate gained --dry-run; safety-model used to say it didn't.
		{"No `--dry-run` yet", "qqd migrate now supports --dry-run. Update this section."},
		// plan now surfaces risks; claims.md "Safety First" used to deny it.
		{"`plan` does not yet surface risks", "qqd plan now surfaces risks (see plan_risks.go). Update this row."},
		// Caddy + raw TCP is now rejected by validate, not "emitted as HTTP".
		{"TCP-style entries are emitted as HTTP", "qqd validate now rejects Caddy + raw TCP. Update this section to say 'rejected', not 'emitted as HTTP'."},
		{"Caddy TCP passthrough flag", "Old framing: was a 'plan danger' before validation rejection landed. Update to mention validation."},
	}

	docsRoots := findDocsRoots(t)
	if len(docsRoots) == 0 {
		t.Skip("no docs roots found; running outside the project tree?")
	}

	var hits []string
	for _, root := range docsRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // best-effort
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".md") {
				return nil
			}
			// Don't trip on this test's own banned-phrase list.
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			body := string(data)
			for _, b := range banned {
				if strings.Contains(body, b.phrase) {
					hits = append(hits, path+": contains banned phrase "+b.phrase+" — "+b.reason)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(hits) > 0 {
		t.Fatalf("docs drift detected:\n  - %s", strings.Join(hits, "\n  - "))
	}
}

// findDocsRoots returns the project's docs/ directory and the project root.
// It walks up from the test working directory because go test cwd's into the package.
func findDocsRoots(t *testing.T) []string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return nil
	}
	dir := wd
	// Walk up until we find a directory containing both `docs/` and `go.mod`.
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			roots := []string{dir}
			if _, err := os.Stat(filepath.Join(dir, "docs")); err == nil {
				roots = append(roots, filepath.Join(dir, "docs"))
			}
			return roots
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return nil
}
