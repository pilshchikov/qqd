package qqd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// CaddyProvider implements ProxyProvider using Caddy v2.
//
// Configuration model (intentionally Caddyfile-only):
//
//   - The container is started with the default Caddy entrypoint, which reads
//     /etc/caddy/Caddyfile.
//   - qqd writes the generated Caddyfile to
//     ~/.config/qqd/<project>/caddy-routes/Caddyfile and bind-mounts it into
//     the container.
//   - There is no separate static JSON config: the listen ports, TLS, and
//     routes are all expressed in the Caddyfile. GenerateStaticConfig returns
//     "" and the deploy layer skips writing empty static configs.
//   - Reload happens via systemctl restart of the proxy unit, NOT the Caddy
//     admin API. The admin API is not enabled or exposed by qqd.
//
// Limitations vs Traefik (tracked in docs/proxy-caddy.md):
//   - Raw TCP passthrough is not supported. Caddy's built-in reverse_proxy is
//     HTTP-only; TCP passthrough requires the layer4 plugin which the default
//     image does not include. TCP-style "bare port" entries in the qqd config
//     are emitted as HTTP reverse_proxy and will only work for HTTP services.
type CaddyProvider struct {
	Image string // custom image override (empty = default "docker.io/library/caddy:2-alpine")
}

// GenerateStaticConfig returns "" because Caddy reads the bind-mounted
// Caddyfile directly. The interface still requires the method; the deploy
// layer treats an empty result as "no static file to write."
func (CaddyProvider) GenerateStaticConfig(project string, expose ExposeConfig) string {
	return ""
}

func (CaddyProvider) GenerateDynamicConfig(project string, services map[string]ServiceConfig, expose ExposeConfig, opts DynamicConfigOpts) string {
	return generateCaddyDynamic(project, services, expose, opts)
}

func (p CaddyProvider) RenderContainerQuadlet(project string, services map[string]ServiceConfig, expose ExposeConfig, skipDeps ...map[string]bool) QuadletFile {
	return renderCaddyContainer(project, p.image(), services, expose, skipDeps...)
}

func (p CaddyProvider) image() string {
	if p.Image != "" {
		return p.Image
	}
	return "docker.io/library/caddy:2-alpine"
}

func (CaddyProvider) StaticConfigPath(project string) string {
	return fmt.Sprintf("~/.config/qqd/%s/caddy.json", project)
}

func (CaddyProvider) DynamicConfigDir(project string) string {
	return fmt.Sprintf("~/.config/qqd/%s/caddy-routes", project)
}

// DynamicConfigPath returns the bind-mounted Caddyfile path. The file is named
// "Caddyfile" (not "routes.json") because the content is Caddyfile-format, not
// JSON.
func (CaddyProvider) DynamicConfigPath(project string) string {
	return fmt.Sprintf("~/.config/qqd/%s/caddy-routes/Caddyfile", project)
}

func (CaddyProvider) ContainerName(project string) string {
	return fmt.Sprintf("%s-proxy", project)
}

func (CaddyProvider) ServiceUnit(project string) string {
	return fmt.Sprintf("%s-proxy.service", project)
}

// generateCaddyDynamic builds the Caddyfile that Caddy reads at startup.
// Listen ports, routes, and TLS are all declared here. Reload happens via
// systemctl restart of the proxy unit (the admin API is not enabled).
func generateCaddyDynamic(project string, services map[string]ServiceConfig, expose ExposeConfig, opts DynamicConfigOpts) string {
	var b strings.Builder

	for _, e := range expose.Entries {
		if e.Target != "" {
			// TCP passthrough — Caddy handles this via layer4 or simple reverse_proxy
			svc, port, _ := parseTarget(e.Target)
			dnsName := containerDNSName(project, svc, opts)
			upstream := buildCaddyUpstream(project, svc, port, services, opts)
			if upstream == "" {
				upstream = fmt.Sprintf("%s:%d", dnsName, port)
			}
			b.WriteString(fmt.Sprintf(":%d {\n", e.HostPort))
			b.WriteString(fmt.Sprintf("  reverse_proxy %s\n", upstream))
			b.WriteString("}\n\n")
		} else {
			// HTTP entry with path routes
			b.WriteString(fmt.Sprintf(":%d {\n", e.HostPort))

			// Sort paths by length descending (longer paths first = higher priority)
			paths := sortedKeys(e.Routes)
			sortPathsByLength(paths)

			for _, path := range paths {
				target := e.Routes[path]
				svc, port, _ := parseTarget(target)
				upstream := buildCaddyUpstream(project, svc, port, services, opts)

				if path == "/" {
					b.WriteString(fmt.Sprintf("  reverse_proxy %s\n", upstream))
				} else {
					b.WriteString(fmt.Sprintf("  handle_path %s* {\n", path))
					b.WriteString(fmt.Sprintf("    reverse_proxy %s\n", upstream))
					b.WriteString("  }\n")
				}
			}

			// TLS configuration
			if e.TLS != nil && e.TLS.CertsDir != "" && e.TLS.ServerName != "" {
				mountPath := caddyTLSMountPath(e.TLS.CertsDir)
				certPath := fmt.Sprintf("%s/live/%s/fullchain.pem", mountPath, e.TLS.ServerName)
				keyPath := fmt.Sprintf("%s/live/%s/privkey.pem", mountPath, e.TLS.ServerName)

				b.WriteString("  tls " + certPath + " " + keyPath + "\n")
			}

			b.WriteString("}\n\n")

			// TLS server block (HTTPS port)
			if e.TLS != nil && e.TLS.Port > 0 && e.TLS.CertsDir != "" && e.TLS.ServerName != "" {
				mountPath := caddyTLSMountPath(e.TLS.CertsDir)
				certPath := fmt.Sprintf("%s/live/%s/fullchain.pem", mountPath, e.TLS.ServerName)
				keyPath := fmt.Sprintf("%s/live/%s/privkey.pem", mountPath, e.TLS.ServerName)

				b.WriteString(fmt.Sprintf(":%d {\n", e.TLS.Port))
				b.WriteString("  tls " + certPath + " " + keyPath + "\n")

				for _, path := range paths {
					target := e.Routes[path]
					svc, port, _ := parseTarget(target)
					upstream := buildCaddyUpstream(project, svc, port, services, opts)

					if path == "/" {
						b.WriteString(fmt.Sprintf("  reverse_proxy %s\n", upstream))
					} else {
						b.WriteString(fmt.Sprintf("  handle_path %s* {\n", path))
						b.WriteString(fmt.Sprintf("    reverse_proxy %s\n", upstream))
						b.WriteString("  }\n")
					}
				}

				b.WriteString("}\n\n")
			}
		}
	}

	return b.String()
}

// buildCaddyUpstream returns space-separated upstream addresses for a service.
func buildCaddyUpstream(project, svcName string, port int, services map[string]ServiceConfig, opts DynamicConfigOpts) string {
	svc, ok := services[svcName]
	if ok && isReplicated(svc) {
		var upstreams []string
		excluded := opts.ExcludeReplicas[svcName]
		for i := 1; i <= effectiveReplicas(svc); i++ {
			if excluded != nil && excluded[i] {
				continue
			}
			upstreams = append(upstreams, fmt.Sprintf("%s-%s-%d:%d", project, svcName, i, port))
		}
		return strings.Join(upstreams, " ")
	}
	dnsName := containerDNSName(project, svcName, opts)
	return fmt.Sprintf("%s:%d", dnsName, port)
}

// caddyTLSMountPath returns a deterministic in-container mount path for a certs_dir.
func caddyTLSMountPath(certsDir string) string {
	h := sha256.Sum256([]byte(certsDir))
	return "/etc/certs/" + hex.EncodeToString(h[:4])
}

// renderCaddyContainer generates the <project>-proxy.container quadlet file for Caddy.
func renderCaddyContainer(project, image string, services map[string]ServiceConfig, expose ExposeConfig, skipDeps ...map[string]bool) QuadletFile {
	var skip map[string]bool
	if len(skipDeps) > 0 {
		skip = skipDeps[0]
	}
	var b strings.Builder

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

	// Publish all host ports
	publishedPorts := map[int]bool{}
	if expose.Dashboard > 0 {
		b.WriteString(fmt.Sprintf("PublishPort=%d:%d\n", expose.Dashboard, expose.Dashboard))
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
			b.WriteString(fmt.Sprintf("Volume=%s:%s:ro,z\n", e.TLS.CertsDir, caddyTLSMountPath(e.TLS.CertsDir)))
		}
	}

	// Mount the generated Caddyfile (listen ports + routes + TLS).
	b.WriteString(fmt.Sprintf("Volume=%%h/.config/qqd/%s/caddy-routes/Caddyfile:/etc/caddy/Caddyfile:ro,z\n", project))

	b.WriteString("\n[Service]\n")
	b.WriteString("Restart=always\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")

	return QuadletFile{
		Name:    fmt.Sprintf("%s-proxy.container", project),
		Content: b.String(),
	}
}

// sortPathsByLength sorts paths by length descending (longer paths get higher priority).
func sortPathsByLength(paths []string) {
	for i := 1; i < len(paths); i++ {
		for j := i; j > 0 && len(paths[j]) > len(paths[j-1]); j-- {
			paths[j], paths[j-1] = paths[j-1], paths[j]
		}
	}
}
