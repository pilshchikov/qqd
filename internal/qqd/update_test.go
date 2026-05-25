package qqd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBumpVersion(t *testing.T) {
	cases := map[string]string{
		"1.44":   "1.45",
		"0.1-b7": "0.1-b8",
		"99":     "100",
	}
	for in, want := range cases {
		got, err := bumpVersion(in)
		if err != nil {
			t.Fatalf("bumpVersion(%q) failed: %v", in, err)
		}
		if got != want {
			t.Fatalf("bumpVersion(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestSplitImageTag(t *testing.T) {
	repo, tag, ok := splitImageTag("ghcr.io/acme/report/server:1.44")
	if !ok {
		t.Fatalf("expected tag")
	}
	if repo != "ghcr.io/acme/report/server" || tag != "1.44" {
		t.Fatalf("split mismatch: %q %q", repo, tag)
	}
	repo, _, ok = splitImageTag("ghcr.io:5000/acme/server")
	if ok || repo != "ghcr.io:5000/acme/server" {
		t.Fatalf("expected no tag for registry-only colon")
	}
}

func TestUpdateConfigVersionsWithBracesInStrings(t *testing.T) {
	td := t.TempDir()
	path := filepath.Join(td, "cfg.conf")
	in := `
services {
  backend {
    image = "ghcr.io/acme/backend:1.44"
    env {
      JSON_CONFIG = "{\"key\": \"value\"}"
    }
  }
}
`
	if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := updateConfigVersions(path, map[string]string{
		"backend": "1.45",
	}); err != nil {
		t.Fatalf("updateConfigVersions with braces in strings failed: %v", err)
	}
	outRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(outRaw)
	if !strings.Contains(out, `image = "ghcr.io/acme/backend:1.45"`) {
		t.Fatalf("backend image not updated correctly:\n%s", out)
	}
	if !strings.Contains(out, `JSON_CONFIG = "{\"key\": \"value\"}"`) {
		t.Fatalf("JSON env value was corrupted:\n%s", out)
	}
}

func TestUpdateConfigVersions(t *testing.T) {
	td := t.TempDir()
	path := filepath.Join(td, "cfg.conf")
	in := `
services {
  backend {
    image = "ghcr.io/acme/backend:1.44" # keep comment
  }
  frontend {
    image = "ghcr.io/acme/frontend:1.30"
  }
}
`
	if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := updateConfigVersions(path, map[string]string{
		"backend":  "1.45",
		"frontend": "2.0",
	}); err != nil {
		t.Fatalf("updateConfigVersions failed: %v", err)
	}
	outRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(outRaw)
	if !strings.Contains(out, `image = "ghcr.io/acme/backend:1.45" # keep comment`) {
		t.Fatalf("backend image not updated correctly:\n%s", out)
	}
	if !strings.Contains(out, `image = "ghcr.io/acme/frontend:2.0"`) {
		t.Fatalf("frontend image not updated:\n%s", out)
	}
}

func TestUpdateConfigVersionsAlternativeFormatting(t *testing.T) {
	tests := []struct {
		name string
		in   string
		svc  string
		ver  string
		want string
	}{
		{
			name: "extra spaces around equals",
			in:   "services {\n  backend {\n    image  =  \"ghcr.io/acme/backend:1.0\"\n  }\n}\n",
			svc:  "backend", ver: "2.0",
			want: `image  =  "ghcr.io/acme/backend:2.0"`,
		},
		{
			name: "tab indentation",
			in:   "services {\n\tbackend {\n\t\timage = \"ghcr.io/acme/backend:1.0\"\n\t}\n}\n",
			svc:  "backend", ver: "2.0",
			want: `image = "ghcr.io/acme/backend:2.0"`,
		},
		{
			name: "service brace on same line with content",
			in:   "services {\n  backend { image = \"ghcr.io/acme/backend:1.0\" }\n}\n",
			svc:  "backend", ver: "2.0",
			want: `image = "ghcr.io/acme/backend:2.0"`,
		},
		{
			name: "nested service with many fields",
			in: `services {
  backend {
    image = "ghcr.io/acme/backend:1.0"
    env { KEY = "val" }
    volumes = ["/data:/data"]
  }
}
`,
			svc: "backend", ver: "3.0",
			want: `image = "ghcr.io/acme/backend:3.0"`,
		},
		{
			name: "services brace on next line",
			in:   "services\n{\n  backend {\n    image = \"ghcr.io/acme/backend:1.0\"\n  }\n}\n",
			svc:  "backend", ver: "2.1",
			want: `image = "ghcr.io/acme/backend:2.1"`,
		},
		{
			name: "service brace on next line",
			in:   "services {\n  backend\n  {\n    image = \"ghcr.io/acme/backend:1.0\"\n  }\n}\n",
			svc:  "backend", ver: "2.2",
			want: `image = "ghcr.io/acme/backend:2.2"`,
		},
		{
			name: "unquoted image value",
			in:   "services {\n  backend {\n    image = ghcr.io/acme/backend:1.0\n  }\n}\n",
			svc:  "backend", ver: "2.3",
			want: `image = ghcr.io/acme/backend:2.3`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := t.TempDir()
			path := filepath.Join(td, "cfg.conf")
			if err := os.WriteFile(path, []byte(tt.in), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := updateConfigVersions(path, map[string]string{tt.svc: tt.ver}); err != nil {
				t.Fatalf("updateConfigVersions failed: %v", err)
			}
			outRaw, _ := os.ReadFile(path)
			if !strings.Contains(string(outRaw), tt.want) {
				t.Fatalf("expected %q in output:\n%s", tt.want, string(outRaw))
			}
		})
	}
}

func TestUpdateConfigVersionsPreservesFileMode(t *testing.T) {
	td := t.TempDir()
	path := filepath.Join(td, "cfg.conf")
	in := "services {\n  backend {\n    image = \"ghcr.io/acme/backend:1.0\"\n  }\n}\n"
	if err := os.WriteFile(path, []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := updateConfigVersions(path, map[string]string{"backend": "2.0"}); err != nil {
		t.Fatalf("updateConfigVersions failed: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode should be preserved as 0600, got %o", st.Mode().Perm())
	}
}
