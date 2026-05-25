package qqd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// slotDeploy performs a zero-downtime slot-based deployment for an exposed, non-replicated service.
func (a *App) slotDeploy(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, exec Executor, serviceName string, svc ServiceConfig, allServices map[string]ServiceConfig, activeSlots map[string]string) error {
	project := cfg.Name
	qdDir := a.rt().UnitDir()
	dynamicPath := a.proxy().DynamicConfigPath(project)

	unitExt := a.rt().UnitExtension()

	// 1. Detect current slot → compute new slot from image hash
	currentSlot := detectActiveSlot(ctx, exec, project, serviceName, qdDir, unitExt)
	newSlot := slotHash(svc.Image)
	if newSlot == currentSlot {
		// Same image hash — reconcile the current slot content, then restart if needed.
		slotUnit := fmt.Sprintf("%s-%s-%s.service", project, serviceName, currentSlot)
		slotPath := fmt.Sprintf("%s/%s-%s-%s%s", qdDir, project, serviceName, currentSlot, unitExt)
		expected := renderExpectedSlotContent(project, serviceName, currentSlot, svc, activeSlots, a.rt())
		currentContent, _ := exec.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null || true", slotPath))
		contentMatches := strings.TrimSpace(currentContent) == strings.TrimSpace(expected)
		if state, err := exec.Run(ctx, fmt.Sprintf(a.sctl()+" is-active %s 2>/dev/null || true", shellQuote(slotUnit))); err == nil && strings.TrimSpace(state) == "active" && contentMatches {
			fmt.Fprintf(a.Stdout, "  %s already on slot %s\n", bold(serviceName), bold(newSlot))
			return nil
		}
		if !contentMatches {
			heredoc := fmt.Sprintf("cat > %s <<'QD_EOF'\n%sQD_EOF", slotPath, expected)
			if _, err := exec.Run(ctx, heredoc); err != nil {
				return fmt.Errorf("rewrite slot %s: %w", currentSlot, err)
			}
			if _, err := exec.Run(ctx, a.sctl()+" daemon-reload"); err != nil {
				return fmt.Errorf("daemon-reload after slot rewrite: %w", err)
			}
		}
		fmt.Fprintf(a.Stdout, "  %s slot %s needs reconciliation, restarting\n", bold(serviceName), bold(currentSlot))
		sp := startSpinner(a.Stdout, fmt.Sprintf("restarting slot %s", currentSlot))
		if _, err := exec.Run(ctx, fmt.Sprintf(a.sctl()+" restart %s", shellQuote(slotUnit))); err != nil {
			sp.stop()
			return fmt.Errorf("restart slot %s: %w", currentSlot, err)
		}
		if err := a.waitForReady(ctx, exec, fmt.Sprintf("%s-%s-%s", project, serviceName, currentSlot), rewriteDepsForSlots(svc, activeSlots)); err != nil {
			sp.stop()
			return fmt.Errorf("reconciled slot %s not ready: %w", currentSlot, err)
		}
		sp.stop()
		return nil
	}

	newContainer := fmt.Sprintf("%s-%s-%s", project, serviceName, newSlot)
	newQuadlet := fmt.Sprintf("%s-%s-%s%s", project, serviceName, newSlot, unitExt)
	newUnit := fmt.Sprintf("%s-%s-%s.service", project, serviceName, newSlot)

	fmt.Fprintf(a.Stdout, "  zero-downtime %s %s slot %s\n", bold(serviceName), bold(newSlot), dim("("+imageTag(svc.Image)+")"))

	// 2. Render quadlet with slot, write to target
	// Rewrite DependsOn to reference active slot units for any slotted deps
	svc = rewriteDepsForSlots(svc, activeSlots)
	content := a.rt().RenderContainerWithSlot(project, serviceName, newSlot, svc)
	path := fmt.Sprintf("%s/%s", qdDir, newQuadlet)
	heredoc := fmt.Sprintf("cat > %s <<'QD_EOF'\n%sQD_EOF", path, content)
	if _, err := exec.Run(ctx, heredoc); err != nil {
		return fmt.Errorf("write slot quadlet: %w", err)
	}

	// cleanupNewSlot removes the new slot's quadlet and stops its unit.
	// Uses context.Background() so cleanup works even when ctx is cancelled (Ctrl+C).
	cleanupNewSlot := func() {
		bgCtx := context.Background()
		exec.Run(bgCtx, fmt.Sprintf(a.sctl()+" stop %s 2>/dev/null || true", shellQuote(newUnit)))
		exec.Run(bgCtx, fmt.Sprintf("rm -f %s", path))
		exec.Run(bgCtx, a.sctl()+" daemon-reload")
	}

	// If context is cancelled (Ctrl+C) while the new slot is starting or
	// waiting for readiness, clean up the new slot before returning.
	deployed := false
	defer func() {
		if !deployed && ctx.Err() != nil {
			fmt.Fprintf(a.Stdout, "\n  interrupted: cleaning up slot %s\n", newSlot)
			cleanupNewSlot()
		}
	}()

	// 3. daemon-reload and start new container
	sp := startSpinner(a.Stdout, fmt.Sprintf("starting new slot %s", newSlot))
	if _, err := exec.Run(ctx, a.sctl()+" daemon-reload"); err != nil {
		sp.stop()
		return fmt.Errorf("daemon-reload: %w", err)
	}
	if _, err := exec.Run(ctx, fmt.Sprintf(a.sctl()+" start %s", shellQuote(newUnit))); err != nil {
		sp.stop()
		cleanupNewSlot()
		deployed = true // cleanup already done, skip defer
		return fmt.Errorf("start new slot %s: %w", newSlot, err)
	}
	sp.stop()

	// 4. Wait for ready — if fails, stop new container, remove quadlet, return error
	sp = startSpinner(a.Stdout, "waiting for readiness")
	if err := a.waitForReady(ctx, exec, newContainer, svc); err != nil {
		sp.stop()
		fmt.Fprintf(a.Stdout, "  error: new %s slot failed readiness, rolling back\n", newSlot)
		cleanupNewSlot()
		deployed = true // cleanup already done, skip defer
		return fmt.Errorf("new slot %s not ready: %w", newSlot, err)
	}
	sp.stop()
	deployed = true

	// 5. Rewrite dynamic routes with SlotOverrides
	opts := DynamicConfigOpts{
		SlotOverrides: map[string]string{serviceName: newContainer},
	}
	dynamicConf := a.proxy().GenerateDynamicConfig(project, allServices, eff.Expose, opts)
	if err := atomicWriteRemote(ctx, exec, dynamicPath, dynamicConf); err != nil {
		return fmt.Errorf("write dynamic config: %w", err)
	}

	// 6. Brief drain wait for in-flight requests
	drainWait := a.effectiveDrainWait()
	if drainWait > 0 {
		sp = startSpinner(a.Stdout, "draining in-flight requests")
		select {
		case <-time.After(drainWait):
		case <-ctx.Done():
		}
		sp.stop()
	}

	// 7. Rewrite dependent quadlets BEFORE stopping old unit.
	// If service B has Requires=proj-server.service and we stop proj-server,
	// systemd cascades the stop to B. By rewriting B's quadlet to reference
	// the new slot (proj-server-a1b2c3d4.service) and reloading, the cascade is avoided.
	oldDep := fmt.Sprintf("%s-%s.service", project, serviceName) // standard unit
	if currentSlot != "" {
		oldDep = fmt.Sprintf("%s-%s-%s.service", project, serviceName, currentSlot) // old slot unit
	}
	newDep := fmt.Sprintf("%s-%s-%s.service", project, serviceName, newSlot)
	needReload := false
	for depName, depSvc := range allServices {
		for _, d := range depSvc.DependsOn {
			if d != serviceName {
				continue
			}
			// Find quadlet file(s) for this dependent service. If the dependent
			// is itself slotted, its file is at proj-<svc>-<hash>.container, not
			// the standard proj-<svc>.container path.
			var depPaths []string
			if isReplicated(depSvc) {
				for i := 1; i <= effectiveReplicas(depSvc); i++ {
					depPaths = append(depPaths, fmt.Sprintf("%s/%s-%s-%d%s", qdDir, project, depName, i, unitExt))
				}
			} else if depSlot, ok := activeSlots[depName]; ok && depSlot != "" {
				depPaths = append(depPaths, fmt.Sprintf("%s/%s-%s-%s%s", qdDir, project, depName, depSlot, unitExt))
			} else {
				depPaths = append(depPaths, fmt.Sprintf("%s/%s-%s%s", qdDir, project, depName, unitExt))
			}
			for _, depPath := range depPaths {
				content, err := exec.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null || true", depPath))
				if err != nil || content == "" {
					continue
				}
				updated := strings.ReplaceAll(content, oldDep, newDep)
				if updated != content {
					heredoc := fmt.Sprintf("cat > %s <<'QD_EOF'\n%sQD_EOF", depPath, updated)
					exec.Run(ctx, heredoc)
					needReload = true
				}
			}
		}
	}
	if needReload {
		exec.Run(ctx, a.sctl()+" daemon-reload")
	}

	// 8. Remove old quadlet files, daemon-reload, THEN stop old containers.
	// Order matters: removing the quadlet and reloading first ensures systemd
	// forgets the unit definition (including Restart=always) before we stop
	// the container. If we stopped first, systemd would auto-restart it in the
	// gap before daemon-reload.
	sp = startSpinner(a.Stdout, "stopping old slot")
	var oldUnitsToStop []string
	if currentSlot != "" {
		oldQuadlet := fmt.Sprintf("%s/%s-%s-%s%s", qdDir, project, serviceName, currentSlot, unitExt)
		exec.Run(ctx, fmt.Sprintf("rm -f %s", oldQuadlet))
		oldUnitsToStop = append(oldUnitsToStop, fmt.Sprintf("%s-%s-%s.service", project, serviceName, currentSlot))
	}

	// 9. Clean up standard (non-slotted) quadlet if migrating from pre-slot deployment
	stdQuadlet := fmt.Sprintf("%s/%s-%s%s", qdDir, project, serviceName, unitExt)
	if fileExistsRemote(ctx, exec, stdQuadlet) {
		exec.Run(ctx, fmt.Sprintf("rm -f %s", stdQuadlet))
		oldUnitsToStop = append(oldUnitsToStop, fmt.Sprintf("%s-%s.service", project, serviceName))
	}

	if _, err := exec.Run(ctx, a.sctl()+" daemon-reload"); err != nil {
		sp.stop()
		return fmt.Errorf("final daemon-reload: %w", err)
	}
	for _, unit := range oldUnitsToStop {
		exec.Run(ctx, fmt.Sprintf(a.sctl()+" stop %s 2>/dev/null || true", shellQuote(unit)))
	}
	sp.stop()

	fmt.Fprintf(a.Stdout, "  %s deployed via %s slot\n", bold(serviceName), bold(newSlot))
	return nil
}

// slotHash returns an 8-character hex hash derived from the image string.
// This provides deterministic, collision-resistant slot names.
func slotHash(image string) string {
	h := sha256.Sum256([]byte(image))
	return hex.EncodeToString(h[:4])
}

// renderExpectedSlotContent renders the slot quadlet content exactly as slotDeploy writes it.
func renderExpectedSlotContent(project, serviceName, slot string, svc ServiceConfig, activeSlots map[string]string, rt ContainerRuntime) string {
	return rt.RenderContainerWithSlot(project, serviceName, slot, rewriteDepsForSlots(svc, activeSlots))
}

// detectActiveSlot checks which slot is currently active for a service.
// Returns the slot suffix (hash like "a1b2c3d4")
// or "" if no slot file exists (first deploy or standard name).
func detectActiveSlot(ctx context.Context, exec Executor, project, service, qdDir, unitExt string) string {
	prefix := fmt.Sprintf("%s-%s-", project, service)
	out, err := exec.Run(ctx, fmt.Sprintf("ls -1 %s 2>/dev/null || true", qdDir))
	if err != nil {
		return ""
	}
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, unitExt) {
			continue
		}
		suffix := strings.TrimSuffix(strings.TrimPrefix(name, prefix), unitExt)
		if suffix == "" {
			continue
		}
		if _, err := strconv.Atoi(suffix); err == nil {
			continue
		}
		if !isValidSlotHash(suffix) {
			continue
		}
		return suffix
	}
	return ""
}

// detectActiveSlotFromListing parses an ls listing to find a slot file.
// This avoids a separate SSH call when the listing is already available.
func detectActiveSlotFromListing(project, service, listing, unitExt string) string {
	prefix := fmt.Sprintf("%s-%s-", project, service)
	for _, name := range strings.Split(strings.TrimSpace(listing), "\n") {
		name = strings.TrimSpace(name)
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, unitExt) {
			continue
		}
		suffix := strings.TrimSuffix(strings.TrimPrefix(name, prefix), unitExt)
		if suffix == "" {
			continue
		}
		if _, err := strconv.Atoi(suffix); err == nil {
			continue
		}
		if !isValidSlotHash(suffix) {
			continue
		}
		return suffix
	}
	return ""
}

// isValidSlotHash checks if a string looks like a slot hash (8 lowercase hex chars).
func isValidSlotHash(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// cleanStaleSlots removes slot quadlet files and orphaned containers that don't
// match the current active slot for each slotted service. This self-heals state
// left by interrupted deploys or previous bugs.
func (a *App) cleanStaleSlots(ctx context.Context, project, qdDir string, slottedSvcs map[string]bool, activeSlots map[string]string, exec Executor) {
	if len(slottedSvcs) == 0 {
		return
	}
	out, err := exec.Run(ctx, fmt.Sprintf("ls -1 %s 2>/dev/null || true", qdDir))
	if err != nil {
		return
	}

	// Find stale slot quadlet files (slot hash doesn't match active slot)
	unitExt := a.rt().UnitExtension()
	var staleFiles []string
	var staleUnits []string
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		if name == "" || !strings.HasSuffix(name, unitExt) {
			continue
		}
		for svcName := range slottedSvcs {
			prefix := fmt.Sprintf("%s-%s-", project, svcName)
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			suffix := strings.TrimSuffix(strings.TrimPrefix(name, prefix), unitExt)
			if !isValidSlotHash(suffix) {
				continue
			}
			if suffix == activeSlots[svcName] {
				continue // active slot, keep it
			}
			staleFiles = append(staleFiles, name)
			staleUnits = append(staleUnits, strings.TrimSuffix(name, unitExt)+".service")
		}
	}

	// Remove stale quadlet files, daemon-reload, then stop units
	if len(staleFiles) > 0 {
		for _, f := range staleFiles {
			fmt.Fprintf(a.Stdout, "  %s stale slot %s\n", yellow("removing"), dim(f))
			exec.Run(ctx, fmt.Sprintf("rm -f %s/%s", qdDir, f))
		}
		exec.Run(ctx, a.sctl()+" daemon-reload")
		for _, unit := range staleUnits {
			exec.Run(ctx, fmt.Sprintf(a.sctl()+" stop %s 2>/dev/null || true", shellQuote(unit)))
		}
	}

	// Clean up orphaned containers (running slot containers without a quadlet file)
	for svcName := range slottedSvcs {
		activeContainer := ""
		if slot := activeSlots[svcName]; slot != "" {
			activeContainer = fmt.Sprintf("%s-%s-%s", project, svcName, slot)
		}
		prefix := fmt.Sprintf("%s-%s-", project, svcName)
		cOut, err := exec.Run(ctx, fmt.Sprintf(a.crt()+" ps -a --filter name=%s --format '{{.Names}}'", shellQuote(prefix)))
		if err != nil || strings.TrimSpace(cOut) == "" {
			continue
		}
		for _, cname := range strings.Split(strings.TrimSpace(cOut), "\n") {
			cname = strings.TrimSpace(cname)
			if cname == "" || cname == activeContainer {
				continue
			}
			suffix := strings.TrimPrefix(cname, prefix)
			if !isValidSlotHash(suffix) {
				continue
			}
			fmt.Fprintf(a.Stdout, "  %s orphaned container %s\n", yellow("removing"), dim(cname))
			exec.Run(ctx, fmt.Sprintf(a.crt()+" stop %s 2>/dev/null || true", shellQuote(cname)))
			exec.Run(ctx, fmt.Sprintf(a.crt()+" rm %s 2>/dev/null || true", shellQuote(cname)))
		}
	}
}

// isSlotFile checks if a quadlet filename is a slot file (hash-based)
// for a service that is managed by zero-downtime slot deployments.
func isSlotFile(project, filename, unitExt string, slottedSvcs map[string]bool) bool {
	if len(slottedSvcs) == 0 {
		return false
	}
	base := strings.TrimSuffix(filename, unitExt)
	prefix := project + "-"
	if !strings.HasPrefix(base, prefix) {
		return false
	}
	rest := strings.TrimPrefix(base, prefix)
	for svcName := range slottedSvcs {
		svcPrefix := svcName + "-"
		if strings.HasPrefix(rest, svcPrefix) {
			suffix := strings.TrimPrefix(rest, svcPrefix)
			if isValidSlotHash(suffix) {
				return true
			}
		}
	}
	return false
}
