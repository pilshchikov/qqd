package qqd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestStatusJSONOutput(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "myapp",
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"prod": {
				Name: "prod",
				Host: "192.0.2.10",
				User: "deploy",
			},
		},
	}
	var buf bytes.Buffer
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"prod": targetExec},
		},
		Stdout:       &buf,
		OutputFormat: "json",
		DrainWait:    -1,
	}
	if err := app.Status(context.Background(), cfg, "prod"); err != nil {
		t.Fatalf("Status JSON failed: %v", err)
	}

	var result StatusResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nraw: %s", err, buf.String())
	}
	if len(result.Targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(result.Targets))
	}
	tgt := result.Targets[0]
	if tgt.Name != "prod" {
		t.Fatalf("target name = %q, want prod", tgt.Name)
	}
	if tgt.Host != "192.0.2.10" {
		t.Fatalf("target host = %q, want 192.0.2.10", tgt.Host)
	}
	if len(tgt.Services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(tgt.Services))
	}
}

func TestStatusJSONContainsAllServices(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "myapp",
		Services: map[string]ServiceConfig{
			"api":      {Image: "ghcr.io/acme/api:2.0"},
			"web":      {Image: "ghcr.io/acme/web:1.0"},
			"worker":   {Image: "ghcr.io/acme/worker:0.5"},
			"database": {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"staging": {
				Name: "staging",
				Host: "192.0.2.11",
				User: "deploy",
			},
		},
	}
	var buf bytes.Buffer
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"staging": targetExec},
		},
		Stdout:       &buf,
		OutputFormat: "json",
		DrainWait:    -1,
	}
	if err := app.Status(context.Background(), cfg, "staging"); err != nil {
		t.Fatalf("Status JSON failed: %v", err)
	}

	var result StatusResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	if len(result.Targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(result.Targets))
	}
	svcNames := map[string]bool{}
	for _, s := range result.Targets[0].Services {
		svcNames[s.Name] = true
	}
	for _, name := range []string{"api", "database", "web", "worker"} {
		if !svcNames[name] {
			t.Errorf("service %q missing from JSON output", name)
		}
	}
}

func TestStatusTextOutputUnchanged(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "myapp",
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"prod": {
				Name: "prod",
				Host: "192.0.2.10",
				User: "deploy",
			},
		},
	}
	var buf bytes.Buffer
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"prod": targetExec},
		},
		Stdout:    &buf,
		DrainWait: -1,
	}
	if err := app.Status(context.Background(), cfg, "prod"); err != nil {
		t.Fatalf("Status text failed: %v", err)
	}
	out := buf.String()
	// Text output should contain the target=name host=host header
	if !strings.Contains(out, "target=prod host=192.0.2.10") {
		t.Fatalf("text output missing target header, got: %q", out)
	}
	// Text output should contain the service label
	if !strings.Contains(out, "web:") {
		t.Fatalf("text output missing service label, got: %q", out)
	}
	// Text output should NOT be valid JSON
	var result StatusResult
	if err := json.Unmarshal(buf.Bytes(), &result); err == nil {
		t.Fatal("text output should not be valid JSON")
	}
}

func TestStatusJSONNoServices(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name:     "empty",
		Services: map[string]ServiceConfig{},
		Targets: map[string]TargetConfig{
			"prod": {
				Name: "prod",
				Host: "192.0.2.10",
				User: "deploy",
			},
		},
	}
	var buf bytes.Buffer
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"prod": targetExec},
		},
		Stdout:       &buf,
		OutputFormat: "json",
		DrainWait:    -1,
	}
	if err := app.Status(context.Background(), cfg, "prod"); err != nil {
		t.Fatalf("Status JSON failed: %v", err)
	}
	var result StatusResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	if len(result.Targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(result.Targets))
	}
	if result.Targets[0].Services != nil {
		t.Fatalf("expected nil services for empty config, got %v", result.Targets[0].Services)
	}
}

func TestStatusJSONServiceStateActive(t *testing.T) {
	// The mock executor returns "active" for all units by default,
	// so all services should appear with state "active" in JSON output.
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "myapp",
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0"},
			"db":  {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"prod": {
				Name: "prod",
				Host: "192.0.2.10",
				User: "deploy",
			},
		},
	}
	var buf bytes.Buffer
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"prod": targetExec},
		},
		Stdout:       &buf,
		OutputFormat: "json",
		DrainWait:    -1,
	}
	if err := app.Status(context.Background(), cfg, "prod"); err != nil {
		t.Fatalf("Status JSON failed: %v", err)
	}
	var result StatusResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	for _, s := range result.Targets[0].Services {
		if s.State == "" {
			t.Errorf("service %q has empty state", s.Name)
		}
	}
}

func TestStatusJSONTextNotMixed(t *testing.T) {
	// Verify that JSON mode produces no text preamble (like "target=...")
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "myapp",
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"prod": {
				Name: "prod",
				Host: "192.0.2.10",
				User: "deploy",
			},
		},
	}
	var buf bytes.Buffer
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"prod": targetExec},
		},
		Stdout:       &buf,
		OutputFormat: "json",
		DrainWait:    -1,
	}
	if err := app.Status(context.Background(), cfg, "prod"); err != nil {
		t.Fatalf("Status JSON failed: %v", err)
	}
	raw := buf.String()
	if strings.Contains(raw, "target=") {
		t.Fatalf("JSON output should not contain text preamble, got: %s", raw)
	}
	// Should start with { (after possible whitespace)
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") {
		t.Fatalf("JSON output should start with {, got: %q", trimmed[:20])
	}
}

func TestStatusJSONDiscardStdout(t *testing.T) {
	// Verify Status with JSON works even with io.Discard (no panic)
	targetExec := newMockExecutor("target-main")
	cfg := ProjectConfig{
		Name: "myapp",
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/acme/web:1.0"},
		},
		Targets: map[string]TargetConfig{
			"prod": {
				Name: "prod",
				Host: "192.0.2.10",
				User: "deploy",
			},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"prod": targetExec},
		},
		Stdout:       io.Discard,
		OutputFormat: "json",
		DrainWait:    -1,
	}
	if err := app.Status(context.Background(), cfg, "prod"); err != nil {
		t.Fatalf("Status JSON with Discard failed: %v", err)
	}
}

func TestParseStatusArgsOutputFlag(t *testing.T) {
	cfgPaths, target, outputFormat, err := parseStatusArgs([]string{"-c", "qd.conf", "-t", "prod", "--output", "json"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfgPaths) != 1 || cfgPaths[0] != "qd.conf" {
		t.Fatalf("cfgPaths = %v", cfgPaths)
	}
	if target != "prod" {
		t.Fatalf("target = %q", target)
	}
	if outputFormat != "json" {
		t.Fatalf("outputFormat = %q, want json", outputFormat)
	}
}

func TestParseStatusArgsDefaultOutput(t *testing.T) {
	cfgPaths, target, outputFormat, err := parseStatusArgs([]string{"-c", "qd.conf", "-t", "main"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfgPaths) != 1 || cfgPaths[0] != "qd.conf" {
		t.Fatalf("cfgPaths = %v", cfgPaths)
	}
	if target != "main" {
		t.Fatalf("target = %q", target)
	}
	if outputFormat != "" {
		t.Fatalf("outputFormat = %q, want empty", outputFormat)
	}
}

func TestParseStatusArgsRejectsServices(t *testing.T) {
	_, _, _, err := parseStatusArgs([]string{"-c", "qd.conf", "server"})
	if err == nil {
		t.Fatal("parseStatusArgs should reject positional service args")
	}
	if !strings.Contains(err.Error(), "does not accept service names") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestParseStatusArgsUnsupportedFormat(t *testing.T) {
	_, _, _, err := parseStatusArgs([]string{"-c", "qd.conf", "--output", "xml"})
	if err == nil {
		t.Fatal("parseStatusArgs should reject unsupported formats")
	}
	if !strings.Contains(err.Error(), "unsupported output format") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestParseStatusArgsMissingConfig(t *testing.T) {
	_, _, _, err := parseStatusArgs([]string{"--output", "json"})
	if err == nil {
		t.Fatal("parseStatusArgs should require -c")
	}
	if !strings.Contains(err.Error(), "-c <config> is required") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestParseStatusArgsOutputBeforeConfig(t *testing.T) {
	cfgPaths, _, outputFormat, err := parseStatusArgs([]string{"--output", "json", "-c", "qd.conf"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outputFormat != "json" {
		t.Fatalf("outputFormat = %q, want json", outputFormat)
	}
	if len(cfgPaths) != 1 || cfgPaths[0] != "qd.conf" {
		t.Fatalf("cfgPaths = %v", cfgPaths)
	}
}
