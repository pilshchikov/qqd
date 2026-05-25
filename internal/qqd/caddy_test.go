package qqd

import (
	"io"
	"strings"
	"testing"
)

func TestCaddyDynamicHTTPRoutes(t *testing.T) {
	services := map[string]ServiceConfig{
		"server":   {Image: "server:1.0", Replicas: 2},
		"frontend": {Image: "frontend:1.0"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{
			HostPort: 80,
			Routes: map[string]string{
				"/api/": "server:8080",
				"/":     "frontend:80",
			},
		},
	}}
	got := generateCaddyDynamic("proj", services, expose, DynamicConfigOpts{})

	wantContains := []string{
		":80 {",
		"reverse_proxy",
		// Replicated server upstreams
		"proj-server-1:8080",
		"proj-server-2:8080",
		// Non-replicated frontend
		"proj-frontend:80",
		// Path-based routing
		"handle_path /api/*",
	}
	for _, needle := range wantContains {
		if !strings.Contains(got, needle) {
			t.Fatalf("caddy dynamic config missing %q:\n%s", needle, got)
		}
	}
}

func TestCaddyDynamicTCPPassthrough(t *testing.T) {
	services := map[string]ServiceConfig{
		"db": {Image: "postgres:16"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{HostPort: 5432, Target: "db:5432"},
	}}
	got := generateCaddyDynamic("proj", services, expose, DynamicConfigOpts{})

	if !strings.Contains(got, ":5432 {") {
		t.Fatalf("caddy TCP config missing port block:\n%s", got)
	}
	if !strings.Contains(got, "proj-db:5432") {
		t.Fatalf("caddy TCP config missing upstream:\n%s", got)
	}
}

func TestCaddyDynamicSlotOverride(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
	}}
	opts := DynamicConfigOpts{
		SlotOverrides: map[string]string{"server": "proj-server-a1b2c3d4"},
	}
	got := generateCaddyDynamic("proj", services, expose, opts)

	if !strings.Contains(got, "proj-server-a1b2c3d4:8080") {
		t.Fatalf("caddy slot override not applied:\n%s", got)
	}
	if strings.Contains(got, "proj-server:8080") {
		t.Fatalf("caddy should not contain default DNS name when slot override is set:\n%s", got)
	}
}

func TestCaddyDynamicExcludeReplica(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0", Replicas: 3},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
	}}
	opts := DynamicConfigOpts{
		ExcludeReplicas: map[string]map[int]bool{
			"server": {2: true},
		},
	}
	got := generateCaddyDynamic("proj", services, expose, opts)

	if !strings.Contains(got, "proj-server-1:8080") {
		t.Fatalf("caddy should include replica 1:\n%s", got)
	}
	if strings.Contains(got, "proj-server-2:8080") {
		t.Fatalf("caddy should exclude replica 2:\n%s", got)
	}
	if !strings.Contains(got, "proj-server-3:8080") {
		t.Fatalf("caddy should include replica 3:\n%s", got)
	}
}

func TestCaddyDynamicTLS(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{
			HostPort: 80,
			Routes:   map[string]string{"/": "server:8080"},
			TLS: &TLSConfig{
				Port:       443,
				CertsDir:   "/etc/letsencrypt",
				ServerName: "example.com",
			},
		},
	}}
	got := generateCaddyDynamic("proj", services, expose, DynamicConfigOpts{})

	wantContains := []string{
		":80 {",
		":443 {",
		"tls ",
		"fullchain.pem",
		"privkey.pem",
		"example.com",
	}
	for _, needle := range wantContains {
		if !strings.Contains(got, needle) {
			t.Fatalf("caddy TLS config missing %q:\n%s", needle, got)
		}
	}
}

func TestCaddyContainerQuadlet(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
	}}
	qf := renderCaddyContainer("proj", "docker.io/library/caddy:2-alpine", services, expose)

	if qf.Name != "proj-proxy.container" {
		t.Fatalf("expected proj-proxy.container, got %s", qf.Name)
	}
	wantContains := []string{
		"ContainerName=proj-proxy",
		"Image=docker.io/library/caddy:2-alpine",
		"Network=proj.network",
		"PublishPort=80:80",
		"Caddyfile:ro,z",
		"Restart=always",
		"WantedBy=default.target",
	}
	for _, needle := range wantContains {
		if !strings.Contains(qf.Content, needle) {
			t.Fatalf("caddy quadlet missing %q:\n%s", needle, qf.Content)
		}
	}
}

func TestCaddyContainerCustomImage(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
	}}
	qf := renderCaddyContainer("proj", "my-registry/caddy-l4:latest", services, expose)

	if !strings.Contains(qf.Content, "Image=my-registry/caddy-l4:latest") {
		t.Fatalf("caddy quadlet should use custom image:\n%s", qf.Content)
	}
}

func TestCaddyContainerTLSVolumes(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{
			HostPort: 80,
			Routes:   map[string]string{"/": "server:8080"},
			TLS: &TLSConfig{
				Port:       443,
				CertsDir:   "/etc/letsencrypt",
				ServerName: "example.com",
			},
		},
	}}
	qf := renderCaddyContainer("proj", "docker.io/library/caddy:2-alpine", services, expose)

	if !strings.Contains(qf.Content, "PublishPort=443:443") {
		t.Fatalf("caddy quadlet should publish TLS port:\n%s", qf.Content)
	}
	if !strings.Contains(qf.Content, "/etc/letsencrypt:") {
		t.Fatalf("caddy quadlet should mount TLS certs dir:\n%s", qf.Content)
	}
}

// TestCaddyStaticConfigIsEmpty pins the audited behavior: Caddy is configured
// entirely via the bind-mounted Caddyfile, so the static config generator
// returns "" and the deploy layer skips writing an empty file. If you change
// this (e.g. switch Caddy to JSON config), update both this test AND the
// proxy-caddy.md doc.
func TestCaddyStaticConfigIsEmpty(t *testing.T) {
	expose := ExposeConfig{Entries: []ExposeEntry{
		{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
	}}
	got := CaddyProvider{}.GenerateStaticConfig("proj", expose)
	if got != "" {
		t.Fatalf("Caddy GenerateStaticConfig should return \"\" (Caddyfile-only); got %d bytes:\n%s", len(got), got)
	}
}

func TestCaddyProviderInterface(t *testing.T) {
	var p ProxyProvider = CaddyProvider{}

	if p.ContainerName("proj") != "proj-proxy" {
		t.Fatalf("unexpected container name: %s", p.ContainerName("proj"))
	}
	if p.ServiceUnit("proj") != "proj-proxy.service" {
		t.Fatalf("unexpected service unit: %s", p.ServiceUnit("proj"))
	}
	if !strings.Contains(p.StaticConfigPath("proj"), "caddy.json") {
		t.Fatalf("unexpected static config path: %s", p.StaticConfigPath("proj"))
	}
	if !strings.Contains(p.DynamicConfigPath("proj"), "Caddyfile") {
		t.Fatalf("unexpected dynamic config path (should be a Caddyfile, not routes.json): %s", p.DynamicConfigPath("proj"))
	}
}

func TestTraefikProviderInterface(t *testing.T) {
	var p ProxyProvider = TraefikProvider{}

	if p.ContainerName("proj") != "proj-proxy" {
		t.Fatalf("unexpected container name: %s", p.ContainerName("proj"))
	}
	if p.ServiceUnit("proj") != "proj-proxy.service" {
		t.Fatalf("unexpected service unit: %s", p.ServiceUnit("proj"))
	}
	if !strings.Contains(p.StaticConfigPath("proj"), "traefik.yml") {
		t.Fatalf("unexpected static config path: %s", p.StaticConfigPath("proj"))
	}
	if !strings.Contains(p.DynamicConfigPath("proj"), "routes.yml") {
		t.Fatalf("unexpected dynamic config path: %s", p.DynamicConfigPath("proj"))
	}
}

func TestProxyProviderByName(t *testing.T) {
	tests := []struct {
		name     string
		image    string
		wantType string
		wantPath string
	}{
		{"", "", "traefik", "traefik.yml"},
		{"traefik", "", "traefik", "traefik.yml"},
		{"Traefik", "", "traefik", "traefik.yml"},
		{"caddy", "", "caddy", "caddy.json"},
		{"Caddy", "", "caddy", "caddy.json"},
	}
	for _, tt := range tests {
		p := proxyProviderByName(tt.name, tt.image)
		got := p.StaticConfigPath("proj")
		if !strings.Contains(got, tt.wantPath) {
			t.Fatalf("proxyProviderByName(%q) returned path %q, want containing %q", tt.name, got, tt.wantPath)
		}
	}
}

func TestProxyCustomImage(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
	}}

	// Traefik with custom image
	tp := TraefikProvider{Image: "my-registry/traefik:custom"}
	tqf := tp.RenderContainerQuadlet("proj", services, expose)
	if !strings.Contains(tqf.Content, "Image=my-registry/traefik:custom") {
		t.Fatalf("traefik should use custom image:\n%s", tqf.Content)
	}

	// Caddy with custom image
	cp := CaddyProvider{Image: "my-registry/caddy-l4:latest"}
	cqf := cp.RenderContainerQuadlet("proj", services, expose)
	if !strings.Contains(cqf.Content, "Image=my-registry/caddy-l4:latest") {
		t.Fatalf("caddy should use custom image:\n%s", cqf.Content)
	}

	// Traefik default image
	td := TraefikProvider{}
	tdqf := td.RenderContainerQuadlet("proj", services, expose)
	if !strings.Contains(tdqf.Content, "Image=docker.io/library/traefik:v3.6") {
		t.Fatalf("traefik should use default image:\n%s", tdqf.Content)
	}

	// Caddy default image
	cd := CaddyProvider{}
	cdqf := cd.RenderContainerQuadlet("proj", services, expose)
	if !strings.Contains(cdqf.Content, "Image=docker.io/library/caddy:2-alpine") {
		t.Fatalf("caddy should use default image:\n%s", cdqf.Content)
	}
}

func TestProxyImageFromConfig(t *testing.T) {
	p := proxyProviderByName("caddy", "ghcr.io/custom/caddy:v2")
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
	}}
	qf := p.RenderContainerQuadlet("proj", services, expose)
	if !strings.Contains(qf.Content, "Image=ghcr.io/custom/caddy:v2") {
		t.Fatalf("proxy_image from config should be used:\n%s", qf.Content)
	}
}

func TestApplyConfigProxySetsProvider(t *testing.T) {
	app := &App{Stdout: io.Discard}

	// No config proxy — should remain nil (defaults to Traefik via proxy())
	app.applyConfig(ProjectConfig{Name: "test"})
	if app.Proxy != nil {
		t.Fatal("proxy should remain nil when config has no proxy field")
	}

	// Config says caddy
	app.applyConfig(ProjectConfig{Name: "test", Proxy: "caddy"})
	if app.Proxy == nil {
		t.Fatal("proxy should be set after applyConfig with caddy")
	}
	if !strings.Contains(app.Proxy.StaticConfigPath("proj"), "caddy.json") {
		t.Fatal("proxy should be CaddyProvider")
	}

	// Once set, should not be overridden
	app.applyConfig(ProjectConfig{Name: "test", Proxy: "traefik"})
	if !strings.Contains(app.Proxy.StaticConfigPath("proj"), "caddy.json") {
		t.Fatal("proxy should not be overridden once set")
	}
}

func TestApplyConfigProxyWithImage(t *testing.T) {
	app := &App{Stdout: io.Discard}
	app.applyConfig(ProjectConfig{
		Name:       "test",
		Proxy:      "caddy",
		ProxyImage: "my-registry/caddy:custom",
	})

	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
	}}
	qf := app.Proxy.RenderContainerQuadlet("proj", services, expose)
	if !strings.Contains(qf.Content, "Image=my-registry/caddy:custom") {
		t.Fatalf("should use proxy_image from config:\n%s", qf.Content)
	}
}

func TestCaddyAndTraefikProduceDifferentConfigs(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
	}}

	traefik := TraefikProvider{}
	caddy := CaddyProvider{}

	tStatic := traefik.GenerateStaticConfig("proj", expose)
	cStatic := caddy.GenerateStaticConfig("proj", expose)

	if tStatic == cStatic {
		t.Fatal("traefik and caddy should produce different static configs")
	}

	tDynamic := traefik.GenerateDynamicConfig("proj", services, expose, DynamicConfigOpts{})
	cDynamic := caddy.GenerateDynamicConfig("proj", services, expose, DynamicConfigOpts{})

	if tDynamic == cDynamic {
		t.Fatal("traefik and caddy should produce different dynamic configs")
	}

	// Both should reference the same upstream
	if !strings.Contains(tDynamic, "proj-server") {
		t.Fatal("traefik dynamic should reference proj-server")
	}
	if !strings.Contains(cDynamic, "proj-server") {
		t.Fatal("caddy dynamic should reference proj-server")
	}
}
