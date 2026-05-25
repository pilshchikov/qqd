package qqd

import (
	"reflect"
	"testing"
)

func TestParseJSON(t *testing.T) {
	input := `{
		"name": "myproject",
		"replicas": 3,
		"services": {
			"server": {
				"image": "server:1.0",
				"ports": [80, 443]
			}
		},
		"enabled": true
	}`
	m, err := parseJSON([]byte(input))
	if err != nil {
		t.Fatalf("parseJSON: %v", err)
	}
	if m["name"] != "myproject" {
		t.Fatalf("name = %v", m["name"])
	}
	if m["replicas"] != 3 {
		t.Fatalf("replicas = %v (type %T), want int 3", m["replicas"], m["replicas"])
	}
	if m["enabled"] != true {
		t.Fatalf("enabled = %v", m["enabled"])
	}
	svcs, ok := m["services"].(map[string]any)
	if !ok {
		t.Fatalf("services type = %T", m["services"])
	}
	server, ok := svcs["server"].(map[string]any)
	if !ok {
		t.Fatalf("server type = %T", svcs["server"])
	}
	ports, ok := server["ports"].([]any)
	if !ok {
		t.Fatalf("ports type = %T", server["ports"])
	}
	if len(ports) != 2 || ports[0] != 80 || ports[1] != 443 {
		t.Fatalf("ports = %v", ports)
	}
}

func TestParseJSONFloatPreserved(t *testing.T) {
	input := `{"ratio": 1.5}`
	m, err := parseJSON([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if m["ratio"] != 1.5 {
		t.Fatalf("ratio = %v (type %T)", m["ratio"], m["ratio"])
	}
}

func TestParseYAMLBasic(t *testing.T) {
	input := `
name: myproject
replicas: 3
enabled: true
image: "server:1.0"
`
	m, err := parseYAML([]byte(input))
	if err != nil {
		t.Fatalf("parseYAML: %v", err)
	}
	if m["name"] != "myproject" {
		t.Fatalf("name = %v", m["name"])
	}
	if m["replicas"] != 3 {
		t.Fatalf("replicas = %v (type %T)", m["replicas"], m["replicas"])
	}
	if m["enabled"] != true {
		t.Fatalf("enabled = %v", m["enabled"])
	}
	if m["image"] != "server:1.0" {
		t.Fatalf("image = %v", m["image"])
	}
}

func TestParseYAMLNestedMap(t *testing.T) {
	input := `
services:
  server:
    image: server:1.0
    replicas: 2
  db:
    image: postgres:16
`
	m, err := parseYAML([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	svcs, ok := m["services"].(map[string]any)
	if !ok {
		t.Fatalf("services type = %T", m["services"])
	}
	server := svcs["server"].(map[string]any)
	if server["image"] != "server:1.0" {
		t.Fatalf("server.image = %v", server["image"])
	}
	if server["replicas"] != 2 {
		t.Fatalf("server.replicas = %v", server["replicas"])
	}
	db := svcs["db"].(map[string]any)
	if db["image"] != "postgres:16" {
		t.Fatalf("db.image = %v", db["image"])
	}
}

func TestParseYAMLArray(t *testing.T) {
	input := `
volumes:
  - /data:/data
  - /logs:/logs
`
	m, err := parseYAML([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	vols, ok := m["volumes"].([]any)
	if !ok {
		t.Fatalf("volumes type = %T", m["volumes"])
	}
	want := []any{"/data:/data", "/logs:/logs"}
	if !reflect.DeepEqual(vols, want) {
		t.Fatalf("volumes = %v, want %v", vols, want)
	}
}

func TestParseYAMLFlowSequence(t *testing.T) {
	input := `
command: ["sh", "-c", "echo hello"]
`
	m, err := parseYAML([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	cmd, ok := m["command"].([]any)
	if !ok {
		t.Fatalf("command type = %T", m["command"])
	}
	want := []any{"sh", "-c", "echo hello"}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("command = %v, want %v", cmd, want)
	}
}

func TestParseYAMLComments(t *testing.T) {
	input := `
# top comment
name: proj # inline comment
# another comment
replicas: 2
`
	m, err := parseYAML([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if m["name"] != "proj" {
		t.Fatalf("name = %v", m["name"])
	}
	if m["replicas"] != 2 {
		t.Fatalf("replicas = %v", m["replicas"])
	}
}

func TestParseYAMLBooleans(t *testing.T) {
	input := `
a: true
b: false
c: yes
d: no
`
	m, err := parseYAML([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if m["a"] != true {
		t.Fatalf("a = %v", m["a"])
	}
	if m["b"] != false {
		t.Fatalf("b = %v", m["b"])
	}
	if m["c"] != true {
		t.Fatalf("c = %v", m["c"])
	}
	if m["d"] != false {
		t.Fatalf("d = %v", m["d"])
	}
}

func TestParseYAMLQuotedKey(t *testing.T) {
	input := `
"special.key": value
`
	m, err := parseYAML([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if m["special.key"] != "value" {
		t.Fatalf("special.key = %v", m["special.key"])
	}
}

func TestStripYAMLInlineComment(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`value # comment`, `value`},
		{`"value # not comment"`, `"value # not comment"`},
		{`'also # not'`, `'also # not'`},
		{`no comment`, `no comment`},
	}
	for _, tt := range tests {
		got := stripYAMLInlineComment(tt.in)
		if got != tt.want {
			t.Errorf("stripYAMLInlineComment(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
