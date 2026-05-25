package qqd

import (
	"strings"
	"testing"
)

func TestHasExposedServices(t *testing.T) {
	if hasExposedServices(ExposeConfig{}) {
		t.Fatal("empty expose should return false")
	}
	if !hasExposedServices(ExposeConfig{Entries: []ExposeEntry{{HostPort: 80}}}) {
		t.Fatal("should return true with entries")
	}
}

func TestGenerateTraefikStaticHTTPAndTCP(t *testing.T) {
	expose := ExposeConfig{Entries: []ExposeEntry{
		{
			HostPort: 9999,
			Routes: map[string]string{
				"/api/": "server:8080",
				"/":     "frontend:80",
			},
		},
		{
			HostPort: 5432,
			Target:   "db:5432",
		},
	}}
	got := generateTraefikStatic("proj", expose)
	wantContains := []string{
		"web-9999:",
		"\":9999\"",
		"tcp-5432:",
		"\":5432\"",
		"providers:",
		"file:",
		"directory: /etc/traefik/dynamic",
		"watch: true",
	}
	for _, needle := range wantContains {
		if !strings.Contains(got, needle) {
			t.Fatalf("static config missing %q:\n%s", needle, got)
		}
	}
}

func TestGenerateTraefikStaticWithTLS(t *testing.T) {
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
	got := generateTraefikStatic("proj", expose)
	if !strings.Contains(got, "tls-443:") {
		t.Fatalf("static config should include TLS entrypoint:\n%s", got)
	}
	if !strings.Contains(got, "\":443\"") {
		t.Fatalf("static config should include TLS port:\n%s", got)
	}
}

func TestGenerateTraefikDynamicHTTP(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {
			Image:    "server:1.0",
			Replicas: 2,
		},
		"frontend": {
			Image: "frontend:1.0",
		},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{
			HostPort: 9999,
			Routes: map[string]string{
				"/api/": "server:8080",
				"/":     "frontend:80",
			},
		},
	}}
	got := generateTraefikDynamic("proj", services, expose)
	wantContains := []string{
		"http:",
		"routers:",
		"services:",
		"PathPrefix(`/api/`)",
		"PathPrefix(`/`)",
		"web-9999",
		// Server replicas
		"http://proj-server-1:8080",
		"http://proj-server-2:8080",
		// Frontend non-replicated
		"http://proj-frontend:80",
	}
	for _, needle := range wantContains {
		if !strings.Contains(got, needle) {
			t.Fatalf("dynamic config missing %q:\n%s", needle, got)
		}
	}
	// /api/ should have higher priority than /
	if !strings.Contains(got, "priority: 5") {
		t.Fatalf("api route should have priority 5 (len of /api/):\n%s", got)
	}
	if !strings.Contains(got, "priority: 1") {
		t.Fatalf("root route should have priority 1 (len of /):\n%s", got)
	}
}

func TestGenerateTraefikDynamicTCP(t *testing.T) {
	services := map[string]ServiceConfig{
		"db": {Image: "postgres:16.1"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{HostPort: 5432, Target: "db:5432"},
	}}
	got := generateTraefikDynamic("proj", services, expose)
	wantContains := []string{
		"tcp:",
		"routers:",
		"services:",
		"HostSNI(`*`)",
		"tcp-5432",
		"address: \"proj-db:5432\"",
	}
	for _, needle := range wantContains {
		if !strings.Contains(got, needle) {
			t.Fatalf("TCP dynamic config missing %q:\n%s", needle, got)
		}
	}
}

func TestGenerateTraefikDynamicTCPReplicated(t *testing.T) {
	services := map[string]ServiceConfig{
		"db": {Image: "postgres:16.1", Replicas: 3},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{HostPort: 5432, Target: "db:5432"},
	}}
	got := generateTraefikDynamic("proj", services, expose)
	for i := 1; i <= 3; i++ {
		needle := "proj-db-"
		if !strings.Contains(got, needle) {
			t.Fatalf("TCP replicated service should have replica entries:\n%s", got)
		}
	}
	if strings.Contains(got, "\"proj-db:5432\"") {
		t.Fatalf("TCP replicated service should not use non-replica name:\n%s", got)
	}
}

func TestRenderProxyContainerTraefik(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {
			Image:    "server:1.0",
			Replicas: 2,
		},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{
			HostPort: 9999,
			Routes:   map[string]string{"/api/": "server:8080"},
		},
	}}
	pf := renderProxyContainer("proj", "docker.io/library/traefik:v3.6", services, expose)
	if pf.Name != "proj-proxy.container" {
		t.Fatalf("proxy file name mismatch: %q", pf.Name)
	}
	wantContains := []string{
		"ContainerName=proj-proxy",
		"Image=docker.io/library/traefik:v3.6",
		"PublishPort=9999:9999",
		"Network=proj.network",
		"traefik.yml:/etc/traefik/traefik.yml:ro,z",
		"dynamic:/etc/traefik/dynamic:ro,z",
		"After=proj-server-1.service proj-server-2.service",
		"Wants=proj-server-1.service proj-server-2.service",
	}
	for _, needle := range wantContains {
		if !strings.Contains(pf.Content, needle) {
			t.Fatalf("proxy container missing %q:\n%s", needle, pf.Content)
		}
	}
}

func TestRenderProxyContainerWithTLS(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0", Replicas: 2},
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
	pf := renderProxyContainer("proj", "docker.io/library/traefik:v3.6", services, expose)
	wantContains := []string{
		"PublishPort=80:80",
		"PublishPort=443:443",
		"Volume=/etc/letsencrypt:" + traefikTLSMountPath("/etc/letsencrypt") + ":ro",
	}
	for _, needle := range wantContains {
		if !strings.Contains(pf.Content, needle) {
			t.Fatalf("TLS proxy container missing %q:\n%s", needle, pf.Content)
		}
	}
}

func TestRenderProxyContainerWithMultipleTLSDirs(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
		"admin":  {Image: "admin:1.0"},
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
		{
			HostPort: 8080,
			Routes:   map[string]string{"/": "admin:8080"},
			TLS: &TLSConfig{
				Port:       8443,
				CertsDir:   "/srv/certs",
				ServerName: "admin.example.com",
			},
		},
	}}
	pf := renderProxyContainer("proj", "docker.io/library/traefik:v3.6", services, expose)
	wantContains := []string{
		"Volume=/etc/letsencrypt:" + traefikTLSMountPath("/etc/letsencrypt") + ":ro",
		"Volume=/srv/certs:" + traefikTLSMountPath("/srv/certs") + ":ro",
	}
	for _, needle := range wantContains {
		if !strings.Contains(pf.Content, needle) {
			t.Fatalf("TLS proxy container missing %q:\n%s", needle, pf.Content)
		}
	}
}

func TestRenderProxyContainerTCPAndHTTP(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
		"db":     {Image: "postgres:16.1"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{
			HostPort: 9999,
			Routes:   map[string]string{"/api/": "server:8080"},
		},
		{HostPort: 5432, Target: "db:5432"},
	}}
	pf := renderProxyContainer("proj", "docker.io/library/traefik:v3.6", services, expose)
	wantContains := []string{
		"PublishPort=9999:9999",
		"PublishPort=5432:5432",
		"After=proj-db.service proj-server.service",
	}
	for _, needle := range wantContains {
		if !strings.Contains(pf.Content, needle) {
			t.Fatalf("TCP+HTTP proxy container missing %q:\n%s", needle, pf.Content)
		}
	}
}

func TestExposedServiceDeps(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0", Replicas: 2},
		"db":     {Image: "postgres:16.1"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{
			HostPort: 80,
			Routes:   map[string]string{"/api/": "server:8080"},
		},
		{HostPort: 5432, Target: "db:5432"},
	}}
	deps := exposedServiceDeps("proj", services, expose)
	want := []string{
		"proj-db.service",
		"proj-server-1.service",
		"proj-server-2.service",
	}
	if len(deps) != len(want) {
		t.Fatalf("deps count mismatch: got %v want %v", deps, want)
	}
	for i, w := range want {
		if deps[i] != w {
			t.Fatalf("deps[%d] = %q, want %q", i, deps[i], w)
		}
	}
}

func TestGenerateTraefikStaticWithDashboard(t *testing.T) {
	expose := ExposeConfig{
		Dashboard: 1111,
		Entries: []ExposeEntry{
			{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
		},
	}
	static := generateTraefikStatic("proj", expose)
	if !strings.Contains(static, "api:") {
		t.Fatalf("should contain api section:\n%s", static)
	}
	if !strings.Contains(static, "dashboard: true") {
		t.Fatalf("should enable dashboard:\n%s", static)
	}
	if !strings.Contains(static, "insecure: true") {
		t.Fatalf("should enable insecure API:\n%s", static)
	}
	// Should have an explicit traefik entrypoint for the API
	if !strings.Contains(static, "traefik:") {
		t.Fatalf("should have explicit traefik entrypoint:\n%s", static)
	}
	// With no 8080 conflict, API uses default port 8080
	if !strings.Contains(static, "address: \":8080\"") {
		t.Fatalf("API should use default port 8080 when no conflict:\n%s", static)
	}
}

func TestGenerateTraefikStaticDashboardPortConflict(t *testing.T) {
	// Dashboard + entrypoint on 8080 should use alternate API port
	expose := ExposeConfig{
		Dashboard: 1111,
		Entries: []ExposeEntry{
			{HostPort: 8080, Routes: map[string]string{"/": "server:8080"}},
		},
	}
	static := generateTraefikStatic("proj", expose)
	if !strings.Contains(static, "address: \":19090\"") {
		t.Fatalf("API should use 19090 when 8080 is taken:\n%s", static)
	}
	// Should still have the user entrypoint on 8080
	if !strings.Contains(static, "web-8080:") {
		t.Fatalf("should have web-8080 entrypoint:\n%s", static)
	}
}

func TestRenderProxyContainerDashboardPortConflict(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
	}
	expose := ExposeConfig{
		Dashboard: 1111,
		Entries: []ExposeEntry{
			{HostPort: 8080, Routes: map[string]string{"/": "server:8080"}},
		},
	}
	qf := renderProxyContainer("proj", "docker.io/library/traefik:v3.6", services, expose)
	// Dashboard should map to the alternate API port
	if !strings.Contains(qf.Content, "PublishPort=1111:19090") {
		t.Fatalf("dashboard should map to 19090 when 8080 conflicts:\n%s", qf.Content)
	}
	if !strings.Contains(qf.Content, "PublishPort=8080:8080") {
		t.Fatalf("should still publish HTTP port:\n%s", qf.Content)
	}
}

func TestGenerateTraefikStaticWithoutDashboard(t *testing.T) {
	expose := ExposeConfig{
		Entries: []ExposeEntry{
			{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
		},
	}
	static := generateTraefikStatic("proj", expose)
	if strings.Contains(static, "api:") {
		t.Fatalf("should NOT contain api section when dashboard disabled:\n%s", static)
	}
}

func TestRenderProxyContainerWithDashboard(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
	}
	expose := ExposeConfig{
		Dashboard: 1111,
		Entries: []ExposeEntry{
			{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
		},
	}
	qf := renderProxyContainer("proj", "docker.io/library/traefik:v3.6", services, expose)
	// Maps external 1111 to Traefik's internal API port 8080
	if !strings.Contains(qf.Content, "PublishPort=1111:8080") {
		t.Fatalf("should publish dashboard port mapped to 8080:\n%s", qf.Content)
	}
	if !strings.Contains(qf.Content, "PublishPort=80:80") {
		t.Fatalf("should still publish HTTP port:\n%s", qf.Content)
	}
}

func TestSanitizeTraefikName(t *testing.T) {
	cases := [][2]string{
		{"proj-server-8080-/api/-tls", "proj-server-8080-api-tls"},
		{"proj.server:80", "proj-server-80"},
		{"name//with//slashes", "namewithslashes"},
	}
	for _, c := range cases {
		got := sanitizeTraefikName(c[0])
		if got != c[1] {
			t.Fatalf("sanitizeTraefikName(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestIsServiceExposed(t *testing.T) {
	expose := ExposeConfig{Entries: []ExposeEntry{
		{
			HostPort: 80,
			Routes:   map[string]string{"/api/": "server:8080", "/": "frontend:80"},
		},
		{HostPort: 5432, Target: "db:5432"},
	}}
	if !isServiceExposed("server", expose) {
		t.Fatal("server should be exposed (HTTP route)")
	}
	if !isServiceExposed("frontend", expose) {
		t.Fatal("frontend should be exposed (HTTP route)")
	}
	if !isServiceExposed("db", expose) {
		t.Fatal("db should be exposed (TCP target)")
	}
	if isServiceExposed("worker", expose) {
		t.Fatal("worker should NOT be exposed")
	}

	// isServiceHTTPExposed: only HTTP routes, not TCP
	if !isServiceHTTPExposed("server", expose) {
		t.Fatal("server should be HTTP exposed")
	}
	if !isServiceHTTPExposed("frontend", expose) {
		t.Fatal("frontend should be HTTP exposed")
	}
	if isServiceHTTPExposed("db", expose) {
		t.Fatal("db should NOT be HTTP exposed (TCP only)")
	}
	if isServiceHTTPExposed("worker", expose) {
		t.Fatal("worker should NOT be HTTP exposed")
	}
}

func TestGenerateTraefikDynamicWithSlotOverride(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{
			HostPort: 80,
			Routes:   map[string]string{"/": "server:8080"},
		},
	}}
	// Without slot override: uses standard name
	got := generateTraefikDynamic("proj", services, expose)
	if !strings.Contains(got, "http://proj-server:8080") {
		t.Fatalf("without override should use standard name:\n%s", got)
	}

	// With slot override: uses slot container name
	opts := DynamicConfigOpts{
		SlotOverrides: map[string]string{"server": "proj-server-a1b2c3d4"},
	}
	got = generateTraefikDynamicOpts("proj", services, expose, opts)
	if !strings.Contains(got, "http://proj-server-a1b2c3d4:8080") {
		t.Fatalf("with slot override should use slot name:\n%s", got)
	}
	if strings.Contains(got, "http://proj-server:8080") {
		t.Fatalf("with slot override should NOT contain standard name:\n%s", got)
	}
}

func TestGenerateTraefikDynamicWithSlotOverrideTCP(t *testing.T) {
	services := map[string]ServiceConfig{
		"db": {Image: "postgres:16.1"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{HostPort: 5432, Target: "db:5432"},
	}}
	opts := DynamicConfigOpts{
		SlotOverrides: map[string]string{"db": "proj-db-e5f6a7b8"},
	}
	got := generateTraefikDynamicOpts("proj", services, expose, opts)
	if !strings.Contains(got, "\"proj-db-e5f6a7b8:5432\"") {
		t.Fatalf("TCP slot override should use slot name:\n%s", got)
	}
}

func TestGenerateTraefikDynamicWithReplicaExclusion(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0", Replicas: 3},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{
			HostPort: 80,
			Routes:   map[string]string{"/": "server:8080"},
		},
	}}
	// Exclude replica 2
	opts := DynamicConfigOpts{
		ExcludeReplicas: map[string]map[int]bool{
			"server": {2: true},
		},
	}
	got := generateTraefikDynamicOpts("proj", services, expose, opts)
	if !strings.Contains(got, "http://proj-server-1:8080") {
		t.Fatalf("replica 1 should be present:\n%s", got)
	}
	if strings.Contains(got, "http://proj-server-2:8080") {
		t.Fatalf("replica 2 should be excluded:\n%s", got)
	}
	if !strings.Contains(got, "http://proj-server-3:8080") {
		t.Fatalf("replica 3 should be present:\n%s", got)
	}
}

func TestGenerateTraefikDynamicTLSCertificates(t *testing.T) {
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
	got := generateTraefikDynamic("proj", services, expose)
	wantContains := []string{
		"tls:",
		"certificates:",
		"certFile: " + traefikTLSMountPath("/etc/letsencrypt") + "/live/example.com/fullchain.pem",
		"keyFile: " + traefikTLSMountPath("/etc/letsencrypt") + "/live/example.com/privkey.pem",
	}
	for _, needle := range wantContains {
		if !strings.Contains(got, needle) {
			t.Fatalf("TLS dynamic config missing %q:\n%s", needle, got)
		}
	}
}

func TestGenerateTraefikDynamicTLSCertificatesWithMultipleDirs(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
		"admin":  {Image: "admin:1.0"},
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
		{
			HostPort: 8080,
			Routes:   map[string]string{"/": "admin:8080"},
			TLS: &TLSConfig{
				Port:       8443,
				CertsDir:   "/srv/certs",
				ServerName: "admin.example.com",
			},
		},
	}}
	got := generateTraefikDynamic("proj", services, expose)
	wantContains := []string{
		"certFile: " + traefikTLSMountPath("/etc/letsencrypt") + "/live/example.com/fullchain.pem",
		"keyFile: " + traefikTLSMountPath("/etc/letsencrypt") + "/live/example.com/privkey.pem",
		"certFile: " + traefikTLSMountPath("/srv/certs") + "/live/admin.example.com/fullchain.pem",
		"keyFile: " + traefikTLSMountPath("/srv/certs") + "/live/admin.example.com/privkey.pem",
	}
	for _, needle := range wantContains {
		if !strings.Contains(got, needle) {
			t.Fatalf("TLS dynamic config missing %q:\n%s", needle, got)
		}
	}
}

func TestGenerateTraefikDynamicNoTLSWithoutConfig(t *testing.T) {
	services := map[string]ServiceConfig{
		"server": {Image: "server:1.0"},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{
			HostPort: 80,
			Routes:   map[string]string{"/": "server:8080"},
		},
	}}
	got := generateTraefikDynamic("proj", services, expose)
	if strings.Contains(got, "tls:") {
		t.Fatalf("should not contain tls section without TLS config:\n%s", got)
	}
}

func TestGenerateTraefikDynamicReplicaExclusionTCP(t *testing.T) {
	services := map[string]ServiceConfig{
		"db": {Image: "postgres:16.1", Replicas: 2},
	}
	expose := ExposeConfig{Entries: []ExposeEntry{
		{HostPort: 5432, Target: "db:5432"},
	}}
	opts := DynamicConfigOpts{
		ExcludeReplicas: map[string]map[int]bool{
			"db": {1: true},
		},
	}
	got := generateTraefikDynamicOpts("proj", services, expose, opts)
	if strings.Contains(got, "\"proj-db-1:5432\"") {
		t.Fatalf("replica 1 should be excluded from TCP:\n%s", got)
	}
	if !strings.Contains(got, "\"proj-db-2:5432\"") {
		t.Fatalf("replica 2 should be present in TCP:\n%s", got)
	}
}
