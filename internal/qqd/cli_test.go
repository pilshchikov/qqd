package qqd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecuteGlobalHelp(t *testing.T) {
	var out bytes.Buffer
	if err := Execute([]string{"--help"}, &out); err != nil {
		t.Fatalf("Execute --help failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "usage:") {
		t.Fatalf("global help missing usage: %q", got)
	}
	if !strings.Contains(got, "qqd man") {
		t.Fatalf("global help missing man command: %q", got)
	}
	if !strings.Contains(got, "qqd docs") {
		t.Fatalf("global help missing docs command: %q", got)
	}
}

func TestExecuteHelpCommand(t *testing.T) {
	var out bytes.Buffer
	if err := Execute([]string{"help", "deploy"}, &out); err != nil {
		t.Fatalf("Execute help deploy failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "usage: qqd deploy") {
		t.Fatalf("deploy help not shown: %q", got)
	}
}

func TestExecuteCommandHelpFlag(t *testing.T) {
	var out bytes.Buffer
	if err := Execute([]string{"update", "--help"}, &out); err != nil {
		t.Fatalf("Execute update --help failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "usage: qqd update") {
		t.Fatalf("update help not shown: %q", got)
	}
}

func TestExecuteManHelpFlag(t *testing.T) {
	var out bytes.Buffer
	if err := Execute([]string{"man", "--help"}, &out); err != nil {
		t.Fatalf("Execute man --help failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "usage: qqd man") {
		t.Fatalf("man help not shown: %q", got)
	}
}

func TestParseCommonArgsInterspersed(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCfg  []string
		wantTgt  string
		wantSvcs []string
		wantReb  bool
	}{
		{
			name:     "flags before services",
			args:     []string{"-c", "qd.conf", "-t", "main", "server"},
			wantCfg:  []string{"qd.conf"},
			wantTgt:  "main",
			wantSvcs: []string{"server"},
		},
		{
			name:     "services before flags",
			args:     []string{"server", "-c", "qd.conf"},
			wantCfg:  []string{"qd.conf"},
			wantSvcs: []string{"server"},
		},
		{
			name:     "services between flags",
			args:     []string{"-c", "qd.conf", "server", "-t", "main"},
			wantCfg:  []string{"qd.conf"},
			wantTgt:  "main",
			wantSvcs: []string{"server"},
		},
		{
			name:     "multiple configs and services",
			args:     []string{"frontend", "-c", "qd.conf", "-c", "secrets.conf", "server"},
			wantCfg:  []string{"qd.conf", "secrets.conf"},
			wantSvcs: []string{"frontend", "server"},
		},
		{
			name:     "rebuild interspersed",
			args:     []string{"server", "--rebuild", "-c", "qd.conf"},
			wantCfg:  []string{"qd.conf"},
			wantSvcs: []string{"server"},
			wantReb:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgPaths, target, services, rebuild, err := parseCommonArgs(tt.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(cfgPaths) != len(tt.wantCfg) {
				t.Fatalf("cfgPaths = %v, want %v", cfgPaths, tt.wantCfg)
			}
			for i := range cfgPaths {
				if cfgPaths[i] != tt.wantCfg[i] {
					t.Fatalf("cfgPaths[%d] = %q, want %q", i, cfgPaths[i], tt.wantCfg[i])
				}
			}
			if target != tt.wantTgt {
				t.Fatalf("target = %q, want %q", target, tt.wantTgt)
			}
			if len(services) != len(tt.wantSvcs) {
				t.Fatalf("services = %v, want %v", services, tt.wantSvcs)
			}
			for i := range services {
				if services[i] != tt.wantSvcs[i] {
					t.Fatalf("services[%d] = %q, want %q", i, services[i], tt.wantSvcs[i])
				}
			}
			if rebuild != tt.wantReb {
				t.Fatalf("rebuild = %v, want %v", rebuild, tt.wantReb)
			}
		})
	}
}

func TestParseCommonOptsApproveFlag(t *testing.T) {
	opts, err := parseCommonOpts([]string{"-c", "qd.conf", "--approve", "server"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !opts.Approve {
		t.Fatal("--approve should set Approve=true")
	}
	if len(opts.Services) != 1 || opts.Services[0] != "server" {
		t.Fatalf("services = %v, want [server]", opts.Services)
	}
}

func TestParseCommonOptsApproveAndRebuild(t *testing.T) {
	opts, err := parseCommonOpts([]string{"--approve", "-c", "qd.conf", "--rebuild", "-t", "main"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !opts.Approve {
		t.Fatal("--approve should be true")
	}
	if !opts.Rebuild {
		t.Fatal("--rebuild should be true")
	}
	if opts.Target != "main" {
		t.Fatalf("target = %q, want main", opts.Target)
	}
}

func TestParseCommonOptsNoApproveByDefault(t *testing.T) {
	opts, err := parseCommonOpts([]string{"-c", "qd.conf"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Approve {
		t.Fatal("Approve should default to false")
	}
}

func TestParseCommonArgsMissingConfig(t *testing.T) {
	_, _, _, _, err := parseCommonArgs([]string{"server"})
	if err == nil {
		t.Fatal("expected error when -c is missing")
	}
}

func TestExecuteDocsHelpFlag(t *testing.T) {
	var out bytes.Buffer
	if err := Execute([]string{"docs", "--help"}, &out); err != nil {
		t.Fatalf("Execute docs --help failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "usage: qqd docs") {
		t.Fatalf("docs help not shown: %q", got)
	}
}

// ---------------------------------------------------------------------------
// CLI edge cases
// ---------------------------------------------------------------------------

func TestExecuteNoArgs(t *testing.T) {
	var out bytes.Buffer
	if err := Execute(nil, &out); err != nil {
		t.Fatalf("Execute nil args failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "usage:") {
		t.Fatalf("no args should show usage, got: %q", got)
	}
}

func TestExecuteEmptyArgs(t *testing.T) {
	var out bytes.Buffer
	if err := Execute([]string{}, &out); err != nil {
		t.Fatalf("Execute empty args failed: %v", err)
	}
	if !strings.Contains(out.String(), "usage:") {
		t.Fatal("empty args should show usage")
	}
}

func TestExecuteShortHelpFlag(t *testing.T) {
	var out bytes.Buffer
	if err := Execute([]string{"-h"}, &out); err != nil {
		t.Fatalf("Execute -h failed: %v", err)
	}
	if !strings.Contains(out.String(), "usage:") {
		t.Fatal("-h should show usage")
	}
}

func TestExecuteUnknownCommand(t *testing.T) {
	var out bytes.Buffer
	err := Execute([]string{"nonexistent"}, &out)
	if err == nil {
		t.Fatal("unknown command should return error")
	}
	if !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("unknown command error should include usage, got: %v", err)
	}
}

func TestExecuteHelpForUnknownCommand(t *testing.T) {
	var out bytes.Buffer
	if err := Execute([]string{"help", "nonexistent"}, &out); err != nil {
		t.Fatalf("help for unknown command should not error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `unknown command "nonexistent"`) {
		t.Fatalf("should indicate unknown command, got: %q", got)
	}
	if !strings.Contains(got, "usage:") {
		t.Fatalf("should fallback to global help, got: %q", got)
	}
}

func TestExecuteHelpAlone(t *testing.T) {
	var out bytes.Buffer
	if err := Execute([]string{"help"}, &out); err != nil {
		t.Fatalf("help alone failed: %v", err)
	}
	if !strings.Contains(out.String(), "usage:") {
		t.Fatal("help alone should show global usage")
	}
}

func TestParseTargetOnlyArgsRejectsServices(t *testing.T) {
	_, _, err := parseTargetOnlyArgs([]string{"-c", "qd.conf", "server"})
	if err == nil {
		t.Fatal("parseTargetOnlyArgs should reject positional service args")
	}
	if !strings.Contains(err.Error(), "does not accept service names") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestParseSingleServiceArgsZero(t *testing.T) {
	_, _, _, err := parseSingleServiceArgs("logs", []string{"-c", "qd.conf"})
	if err == nil {
		t.Fatal("parseSingleServiceArgs should error with 0 services")
	}
	if !strings.Contains(err.Error(), "requires exactly one service") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestParseSingleServiceArgsTooMany(t *testing.T) {
	_, _, _, err := parseSingleServiceArgs("logs", []string{"-c", "qd.conf", "a", "b"})
	if err == nil {
		t.Fatal("parseSingleServiceArgs should error with 2 services")
	}
}

func TestParseSingleServiceArgsExactlyOne(t *testing.T) {
	cfgPaths, target, service, err := parseSingleServiceArgs("logs", []string{"-c", "qd.conf", "-t", "main", "web"})
	if err != nil {
		t.Fatalf("parseSingleServiceArgs error: %v", err)
	}
	if len(cfgPaths) != 1 || cfgPaths[0] != "qd.conf" {
		t.Fatalf("cfgPaths = %v", cfgPaths)
	}
	if target != "main" {
		t.Fatalf("target = %q", target)
	}
	if service != "web" {
		t.Fatalf("service = %q", service)
	}
}

func TestParseCommonArgsDanglingCFlag(t *testing.T) {
	// -c at end with no value: gets treated as service name, then errors on missing -c
	_, _, _, _, err := parseCommonArgs([]string{"-c"})
	if err == nil {
		t.Fatal("dangling -c should error")
	}
}

func TestParseCommonArgsDanglingTFlag(t *testing.T) {
	// -t at end with no value, but has -c
	cfgPaths, target, _, _, err := parseCommonArgs([]string{"-c", "qd.conf", "-t"})
	if err != nil {
		// -t at end gets treated as service name since there's no next arg
		// The current implementation would treat "-t" at the end as a service
		t.Fatalf("unexpected error: %v", err)
	}
	_ = cfgPaths
	_ = target
}

func TestParseCommonArgsServicesSorted(t *testing.T) {
	_, _, services, _, err := parseCommonArgs([]string{"-c", "qd.conf", "z-svc", "a-svc", "m-svc"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(services) != 3 || services[0] != "a-svc" || services[1] != "m-svc" || services[2] != "z-svc" {
		t.Fatalf("services should be sorted, got %v", services)
	}
}

func TestExecuteNilStdout(t *testing.T) {
	// Should not panic with nil stdout
	if err := Execute([]string{"--help"}, nil); err != nil {
		t.Fatalf("nil stdout should not error: %v", err)
	}
}

func TestGlobalUsageContainsClean(t *testing.T) {
	if !strings.Contains(globalUsage(), "qqd clean") {
		t.Fatal("global usage should contain clean command")
	}
}

func TestCommandSpecsContainsClean(t *testing.T) {
	_, found := commandSpecByName("clean")
	if !found {
		t.Fatal("commandSpecs should contain clean")
	}
}

func TestAllHelpFlagsShowHelp(t *testing.T) {
	commands := []string{"plan", "init", "deploy", "build", "update", "status", "logs", "rollback", "stop", "start", "destroy", "clean", "doctor", "validate", "man", "docs"}
	for _, cmd := range commands {
		t.Run(cmd, func(t *testing.T) {
			var out bytes.Buffer
			if err := Execute([]string{cmd, "--help"}, &out); err != nil {
				t.Fatalf("Execute %s --help failed: %v", cmd, err)
			}
			got := out.String()
			if !strings.Contains(got, "usage:") {
				t.Fatalf("%s --help should show usage, got: %q", cmd, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// plan command and --dry-run tests
// ---------------------------------------------------------------------------

const testHOCONConfig = `
name = "test-proj"
repo = "https://github.com/test/repo.git"
services {
    server {
        image = "ghcr.io/test/server:1.0"
    }
    worker {
        image = "ghcr.io/test/worker:2.0"
        dockerfile = "Dockerfile.worker"
    }
}
targets {
    main {
        host = "local"
        user = "test"
        repo_dir = "/tmp/test"
    }
}
`

// writeTempConfig writes a HOCON config to a temp directory and returns the file path.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "test.conf")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return p
}

func TestPlanCommandShowsPlan(t *testing.T) {
	cfgPath := writeTempConfig(t, testHOCONConfig)
	var out bytes.Buffer
	err := Execute([]string{"plan", "-c", cfgPath}, &out)
	if err != nil {
		t.Fatalf("plan command failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "test-proj") {
		t.Fatalf("plan output should contain project name, got: %q", got)
	}
	if !strings.Contains(got, "server") {
		t.Fatalf("plan output should contain service name 'server', got: %q", got)
	}
	if !strings.Contains(got, "worker") {
		t.Fatalf("plan output should contain service name 'worker', got: %q", got)
	}
	if !strings.Contains(got, "main") {
		t.Fatalf("plan output should contain target name 'main', got: %q", got)
	}
}

func TestPlanCommandWithTarget(t *testing.T) {
	cfgPath := writeTempConfig(t, testHOCONConfig)
	var out bytes.Buffer
	err := Execute([]string{"plan", "-c", cfgPath, "-t", "main"}, &out)
	if err != nil {
		t.Fatalf("plan command with target failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "main") {
		t.Fatalf("plan output should contain target 'main', got: %q", got)
	}
}

func TestPlanCommandWithServiceFilter(t *testing.T) {
	cfgPath := writeTempConfig(t, testHOCONConfig)
	var out bytes.Buffer
	err := Execute([]string{"plan", "-c", cfgPath, "server"}, &out)
	if err != nil {
		t.Fatalf("plan command with service filter failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "server") {
		t.Fatalf("plan output should contain 'server', got: %q", got)
	}
	if !strings.Contains(got, "partial deploy") {
		t.Fatalf("plan output should indicate partial deploy, got: %q", got)
	}
}

func TestPlanHelpFlag(t *testing.T) {
	var out bytes.Buffer
	err := Execute([]string{"plan", "--help"}, &out)
	if err != nil {
		t.Fatalf("plan --help failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "usage: qqd plan") {
		t.Fatalf("plan help should show usage, got: %q", got)
	}
}

func TestDryRunFlag(t *testing.T) {
	opts, err := parseCommonOpts([]string{"-c", "qd.conf", "--dry-run", "server"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !opts.DryRun {
		t.Fatal("--dry-run should set DryRun=true")
	}
	if len(opts.Services) != 1 || opts.Services[0] != "server" {
		t.Fatalf("services = %v, want [server]", opts.Services)
	}
}

func TestDryRunDefaultFalse(t *testing.T) {
	opts, err := parseCommonOpts([]string{"-c", "qd.conf"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.DryRun {
		t.Fatal("DryRun should default to false")
	}
}

func TestDryRunWithOtherFlags(t *testing.T) {
	opts, err := parseCommonOpts([]string{"--dry-run", "--approve", "-c", "qd.conf", "--rebuild", "-t", "main"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !opts.DryRun {
		t.Fatal("--dry-run should be true")
	}
	if !opts.Approve {
		t.Fatal("--approve should be true")
	}
	if !opts.Rebuild {
		t.Fatal("--rebuild should be true")
	}
	if opts.Target != "main" {
		t.Fatalf("target = %q, want main", opts.Target)
	}
}

func TestDryRunShowsPlanWithoutExecuting(t *testing.T) {
	cfgPath := writeTempConfig(t, testHOCONConfig)
	var out bytes.Buffer
	err := Execute([]string{"deploy", "-c", cfgPath, "--dry-run"}, &out)
	if err != nil {
		t.Fatalf("deploy --dry-run failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "dry-run") {
		t.Fatalf("deploy --dry-run output should contain 'dry-run', got: %q", got)
	}
	if !strings.Contains(got, "test-proj") {
		t.Fatalf("deploy --dry-run output should contain project name, got: %q", got)
	}
	if !strings.Contains(got, "server") {
		t.Fatalf("deploy --dry-run output should contain service 'server', got: %q", got)
	}
}

func TestGlobalUsageContainsPlan(t *testing.T) {
	if !strings.Contains(globalUsage(), "qqd plan") {
		t.Fatal("global usage should contain plan command")
	}
}

func TestGlobalUsageContainsDryRun(t *testing.T) {
	if !strings.Contains(globalUsage(), "--dry-run") {
		t.Fatal("global usage should contain --dry-run flag")
	}
}

// TestGlobalUsageContainsAllNewFlags pins the second-pass review's complaint:
// previously, hand-maintained globalUsage drifted from per-command help. Now
// it's generated from commandSpecs(), so any new flag added to a spec's Usage
// line shows up in `qqd --help` automatically. This test fails fast if someone
// reverts to a hand-maintained constant.
func TestGlobalUsageContainsAllNewFlags(t *testing.T) {
	usage := globalUsage()
	for _, flag := range []string{
		"--output json", // plan
		"--force-unlock",
		"migrate",
		"--yes",
	} {
		if !strings.Contains(usage, flag) {
			t.Errorf("globalUsage() should contain %q (regenerate from commandSpecs() if you changed the source)", flag)
		}
	}
}

func TestCommandSpecsContainsPlan(t *testing.T) {
	_, found := commandSpecByName("plan")
	if !found {
		t.Fatal("commandSpecs should contain plan")
	}
}

func TestDeploySpecContainsDryRun(t *testing.T) {
	spec, found := commandSpecByName("deploy")
	if !found {
		t.Fatal("commandSpecs should contain deploy")
	}
	if !strings.Contains(spec.Usage, "--dry-run") {
		t.Fatalf("deploy spec usage should mention --dry-run, got: %q", spec.Usage)
	}
	hasDryRunDetail := false
	for _, d := range spec.Details {
		if strings.Contains(d, "--dry-run") {
			hasDryRunDetail = true
			break
		}
	}
	if !hasDryRunDetail {
		t.Fatal("deploy spec details should mention --dry-run")
	}
}

func TestPlanRedactsSecretEnvVars(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "myapp"
repo = "https://github.com/test/repo.git"
services {
    server {
        image = "server:1.0"
        env {
            DB_HOST = "localhost"
            DB_PASSWORD = "s3cret!"
            API_TOKEN = "tok_abc123"
        }
    }
}
targets {
    dev {
        host = "local"
        repo_dir = "/tmp/myapp"
    }
}
`
	os.WriteFile(cfgPath, []byte(cfgText), 0o644)

	var out bytes.Buffer
	err := Execute([]string{"plan", "-c", cfgPath}, &out)
	if err != nil {
		t.Fatalf("plan failed: %v", err)
	}

	got := out.String()
	// Non-secret env should be visible
	if !strings.Contains(got, "DB_HOST=localhost") {
		t.Fatalf("plan should show non-secret env var DB_HOST, got:\n%s", got)
	}
	// Secret env should be redacted
	if strings.Contains(got, "s3cret!") {
		t.Fatalf("plan should redact DB_PASSWORD value, got:\n%s", got)
	}
	if strings.Contains(got, "tok_abc123") {
		t.Fatalf("plan should redact API_TOKEN value, got:\n%s", got)
	}
	// Redacted values should show the key
	if !strings.Contains(got, "DB_PASSWORD=") {
		t.Fatalf("plan should show DB_PASSWORD key, got:\n%s", got)
	}
	if !strings.Contains(got, "API_TOKEN=") {
		t.Fatalf("plan should show API_TOKEN key, got:\n%s", got)
	}
}
