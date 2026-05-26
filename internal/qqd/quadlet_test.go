package qqd

import (
	"strings"
	"testing"
)

func TestRenderContainerBuiltService(t *testing.T) {
	svc := ServiceConfig{
		Image:      "ghcr.io/acme/report/server:1.44",
		Dockerfile: "backend/server/Dockerfile",
		DependsOn:  []string{"db"},
		Volumes:    []string{"/data:/app/data"},
		Env: map[string]string{
			"DB_URL": "db:5432",
		},
		Command: []string{"python server.py"},
	}
	got := renderContainer("report", "server", svc)
	wantContains := []string{
		"[Unit]",
		"After=report-db.service",
		"[Container]",
		"ContainerName=report-server",
		"Image=ghcr.io/acme/report/server:1.44",
		"Network=report.network",
		"Environment=DB_URL=db:5432",
		"Volume=/data:/app/data:z",
		`Exec="python server.py"`,
	}
	for _, needle := range wantContains {
		if !strings.Contains(got, needle) {
			t.Fatalf("rendered container missing %q:\n%s", needle, got)
		}
	}
	// No PublishPort (all traffic through Traefik)
	if strings.Contains(got, "PublishPort") {
		t.Fatalf("should not contain PublishPort:\n%s", got)
	}
}

func TestRenderContainerThirdParty(t *testing.T) {
	svc := ServiceConfig{
		Image: "postgres:16.1",
		Env: map[string]string{
			"POSTGRES_PASSWORD": "postgres",
		},
	}
	got := renderContainer("report", "db", svc)
	if !strings.Contains(got, "Image=postgres:16.1") {
		t.Fatalf("should use Image directly:\n%s", got)
	}
	// No PublishPort
	if strings.Contains(got, "PublishPort") {
		t.Fatalf("should not contain PublishPort:\n%s", got)
	}
}

func TestEnsureVolumeFlags(t *testing.T) {
	tests := []struct {
		name string
		in   string
		addU bool
		want string
	}{
		{name: "simple absolute bind", in: "/host/data:/container/data", want: "/host/data:/container/data:z"},
		{name: "non-root bind adds ownership flag", in: "/host/data:/container/data", addU: true, want: "/host/data:/container/data:z,U"},
		{name: "keeps explicit options", in: "/host/data:/container/data:ro", want: "/host/data:/container/data:ro,z"},
		{name: "explicit options add ownership when needed", in: "/host/data:/container/data:ro", addU: true, want: "/host/data:/container/data:ro,U,z"},
		{name: "keeps existing flags", in: "/host/data:/container/data:rw,U,z", want: "/host/data:/container/data:rw,U,z"},
		{name: "keeps private relabel flag", in: "/host/data:/container/data:Z", addU: true, want: "/host/data:/container/data:Z,U"},
		{name: "relative bind", in: "./data:/container/data", want: "./data:/container/data:z"},
		{name: "env bind", in: "${DB_PATH}:/var/lib/postgresql/data", want: "${DB_PATH}:/var/lib/postgresql/data:z"},
		{name: "named volume unchanged", in: "pgdata:/var/lib/postgresql/data", want: "pgdata:/var/lib/postgresql/data"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ensureVolumeFlags(tc.in, tc.addU); got != tc.want {
				t.Fatalf("ensureVolumeFlags(%q, %v) = %q, want %q", tc.in, tc.addU, got, tc.want)
			}
		})
	}
}

func TestUserNeedsVolumeOwnershipMapping(t *testing.T) {
	tests := []struct {
		user string
		want bool
	}{
		{"", false},
		{"root", false},
		{"0", false},
		{"0:0", false},
		{"1000:1000", true},
		{"app", true},
	}
	for _, tc := range tests {
		if got := userNeedsVolumeOwnershipMapping(tc.user); got != tc.want {
			t.Fatalf("userNeedsVolumeOwnershipMapping(%q) = %v, want %v", tc.user, got, tc.want)
		}
	}
}

func TestRenderContainerHealthCheck(t *testing.T) {
	svc := ServiceConfig{
		Image:  "ghcr.io/acme/server:1.0",
		Health: HealthConfig{Path: "/api/health", Port: 8080},
	}
	got := renderContainer("proj", "server", svc)
	wantContains := []string{
		"HealthCmd=curl -sf http://localhost:8080/api/health || exit 1",
		"HealthInterval=10s",
		"HealthTimeout=5s",
		"HealthRetries=3",
		"HealthStartPeriod=30s",
	}
	for _, needle := range wantContains {
		if !strings.Contains(got, needle) {
			t.Fatalf("rendered container missing %q:\n%s", needle, got)
		}
	}
}

func TestRenderContainerResourceLimits(t *testing.T) {
	svc := ServiceConfig{
		Image: "ghcr.io/acme/server:1.0",
		Resources: ResourceConfig{
			CPUs:   "2",
			Memory: "1g",
		},
	}
	got := renderContainer("proj", "server", svc)
	if !strings.Contains(got, "PodmanArgs=--cpus=2 --memory=1g") {
		t.Fatalf("should contain PodmanArgs with cpus and memory:\n%s", got)
	}
}

func TestRenderReplicaContainer(t *testing.T) {
	svc := ServiceConfig{
		Image:     "ghcr.io/acme/server:1.0",
		Replicas:  3,
		DependsOn: []string{"db"},
		Health:    HealthConfig{Path: "/api/health", Port: 8080},
		Env:       map[string]string{"FOO": "bar"},
		Volumes:   []string{"/data:/app/data"},
	}
	got := renderReplicaContainer("proj", "server", 2, svc)
	wantContains := []string{
		"ContainerName=proj-server-2",
		"Image=ghcr.io/acme/server:1.0",
		"After=proj-db.service",
		"HealthCmd=curl -sf http://localhost:8080/api/health || exit 1",
		"Environment=FOO=bar",
		"Volume=/data:/app/data:z",
	}
	for _, needle := range wantContains {
		if !strings.Contains(got, needle) {
			t.Fatalf("rendered replica missing %q:\n%s", needle, got)
		}
	}
	// Replicas should not have PublishPort
	if strings.Contains(got, "PublishPort") {
		t.Fatalf("replica should not have PublishPort:\n%s", got)
	}
}

func TestRenderReplicaContainerAddsOwnershipFlagWhenAnnotated(t *testing.T) {
	svc := ServiceConfig{
		Image:        "ghcr.io/acme/server:1.0",
		Replicas:     3,
		Volumes:      []string{"/data:/app/data"},
		volumeNeedsU: true,
	}
	got := renderReplicaContainer("proj", "server", 2, svc)
	if !strings.Contains(got, "Volume=/data:/app/data:z,U") {
		t.Fatalf("non-root annotated service should get :U volume flag:\n%s", got)
	}
}

func TestRenderQuadletFilesWithExpose(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {
			Image:    "ghcr.io/acme/server:1.0",
			Replicas: 2,
		},
		"db": {
			Image: "postgres:16.1",
		},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{
			HostPort: 9999,
			Routes:   map[string]string{"/api/": "server:8080"},
		},
		{HostPort: 5432, Target: "db:5432"},
	}}
	files := renderQuadletFiles("proj", services, services, expose, TraefikProvider{}, PodmanRuntime{}, "")
	fileNames := make([]string, 0, len(files))
	for _, f := range files {
		fileNames = append(fileNames, f.Name)
	}
	wantNames := []string{
		"proj.network",
		"proj-db.container",
		"proj-server-1.container",
		"proj-server-2.container",
		"proj-proxy.container",
	}
	for _, want := range wantNames {
		found := false
		for _, name := range fileNames {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing quadlet file %q; got: %v", want, fileNames)
		}
	}
	// Should NOT have proj-server.container (it's replicated)
	for _, name := range fileNames {
		if name == "proj-server.container" {
			t.Fatalf("replicated service should not produce proj-server.container; got: %v", fileNames)
		}
	}
}

func TestRenderQuadletFilesNoExpose(t *testing.T) {
	services := map[string]ServiceConfig{
		"db": {Image: "postgres:16.1"},
	}
	expose := ExposeConfig{}
	files := renderQuadletFiles("proj", services, services, expose, TraefikProvider{}, PodmanRuntime{}, "")
	for _, f := range files {
		if f.Name == "proj-proxy.container" {
			t.Fatal("should not generate proxy container when no expose entries")
		}
	}
}

func TestEffectiveReplicas(t *testing.T) {
	if effectiveReplicas(ServiceConfig{Replicas: 0}) != 1 {
		t.Fatal("0 replicas should default to 1")
	}
	if effectiveReplicas(ServiceConfig{Replicas: 3}) != 3 {
		t.Fatal("3 replicas should stay 3")
	}
}

func TestIsReplicated(t *testing.T) {
	if isReplicated(ServiceConfig{}) {
		t.Fatal("default should not be replicated")
	}
	if isReplicated(ServiceConfig{Replicas: 1}) {
		t.Fatal("replicas=1 should not be replicated")
	}
	if !isReplicated(ServiceConfig{Replicas: 2}) {
		t.Fatal("replicas=2 should be replicated")
	}
}

func TestRenderContainerMultilineEnv(t *testing.T) {
	jsonValue := "{\n  \"type\": \"service_account\",\n  \"url\": \"https://example.com/x509/user%40domain.com\"\n}"
	svc := ServiceConfig{
		Image: "server:1.0",
		Env: map[string]string{
			"GCP_KEY": jsonValue,
			"PLAIN":   "simple",
		},
	}
	got := renderContainer("proj", "server", svc)
	// Multi-line value with spaces/quotes must use systemd quoted form
	// Newlines → \n, quotes → \", % → %%
	want := `Environment="GCP_KEY={\n  \"type\": \"service_account\",\n  \"url\": \"https://example.com/x509/user%%40domain.com\"\n}"`
	if !strings.Contains(got, want) {
		t.Fatalf("expected quoted JSON env line:\n  want: %s\n  got:\n%s", want, got)
	}
	if !strings.Contains(got, "Environment=PLAIN=simple") {
		t.Fatalf("plain env value should be unchanged:\n%s", got)
	}
}

func TestFormatQuadletEnvSimple(t *testing.T) {
	got := formatQuadletEnv("KEY", "value")
	if got != "Environment=KEY=value\n" {
		t.Fatalf("expected simple format, got %q", got)
	}
}

func TestFormatQuadletEnvWithSpaces(t *testing.T) {
	got := formatQuadletEnv("KEY", "value with spaces")
	if got != "Environment=\"KEY=value with spaces\"\n" {
		t.Fatalf("expected quoted format, got %q", got)
	}
}

func TestFormatQuadletEnvWithQuotes(t *testing.T) {
	got := formatQuadletEnv("KEY", `{"type":"sa"}`)
	// No spaces/newlines/backslashes, just quotes → needs quoting
	want := "Environment=\"KEY={\\\"type\\\":\\\"sa\\\"}\"\n"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestFormatQuadletEnvPercent(t *testing.T) {
	got := formatQuadletEnv("KEY", "user%40domain.com")
	if got != "Environment=KEY=user%%40domain.com\n" {
		t.Fatalf("expected %% escaping, got %q", got)
	}
}

func TestFormatQuadletEnvJSON(t *testing.T) {
	json := "{\n  \"type\": \"service_account\",\n  \"project_id\": \"myproject\"\n}"
	got := formatQuadletEnv("GCP_KEY", json)
	// Should be quoted with escaped newlines, quotes, and backslashes
	if !strings.HasPrefix(got, "Environment=\"GCP_KEY=") {
		t.Fatalf("expected quoted form, got %q", got)
	}
	if strings.Contains(got, "\n  \"type\"") {
		t.Fatalf("raw newline should not appear in output, got %q", got)
	}
}

// TestFormatQuadletEnvRealisticGCPKey tests with a realistic-shaped service
// account JSON value that includes multiline text, escaped characters, and a
// URL-encoded email. The key material is intentionally fake.
func TestFormatQuadletEnvRealisticGCPKey(t *testing.T) {
	gcpKey := `{
  "type": "service_account",
  "project_id": "my-project-123",
  "private_key_id": "abcdef1234567890",
  "private_key": "FAKE_MULTILINE_PRIVATE_KEY\nLINE2\n",
  "client_email": "sa@my-project-123.iam.gserviceaccount.com",
  "client_x509_cert_url": "https://www.googleapis.com/robot/v1/metadata/x509/sa%40my-project-123.iam.gserviceaccount.com"
}`
	got := formatQuadletEnv("GCP_SERVICE_ACCOUNT_KEY", gcpKey)

	// Must be a single line (no raw newlines in output except the trailing \n)
	lines := strings.Split(got, "\n")
	// Should be exactly 2 parts: the env line + empty trailing
	if len(lines) != 2 || lines[1] != "" {
		t.Fatalf("expected single line + trailing newline, got %d lines: %q", len(lines), got)
	}

	envLine := lines[0]

	// Must use quoted form
	if !strings.HasPrefix(envLine, `Environment="GCP_SERVICE_ACCOUNT_KEY=`) {
		t.Fatalf("should use quoted form, got: %s", envLine)
	}
	if !strings.HasSuffix(envLine, `"`) {
		t.Fatalf("should end with closing quote, got: %s", envLine)
	}

	// Must contain escaped newlines (\\n) not raw ones
	if !strings.Contains(envLine, `\n`) {
		t.Fatalf("should contain escaped newlines, got: %s", envLine)
	}

	// Must contain escaped quotes (\")
	if !strings.Contains(envLine, `\"`) {
		t.Fatalf("should contain escaped quotes, got: %s", envLine)
	}

	// Must contain %% for the URL-encoded %40
	if !strings.Contains(envLine, "%%40") {
		t.Fatalf("should escape %% as %%%% for systemd, got: %s", envLine)
	}

	// Must contain the fake key marker with escaped backslash-n.
	// The literal \n in the private_key value becomes \\n in Go source,
	// which is a backslash followed by n. This needs to be double-escaped:
	// backslash → \\\\ and then the literal n stays as n
	if !strings.Contains(envLine, "FAKE_MULTILINE_PRIVATE_KEY") {
		t.Fatalf("should contain fake key marker, got: %s", envLine)
	}
}

// TestFormatQuadletEnvBackslash tests that backslashes are properly escaped.
func TestFormatQuadletEnvBackslash(t *testing.T) {
	got := formatQuadletEnv("KEY", "path\\to\\file")
	// Backslash needs quoting
	if !strings.HasPrefix(got, `Environment="KEY=`) {
		t.Fatalf("backslash should trigger quoting, got %q", got)
	}
	// Each \ should become \\
	if !strings.Contains(got, `path\\to\\file`) {
		t.Fatalf("backslashes should be escaped, got %q", got)
	}
}

// TestFormatQuadletEnvTabsAndCarriageReturns tests tab and CR escaping.
func TestFormatQuadletEnvTabsAndCarriageReturns(t *testing.T) {
	got := formatQuadletEnv("KEY", "col1\tcol2\r\n")
	if !strings.HasPrefix(got, `Environment="KEY=`) {
		t.Fatalf("tabs/CR should trigger quoting, got %q", got)
	}
	// Should contain \t and \r escapes
	envLine := strings.TrimSuffix(got, "\n")
	if !strings.Contains(envLine, `\t`) {
		t.Fatalf("tab should be escaped, got %q", envLine)
	}
	if !strings.Contains(envLine, `\r`) {
		t.Fatalf("carriage return should be escaped, got %q", envLine)
	}
}

// TestRenderContainerGCPServiceAccountKey tests end-to-end rendering of a container
// quadlet with a realistic GCP service account key as an environment variable.
func TestRenderContainerGCPServiceAccountKey(t *testing.T) {
	gcpKey := "{\n  \"type\": \"service_account\",\n  \"client_email\": \"sa@proj.iam.gserviceaccount.com\",\n  \"client_x509_cert_url\": \"https://www.googleapis.com/robot/v1/metadata/x509/sa%40proj.iam.gserviceaccount.com\"\n}"
	svc := ServiceConfig{
		Image: "server:1.0",
		Env: map[string]string{
			"GCP_KEY":  gcpKey,
			"APP_NAME": "myapp",
			"DB_HOST":  "db:5432",
		},
	}
	got := renderContainer("proj", "server", svc)

	// The rendered output must contain all env vars
	if !strings.Contains(got, "Environment=APP_NAME=myapp") {
		t.Fatalf("simple env APP_NAME missing:\n%s", got)
	}
	if !strings.Contains(got, "Environment=DB_HOST=db:5432") {
		t.Fatalf("simple env DB_HOST missing:\n%s", got)
	}

	// GCP_KEY must be in quoted form on a single line
	foundGCP := false
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "GCP_KEY") {
			foundGCP = true
			if !strings.HasPrefix(line, `Environment="GCP_KEY=`) {
				t.Fatalf("GCP_KEY should use quoted form:\n%s", line)
			}
			// Must not contain raw newlines within the value
			// (the loop already splits on \n, so if we're here, this line is a single line)
			// Must contain escaped content
			if !strings.Contains(line, `\"type\"`) {
				t.Fatalf("GCP_KEY should contain escaped type quote:\n%s", line)
			}
			if !strings.Contains(line, `\n`) {
				t.Fatalf("GCP_KEY should contain escaped newlines:\n%s", line)
			}
			if !strings.Contains(line, "%%40") {
				t.Fatalf("GCP_KEY should contain %%%%40 for URL-encoded @:\n%s", line)
			}
			break
		}
	}
	if !foundGCP {
		t.Fatalf("GCP_KEY env line not found in rendered output:\n%s", got)
	}
}
