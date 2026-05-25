package qqd

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// statusDirect is the direct-mode counterpart to the systemd-aware
// portion of Status. It uses the Lifecycle's List + Status to produce the
// same shape of output (text or JSON). Scoped to this target so multiple
// targets sharing one runtime daemon stay separate.
func (a *App) statusDirect(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, exec Executor, lc Lifecycle, jsonMode bool, result *StatusResult, name string) error {
	var statuses []UnitStatus
	var err error
	if dl, ok := lc.(directLifecycle); ok {
		statuses, err = dl.listForTarget(ctx, exec, cfg.Name, eff.Target.Name)
	} else {
		statuses, err = lc.List(ctx, exec, cfg.Name)
	}
	if err != nil {
		return fmt.Errorf("list project containers: %w", err)
	}
	statusByName := map[string]UnitStatus{}
	for _, st := range statuses {
		statusByName[st.Name] = st
	}

	type row struct {
		label string
		name  string
	}
	var rows []row
	for _, svcName := range sortedKeys(eff.Services) {
		svc := eff.Services[svcName]
		if isReplicated(svc) {
			for i := 1; i <= effectiveReplicas(svc); i++ {
				rows = append(rows, row{
					label: fmt.Sprintf("%s/%d", svcName, i),
					name:  directContainerName(cfg.Name, eff.Target.Name, svcName, i),
				})
			}
		} else {
			rows = append(rows, row{
				label: svcName,
				name:  directContainerName(cfg.Name, eff.Target.Name, svcName, 1),
			})
		}
	}
	if hasExposedServices(eff.Expose) {
		rows = append(rows, row{
			label: "proxy",
			name:  directProxyName(cfg.Name, eff.Target.Name),
		})
	}

	if jsonMode {
		ts := TargetStatus{Name: name, Host: eff.Target.Host, Backend: "direct"}
		for _, r := range rows {
			st := statusByName[r.name]
			ss := ServiceStatus{Name: r.label, State: stateOrMissing(st)}
			if st.Image != "" {
				ss.Image = st.Image
			}
			if st.UptimeS > 0 {
				ss.Uptime = humanDuration(time.Duration(st.UptimeS) * time.Second)
			}
			ts.Services = append(ts.Services, ss)
		}
		result.Targets = append(result.Targets, ts)
		return nil
	}

	fmt.Fprintf(a.Stdout, "  %s lifecycle: %s\n", dim("backend"), bold("direct"))
	for _, r := range rows {
		st := statusByName[r.name]
		s := stateOrMissing(st)
		coloredState := red(s)
		if s == "active" {
			coloredState = green(s)
		}
		extra := ""
		if st.Image != "" {
			extra += " " + st.Image
		}
		if st.UptimeS > 0 {
			extra += " up=" + humanDuration(time.Duration(st.UptimeS)*time.Second)
		}
		if st.Health != "" {
			extra += " health=" + st.Health
		}
		fmt.Fprintf(a.Stdout, "  %s: %s%s\n", bold(r.label), coloredState, dim(extra))
	}
	a.showOtherContainers(ctx, exec, cfg.Name)
	a.checkPortConflicts(ctx, exec, eff.Expose, cfg.Name)
	return nil
}

func stateOrMissing(st UnitStatus) string {
	if st.State == "" {
		return "missing"
	}
	return st.State
}

// stopStartDirect implements the direct-mode counterpart of systemdCommand
// for the action verbs "stop" and "start". For "start" this also ensures
// the project network exists so a fresh `qqd start` works after a `stop`.
func (a *App) stopStartDirect(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, exec Executor, lc Lifecycle, action, name string) error {
	if action == "start" {
		_, _ = exec.Run(ctx, fmt.Sprintf("%s network create %s 2>/dev/null || true", a.crt(), shellQuote(directNetworkName(cfg.Name, eff.Target.Name))))
	}
	type targetRow struct {
		container string
		role      string
	}
	var rows []targetRow
	for _, svcName := range sortedKeys(eff.Services) {
		svc := eff.Services[svcName]
		if isReplicated(svc) {
			for i := 1; i <= effectiveReplicas(svc); i++ {
				rows = append(rows, targetRow{container: directContainerName(cfg.Name, eff.Target.Name, svcName, i), role: "app"})
			}
		} else {
			rows = append(rows, targetRow{container: directContainerName(cfg.Name, eff.Target.Name, svcName, 1), role: "app"})
		}
	}
	if hasExposedServices(eff.Expose) {
		rows = append(rows, targetRow{container: directProxyName(cfg.Name, eff.Target.Name), role: "proxy"})
	}
	sp := startSpinner(a.Stdout, fmt.Sprintf("%sing %d containers on %s", action, len(rows), name))
	defer sp.stop()
	for _, r := range rows {
		var err error
		switch action {
		case "stop":
			err = lc.Stop(ctx, exec, r.container)
		case "start":
			err = lc.Start(ctx, exec, r.container)
		default:
			return fmt.Errorf("unknown action %q", action)
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", action, r.container, err)
		}
	}
	fmt.Fprintf(a.Stdout, "%s target %s\n", boldGreen(action+"ped"), bold(name))
	return nil
}

// destroyDirect tears down everything qqd installed for this project *on
// this target*: every container labeled with qqd.project=<name> AND
// qqd.target=<target>, plus the proxy config files. Other targets sharing
// the same runtime daemon are unaffected.
func (a *App) destroyDirect(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, exec Executor, lc Lifecycle, name string) error {
	var statuses []UnitStatus
	var err error
	if dl, ok := lc.(directLifecycle); ok {
		statuses, err = dl.listForTarget(ctx, exec, cfg.Name, eff.Target.Name)
	} else {
		statuses, err = lc.List(ctx, exec, cfg.Name)
	}
	if err != nil {
		return fmt.Errorf("list project containers: %w", err)
	}
	if len(statuses) > 0 {
		sp := startSpinner(a.Stdout, fmt.Sprintf("removing %d containers on %s", len(statuses), name))
		// Stable order so output is reproducible.
		sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
		for _, st := range statuses {
			if err := lc.Remove(ctx, exec, st.Name); err != nil {
				sp.stop()
				return fmt.Errorf("remove %s: %w", st.Name, err)
			}
		}
		sp.stop()
	}
	sp := startSpinner(a.Stdout, "cleaning up proxy config")
	// Only remove THIS target's proxy config dir; sibling targets sharing
	// the same project keep theirs.
	_, _ = exec.Run(ctx, fmt.Sprintf("rm -rf %s", directProxyConfDir(cfg.Name, eff.Target.Name)))
	sp.stop()
	// Remove this target's network. Best-effort: if any container is still
	// attached, the runtime refuses, which is fine.
	_, _ = exec.Run(ctx, fmt.Sprintf("%s network rm %s 2>/dev/null || true", a.crt(), shellQuote(directNetworkName(cfg.Name, eff.Target.Name))))
	fmt.Fprintf(a.Stdout, "%s target %s\n", boldGreen("destroyed"), bold(name))
	return nil
}

// directRestartChanged restarts a set of services that changed image or
// config. In direct mode this is just lc.Install per container — the
// implementation does the atomic stop+rm+run replace. Names are
// target-scoped to match installAndStartDirect.
func (a *App) directRestartChanged(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, exec Executor, lc Lifecycle, changed []string) error {
	deployID := releaseID()
	for _, name := range changed {
		if name == "__proxy__" {
			spec := buildProxyContainerSpec(cfg, eff, eff.Expose, eff.Services, a.proxy(), deployID)
			if err := lc.Install(ctx, exec, spec); err != nil {
				return fmt.Errorf("restart proxy: %w", err)
			}
			continue
		}
		svc, ok := eff.Services[name]
		if !ok {
			continue
		}
		if isReplicated(svc) {
			for i := 1; i <= effectiveReplicas(svc); i++ {
				cname := directContainerName(cfg.Name, eff.Target.Name, name, i)
				spec := buildContainerSpec(cfg, eff, name, svc, i, cname, deployID)
				if err := lc.Install(ctx, exec, spec); err != nil {
					return fmt.Errorf("restart %s replica %d: %w", name, i, err)
				}
			}
			continue
		}
		cname := directContainerName(cfg.Name, eff.Target.Name, name, 1)
		spec := buildContainerSpec(cfg, eff, name, svc, 1, cname, deployID)
		if err := lc.Install(ctx, exec, spec); err != nil {
			return fmt.Errorf("restart %s: %w", name, err)
		}
	}
	return nil
}
