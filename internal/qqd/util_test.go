package qqd

import (
	"context"
	"errors"
	"io"
	"testing"
)

// ---------------------------------------------------------------------------
// shellQuote
// ---------------------------------------------------------------------------

func TestShellQuoteSimple(t *testing.T) {
	if got := shellQuote("hello"); got != "'hello'" {
		t.Fatalf("shellQuote(hello) = %s", got)
	}
}

func TestShellQuoteEmpty(t *testing.T) {
	if got := shellQuote(""); got != "''" {
		t.Fatalf("shellQuote(\"\") = %s", got)
	}
}

func TestShellQuoteWithSingleQuotes(t *testing.T) {
	got := shellQuote("it's")
	// Expected: 'it'"'"'s'
	if got != `'it'"'"'s'` {
		t.Fatalf("shellQuote(it's) = %s", got)
	}
}

func TestShellQuoteSpecialChars(t *testing.T) {
	got := shellQuote("a b; rm -rf /")
	if got != "'a b; rm -rf /'" {
		t.Fatalf("shellQuote special chars = %s", got)
	}
}

func TestShellQuoteBackticks(t *testing.T) {
	got := shellQuote("`whoami`")
	if got != "'`whoami`'" {
		t.Fatalf("shellQuote backticks = %s", got)
	}
}

func TestShellQuoteDollarSign(t *testing.T) {
	got := shellQuote("$HOME")
	if got != "'$HOME'" {
		t.Fatalf("shellQuote dollar = %s", got)
	}
}

// ---------------------------------------------------------------------------
// deepMergeMaps
// ---------------------------------------------------------------------------

func TestDeepMergeMapsSimple(t *testing.T) {
	dst := map[string]any{"a": "1", "b": "2"}
	src := map[string]any{"b": "3", "c": "4"}
	result := deepMergeMaps(dst, src)
	if result["a"] != "1" {
		t.Fatal("a should be preserved")
	}
	if result["b"] != "3" {
		t.Fatal("b should be overwritten by src")
	}
	if result["c"] != "4" {
		t.Fatal("c should be added from src")
	}
}

func TestDeepMergeMapsNested(t *testing.T) {
	dst := map[string]any{
		"top": map[string]any{"a": "1", "b": "2"},
	}
	src := map[string]any{
		"top": map[string]any{"b": "3", "c": "4"},
	}
	result := deepMergeMaps(dst, src)
	top, ok := result["top"].(map[string]any)
	if !ok {
		t.Fatal("top should be a map")
	}
	if top["a"] != "1" {
		t.Fatal("top.a should be preserved")
	}
	if top["b"] != "3" {
		t.Fatal("top.b should be overwritten")
	}
	if top["c"] != "4" {
		t.Fatal("top.c should be added")
	}
}

func TestDeepMergeMapsNilDst(t *testing.T) {
	src := map[string]any{"a": "1"}
	result := deepMergeMaps(nil, src)
	if result["a"] != "1" {
		t.Fatal("nil dst should create new map with src values")
	}
}

func TestDeepMergeMapsScalarOverridesMap(t *testing.T) {
	dst := map[string]any{"a": map[string]any{"x": "1"}}
	src := map[string]any{"a": "scalar"}
	result := deepMergeMaps(dst, src)
	if result["a"] != "scalar" {
		t.Fatal("scalar from src should overwrite map in dst")
	}
}

func TestDeepMergeMapsMapOverridesScalar(t *testing.T) {
	dst := map[string]any{"a": "scalar"}
	src := map[string]any{"a": map[string]any{"x": "1"}}
	result := deepMergeMaps(dst, src)
	sub, ok := result["a"].(map[string]any)
	if !ok || sub["x"] != "1" {
		t.Fatal("map from src should overwrite scalar in dst")
	}
}

// ---------------------------------------------------------------------------
// asString
// ---------------------------------------------------------------------------

func TestAsStringTypes(t *testing.T) {
	tests := []struct {
		input any
		want  string
		ok    bool
	}{
		{"hello", "hello", true},
		{42, "42", true},
		{int64(100), "100", true},
		{3.14, "3.14", true},
		{true, "true", true},
		{false, "false", true},
		{nil, "", false},
		{[]string{"a"}, "", false},
	}
	for _, tt := range tests {
		got, ok := asString(tt.input)
		if ok != tt.ok || got != tt.want {
			t.Errorf("asString(%v) = (%q, %v), want (%q, %v)", tt.input, got, ok, tt.want, tt.ok)
		}
	}
}

// ---------------------------------------------------------------------------
// asInt
// ---------------------------------------------------------------------------

func TestAsIntTypes(t *testing.T) {
	tests := []struct {
		input any
		want  int
		ok    bool
	}{
		{42, 42, true},
		{int64(100), 100, true},
		{3.7, 3, true},
		{"42", 42, true},
		{"abc", 0, false},
		{nil, 0, false},
		{true, 0, false},
	}
	for _, tt := range tests {
		got, ok := asInt(tt.input)
		if ok != tt.ok || got != tt.want {
			t.Errorf("asInt(%v) = (%d, %v), want (%d, %v)", tt.input, got, ok, tt.want, tt.ok)
		}
	}
}

// ---------------------------------------------------------------------------
// asMap
// ---------------------------------------------------------------------------

func TestAsMapTypes(t *testing.T) {
	// map[string]any
	m1 := map[string]any{"a": 1}
	got, ok := asMap(m1)
	if !ok || got["a"] != 1 {
		t.Fatal("should convert map[string]any")
	}
	// map[string]string
	m2 := map[string]string{"b": "2"}
	got, ok = asMap(m2)
	if !ok || got["b"] != "2" {
		t.Fatal("should convert map[string]string")
	}
	// other types
	_, ok = asMap("not a map")
	if ok {
		t.Fatal("string should not convert to map")
	}
	_, ok = asMap(nil)
	if ok {
		t.Fatal("nil should not convert to map")
	}
}

// ---------------------------------------------------------------------------
// asStringSlice
// ---------------------------------------------------------------------------

func TestAsStringSliceTypes(t *testing.T) {
	// nil
	if got := asStringSlice(nil); got != nil {
		t.Fatal("nil should return nil")
	}
	// []string
	got := asStringSlice([]string{"a", "b"})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatal("should return copy of []string")
	}
	// []any
	got = asStringSlice([]any{"x", 42, true})
	if len(got) != 3 || got[0] != "x" || got[1] != "42" || got[2] != "true" {
		t.Fatalf("should convert []any: %v", got)
	}
	// single string
	got = asStringSlice("single")
	if len(got) != 1 || got[0] != "single" {
		t.Fatal("single string should return 1-element slice")
	}
	// unsupported type
	if got := asStringSlice(42); got != nil {
		t.Fatal("int should return nil")
	}
}

func TestAsStringSliceIsCopy(t *testing.T) {
	original := []string{"a", "b"}
	copy := asStringSlice(original)
	copy[0] = "modified"
	if original[0] != "a" {
		t.Fatal("asStringSlice should return a copy, not modify original")
	}
}

// ---------------------------------------------------------------------------
// asStringMap
// ---------------------------------------------------------------------------

func TestAsStringMapTypes(t *testing.T) {
	got := asStringMap(map[string]any{"a": "1", "b": 42, "c": true})
	if got["a"] != "1" || got["b"] != "42" || got["c"] != "true" {
		t.Fatalf("asStringMap = %v", got)
	}
	got = asStringMap("not a map")
	if len(got) != 0 {
		t.Fatal("should return empty map for non-map input")
	}
}

// ---------------------------------------------------------------------------
// expandVars
// ---------------------------------------------------------------------------

func TestExpandVarsFromMap(t *testing.T) {
	vars := map[string]string{"HOST": "192.0.2.10", "PORT": "8080"}
	got := expandVars("http://${HOST}:${PORT}", vars)
	if got != "http://192.0.2.10:8080" {
		t.Fatalf("expandVars = %s", got)
	}
}

func TestExpandVarsMissingKeepsPlaceholder(t *testing.T) {
	got := expandVars("${NONEXISTENT_QQD_VAR_12345}", nil)
	if got != "${NONEXISTENT_QQD_VAR_12345}" {
		t.Fatalf("missing var should keep placeholder, got %s", got)
	}
}

func TestExpandVarsNoPlaceholders(t *testing.T) {
	got := expandVars("no vars here", nil)
	if got != "no vars here" {
		t.Fatalf("no placeholders should return unchanged, got %s", got)
	}
}

func TestExpandVarsEmptyString(t *testing.T) {
	got := expandVars("", nil)
	if got != "" {
		t.Fatalf("empty string should return empty, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// sortedKeys
// ---------------------------------------------------------------------------

func TestSortedKeysBasic(t *testing.T) {
	m := map[string]int{"c": 3, "a": 1, "b": 2}
	got := sortedKeys(m)
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("sortedKeys = %v", got)
	}
}

func TestSortedKeysEmpty(t *testing.T) {
	got := sortedKeys(map[string]int{})
	if len(got) != 0 {
		t.Fatal("empty map should return empty slice")
	}
}

// ---------------------------------------------------------------------------
// mergeBuild
// ---------------------------------------------------------------------------

func TestMergeBuildOverrides(t *testing.T) {
	base := BuildConfig{Strategy: "local", CPU: 2, Memory: "2g", Host: "h1"}
	override := BuildConfig{CPU: 4, Host: "h2"}
	result := mergeBuild(base, override)
	if result.Strategy != "local" {
		t.Fatal("strategy should be preserved from base")
	}
	if result.CPU != 4 {
		t.Fatal("cpu should be overridden")
	}
	if result.Memory != "2g" {
		t.Fatal("memory should be preserved from base")
	}
	if result.Host != "h2" {
		t.Fatal("host should be overridden")
	}
}

func TestMergeBuildEmptyOverride(t *testing.T) {
	base := BuildConfig{Strategy: "local", CPU: 2}
	result := mergeBuild(base, BuildConfig{})
	if result.Strategy != "local" || result.CPU != 2 {
		t.Fatal("empty override should not change anything")
	}
}

// ---------------------------------------------------------------------------
// injectGHToken
// ---------------------------------------------------------------------------

func TestInjectGHTokenHTTPS(t *testing.T) {
	got := injectGHToken("https://github.com/acme/repo.git", "example-token")
	if got != "https://example-token@github.com/acme/repo.git" {
		t.Fatalf("injectGHToken HTTPS = %s", got)
	}
}

func TestInjectGHTokenSSHUnchanged(t *testing.T) {
	url := "git@github.com:acme/repo.git"
	got := injectGHToken(url, "example-token")
	if got != url {
		t.Fatalf("SSH URL should be unchanged, got %s", got)
	}
}

func TestInjectGHTokenEmptyToken(t *testing.T) {
	url := "https://github.com/acme/repo.git"
	got := injectGHToken(url, "")
	if got != url {
		t.Fatalf("empty token should leave URL unchanged, got %s", got)
	}
}

func TestInjectGHTokenNonGitHub(t *testing.T) {
	url := "https://gitlab.com/acme/repo.git"
	got := injectGHToken(url, "example-token")
	if got != url {
		t.Fatalf("non-GitHub URL should be unchanged, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// resolveGHToken
// ---------------------------------------------------------------------------

type tokenMockExec struct {
	output string
	err    error
}

func (m tokenMockExec) Run(_ context.Context, _ string) (string, error) { return m.output, m.err }
func (m tokenMockExec) RunStream(_ context.Context, _ string, _ io.Writer) error {
	return nil
}
func (m tokenMockExec) CopyFrom(_ context.Context, _, _ string) error { return nil }
func (m tokenMockExec) CopyTo(_ context.Context, _, _ string) error   { return nil }
func (m tokenMockExec) Close() error                                  { return nil }
func (m tokenMockExec) ID() string                                    { return "mock" }

func TestResolveGHTokenEmpty(t *testing.T) {
	got, err := resolveGHToken(context.Background(), tokenMockExec{}, "")
	if err != nil || got != "" {
		t.Fatalf("empty raw should return empty, got %q, err %v", got, err)
	}
}

func TestResolveGHTokenLiteral(t *testing.T) {
	got, err := resolveGHToken(context.Background(), tokenMockExec{}, "example-token")
	if err != nil || got != "example-token" {
		t.Fatalf("literal token should pass through, got %q", got)
	}
}

func TestResolveGHTokenGH(t *testing.T) {
	mock := tokenMockExec{output: "  example-token-from-cli  \n"}
	got, err := resolveGHToken(context.Background(), mock, "gh")
	if err != nil || got != "example-token-from-cli" {
		t.Fatalf("gh should resolve via `gh auth token`, got %q", got)
	}
}

func TestResolveGHTokenGHError(t *testing.T) {
	mock := tokenMockExec{err: errors.New("gh not installed")}
	_, err := resolveGHToken(context.Background(), mock, "gh")
	if err == nil {
		t.Fatal("should return error when gh auth token fails")
	}
}

// ---------------------------------------------------------------------------
// rewriteDepsForSlots
// ---------------------------------------------------------------------------

func TestRewriteDepsForSlots(t *testing.T) {
	svc := ServiceConfig{DependsOn: []string{"server", "db"}}
	slots := map[string]string{"server": "a1b2c3d4"}
	result := rewriteDepsForSlots(svc, slots)
	if result.DependsOn[0] != "server-a1b2c3d4" {
		t.Fatalf("server dep should be rewritten, got %s", result.DependsOn[0])
	}
	if result.DependsOn[1] != "db" {
		t.Fatalf("db dep should be unchanged, got %s", result.DependsOn[1])
	}
}

func TestRewriteDepsForSlotsNoDeps(t *testing.T) {
	svc := ServiceConfig{}
	result := rewriteDepsForSlots(svc, map[string]string{"server": "a1b2c3d4"})
	if len(result.DependsOn) != 0 {
		t.Fatal("no deps should remain no deps")
	}
}

func TestRewriteDepsForSlotsNoSlots(t *testing.T) {
	svc := ServiceConfig{DependsOn: []string{"server"}}
	result := rewriteDepsForSlots(svc, nil)
	if result.DependsOn[0] != "server" {
		t.Fatal("no slots should leave deps unchanged")
	}
}

func TestRewriteDepsForSlotsNoMatchingSlot(t *testing.T) {
	svc := ServiceConfig{DependsOn: []string{"db"}}
	result := rewriteDepsForSlots(svc, map[string]string{"server": "a1b2c3d4"})
	if result.DependsOn[0] != "db" {
		t.Fatal("non-matching dep should be unchanged")
	}
}

func TestRewriteDepsDoesNotMutateOriginal(t *testing.T) {
	svc := ServiceConfig{DependsOn: []string{"server"}}
	slots := map[string]string{"server": "a1b2c3d4"}
	rewriteDepsForSlots(svc, slots)
	if svc.DependsOn[0] != "server" {
		t.Fatal("original service should not be mutated")
	}
}

// ---------------------------------------------------------------------------
// ServiceConfig.Clone
// ---------------------------------------------------------------------------

func TestServiceConfigCloneIsDeep(t *testing.T) {
	orig := ServiceConfig{
		Image:     "img:1.0",
		Command:   []string{"cmd1", "cmd2"},
		DependsOn: []string{"dep1"},
		Volumes:   []string{"vol1"},
		Env:       map[string]string{"KEY": "VALUE"},
	}
	cloned := orig.Clone()
	// Mutate the clone
	cloned.Command[0] = "modified"
	cloned.DependsOn[0] = "modified"
	cloned.Volumes[0] = "modified"
	cloned.Env["KEY"] = "modified"
	// Original should be unchanged
	if orig.Command[0] != "cmd1" {
		t.Fatal("clone should not share Command slice")
	}
	if orig.DependsOn[0] != "dep1" {
		t.Fatal("clone should not share DependsOn slice")
	}
	if orig.Volumes[0] != "vol1" {
		t.Fatal("clone should not share Volumes slice")
	}
	if orig.Env["KEY"] != "VALUE" {
		t.Fatal("clone should not share Env map")
	}
}

func TestServiceConfigCloneNilFields(t *testing.T) {
	orig := ServiceConfig{Image: "img:1.0"}
	cloned := orig.Clone()
	if cloned.Image != "img:1.0" {
		t.Fatal("basic fields should be copied")
	}
	if cloned.Command == nil {
		// append(nil, ...) returns nil, which is fine
	}
}

// ---------------------------------------------------------------------------
// resolveLocalPath
// ---------------------------------------------------------------------------

func TestResolveLocalPathEmpty(t *testing.T) {
	got, err := resolveLocalPath("/wd", "")
	if err != nil || got != "" {
		t.Fatalf("empty path should return empty, got %q", got)
	}
}

func TestResolveLocalPathAbsolute(t *testing.T) {
	got, err := resolveLocalPath("/wd", "/abs/path")
	if err != nil || got != "/abs/path" {
		t.Fatalf("absolute path should be returned as-is, got %q", got)
	}
}

func TestResolveLocalPathRelative(t *testing.T) {
	got, err := resolveLocalPath("/wd", "rel/path")
	if err != nil || got != "/wd/rel/path" {
		t.Fatalf("relative path should be joined with wd, got %q", got)
	}
}
