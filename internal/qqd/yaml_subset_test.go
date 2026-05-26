package qqd

import (
	"reflect"
	"strings"
	"testing"
)

// These tests pin the documented YAML subset from docs/yaml-subset.md. If you
// extend the parser, add a "supported" case here. If you find a YAML feature
// the parser silently miscompiles, add an "unsupported" case here that asserts
// the parser returns a clear error.

func TestYAMLAnchorsAreRejected(t *testing.T) {
	in := `
defaults: &defaults
  image: nginx:1
server: *defaults
`
	_, err := parseYAML([]byte(in))
	if err == nil {
		t.Fatal("expected anchors to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "anchors") {
		t.Fatalf("error should mention 'anchors', got: %v", err)
	}
}

func TestYAMLAliasesAreRejected(t *testing.T) {
	in := `
target: *some_alias
`
	_, err := parseYAML([]byte(in))
	if err == nil || !strings.Contains(err.Error(), "aliases") {
		t.Fatalf("expected aliases to be rejected, got: %v", err)
	}
}

func TestYAMLMultilineScalarsAreRejected(t *testing.T) {
	for _, marker := range []string{"|", ">", "|-", ">+"} {
		in := "script: " + marker + "\n  echo hello\n"
		_, err := parseYAML([]byte(in))
		if err == nil || !strings.Contains(err.Error(), "multi-line") {
			t.Errorf("marker %q: expected multi-line error, got: %v", marker, err)
		}
	}
}

func TestYAMLTagsAreRejected(t *testing.T) {
	in := `value: !!str 1`
	_, err := parseYAML([]byte(in))
	if err == nil || !strings.Contains(err.Error(), "tags") {
		t.Fatalf("expected tags to be rejected, got: %v", err)
	}
}

func TestYAMLMergeKeysAreRejected(t *testing.T) {
	in := `
service:
  <<: *base
  image: x
`
	_, err := parseYAML([]byte(in))
	if err == nil || !strings.Contains(err.Error(), "merge") {
		t.Fatalf("expected merge keys to be rejected, got: %v", err)
	}
}

func TestYAMLFlowMapsAreRejected(t *testing.T) {
	in := `value: {a: 1, b: 2}`
	_, err := parseYAML([]byte(in))
	if err == nil || !strings.Contains(err.Error(), "flow maps") {
		t.Fatalf("expected flow maps to be rejected, got: %v", err)
	}
}

func TestYAMLDocumentSeparatorRejected(t *testing.T) {
	in := `
---
name: x
`
	_, err := parseYAML([]byte(in))
	if err == nil || !strings.Contains(err.Error(), "multi-document") {
		t.Fatalf("expected --- to be rejected, got: %v", err)
	}
}

func TestYAMLAnchorMarkerInsideQuotedStringIsNotRejected(t *testing.T) {
	// `&` inside a quoted value is data, not an anchor.
	in := `password: "p&ss"`
	m, err := parseYAML([]byte(in))
	if err != nil {
		t.Fatalf("quoted &-containing string should parse, got: %v", err)
	}
	if m["password"] != "p&ss" {
		t.Fatalf("password = %v, want %q", m["password"], "p&ss")
	}
}

func TestYAMLEmptyValueProducesEmptyString(t *testing.T) {
	in := `password: ""`
	m, err := parseYAML([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if m["password"] != "" {
		t.Fatalf("password = %v, want empty string", m["password"])
	}
}

// TestYAMLRealisticConfigParses verifies a representative qqd config (the one
// from the README quick-start, slightly trimmed) round-trips through the
// parser without losing structure.
func TestYAMLRealisticConfigParses(t *testing.T) {
	in := `
name: my-app
repo: "https://github.com/org/my-app.git"
branch: main

services:
  db:
    image: "docker.io/library/postgres:16.1"
    volumes:
      - "${DB_PATH}:/var/lib/postgresql/data"
    env:
      POSTGRES_PASSWORD: "${PG_PASSWORD}"
  server:
    image: "ghcr.io/org/my-app/server:1.0"
    dockerfile: backend/Dockerfile
    context: backend
    replicas: 2
    health:
      path: /api/health
      port: 8080
    depends_on:
      - db
    env:
      DB_HOST: my-app-db

targets:
  prod:
    host: "192.0.2.10"
    user: ec2-user
    ssh_key: "~/.ssh/my-key.pem"
    repo_dir: /home/ec2-user/my-app
    dirs:
      - "${DB_PATH}"
    env:
      DB_PATH: /home/ec2-user/pg-data
      PG_PASSWORD: ""
`
	m, err := parseYAML([]byte(in))
	if err != nil {
		t.Fatalf("realistic config failed to parse: %v", err)
	}
	if m["name"] != "my-app" {
		t.Errorf("name = %v", m["name"])
	}
	svcs, ok := m["services"].(map[string]any)
	if !ok {
		t.Fatalf("services type = %T", m["services"])
	}
	server, ok := svcs["server"].(map[string]any)
	if !ok {
		t.Fatalf("server type = %T", svcs["server"])
	}
	if server["replicas"] != 2 {
		t.Errorf("replicas = %v, want int 2", server["replicas"])
	}
	deps, ok := server["depends_on"].([]any)
	if !ok || len(deps) != 1 || deps[0] != "db" {
		t.Errorf("depends_on = %v", deps)
	}
	health, ok := server["health"].(map[string]any)
	if !ok {
		t.Fatalf("health type = %T", server["health"])
	}
	if health["path"] != "/api/health" || health["port"] != 8080 {
		t.Errorf("health = %v", health)
	}
}

// TestYAMLToJSONRoundTrip verifies a YAML config can be re-emitted as JSON and
// parsed back to the same structure. This exercises what `qqd convert` does
// under the hood (YAML -> map -> JSON -> map should be an identity).
func TestYAMLToJSONRoundTrip(t *testing.T) {
	in := `
name: roundtrip
replicas: 3
services:
  app:
    image: app:1
    ports:
      - 80
      - 443
`
	m1, err := parseYAML([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	jsonOut := generateJSONFromMap(m1)
	m2, err := parseJSON([]byte(jsonOut))
	if err != nil {
		t.Fatalf("parseJSON of round-trip: %v", err)
	}
	if !reflect.DeepEqual(m1, m2) {
		t.Fatalf("round-trip mismatch:\n  yaml-parsed: %#v\n  json-parsed: %#v", m1, m2)
	}
}

// TestYAMLToYAMLRoundTrip verifies the YAML emitter produces output that the
// parser accepts and decodes to the same structure. This catches drift between
// emitter and parser when the parser supports something the emitter doesn't (or
// vice versa).
func TestYAMLToYAMLRoundTrip(t *testing.T) {
	in := `
name: rt
services:
  s:
    image: i:1
    env:
      A: "1"
      B: "two"
`
	m1, err := parseYAML([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out := generateYAMLFromMap(m1, 0)
	m2, err := parseYAML([]byte(out))
	if err != nil {
		t.Fatalf("re-parse of generated YAML failed: %v\noutput was:\n%s", err, out)
	}
	if !reflect.DeepEqual(m1, m2) {
		t.Fatalf("YAML round-trip mismatch:\nin:  %#v\nout: %#v\nemitted:\n%s", m1, m2, out)
	}
}
