package qqd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	td := t.TempDir()
	envPath := filepath.Join(td, ".env")
	content := "DB_HOST=localhost\nDB_PORT=5432\n"
	if err := os.WriteFile(envPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := loadEnvFile(envPath)
	if err != nil {
		t.Fatalf("loadEnvFile failed: %v", err)
	}
	if env["DB_HOST"] != "localhost" {
		t.Fatalf("DB_HOST mismatch: got %q", env["DB_HOST"])
	}
	if env["DB_PORT"] != "5432" {
		t.Fatalf("DB_PORT mismatch: got %q", env["DB_PORT"])
	}
}

func TestLoadEnvFileQuoted(t *testing.T) {
	td := t.TempDir()
	envPath := filepath.Join(td, ".env")
	content := `DB_URL="postgres://user:pass@host/db"
GREETING="hello world"
`
	if err := os.WriteFile(envPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := loadEnvFile(envPath)
	if err != nil {
		t.Fatalf("loadEnvFile failed: %v", err)
	}
	if env["DB_URL"] != "postgres://user:pass@host/db" {
		t.Fatalf("DB_URL mismatch: got %q", env["DB_URL"])
	}
	if env["GREETING"] != "hello world" {
		t.Fatalf("GREETING mismatch: got %q", env["GREETING"])
	}
}

func TestLoadEnvFileSingleQuoted(t *testing.T) {
	td := t.TempDir()
	envPath := filepath.Join(td, ".env")
	content := "SECRET='my secret value'\n"
	if err := os.WriteFile(envPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := loadEnvFile(envPath)
	if err != nil {
		t.Fatalf("loadEnvFile failed: %v", err)
	}
	if env["SECRET"] != "my secret value" {
		t.Fatalf("SECRET mismatch: got %q", env["SECRET"])
	}
}

func TestLoadEnvFileComments(t *testing.T) {
	td := t.TempDir()
	envPath := filepath.Join(td, ".env")
	content := "# This is a comment\nKEY=value\n# Another comment\n"
	if err := os.WriteFile(envPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := loadEnvFile(envPath)
	if err != nil {
		t.Fatalf("loadEnvFile failed: %v", err)
	}
	if len(env) != 1 {
		t.Fatalf("expected 1 key, got %d", len(env))
	}
	if env["KEY"] != "value" {
		t.Fatalf("KEY mismatch: got %q", env["KEY"])
	}
}

func TestLoadEnvFileExportPrefix(t *testing.T) {
	td := t.TempDir()
	envPath := filepath.Join(td, ".env")
	content := "export API_TOKEN=abc123\nexport DB_HOST=localhost\n"
	if err := os.WriteFile(envPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := loadEnvFile(envPath)
	if err != nil {
		t.Fatalf("loadEnvFile failed: %v", err)
	}
	if env["API_TOKEN"] != "abc123" {
		t.Fatalf("API_TOKEN mismatch: got %q", env["API_TOKEN"])
	}
	if env["DB_HOST"] != "localhost" {
		t.Fatalf("DB_HOST mismatch: got %q", env["DB_HOST"])
	}
}

func TestLoadEnvFileEmptyLines(t *testing.T) {
	td := t.TempDir()
	envPath := filepath.Join(td, ".env")
	content := "\nKEY1=val1\n\n\nKEY2=val2\n\n"
	if err := os.WriteFile(envPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := loadEnvFile(envPath)
	if err != nil {
		t.Fatalf("loadEnvFile failed: %v", err)
	}
	if len(env) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(env))
	}
	if env["KEY1"] != "val1" || env["KEY2"] != "val2" {
		t.Fatalf("unexpected values: %v", env)
	}
}

func TestLoadEnvFileNotFound(t *testing.T) {
	_, err := loadEnvFile("/nonexistent/path/.env")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadEnvFileEmptyValue(t *testing.T) {
	td := t.TempDir()
	envPath := filepath.Join(td, ".env")
	content := "EMPTY_KEY=\n"
	if err := os.WriteFile(envPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := loadEnvFile(envPath)
	if err != nil {
		t.Fatalf("loadEnvFile failed: %v", err)
	}
	val, exists := env["EMPTY_KEY"]
	if !exists {
		t.Fatal("EMPTY_KEY should exist")
	}
	if val != "" {
		t.Fatalf("EMPTY_KEY should be empty string, got %q", val)
	}
}
