package qqd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// statusEntry holds the label, systemd unit name, and container name for one service.
type statusEntry struct{ label, unit, container string }

// collectStatusEntries builds the list of status entries for a target.
func collectStatusEntries(cfg ProjectConfig, eff EffectiveTarget, qdListing, unitExt string, proxy ProxyProvider) []statusEntry {
	var entries []statusEntry
	for _, svcName := range sortedKeys(eff.Services) {
		svc := eff.Services[svcName]
		if isReplicated(svc) {
			for i := 1; i <= effectiveReplicas(svc); i++ {
				entries = append(entries, statusEntry{
					label:     fmt.Sprintf("%s/%d", svcName, i),
					unit:      fmt.Sprintf("%s-%s-%d.service", cfg.Name, svcName, i),
					container: fmt.Sprintf("%s-%s-%d", cfg.Name, svcName, i),
				})
			}
		} else {
			slot := detectActiveSlotFromListing(cfg.Name, svcName, qdListing, unitExt)
			if slot != "" {
				entries = append(entries, statusEntry{
					label:     svcName,
					unit:      fmt.Sprintf("%s-%s-%s.service", cfg.Name, svcName, slot),
					container: fmt.Sprintf("%s-%s-%s", cfg.Name, svcName, slot),
				})
			} else {
				entries = append(entries, statusEntry{
					label:     svcName,
					unit:      containerUnit(cfg.Name, svcName),
					container: containerName(cfg.Name, svcName),
				})
			}
		}
	}
	if hasExposedServices(eff.Expose) {
		entries = append(entries, statusEntry{
			label:     "proxy",
			unit:      proxy.ServiceUnit(cfg.Name),
			container: proxy.ContainerName(cfg.Name),
		})
	}
	return entries
}

// queryStatusEntries runs a batched SSH command to get state and inspect info for all entries.
// Returns parallel slices of (state, inspect) strings.
func queryStatusEntries(ctx context.Context, exec Executor, entries []statusEntry, rt ContainerRuntime) ([]string, []string, error) {
	sctl := "systemctl --user"
	crt := "podman"
	if rt != nil {
		sctl = rt.SystemctlPrefix()
		crt = rt.Cmd()
	}
	imageField := "{{.ImageName}}"
	var cmdParts []string
	for _, e := range entries {
		cmdParts = append(cmdParts, fmt.Sprintf(
			`printf '%%s|%%s\n' "$(%s is-active %s 2>/dev/null || echo inactive)" "$(%s inspect --format '%s {{.State.StartedAt}}' %s 2>/dev/null || echo -)"`,
			sctl, shellQuote(e.unit), crt, imageField, shellQuote(e.container),
		))
	}
	out, err := exec.Run(ctx, strings.Join(cmdParts, "; "))
	if err != nil {
		return nil, nil, err
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	states := make([]string, len(entries))
	inspects := make([]string, len(entries))
	for i := range entries {
		states[i] = "unknown"
		if i < len(lines) {
			parts := strings.SplitN(lines[i], "|", 2)
			states[i] = strings.TrimSpace(parts[0])
			if len(parts) > 1 {
				inspects[i] = strings.TrimSpace(parts[1])
			}
		}
		if inspects[i] == "-" {
			inspects[i] = ""
		}
	}
	return states, inspects, nil
}

// parseInspectFields splits raw podman inspect output into image and timestamp.
func parseInspectFields(raw string) (image, startedAt, uptime string) {
	parts := strings.SplitN(raw, " ", 2)
	if len(parts) < 2 {
		return raw, "", ""
	}
	image = parts[0]
	ts := parts[1]
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", ts)
		if err != nil {
			return image, ts, ""
		}
	}
	startedAt = t.UTC().Format("2006-01-02 15:04:05 MST")
	uptime = humanDuration(time.Since(t))
	return image, startedAt, uptime
}

// Status prints service status information for one or all targets.
// It batches SSH calls: one ls for slot detection, then a single command
// that queries is-active + podman inspect for every service at once.
// When a.OutputFormat is "json", it outputs structured JSON instead of colored text.
func (a *App) Status(ctx context.Context, cfg ProjectConfig, targetName string) error {
	a.applyConfig(cfg)
	jsonMode := a.OutputFormat == "json"
	if !jsonMode {
		InitColor(a.Stdout)
	}

	var result StatusResult
	targets := targetOrder(cfg, targetName)
	for _, name := range targets {
		eff, err := resolveTarget(cfg, name, nil)
		if err != nil {
			return err
		}
		exec, err := a.ExecFactory.ForTarget(eff.Target)
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		defer exec.Close()

		// Decide backend before issuing systemd-flavored probes.
		sel := a.lifecycleFor(ctx, eff.Target, exec)
		if !jsonMode {
			fmt.Fprintf(a.Stdout, "target=%s host=%s\n", name, eff.Target.Host)
		}
		if sel.Backend == "direct" {
			if err := a.statusDirect(ctx, cfg, eff, exec, sel.Selected, jsonMode, &result, name); err != nil {
				return fmt.Errorf("target %s: %w", name, err)
			}
			continue
		}

		// Single SSH call: connectivity check + slot detection
		qdDir := a.rt().UnitDir()
		sp := startSpinner(a.Stdout, fmt.Sprintf("connecting to %s (%s)", name, eff.Target.Host))
		qdListing, err := exec.Run(ctx, fmt.Sprintf("ls -1 %s 2>/dev/null || true", qdDir))
		sp.stop()
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}

		entries := collectStatusEntries(cfg, eff, qdListing, a.rt().UnitExtension(), a.proxy())
		if len(entries) == 0 {
			if jsonMode {
				result.Targets = append(result.Targets, TargetStatus{
					Name: name, Host: eff.Target.Host,
				})
			}
			continue
		}

		sp = startSpinner(a.Stdout, fmt.Sprintf("checking %d services on %s", len(entries), name))
		states, inspects, err := queryStatusEntries(ctx, exec, entries, a.rt())
		sp.stop()
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}

		if jsonMode {
			ts := TargetStatus{Name: name, Host: eff.Target.Host}
			for i, e := range entries {
				ss := ServiceStatus{Name: e.label, State: states[i]}
				if inspects[i] != "" {
					ss.Image, ss.StartedAt, ss.Uptime = parseInspectFields(inspects[i])
				}
				ts.Services = append(ts.Services, ss)
			}
			result.Targets = append(result.Targets, ts)
		} else {
			for i, e := range entries {
				coloredState := red(states[i])
				if states[i] == "active" {
					coloredState = green(states[i])
				}
				fmt.Fprintf(a.Stdout, "  %s: %s %s\n", bold(e.label), coloredState, dim(formatInspectOutput(inspects[i])))
			}

			// Detect other containers not managed by this project
			a.showOtherContainers(ctx, exec, cfg.Name)

			// Check port conflicts
			a.checkPortConflicts(ctx, exec, eff.Expose, cfg.Name)
		}
	}

	if jsonMode {
		return writeJSON(a.Stdout, result)
	}
	return nil
}

// showOtherContainers detects containers on the host not managed by this qqd project.
func (a *App) showOtherContainers(ctx context.Context, exec Executor, project string) {
	cmd := a.crt()
	out, err := exec.Run(ctx, fmt.Sprintf("%s ps --format '{{.Names}}' 2>/dev/null", cmd))
	if err != nil || strings.TrimSpace(out) == "" {
		return
	}
	prefix := project + "-"
	var others []string
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		if name == "" || strings.HasPrefix(name, prefix) {
			continue
		}
		others = append(others, name)
	}
	if len(others) > 0 {
		fmt.Fprintf(a.Stdout, "\n  %s other containers on host:\n", yellow("warning:"))
		for _, name := range others {
			fmt.Fprintf(a.Stdout, "    %s\n", dim(name))
		}
	}
}

// checkPortConflicts detects if qqd's exposed ports are already in use on the host.
// It checks container port mappings from both runtimes and system listeners.
func (a *App) checkPortConflicts(ctx context.Context, exec Executor, expose ExposeConfig, project string) {
	if !hasExposedServices(expose) {
		return
	}
	// Collect ports this project wants to expose
	wantPorts := map[int]string{} // port -> "service:containerPort"
	for _, e := range expose.Entries {
		if e.Target != "" {
			wantPorts[e.HostPort] = e.Target
		} else {
			for _, target := range e.Routes {
				wantPorts[e.HostPort] = target
				break // one entry per host port is enough for the warning
			}
		}
		if e.TLS != nil && e.TLS.Port > 0 {
			wantPorts[e.TLS.Port] = "tls"
		}
	}
	if len(wantPorts) == 0 {
		return
	}

	// Get all Podman container port mappings.
	// Format: "0.0.0.0:80->80/tcp" or "80/tcp -> 0.0.0.0:80"
	var conflicts []string
	seen := map[int]bool{}

	checkRuntimePorts := func(cmd string) {
		out, err := exec.Run(ctx, fmt.Sprintf("%s ps --format '{{.Names}}|{{.Ports}}' 2>/dev/null || true", cmd))
		if err != nil || strings.TrimSpace(out) == "" {
			return
		}
		prefix := project + "-"
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "|", 2)
			cname := parts[0]
			if strings.HasPrefix(cname, prefix) {
				continue // our own project's container
			}
			if len(parts) < 2 || parts[1] == "" {
				continue
			}
			// Parse port mappings like "0.0.0.0:80->80/tcp, 0.0.0.0:5432->5432/tcp"
			for _, mapping := range strings.Split(parts[1], ", ") {
				mapping = strings.TrimSpace(mapping)
				// Extract host port from "0.0.0.0:PORT->..." or "[::]:PORT->..."
				arrowIdx := strings.Index(mapping, "->")
				if arrowIdx < 0 {
					continue
				}
				hostPart := mapping[:arrowIdx]
				colonIdx := strings.LastIndex(hostPart, ":")
				if colonIdx < 0 {
					continue
				}
				var hostPort int
				if _, err := fmt.Sscanf(hostPart[colonIdx+1:], "%d", &hostPort); err != nil {
					continue
				}
				if _, want := wantPorts[hostPort]; want && !seen[hostPort] {
					seen[hostPort] = true
					conflicts = append(conflicts, fmt.Sprintf(
						"port %d needed by this project, but already used by container %s (%s)",
						hostPort, bold(cname), dim(cmd)))
				}
			}
		}
	}

	checkRuntimePorts("podman")

	// Also check system listeners (non-container processes like nginx, sshd)
	ssOut, err := exec.Run(ctx, "ss -tlnp 2>/dev/null || true")
	if err == nil && strings.TrimSpace(ssOut) != "" {
		for _, line := range strings.Split(ssOut, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "LISTEN") {
				continue
			}
			// Skip container-managed ports.
			if strings.Contains(line, "rootlessport") {
				continue
			}
			fields := strings.Fields(line)
			for _, f := range fields {
				if !strings.Contains(f, ":") {
					continue
				}
				parts := strings.Split(f, ":")
				portStr := parts[len(parts)-1]
				var port int
				if _, err := fmt.Sscanf(portStr, "%d", &port); err == nil {
					if _, want := wantPorts[port]; want && !seen[port] {
						seen[port] = true
						// Extract process info
						procInfo := ""
						for _, field := range fields {
							if strings.Contains(field, "users:") {
								procInfo = field
								break
							}
						}
						conflicts = append(conflicts, fmt.Sprintf(
							"port %d needed by this project, but already in use %s",
							port, dim(procInfo)))
					}
				}
			}
		}
	}

	if len(conflicts) > 0 {
		fmt.Fprintf(a.Stdout, "\n  %s port conflicts:\n", boldRed("warning:"))
		for _, c := range conflicts {
			fmt.Fprintf(a.Stdout, "    %s\n", c)
		}
	}
}

// formatInspectOutput reformats "image timestamp" from podman inspect into
// a human-readable form like "image (2026-02-26 20:21:37 UTC, up 3h 12m)".
func formatInspectOutput(raw string) string {
	parts := strings.SplitN(raw, " ", 2)
	if len(parts) < 2 {
		return raw
	}
	image, ts := parts[0], parts[1]
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		// Try the Go default format that older podman versions may emit
		t, err = time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", ts)
		if err != nil {
			return raw
		}
	}
	return fmt.Sprintf("%s (%s, up %s)", image, t.UTC().Format("2006-01-02 15:04:05 MST"), humanDuration(time.Since(t)))
}

// humanDuration formats a duration as a compact human-readable string.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd %dh", days, h)
}

// Logs streams recent container logs via podman logs.
// When services is empty, logs all services. For replicated services, all
// replicas are streamed. Each line is prefixed with the container name.
func (a *App) Logs(ctx context.Context, cfg ProjectConfig, targetName string, services []string) error {
	eff, err := resolveTarget(cfg, targetName, services)
	if err != nil {
		return err
	}
	exec, err := a.ExecFactory.ForTarget(eff.Target)
	if err != nil {
		return err
	}
	defer exec.Close()
	qdDir := a.rt().UnitDir()

	// Collect all container names to tail.
	var containers []string
	for _, svcName := range sortedKeys(eff.Services) {
		svc := eff.Services[svcName]
		containers = append(containers, resolveContainerNames(ctx, exec, cfg.Name, svcName, svc, qdDir, a.rt().UnitExtension(), a.sctl())...)
	}
	if len(containers) == 0 {
		return errors.New("no containers to show logs for")
	}

	// Single container — stream directly without prefix.
	if len(containers) == 1 {
		return exec.RunStream(ctx, fmt.Sprintf(a.crt()+" logs --tail 200 -f %s", shellQuote(containers[0])), a.Stdout)
	}

	// Multiple containers — stream in parallel, prefixing each line.
	// Find longest container name for alignment.
	maxLen := 0
	for _, c := range containers {
		if len(c) > maxLen {
			maxLen = len(c)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, len(containers))
	for _, c := range containers {
		c := c
		go func() {
			pw := &prefixWriter{w: a.Stdout, prefix: fmt.Sprintf("%-*s | ", maxLen, c), bol: true}
			errCh <- exec.RunStream(ctx, fmt.Sprintf(a.crt()+" logs --tail 200 -f %s", shellQuote(c)), pw)
		}()
	}
	// Wait for first error (e.g. container not found) or all finish.
	for range containers {
		if err := <-errCh; err != nil {
			cancel()
			return err
		}
	}
	return nil
}

// prefixWriter wraps an io.Writer and prepends a prefix at the start of each line.
type prefixWriter struct {
	w      io.Writer
	prefix string
	bol    bool // true when next write starts a new line
}

func (pw *prefixWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		if pw.bol {
			fmt.Fprint(pw.w, pw.prefix)
			pw.bol = false
		}
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			pw.w.Write(p)
			break
		}
		pw.w.Write(p[:idx+1])
		p = p[idx+1:]
		pw.bol = true
	}
	return total, nil
}

// resolveContainerNames returns all runtime container names for a service,
// including all replicas for replicated services.
func resolveContainerNames(ctx context.Context, exec Executor, project, service string, svc ServiceConfig, qdDir, unitExt, sctl string) []string {
	if isReplicated(svc) {
		names := make([]string, 0, effectiveReplicas(svc))
		for i := 1; i <= effectiveReplicas(svc); i++ {
			names = append(names, fmt.Sprintf("%s-%s-%d", project, service, i))
		}
		return names
	}
	slot := detectActiveSlot(ctx, exec, project, service, qdDir, unitExt, sctl)
	if slot != "" {
		return []string{fmt.Sprintf("%s-%s-%s", project, service, slot)}
	}
	return []string{containerName(project, service)}
}

// Rollback restores the previous release on one or all targets.
// When service is non-empty, only that service is rolled back.
// When service is empty, all services are rolled back.
func (a *App) Rollback(ctx context.Context, cfg ProjectConfig, targetName, service string) error {
	a.applyConfig(cfg)
	InitColor(a.Stdout)
	targets := targetOrder(cfg, targetName)
	for _, name := range targets {
		eff, err := resolveTarget(cfg, name, nil)
		if err != nil {
			return err
		}
		exec, err := a.ExecFactory.ForTarget(eff.Target)
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		defer exec.Close()

		err = a.withTargetLock(ctx, exec, cfg.Name, name, "rollback", func() error {
			prev, err := previousRelease(ctx, exec, cfg.Name)
			if err != nil {
				return err
			}

			fmt.Fprintf(a.Stdout, "%s rolling back to release %s on %s\n", boldCyan("[rollback]"), bold(prev.ID), bold(name))

			var changedServices []string
			if service != "" {
				prevImage, ok := prev.Services[service]
				if !ok {
					return fmt.Errorf("service %s not found in release %s", service, prev.ID)
				}
				svc, ok := eff.Services[service]
				if !ok {
					return fmt.Errorf("service %s not found in current config", service)
				}
				if svc.Image == prevImage {
					fmt.Fprintf(a.Stdout, "  %s already on %s\n", bold(service), dim(prevImage))
					return nil
				}
				fmt.Fprintf(a.Stdout, "  %s %s → %s\n", bold(service), dim(svc.Image), green(prevImage))
				svc.Image = prevImage
				eff.Services[service] = svc
				changedServices = append(changedServices, service)
			} else {
				// Full rollback
				for svcName, prevImage := range prev.Services {
					svc, ok := eff.Services[svcName]
					if !ok {
						continue
					}
					if svc.Image != prevImage {
						fmt.Fprintf(a.Stdout, "  %s %s → %s\n", bold(svcName), dim(svc.Image), green(prevImage))
						svc.Image = prevImage
						eff.Services[svcName] = svc
						changedServices = append(changedServices, svcName)
					}
				}
			}
			if len(changedServices) == 0 {
				fmt.Fprintf(a.Stdout, "  %s\n", dim("no service changes needed"))
				return nil
			}

			// Ensure the old images exist on target
			sort.Strings(changedServices)
			for _, svcName := range changedServices {
				svc := eff.Services[svcName]
				exists, err := imageExists(ctx, exec, svc.Image, a.rt())
				if err != nil {
					return err
				}
				if !exists {
					sp := startSpinner(a.Stdout, fmt.Sprintf("pulling %s", svc.Image))
					if err := exec.RunStream(ctx, fmt.Sprintf(a.crt()+" pull %s", shellQuote(svc.Image)), a.Stdout); err != nil {
						sp.stop()
						return fmt.Errorf("pull %s: %w", svc.Image, err)
					}
					sp.stop()
				}
			}

			// Reinstall and restart with previous images
			if err := a.installAndStart(ctx, cfg, eff, exec, false, changedServices, true); err != nil {
				return fmt.Errorf("rollback: %w", err)
			}

			// Save rollback as a new release
			rel := newRelease(eff.Services)
			if err := saveRelease(ctx, exec, cfg.Name, rel); err != nil {
				fmt.Fprintf(a.Stdout, "  %s save release: %s\n", yellow("warning"), err)
			}
			trimReleases(ctx, exec, cfg.Name)

			fmt.Fprintf(a.Stdout, "%s target %s rolled back to %s\n", boldGreen("rolled back"), bold(name), bold(prev.ID))
			return nil
		})
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
	}
	return nil
}

// History shows deployment release history for one or all targets.
func (a *App) History(ctx context.Context, cfg ProjectConfig, targetName string) error {
	InitColor(a.Stdout)
	targets := targetOrder(cfg, targetName)
	for _, name := range targets {
		eff, err := resolveTarget(cfg, name, nil)
		if err != nil {
			return err
		}
		exec, err := a.ExecFactory.ForTarget(eff.Target)
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		defer exec.Close()
		releases, err := listReleases(ctx, exec, cfg.Name)
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		fmt.Fprintf(a.Stdout, "target=%s host=%s releases=%d\n", name, eff.Target.Host, len(releases))
		for i, rel := range releases {
			marker := "  "
			if i == 0 {
				marker = dim("→ ") // current
			}
			fmt.Fprintf(a.Stdout, "%s%s  %s  services: %s\n",
				marker,
				bold(rel.ID),
				dim(rel.Timestamp),
				dim(strings.Join(sortedKeys(rel.Services), ", ")),
			)
			for _, svc := range sortedKeys(rel.Services) {
				fmt.Fprintf(a.Stdout, "    %s: %s\n", svc, dim(rel.Services[svc]))
			}
		}
	}
	return nil
}

// Stop stops selected or all service units on one or all targets.
func (a *App) Stop(ctx context.Context, cfg ProjectConfig, targetName string, services []string) error {
	return a.systemdCommand(ctx, cfg, targetName, services, "stop")
}

// Start starts selected or all service units on one or all targets.
func (a *App) Start(ctx context.Context, cfg ProjectConfig, targetName string, services []string) error {
	return a.systemdCommand(ctx, cfg, targetName, services, "start")
}

// Destroy removes generated Quadlet units for one or all targets.
func (a *App) Destroy(ctx context.Context, cfg ProjectConfig, targetName string) error {
	a.applyConfig(cfg)
	InitColor(a.Stdout)
	targets := targetOrder(cfg, targetName)
	for _, name := range targets {
		eff, err := resolveTarget(cfg, name, nil)
		if err != nil {
			return err
		}
		exec, err := a.ExecFactory.ForTarget(eff.Target)
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		defer exec.Close()

		sel := a.lifecycleFor(ctx, eff.Target, exec)
		if sel.Backend == "direct" {
			err = a.withTargetLock(ctx, exec, cfg.Name, name, "destroy", func() error {
				return a.destroyDirect(ctx, cfg, eff, exec, sel.Selected, name)
			})
			if err != nil {
				return fmt.Errorf("target %s: %w", name, err)
			}
			continue
		}
		err = a.withTargetLock(ctx, exec, cfg.Name, name, "destroy", func() error {
			units := append([]string{networkUnit(cfg.Name)}, allContainerUnits(cfg.Name, eff.Services)...)
			if hasExposedServices(eff.Expose) {
				units = append(units, a.proxy().ServiceUnit(cfg.Name))
			}
			if len(units) > 0 {
				sp := startSpinner(a.Stdout, fmt.Sprintf("stopping units on %s", name))
				exec.Run(ctx, fmt.Sprintf(a.sctl()+" stop %s 2>/dev/null || true", joinQuoted(units)))
				sp.stop()
				sp = startSpinner(a.Stdout, fmt.Sprintf("disabling units on %s", name))
				exec.Run(ctx, fmt.Sprintf(a.sctl()+" disable %s 2>/dev/null || true", joinQuoted(units)))
				sp.stop()
			}
			qdDir := a.rt().UnitDir()
			sp := startSpinner(a.Stdout, "removing quadlet files")
			// Resolve the files that belong to this project instead of globbing
			// "<project>-*": the glob also matches the units of a project whose
			// name starts with this one ("app" vs "app-report").
			files := a.projectQuadletFiles(ctx, cfg.Name, qdDir, eff.Services, exec)
			if len(files) > 0 {
				paths := make([]string, 0, len(files))
				for _, f := range files {
					paths = append(paths, qdDir+"/"+shellQuote(f))
				}
				sudo := a.sudoPrefix()
				if _, err := exec.Run(ctx, fmt.Sprintf("%srm -f %s", sudo, strings.Join(paths, " "))); err != nil {
					sp.stop()
					return err
				}
			}
			sp.stop()
			sp = startSpinner(a.Stdout, "cleaning up proxy config")
			exec.Run(ctx, fmt.Sprintf("rm -rf ~/.config/qqd/%s", shellQuote(cfg.Name)))
			sp.stop()
			sp = startSpinner(a.Stdout, "reloading systemd daemon")
			if _, err := exec.Run(ctx, a.sctl()+" daemon-reload"); err != nil {
				sp.stop()
				return err
			}
			sp.stop()
			return nil
		})
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		fmt.Fprintf(a.Stdout, "%s target %s\n", boldGreen("destroyed"), bold(name))
		exec.Close()
	}
	return nil
}

// Clean removes project containers, project images, and dangling images from targets.
func (a *App) Clean(ctx context.Context, cfg ProjectConfig, targetName string) error {
	InitColor(a.Stdout)
	targets := targetOrder(cfg, targetName)
	for _, name := range targets {
		eff, err := resolveTarget(cfg, name, nil)
		if err != nil {
			return err
		}
		exec, err := a.ExecFactory.ForTarget(eff.Target)
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		defer exec.Close()

		// Remove stopped project containers. The container names are matched
		// against the configured service set rather than a "<project>-" name
		// filter, which would also match another project's containers when one
		// project name is a prefix of the other ("app" vs "app-report").
		sp := startSpinner(a.Stdout, fmt.Sprintf("removing containers on %s", name))
		out, err := exec.Run(ctx, a.crt()+" ps -a --filter status=exited --filter status=created --format '{{.Names}}'")
		sp.stop()
		if err == nil && strings.TrimSpace(out) != "" {
			var names []string
			for _, cname := range strings.Fields(strings.TrimSpace(out)) {
				if projectOwnsContainer(cfg.Name, cname, eff.Services) {
					names = append(names, cname)
				}
			}
			if len(names) > 0 {
				sp = startSpinner(a.Stdout, fmt.Sprintf("removing %d containers on %s", len(names), name))
				exec.Run(ctx, fmt.Sprintf(a.crt()+" rm -f %s", joinQuoted(names)))
				sp.stop()
			}
		}

		// Remove project images (old tags for each service's image repository)
		// Collect current image tags to skip (they may be in use by running containers)
		activeImages := map[string]bool{}
		repos := map[string]bool{}
		for _, svc := range eff.Services {
			activeImages[svc.Image] = true
			repo, _, ok := splitImageTag(svc.Image)
			if ok {
				repos[repo] = true
			} else {
				repos[svc.Image] = true
			}
		}
		// Keep every image a stored release points at: those are what `qqd
		// rollback` and the post-failure auto-rollback restore. Deleting them
		// leaves rollback dependent on the image still being pullable, which it
		// isn't for locally built images.
		if releases, err := listReleases(ctx, exec, cfg.Name); err == nil {
			for _, rel := range releases {
				for _, image := range rel.Services {
					activeImages[image] = true
				}
			}
		}
		if len(repos) > 0 {
			var filters []string
			for repo := range repos {
				filters = append(filters, fmt.Sprintf("--filter reference=%s", shellQuote(repo)))
			}
			sort.Strings(filters)
			sp = startSpinner(a.Stdout, fmt.Sprintf("removing project images on %s", name))
			out, err = exec.Run(ctx, fmt.Sprintf(a.crt()+" images %s --format '{{.Repository}}:{{.Tag}}'", strings.Join(filters, " ")))
			sp.stop()
			if err == nil && strings.TrimSpace(out) != "" {
				var stale []string
				for _, img := range strings.Split(strings.TrimSpace(out), "\n") {
					img = strings.TrimSpace(img)
					if img != "" && !activeImages[img] {
						stale = append(stale, img)
					}
				}
				if len(stale) > 0 {
					sp = startSpinner(a.Stdout, fmt.Sprintf("removing %d project images on %s", len(stale), name))
					exec.Run(ctx, fmt.Sprintf(a.crt()+" rmi %s", joinQuoted(stale)))
					sp.stop()
				}
			}
		}

		// Remove dangling images
		sp = startSpinner(a.Stdout, fmt.Sprintf("removing dangling images on %s", name))
		exec.Run(ctx, a.crt()+" image prune -f")
		sp.stop()

		fmt.Fprintf(a.Stdout, "%s target %s\n", boldGreen("cleaned"), bold(name))
		exec.Close()
	}
	return nil
}

// systemdCommand executes a start/stop action for selected service units.
func (a *App) systemdCommand(ctx context.Context, cfg ProjectConfig, targetName string, services []string, action string) error {
	a.applyConfig(cfg)
	InitColor(a.Stdout)
	targets := targetOrder(cfg, targetName)
	qdDir := a.rt().UnitDir()
	for _, name := range targets {
		eff, err := resolveTarget(cfg, name, services)
		if err != nil {
			return err
		}
		exec, err := a.ExecFactory.ForTarget(eff.Target)
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		defer exec.Close()
		sel := a.lifecycleFor(ctx, eff.Target, exec)
		if sel.Backend == "direct" {
			if err := a.stopStartDirect(ctx, cfg, eff, exec, sel.Selected, action, name); err != nil {
				return fmt.Errorf("target %s: %w", name, err)
			}
			continue
		}
		var units []string
		if action == "start" {
			units = append(units, networkUnit(cfg.Name))
		}
		for _, svcName := range sortedKeys(eff.Services) {
			svc := eff.Services[svcName]
			unit := resolveServiceUnit(ctx, exec, cfg.Name, svcName, svc, qdDir, a.rt().UnitExtension(), a.sctl())
			units = append(units, unit)
		}
		if hasExposedServices(eff.Expose) {
			units = append(units, a.proxy().ServiceUnit(cfg.Name))
		}
		if len(units) == 0 {
			continue
		}
		sp := startSpinner(a.Stdout, fmt.Sprintf("%sing %d units on %s", action, len(units), name))
		if _, err := exec.Run(ctx, fmt.Sprintf(a.sctl()+" %s %s", action, joinQuoted(units))); err != nil {
			sp.stop()
			return err
		}
		sp.stop()
		fmt.Fprintf(a.Stdout, "%s target %s\n", boldGreen(action+"ped"), bold(name))
		exec.Close()
	}
	return nil
}

// resolveServiceUnit returns the correct systemd unit name for a service,
// checking for slot-based deployments. For replicated services, returns the standard
// unit (replicas are handled individually where needed).
func resolveServiceUnit(ctx context.Context, exec Executor, project, service string, svc ServiceConfig, qdDir, unitExt, sctl string) string {
	if isReplicated(svc) {
		return fmt.Sprintf("%s-%s-1.service", project, service)
	}
	slot := detectActiveSlot(ctx, exec, project, service, qdDir, unitExt, sctl)
	if slot != "" {
		return fmt.Sprintf("%s-%s-%s.service", project, service, slot)
	}
	return containerUnit(project, service)
}

// diagnoseFailedUnits prints diagnostics for any non-active service units.
// Slotted services are probed by their active slot unit/container name so the
// output reflects what's actually on disk.
func (a *App) diagnoseFailedUnits(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, targetExec Executor) {
	qdDir := a.rt().UnitDir()
	for _, svcName := range sortedKeys(eff.Services) {
		svc := eff.Services[svcName]
		if isReplicated(svc) {
			for i := 1; i <= effectiveReplicas(svc); i++ {
				unit := fmt.Sprintf("%s-%s-%d.service", cfg.Name, svcName, i)
				cname := fmt.Sprintf("%s-%s-%d", cfg.Name, svcName, i)
				a.diagnoseUnit(ctx, targetExec, fmt.Sprintf("%s/%d", svcName, i), unit, cname, svc)
			}
		} else {
			unit := containerUnit(cfg.Name, svcName)
			cname := containerName(cfg.Name, svcName)
			if slot := detectActiveSlot(ctx, targetExec, cfg.Name, svcName, qdDir, a.rt().UnitExtension(), a.sctl()); slot != "" {
				unit = fmt.Sprintf("%s-%s-%s.service", cfg.Name, svcName, slot)
				cname = fmt.Sprintf("%s-%s-%s", cfg.Name, svcName, slot)
			}
			a.diagnoseUnit(ctx, targetExec, svcName, unit, cname, svc)
		}
	}
}

// diagnoseUnit prints diagnostics for a single unit if it is not active.
func (a *App) diagnoseUnit(ctx context.Context, exec Executor, label, unit, cname string, svc ServiceConfig) {
	state, _ := exec.Run(ctx, fmt.Sprintf(a.sctl()+" is-active %s 2>/dev/null || true", shellQuote(unit)))
	state = strings.TrimSpace(state)
	if state == "active" {
		return
	}
	fmt.Fprintf(a.Stdout, "\n%s\n", boldRed(fmt.Sprintf("--- %s (state: %s) ---", label, state)))

	if status, err := exec.Run(ctx, fmt.Sprintf(a.sctl()+" status %s --no-pager -l 2>&1 || true", shellQuote(unit))); err == nil && strings.TrimSpace(status) != "" {
		fmt.Fprintf(a.Stdout, "[systemctl status]\n%s\n", strings.TrimSpace(status))
	}

	// Slot units run with --rm, so a failed container is usually already reaped
	// by the time we get here and `podman logs` only reports "no such
	// container". Fall back to the unit journal, which still holds its output.
	logs, err := exec.Run(ctx, fmt.Sprintf(a.crt()+" logs --tail 30 %s 2>&1 || true", shellQuote(cname)))
	logs = strings.TrimSpace(logs)
	if err == nil && logs != "" && !containerMissing(logs) {
		fmt.Fprintf(a.Stdout, "[podman logs]\n%s\n", logs)
	} else if j, jerr := exec.Run(ctx, fmt.Sprintf("%s -u %s --no-pager -n 40 2>&1 || true", journalPrefix(a.sctl()), shellQuote(unit))); jerr == nil && strings.TrimSpace(j) != "" {
		fmt.Fprintf(a.Stdout, "[journal %s]\n%s\n", unit, strings.TrimSpace(j))
	}

	for _, vol := range svc.Volumes {
		hostPath := strings.SplitN(vol, ":", 2)[0]
		if hostPath == "" {
			continue
		}
		if lsOut, err := exec.Run(ctx, fmt.Sprintf("ls -ld %s 2>&1 || true", shellQuote(hostPath))); err == nil && strings.TrimSpace(lsOut) != "" {
			fmt.Fprintf(a.Stdout, "[volume %s]\n%s\n", hostPath, strings.TrimSpace(lsOut))
		}
	}
}
