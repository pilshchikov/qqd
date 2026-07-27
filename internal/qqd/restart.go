package qqd

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// rollingRestart restarts replicas one at a time, waiting for health between each.
func (a *App) rollingRestart(ctx context.Context, project, service string, svc ServiceConfig, exec Executor) error {
	for i := 1; i <= effectiveReplicas(svc); i++ {
		unit := fmt.Sprintf("%s-%s-%d.service", project, service, i)
		cname := fmt.Sprintf("%s-%s-%d", project, service, i)
		sp := startSpinner(a.Stdout, fmt.Sprintf("  restarting replica %d", i))
		if _, err := exec.Run(ctx, fmt.Sprintf(a.sctl()+" restart %s", shellQuote(unit))); err != nil {
			sp.stop()
			return err
		}
		if err := a.waitForHealthy(ctx, exec, cname); err != nil {
			sp.stop()
			return fmt.Errorf("replica %d unhealthy after restart: %w", i, err)
		}
		sp.stop()
	}
	return nil
}

// rollingRestartWithDrain performs zero-downtime rolling restart for exposed, replicated services.
// For each replica: remove from proxy routes → drain → restart → wait ready → add back.
func (a *App) rollingRestartWithDrain(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, exec Executor, serviceName string, svc ServiceConfig, allServices map[string]ServiceConfig) error {
	project := cfg.Name
	dynamicPath := a.proxy().DynamicConfigPath(project)

	n := effectiveReplicas(svc)
	fmt.Fprintf(a.Stdout, "  rolling restart with drain %s %s\n", bold(serviceName), dim(fmt.Sprintf("(%s, %d replicas)", imageTag(svc.Image), n)))

	// A replica is pulled out of the proxy's route set before it is restarted.
	// If the restart or the readiness wait fails we must put the full set back,
	// otherwise the excluded replica stays out of the load balancer — silently
	// reducing capacity — until some later deploy rewrites the routes.
	restoreRoutes := func() {
		restoreCtx := ctx
		if ctx.Err() != nil {
			restoreCtx = context.Background()
		}
		fullConf := a.proxy().GenerateDynamicConfig(project, allServices, eff.Expose, DynamicConfigOpts{})
		if err := atomicWriteRemote(restoreCtx, exec, dynamicPath, fullConf); err != nil {
			fmt.Fprintf(a.Stdout, "  %s could not restore proxy routes: %s\n", yellow("warning"), err)
		}
	}

	for i := 1; i <= n; i++ {
		unit := fmt.Sprintf("%s-%s-%d.service", project, serviceName, i)
		cname := fmt.Sprintf("%s-%s-%d", project, serviceName, i)

		// 1. Rewrite routes.yml excluding this replica
		opts := DynamicConfigOpts{
			ExcludeReplicas: map[string]map[int]bool{
				serviceName: {i: true},
			},
		}
		dynamicConf := a.proxy().GenerateDynamicConfig(project, allServices, eff.Expose, opts)
		if err := atomicWriteRemote(ctx, exec, dynamicPath, dynamicConf); err != nil {
			return fmt.Errorf("exclude replica %d from routes: %w", i, err)
		}

		// 2. Drain wait
		drainWait := a.effectiveDrainWait()
		if drainWait > 0 {
			sp := startSpinner(a.Stdout, fmt.Sprintf("  draining replica %d", i))
			select {
			case <-time.After(drainWait):
			case <-ctx.Done():
			}
			sp.stop()
			if ctx.Err() != nil {
				restoreRoutes()
				return ctx.Err()
			}
		}

		// 3. Restart replica + wait for ready
		sp := startSpinner(a.Stdout, fmt.Sprintf("  restarting replica %d", i))
		if _, err := exec.Run(ctx, fmt.Sprintf(a.sctl()+" restart %s", shellQuote(unit))); err != nil {
			sp.stop()
			restoreRoutes()
			return err
		}

		// 4. Wait for ready
		if err := a.waitForReady(ctx, exec, cname, svc); err != nil {
			sp.stop()
			restoreRoutes()
			return fmt.Errorf("replica %d not ready after restart: %w", i, err)
		}
		sp.stop()

		// 5. Rewrite dynamic routes with all replicas back
		fullConf := a.proxy().GenerateDynamicConfig(project, allServices, eff.Expose, DynamicConfigOpts{})
		if err := atomicWriteRemote(ctx, exec, dynamicPath, fullConf); err != nil {
			return fmt.Errorf("restore replica %d to routes: %w", i, err)
		}
	}
	return nil
}

// waitForHealthy polls podman health status until the container reports "healthy".
func (a *App) waitForHealthy(ctx context.Context, exec Executor, containerName string) error {
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out, err := exec.Run(ctx, fmt.Sprintf(a.crt()+" inspect --format '{{.State.Health.Status}}' %s", shellQuote(containerName)))
		if err == nil && strings.TrimSpace(out) == "healthy" {
			return nil
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("container %s did not become healthy within 120s", containerName)
}

// waitForReady waits until a container is ready. Uses health check if configured,
// otherwise falls back to StartupDelay (default 5 seconds).
// When DrainWait is negative (test mode), the startup delay is skipped.
func (a *App) waitForReady(ctx context.Context, exec Executor, containerName string, svc ServiceConfig) error {
	if svc.Health.Path != "" && svc.Health.Port != 0 {
		return a.waitForHealthy(ctx, exec, containerName)
	}
	if a.DrainWait < 0 {
		return nil
	}
	delay := svc.StartupDelay
	if delay <= 0 {
		delay = 5
	}
	select {
	case <-time.After(time.Duration(delay) * time.Second):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
