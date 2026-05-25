package qqd

import (
	"fmt"
	"strings"
)

// isCaddyProxy is true when the project explicitly opts into the Caddy proxy
// provider. Anything else (empty / "traefik" / unknown) is treated as not
// Caddy; the proxyProviderByName fallback decides how to handle unknowns.
func isCaddyProxy(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), "caddy")
}

// ValidateConfig performs semantic validation on a loaded project config.
// It returns a list of diagnostic messages prefixed with "error:" or "warning:".
// An empty slice means the config is valid.
func ValidateConfig(cfg ProjectConfig) []string {
	var msgs []string

	// Check depends_on references exist and detect circular dependencies.
	for _, name := range sortedKeys(cfg.Services) {
		svc := cfg.Services[name]
		for _, dep := range svc.DependsOn {
			if _, ok := cfg.Services[dep]; !ok {
				msgs = append(msgs, fmt.Sprintf("error: service %q depends_on %q which does not exist", name, dep))
			}
		}
	}
	if cycle := detectCycle(cfg.Services); cycle != "" {
		msgs = append(msgs, fmt.Sprintf("error: circular dependency: %s", cycle))
	}

	// Port range validation in expose entries.
	for _, tName := range sortedKeys(cfg.Targets) {
		t := cfg.Targets[tName]
		for _, e := range t.Expose.Entries {
			if e.HostPort < 1 || e.HostPort > 65535 {
				msgs = append(msgs, fmt.Sprintf("error: target %q expose port %d out of range 1-65535", tName, e.HostPort))
			}
			if e.Target != "" {
				_, port, err := parseTarget(e.Target)
				if err == nil && (port < 1 || port > 65535) {
					msgs = append(msgs, fmt.Sprintf("error: target %q expose TCP target port %d out of range 1-65535", tName, port))
				}
			}
			for path, target := range e.Routes {
				_, port, err := parseTarget(target)
				if err == nil && (port < 1 || port > 65535) {
					msgs = append(msgs, fmt.Sprintf("error: target %q expose port %d route %q target port %d out of range 1-65535", tName, e.HostPort, path, port))
				}
			}
		}

		// TLS completeness (already caught at parse time, but validate catches
		// cases where TLS port is set but certs_dir/server_name are missing).
		for _, e := range t.Expose.Entries {
			if e.TLS != nil {
				if e.TLS.CertsDir == "" && e.TLS.ServerName == "" && e.TLS.Port > 0 {
					msgs = append(msgs, fmt.Sprintf("error: target %q expose port %d: tls.port is set but certs_dir and server_name are missing", tName, e.HostPort))
				}
			}
		}
	}

	// Health check port inference: warn if health.path is set but port is 0 and
	// the service has no HTTP expose routes to infer from.
	for _, tName := range sortedKeys(cfg.Targets) {
		t := cfg.Targets[tName]
		for _, sName := range sortedKeys(cfg.Services) {
			svc := cfg.Services[sName]
			if svc.Health.Path != "" && svc.Health.Port == 0 {
				_, err := inferHealthPort(sName, t.Expose)
				if err != nil {
					msgs = append(msgs, fmt.Sprintf("error: target %q service %q: health.path is set but port cannot be inferred (%s)", tName, sName, err))
				}
			}
		}
	}

	// Replicated services should not have depends_on.
	for _, name := range sortedKeys(cfg.Services) {
		svc := cfg.Services[name]
		if isReplicated(svc) && len(svc.DependsOn) > 0 {
			msgs = append(msgs, fmt.Sprintf("warning: service %q is replicated (replicas=%d) but has depends_on; systemd dependency on replicas may not behave as expected", name, svc.Replicas))
		}
	}

	// Warn if no targets defined (already caught at parse time, but be defensive).
	if len(cfg.Targets) == 0 {
		msgs = append(msgs, "warning: no targets defined")
	}

	// Warn about mutable image tags.
	for _, name := range sortedKeys(cfg.Services) {
		svc := cfg.Services[name]
		if isMutableTag(svc.Image) {
			msgs = append(msgs, fmt.Sprintf("warning: service %q uses mutable image tag %q; consider pinning to an immutable version", name, svc.Image))
		}
	}

	// Hard-fail Caddy + raw TCP passthrough. The default Caddy image only
	// supports HTTP via reverse_proxy; a TCP-style expose entry will silently
	// produce a Caddyfile that can't carry the protocol the user expects.
	// This used to be a `plan` danger only, but a deployment tool should not
	// ship a known-broken config — see docs/proxy-caddy.md.
	if isCaddyProxy(cfg.Proxy) {
		for _, tName := range sortedKeys(cfg.Targets) {
			t := cfg.Targets[tName]
			for _, e := range t.Expose.Entries {
				if e.Target == "" {
					continue
				}
				msgs = append(msgs, fmt.Sprintf(
					"error: target %q port %d is configured as raw TCP passthrough (target %q) but proxy is Caddy; "+
						"Caddy's built-in reverse_proxy is HTTP-only. "+
						"Use proxy: traefik, or change this entry to an HTTP route. See docs/proxy-caddy.md.",
					tName, e.HostPort, e.Target))
			}
		}
	}

	// Build strategy validation.
	validateBuildStrategy(&msgs, "global", cfg.Build)
	for _, tName := range sortedKeys(cfg.Targets) {
		t := cfg.Targets[tName]
		merged := mergeBuild(cfg.Build, t.Build)
		if merged.Strategy != "" {
			validateBuildStrategy(&msgs, fmt.Sprintf("target %q", tName), merged)
		}
	}

	return msgs
}

// validateBuildStrategy checks that required fields are present for a given build strategy.
func validateBuildStrategy(msgs *[]string, scope string, b BuildConfig) {
	switch b.Strategy {
	case "build-host":
		if b.Host == "" {
			*msgs = append(*msgs, fmt.Sprintf("error: %s build strategy \"build-host\" requires host", scope))
		}
		if b.User == "" {
			*msgs = append(*msgs, fmt.Sprintf("error: %s build strategy \"build-host\" requires user", scope))
		}
		if b.RepoDir == "" {
			*msgs = append(*msgs, fmt.Sprintf("error: %s build strategy \"build-host\" requires repo_dir", scope))
		}
	case "github-actions":
		if b.Repo == "" {
			*msgs = append(*msgs, fmt.Sprintf("error: %s build strategy \"github-actions\" requires repo", scope))
		}
		if b.Workflow == "" {
			*msgs = append(*msgs, fmt.Sprintf("error: %s build strategy \"github-actions\" requires workflow", scope))
		}
	}
}

// detectCycle checks for circular dependencies in the service dependency graph.
// Returns a human-readable cycle description or empty string if no cycle exists.
func detectCycle(services map[string]ServiceConfig) string {
	const (
		unvisited = 0
		visiting  = 1
		visited   = 2
	)
	state := map[string]int{}
	parent := map[string]string{}

	for _, name := range sortedKeys(services) {
		state[name] = unvisited
	}

	var dfs func(name string) string
	dfs = func(name string) string {
		state[name] = visiting
		svc, ok := services[name]
		if !ok {
			state[name] = visited
			return ""
		}
		for _, dep := range svc.DependsOn {
			if _, exists := services[dep]; !exists {
				continue // missing dep is reported separately
			}
			if state[dep] == visiting {
				// Found a cycle — reconstruct it.
				cycle := dep
				cur := name
				for cur != dep {
					cycle = cur + " -> " + cycle
					cur = parent[cur]
				}
				cycle = dep + " -> " + cycle
				return cycle
			}
			if state[dep] == unvisited {
				parent[dep] = name
				if result := dfs(dep); result != "" {
					return result
				}
			}
		}
		state[name] = visited
		return ""
	}

	for _, name := range sortedKeys(services) {
		if state[name] == unvisited {
			if result := dfs(name); result != "" {
				return result
			}
		}
	}
	return ""
}
