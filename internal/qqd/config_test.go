package qqd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAndResolveConfigWithSecrets(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	secPath := filepath.Join(td, "secrets.conf")

	cfgText := `
name = "report-service"
repo = "git@github.com:acme/report.git"
branch = "master"
build {
  strategy = "local"
  cpu = 2
  memory = "2g"
}
services {
  server {
    image = "ghcr.io/acme/report/server:1.44"
    dockerfile = "backend/server/Dockerfile"
    volumes = ["${DATA_DIR}:/app/data"]
    env {
      DB_URL = "db:5432"
      TOKEN = "${SECRET_TOKEN}"
    }
  }
  db {
    image = "postgres:16.1"
  }
}
targets {
  main {
    host = "192.0.2.20"
    user = "centos"
    ssh_key = "keys/my-key"
    repo_dir = "/home/centos/report/repo"
    dirs = ["/home/centos/report/data"]
    env {
      DATA_DIR = "/home/centos/report/data"
    }
    overrides {
      server {
        env {
          DB_URL = "192.0.2.20:5432"
        }
      }
    }
    build {
      cpu = 4
    }
  }
}
`
	secText := `
targets.main.env {
  SECRET_TOKEN = "abc123"
}
targets.main.overrides.server.env {
  API_KEY = "secret-key"
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secPath, []byte(secText), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadProjectConfig([]string{cfgPath, secPath}, td)
	if err != nil {
		t.Fatalf("loadProjectConfig failed: %v", err)
	}
	main := cfg.Targets["main"]
	wantSSH := filepath.Join(td, "keys/my-key")
	if main.SSHKey != wantSSH {
		t.Fatalf("ssh key path mismatch: got %q want %q", main.SSHKey, wantSSH)
	}

	eff, err := resolveTarget(cfg, "main", nil)
	if err != nil {
		t.Fatalf("resolveTarget failed: %v", err)
	}
	if eff.Build.CPU != 4 {
		t.Fatalf("build cpu override mismatch: got %d", eff.Build.CPU)
	}
	server := eff.Services["server"]
	if server.Env["DB_URL"] != "192.0.2.20:5432" {
		t.Fatalf("env override not applied: %q", server.Env["DB_URL"])
	}
	if server.Env["TOKEN"] != "abc123" {
		t.Fatalf("secret variable substitution failed: %q", server.Env["TOKEN"])
	}
	if server.Env["API_KEY"] != "secret-key" {
		t.Fatalf("secret override missing: %q", server.Env["API_KEY"])
	}
	if got := server.Volumes[0]; got != "/home/centos/report/data:/app/data" {
		t.Fatalf("volume substitution failed: %q", got)
	}
}

// TestRelativePathsResolveAgainstConfigDir pins the contract that paths
// inside a config file resolve against the directory of the first -c file,
// not against the shell cwd. The user can run `qqd deploy` from anywhere
// and the deploy behaves the same.
func TestRelativePathsResolveAgainstConfigDir(t *testing.T) {
	root := t.TempDir()
	cfgDir := filepath.Join(root, "deploy", "prod")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfgDir, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "keys", "id"), []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, ".env"), []byte("SHARED=value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "app.conf")
	cfgText := `
name = "demo"
repo = "git@example.com:demo.git"
env_file = [".env"]
services {
  api {
    image = "demo/api:1"
    dockerfile = "backend/api/Dockerfile"
    context = "backend/api"
  }
}
targets {
  prod {
    host = "192.0.2.10"
    user = "deploy"
    ssh_key = "keys/id"
    repo_dir = "/srv/demo"
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pretend the user invoked qqd from somewhere completely unrelated.
	bogusCwd := filepath.Join(root, "some", "other", "dir")
	if err := os.MkdirAll(bogusCwd, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadProjectConfig([]string{cfgPath}, bogusCwd)
	if err != nil {
		t.Fatalf("loadProjectConfig: %v", err)
	}
	if cfg.InvocationWD != cfgDir {
		t.Fatalf("InvocationWD should be the config dir %q, got %q", cfgDir, cfg.InvocationWD)
	}
	wantKey := filepath.Join(cfgDir, "keys", "id")
	if got := cfg.Targets["prod"].SSHKey; got != wantKey {
		t.Fatalf("ssh_key should resolve against config dir: got %q want %q", got, wantKey)
	}
	eff, err := resolveTarget(cfg, "prod", nil)
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if got := eff.Target.Env["SHARED"]; got != "value" {
		t.Fatalf("env_file %q should have loaded SHARED=value, got %q (full env: %+v)", filepath.Join(cfgDir, ".env"), got, eff.Target.Env)
	}
}

// TestRelativeCLIPathsStayCwdRelative verifies that the -c arguments
// themselves are still resolved against the shell cwd (they are typed by
// the user at the prompt; only paths *inside* the config become
// config-relative).
func TestRelativeCLIPathsStayCwdRelative(t *testing.T) {
	root := t.TempDir()
	cfgDir := filepath.Join(root, "configs")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "app.conf")
	if err := os.WriteFile(cfgPath, []byte(`name = "x"
repo = "x"
services { web { image = "img:1" } }
targets { prod { host = "192.0.2.10", user = "u", repo_dir = "/srv/x" } }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Cwd is `root`; user passes `-c configs/app.conf`.
	cfg, err := loadProjectConfig([]string{"configs/app.conf"}, root)
	if err != nil {
		t.Fatalf("loadProjectConfig: %v", err)
	}
	if cfg.InvocationWD != cfgDir {
		t.Fatalf("InvocationWD should still resolve to config dir, got %q", cfg.InvocationWD)
	}
}

func TestPureImageDeployDoesNotRequireRepoOrRepoDir(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.yaml")
	cfgText := `name: nginx-local
services:
  web:
    image: docker.io/library/nginx:1
targets:
  local:
    host: local
    expose:
      18080:
        "/": "web:80"
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadProjectConfig([]string{cfgPath}, td)
	if err != nil {
		t.Fatalf("pure image-pull config should load without repo/repo_dir: %v", err)
	}
	if cfg.Targets["local"].RepoDir != "" {
		t.Fatalf("RepoDir should remain empty, got %q", cfg.Targets["local"].RepoDir)
	}
	if cfg.needsSource() {
		t.Fatal("needsSource() should be false when no service has a build context")
	}
}

func TestRepoStillRequiredWhenServiceBuildsFromSource(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.yaml")
	cfgText := `name: app
services:
  api:
    image: app/api:1
    dockerfile: Dockerfile
targets:
  local:
    host: local
    repo_dir: /tmp/app
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadProjectConfig([]string{cfgPath}, td)
	if err == nil {
		t.Fatal("config with a Dockerfile but no repo should still fail validation")
	}
	if !strings.Contains(err.Error(), "repo is required") {
		t.Fatalf("expected repo-required error, got: %v", err)
	}
}

func TestRepoDirStillRequiredWhenServiceBuildsFromSource(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.yaml")
	cfgText := `name: app
repo: "https://example.com/r.git"
services:
  api:
    image: app/api:1
    dockerfile: Dockerfile
targets:
  local:
    host: local
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadProjectConfig([]string{cfgPath}, td)
	if err == nil {
		t.Fatal("config with a build context but no repo_dir should fail validation")
	}
	if !strings.Contains(err.Error(), "repo_dir is required") {
		t.Fatalf("expected repo_dir-required error, got: %v", err)
	}
}

func TestLocalTargetDoesNotRequireUser(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "myapp"
repo = "git@github.com:acme/myapp.git"
services {
  app {
    image = "myapp:1.0"
    dockerfile = "Dockerfile"
  }
}
targets {
  dev {
    host = "local"
    repo_dir = "/tmp/myapp/repo"
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadProjectConfig([]string{cfgPath}, td)
	if err != nil {
		t.Fatalf("loadProjectConfig should accept local target without user: %v", err)
	}
	dev := cfg.Targets["dev"]
	if dev.Host != "local" {
		t.Fatalf("host mismatch: %q", dev.Host)
	}
	if dev.User != "" {
		t.Fatalf("user should be empty for local target: %q", dev.User)
	}
}

func TestRemoteTargetRequiresUser(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "myapp"
repo = "git@github.com:acme/myapp.git"
services {
  app {
    image = "myapp:1.0"
  }
}
targets {
  prod {
    host = "192.0.2.10"
    repo_dir = "/home/deploy/myapp"
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadProjectConfig([]string{cfgPath}, td)
	if err == nil {
		t.Fatalf("should fail when remote target has no user")
	}
}

func TestParseExposeHTTPAndTCP(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "proj"
repo = "git@github.com:acme/proj.git"
services {
  server {
    image = "server:1.0"
    replicas = 3
    health { path = "/api/v1/health", port = 8080 }
    resources { cpus = "2", memory = "1g" }
    depends_on = ["db"]
  }
  frontend {
    image = "frontend:1.0"
  }
  db {
    image = "postgres:16.1"
  }
}
targets {
  main {
    host = "local"
    repo_dir = "/tmp/proj"
    expose {
      9999 {
        "/api/" = "server:8080"
        "/" = "frontend:80"
      }
      5432 = "db:5432"
    }
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadProjectConfig([]string{cfgPath}, td)
	if err != nil {
		t.Fatalf("loadProjectConfig failed: %v", err)
	}
	server := cfg.Services["server"]
	if server.Replicas != 3 {
		t.Fatalf("replicas mismatch: %d", server.Replicas)
	}
	if server.Health.Path != "/api/v1/health" || server.Health.Port != 8080 {
		t.Fatalf("health mismatch: %+v", server.Health)
	}
	if server.Resources.CPUs != "2" || server.Resources.Memory != "1g" {
		t.Fatalf("resources mismatch: %+v", server.Resources)
	}
	if len(cfg.Targets["main"].Expose.Entries) != 2 {
		t.Fatalf("expose entries count mismatch: %d", len(cfg.Targets["main"].Expose.Entries))
	}
	// Entries are sorted by port: 5432, 9999
	tcp := cfg.Targets["main"].Expose.Entries[0]
	if tcp.HostPort != 5432 || tcp.Target != "db:5432" {
		t.Fatalf("TCP entry mismatch: %+v", tcp)
	}
	http := cfg.Targets["main"].Expose.Entries[1]
	if http.HostPort != 9999 || len(http.Routes) != 2 {
		t.Fatalf("HTTP entry mismatch: %+v", http)
	}
	if http.Routes["/api/"] != "server:8080" {
		t.Fatalf("HTTP route mismatch: %v", http.Routes)
	}
}

func TestParseExposeWithTLS(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "proj"
repo = "git@github.com:acme/proj.git"
services {
  server {
    image = "server:1.0"
    replicas = 2
  }
}
targets {
  main {
    host = "local"
    repo_dir = "/tmp/proj"
    env { CERTS_DIR = "/etc/letsencrypt" }
    expose {
      80 {
        "/" = "server:8080"
        tls {
          port = 443
          certs_dir = "${CERTS_DIR}"
          server_name = "example.com"
        }
      }
    }
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadProjectConfig([]string{cfgPath}, td)
	if err != nil {
		t.Fatalf("loadProjectConfig failed: %v", err)
	}
	if len(cfg.Targets["main"].Expose.Entries) != 1 {
		t.Fatalf("expose entries count mismatch: %d", len(cfg.Targets["main"].Expose.Entries))
	}
	entry := cfg.Targets["main"].Expose.Entries[0]
	if entry.TLS == nil {
		t.Fatal("TLS should be set")
	}
	if entry.TLS.Port != 443 {
		t.Fatalf("TLS port mismatch: %d", entry.TLS.Port)
	}
	if entry.TLS.ServerName != "example.com" {
		t.Fatalf("TLS server_name mismatch: %q", entry.TLS.ServerName)
	}
	// CertsDir has var — should be expanded in resolveTarget
	eff, err := resolveTarget(cfg, "main", nil)
	if err != nil {
		t.Fatalf("resolveTarget failed: %v", err)
	}
	if eff.Expose.Entries[0].TLS.CertsDir != "/etc/letsencrypt" {
		t.Fatalf("TLS certs_dir var expansion failed: %q", eff.Expose.Entries[0].TLS.CertsDir)
	}
}

func TestValidationExposeBadServiceRef(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "proj"
repo = "git@github.com:acme/proj.git"
services {
  server { image = "server:1.0" }
}
targets {
  main {
    host = "local", repo_dir = "/tmp"
    expose {
      9999 {
        "/" = "nonexistent:8080"
      }
    }
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadProjectConfig([]string{cfgPath}, td)
	if err == nil {
		t.Fatal("should fail when expose references unknown service")
	}
	if !strings.Contains(err.Error(), "unknown service") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidationExposeTCPBadServiceRef(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "proj"
repo = "git@github.com:acme/proj.git"
services {
  db { image = "postgres:16.1" }
}
targets {
  main {
    host = "local", repo_dir = "/tmp"
    expose {
      5432 = "missing:5432"
    }
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadProjectConfig([]string{cfgPath}, td)
	if err == nil {
		t.Fatal("should fail when TCP expose references unknown service")
	}
	if !strings.Contains(err.Error(), "unknown service") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidationTLSMissingServerName(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "proj"
repo = "git@github.com:acme/proj.git"
services {
  server { image = "server:1.0" }
}
targets {
  main {
    host = "local", repo_dir = "/tmp"
    expose {
      80 {
        "/" = "server:8080"
        tls { certs_dir = "/etc/letsencrypt" }
      }
    }
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadProjectConfig([]string{cfgPath}, td)
	if err == nil {
		t.Fatal("should fail when TLS has certs_dir but no server_name")
	}
	if !strings.Contains(err.Error(), "certs_dir and server_name") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveFileRef(t *testing.T) {
	td := t.TempDir()
	secretFile := filepath.Join(td, "secret.json")
	if err := os.WriteFile(secretFile, []byte("  secret-content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveFileRef(td, "file::"+secretFile)
	if err != nil {
		t.Fatalf("resolveFileRef failed: %v", err)
	}
	if got != "secret-content" {
		t.Fatalf("expected trimmed content, got %q", got)
	}
}

func TestResolveFileRefPassthrough(t *testing.T) {
	got, err := resolveFileRef("/tmp", "plain-value")
	if err != nil {
		t.Fatalf("resolveFileRef failed: %v", err)
	}
	if got != "plain-value" {
		t.Fatalf("expected passthrough, got %q", got)
	}
}

func TestResolveFileRefPreservesNewlines(t *testing.T) {
	td := t.TempDir()
	secretFile := filepath.Join(td, "key.json")
	content := "{\n\t\"type\": \"service_account\",\n\t\"private_key\": \"-----BEGIN-----\\nMIIE\\n-----END-----\\n\"\n}\n"
	if err := os.WriteFile(secretFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveFileRef(td, "file::"+secretFile)
	if err != nil {
		t.Fatalf("resolveFileRef failed: %v", err)
	}
	// resolveFileRef preserves raw content (just trims); quoting is done by formatQuadletEnv
	if !strings.Contains(got, "\"type\": \"service_account\"") {
		t.Fatalf("expected JSON content preserved, got %q", got)
	}
	if !strings.Contains(got, "\n") {
		t.Fatalf("expected newlines preserved in raw file ref, got %q", got)
	}
}

func TestResolveFileRefMissingFile(t *testing.T) {
	_, err := resolveFileRef("/tmp", "file::/nonexistent/path/secret.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestResolveTargetFileRef(t *testing.T) {
	td := t.TempDir()
	secretFile := filepath.Join(td, "gcp-key.json")
	if err := os.WriteFile(secretFile, []byte(`{"type":"service_account"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "myapp"
repo = "git@github.com:acme/myapp.git"
services {
  server {
    image = "server:1.0"
    env {
      GCP_KEY = "${GCP_SERVICE_ACCOUNT_KEY}"
    }
  }
}
targets {
  main {
    host = "local"
    repo_dir = "/tmp/myapp"
    env {
      GCP_SERVICE_ACCOUNT_KEY = "file::` + secretFile + `"
    }
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadProjectConfig([]string{cfgPath}, td)
	if err != nil {
		t.Fatalf("loadProjectConfig failed: %v", err)
	}
	eff, err := resolveTarget(cfg, "main", nil)
	if err != nil {
		t.Fatalf("resolveTarget failed: %v", err)
	}
	got := eff.Services["server"].Env["GCP_KEY"]
	if got != `{"type":"service_account"}` {
		t.Fatalf("file ref not expanded into service env: got %q", got)
	}
}

// TestResolveTargetFileRefMultilineJSON verifies the full pipeline:
// multiline JSON file → file:: ref → resolveFileRef → renderContainer → properly escaped quadlet.
func TestResolveTargetFileRefMultilineJSON(t *testing.T) {
	td := t.TempDir()
	gcpContent := `{
  "type": "service_account",
  "project_id": "my-project",
  "private_key": "FAKE_MULTILINE_PRIVATE_KEY\nLINE2\n",
  "client_email": "sa@my-project.iam.gserviceaccount.com",
  "client_x509_cert_url": "https://www.googleapis.com/robot/v1/metadata/x509/sa%40my-project.iam.gserviceaccount.com"
}`
	secretFile := filepath.Join(td, "gcp-key.json")
	if err := os.WriteFile(secretFile, []byte(gcpContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "myapp"
repo = "git@github.com:acme/myapp.git"
services {
  server {
    image = "server:1.0"
    env {
      GCP_KEY = "${GCP_SERVICE_ACCOUNT_KEY}"
      SIMPLE = "hello"
    }
  }
}
targets {
  main {
    host = "local"
    repo_dir = "/tmp/myapp"
    env {
      GCP_SERVICE_ACCOUNT_KEY = "file::` + secretFile + `"
    }
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadProjectConfig([]string{cfgPath}, td)
	if err != nil {
		t.Fatalf("loadProjectConfig failed: %v", err)
	}
	eff, err := resolveTarget(cfg, "main", nil)
	if err != nil {
		t.Fatalf("resolveTarget failed: %v", err)
	}

	// 1. Verify resolveFileRef preserved the raw JSON content (with newlines)
	gcpVal := eff.Services["server"].Env["GCP_KEY"]
	if !strings.Contains(gcpVal, "\"type\": \"service_account\"") {
		t.Fatalf("file ref should preserve JSON content, got %q", gcpVal)
	}
	if !strings.Contains(gcpVal, "\n") {
		t.Fatalf("file ref should preserve newlines, got %q", gcpVal)
	}
	if !strings.Contains(gcpVal, "%40") {
		t.Fatalf("file ref should preserve URL-encoded characters, got %q", gcpVal)
	}

	// 2. Render the container quadlet and verify the Environment= line is correct
	rendered := renderContainer("myapp", "server", eff.Services["server"])

	// GCP_KEY must be on a single line, quoted, with proper escaping
	foundGCP := false
	for _, line := range strings.Split(rendered, "\n") {
		if strings.Contains(line, "GCP_KEY") {
			foundGCP = true
			// Must be quoted form
			if !strings.HasPrefix(line, `Environment="GCP_KEY=`) {
				t.Fatalf("multiline env should use quoted form:\n%s", line)
			}
			// Must contain escaped newlines
			if !strings.Contains(line, `\n`) {
				t.Fatalf("should have escaped newlines:\n%s", line)
			}
			// Must contain escaped quotes
			if !strings.Contains(line, `\"type\"`) {
				t.Fatalf("should have escaped quotes:\n%s", line)
			}
			// Must have %% for systemd percent escaping
			if !strings.Contains(line, "%%40") {
				t.Fatalf("should have %%%% for systemd percent escaping:\n%s", line)
			}
			break
		}
	}
	if !foundGCP {
		t.Fatalf("GCP_KEY line not found in rendered quadlet:\n%s", rendered)
	}

	// Simple env should be unquoted
	if !strings.Contains(rendered, "Environment=SIMPLE=hello") {
		t.Fatalf("simple env should be unquoted:\n%s", rendered)
	}
}

func TestParseTarget(t *testing.T) {
	svc, port, err := parseTarget("server:8080")
	if err != nil {
		t.Fatalf("parseTarget failed: %v", err)
	}
	if svc != "server" || port != 8080 {
		t.Fatalf("parseTarget mismatch: svc=%q port=%d", svc, port)
	}
	_, _, err = parseTarget("invalid")
	if err == nil {
		t.Fatal("parseTarget should fail for string without colon")
	}
	_, _, err = parseTarget("svc:abc")
	if err == nil {
		t.Fatal("parseTarget should fail for non-numeric port")
	}
}

func TestMultipleTargetsDifferentServicesAndExpose(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "proj"
repo = "git@github.com:acme/proj.git"
services {
  server {
    image = "server:1.0"
    dockerfile = "Dockerfile"
  }
  db {
    image = "postgres:16.1"
  }
  metrics {
    image = "victoria:1.0"
  }
}
targets {
  dev {
    host = "local"
    repo_dir = "/tmp/proj-dev"
    services = ["server", "db"]
    expose {
      8080 {
        "/" = "server:8080"
      }
    }
  }
  prod {
    host = "192.0.2.10"
    user = "deploy"
    repo_dir = "/home/deploy/proj"
    expose {
      80 {
        "/api/" = "server:8080"
      }
      5432 = "db:5432"
      8428 = "metrics:8428"
    }
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadProjectConfig([]string{cfgPath}, td)
	if err != nil {
		t.Fatalf("loadProjectConfig failed: %v", err)
	}

	// dev target: only server+db, one HTTP expose entry
	devExpose := cfg.Targets["dev"].Expose
	if len(devExpose.Entries) != 1 {
		t.Fatalf("dev expose entries: got %d, want 1", len(devExpose.Entries))
	}
	if devExpose.Entries[0].HostPort != 8080 {
		t.Fatalf("dev expose port: got %d, want 8080", devExpose.Entries[0].HostPort)
	}

	effDev, err := resolveTarget(cfg, "dev", nil)
	if err != nil {
		t.Fatalf("resolveTarget dev failed: %v", err)
	}
	if len(effDev.Services) != 2 {
		t.Fatalf("dev services count: got %d, want 2", len(effDev.Services))
	}
	if _, ok := effDev.Services["server"]; !ok {
		t.Fatal("dev should include server service")
	}
	if _, ok := effDev.Services["db"]; !ok {
		t.Fatal("dev should include db service")
	}
	if _, ok := effDev.Services["metrics"]; ok {
		t.Fatal("dev should NOT include metrics service")
	}
	if len(effDev.Expose.Entries) != 1 {
		t.Fatalf("dev resolved expose entries: got %d, want 1", len(effDev.Expose.Entries))
	}

	// prod target: all services, three expose entries (HTTP + two TCP)
	prodExpose := cfg.Targets["prod"].Expose
	if len(prodExpose.Entries) != 3 {
		t.Fatalf("prod expose entries: got %d, want 3", len(prodExpose.Entries))
	}

	effProd, err := resolveTarget(cfg, "prod", nil)
	if err != nil {
		t.Fatalf("resolveTarget prod failed: %v", err)
	}
	if len(effProd.Services) != 3 {
		t.Fatalf("prod services count: got %d, want 3", len(effProd.Services))
	}
	if len(effProd.Expose.Entries) != 3 {
		t.Fatalf("prod resolved expose entries: got %d, want 3", len(effProd.Expose.Entries))
	}
	// Entries sorted lexicographically by key: "5432", "80", "8428"
	if effProd.Expose.Entries[0].HostPort != 5432 || effProd.Expose.Entries[0].Target != "db:5432" {
		t.Fatalf("prod expose[0] mismatch: %+v", effProd.Expose.Entries[0])
	}
	if effProd.Expose.Entries[1].HostPort != 80 || effProd.Expose.Entries[1].Routes["/api/"] != "server:8080" {
		t.Fatalf("prod expose[1] mismatch: %+v", effProd.Expose.Entries[1])
	}
	if effProd.Expose.Entries[2].HostPort != 8428 || effProd.Expose.Entries[2].Target != "metrics:8428" {
		t.Fatalf("prod expose[2] mismatch: %+v", effProd.Expose.Entries[2])
	}
}

func TestReplicasWithoutRoutes(t *testing.T) {
	// Replicas without routes should now be allowed (TCP through expose)
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "proj"
repo = "git@github.com:acme/proj.git"
services {
  db {
    image = "postgres:16.1"
    replicas = 3
  }
}
targets {
  main {
    host = "local", repo_dir = "/tmp"
    expose {
      5432 = "db:5432"
    }
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadProjectConfig([]string{cfgPath}, td)
	if err != nil {
		t.Fatalf("replicas without routes should be allowed: %v", err)
	}
	if cfg.Services["db"].Replicas != 3 {
		t.Fatalf("replicas mismatch: %d", cfg.Services["db"].Replicas)
	}
}

func TestDefaultBranchIsMain(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "myapp"
repo = "git@github.com:acme/myapp.git"
services {
  app { image = "myapp:1.0" }
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
	cfg, err := loadProjectConfig([]string{cfgPath}, td)
	if err != nil {
		t.Fatalf("loadProjectConfig failed: %v", err)
	}
	if cfg.Branch != "main" {
		t.Fatalf("default branch should be 'main', got %q", cfg.Branch)
	}
}

func TestHealthShorthandInfersPortFromExpose(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "myapp"
repo = "git@github.com:acme/myapp.git"
services {
  server {
    image = "server:1.0"
    health = "/api/health"
  }
}
targets {
  prod {
    host = "local"
    repo_dir = "/tmp/myapp"
    expose {
      80 {
        "/api/" = "server:8080"
      }
    }
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadProjectConfig([]string{cfgPath}, td)
	if err != nil {
		t.Fatalf("loadProjectConfig failed: %v", err)
	}
	// At config level, Port should still be 0 (shorthand)
	if cfg.Services["server"].Health.Port != 0 {
		t.Fatalf("raw config should have Port=0, got %d", cfg.Services["server"].Health.Port)
	}
	// After resolveTarget, Port should be inferred from expose
	eff, err := resolveTarget(cfg, "prod", nil)
	if err != nil {
		t.Fatalf("resolveTarget failed: %v", err)
	}
	if eff.Services["server"].Health.Port != 8080 {
		t.Fatalf("health port should be inferred as 8080, got %d", eff.Services["server"].Health.Port)
	}
	if eff.Services["server"].Health.Path != "/api/health" {
		t.Fatalf("health path should be preserved, got %q", eff.Services["server"].Health.Path)
	}
}

func TestHealthShorthandRequiresHTTPExposeOrExplicitPort(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "myapp"
repo = "git@github.com:acme/myapp.git"
services {
  server {
    image = "server:1.0"
    health = "/api/health"
  }
}
targets {
  prod {
    host = "local"
    repo_dir = "/tmp/myapp"
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadProjectConfig([]string{cfgPath}, td)
	if err != nil {
		t.Fatalf("loadProjectConfig failed: %v", err)
	}
	_, err = resolveTarget(cfg, "prod", nil)
	if err == nil {
		t.Fatal("resolveTarget should fail when shorthand health cannot infer a port")
	}
	if !strings.Contains(err.Error(), "cannot infer port") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHealthShorthandRejectsAmbiguousHTTPExposePorts(t *testing.T) {
	td := t.TempDir()
	cfgPath := filepath.Join(td, "project.conf")
	cfgText := `
name = "myapp"
repo = "git@github.com:acme/myapp.git"
services {
  server {
    image = "server:1.0"
    health = "/api/health"
  }
}
targets {
  prod {
    host = "local"
    repo_dir = "/tmp/myapp"
    expose {
      80 {
        "/api/" = "server:8080"
        "/admin/" = "server:9090"
      }
    }
  }
}
`
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadProjectConfig([]string{cfgPath}, td)
	if err != nil {
		t.Fatalf("loadProjectConfig failed: %v", err)
	}
	_, err = resolveTarget(cfg, "prod", nil)
	if err == nil {
		t.Fatal("resolveTarget should fail when shorthand health port is ambiguous")
	}
	if !strings.Contains(err.Error(), "ambiguous HTTP expose ports") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnvFilePrecedenceLastWins(t *testing.T) {
	td := t.TempDir()

	// First env file
	env1Path := filepath.Join(td, "base.env")
	os.WriteFile(env1Path, []byte("DB_HOST=localhost\nDB_PORT=5432\n"), 0o644)

	// Second env file overrides DB_HOST
	env2Path := filepath.Join(td, "override.env")
	os.WriteFile(env2Path, []byte("DB_HOST=prod-db.example.com\n"), 0o644)

	cfgPath := filepath.Join(td, "project.conf")
	cfgContent := fmt.Sprintf(`
name = "myapp"
repo = "https://github.com/test/repo.git"
env_file = ["%s", "%s"]
services {
    server {
        image = "server:1.0"
    }
}
targets {
    prod {
        host = "192.0.2.10"
        user = "deploy"
        repo_dir = "/opt/app"
    }
}
`, env1Path, env2Path)
	os.WriteFile(cfgPath, []byte(cfgContent), 0o644)

	cfg, err := loadProjectConfig([]string{cfgPath}, td)
	if err != nil {
		t.Fatalf("loadProjectConfig: %v", err)
	}

	eff, err := resolveTarget(cfg, "prod", nil)
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}

	// Later file should win
	if eff.Target.Env["DB_HOST"] != "prod-db.example.com" {
		t.Fatalf("DB_HOST = %q, want prod-db.example.com (last file should win)", eff.Target.Env["DB_HOST"])
	}
	// Value only in first file should still be present
	if eff.Target.Env["DB_PORT"] != "5432" {
		t.Fatalf("DB_PORT = %q, want 5432", eff.Target.Env["DB_PORT"])
	}
}

func TestEnvFileExplicitEnvTakesPriority(t *testing.T) {
	td := t.TempDir()

	envPath := filepath.Join(td, "base.env")
	os.WriteFile(envPath, []byte("DB_HOST=from-file\n"), 0o644)

	cfgPath := filepath.Join(td, "project.conf")
	cfgContent := fmt.Sprintf(`
name = "myapp"
repo = "https://github.com/test/repo.git"
env_file = ["%s"]
services {
    server {
        image = "server:1.0"
    }
}
targets {
    prod {
        host = "192.0.2.10"
        user = "deploy"
        repo_dir = "/opt/app"
        env {
            DB_HOST = "explicit-value"
        }
    }
}
`, envPath)
	os.WriteFile(cfgPath, []byte(cfgContent), 0o644)

	cfg, err := loadProjectConfig([]string{cfgPath}, td)
	if err != nil {
		t.Fatalf("loadProjectConfig: %v", err)
	}

	eff, err := resolveTarget(cfg, "prod", nil)
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}

	// Explicit env should override file env
	if eff.Target.Env["DB_HOST"] != "explicit-value" {
		t.Fatalf("DB_HOST = %q, want explicit-value (explicit env should override file)", eff.Target.Env["DB_HOST"])
	}
}
