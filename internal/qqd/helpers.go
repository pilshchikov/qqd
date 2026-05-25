package qqd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

func containerName(project, service string) string {
	return fmt.Sprintf("%s-%s", project, service)
}

func containerUnit(project, service string) string {
	return fmt.Sprintf("%s-%s.service", project, service)
}

func networkUnit(project string) string {
	return fmt.Sprintf("%s-network.service", project)
}

func allContainerUnits(project string, services map[string]ServiceConfig) []string {
	var out []string
	for _, name := range sortedKeys(services) {
		svc := services[name]
		out = append(out, serviceUnits(project, name, svc)...)
	}
	return out
}

func serviceUnits(project, service string, svc ServiceConfig) []string {
	if isReplicated(svc) {
		units := make([]string, 0, effectiveReplicas(svc))
		for i := 1; i <= effectiveReplicas(svc); i++ {
			units = append(units, fmt.Sprintf("%s-%s-%d.service", project, service, i))
		}
		return units
	}
	return []string{containerUnit(project, service)}
}

func containerUnits(project string, services map[string]ServiceConfig) []string {
	out := make([]string, 0, len(services))
	for _, name := range sortedKeys(services) {
		out = append(out, containerUnit(project, name))
	}
	return out
}

// serviceNameFromQuadlet extracts the service name from a unit filename.
// Returns "" for network files and proxy containers.
func serviceNameFromQuadlet(project, filename string) string {
	var name string
	switch {
	case strings.HasSuffix(filename, ".container"):
		name = strings.TrimSuffix(filename, ".container")
	case strings.HasSuffix(filename, ".service"):
		name = strings.TrimSuffix(filename, ".service")
	default:
		return ""
	}
	name = strings.TrimPrefix(name, project+"-")
	if name == "proxy" || name == "network" {
		return ""
	}
	// Strip replica suffix (-1, -2, etc.)
	if idx := strings.LastIndex(name, "-"); idx >= 0 {
		suffix := name[idx+1:]
		if _, err := strconv.Atoi(suffix); err == nil {
			name = name[:idx]
		}
	}
	return name
}

// joinQuoted shell-quotes and joins values for command construction.
func joinQuoted(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, shellQuote(v))
	}
	return strings.Join(quoted, " ")
}

func fileExistsRemote(ctx context.Context, exec Executor, path string) bool {
	// Don't use shellQuote here: paths containing ~ must remain unquoted for tilde expansion.
	// These paths are generated internally and are safe from injection.
	_, err := exec.Run(ctx, fmt.Sprintf("test -f %s", path))
	return err == nil
}

// atomicWriteRemote writes content to path atomically via a temp file + rename.
// This prevents file watchers (like Traefik's) from seeing a truncated file.
func atomicWriteRemote(ctx context.Context, exec Executor, path, content string) error {
	tmp := path + ".tmp"
	heredoc := fmt.Sprintf("cat > %s <<'QD_EOF'\n%sQD_EOF\nmv %s %s", tmp, content, tmp, path)
	_, err := exec.Run(ctx, heredoc)
	return err
}

// removeStaleQuadlets stops and removes project container quadlet files that are
// no longer needed. This handles cases like replica count changes or switching
// between replicated and non-replicated mode. Only files for services included
// in the current deployment are considered — files for other services are left
// untouched so that partial deploys (e.g. "qqd deploy server") don't break
// unrelated services. When fullDeploy is true (no service names on the CLI),
// files for services no longer in the config are also removed, since a full
// deploy means the config is the complete source of truth.
// Slot files (hash-based) for services in
// slottedSvcs are managed by slotDeploy and are left untouched.
func (a *App) removeStaleQuadlets(ctx context.Context, project, qdDir string, newFiles []QuadletFile, services map[string]ServiceConfig, exec Executor, fullDeploy bool, slottedSvcs map[string]bool) {
	newNames := map[string]bool{}
	for _, f := range newFiles {
		newNames[f.Name] = true
	}
	out, err := exec.Run(ctx, fmt.Sprintf("ls -1 %s 2>/dev/null || true", qdDir))
	if err != nil {
		return
	}
	prefix := project + "-"
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		ext := a.rt().UnitExtension()
		if name == "" || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ext) {
			continue
		}
		if newNames[name] {
			continue
		}
		// Check if this is a slot file — skip if the base service is managed by slot-based deploy
		if isSlotFile(project, name, ext, slottedSvcs) {
			continue
		}
		svcName := serviceNameFromQuadlet(project, name)
		// Skip the standard-named file for slotted services — slotDeploy manages the transition.
		// Only skip the exact standard name (e.g., proj-server.container), not replica files
		// (e.g., proj-server-1.container) which should still be removed when switching from replicated.
		if svcName != "" && slottedSvcs[svcName] && name == fmt.Sprintf("%s-%s%s", project, svcName, ext) {
			continue
		}
		if svcName == "" {
			continue // proxy or unrecognized — don't touch
		}
		if _, ok := services[svcName]; !ok && !fullDeploy {
			continue // service not in this deploy run — leave it alone
		}
		unit := strings.TrimSuffix(name, ext) + ".service"
		fmt.Fprintf(a.Stdout, "  %s stale unit %s\n", yellow("removing"), dim(unit))
		exec.Run(ctx, fmt.Sprintf(a.sctl()+" stop %s 2>/dev/null || true", shellQuote(unit)))
		exec.Run(ctx, fmt.Sprintf("rm -f %s/%s", qdDir, shellQuote(name)))
	}
}

// contains reports whether needle exists in values.
func contains(values []string, needle string) bool {
	for _, v := range values {
		if v == needle {
			return true
		}
	}
	return false
}

// mergeUnique merges two string slices, removing duplicates.
func mergeUnique(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
