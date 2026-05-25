package qqd

import (
	"encoding/json"
	"io"
)

// StatusResult is the top-level JSON output for qqd status.
type StatusResult struct {
	Targets []TargetStatus `json:"targets"`
}

// TargetStatus describes the status of one deployment target.
type TargetStatus struct {
	Name     string          `json:"name"`
	Host     string          `json:"host"`
	Backend  string          `json:"backend,omitempty"` // "systemd" or "direct"
	Services []ServiceStatus `json:"services"`
}

// ServiceStatus describes the runtime state of one service.
type ServiceStatus struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	Image     string `json:"image,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	Uptime    string `json:"uptime,omitempty"`
}

// PlanResult is the top-level JSON output for `qqd plan --output json`.
//
// The schema is intentionally narrow: targets, services per target, and a flat
// risks array. Risks reference target/service by name so consumers can join
// them back. CI gates can filter by `risks[].level == "danger"` to fail fast.
type PlanResult struct {
	Project string       `json:"project"`
	Runtime string       `json:"runtime"`
	Proxy   string       `json:"proxy,omitempty"`
	Sync    string       `json:"sync"`
	Mode    PlanModeJSON `json:"mode"`
	Targets []TargetPlan `json:"targets"`
	Risks   []PlanRisk   `json:"risks"`
}

// PlanModeJSON mirrors the planMode flags in JSON form.
type PlanModeJSON struct {
	Rebuild    bool `json:"rebuild"`
	NoBuild    bool `json:"no_build"`
	ConfigOnly bool `json:"config_only"`
}

// TargetPlan describes one target inside a PlanResult.
type TargetPlan struct {
	Name     string        `json:"name"`
	Host     string        `json:"host"`
	Services []ServicePlan `json:"services"`
	Proxy    string        `json:"proxy,omitempty"`
}

// ServicePlan describes one service inside a target plan.
type ServicePlan struct {
	Name       string `json:"name"`
	Image      string `json:"image"`
	Action     string `json:"action"`      // "build (if changed)", "rebuild", "pull (if missing)", "skip build", "config update"
	DeployMode string `json:"deploy_mode"` // "restart if changed", "rolling (N replicas)", "zero-downtime slot"
	HasHealth  bool   `json:"has_health"`
	HasVolumes bool   `json:"has_volumes"`
	Replicas   int    `json:"replicas,omitempty"`
}

// writeJSON marshals v as indented JSON and writes it to w.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
