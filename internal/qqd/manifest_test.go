package qqd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestManifestJSONIsValidAndCoversCommands asserts the manifest serializes to
// valid JSON, parses back into the same shape, and includes every command
// declared in commandSpecs() (the source of truth) plus the `manifest`
// command itself.
func TestManifestJSONIsValidAndCoversCommands(t *testing.T) {
	m := buildManifest()
	raw, err := renderManifestJSON(m)
	if err != nil {
		t.Fatalf("renderManifestJSON: %v", err)
	}
	var back Manifest
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("manifest JSON round-trip failed: %v\n%s", err, raw)
	}

	gotNames := map[string]bool{}
	for _, c := range back.Commands {
		gotNames[c.Name] = true
	}
	for _, spec := range commandSpecs() {
		if !gotNames[spec.Name] {
			t.Errorf("command %q from commandSpecs() missing in manifest", spec.Name)
		}
	}
	if !gotNames["manifest"] {
		t.Error("`manifest` itself should be listed in the manifest")
	}
}

// TestManifestCommandUsageMatchesCommandSpecs guards against drift: every
// command in the manifest must have the same usage string as commandSpecs().
// If you change one, the other must follow — they share one source.
func TestManifestCommandUsageMatchesCommandSpecs(t *testing.T) {
	m := buildManifest()
	specByName := map[string]commandSpec{}
	for _, s := range commandSpecs() {
		specByName[s.Name] = s
	}
	for _, c := range m.Commands {
		s, ok := specByName[c.Name]
		if !ok {
			// `manifest` is appended by buildCommandManifest, not in commandSpecs.
			continue
		}
		if c.Usage != s.Usage {
			t.Errorf("command %q: manifest usage %q != commandSpecs usage %q", c.Name, c.Usage, s.Usage)
		}
		if c.Summary != s.Summary {
			t.Errorf("command %q: manifest summary differs from commandSpecs", c.Name)
		}
	}
}

// TestConfigSchemaReflectsTaggedFields asserts the reflection-based schema
// builder picks up every `qqd:`-tagged field on the public config structs
// (and only those). Adding a tag to a struct field in types.go must surface
// in the manifest with no edits to manifest.go.
func TestConfigSchemaReflectsTaggedFields(t *testing.T) {
	cases := []struct {
		section string
		typ     reflect.Type
	}{
		{"project", reflect.TypeOf(ProjectConfig{})},
		{"service", reflect.TypeOf(ServiceConfig{})},
		{"target", reflect.TypeOf(TargetConfig{})},
		{"build", reflect.TypeOf(BuildConfig{})},
		{"health", reflect.TypeOf(HealthConfig{})},
		{"resources", reflect.TypeOf(ResourceConfig{})},
		{"hooks", reflect.TypeOf(HooksConfig{})},
		{"tls", reflect.TypeOf(TLSConfig{})},
		{"service_override", reflect.TypeOf(ServiceOverride{})},
	}
	schema := buildConfigSchema()
	byName := map[string]ConfigSection{}
	for _, s := range schema {
		byName[s.Name] = s
	}

	for _, tc := range cases {
		t.Run(tc.section, func(t *testing.T) {
			sec, ok := byName[tc.section]
			if !ok {
				t.Fatalf("manifest is missing section %q", tc.section)
			}
			expected := taggedFieldKeys(tc.typ)
			got := map[string]bool{}
			for _, f := range sec.Fields {
				got[f.Name] = true
			}
			for k := range expected {
				if !got[k] {
					t.Errorf("section %q is missing field %q (present in %s as qqd:key)", tc.section, k, tc.typ.Name())
				}
			}
			for k := range got {
				if !expected[k] {
					t.Errorf("section %q has field %q not present as a qqd:key on %s", tc.section, k, tc.typ.Name())
				}
			}
		})
	}
}

// TestCommonFlagsRegistryMatchesParser pins the parseCommonOpts switch and
// the commonFlagRegistry together. If you add a new common flag, you must
// add a matching registry entry and vice versa — otherwise agents that read
// the manifest see a different surface than they can actually invoke.
func TestCommonFlagsRegistryMatchesParser(t *testing.T) {
	registered := map[string]bool{}
	for _, f := range commonFlagRegistry() {
		registered[f.Name] = true
		if f.Short != "" {
			registered[f.Short] = true
		}
	}
	// Spell out the flags parseCommonOpts accepts. If you change the parser,
	// change this list too — and ensure commonFlagRegistry() lists each one.
	parserFlags := []string{
		"-c", "--config",
		"-t", "--target",
		"--rebuild",
		"--approve",
		"--dry-run",
		"--config-only",
		"--no-build",
		"--force-unlock",
	}
	for _, f := range parserFlags {
		if !registered[f] {
			t.Errorf("parser flag %q has no entry in commonFlagRegistry()", f)
		}
	}
	// And the reverse: every registry entry should map to a parser flag.
	expected := map[string]bool{}
	for _, f := range parserFlags {
		expected[f] = true
	}
	for _, f := range commonFlagRegistry() {
		if !expected[f.Name] {
			t.Errorf("commonFlagRegistry() entry %q is not accepted by parseCommonOpts", f.Name)
		}
		if f.Short != "" && !expected[f.Short] {
			t.Errorf("commonFlagRegistry() entry %q claims short %q but parseCommonOpts does not accept it", f.Name, f.Short)
		}
	}
}

// TestManifestCommandFlagsExist sanity-checks that every command listed as
// using common flags is one we can dispatch through Execute (i.e. exists in
// the actual case statement).
func TestManifestCommandFlagsExist(t *testing.T) {
	m := buildManifest()
	dispatch := executeKnownCommands()
	for _, c := range m.Commands {
		if !dispatch[c.Name] {
			t.Errorf("manifest lists command %q but cli.go Execute does not dispatch it", c.Name)
		}
	}
}

// TestManifestMarkdownIsNonEmpty smoke-checks the markdown renderer.
func TestManifestMarkdownIsNonEmpty(t *testing.T) {
	out := renderManifestMarkdown(buildManifest())
	for _, must := range []string{
		"# qqd",
		"## Commands",
		"## Config schema",
		"## Pitfalls",
		"qqd deploy",  // a command usage line should appear
		"image",       // a config field
		"known_hosts", // a pitfall topic
	} {
		if !strings.Contains(out, must) {
			t.Errorf("markdown manifest missing expected substring %q", must)
		}
	}
}

// TestManifestCommandWritesJSON exercises the full `qqd manifest` path via
// Execute and checks that JSON is what lands on stdout / in a file.
func TestManifestCommandWritesJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := Execute([]string{"manifest"}, &buf); err != nil {
		t.Fatalf("qqd manifest failed: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("qqd manifest stdout is not valid JSON: %v\nfirst 200 bytes:\n%s", err, truncate(buf.String(), 200))
	}
	if m.Tool.Name != "qqd" {
		t.Fatalf("tool name expected 'qqd', got %q", m.Tool.Name)
	}

	td := t.TempDir()
	outPath := filepath.Join(td, "m.json")
	var stdoutBuf bytes.Buffer
	if err := Execute([]string{"manifest", "-o", outPath}, &stdoutBuf); err != nil {
		t.Fatalf("qqd manifest -o: %v", err)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("output file unreadable: %v", err)
	}
	var m2 Manifest
	if err := json.Unmarshal(raw, &m2); err != nil {
		t.Fatalf("manifest file is not valid JSON: %v", err)
	}
}

// TestManifestCommandWritesMarkdown checks the --format md path.
func TestManifestCommandWritesMarkdown(t *testing.T) {
	var buf bytes.Buffer
	if err := Execute([]string{"manifest", "--format", "md"}, &buf); err != nil {
		t.Fatalf("qqd manifest --format md failed: %v", err)
	}
	if !strings.HasPrefix(buf.String(), "# qqd") {
		t.Fatalf("markdown manifest should start with '# qqd', got %q", truncate(buf.String(), 80))
	}
	if !strings.Contains(buf.String(), "info\\|warn\\|danger") {
		t.Fatal("markdown table cells should escape pipe characters")
	}
}

func TestManifestCommandDocumentsItsOwnFlags(t *testing.T) {
	m := buildManifest()
	var manifest Command
	for _, c := range m.Commands {
		if c.Name == "manifest" {
			manifest = c
			break
		}
	}
	if manifest.Name == "" {
		t.Fatal("manifest command missing from command list")
	}
	flags := map[string]bool{}
	for _, f := range manifest.ExtraFlags {
		flags[f.Name] = true
		if f.Short != "" {
			flags[f.Short] = true
		}
	}
	for _, want := range []string{"--format", "-o", "--output"} {
		if !flags[want] {
			t.Fatalf("manifest command should document %s; got %+v", want, manifest.ExtraFlags)
		}
	}
}

// taggedFieldKeys returns every qqd:key= name on a struct type.
func taggedFieldKeys(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("qqd")
		if tag == "" || tag == "-" {
			continue
		}
		m := parseQQDTag(tag)
		if m.key != "" {
			out[m.key] = true
		}
	}
	return out
}

// executeKnownCommands lists the verbs cli.go's Execute switch accepts.
// Updated alongside the dispatcher so the manifest can never claim a verb
// that the binary doesn't actually support.
func executeKnownCommands() map[string]bool {
	return map[string]bool{
		"init": true, "plan": true, "deploy": true, "build": true,
		"status": true, "logs": true, "rollback": true, "history": true,
		"stop": true, "start": true, "destroy": true, "clean": true,
		"update": true, "doctor": true, "validate": true, "man": true,
		"import": true, "migrate": true, "convert": true, "docs": true,
		"manifest": true,
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// keep io import even if unused in this file (encoding/json + bytes already are).
var _ = io.Discard
