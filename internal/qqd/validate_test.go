package qqd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateDetectsCyclicDependencies(t *testing.T) {
	cfg := ProjectConfig{
		Name: "test",
		Services: map[string]ServiceConfig{
			"a": {Image: "a:1.0", DependsOn: []string{"b"}},
			"b": {Image: "b:1.0", DependsOn: []string{"c"}},
			"c": {Image: "c:1.0", DependsOn: []string{"a"}},
		},
		Targets: map[string]TargetConfig{
			"dev": {Host: "local", RepoDir: "/tmp"},
		},
	}
	msgs := ValidateConfig(cfg)
	found := false
	for _, m := range msgs {
		if strings.Contains(m, "circular dependency") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected circular dependency error, got: %v", msgs)
	}
}

func TestValidateDetectsMissingDependency(t *testing.T) {
	cfg := ProjectConfig{
		Name: "test",
		Services: map[string]ServiceConfig{
			"web": {Image: "web:1.0", DependsOn: []string{"missing"}},
		},
		Targets: map[string]TargetConfig{
			"dev": {Host: "local", RepoDir: "/tmp"},
		},
	}
	msgs := ValidateConfig(cfg)
	found := false
	for _, m := range msgs {
		if strings.Contains(m, "does not exist") && strings.Contains(m, "missing") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected missing dependency error, got: %v", msgs)
	}
}

func TestValidateDetectsInvalidPortRange(t *testing.T) {
	cfg := ProjectConfig{
		Name: "test",
		Services: map[string]ServiceConfig{
			"app": {Image: "app:1.0"},
		},
		Targets: map[string]TargetConfig{
			"dev": {
				Host:    "local",
				RepoDir: "/tmp",
				Expose: ExposeConfig{
					Entries: []ExposeEntry{
						{HostPort: 0},
						{HostPort: 70000},
					},
				},
			},
		},
	}
	msgs := ValidateConfig(cfg)
	portErrors := 0
	for _, m := range msgs {
		if strings.Contains(m, "out of range") {
			portErrors++
		}
	}
	if portErrors < 2 {
		t.Fatalf("expected at least 2 port range errors, got %d: %v", portErrors, msgs)
	}
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	cfg := ProjectConfig{
		Name: "test",
		Services: map[string]ServiceConfig{
			"server": {Image: "server:1.0", DependsOn: []string{"db"}},
			"db":     {Image: "postgres:16.1"},
		},
		Targets: map[string]TargetConfig{
			"dev": {
				Host:    "local",
				RepoDir: "/tmp",
				Expose: ExposeConfig{
					Entries: []ExposeEntry{
						{
							HostPort: 8080,
							Routes:   map[string]string{"/": "server:8080"},
						},
					},
				},
			},
		},
	}
	msgs := ValidateConfig(cfg)
	errors := 0
	for _, m := range msgs {
		if strings.HasPrefix(m, "error:") {
			errors++
		}
	}
	if errors > 0 {
		t.Fatalf("valid config should have no errors, got: %v", msgs)
	}
}

func TestValidateDetectsMutableTag(t *testing.T) {
	cfg := ProjectConfig{
		Name: "test",
		Services: map[string]ServiceConfig{
			"app": {Image: "foo:latest"},
		},
		Targets: map[string]TargetConfig{
			"dev": {Host: "local", RepoDir: "/tmp"},
		},
	}
	msgs := ValidateConfig(cfg)
	found := false
	for _, m := range msgs {
		if strings.Contains(m, "mutable image tag") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected mutable tag warning, got: %v", msgs)
	}
}

func TestValidateBuildStrategyRequirements(t *testing.T) {
	cfg := ProjectConfig{
		Name: "test",
		Build: BuildConfig{
			Strategy: "build-host",
			// Missing host, user, repo_dir
		},
		Services: map[string]ServiceConfig{
			"app": {Image: "app:1.0"},
		},
		Targets: map[string]TargetConfig{
			"dev": {Host: "local", RepoDir: "/tmp"},
		},
	}
	msgs := ValidateConfig(cfg)
	hostErr, userErr, repoDirErr := false, false, false
	for _, m := range msgs {
		if strings.Contains(m, "build-host") && strings.Contains(m, "requires host") {
			hostErr = true
		}
		if strings.Contains(m, "build-host") && strings.Contains(m, "requires user") {
			userErr = true
		}
		if strings.Contains(m, "build-host") && strings.Contains(m, "requires repo_dir") {
			repoDirErr = true
		}
	}
	if !hostErr || !userErr || !repoDirErr {
		t.Fatalf("expected build-host strategy errors for host/user/repo_dir, got: %v", msgs)
	}
}

func TestValidateBuildStrategyGitHubActions(t *testing.T) {
	cfg := ProjectConfig{
		Name: "test",
		Build: BuildConfig{
			Strategy: "github-actions",
			// Missing repo, workflow
		},
		Services: map[string]ServiceConfig{
			"app": {Image: "app:1.0"},
		},
		Targets: map[string]TargetConfig{
			"dev": {Host: "local", RepoDir: "/tmp"},
		},
	}
	msgs := ValidateConfig(cfg)
	repoErr, workflowErr := false, false
	for _, m := range msgs {
		if strings.Contains(m, "github-actions") && strings.Contains(m, "requires repo") {
			repoErr = true
		}
		if strings.Contains(m, "github-actions") && strings.Contains(m, "requires workflow") {
			workflowErr = true
		}
	}
	if !repoErr || !workflowErr {
		t.Fatalf("expected github-actions strategy errors for repo/workflow, got: %v", msgs)
	}
}

func TestValidateReplicatedWithDependsOn(t *testing.T) {
	cfg := ProjectConfig{
		Name: "test",
		Services: map[string]ServiceConfig{
			"worker": {Image: "worker:1.0", Replicas: 3, DependsOn: []string{"db"}},
			"db":     {Image: "postgres:16.1"},
		},
		Targets: map[string]TargetConfig{
			"dev": {Host: "local", RepoDir: "/tmp"},
		},
	}
	msgs := ValidateConfig(cfg)
	found := false
	for _, m := range msgs {
		if strings.Contains(m, "replicated") && strings.Contains(m, "depends_on") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected replicated+depends_on warning, got: %v", msgs)
	}
}

func TestValidateCommandDispatch(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "myapp"
repo = "git@github.com:acme/myapp.git"
services {
  server {
    image = "server:1.0"
    depends_on = ["db"]
  }
  db {
    image = "postgres:16.1"
  }
}
targets {
  dev {
    host = "local"
    repo_dir = "/tmp/myapp"
    expose {
      8080 {
        "/" = "server:8080"
      }
    }
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := Execute([]string{"validate", "-c", cfgPath}, &out)
	if err != nil {
		t.Fatalf("validate command failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "valid") {
		t.Fatalf("expected 'valid' in output for good config, got: %q", got)
	}
}

func TestValidateCommandDispatchWithErrors(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "myapp"
repo = "git@github.com:acme/myapp.git"
services {
  server {
    image = "server:latest"
    depends_on = ["missing"]
  }
}
targets {
  dev {
    host = "local"
    repo_dir = "/tmp/myapp"
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := Execute([]string{"validate", "-c", cfgPath}, &out)
	if err == nil {
		t.Fatal("validate should return error when config has errors")
	}
	got := out.String()
	if !strings.Contains(got, "does not exist") {
		t.Fatalf("output should contain validation error, got: %q", got)
	}
}

func TestValidateHelpFlag(t *testing.T) {
	var out bytes.Buffer
	if err := Execute([]string{"validate", "--help"}, &out); err != nil {
		t.Fatalf("validate --help failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "usage: qqd validate") {
		t.Fatalf("validate help not shown: %q", got)
	}
}

func TestValidateHealthPortInferenceIsError(t *testing.T) {
	cfg := ProjectConfig{
		Name: "test",
		Services: map[string]ServiceConfig{
			"api": {Image: "api:1.0", Health: HealthConfig{Path: "/health"}},
		},
		Targets: map[string]TargetConfig{
			"dev": {
				Host:    "local",
				RepoDir: "/tmp",
				// No expose entries — port cannot be inferred
			},
		},
	}
	msgs := ValidateConfig(cfg)
	found := false
	for _, m := range msgs {
		if strings.HasPrefix(m, "error:") && strings.Contains(m, "health.path is set but port cannot be inferred") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected error (not warning) for health port inference failure, got: %v", msgs)
	}
}

func TestValidateBuildStrategyUsesMergedConfig(t *testing.T) {
	cfg := ProjectConfig{
		Name: "test",
		Build: BuildConfig{
			Strategy: "build-host",
			Host:     "build.example.com",
			User:     "builder",
			RepoDir:  "/opt/builds",
		},
		Services: map[string]ServiceConfig{
			"app": {Image: "app:1.0"},
		},
		Targets: map[string]TargetConfig{
			"prod": {
				Host:    "prod.example.com",
				User:    "deploy",
				RepoDir: "/opt/app",
				// No build block — should inherit from global
			},
		},
	}
	msgs := ValidateConfig(cfg)
	// The merged config should have all required build-host fields.
	// If validate used the raw target build (empty), it wouldn't check at all.
	// If validate used merged config, it should pass validation without errors for
	// the build strategy fields.
	for _, m := range msgs {
		if strings.Contains(m, "build strategy") && strings.Contains(m, "requires") {
			t.Fatalf("merged build config should satisfy requirements, got: %v", msgs)
		}
	}
}

// TestValidateRejectsCaddyTCPPassthrough pins the second-pass review's
// decision: a known-broken config (Caddy + raw TCP) should fail validate, not
// just produce a danger warning at plan time.
func TestValidateRejectsCaddyTCPPassthrough(t *testing.T) {
	cfg := ProjectConfig{
		Name:  "p",
		Proxy: "caddy",
		Services: map[string]ServiceConfig{
			"db": {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Host:    "h",
				RepoDir: "/x",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 5432, Target: "db:5432"}, // raw TCP
				}},
			},
		},
	}
	msgs := ValidateConfig(cfg)
	found := false
	for _, m := range msgs {
		if strings.HasPrefix(m, "error:") &&
			strings.Contains(m, "Caddy") &&
			strings.Contains(m, "TCP passthrough") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected error about Caddy + TCP passthrough; got:\n  %s", strings.Join(msgs, "\n  "))
	}
}

// TestValidateAllowsCaddyHTTPRoutes confirms the new rule is targeted: HTTP
// routes under Caddy still validate. Only raw TCP entries are blocked.
func TestValidateAllowsCaddyHTTPRoutes(t *testing.T) {
	cfg := ProjectConfig{
		Name:  "p",
		Proxy: "caddy",
		Services: map[string]ServiceConfig{
			"web": {Image: "nginx:1"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Host:    "h",
				RepoDir: "/x",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "web:80"}},
				}},
			},
		},
	}
	msgs := ValidateConfig(cfg)
	for _, m := range msgs {
		if strings.HasPrefix(m, "error:") && strings.Contains(m, "Caddy") {
			t.Fatalf("Caddy + HTTP routing should validate clean; got error:\n  %s", m)
		}
	}
}

// TestValidateAllowsTraefikTCPPassthrough confirms the rule is Caddy-specific.
func TestValidateAllowsTraefikTCPPassthrough(t *testing.T) {
	cfg := ProjectConfig{
		Name:  "p",
		Proxy: "traefik",
		Services: map[string]ServiceConfig{
			"db": {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Host:    "h",
				RepoDir: "/x",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 5432, Target: "db:5432"},
				}},
			},
		},
	}
	msgs := ValidateConfig(cfg)
	for _, m := range msgs {
		if strings.HasPrefix(m, "error:") && strings.Contains(m, "TCP passthrough") {
			t.Fatalf("Traefik + TCP passthrough should validate clean; got error:\n  %s", m)
		}
	}
}

func TestDetectCycleNoCycle(t *testing.T) {
	services := map[string]ServiceConfig{
		"a": {DependsOn: []string{"b"}},
		"b": {DependsOn: []string{"c"}},
		"c": {},
	}
	if result := detectCycle(services); result != "" {
		t.Fatalf("no cycle expected, got: %q", result)
	}
}

func TestDetectCycleSelfLoop(t *testing.T) {
	services := map[string]ServiceConfig{
		"a": {DependsOn: []string{"a"}},
	}
	result := detectCycle(services)
	if result == "" {
		t.Fatal("expected self-loop cycle")
	}
	if !strings.Contains(result, "a") {
		t.Fatalf("cycle should mention 'a', got: %q", result)
	}
}
