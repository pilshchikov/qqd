package qqd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// App wires command handlers to execution primitives.
type App struct {
	ExecFactory     ExecFactory
	Runtime         ContainerRuntime // container runtime (nil = Podman)
	Proxy           ProxyProvider
	BuildStrategies map[string]BuildStrategy // custom build strategies (nil = use defaults)
	Stdout          io.Writer
	NoBuild         bool          // skip building dockerfile services
	DrainWait       time.Duration // pause for in-flight requests during zero-downtime slot switch (default 2s)
	OutputFormat    string        // "" for text (default), "json" for JSON
	ForceUnlock     bool          // --force-unlock: take the deploy lock even if another holder is recorded
	NoLock          bool          // skip the deploy lock entirely (only for tests / read-only ops)
	DryRun          bool          // --dry-run: print actions instead of executing (currently honored by migrate)
	AssumeYes       bool          // --yes: skip confirmation prompts on destructive commands

	// lifecycleCache memoizes the per-target Lifecycle selection within a
	// single qqd invocation so we don't re-probe systemctl on every command.
	// Keyed by target name. Populated lazily by lifecycleFor.
	lifecycleCache map[string]LifecycleSelection

	// homeDirCache memoizes the resolved $HOME for each target, used by the
	// direct backend to expand "~/..." paths in volume mounts. Keyed by
	// target name.
	homeDirCache map[string]string
}

// NewApp constructs an App with default dependencies.
func NewApp() *App {
	return &App{
		ExecFactory: DefaultExecFactory{},
		Proxy:       TraefikProvider{},
		Stdout:      os.Stdout,
	}
}

func (a *App) crt() string {
	return a.rt().Cmd()
}

func (a *App) sctl() string {
	return a.rt().SystemctlPrefix()
}

func (a *App) sudoPrefix() string {
	return ""
}

// rt returns the configured ContainerRuntime, defaulting to PodmanRuntime.
func (a *App) rt() ContainerRuntime {
	if a.Runtime != nil {
		return a.Runtime
	}
	return PodmanRuntime{}
}

// homeDirFor returns the value of $HOME on the given target, queried once
// and cached. Used by the direct backend to pre-expand "~/..." paths in
// volume mounts.
func (a *App) homeDirFor(ctx context.Context, t TargetConfig, exec Executor) string {
	if a.homeDirCache == nil {
		a.homeDirCache = map[string]string{}
	}
	if cached, ok := a.homeDirCache[t.Name]; ok {
		return cached
	}
	out, err := exec.Run(ctx, "printf %s \"$HOME\"")
	home := strings.TrimSpace(out)
	if err != nil || home == "" {
		// Best-effort fallback to the local user's home; better than emitting
		// a literal "~" into a volume argument.
		if h, herr := os.UserHomeDir(); herr == nil {
			home = h
		}
	}
	a.homeDirCache[t.Name] = home
	return home
}

// expandTildeAt returns p with a leading "~" or "~/" replaced by the
// supplied home directory. Other paths are returned unchanged.
func expandTildeAt(p, home string) string {
	if home == "" {
		return p
	}
	switch {
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return home + p[1:]
	default:
		return p
	}
}

// lifecycleFor resolves which Lifecycle backend should manage processes on
// the given target. Result is memoized per-target for the lifetime of the
// App. The probe (when needed) runs against the supplied executor.
//
// "auto" probes the target with `command -v systemctl` and falls back to
// "direct" if not present. "systemd" and "direct" force their respective
// backends without probing.
func (a *App) lifecycleFor(ctx context.Context, t TargetConfig, exec Executor) LifecycleSelection {
	if a.lifecycleCache == nil {
		a.lifecycleCache = map[string]LifecycleSelection{}
	}
	if cached, ok := a.lifecycleCache[t.Name]; ok {
		return cached
	}
	setting := strings.ToLower(strings.TrimSpace(t.Lifecycle))
	probeOK := true
	if setting == "" || setting == "auto" {
		probeOK = probeSystemctl(ctx, exec)
	}
	sel := chooseLifecycle(setting, a.rt(), probeOK)
	a.lifecycleCache[t.Name] = sel
	return sel
}

// proxy returns the configured ProxyProvider, defaulting to TraefikProvider.
func (a *App) proxy() ProxyProvider {
	if a.Proxy != nil {
		return a.Proxy
	}
	return TraefikProvider{}
}

func (a *App) applyConfig(cfg ProjectConfig) {
	if a.Runtime == nil && cfg.Runtime != "" {
		a.Runtime = runtimeByName(cfg.Runtime)
	}
	if a.Proxy == nil && (cfg.Proxy != "" || cfg.ProxyImage != "") {
		a.Proxy = proxyProviderByName(cfg.Proxy, cfg.ProxyImage)
	}
}

func (a *App) effectiveDrainWait() time.Duration {
	if a.DrainWait > 0 {
		return a.DrainWait
	}
	if a.DrainWait < 0 {
		return 0
	}
	return 2 * time.Second
}

// Init performs first-time setup for one or all targets.
func (a *App) Init(ctx context.Context, cfg ProjectConfig, targetName string, cliServices []string, rebuild bool) error {
	a.applyConfig(cfg)
	InitColor(a.Stdout)
	targets := targetOrder(cfg, targetName)
	for _, name := range targets {
		fmt.Fprintf(a.Stdout, "%s resolving target %s\n", boldCyan("[init]"), bold(name))
		eff, err := resolveTarget(cfg, name, cliServices)
		if err != nil {
			return err
		}
		exec, err := a.ExecFactory.ForTarget(eff.Target)
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		defer exec.Close()
		err = a.withTargetLock(ctx, exec, cfg.Name, name, "init", func() error {
			fmt.Fprintf(a.Stdout, "%s syncing repo and directories on %s\n", boldCyan("[init]"), dim(eff.Target.Host))
			uploaded, err := a.ensureRepoAndDirs(ctx, cfg, eff, exec, rebuild)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "%s ensuring images on %s\n", boldCyan("[init]"), dim(eff.Target.Host))
			if _, err := a.ensureImages(ctx, cfg, eff, exec, false, rebuild); err != nil {
				return err
			}
			if len(uploaded) > 0 {
				a.cleanupUploadedSource(ctx, exec, uploaded)
			}
			fmt.Fprintf(a.Stdout, "%s installing quadlet files and starting services on %s\n", boldCyan("[init]"), dim(eff.Target.Host))
			return a.installAndStart(ctx, cfg, eff, exec, true, nil, false)
		})
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		fmt.Fprintf(a.Stdout, "%s target %s %s\n", boldGreen("initialized"), bold(name), dim("("+eff.Target.Host+")"))
	}
	return nil
}

// withTargetLock runs fn while holding the deploy lock for (project, target) on
// the given executor. Skipped when a.NoLock is set. If the lock is already held
// and a.ForceUnlock is false, the lock contention error from acquireLock is
// returned so the caller can surface it to the user.
func (a *App) withTargetLock(ctx context.Context, exec Executor, project, target, command string, fn func() error) error {
	if a.NoLock {
		return fn()
	}
	release, err := acquireLock(ctx, exec, project, target, command, a.ForceUnlock)
	if err != nil {
		return err
	}
	if a.ForceUnlock {
		fmt.Fprintf(a.Stdout, "  %s overriding existing deploy lock on %s\n", yellow("warning"), bold(target))
	}
	defer release()
	return fn()
}

// runHook executes a deploy lifecycle hook command if set.
func (a *App) runHook(ctx context.Context, exec Executor, phase, scope, cmd string) error {
	if cmd == "" {
		return nil
	}
	sp := startSpinner(a.Stdout, fmt.Sprintf("hook: %s %s", phase, scope))
	if _, err := exec.Run(ctx, cmd); err != nil {
		sp.stop()
		return fmt.Errorf("hook %s %s: %w", phase, scope, err)
	}
	sp.stop()
	return nil
}

// Deploy performs idempotent deployment for one or all targets.
func (a *App) Deploy(ctx context.Context, cfg ProjectConfig, targetName string, cliServices []string, rebuild bool) error {
	a.applyConfig(cfg)
	InitColor(a.Stdout)
	targets := targetOrder(cfg, targetName)
	for _, name := range targets {
		fmt.Fprintf(a.Stdout, "%s resolving target %s\n", boldCyan("[deploy]"), bold(name))
		eff, err := resolveTarget(cfg, name, cliServices)
		if err != nil {
			return err
		}
		exec, err := a.ExecFactory.ForTarget(eff.Target)
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		defer exec.Close()
		var changed []string
		err = a.withTargetLock(ctx, exec, cfg.Name, name, "deploy", func() error {
			if err := a.runHook(ctx, exec, "pre_deploy", "project", cfg.Hooks.PreDeploy); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "%s syncing repo and directories on %s\n", boldCyan("[deploy]"), dim(eff.Target.Host))
			uploaded, err := a.ensureRepoAndDirs(ctx, cfg, eff, exec, rebuild)
			if err != nil {
				return err
			}
			if err := a.runHook(ctx, exec, "pre_build", "project", cfg.Hooks.PreBuild); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "%s ensuring images on %s\n", boldCyan("[deploy]"), dim(eff.Target.Host))
			changed, err = a.ensureImages(ctx, cfg, eff, exec, false, rebuild)
			if err != nil {
				return err
			}
			if err := a.runHook(ctx, exec, "post_build", "project", cfg.Hooks.PostBuild); err != nil {
				return err
			}
			if len(uploaded) > 0 {
				a.cleanupUploadedSource(ctx, exec, uploaded)
			}
			fmt.Fprintf(a.Stdout, "%s installing quadlet files and starting services on %s\n", boldCyan("[deploy]"), dim(eff.Target.Host))
			if err := a.installAndStart(ctx, cfg, eff, exec, false, changed, len(cliServices) == 0); err != nil {
				a.attemptAutoRollback(ctx, cfg, eff, exec)
				return err
			}
			// Save release before post_deploy hook so a successful deploy is always recorded
			releaseImages, err := releaseImagesForDeploy(ctx, exec, cfg.Name, eff.Services, len(cliServices) == 0)
			if err != nil {
				fmt.Fprintf(a.Stdout, "  %s release snapshot: %s\n", yellow("warning"), err)
				releaseImages = releaseImagesFromServices(eff.Services)
			}
			rel := newReleaseFromImages(releaseImages)
			if err := saveRelease(ctx, exec, cfg.Name, rel); err != nil {
				fmt.Fprintf(a.Stdout, "  %s save release: %s\n", yellow("warning"), err)
			}
			trimReleases(ctx, exec, cfg.Name)
			return a.runHook(ctx, exec, "post_deploy", "project", cfg.Hooks.PostDeploy)
		})
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}

		if len(changed) > 0 {
			fmt.Fprintf(a.Stdout, "%s target %s %s; updated: %s\n", boldGreen("deployed"), bold(name), dim("("+eff.Target.Host+")"), strings.Join(changed, ", "))
		} else {
			fmt.Fprintf(a.Stdout, "target %s %s %s\n", bold(name), dim("("+eff.Target.Host+")"), green("is up to date"))
		}
	}
	return nil
}

// Build ensures images exist for selected services without restarting units.
func (a *App) Build(ctx context.Context, cfg ProjectConfig, targetName string, cliServices []string, rebuild bool) error {
	a.applyConfig(cfg)
	InitColor(a.Stdout)
	targets := targetOrder(cfg, targetName)
	for _, name := range targets {
		eff, err := resolveTarget(cfg, name, cliServices)
		if err != nil {
			return err
		}
		exec, err := a.ExecFactory.ForTarget(eff.Target)
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		defer exec.Close()
		var changed []string
		err = a.withTargetLock(ctx, exec, cfg.Name, name, "build", func() error {
			uploaded, err := a.ensureRepoAndDirs(ctx, cfg, eff, exec, rebuild)
			if err != nil {
				return err
			}
			if err := a.runHook(ctx, exec, "pre_build", "project", cfg.Hooks.PreBuild); err != nil {
				return err
			}
			changed, err = a.ensureImages(ctx, cfg, eff, exec, true, rebuild)
			if err != nil {
				return err
			}
			if err := a.runHook(ctx, exec, "post_build", "project", cfg.Hooks.PostBuild); err != nil {
				return err
			}
			if len(uploaded) > 0 {
				a.cleanupUploadedSource(ctx, exec, uploaded)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		fmt.Fprintf(a.Stdout, "%s target %s %s; changed images: %s\n", boldGreen("built"), bold(name), dim("("+eff.Target.Host+")"), strings.Join(changed, ", "))
	}
	return nil
}

// DeployConfigOnly updates only Quadlet files and proxy config without syncing source or rebuilding images.
// Use this when you changed env, expose, volumes, or other config without changing code.
func (a *App) DeployConfigOnly(ctx context.Context, cfg ProjectConfig, targetName string, cliServices []string) error {
	a.applyConfig(cfg)
	InitColor(a.Stdout)
	targets := targetOrder(cfg, targetName)
	for _, name := range targets {
		fmt.Fprintf(a.Stdout, "%s config-only deploy on %s\n", boldCyan("[deploy]"), bold(name))
		eff, err := resolveTarget(cfg, name, cliServices)
		if err != nil {
			return err
		}
		exec, err := a.ExecFactory.ForTarget(eff.Target)
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}
		defer exec.Close()

		err = a.withTargetLock(ctx, exec, cfg.Name, name, "deploy --config-only", func() error {
			if len(eff.Target.Dirs) > 0 {
				if _, err := exec.Run(ctx, fmt.Sprintf("mkdir -p %s", joinQuoted(eff.Target.Dirs))); err != nil {
					return err
				}
			}
			return a.installAndStart(ctx, cfg, eff, exec, false, nil, len(cliServices) == 0)
		})
		if err != nil {
			return fmt.Errorf("target %s: %w", name, err)
		}

		fmt.Fprintf(a.Stdout, "%s config updated on %s\n", boldGreen("deployed"), bold(name))
	}
	return nil
}

// targetOrder resolves target execution order from CLI scope.
func targetOrder(cfg ProjectConfig, targetName string) []string {
	if targetName != "" {
		return []string{targetName}
	}
	return sortedKeys(cfg.Targets)
}

// installAndStart writes Quadlet files, reloads systemd, and validates units.
//
// Routing: when the target's lifecycle resolves to "direct" (explicit
// `lifecycle: direct` or auto-detected on a target without systemctl), this
// dispatches to installAndStartDirect, which performs the same end goal
// using `podman run --restart=...` plus qqd.*
// labels. Otherwise the existing systemd-backed path runs unchanged.
func (a *App) installAndStart(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, targetExec Executor, firstInit bool, changedImages []string, fullDeploy bool) error {
	eff.Services = a.annotateVolumeOwnership(ctx, targetExec, eff.Services)

	sel := a.lifecycleFor(ctx, eff.Target, targetExec)
	if sel.Backend == "direct" {
		fmt.Fprintf(a.Stdout, "  %s lifecycle: %s (%s)\n", dim("backend"), bold("direct"), dim(sel.Reason))
		return a.installAndStartDirect(ctx, cfg, eff, targetExec, sel.Selected, firstInit, fullDeploy)
	}
	// Resolve full (unfiltered) service set for this target.
	fullEff, err := resolveTarget(cfg, eff.Target.Name, nil)
	if err != nil {
		return err
	}
	fullEff.Services = a.annotateVolumeOwnership(ctx, targetExec, fullEff.Services)
	allServices := fullEff.Services

	// For partial deploys, filter expose config to only include services that
	// are being deployed now or already running on the target.
	effectiveExpose := eff.Expose
	if !fullDeploy && hasExposedServices(eff.Expose) {
		runningServices := detectRunningServices(ctx, targetExec, cfg.Name, allServices, a.crt())
		activeServices := map[string]bool{}
		for name := range eff.Services {
			activeServices[name] = true
		}
		for name := range runningServices {
			activeServices[name] = true
		}
		effectiveExpose = filterExposeByServices(eff.Expose, activeServices)
	}
	qdDir := a.rt().UnitDir()

	// Identify zero-downtime slot-eligible services: HTTP-exposed + non-replicated + not first init.
	// Only HTTP-routed services qualify — TCP passthrough services (databases, metrics)
	// can't safely use slot-based deploys (stateful, long-lived connections).
	slottedSvcs := map[string]bool{}
	if !firstInit {
		for name, svc := range allServices {
			if isServiceHTTPExposed(name, eff.Expose) && !isReplicated(svc) {
				slottedSvcs[name] = true
			}
		}
	}

	// Only exclude slotted services from standard rendering if they already have
	// an active slot on the target. Services without a slot still need their standard
	// quadlet so that depends_on references and systemd dependencies work correctly.
	activeSlots := map[string]string{} // service → active slot hash
	if len(slottedSvcs) > 0 {
		for name := range slottedSvcs {
			slot := detectActiveSlot(ctx, targetExec, cfg.Name, name, qdDir, a.rt().UnitExtension())
			if slot != "" {
				activeSlots[name] = slot
			}
		}
	}

	// Clean up stale slot files and orphaned containers left from previous
	// deploys (e.g. from the old race between stop and daemon-reload).
	a.cleanStaleSlots(ctx, cfg.Name, qdDir, slottedSvcs, activeSlots, targetExec)

	standardServices := map[string]ServiceConfig{}
	for name, svc := range eff.Services {
		if slottedSvcs[name] && activeSlots[name] != "" {
			continue // has active slot — managed by slotDeploy
		}
		standardServices[name] = svc
	}
	// Rewrite DependsOn for services whose dependencies have active slots.
	// E.g. if "server" has slot "a1b2c3d4", mcp's dependency on "server" becomes "server-a1b2c3d4"
	// so the quadlet's After=/Requires= references the correct slot unit.
	for name, svc := range standardServices {
		standardServices[name] = rewriteDepsForSlots(svc, activeSlots)
	}

	// For proxy deps: skip services that have active slots (managed by slotDeploy)
	proxySkipDeps := map[string]bool{}
	for name := range activeSlots {
		proxySkipDeps[name] = true
	}

	files := renderQuadletFiles(cfg.Name, standardServices, allServices, effectiveExpose, a.proxy(), a.rt(), eff.Target.User, proxySkipDeps)

	// Read existing file content before writing (for config change detection)
	oldQuadlet := map[string]string{}
	if !firstInit {
		for _, f := range files {
			path := fmt.Sprintf("%s/%s", qdDir, f.Name)
			if out, err := targetExec.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null || true", path)); err == nil {
				oldQuadlet[f.Name] = out
			}
		}
	}

	// Read existing proxy static config
	var oldProxyStatic, proxyStatic string
	staticPath := a.proxy().StaticConfigPath(cfg.Name)
	dynamicDir := a.proxy().DynamicConfigDir(cfg.Name)
	dynamicPath := a.proxy().DynamicConfigPath(cfg.Name)
	if !firstInit && hasExposedServices(effectiveExpose) {
		if out, err := targetExec.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null || true", staticPath)); err == nil {
			oldProxyStatic = out
		}
	}

	// Write proxy config if any services are exposed
	if hasExposedServices(effectiveExpose) {
		if _, err := targetExec.Run(ctx, fmt.Sprintf("mkdir -p %s", dynamicDir)); err != nil {
			return err
		}
		proxyStatic = a.proxy().GenerateStaticConfig(cfg.Name, effectiveExpose)
		// Some providers (Caddy) intentionally have no static config; skip the
		// write when the generator returns empty so we don't litter the target
		// with an empty file or trigger spurious heredoc edge cases.
		if proxyStatic != "" && proxyStatic != oldProxyStatic {
			heredoc := fmt.Sprintf("cat > %s <<'QD_EOF'\n%sQD_EOF", staticPath, proxyStatic)
			if _, err := targetExec.Run(ctx, heredoc); err != nil {
				return err
			}
		}
		// Build dynamic config with slot overrides for any slot-based services
		// that already have an active slot on the target.
		dynamicOpts := DynamicConfigOpts{}
		if len(activeSlots) > 0 {
			dynamicOpts.SlotOverrides = map[string]string{}
			for svcName, slot := range activeSlots {
				dynamicOpts.SlotOverrides[svcName] = fmt.Sprintf("%s-%s-%s", cfg.Name, svcName, slot)
			}
		}
		dynamicConf := a.proxy().GenerateDynamicConfig(cfg.Name, allServices, effectiveExpose, dynamicOpts)
		if err := atomicWriteRemote(ctx, targetExec, dynamicPath, dynamicConf); err != nil {
			return err
		}
	}

	sudo := a.sudoPrefix()
	if _, err := targetExec.Run(ctx, fmt.Sprintf("%smkdir -p %s", sudo, qdDir)); err != nil {
		return err
	}
	// Only write quadlet files that have changed (or are new)
	var written int
	for _, f := range files {
		if old, ok := oldQuadlet[f.Name]; ok && old == f.Content {
			continue // unchanged
		}
		path := fmt.Sprintf("%s/%s", qdDir, f.Name)
		heredoc := fmt.Sprintf("%ssh -c 'cat > %s' <<'QD_EOF'\n%sQD_EOF", sudo, path, f.Content)
		if sudo == "" {
			heredoc = fmt.Sprintf("cat > %s <<'QD_EOF'\n%sQD_EOF", path, f.Content)
		}
		if _, err := targetExec.Run(ctx, heredoc); err != nil {
			return err
		}
		written++
	}
	if written > 0 {
		fmt.Fprintf(a.Stdout, "  writing %s quadlet files\n", dim(fmt.Sprintf("%d", written)))
	}

	// Remove stale quadlet files from previous deployments
	// (e.g., when replica count changes or service switches between replicated/non-replicated)
	if !firstInit {
		a.removeStaleQuadlets(ctx, cfg.Name, qdDir, files, eff.Services, targetExec, fullDeploy, slottedSvcs)
	}

	// Detect quadlet/static config changes.
	// Proxy restart is ONLY triggered by static config changes (entrypoints, ports).
	// Dynamic route config is auto-reloaded by the proxy's file watcher without a restart.
	// Proxy quadlet dep changes (After=/Wants=) are ignored — they're just startup ordering
	// hints and don't affect a running proxy instance.
	var configChanged []string
	if !firstInit {
		for _, f := range files {
			if old := oldQuadlet[f.Name]; old != "" && old != f.Content {
				if svc := serviceNameFromQuadlet(cfg.Name, f.Name); svc != "" {
					configChanged = append(configChanged, svc)
				}
			}
		}
		if hasExposedServices(effectiveExpose) && oldProxyStatic != "" && oldProxyStatic != proxyStatic {
			configChanged = append(configChanged, "__proxy__")
		}
	}

	sp := startSpinner(a.Stdout, "reloading systemd daemon")
	if _, err := targetExec.Run(ctx, a.sctl()+" daemon-reload"); err != nil {
		sp.stop()
		return err
	}
	sp.stop()

	// Build unit list — skip slotted services (they're managed by slotDeploy)
	units := []string{networkUnit(cfg.Name)}
	units = append(units, allContainerUnits(cfg.Name, standardServices)...)
	if hasExposedServices(effectiveExpose) {
		units = append(units, a.proxy().ServiceUnit(cfg.Name))
	}
	sp = startSpinner(a.Stdout, fmt.Sprintf("starting %d units", len(units)))
	if _, err := targetExec.Run(ctx, fmt.Sprintf(a.sctl()+" start %s", joinQuoted(units))); err != nil {
		sp.stop()
		a.diagnoseFailedUnits(ctx, cfg, eff, targetExec)
		return err
	}
	sp.stop()
	allChanged := mergeUnique(changedImages, configChanged)
	// Slotted services need slotDeploy when:
	// Slotted services with active slots: check if the slot's image differs from config.
	// Services without active slots are handled normally (standard quadlet was written above).
	for svcName, slot := range activeSlots {
		if contains(allChanged, svcName) {
			continue
		}
		if svc, ok := allServices[svcName]; ok {
			slotPath := fmt.Sprintf("%s/%s-%s-%s%s", qdDir, cfg.Name, svcName, slot, a.rt().UnitExtension())
			if out, err := targetExec.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null || true", slotPath)); err == nil {
				expected := renderExpectedSlotContent(cfg.Name, svcName, slot, svc, activeSlots, a.rt())
				if strings.TrimSpace(out) != strings.TrimSpace(expected) {
					allChanged = append(allChanged, svcName)
					continue
				}
			}
			// Also redeploy if the slot unit is not active (failed/inactive)
			slotUnit := fmt.Sprintf("%s-%s-%s.service", cfg.Name, svcName, slot)
			if state, err := targetExec.Run(ctx, fmt.Sprintf(a.sctl()+" is-active %s 2>/dev/null || true", shellQuote(slotUnit))); err == nil {
				if strings.TrimSpace(state) != "active" {
					allChanged = append(allChanged, svcName)
				}
			}
		}
	}
	var slotDeployed map[string]bool
	if !firstInit && len(allChanged) > 0 {
		var err error
		slotDeployed, err = a.restartChangedServices(ctx, cfg, eff, targetExec, allChanged, allServices, activeSlots)
		if err != nil {
			return err
		}
	}
	// Verify units — check standard services and slotted services
	sp = startSpinner(a.Stdout, "verifying units")
	for _, svcName := range sortedKeys(standardServices) {
		if slotDeployed[svcName] {
			continue
		}
		svc := standardServices[svcName]
		for _, unit := range serviceUnits(cfg.Name, svcName, svc) {
			if _, err := targetExec.Run(ctx, fmt.Sprintf(a.sctl()+" is-active %s", shellQuote(unit))); err != nil {
				sp.stop()
				a.diagnoseFailedUnits(ctx, cfg, eff, targetExec)
				return fmt.Errorf("unit %s is not active", unit)
			}
		}
	}
	// Also verify slotted services that have active slots
	for svcName, slot := range activeSlots {
		if slotDeployed[svcName] {
			// slot just deployed — verify the new slot unit
			newSlot := detectActiveSlot(ctx, targetExec, cfg.Name, svcName, qdDir, a.rt().UnitExtension())
			if newSlot == "" {
				continue
			}
			slot = newSlot
		}
		slotUnit := fmt.Sprintf("%s-%s-%s.service", cfg.Name, svcName, slot)
		if _, err := targetExec.Run(ctx, fmt.Sprintf(a.sctl()+" is-active %s", shellQuote(slotUnit))); err != nil {
			sp.stop()
			a.diagnoseFailedUnits(ctx, cfg, eff, targetExec)
			return fmt.Errorf("unit %s is not active", slotUnit)
		}
	}
	sp.stop()
	return nil
}

func (a *App) annotateVolumeOwnership(ctx context.Context, exec Executor, services map[string]ServiceConfig) map[string]ServiceConfig {
	annotated := make(map[string]ServiceConfig, len(services))
	for name, svc := range services {
		if len(svc.Volumes) > 0 {
			user := svc.User
			if strings.TrimSpace(user) == "" {
				if imageUser, ok := imageConfigUser(ctx, exec, svc.Image, a.rt()); ok {
					user = imageUser
				}
			}
			svc.volumeNeedsU = userNeedsVolumeOwnershipMapping(user)
		}
		annotated[name] = svc
	}
	return annotated
}

// attemptAutoRollback tries to restore the previous release after a deploy failure.
// Best-effort: if the rollback itself fails, the original deploy error is still
// returned to the caller. Skipped when no previous release exists or when the
// context was cancelled (user interrupted).
//
// Two cases:
//
//  1. Image-shape change between previous release and the failing one:
//     point each affected service back at the previous image and re-run
//     installAndStart with the rollback spec. The new container ends up
//     atomically replacing the broken one (under direct mode) or via
//     systemctl restart (under systemd mode).
//
//  2. No image change: the deploy still failed (infra issue, port conflict,
//     misconfiguration, etc.) and the install left the target in a possibly
//     half-applied state. Re-run installAndStart with the previous release's
//     spec so the last-known-good shape is restored. Without this we leave
//     the operator looking at a half-broken target with the original error
//     and no recourse beyond manual cleanup.
func (a *App) attemptAutoRollback(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, exec Executor) {
	if ctx.Err() != nil {
		return
	}

	rel, ok, err := latestRelease(ctx, exec, cfg.Name)
	if err != nil || !ok {
		fmt.Fprintf(a.Stdout, "  %s no previous release for auto-rollback\n", yellow("warning"))
		return
	}

	fmt.Fprintf(a.Stdout, "\n%s deploy failed, rolling back to %s\n", boldYellow("[rollback]"), bold(rel.ID))

	rollbackServices := make(map[string]ServiceConfig, len(eff.Services))
	var changed []string
	for _, svcName := range sortedKeys(eff.Services) {
		svc := eff.Services[svcName]
		prevImage, hasPrev := rel.Services[svcName]
		if hasPrev && svc.Image != prevImage {
			fmt.Fprintf(a.Stdout, "  %s %s -> %s\n", bold(svcName), dim(svc.Image), green(prevImage))
			svc.Image = prevImage
			changed = append(changed, svcName)
		}
		rollbackServices[svcName] = svc
	}

	if len(changed) == 0 {
		// No image rollback needed, but we still want to restore the
		// previous-known-good shape - the failed deploy may have left
		// half-installed state. Re-run installAndStart with the previous
		// release's spec; under direct mode this atomically rebuilds every
		// container with the last-good config.
		fmt.Fprintf(a.Stdout, "  %s\n", dim("no image changes; restoring previous container shape"))
		rollbackEff := eff
		rollbackEff.Services = rollbackServices
		// Pass the full service set as "changed" so installAndStart treats
		// every service as needing restart, not just the ones whose images
		// moved.
		all := sortedKeys(rollbackServices)
		if err := a.installAndStart(ctx, cfg, rollbackEff, exec, false, all, true); err != nil {
			fmt.Fprintf(a.Stdout, "  %s restore previous shape failed: %s\n", red("error"), err)
			return
		}
		fmt.Fprintf(a.Stdout, "%s previous container shape restored from release %s\n", boldGreen("rolled back"), bold(rel.ID))
		return
	}

	for _, svcName := range changed {
		svc := rollbackServices[svcName]
		exists, err := imageExists(ctx, exec, svc.Image, a.rt())
		if err != nil {
			fmt.Fprintf(a.Stdout, "  %s auto-rollback failed: %s\n", red("error"), err)
			return
		}
		if !exists {
			sp := startSpinner(a.Stdout, fmt.Sprintf("pulling %s", svc.Image))
			if err := exec.RunStream(ctx, fmt.Sprintf(a.crt()+" pull %s", shellQuote(svc.Image)), a.Stdout); err != nil {
				sp.stop()
				fmt.Fprintf(a.Stdout, "  %s auto-rollback pull %s: %s\n", red("error"), svc.Image, err)
				return
			}
			sp.stop()
		}
	}

	rollbackEff := eff
	rollbackEff.Services = rollbackServices
	if err := a.installAndStart(ctx, cfg, rollbackEff, exec, false, changed, true); err != nil {
		fmt.Fprintf(a.Stdout, "  %s auto-rollback failed: %s\n", red("error"), err)
		return
	}

	fmt.Fprintf(a.Stdout, "%s auto-rolled back to %s\n", boldGreen("rolled back"), bold(rel.ID))
}

// restartChangedServices restarts services whose images or config changed.
// Dispatch logic:
//   - exposed + non-replicated → slot-based deploy (zero downtime)
//   - exposed + replicated → rolling restart with drain (zero downtime)
//   - non-exposed + replicated + health → rolling restart (existing)
//   - otherwise → restart in place
//
// The special "__proxy__" sentinel triggers a proxy restart (only when static config changed).
func (a *App) restartChangedServices(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, exec Executor, changed []string, allServices map[string]ServiceConfig, activeSlots map[string]string) (map[string]bool, error) {
	slotDeployed := map[string]bool{}
	for _, name := range changed {
		if name == "__proxy__" {
			proxyUnit := a.proxy().ServiceUnit(cfg.Name)
			sp := startSpinner(a.Stdout, "restarting proxy (entrypoints changed)")
			exec.Run(ctx, fmt.Sprintf(a.sctl()+" restart %s", shellQuote(proxyUnit)))
			sp.stop()
			continue
		}
		svc, ok := eff.Services[name]
		if !ok {
			continue
		}
		httpExposed := isServiceHTTPExposed(name, eff.Expose)
		exposed := isServiceExposed(name, eff.Expose)
		replicated := isReplicated(svc)

		switch {
		case httpExposed && !replicated:
			// Slot-based deploy (HTTP-routed services only)
			if err := a.slotDeploy(ctx, cfg, eff, exec, name, svc, allServices, activeSlots); err != nil {
				return nil, err
			}
			slotDeployed[name] = true
		case exposed && replicated:
			// Rolling restart with drain
			if err := a.rollingRestartWithDrain(ctx, cfg, eff, exec, name, svc, allServices); err != nil {
				return nil, err
			}
		case replicated && svc.Health.Path != "":
			// Rolling restart (existing, no drain)
			fmt.Fprintf(a.Stdout, "  %s %s %s\n", yellow("rolling restart"), bold(name), dim(fmt.Sprintf("(%s, %d replicas)", imageTag(svc.Image), effectiveReplicas(svc))))
			if err := a.rollingRestart(ctx, cfg.Name, name, svc, exec); err != nil {
				return nil, err
			}
		default:
			// Direct restart
			units := serviceUnits(cfg.Name, name, svc)
			sp := startSpinner(a.Stdout, fmt.Sprintf("restarting %s (%s)", name, imageTag(svc.Image)))
			for _, unit := range units {
				if _, err := exec.Run(ctx, fmt.Sprintf(a.sctl()+" restart %s", shellQuote(unit))); err != nil {
					sp.stop()
					return nil, err
				}
			}
			sp.stop()
		}
	}
	return slotDeployed, nil
}
