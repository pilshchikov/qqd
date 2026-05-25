package qqd

import (
	"strings"
	"testing"
)

func TestParseHOCON(t *testing.T) {
	input := `
# comment
name = "proj"
build {
  cpu = 2
  memory = "2g"
}
services {
  backend {
    image = "ghcr.io/acme/backend:1.44"
    ports = ["9999:9999" "443:80"]
    env {
      DB_URL = "db:5432"
    }
  }
}
targets.main.env {
  DATA_DIR = "/tmp/data"
}
`
	m, err := parseHOCON(input)
	if err != nil {
		t.Fatalf("parseHOCON returned error: %v", err)
	}
	if got, _ := asString(m["name"]); got != "proj" {
		t.Fatalf("name mismatch: got %q", got)
	}
	build, ok := asMap(m["build"])
	if !ok {
		t.Fatalf("build missing")
	}
	if got, _ := asInt(build["cpu"]); got != 2 {
		t.Fatalf("build.cpu mismatch: got %d", got)
	}
	services, ok := asMap(m["services"])
	if !ok {
		t.Fatalf("services missing")
	}
	backend, ok := asMap(services["backend"])
	if !ok {
		t.Fatalf("backend missing")
	}
	ports := asStringSlice(backend["ports"])
	if len(ports) != 2 || ports[1] != "443:80" {
		t.Fatalf("ports parse mismatch: %#v", ports)
	}
	targets, ok := asMap(m["targets"])
	if !ok {
		t.Fatalf("targets missing")
	}
	mainTarget, ok := asMap(targets["main"])
	if !ok {
		t.Fatalf("targets.main missing")
	}
	env := asStringMap(mainTarget["env"])
	if env["DATA_DIR"] != "/tmp/data" {
		t.Fatalf("targets.main.env.DATA_DIR mismatch: %#v", env)
	}
}

func TestParseHOCONNumericKeys(t *testing.T) {
	input := `
expose {
  9999 {
    "/api/" = "server:8080"
    "/" = "frontend:80"
  }
  5432 = "db:5432"
}
`
	m, err := parseHOCON(input)
	if err != nil {
		t.Fatalf("parseHOCON returned error: %v", err)
	}
	expose, ok := asMap(m["expose"])
	if !ok {
		t.Fatalf("expose missing")
	}
	// TCP entry
	tcpVal, ok := asString(expose["5432"])
	if !ok || tcpVal != "db:5432" {
		t.Fatalf("expose.5432 mismatch: got %v", expose["5432"])
	}
	// HTTP entry
	httpEntry, ok := asMap(expose["9999"])
	if !ok {
		t.Fatalf("expose.9999 should be a map")
	}
	apiVal, ok := asString(httpEntry["/api/"])
	if !ok || apiVal != "server:8080" {
		t.Fatalf("expose.9999./api/ mismatch: got %v", httpEntry["/api/"])
	}
	rootVal, ok := asString(httpEntry["/"])
	if !ok || rootVal != "frontend:80" {
		t.Fatalf("expose.9999./ mismatch: got %v", httpEntry["/"])
	}
}

// ---------------------------------------------------------------------------
// HOCON parser edge cases
// ---------------------------------------------------------------------------

func TestParseHOCONEmptyInput(t *testing.T) {
	m, err := parseHOCON("")
	if err != nil {
		t.Fatalf("empty input should parse: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("empty input should produce empty map, got %v", m)
	}
}

func TestParseHOCONEmptyObject(t *testing.T) {
	m, err := parseHOCON("a {}")
	if err != nil {
		t.Fatalf("empty object should parse: %v", err)
	}
	sub, ok := asMap(m["a"])
	if !ok {
		t.Fatal("a should be a map")
	}
	if len(sub) != 0 {
		t.Fatal("empty object should have no keys")
	}
}

func TestParseHOCONEmptyArray(t *testing.T) {
	m, err := parseHOCON("a = []")
	if err != nil {
		t.Fatalf("empty array should parse: %v", err)
	}
	arr, ok := m["a"].([]any)
	if !ok {
		t.Fatalf("a should be an array, got %T", m["a"])
	}
	if len(arr) != 0 {
		t.Fatal("empty array should be empty")
	}
}

func TestParseHOCONColonSeparator(t *testing.T) {
	m, err := parseHOCON(`key: "value"`)
	if err != nil {
		t.Fatalf("colon separator should parse: %v", err)
	}
	if got, _ := asString(m["key"]); got != "value" {
		t.Fatalf("colon separator key = %q", got)
	}
}

func TestParseHOCONBooleans(t *testing.T) {
	m, err := parseHOCON("a = true\nb = false")
	if err != nil {
		t.Fatalf("booleans should parse: %v", err)
	}
	if m["a"] != true {
		t.Fatalf("a should be true, got %v", m["a"])
	}
	if m["b"] != false {
		t.Fatalf("b should be false, got %v", m["b"])
	}
}

func TestParseHOCONFloat(t *testing.T) {
	m, err := parseHOCON("pi = 3.14")
	if err != nil {
		t.Fatalf("float should parse: %v", err)
	}
	if m["pi"] != 3.14 {
		t.Fatalf("pi should be 3.14, got %v", m["pi"])
	}
}

func TestParseHOCONEscapeSequences(t *testing.T) {
	m, err := parseHOCON(`msg = "line1\nline2\ttab"`)
	if err != nil {
		t.Fatalf("escape sequences should parse: %v", err)
	}
	got, _ := asString(m["msg"])
	if got != "line1\nline2\ttab" {
		t.Fatalf("escapes not expanded correctly: %q", got)
	}
}

func TestParseHOCONEscapedQuoteInString(t *testing.T) {
	m, err := parseHOCON(`msg = "say \"hello\""`)
	if err != nil {
		t.Fatalf("escaped quote should parse: %v", err)
	}
	got, _ := asString(m["msg"])
	if got != `say "hello"` {
		t.Fatalf("escaped quote = %q", got)
	}
}

func TestParseHOCONEscapedBackslash(t *testing.T) {
	m, err := parseHOCON(`path = "C:\\Users\\test"`)
	if err != nil {
		t.Fatalf("escaped backslash should parse: %v", err)
	}
	got, _ := asString(m["path"])
	if got != `C:\Users\test` {
		t.Fatalf("escaped backslash = %q", got)
	}
}

func TestParseHOCONUnterminatedString(t *testing.T) {
	_, err := parseHOCON(`msg = "unterminated`)
	if err == nil {
		t.Fatal("unterminated string should error")
	}
	if !strings.Contains(err.Error(), "unterminated") {
		t.Fatalf("error should mention unterminated: %v", err)
	}
}

func TestParseHOCONUnsupportedEscape(t *testing.T) {
	_, err := parseHOCON(`msg = "bad\x"`)
	if err == nil {
		t.Fatal("unsupported escape should error")
	}
	if !strings.Contains(err.Error(), "unsupported escape") {
		t.Fatalf("error should mention unsupported escape: %v", err)
	}
}

func TestParseHOCONUnclosedBrace(t *testing.T) {
	_, err := parseHOCON("a { b = 1")
	if err == nil {
		t.Fatal("unclosed brace should error")
	}
}

func TestParseHOCONHashComment(t *testing.T) {
	m, err := parseHOCON("# comment\na = 1 # inline comment\n")
	if err != nil {
		t.Fatalf("hash comment should parse: %v", err)
	}
	if m["a"] != 1 {
		t.Fatalf("a should be 1, got %v", m["a"])
	}
}

func TestParseHOCONSlashSlashComment(t *testing.T) {
	m, err := parseHOCON("// comment\na = 1 // inline comment\n")
	if err != nil {
		t.Fatalf("// comment should parse: %v", err)
	}
	if m["a"] != 1 {
		t.Fatalf("a should be 1, got %v", m["a"])
	}
}

func TestParseHOCONHashInsideString(t *testing.T) {
	m, err := parseHOCON(`a = "has # inside"`)
	if err != nil {
		t.Fatalf("hash inside string should parse: %v", err)
	}
	got, _ := asString(m["a"])
	if got != "has # inside" {
		t.Fatalf("hash inside string = %q", got)
	}
}

func TestParseHOCONDottedKeys(t *testing.T) {
	m, err := parseHOCON("a.b.c = 1")
	if err != nil {
		t.Fatalf("dotted keys should parse: %v", err)
	}
	a, ok := asMap(m["a"])
	if !ok {
		t.Fatal("a should be a map")
	}
	b, ok := asMap(a["b"])
	if !ok {
		t.Fatal("a.b should be a map")
	}
	if b["c"] != 1 {
		t.Fatalf("a.b.c should be 1, got %v", b["c"])
	}
}

func TestParseHOCONDottedKeysMerge(t *testing.T) {
	m, err := parseHOCON("a.b = 1\na.c = 2")
	if err != nil {
		t.Fatalf("dotted keys merge should parse: %v", err)
	}
	a, ok := asMap(m["a"])
	if !ok {
		t.Fatal("a should be a map")
	}
	if a["b"] != 1 || a["c"] != 2 {
		t.Fatalf("a = %v", a)
	}
}

func TestParseHOCONNestedObjectMerge(t *testing.T) {
	input := `
a {
  x = 1
}
a {
  y = 2
}
`
	m, err := parseHOCON(input)
	if err != nil {
		t.Fatalf("nested object merge should parse: %v", err)
	}
	a, ok := asMap(m["a"])
	if !ok {
		t.Fatal("a should be a map")
	}
	if a["x"] != 1 || a["y"] != 2 {
		t.Fatalf("merged a = %v", a)
	}
}

func TestParseHOCONCommasBetweenFields(t *testing.T) {
	m, err := parseHOCON(`a = 1, b = 2`)
	if err != nil {
		t.Fatalf("commas between fields should parse: %v", err)
	}
	if m["a"] != 1 || m["b"] != 2 {
		t.Fatalf("comma separated = %v", m)
	}
}

func TestParseHOCONArrayWithCommas(t *testing.T) {
	m, err := parseHOCON(`a = [1, 2, 3]`)
	if err != nil {
		t.Fatalf("array with commas should parse: %v", err)
	}
	arr, ok := m["a"].([]any)
	if !ok {
		t.Fatalf("a should be array, got %T", m["a"])
	}
	if len(arr) != 3 {
		t.Fatalf("array length = %d", len(arr))
	}
}

func TestParseHOCONNestedArray(t *testing.T) {
	m, err := parseHOCON(`a = ["x", "y"]`)
	if err != nil {
		t.Fatalf("string array should parse: %v", err)
	}
	arr, ok := m["a"].([]any)
	if !ok {
		t.Fatalf("a should be array, got %T", m["a"])
	}
	if len(arr) != 2 || arr[0] != "x" || arr[1] != "y" {
		t.Fatalf("string array = %v", arr)
	}
}

func TestParseHOCONUnquotedStringValue(t *testing.T) {
	m, err := parseHOCON("strategy = local")
	if err != nil {
		t.Fatalf("unquoted string should parse: %v", err)
	}
	if m["strategy"] != "local" {
		t.Fatalf("unquoted string = %v", m["strategy"])
	}
}

func TestParseHOCONObjectInArray(t *testing.T) {
	m, err := parseHOCON(`a = [{ x = 1 }, { y = 2 }]`)
	if err != nil {
		t.Fatalf("object in array should parse: %v", err)
	}
	arr, ok := m["a"].([]any)
	if !ok {
		t.Fatalf("a should be array, got %T", m["a"])
	}
	if len(arr) != 2 {
		t.Fatalf("array should have 2 elements, got %d", len(arr))
	}
	obj, ok := arr[0].(map[string]any)
	if !ok || obj["x"] != 1 {
		t.Fatalf("first element should be {x:1}, got %v", arr[0])
	}
}

func TestParseHOCONUnexpectedToken(t *testing.T) {
	_, err := parseHOCON(`= value`)
	if err == nil {
		t.Fatal("value without key should error")
	}
}

func TestParseHOCONUnterminatedArray(t *testing.T) {
	_, err := parseHOCON(`a = [1, 2`)
	if err == nil {
		t.Fatal("unterminated array should error")
	}
}

func TestParseHOCONQuotedKeysWithDots(t *testing.T) {
	// Quoted keys should NOT be split on dots — they are literal single keys.
	input := `
routes {
  "/.well-known/" = "server:8080"
  "/api/" = "server:8080"
}
`
	m, err := parseHOCON(input)
	if err != nil {
		t.Fatalf("quoted keys with dots should parse: %v", err)
	}
	routes, ok := asMap(m["routes"])
	if !ok {
		t.Fatal("routes should be a map")
	}
	// The key should remain literal, not split into nested maps
	val, ok := asString(routes["/.well-known/"])
	if !ok || val != "server:8080" {
		t.Fatalf("quoted key with dots should be literal, got routes = %v", routes)
	}
	// Verify no spurious nesting occurred
	if _, nested := asMap(routes["/.well-known/"]); nested {
		t.Fatal("quoted key should not create nested structure")
	}
}

func TestParseHOCONQuotedKeyDotVsUnquoted(t *testing.T) {
	// Unquoted keys with dots should still split into nested paths
	input := `
a.b.c = 1
"x.y.z" = 2
`
	m, err := parseHOCON(input)
	if err != nil {
		t.Fatalf("mixed quoted/unquoted keys should parse: %v", err)
	}
	// Unquoted: a -> b -> c = 1
	a, ok := asMap(m["a"])
	if !ok {
		t.Fatal("unquoted dotted key should create nested map")
	}
	b, ok := asMap(a["b"])
	if !ok {
		t.Fatal("a.b should be a map")
	}
	if b["c"] != 1 {
		t.Fatalf("a.b.c = %v, want 1", b["c"])
	}
	// Quoted: single key "x.y.z" = 2
	if m["x.y.z"] != 2 {
		t.Fatalf("quoted key x.y.z = %v, want 2", m["x.y.z"])
	}
}
