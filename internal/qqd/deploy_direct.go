package qqd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// directContainerName is the canonical container name for a service under
// direct mode. It is target-scoped (`<project>-<target>-<service>[-N]`) so
// multiple targets that happen to share a single Podman machine
// (the macOS `host: "local"` case where alpha + bravo both land on the
// developer's machine) don't collide.
//
// Within a target's network, every container also gets a network-alias of
// the `<project>-<service>` form so existing service-to-service DNS names
// keep working.
func directContainerName(project, target, service string, replica int) string {
	if replica > 1 {
		return fmt.Sprintf("%s-%s-%s-%d", project, target, service, replica)
	}
	return fmt.Sprintf("%s-%s-%s", project, target, service)
}

// directNetworkName returns the Podman network name used for one target's
// containers under direct mode.
func directNetworkName(project, target string) string {
	return fmt.Sprintf("%s-%s", project, target)
}

// directProxyName is the proxy container name under direct mode.
func directProxyName(project, target string) string {
	return fmt.Sprintf("%s-%s-proxy", project, target)
}

// directProxyConfDir returns the target-scoped directory holding the
// running proxy's static and dynamic config. Targets that share a host
// (the macOS local case) MUST have separate config dirs - the proxy
// container is the only consumer of these files but two targets writing
// to one shared dir would let the second deploy stomp the first's routes.
func directProxyConfDir(project, target string) string {
	return fmt.Sprintf("~/.config/qqd/%s/%s", project, target)
}

func (a *App) installAndStartDirect(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, exec Executor, lc Lifecycle, firstInit bool, fullDeploy bool) error {
	fullEff, err := resolveTarget(cfg, eff.Target.Name, nil)
	if err != nil {
		return err
	}
	allServices := fullEff.Services

	effectiveExpose := eff.Expose
	if !fullDeploy && hasExposedServices(eff.Expose) {
		runningSvcs := directDetectRunningServices(ctx, exec, cfg.Name, eff.Target.Name, lc)
		active := map[string]bool{}
		for n := range eff.Services {
			active[n] = true
		}
		for n := range runningSvcs {
			active[n] = true
		}
		effectiveExpose = filterExposeByServices(eff.Expose, active)
	}

	// 3. Write the proxy config files at target-scoped paths so two
	//    targets sharing one host don't stomp each other's routes. The
	//    running proxy container mounts them via -v.
	if hasExposedServices(effectiveExpose) {
		if err := a.writeProxyConfigDirect(ctx, exec, cfg, eff.Target.Name, effectiveExpose, allServices); err != nil {
			return err
		}
	}

	// 4. Compute the deterministic deploy id used for labels. Stable per
	//    invocation so all containers from one deploy share it.
	deployID := releaseID()

	// 5. Pre-sweep BEFORE installing anything. Two reasons we can't wait
	//    until after install:
	//      - Legacy orphans (qqd.project=<X> with no qqd.target, from older
	//        qqd versions) hold ports the new target-scoped containers want.
	//        If we left them until step 8, install would fail with
	//        "Bind for 0.0.0.0:N failed: port is already allocated".
	//      - Same for stale per-target containers whose ports moved (e.g.
	//        the user changed expose).
	//    Skipped on first init (nothing to sweep) and on partial deploys
	//    of services that aren't being touched (handled inside the sweep).
	if !firstInit {
		a.sweepStaleDirectContainers(ctx, exec, lc, cfg.Name, eff, fullDeploy)
	}

	// 6. Install service containers (and replicas). Pre-expand "~/..." in
	//    volume mounts against the target's $HOME.
	home := a.homeDirFor(ctx, eff.Target, exec)
	type started struct {
		container string
		image     string
		label     string
	}
	var startedList []started
	for _, name := range sortedKeys(eff.Services) {
		svc := eff.Services[name]
		if isReplicated(svc) {
			for i := 1; i <= effectiveReplicas(svc); i++ {
				cname := directContainerName(cfg.Name, eff.Target.Name, name, i)
				spec := buildContainerSpec(cfg, eff, name, svc, i, cname, deployID)
				expandSpecTildes(&spec, home)
				fmt.Fprintf(a.Stdout, "  %s %s %s\n", bold(name), dim(fmt.Sprintf("(replica %d)", i)), dim(spec.Image))
				if err := lc.Install(ctx, exec, spec); err != nil {
					return fmt.Errorf("install %s replica %d: %w", name, i, err)
				}
				startedList = append(startedList, started{container: cname, image: spec.Image, label: fmt.Sprintf("%s/%d", name, i)})
			}
		} else {
			cname := directContainerName(cfg.Name, eff.Target.Name, name, 1)
			spec := buildContainerSpec(cfg, eff, name, svc, 1, cname, deployID)
			expandSpecTildes(&spec, home)
			fmt.Fprintf(a.Stdout, "  %s %s\n", bold(name), dim(spec.Image))
			if err := lc.Install(ctx, exec, spec); err != nil {
				return fmt.Errorf("install %s: %w", name, err)
			}
			startedList = append(startedList, started{container: cname, image: spec.Image, label: name})
		}
	}

	// 7. Install the proxy container if anything is exposed.
	if hasExposedServices(effectiveExpose) {
		spec := buildProxyContainerSpec(cfg, eff, effectiveExpose, allServices, a.proxy(), deployID)
		expandSpecTildes(&spec, home)
		fmt.Fprintf(a.Stdout, "  %s %s\n", bold("proxy"), dim(spec.Image))
		if err := lc.Install(ctx, exec, spec); err != nil {
			return fmt.Errorf("install proxy: %w", err)
		}
		startedList = append(startedList, started{container: spec.Container, image: spec.Image, label: "proxy"})
	}

	sp := startSpinner(a.Stdout, fmt.Sprintf("verifying %d containers", len(startedList)))
	for _, s := range startedList {
		if err := waitContainerActive(ctx, exec, lc, s.container, 30*time.Second); err != nil {
			sp.stop()
			a.diagnoseDirectFailure(ctx, exec, s.container, s.label)
			return err
		}
	}
	sp.stop()

	return nil
}

// buildContainerSpec turns a (cfg, service, replica) tuple into the spec
// the direct backend feeds into `podman run`. Container names are scoped
// by target so multiple targets sharing one runtime daemon don't collide;
// network aliases preserve the `<project>-<service>` DNS name so
// existing user envs that reference services by short name still resolve.
func buildContainerSpec(cfg ProjectConfig, eff EffectiveTarget, svcName string, svc ServiceConfig, replica int, cname, deployID string) ContainerSpec {
	hash := configHash(svc)
	aliases := []string{containerName(cfg.Name, svcName)}
	if isReplicated(svc) {
		// Replicas additionally advertise their replica-suffixed name so
		// peer-discovery code that hits "<project>-<svc>-N" still works.
		aliases = append(aliases, fmt.Sprintf("%s-%s-%d", cfg.Name, svcName, replica))
	}
	return ContainerSpec{
		Project:    cfg.Name,
		Target:     eff.Target.Name,
		Service:    svcName,
		Replica:    replica,
		Container:  cname,
		Network:    directNetworkName(cfg.Name, eff.Target.Name),
		Aliases:    aliases,
		Image:      svc.Image,
		Env:        svc.Env,
		Volumes:    svc.Volumes,
		User:       svc.User,
		Command:    svc.Command,
		Health:     svc.Health,
		Resources:  svc.Resources,
		DependsOn:  svc.DependsOn,
		Role:       "app",
		DeployID:   deployID,
		ConfigHash: hash,
		Runtime:    cfg.Runtime,
	}
}

// buildProxyContainerSpec produces the spec for the Traefik/Caddy proxy
// container under direct mode. Mirrors the layout that
// renderProxyContainer assembles in systemd mode.
func buildProxyContainerSpec(cfg ProjectConfig, eff EffectiveTarget, expose ExposeConfig, services map[string]ServiceConfig, proxy ProxyProvider, deployID string) ContainerSpec {
	cname := directProxyName(cfg.Name, eff.Target.Name)
	confDir := directProxyConfDir(cfg.Name, eff.Target.Name)

	var ports []string
	published := map[int]bool{}
	if expose.Dashboard > 0 {
		api := traefikAPIPort(expose)
		ports = append(ports, fmt.Sprintf("%d:%d", expose.Dashboard, api))
		published[expose.Dashboard] = true
	}
	for _, e := range expose.Entries {
		if !published[e.HostPort] {
			ports = append(ports, fmt.Sprintf("%d:%d", e.HostPort, e.HostPort))
			published[e.HostPort] = true
		}
		if e.TLS != nil && e.TLS.Port > 0 && !published[e.TLS.Port] {
			ports = append(ports, fmt.Sprintf("%d:%d", e.TLS.Port, e.TLS.Port))
			published[e.TLS.Port] = true
		}
	}

	var vols []string
	tlsSeen := map[string]bool{}
	for _, e := range expose.Entries {
		if e.TLS != nil && e.TLS.CertsDir != "" && !tlsSeen[e.TLS.CertsDir] {
			tlsSeen[e.TLS.CertsDir] = true
			vols = append(vols, fmt.Sprintf("%s:%s:ro", e.TLS.CertsDir, traefikTLSMountPath(e.TLS.CertsDir)))
		}
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Proxy)) {
	case "caddy":
		vols = append(vols, fmt.Sprintf("%s/caddy-routes/Caddyfile:/etc/caddy/Caddyfile:ro", confDir))
	default: // traefik
		vols = append(vols, fmt.Sprintf("%s/traefik.yml:/etc/traefik/traefik.yml:ro", confDir))
		vols = append(vols, fmt.Sprintf("%s/dynamic:/etc/traefik/dynamic:ro", confDir))
	}

	image := proxyImageForCfg(cfg, proxy)

	return ContainerSpec{
		Project:   cfg.Name,
		Target:    eff.Target.Name,
		Service:   "proxy",
		Replica:   1,
		Container: cname,
		Network:   directNetworkName(cfg.Name, eff.Target.Name),
		Image:     image,
		Ports:     ports,
		Volumes:   vols,
		Role:      "proxy",
		DeployID:  deployID,
		Runtime:   cfg.Runtime,
	}
}

func proxyImageForCfg(cfg ProjectConfig, p ProxyProvider) string {
	if cfg.ProxyImage != "" {
		return cfg.ProxyImage
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Proxy)) {
	case "caddy":
		return "docker.io/library/caddy:2-alpine"
	default:
		return "docker.io/library/traefik:v3.6"
	}
}

// configHash hashes the service config bits that affect the running container,
// so the qqd.config_hash label changes when those bits change.
func configHash(svc ServiceConfig) string {
	h := sha256.New()
	fmt.Fprintf(h, "image=%s\n", svc.Image)
	fmt.Fprintf(h, "user=%s\n", svc.User)
	fmt.Fprintf(h, "command=%s\n", strings.Join(svc.Command, " "))
	fmt.Fprintf(h, "deps=%s\n", strings.Join(svc.DependsOn, ","))
	for _, k := range sortedKeys(svc.Env) {
		fmt.Fprintf(h, "env.%s=%s\n", k, svc.Env[k])
	}
	for _, v := range svc.Volumes {
		fmt.Fprintf(h, "vol=%s\n", v)
	}
	fmt.Fprintf(h, "health=%s:%d\n", svc.Health.Path, svc.Health.Port)
	fmt.Fprintf(h, "cpu=%s,mem=%s\n", svc.Resources.CPUs, svc.Resources.Memory)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// writeProxyConfigDirect writes the static + dynamic proxy config files at
// the *target-scoped* paths the running proxy container reads (mounted via
// -v). The proxy provider's StaticConfigPath/DynamicConfigPath are
// project-scoped only and would be shared between two targets sharing one
// host - so we layer the target dir on top to keep alpha and bravo's
// routes from stomping each other.
//
// Layout written:
//
//	~/.config/qqd/<project>/<target>/traefik.yml
//	~/.config/qqd/<project>/<target>/dynamic/routes.yml      (Traefik)
//	~/.config/qqd/<project>/<target>/caddy-routes/Caddyfile  (Caddy)
func (a *App) writeProxyConfigDirect(ctx context.Context, exec Executor, cfg ProjectConfig, target string, expose ExposeConfig, allServices map[string]ServiceConfig) error {
	confDir := directProxyConfDir(cfg.Name, target)
	staticPath := directProxyStaticPath(cfg, target)
	dynamicDir, dynamicPath := directProxyDynamicPaths(cfg, target)
	if _, err := exec.Run(ctx, fmt.Sprintf("mkdir -p %s", dynamicDir)); err != nil {
		return err
	}
	if static := a.proxy().GenerateStaticConfig(cfg.Name, expose); static != "" {
		// Traefik's static config references "/etc/traefik/dynamic" which is
		// the in-container path the proxy spec mounts to. The on-host path
		// is target-scoped; the in-container path stays the same so the
		// generated static doesn't need rewriting.
		heredoc := fmt.Sprintf("cat > %s <<'QD_EOF'\n%sQD_EOF", staticPath, static)
		if _, err := exec.Run(ctx, heredoc); err != nil {
			return err
		}
	}
	dyn := a.proxy().GenerateDynamicConfig(cfg.Name, allServices, expose, DynamicConfigOpts{})
	_ = confDir
	return atomicWriteRemote(ctx, exec, dynamicPath, dyn)
}

// directProxyStaticPath returns the target-scoped static-config file path
// the proxy container mounts at /etc/<proxy>/<file>.
func directProxyStaticPath(cfg ProjectConfig, target string) string {
	confDir := directProxyConfDir(cfg.Name, target)
	switch strings.ToLower(strings.TrimSpace(cfg.Proxy)) {
	case "caddy":
		// Caddy provider returns "" for GenerateStaticConfig today, but for
		// future-proofing we still pick a sane path.
		return confDir + "/Caddyfile.static"
	default:
		return confDir + "/traefik.yml"
	}
}

// directProxyDynamicPaths returns the target-scoped dynamic-config dir and
// the file inside it that GenerateDynamicConfig writes to.
func directProxyDynamicPaths(cfg ProjectConfig, target string) (dir, file string) {
	confDir := directProxyConfDir(cfg.Name, target)
	switch strings.ToLower(strings.TrimSpace(cfg.Proxy)) {
	case "caddy":
		return confDir + "/caddy-routes", confDir + "/caddy-routes/Caddyfile"
	default:
		return confDir + "/dynamic", confDir + "/dynamic/routes.yml"
	}
}

func directDetectRunningServices(ctx context.Context, exec Executor, project, target string, lc Lifecycle) map[string]bool {
	out := map[string]bool{}
	var statuses []UnitStatus
	var err error
	if dl, ok := lc.(directLifecycle); ok {
		statuses, err = dl.listForTarget(ctx, exec, project, target)
	} else {
		statuses, err = lc.List(ctx, exec, project)
	}
	if err != nil {
		return out
	}
	for _, st := range statuses {
		if st.Target != "" && st.Target != target {
			continue
		}
		if st.Service != "" && st.State == "active" {
			out[st.Service] = true
		}
	}
	return out
}

// waitContainerActive polls Status until the container reports "active" or
// the timeout expires. Bails fast (no further polling) when the container
// settles into a terminal state (inactive/failed/missing) so we don't wait
// 30s for something that already crashed. Returns a friendly error
// containing the last observed state and exit code if available.
func waitContainerActive(ctx context.Context, exec Executor, lc Lifecycle, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last UnitStatus
	settledMisses := 0
	for time.Now().Before(deadline) {
		st, err := lc.Status(ctx, exec, name)
		if err == nil && st.State == "active" {
			return nil
		}
		last = st
		// "missing" right after install can happen briefly while Podman
		// records the container; only treat it as terminal after we've seen
		// it twice in a row to avoid a race.
		if st.State == "missing" {
			settledMisses++
			if settledMisses >= 2 {
				return fmt.Errorf("container %s never appeared (last state: missing)", name)
			}
		} else {
			settledMisses = 0
		}
		// Terminal states: stop polling.
		if st.State == "inactive" || st.State == "failed" {
			if st.ExitCode != 0 {
				return fmt.Errorf("container %s is %s (exit code %d)", name, st.State, st.ExitCode)
			}
			return fmt.Errorf("container %s is %s", name, st.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if last.State == "" {
		return fmt.Errorf("container %s did not become active within %s", name, timeout)
	}
	return fmt.Errorf("container %s did not become active within %s (last state: %s)", name, timeout, last.State)
}

// diagnoseDirectFailure tails the last few log lines from a container that
// failed to become active, so the operator immediately sees why it
// crashed instead of having to manually run `podman logs`.
func (a *App) diagnoseDirectFailure(ctx context.Context, exec Executor, container, label string) {
	out, err := exec.Run(ctx, fmt.Sprintf("podman logs --tail 40 %s 2>&1 || true", shellQuote(container)))
	if err != nil || strings.TrimSpace(out) == "" {
		return
	}
	fmt.Fprintf(a.Stdout, "\n%s last 40 log lines from %s:\n", red("error"), bold(label))
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		fmt.Fprintf(a.Stdout, "  %s\n", dim(line))
	}
}

// expandSpecTildes resolves leading "~/..." (and bare "~") in the host
// portion of every volume mount on spec, against the supplied home dir.
// No-op for absolute or relative-without-tilde paths.
func expandSpecTildes(spec *ContainerSpec, home string) {
	if home == "" || spec == nil {
		return
	}
	for i, v := range spec.Volumes {
		// Volume format: "<source>:<target>[:opts]". Only expand the source.
		idx := strings.Index(v, ":")
		if idx <= 0 {
			continue
		}
		src := v[:idx]
		rest := v[idx:]
		spec.Volumes[i] = expandTildeAt(src, home) + rest
	}
}

// sweepStaleDirectContainers removes qqd-labeled containers that don't have
// a counterpart in the current desired service set.
//
// Two passes:
//
//  1. Per-target stale containers (qqd.project=<X>, qqd.target=<this target>):
//     containers from a previous deploy of this same target that are no
//     longer in the desired set (e.g. replica count dropped, service was
//     removed from the target's services list).
//
//  2. Legacy orphans (qqd.project=<X>, no qqd.target): containers from an
//     earlier qqd version that pre-dates target-scoped naming. These were
//     created with the old `<project>-<service>` shape and would now hold
//     ports / DNS aliases that the new target-scoped containers want.
//     Sweeping them is required for a clean upgrade. Each target's deploy
//     independently does this; whichever runs first cleans up.
//
// Containers belonging to a *different* known target are explicitly skipped
// so two targets sharing one runtime daemon don't sweep each other.
func (a *App) sweepStaleDirectContainers(ctx context.Context, exec Executor, lc Lifecycle, project string, eff EffectiveTarget, fullDeploy bool) {
	target := eff.Target.Name
	want := map[string]bool{}
	want[directProxyName(project, target)] = true
	for name, svc := range eff.Services {
		if isReplicated(svc) {
			for i := 1; i <= effectiveReplicas(svc); i++ {
				want[directContainerName(project, target, name, i)] = true
			}
		} else {
			want[directContainerName(project, target, name, 1)] = true
		}
	}
	// List ALL containers labeled with this project (across targets) so we
	// can also catch legacy orphans without a qqd.target label.
	statuses, err := lc.List(ctx, exec, project)
	if err != nil {
		return
	}
	for _, st := range statuses {
		if st.Name == "" || want[st.Name] {
			continue
		}
		// Skip containers that belong to a different known target. Their
		// owning target's own deploy will sweep them.
		if st.Target != "" && st.Target != target {
			continue
		}
		// At this point the container is either ours (qqd.target == target)
		// or a legacy orphan (qqd.target unset). Both should be removed,
		// with one carve-out:
		//
		// On a partial deploy, only touch services explicitly being
		// deployed now - we don't want `qqd deploy -t alpha api` to kill
		// alpha's frontend just because frontend isn't in the partial.
		// Legacy orphans (no qqd.target) are still removed because the
		// caller can't be ambiguous about wanting them gone.
		if !fullDeploy && st.Target == target && st.Service != "" {
			if _, ok := eff.Services[st.Service]; !ok {
				continue
			}
		}
		if st.Target == "" {
			fmt.Fprintf(a.Stdout, "  %s removing legacy container %s (no qqd.target label)\n", yellow("warning"), bold(st.Name))
		}
		_ = lc.Remove(ctx, exec, st.Name)
	}
}
