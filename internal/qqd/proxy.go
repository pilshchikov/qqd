package qqd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// ProxyProvider generates and manages reverse proxy configuration.
// Implement this interface to add support for a different proxy (e.g. Nginx, Caddy, HAProxy).
type ProxyProvider interface {
	// GenerateStaticConfig returns the static proxy configuration content.
	GenerateStaticConfig(project string, expose ExposeConfig) string

	// GenerateDynamicConfig returns the dynamic routing configuration.
	// opts controls slot overrides (zero-downtime deploys) and replica exclusion (rolling drain).
	GenerateDynamicConfig(project string, services map[string]ServiceConfig, expose ExposeConfig, opts DynamicConfigOpts) string

	// RenderContainerQuadlet returns the Quadlet container file for the proxy (Podman).
	RenderContainerQuadlet(project string, services map[string]ServiceConfig, expose ExposeConfig, skipDeps ...map[string]bool) QuadletFile

	// StaticConfigPath returns the full remote path to the static config file.
	StaticConfigPath(project string) string

	// DynamicConfigDir returns the remote directory path for dynamic config.
	DynamicConfigDir(project string) string

	// DynamicConfigPath returns the full remote path to the dynamic routes file.
	DynamicConfigPath(project string) string

	// ContainerName returns the proxy container name.
	ContainerName(project string) string

	// ServiceUnit returns the systemd service unit name for the proxy.
	ServiceUnit(project string) string
}

// TraefikProvider implements ProxyProvider using Traefik v3.6.
type TraefikProvider struct {
	Image string // custom image override (empty = default "docker.io/library/traefik:v3.6")
}

func (TraefikProvider) GenerateStaticConfig(project string, expose ExposeConfig) string {
	return generateTraefikStatic(project, expose)
}

func (TraefikProvider) GenerateDynamicConfig(project string, services map[string]ServiceConfig, expose ExposeConfig, opts DynamicConfigOpts) string {
	return generateTraefikDynamicOpts(project, services, expose, opts)
}

func (p TraefikProvider) RenderContainerQuadlet(project string, services map[string]ServiceConfig, expose ExposeConfig, skipDeps ...map[string]bool) QuadletFile {
	return renderProxyContainer(project, p.image(), services, expose, skipDeps...)
}

func (p TraefikProvider) image() string {
	if p.Image != "" {
		return p.Image
	}
	return "docker.io/library/traefik:v3.6"
}

func (TraefikProvider) StaticConfigPath(project string) string {
	return fmt.Sprintf("~/.config/qqd/%s/traefik.yml", project)
}

func (TraefikProvider) DynamicConfigDir(project string) string {
	return fmt.Sprintf("~/.config/qqd/%s/dynamic", project)
}

func (TraefikProvider) DynamicConfigPath(project string) string {
	return fmt.Sprintf("~/.config/qqd/%s/dynamic/routes.yml", project)
}

func (TraefikProvider) ContainerName(project string) string {
	return fmt.Sprintf("%s-proxy", project)
}

func (TraefikProvider) ServiceUnit(project string) string {
	return fmt.Sprintf("%s-proxy.service", project)
}

// proxyProviderByName returns a ProxyProvider for the given name and optional custom image.
// Defaults to TraefikProvider when name is empty.
func proxyProviderByName(name, image string) ProxyProvider {
	switch strings.ToLower(name) {
	case "caddy":
		return CaddyProvider{Image: image}
	default:
		return TraefikProvider{Image: image}
	}
}

// filterExposeByServices returns an ExposeConfig with only entries that route
// to services in the activeServices set.
func filterExposeByServices(expose ExposeConfig, activeServices map[string]bool) ExposeConfig {
	var filtered []ExposeEntry
	for _, e := range expose.Entries {
		if e.Target != "" {
			// TCP entry
			svc, _, _ := parseTarget(e.Target)
			if activeServices[svc] {
				filtered = append(filtered, e)
			}
		} else {
			// HTTP entry - keep only routes to active services
			routes := map[string]string{}
			for path, target := range e.Routes {
				svc, _, _ := parseTarget(target)
				if activeServices[svc] {
					routes[path] = target
				}
			}
			if len(routes) > 0 {
				entry := e
				entry.Routes = routes
				filtered = append(filtered, entry)
			}
		}
	}
	return ExposeConfig{Entries: filtered, Dashboard: expose.Dashboard}
}

// detectRunningServices checks which services from allServices have a running container on the target.
func detectRunningServices(ctx context.Context, exec Executor, project string, allServices map[string]ServiceConfig, cmd string) map[string]bool {
	out, err := exec.Run(ctx, fmt.Sprintf("%s ps --format '{{.Names}}' 2>/dev/null || true", cmd))
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	running := map[string]bool{}
	prefix := project + "-"
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		svcPart := strings.TrimPrefix(name, prefix)
		// Strip slot hash or replica number
		for svcName := range allServices {
			if svcPart == svcName || strings.HasPrefix(svcPart, svcName+"-") {
				running[svcName] = true
			}
		}
	}
	return running
}

// hasExposedServices reports whether any expose entries exist.
func hasExposedServices(expose ExposeConfig) bool {
	return len(expose.Entries) > 0
}

// isServiceExposed reports whether a service appears in any expose entry.
func isServiceExposed(serviceName string, expose ExposeConfig) bool {
	for _, e := range expose.Entries {
		if e.Target != "" {
			svc, _, _ := parseTarget(e.Target)
			if svc == serviceName {
				return true
			}
		} else {
			for _, target := range e.Routes {
				svc, _, _ := parseTarget(target)
				if svc == serviceName {
					return true
				}
			}
		}
	}
	return false
}

// isServiceHTTPExposed reports whether a service appears in HTTP routes (not TCP passthrough).
// Only HTTP-routed services benefit from zero-downtime slot deployment (atomic Traefik route switching).
// TCP passthrough services (databases, metrics, etc.) don't benefit and can't safely use slot-based deploys.
func isServiceHTTPExposed(serviceName string, expose ExposeConfig) bool {
	for _, e := range expose.Entries {
		if e.Target != "" {
			continue // TCP passthrough — skip
		}
		for _, target := range e.Routes {
			svc, _, _ := parseTarget(target)
			if svc == serviceName {
				return true
			}
		}
	}
	return false
}

// DynamicConfigOpts provides fine-grained control over Traefik dynamic config generation.
type DynamicConfigOpts struct {
	SlotOverrides   map[string]string       // service name → container DNS name (slot-based deploy)
	ExcludeReplicas map[string]map[int]bool // service name → replica indices to exclude (rolling drain)
}

// traefikAPIPort returns the internal port for the Traefik API/dashboard listener.
// It picks a port that doesn't collide with any user-defined entrypoint.
func traefikAPIPort(expose ExposeConfig) int {
	used := map[int]bool{}
	for _, e := range expose.Entries {
		used[e.HostPort] = true
		if e.TLS != nil && e.TLS.Port > 0 {
			used[e.TLS.Port] = true
		}
	}
	// Prefer Traefik's default 8080; fall back to 19090 if taken.
	if !used[8080] {
		return 8080
	}
	return 19090
}

// generateTraefikStatic builds the static traefik.yml config with entrypoints and file provider.
func generateTraefikStatic(project string, expose ExposeConfig) string {
	var b strings.Builder
	if expose.Dashboard > 0 {
		b.WriteString("api:\n")
		b.WriteString("  dashboard: true\n")
		b.WriteString("  insecure: true\n")
	}
	b.WriteString("entryPoints:\n")
	if expose.Dashboard > 0 {
		apiPort := traefikAPIPort(expose)
		b.WriteString("  traefik:\n")
		b.WriteString(fmt.Sprintf("    address: \":%d\"\n", apiPort))
	}
	for _, e := range expose.Entries {
		name := entrypointName(e)
		b.WriteString(fmt.Sprintf("  %s:\n", name))
		b.WriteString(fmt.Sprintf("    address: \":%d\"\n", e.HostPort))
		if e.TLS != nil && e.TLS.Port > 0 {
			tlsName := fmt.Sprintf("tls-%d", e.TLS.Port)
			b.WriteString(fmt.Sprintf("  %s:\n", tlsName))
			b.WriteString(fmt.Sprintf("    address: \":%d\"\n", e.TLS.Port))
			b.WriteString("    http:\n")
			b.WriteString("      tls: {}\n")
		}
	}
	b.WriteString("providers:\n")
	b.WriteString("  file:\n")
	b.WriteString("    directory: /etc/traefik/dynamic\n")
	b.WriteString("    watch: true\n")
	return b.String()
}

// generateTraefikDynamic builds the dynamic routes.yml config with HTTP/TCP routers and services.
func generateTraefikDynamic(project string, services map[string]ServiceConfig, expose ExposeConfig) string {
	return generateTraefikDynamicOpts(project, services, expose, DynamicConfigOpts{})
}

// generateTraefikDynamicOpts builds dynamic routes.yml with fine-grained control over
// container DNS names (for slot-based deploy overrides) and replica exclusion (for rolling drain).
func generateTraefikDynamicOpts(project string, services map[string]ServiceConfig, expose ExposeConfig, opts DynamicConfigOpts) string {
	var httpRouters, httpServices, tcpRouters, tcpServices []string

	for _, e := range expose.Entries {
		if e.Target != "" {
			// TCP entry
			svc, port, _ := parseTarget(e.Target)
			routerName := sanitizeTraefikName(fmt.Sprintf("%s-%s-%d", project, svc, port))
			ep := entrypointName(e)

			tcpRouters = append(tcpRouters, formatTCPRouter(routerName, ep, svc, port))
			tcpServices = append(tcpServices, formatTCPServiceOpts(routerName, project, svc, port, services, opts))
		} else {
			// HTTP entry
			ep := entrypointName(e)
			for _, path := range sortedKeys(e.Routes) {
				target := e.Routes[path]
				svc, port, _ := parseTarget(target)
				routerName := sanitizeTraefikName(fmt.Sprintf("%s-%s-%d", project, svc, port))
				// Add path suffix to make router names unique when multiple paths route to same service
				routerKey := sanitizeTraefikName(fmt.Sprintf("%s-%s-%d-%s", project, svc, port, path))

				priority := len(path)
				httpRouters = append(httpRouters, formatHTTPRouter(routerKey, ep, path, routerName, priority))
				httpServices = append(httpServices, formatHTTPServiceOpts(routerName, project, svc, port, services, opts))
			}

			// TLS routers
			if e.TLS != nil && e.TLS.CertsDir != "" && e.TLS.ServerName != "" {
				tlsEP := fmt.Sprintf("tls-%d", e.TLS.Port)
				if e.TLS.Port == 0 {
					tlsEP = "tls-443"
				}
				for _, path := range sortedKeys(e.Routes) {
					target := e.Routes[path]
					svc, port, _ := parseTarget(target)
					routerName := sanitizeTraefikName(fmt.Sprintf("%s-%s-%d", project, svc, port))
					routerKey := sanitizeTraefikName(fmt.Sprintf("%s-%s-%d-%s-tls", project, svc, port, path))

					priority := len(path)
					httpRouters = append(httpRouters, formatHTTPTLSRouter(routerKey, tlsEP, path, routerName, priority, e.TLS))
				}

				// HTTP → HTTPS redirect router
				redirectKey := sanitizeTraefikName(fmt.Sprintf("%s-redirect-%d", project, e.HostPort))
				httpRouters = append(httpRouters, formatHTTPRedirectRouter(redirectKey, ep, e.TLS.hostNames()))
				httpServices = append(httpServices, formatRedirectService(redirectKey))
			}
		}
	}

	var b strings.Builder
	if len(httpRouters) > 0 || len(httpServices) > 0 {
		b.WriteString("http:\n")
		if len(httpRouters) > 0 {
			b.WriteString("  routers:\n")
			// Deduplicate routers
			seen := map[string]bool{}
			for _, r := range httpRouters {
				if !seen[r] {
					seen[r] = true
					b.WriteString(r)
				}
			}
		}
		if len(httpServices) > 0 {
			b.WriteString("  services:\n")
			seen := map[string]bool{}
			for _, s := range httpServices {
				if !seen[s] {
					seen[s] = true
					b.WriteString(s)
				}
			}
		}
	}
	// Add redirect middleware if any TLS entries exist
	hasTLS := false
	for _, e := range expose.Entries {
		if e.TLS != nil && e.TLS.CertsDir != "" && e.TLS.ServerName != "" {
			hasTLS = true
			break
		}
	}
	if hasTLS && (len(httpRouters) > 0 || len(httpServices) > 0) {
		b.WriteString("  middlewares:\n")
		b.WriteString("    redirect-to-https:\n")
		b.WriteString("      redirectScheme:\n")
		b.WriteString("        scheme: https\n")
		b.WriteString("        permanent: true\n")
	}

	if len(tcpRouters) > 0 || len(tcpServices) > 0 {
		b.WriteString("tcp:\n")
		if len(tcpRouters) > 0 {
			b.WriteString("  routers:\n")
			seen := map[string]bool{}
			for _, r := range tcpRouters {
				if !seen[r] {
					seen[r] = true
					b.WriteString(r)
				}
			}
		}
		if len(tcpServices) > 0 {
			b.WriteString("  services:\n")
			seen := map[string]bool{}
			for _, s := range tcpServices {
				if !seen[s] {
					seen[s] = true
					b.WriteString(s)
				}
			}
		}
	}

	// TLS certificate configuration — map certs_dir/server_name to Traefik
	// file-based TLS stores so HTTPS actually works with the documented layout.
	tlsSeen := map[string]bool{}
	var tlsCerts []string
	for _, e := range expose.Entries {
		if e.TLS != nil && e.TLS.CertsDir != "" && e.TLS.ServerName != "" {
			key := e.TLS.ServerName
			if tlsSeen[key] {
				continue
			}
			tlsSeen[key] = true
			mountPath := traefikTLSMountPath(e.TLS.CertsDir)
			tlsCerts = append(tlsCerts, fmt.Sprintf(
				"    - certFile: %s/live/%s/fullchain.pem\n      keyFile: %s/live/%s/privkey.pem\n",
				mountPath, e.TLS.ServerName, mountPath, e.TLS.ServerName))
		}
	}
	if len(tlsCerts) > 0 {
		b.WriteString("tls:\n")
		b.WriteString("  certificates:\n")
		for _, c := range tlsCerts {
			b.WriteString(c)
		}
	}

	return b.String()
}

// tlsCertFiles returns the host-side certificate chains referenced by an expose
// config, deduplicated. Only fullchain.pem is listed: it is world-readable (so no
// sudo is needed to read it) and always rewritten alongside privkey.pem, which
// makes it a faithful stand-in for the whole certificate.
func tlsCertFiles(expose ExposeConfig) []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range expose.Entries {
		if e.TLS == nil || e.TLS.CertsDir == "" || e.TLS.ServerName == "" {
			continue
		}
		p := fmt.Sprintf("%s/live/%s/fullchain.pem", e.TLS.CertsDir, e.TLS.ServerName)
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// tlsFingerprintPath is where the last deployed certificate fingerprint is kept
// on the target, next to the proxy's generated config.
func tlsFingerprintPath(project string) string {
	return fmt.Sprintf("~/.config/qqd/%s/tls-fingerprint", project)
}

// traefikTLSMountPath returns a deterministic in-container mount path for a certs_dir.
func traefikTLSMountPath(certsDir string) string {
	h := sha256.Sum256([]byte(certsDir))
	return "/etc/certs/" + hex.EncodeToString(h[:4])
}

// renderProxyContainer generates the <project>-proxy.container quadlet file for Traefik.
// skipDeps services are excluded from systemd After/Requires (used for slot-based services
// whose lifecycle is managed separately by slotDeploy).
func renderProxyContainer(project, image string, services map[string]ServiceConfig, expose ExposeConfig, skipDeps ...map[string]bool) QuadletFile {
	var skip map[string]bool
	if len(skipDeps) > 0 {
		skip = skipDeps[0]
	}
	var b strings.Builder

	// Depend on all exposed services (except slot-based ones).
	// Use Wants= (not Requires=) so the proxy survives backend restarts during
	// rolling restarts and slot-based deploys without being stopped by systemd.
	deps := exposedServiceDeps(project, services, expose, skip)
	if len(deps) > 0 {
		b.WriteString("[Unit]\n")
		b.WriteString("After=" + strings.Join(deps, " ") + "\n")
		b.WriteString("Wants=" + strings.Join(deps, " ") + "\n\n")
	}

	b.WriteString("[Container]\n")
	b.WriteString(fmt.Sprintf("ContainerName=%s-proxy\n", project))
	b.WriteString(fmt.Sprintf("Image=%s\n", image))
	b.WriteString(fmt.Sprintf("Network=%s.network\n", project))

	// Publish all host ports from expose entries
	publishedPorts := map[int]bool{}
	if expose.Dashboard > 0 {
		apiPort := traefikAPIPort(expose)
		b.WriteString(fmt.Sprintf("PublishPort=%d:%d\n", expose.Dashboard, apiPort))
		publishedPorts[expose.Dashboard] = true
	}
	for _, e := range expose.Entries {
		if !publishedPorts[e.HostPort] {
			b.WriteString(fmt.Sprintf("PublishPort=%d:%d\n", e.HostPort, e.HostPort))
			publishedPorts[e.HostPort] = true
		}
		if e.TLS != nil && e.TLS.Port > 0 && !publishedPorts[e.TLS.Port] {
			b.WriteString(fmt.Sprintf("PublishPort=%d:%d\n", e.TLS.Port, e.TLS.Port))
			publishedPorts[e.TLS.Port] = true
		}
	}

	// TLS cert volume mounts
	mountedTLSDirs := map[string]bool{}
	for _, e := range expose.Entries {
		if e.TLS != nil && e.TLS.CertsDir != "" && !mountedTLSDirs[e.TLS.CertsDir] {
			mountedTLSDirs[e.TLS.CertsDir] = true
			b.WriteString(fmt.Sprintf("Volume=%s:%s:ro\n", e.TLS.CertsDir, traefikTLSMountPath(e.TLS.CertsDir)))
		}
	}

	b.WriteString(fmt.Sprintf("Volume=%%h/.config/qqd/%s/traefik.yml:/etc/traefik/traefik.yml:ro,z\n", project))
	b.WriteString(fmt.Sprintf("Volume=%%h/.config/qqd/%s/dynamic:/etc/traefik/dynamic:ro,z\n", project))

	b.WriteString("\n[Service]\n")
	b.WriteString("Restart=always\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")

	return QuadletFile{
		Name:    fmt.Sprintf("%s-proxy.container", project),
		Content: b.String(),
	}
}

// exposedServiceDeps extracts systemd service dependencies from expose entries.
// Services in skip are excluded (used for slot-based services managed separately).
func exposedServiceDeps(project string, services map[string]ServiceConfig, expose ExposeConfig, skip ...map[string]bool) []string {
	var skipSet map[string]bool
	if len(skip) > 0 {
		skipSet = skip[0]
	}
	seen := map[string]bool{}
	var deps []string

	addDep := func(svcName string) {
		if skipSet != nil && skipSet[svcName] {
			return
		}
		svc, ok := services[svcName]
		if !ok {
			return
		}
		if isReplicated(svc) {
			for i := 1; i <= effectiveReplicas(svc); i++ {
				dep := fmt.Sprintf("%s-%s-%d.service", project, svcName, i)
				if !seen[dep] {
					seen[dep] = true
					deps = append(deps, dep)
				}
			}
		} else {
			dep := fmt.Sprintf("%s-%s.service", project, svcName)
			if !seen[dep] {
				seen[dep] = true
				deps = append(deps, dep)
			}
		}
	}

	for _, e := range expose.Entries {
		if e.Target != "" {
			svc, _, _ := parseTarget(e.Target)
			addDep(svc)
		} else {
			for _, target := range e.Routes {
				svc, _, _ := parseTarget(target)
				addDep(svc)
			}
		}
	}

	sort.Strings(deps)
	return deps
}

// entrypointName returns the Traefik entrypoint name for an expose entry.
func entrypointName(e ExposeEntry) string {
	if e.Target != "" {
		return fmt.Sprintf("tcp-%d", e.HostPort)
	}
	return fmt.Sprintf("web-%d", e.HostPort)
}

// sanitizeTraefikName replaces characters that aren't valid in Traefik router/service names.
func sanitizeTraefikName(name string) string {
	r := strings.NewReplacer("/", "", ".", "-", ":", "-", " ", "-")
	s := r.Replace(name)
	// Collapse multiple dashes
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}

// formatHTTPRouter returns a YAML fragment for one HTTP router.
func formatHTTPRouter(routerKey, entrypoint, path, serviceName string, priority int) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("    %s:\n", routerKey))
	b.WriteString(fmt.Sprintf("      entryPoints:\n        - %s\n", entrypoint))
	b.WriteString(fmt.Sprintf("      rule: \"PathPrefix(`%s`)\"\n", path))
	b.WriteString(fmt.Sprintf("      service: %s\n", serviceName))
	b.WriteString(fmt.Sprintf("      priority: %d\n", priority))
	return b.String()
}

// formatHTTPTLSRouter returns a YAML fragment for one HTTPS router with TLS termination.
func formatHTTPTLSRouter(routerKey, entrypoint, path, serviceName string, priority int, tls *TLSConfig) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("    %s:\n", routerKey))
	b.WriteString(fmt.Sprintf("      entryPoints:\n        - %s\n", entrypoint))
	b.WriteString(fmt.Sprintf("      rule: \"PathPrefix(`%s`)\"\n", path))
	b.WriteString(fmt.Sprintf("      service: %s\n", serviceName))
	b.WriteString(fmt.Sprintf("      priority: %d\n", priority))
	b.WriteString("      tls:\n")
	return b.String()
}

// formatHTTPRedirectRouter returns a YAML fragment for HTTP→HTTPS redirect.
// Every hostname the certificate covers gets redirected, not just the primary.
func formatHTTPRedirectRouter(routerKey, entrypoint string, serverNames []string) string {
	var b strings.Builder
	clauses := make([]string, 0, len(serverNames))
	for _, n := range serverNames {
		clauses = append(clauses, fmt.Sprintf("HostRegexp(`%s`)", n))
	}
	b.WriteString(fmt.Sprintf("    %s:\n", routerKey))
	b.WriteString(fmt.Sprintf("      entryPoints:\n        - %s\n", entrypoint))
	b.WriteString(fmt.Sprintf("      rule: \"%s\"\n", strings.Join(clauses, " || ")))
	b.WriteString(fmt.Sprintf("      service: %s\n", routerKey))
	b.WriteString("      middlewares:\n        - redirect-to-https\n")
	b.WriteString("      priority: 1\n")
	return b.String()
}

// formatRedirectService returns a noop service placeholder for redirect routers.
func formatRedirectService(name string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("    %s:\n", name))
	b.WriteString("      loadBalancer:\n")
	b.WriteString("        servers: []\n")
	return b.String()
}

// formatHTTPService returns a YAML fragment for one HTTP service (load balancer with servers).
func formatHTTPService(serviceName, project, svcName string, port int, services map[string]ServiceConfig) string {
	return formatHTTPServiceOpts(serviceName, project, svcName, port, services, DynamicConfigOpts{})
}

// formatHTTPServiceOpts returns a YAML fragment for one HTTP service with opts support.
// SlotOverrides replaces the container DNS name for non-replicated services.
// ExcludeReplicas skips specified replica indices for replicated services.
func formatHTTPServiceOpts(serviceName, project, svcName string, port int, services map[string]ServiceConfig, opts DynamicConfigOpts) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("    %s:\n", serviceName))
	b.WriteString("      loadBalancer:\n")
	b.WriteString("        servers:\n")
	svc, ok := services[svcName]
	if ok && isReplicated(svc) {
		excluded := opts.ExcludeReplicas[svcName]
		for i := 1; i <= effectiveReplicas(svc); i++ {
			if excluded != nil && excluded[i] {
				continue
			}
			b.WriteString(fmt.Sprintf("          - url: \"http://%s-%s-%d:%d\"\n", project, svcName, i, port))
		}
	} else {
		dnsName := containerDNSName(project, svcName, opts)
		b.WriteString(fmt.Sprintf("          - url: \"http://%s:%d\"\n", dnsName, port))
	}
	return b.String()
}

// formatTCPRouter returns a YAML fragment for one TCP router.
func formatTCPRouter(routerName, entrypoint, svcName string, port int) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("    %s:\n", routerName))
	b.WriteString(fmt.Sprintf("      entryPoints:\n        - %s\n", entrypoint))
	b.WriteString("      rule: \"HostSNI(`*`)\"\n")
	b.WriteString(fmt.Sprintf("      service: %s\n", routerName))
	return b.String()
}

// formatTCPService returns a YAML fragment for one TCP service (load balancer with addresses).
func formatTCPService(serviceName, project, svcName string, port int, services map[string]ServiceConfig) string {
	return formatTCPServiceOpts(serviceName, project, svcName, port, services, DynamicConfigOpts{})
}

// formatTCPServiceOpts returns a YAML fragment for one TCP service with opts support.
func formatTCPServiceOpts(serviceName, project, svcName string, port int, services map[string]ServiceConfig, opts DynamicConfigOpts) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("    %s:\n", serviceName))
	b.WriteString("      loadBalancer:\n")
	b.WriteString("        servers:\n")
	svc, ok := services[svcName]
	if ok && isReplicated(svc) {
		excluded := opts.ExcludeReplicas[svcName]
		for i := 1; i <= effectiveReplicas(svc); i++ {
			if excluded != nil && excluded[i] {
				continue
			}
			b.WriteString(fmt.Sprintf("          - address: \"%s-%s-%d:%d\"\n", project, svcName, i, port))
		}
	} else {
		dnsName := containerDNSName(project, svcName, opts)
		b.WriteString(fmt.Sprintf("          - address: \"%s:%d\"\n", dnsName, port))
	}
	return b.String()
}

// containerDNSName returns the container DNS name for a non-replicated service,
// using slot overrides if present (for zero-downtime slot deployments).
func containerDNSName(project, svcName string, opts DynamicConfigOpts) string {
	if opts.SlotOverrides != nil {
		if override, ok := opts.SlotOverrides[svcName]; ok {
			return override
		}
	}
	return fmt.Sprintf("%s-%s", project, svcName)
}
