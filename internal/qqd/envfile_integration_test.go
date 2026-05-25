package qqd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigWithEnvFile(t *testing.T) {
	td := t.TempDir()

	// Write .env file
	envContent := "DB_HOST=localhost\nDB_PASSWORD=\"super-secret\"\n# comment\nexport API_TOKEN=abc123\n"
	envPath := filepath.Join(td, ".env")
	if err := os.WriteFile(envPath, []byte(envContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write config referencing env_file
	cfgText := `
name = "myapp"
repo = "https://github.com/test/repo.git"
env_file = ".env"
services {
    server {
        image = "ghcr.io/test/server:1.0"
        env {
            DB_HOST_VAR = "${DB_HOST}"
            API_TOKEN_VAR = "${API_TOKEN}"
        }
    }
}
targets {
    main {
        host = "local"
        repo_dir = "/opt/app"
    }
}
`
	cfgPath := filepath.Join(td, "project.conf")
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

	server := eff.Services["server"]
	if server.Env["DB_HOST_VAR"] != "localhost" {
		t.Fatalf("DB_HOST_VAR should be 'localhost', got %q", server.Env["DB_HOST_VAR"])
	}
	if server.Env["API_TOKEN_VAR"] != "abc123" {
		t.Fatalf("API_TOKEN_VAR should be 'abc123', got %q", server.Env["API_TOKEN_VAR"])
	}
}

func TestServiceEnvFile(t *testing.T) {
	td := t.TempDir()

	// Write service-level .env file
	envContent := "SVC_HOST=svc-host\nSVC_PORT=9090\n"
	envPath := filepath.Join(td, "server.env")
	if err := os.WriteFile(envPath, []byte(envContent), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgText := `
name = "myapp"
repo = "https://github.com/test/repo.git"
services {
    server {
        image = "ghcr.io/test/server:1.0"
        env_file = "server.env"
    }
}
targets {
    main {
        host = "local"
        repo_dir = "/opt/app"
    }
}
`
	cfgPath := filepath.Join(td, "project.conf")
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

	server := eff.Services["server"]
	if server.Env["SVC_HOST"] != "svc-host" {
		t.Fatalf("SVC_HOST should be 'svc-host', got %q", server.Env["SVC_HOST"])
	}
	if server.Env["SVC_PORT"] != "9090" {
		t.Fatalf("SVC_PORT should be '9090', got %q", server.Env["SVC_PORT"])
	}
}

func TestEnvFileExplicitEnvOverrides(t *testing.T) {
	td := t.TempDir()

	// Write project-level .env file
	projEnvContent := "SHARED=from-env-file\nONLY_FILE=file-value\n"
	projEnvPath := filepath.Join(td, ".env")
	if err := os.WriteFile(projEnvPath, []byte(projEnvContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write service-level .env file
	svcEnvContent := "SVC_VAR=from-svc-file\nSVC_OVERRIDE=file-value\n"
	svcEnvPath := filepath.Join(td, "server.env")
	if err := os.WriteFile(svcEnvPath, []byte(svcEnvContent), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgText := `
name = "myapp"
repo = "https://github.com/test/repo.git"
env_file = ".env"
services {
    server {
        image = "ghcr.io/test/server:1.0"
        env_file = "server.env"
        env {
            SVC_OVERRIDE = "explicit-value"
        }
    }
}
targets {
    main {
        host = "local"
        repo_dir = "/opt/app"
        env {
            SHARED = "explicit-target-value"
        }
    }
}
`
	cfgPath := filepath.Join(td, "project.conf")
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

	// Explicit target env should override project-level env_file
	if eff.Target.Env["SHARED"] != "explicit-target-value" {
		t.Fatalf("SHARED should be 'explicit-target-value', got %q", eff.Target.Env["SHARED"])
	}
	// Value only in env_file should be present
	if eff.Target.Env["ONLY_FILE"] != "file-value" {
		t.Fatalf("ONLY_FILE should be 'file-value', got %q", eff.Target.Env["ONLY_FILE"])
	}

	server := eff.Services["server"]
	// Explicit service env should override service-level env_file
	if server.Env["SVC_OVERRIDE"] != "explicit-value" {
		t.Fatalf("SVC_OVERRIDE should be 'explicit-value', got %q", server.Env["SVC_OVERRIDE"])
	}
	// Value only in service env_file should be present
	if server.Env["SVC_VAR"] != "from-svc-file" {
		t.Fatalf("SVC_VAR should be 'from-svc-file', got %q", server.Env["SVC_VAR"])
	}
}
