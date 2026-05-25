package qqd

import (
	"context"
	"fmt"
	"strings"
)

// MigrateComposeOpts holds options for migrating from docker-compose/swarm to qqd.
type MigrateComposeOpts struct {
	CfgPaths  []string // qqd config file(s)
	Target    string
	StackName string // docker stack name (if migrating from swarm)
	Runtime   string // target runtime; only "podman" is supported
}

// MigrateCompose stops a running docker-compose or swarm stack and deploys with qqd.
// It preserves images by transferring them from Docker to the target runtime,
// fixes volume ownership for rootless runtimes, and deploys fresh with qqd.
func (a *App) MigrateCompose(ctx context.Context, cfg ProjectConfig, opts MigrateComposeOpts) error {
	a.applyConfig(cfg)
	InitColor(a.Stdout)

	targets := targetOrder(cfg, opts.Target)
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

		if a.DryRun {
			fmt.Fprintf(a.Stdout, "%s %s — destructive actions are PRINTED, not executed\n",
				boldCyan("[migrate-compose dry-run]"), bold(name))
		}
		fmt.Fprintf(a.Stdout, "%s migrating compose/swarm to qqd on %s\n",
			boldCyan("[migrate-compose]"), bold(name))

		// 1. Detect running compose/swarm stack
		stackName := opts.StackName
		if stackName == "" {
			stackName = cfg.Name
		}

		// Check for Docker Swarm stack
		sp := startSpinner(a.Stdout, "detecting running stack")
		swarmState, _ := exec.Run(ctx, "docker info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null")
		isSwarm := strings.TrimSpace(swarmState) == "active"

		var composeContainers []string
		if isSwarm {
			// List swarm stack services
			out, _ := exec.Run(ctx, fmt.Sprintf("docker stack services %s --format '{{.Name}}' 2>/dev/null", shellQuote(stackName)))
			for _, svc := range strings.Split(strings.TrimSpace(out), "\n") {
				if strings.TrimSpace(svc) != "" {
					composeContainers = append(composeContainers, strings.TrimSpace(svc))
				}
			}
			fmt.Fprintf(a.Stdout, "  detected Docker Swarm stack %q with %d services\n", stackName, len(composeContainers))
		} else {
			// List docker-compose containers
			out, _ := exec.Run(ctx, fmt.Sprintf("docker ps --filter 'label=com.docker.compose.project=%s' --format '{{.Names}}' 2>/dev/null", shellQuote(stackName)))
			for _, c := range strings.Split(strings.TrimSpace(out), "\n") {
				if strings.TrimSpace(c) != "" {
					composeContainers = append(composeContainers, strings.TrimSpace(c))
				}
			}
			fmt.Fprintf(a.Stdout, "  detected Docker Compose project %q with %d containers\n", stackName, len(composeContainers))
		}
		sp.stop()

		if len(composeContainers) == 0 {
			fmt.Fprintf(a.Stdout, "  %s no running compose/swarm containers found for %q\n", yellow("warning"), stackName)
		}

		// 2. Transfer images from Docker to Podman
		targetRT := a.rt()
		sp = startSpinner(a.Stdout, "transferring images from Docker to Podman")
		for _, svcName := range sortedKeys(eff.Services) {
			svc := eff.Services[svcName]
			img := svc.Image

			// Check if already in Podman
			if _, err := exec.Run(ctx, targetRT.ImageExistsCmd(img)); err == nil {
				fmt.Fprintf(a.Stdout, "  %s: already in podman\n", dim(imageTag(img)))
				continue
			}

			// Check if in Docker
			if _, err := exec.Run(ctx, dockerImageExistsCmd(img)); err != nil {
				fmt.Fprintf(a.Stdout, "  %s: not in docker, will build\n", dim(imageTag(img)))
				continue
			}

			// Transfer Docker -> Podman via tar
			tarPath := fmt.Sprintf("/tmp/qqd-migrate-%s.tar", shellSafeImageName(img))
			if _, err := a.run(ctx, exec, fmt.Sprintf("docker save %s", img),
				fmt.Sprintf("docker save %s -o %s", shellQuote(img), shellQuote(tarPath))); err != nil {
				sp.stop()
				return fmt.Errorf("save image %s: %w", img, err)
			}
			if _, err := a.run(ctx, exec, fmt.Sprintf("podman load %s", img),
				fmt.Sprintf("podman load -i %s", shellQuote(tarPath))); err != nil {
				sp.stop()
				return fmt.Errorf("load image %s: %w", img, err)
			}
			a.run(ctx, exec, "remove migration tarball", fmt.Sprintf("rm -f %s", shellQuote(tarPath)))
			fmt.Fprintf(a.Stdout, "  %s: docker -> podman\n", bold(imageTag(img)))
		}
		sp.stop()

		// Confirmation gate (skipped on dry-run or --yes).
		if err := a.confirmComposeMigrateDestructive(name, isSwarm, stackName, targetRT, eff); err != nil {
			return err
		}

		// 3. Stop the compose/swarm stack
		sp = startSpinner(a.Stdout, "stopping compose/swarm stack")
		if isSwarm {
			a.run(ctx, exec, fmt.Sprintf("docker stack rm %s", stackName),
				fmt.Sprintf("docker stack rm %s 2>/dev/null || true", shellQuote(stackName)))
			if !a.DryRun {
				// Wait for services to drain
				for i := 0; i < 10; i++ {
					out, _ := exec.Run(ctx, fmt.Sprintf("docker stack services %s --format '{{.Name}}' 2>/dev/null", shellQuote(stackName)))
					if strings.TrimSpace(out) == "" {
						break
					}
					exec.Run(ctx, "sleep 2")
				}
			}
			a.run(ctx, exec, "leave Docker swarm (force)",
				"docker swarm leave --force 2>/dev/null || true")
		} else {
			// Stop the compose project by name. Works for both `docker compose` v2 and
			// the legacy `docker-compose` binary; both accept `-p <project>` and do not
			// require the original compose file to be reachable.
			a.run(ctx, exec, fmt.Sprintf("stop compose project %q (v2)", stackName),
				fmt.Sprintf("docker compose -p %s down 2>/dev/null || true", shellQuote(stackName)))
			a.run(ctx, exec, fmt.Sprintf("stop compose project %q (legacy)", stackName),
				fmt.Sprintf("docker-compose -p %s down 2>/dev/null || true", shellQuote(stackName)))
		}
		sp.stop()
		fmt.Fprintf(a.Stdout, "  %s stack stopped\n", green("compose/swarm"))

		// 4. Fix volume ownership for rootless Podman
		sp = startSpinner(a.Stdout, "fixing volume ownership for rootless podman")
		for _, svcName := range sortedKeys(eff.Services) {
			svc := eff.Services[svcName]
			for _, vol := range svc.Volumes {
				hostPath := strings.SplitN(vol, ":", 2)[0]
				if hostPath != "" && hostPath[0] == '/' {
					a.run(ctx, exec, fmt.Sprintf("chown -R $UID:$GID %s", hostPath),
						fmt.Sprintf("sudo chown -R $(id -u):$(id -g) %s 2>/dev/null || true", shellQuote(hostPath)))
				}
			}
		}
		for _, dir := range eff.Target.Dirs {
			a.run(ctx, exec, fmt.Sprintf("chown -R $UID:$GID %s", dir),
				fmt.Sprintf("sudo chown -R $(id -u):$(id -g) %s 2>/dev/null || true", shellQuote(dir)))
		}
		sp.stop()

		// 5. Clean up Docker networks from the compose/swarm stack
		sp = startSpinner(a.Stdout, "cleaning up Docker networks")
		a.run(ctx, exec, "docker network prune (force)", "docker network prune -f 2>/dev/null || true")
		sp.stop()

		// 6. Deploy with qqd
		if a.DryRun {
			fmt.Fprintf(a.Stdout, "  %s deploy with qqd (%s runtime) (skipped: dry-run)\n",
				dim("[would run]"), green(targetRT.Name()))
			fmt.Fprintf(a.Stdout, "%s dry-run complete for %s - no changes were made\n",
				boldGreen("dry-run"), bold(name))
			continue
		}
		fmt.Fprintf(a.Stdout, "%s deploying with qqd (%s runtime)\n",
			boldCyan("[migrate-compose]"), green(targetRT.Name()))

		if err := a.Init(ctx, cfg, name, nil, false); err != nil {
			return fmt.Errorf("target %s: qqd deploy failed: %w", name, err)
		}

		fmt.Fprintf(a.Stdout, "%s migrated from compose/swarm to qqd on %s\n",
			boldGreen("migrated"), bold(name))
	}
	return nil
}
