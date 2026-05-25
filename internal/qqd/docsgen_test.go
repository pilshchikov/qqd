package qqd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecuteDocsToStdout(t *testing.T) {
	var out bytes.Buffer
	if err := Execute([]string{"docs"}, &out); err != nil {
		t.Fatalf("Execute docs failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "# qqd CLI Reference") {
		t.Fatalf("docs header missing: %q", got)
	}
	if !strings.Contains(got, "### deploy") {
		t.Fatalf("command section missing: %q", got)
	}
}

func TestExecuteDocsToFile(t *testing.T) {
	td := t.TempDir()
	outPath := filepath.Join(td, "CLI.md")
	var out bytes.Buffer
	if err := Execute([]string{"docs", "-o", outPath}, &out); err != nil {
		t.Fatalf("Execute docs -o failed: %v", err)
	}
	if !strings.Contains(out.String(), "generated markdown documentation") {
		t.Fatalf("expected generation status output, got: %q", out.String())
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading generated docs failed: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "qqd docs [config] [--format markdown] [-o <path>]") {
		t.Fatalf("generated docs missing docs command usage: %q", got)
	}
}

func TestExecuteDocsRejectsBadFormat(t *testing.T) {
	var out bytes.Buffer
	err := Execute([]string{"docs", "--format", "html"}, &out)
	if err == nil {
		t.Fatalf("expected docs format validation error")
	}
	if !strings.Contains(err.Error(), "unsupported docs format") {
		t.Fatalf("unexpected error: %v", err)
	}
}
