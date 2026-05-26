package qqd

import (
	"fmt"
	"strings"
)

// QuadletFile is one generated Quadlet unit file.
type QuadletFile struct {
	Name    string
	Content string
}

// renderQuadletFiles renders network, container, and proxy units for a project.
// deployServices controls which service containers are generated (may be a subset during partial deploys).
// allServices is the full service set for the target, used for proxy generation so that
// routes remain complete even during partial deploys.
// skipProxyDeps services are excluded from the proxy's systemd dependencies (used for
// slot-based services whose lifecycle is managed separately).
func renderQuadletFiles(project string, deployServices, allServices map[string]ServiceConfig, expose ExposeConfig, proxy ProxyProvider, rt ContainerRuntime, targetUser string, skipProxyDeps ...map[string]bool) []QuadletFile {
	if rt == nil {
		rt = PodmanRuntime{}
	}
	files := []QuadletFile{
		{
			Name:    rt.NetworkFileName(project),
			Content: rt.RenderNetwork(project),
		},
	}
	for _, name := range sortedKeys(deployServices) {
		svc := deployServices[name]
		if isReplicated(svc) {
			for i := 1; i <= effectiveReplicas(svc); i++ {
				files = append(files, QuadletFile{
					Name:    rt.ReplicaFileName(project, name, i),
					Content: rt.RenderReplicaContainer(project, name, i, svc),
				})
			}
		} else {
			files = append(files, QuadletFile{
				Name:    rt.ContainerFileName(project, name),
				Content: rt.RenderContainer(project, name, svc),
			})
		}
	}
	if hasExposedServices(expose) {
		var skip map[string]bool
		if len(skipProxyDeps) > 0 {
			skip = skipProxyDeps[0]
		}
		files = append(files, proxy.RenderContainerQuadlet(project, allServices, expose, skip))
	}
	return files
}

// renderNetwork renders the project bridge network definition.
func renderNetwork(project string) string {
	return strings.TrimSpace(fmt.Sprintf(`
[Network]
NetworkName=%s
Driver=bridge
`, project)) + "\n"
}

// renderContainer renders one service .container unit (non-replicated).
func renderContainer(project, service string, cfg ServiceConfig) string {
	var b strings.Builder
	if len(cfg.DependsOn) > 0 {
		b.WriteString("[Unit]\n")
		after := make([]string, 0, len(cfg.DependsOn))
		for _, dep := range cfg.DependsOn {
			after = append(after, fmt.Sprintf("%s-%s.service", project, dep))
		}
		b.WriteString("After=" + strings.Join(after, " ") + "\n")
		b.WriteString("Requires=" + strings.Join(after, " ") + "\n\n")
	}
	b.WriteString("[Container]\n")
	b.WriteString(fmt.Sprintf("ContainerName=%s-%s\n", project, service))
	b.WriteString(fmt.Sprintf("Image=%s\n", cfg.Image))
	b.WriteString(fmt.Sprintf("Network=%s.network\n", project))
	if cfg.User != "" {
		b.WriteString(fmt.Sprintf("User=%s\n", cfg.User))
	}
	for _, key := range sortedKeys(cfg.Env) {
		b.WriteString(formatQuadletEnv(key, cfg.Env[key]))
	}
	for _, volume := range cfg.Volumes {
		b.WriteString(fmt.Sprintf("Volume=%s\n", ensureVolumeFlags(volume, cfg.volumeNeedsU)))
	}
	if len(cfg.Command) > 0 {
		b.WriteString(fmt.Sprintf("Exec=%s\n", formatExecArgs(cfg.Command)))
	}
	writeHealthCheck(&b, cfg)
	writePodmanArgs(&b, cfg)
	b.WriteString("\n[Service]\n")
	b.WriteString("Restart=always\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// renderContainerWithSlot renders a service .container unit with a slot suffix.
// The container is named {project}-{service}-{slot} and gets a network alias of
// {project}-{service} so that inter-service DNS still resolves to it.
func renderContainerWithSlot(project, service, slot string, cfg ServiceConfig) string {
	var b strings.Builder
	if len(cfg.DependsOn) > 0 {
		b.WriteString("[Unit]\n")
		after := make([]string, 0, len(cfg.DependsOn))
		for _, dep := range cfg.DependsOn {
			after = append(after, fmt.Sprintf("%s-%s.service", project, dep))
		}
		b.WriteString("After=" + strings.Join(after, " ") + "\n")
		b.WriteString("Requires=" + strings.Join(after, " ") + "\n\n")
	}
	b.WriteString("[Container]\n")
	b.WriteString(fmt.Sprintf("ContainerName=%s-%s-%s\n", project, service, slot))
	b.WriteString(fmt.Sprintf("Image=%s\n", cfg.Image))
	b.WriteString(fmt.Sprintf("Network=%s.network:alias=%s-%s\n", project, project, service))
	if cfg.User != "" {
		b.WriteString(fmt.Sprintf("User=%s\n", cfg.User))
	}
	for _, key := range sortedKeys(cfg.Env) {
		b.WriteString(formatQuadletEnv(key, cfg.Env[key]))
	}
	for _, volume := range cfg.Volumes {
		b.WriteString(fmt.Sprintf("Volume=%s\n", ensureVolumeFlags(volume, cfg.volumeNeedsU)))
	}
	if len(cfg.Command) > 0 {
		b.WriteString(fmt.Sprintf("Exec=%s\n", formatExecArgs(cfg.Command)))
	}
	writeHealthCheck(&b, cfg)
	writePodmanArgs(&b, cfg)
	b.WriteString("\n[Service]\n")
	b.WriteString("Restart=always\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// renderReplicaContainer renders one replica .container unit.
// Replica containers have no PublishPort (traffic goes through proxy).
func renderReplicaContainer(project, service string, replica int, cfg ServiceConfig) string {
	var b strings.Builder
	if len(cfg.DependsOn) > 0 {
		b.WriteString("[Unit]\n")
		after := make([]string, 0, len(cfg.DependsOn))
		for _, dep := range cfg.DependsOn {
			after = append(after, fmt.Sprintf("%s-%s.service", project, dep))
		}
		b.WriteString("After=" + strings.Join(after, " ") + "\n")
		b.WriteString("Requires=" + strings.Join(after, " ") + "\n\n")
	}
	b.WriteString("[Container]\n")
	b.WriteString(fmt.Sprintf("ContainerName=%s-%s-%d\n", project, service, replica))
	b.WriteString(fmt.Sprintf("Image=%s\n", cfg.Image))
	b.WriteString(fmt.Sprintf("Network=%s.network\n", project))
	if cfg.User != "" {
		b.WriteString(fmt.Sprintf("User=%s\n", cfg.User))
	}
	for _, key := range sortedKeys(cfg.Env) {
		b.WriteString(formatQuadletEnv(key, cfg.Env[key]))
	}
	for _, volume := range cfg.Volumes {
		b.WriteString(fmt.Sprintf("Volume=%s\n", ensureVolumeFlags(volume, cfg.volumeNeedsU)))
	}
	if len(cfg.Command) > 0 {
		b.WriteString(fmt.Sprintf("Exec=%s\n", formatExecArgs(cfg.Command)))
	}
	writeHealthCheck(&b, cfg)
	writePodmanArgs(&b, cfg)
	b.WriteString("\n[Service]\n")
	b.WriteString("Restart=always\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// writeHealthCheck appends health check directives if configured.
func writeHealthCheck(b *strings.Builder, cfg ServiceConfig) {
	if cfg.Health.Path == "" || cfg.Health.Port == 0 {
		return
	}
	b.WriteString(fmt.Sprintf("HealthCmd=curl -sf http://localhost:%d%s || exit 1\n", cfg.Health.Port, cfg.Health.Path))
	b.WriteString("HealthInterval=10s\n")
	b.WriteString("HealthTimeout=5s\n")
	b.WriteString("HealthRetries=3\n")
	b.WriteString("HealthStartPeriod=30s\n")
}

// writePodmanArgs appends resource limit flags via PodmanArgs if configured.
func writePodmanArgs(b *strings.Builder, cfg ServiceConfig) {
	var args []string
	if cfg.Resources.CPUs != "" {
		args = append(args, fmt.Sprintf("--cpus=%s", cfg.Resources.CPUs))
	}
	if cfg.Resources.Memory != "" {
		args = append(args, fmt.Sprintf("--memory=%s", cfg.Resources.Memory))
	}
	if len(args) > 0 {
		b.WriteString(fmt.Sprintf("PodmanArgs=%s\n", strings.Join(args, " ")))
	}
}

// formatExecArgs formats command arguments for the Quadlet Exec= directive.
// Systemd treats a leading '-' as "ignore errors" prefix, so arguments that
// start with '-' must be quoted to prevent misinterpretation.
func formatExecArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "-") || strings.ContainsAny(a, " \t\"'") {
			quoted[i] = fmt.Sprintf("%q", a)
		} else {
			quoted[i] = a
		}
	}
	return strings.Join(quoted, " ")
}

// formatQuadletEnv returns a complete Environment= directive for a Quadlet file.
// Simple values produce: Environment=KEY=VALUE
// Values with spaces, newlines, quotes, or backslashes use systemd's quoted form:
//
//	Environment="KEY=escaped_value"
//
// Systemd specifiers (%) are always escaped as %%.
func formatQuadletEnv(key, value string) string {
	needsQuoting := strings.ContainsAny(value, " \n\r\t\"\\")

	// Escape systemd specifiers (works in both quoted and unquoted)
	value = strings.ReplaceAll(value, "%", "%%")

	if !needsQuoting {
		return fmt.Sprintf("Environment=%s=%s\n", key, value)
	}

	// Escape for systemd double-quoted string (order matters: backslash first)
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.ReplaceAll(value, "\n", "\\n")
	value = strings.ReplaceAll(value, "\r", "\\r")
	value = strings.ReplaceAll(value, "\t", "\\t")

	return fmt.Sprintf("Environment=\"%s=%s\"\n", key, value)
}

// effectiveReplicas returns the number of replicas for a service (minimum 1).
func effectiveReplicas(svc ServiceConfig) int {
	if svc.Replicas < 1 {
		return 1
	}
	return svc.Replicas
}

// isReplicated reports whether a service uses replica containers.
func isReplicated(svc ServiceConfig) bool {
	return svc.Replicas > 1
}
