package qqd

import (
	"fmt"
	"io"
	"strings"
)

// RiskLevel grades a deploy risk so callers (CLI output, JSON consumers, CI
// gates) can decide what to surface or block on.
type RiskLevel string

const (
	RiskInfo   RiskLevel = "info"   // worth knowing
	RiskWarn   RiskLevel = "warn"   // user should review
	RiskDanger RiskLevel = "danger" // likely to cause downtime or data loss
)

// PlanRisk is one observation about an upcoming deploy.
type PlanRisk struct {
	Level   RiskLevel `json:"level"`
	Service string    `json:"service,omitempty"` // empty = project- or target-scoped
	Target  string    `json:"target,omitempty"`
	Code    string    `json:"code"` // stable identifier for matchers/filters
	Message string    `json:"message"`
}

// detectPlanRisks scans a resolved target's effective config and the project
// settings, returning every notable risk the deploy will expose. The output is
// stable-ordered (target, then service, then code) so JSON callers can diff it.
func detectPlanRisks(cfg ProjectConfig, eff EffectiveTarget, mode planMode) []PlanRisk {
	var risks []PlanRisk

	target := eff.Target.Name
	proxyName := strings.ToLower(cfg.Proxy)
	if proxyName == "" {
		proxyName = "traefik"
	}
	runtime := strings.ToLower(cfg.Runtime)
	if runtime == "" {
		runtime = "podman"
	}

	lifecycleSetting := strings.ToLower(strings.TrimSpace(eff.Target.Lifecycle))
	if lifecycleSetting == "direct" {
		risks = append(risks, PlanRisk{
			Level:   RiskInfo,
			Target:  target,
			Code:    "direct-lifecycle",
			Message: "lifecycle=direct: containers run with podman --restart=always. No systemd. Survives reboot via Podman restart policy, not via ordered systemd dependencies.",
		})
	}

	for _, svcName := range sortedKeys(eff.Services) {
		svc := eff.Services[svcName]

		if isMutableImageTag(svc.Image) {
			risks = append(risks, PlanRisk{
				Level:   RiskWarn,
				Target:  target,
				Service: svcName,
				Code:    "mutable-image-tag",
				Message: fmt.Sprintf("image tag %q is mutable; rollback to a previous release may pull a different image than was originally deployed", tagOrEmpty(svc.Image)),
			})
		}

		exposed := isServiceHTTPExposed(svcName, eff.Expose)
		hasHealth := svc.Health.Path != "" && svc.Health.Port > 0
		if exposed && !hasHealth {
			risks = append(risks, PlanRisk{
				Level:   RiskWarn,
				Target:  target,
				Service: svcName,
				Code:    "exposed-no-health",
				Message: "exposed via proxy but has no health check; rolling/blue-green deploys cannot wait for readiness and will cut traffic to a starting container",
			})
		}

		// Direct restart (no expose, no replicas) means a service window of unavailability.
		if !exposed && !isReplicated(svc) && !isServiceTCPExposed(svcName, eff.Expose) {
			risks = append(risks, PlanRisk{
				Level:   RiskInfo,
				Target:  target,
				Service: svcName,
				Code:    "direct-restart",
				Message: "no expose and no replicas: restart strategy is direct (brief unavailability)",
			})
		}

		// Stateful-shape detection: a service with volume mounts is a candidate for
		// "be careful what you do to me on rollback / migration."
		if len(svc.Volumes) > 0 {
			risks = append(risks, PlanRisk{
				Level:   RiskInfo,
				Target:  target,
				Service: svcName,
				Code:    "has-volumes",
				Message: "service has volume mounts: rollback restores the image but NOT the data inside the volume",
			})
		}

		// TCP passthrough on Caddy is silently broken (HTTP-only reverse_proxy).
		if proxyName == "caddy" && isServiceTCPExposed(svcName, eff.Expose) {
			risks = append(risks, PlanRisk{
				Level:   RiskDanger,
				Target:  target,
				Service: svcName,
				Code:    "caddy-tcp-passthrough",
				Message: "service is TCP-exposed but proxy is Caddy; Caddy's built-in reverse_proxy is HTTP-only. Use proxy: traefik for non-HTTP services. See docs/proxy-caddy.md.",
			})
		}
	}

	if mode.ConfigOnly {
		risks = append(risks, PlanRisk{
			Level:   RiskInfo,
			Target:  target,
			Code:    "config-only",
			Message: "--config-only: source sync and image build are skipped. Container restart still happens for services whose unit content changed.",
		})
	}
	if mode.NoBuild {
		risks = append(risks, PlanRisk{
			Level:   RiskInfo,
			Target:  target,
			Code:    "no-build",
			Message: "--no-build: services with a Dockerfile will not be rebuilt. They will redeploy using whatever image already exists on the target.",
		})
	}

	return risks
}

// isMutableImageTag returns true if the image tag is one of the well-known
// "moves over time" tags. Untagged (no `:`) is also treated as mutable, since
// it implies `:latest`.
func isMutableImageTag(image string) bool {
	if image == "" {
		return false
	}
	_, tag, ok := splitImageTag(image)
	if !ok {
		return true // no tag = ":latest" implicit
	}
	switch strings.ToLower(tag) {
	case "latest", "main", "master", "edge", "stable", "develop", "dev":
		return true
	}
	return false
}

// tagOrEmpty returns the tag portion of an image reference, or "" if untagged.
func tagOrEmpty(image string) string {
	_, tag, _ := splitImageTag(image)
	return tag
}

// isServiceTCPExposed mirrors isServiceHTTPExposed but for TCP passthrough
// entries (single-target ExposeEntry with Target set).
func isServiceTCPExposed(serviceName string, expose ExposeConfig) bool {
	for _, e := range expose.Entries {
		if e.Target == "" {
			continue
		}
		svc, _, _ := parseTarget(e.Target)
		if svc == serviceName {
			return true
		}
	}
	return false
}

// writeDeployPlanJSON renders the plan as JSON. The text variant
// (printDeployPlan) is for humans; this is for CI gates and other tooling.
func writeDeployPlanJSON(w io.Writer, cfg ProjectConfig, targetName string, cliServices []string, mode planMode) error {
	runtime := strings.ToLower(cfg.Runtime)
	if runtime == "" {
		runtime = "podman"
	}
	syncMode := strings.ToLower(cfg.Sync)
	if syncMode == "" {
		syncMode = "git"
	}

	result := PlanResult{
		Project: cfg.Name,
		Runtime: runtime,
		Proxy:   cfg.Proxy,
		Sync:    syncMode,
		Mode: PlanModeJSON{
			Rebuild:    mode.Rebuild,
			NoBuild:    mode.NoBuild,
			ConfigOnly: mode.ConfigOnly,
		},
		Targets: []TargetPlan{},
		Risks:   []PlanRisk{},
	}

	for _, name := range targetOrder(cfg, targetName) {
		eff, err := resolveTarget(cfg, name, cliServices)
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		tp := TargetPlan{
			Name: name,
			Host: eff.Target.Host,
		}
		if hasExposedServices(eff.Expose) {
			tp.Proxy = strings.ToLower(cfg.Proxy)
			if tp.Proxy == "" {
				tp.Proxy = "traefik"
			}
		}
		for _, svcName := range sortedKeys(eff.Services) {
			svc := eff.Services[svcName]
			tp.Services = append(tp.Services, ServicePlan{
				Name:       svcName,
				Image:      svc.Image,
				Action:     planAction(svc, mode),
				DeployMode: planDeployMode(svcName, svc, eff.Expose),
				HasHealth:  svc.Health.Path != "" && svc.Health.Port > 0,
				HasVolumes: len(svc.Volumes) > 0,
				Replicas:   effectiveReplicas(svc),
			})
		}
		result.Targets = append(result.Targets, tp)
		result.Risks = append(result.Risks, detectPlanRisks(cfg, eff, mode)...)
	}

	return writeJSON(w, result)
}

// planAction is the human-readable label for "what will the deploy actually do
// for this service." Centralized so the JSON and text outputs agree.
func planAction(svc ServiceConfig, mode planMode) string {
	switch {
	case mode.ConfigOnly:
		return "config update"
	case mode.NoBuild && svc.Dockerfile != "":
		return "skip build"
	case svc.Dockerfile != "":
		if mode.Rebuild {
			return "rebuild"
		}
		return "build (if changed)"
	default:
		return "pull (if missing)"
	}
}

// planDeployMode is the human-readable restart strategy.
func planDeployMode(svcName string, svc ServiceConfig, expose ExposeConfig) string {
	if isReplicated(svc) {
		return fmt.Sprintf("rolling (%d replicas)", effectiveReplicas(svc))
	}
	if isServiceHTTPExposed(svcName, expose) {
		return "zero-downtime slot"
	}
	return "restart if changed"
}

// formatRiskLine returns the human-readable line for one risk.
func formatRiskLine(r PlanRisk) string {
	var label string
	switch r.Level {
	case RiskDanger:
		label = boldRed("DANGER")
	case RiskWarn:
		label = yellow("warn  ")
	default:
		label = dim("info  ")
	}
	scope := ""
	switch {
	case r.Service != "" && r.Target != "":
		scope = fmt.Sprintf("%s/%s: ", r.Target, r.Service)
	case r.Target != "":
		scope = fmt.Sprintf("%s: ", r.Target)
	case r.Service != "":
		scope = fmt.Sprintf("%s: ", r.Service)
	}
	return fmt.Sprintf("    %s  %s%s", label, scope, r.Message)
}
