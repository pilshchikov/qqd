package qqd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestPlanRiskMutableImageTag(t *testing.T) {
	cases := map[string]bool{
		"nginx":           true, // implicit :latest
		"nginx:latest":    true,
		"nginx:main":      true,
		"nginx:1.27":      false,
		"nginx:1.27.0-r1": false,
		"":                false,
	}
	for img, want := range cases {
		if got := isMutableImageTag(img); got != want {
			t.Errorf("isMutableImageTag(%q) = %v, want %v", img, got, want)
		}
	}
}

func TestDetectPlanRisksExposedNoHealth(t *testing.T) {
	cfg := ProjectConfig{
		Name: "p",
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/me/web:1.2"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name: "main", Host: "h",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "web:8080"}},
				}},
			},
		},
	}
	eff, _ := resolveTarget(cfg, "main", nil)
	risks := detectPlanRisks(cfg, eff, planMode{})
	want := false
	for _, r := range risks {
		if r.Code == "exposed-no-health" && r.Service == "web" {
			want = true
		}
	}
	if !want {
		t.Fatalf("expected exposed-no-health risk for web; got: %+v", risks)
	}
}

func TestDetectPlanRisksMutableTag(t *testing.T) {
	cfg := ProjectConfig{
		Name: "p",
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/me/web:latest"},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "h"},
		},
	}
	eff, _ := resolveTarget(cfg, "main", nil)
	risks := detectPlanRisks(cfg, eff, planMode{})
	found := false
	for _, r := range risks {
		if r.Code == "mutable-image-tag" && r.Level == RiskWarn {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected mutable-image-tag warn risk; got: %+v", risks)
	}
}

func TestDetectPlanRisksCaddyTCPPassthroughIsDanger(t *testing.T) {
	cfg := ProjectConfig{
		Name:  "p",
		Proxy: "caddy",
		Services: map[string]ServiceConfig{
			"db": {Image: "postgres:16"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name: "main", Host: "h",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 5432, Target: "db:5432"},
				}},
			},
		},
	}
	eff, _ := resolveTarget(cfg, "main", nil)
	risks := detectPlanRisks(cfg, eff, planMode{})
	var got *PlanRisk
	for i := range risks {
		if risks[i].Code == "caddy-tcp-passthrough" {
			got = &risks[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("expected caddy-tcp-passthrough risk; got: %+v", risks)
	}
	if got.Level != RiskDanger {
		t.Fatalf("caddy-tcp-passthrough should be DANGER; got %s", got.Level)
	}
}

func TestPlanJSONOutputShape(t *testing.T) {
	cfg := ProjectConfig{
		Name:    "myapp",
		Runtime: "podman",
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/me/web:1.2", Health: HealthConfig{Path: "/h", Port: 8080}},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name: "main", Host: "192.0.2.50",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 80, Routes: map[string]string{"/": "web:8080"}},
				}},
			},
		},
	}
	var buf bytes.Buffer
	if err := writeDeployPlanJSON(&buf, cfg, "main", nil, planMode{}); err != nil {
		t.Fatalf("writeDeployPlanJSON: %v", err)
	}
	var out PlanResult
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("plan JSON did not decode into PlanResult: %v\nraw:\n%s", err, buf.String())
	}
	if out.Project != "myapp" || out.Runtime != "podman" {
		t.Errorf("unexpected top-level: %+v", out)
	}
	if len(out.Targets) != 1 || out.Targets[0].Name != "main" {
		t.Errorf("expected one target named 'main'; got %+v", out.Targets)
	}
	if len(out.Targets[0].Services) != 1 || out.Targets[0].Services[0].Name != "web" {
		t.Errorf("expected web service in plan; got %+v", out.Targets[0].Services)
	}
	if out.Targets[0].Services[0].DeployMode != "zero-downtime slot" {
		t.Errorf("web is HTTP-exposed and should be zero-downtime slot; got %q", out.Targets[0].Services[0].DeployMode)
	}
	// Risks array must be present (even if empty); its absence breaks consumers.
	if out.Risks == nil {
		t.Errorf("risks field should be a (possibly empty) array, got nil")
	}
}

func TestPlanJSONIncludesRisks(t *testing.T) {
	cfg := ProjectConfig{
		Name: "p",
		Services: map[string]ServiceConfig{
			"web": {Image: "ghcr.io/me/web:latest"}, // mutable tag
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "h"},
		},
	}
	var buf bytes.Buffer
	if err := writeDeployPlanJSON(&buf, cfg, "main", nil, planMode{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "mutable-image-tag") {
		t.Fatalf("plan JSON should include mutable-image-tag risk:\n%s", buf.String())
	}
}
