package qqd

import (
	"context"
	"fmt"
	"strings"
)

// run wraps an executor command and respects dry-run mode. When DryRun is
// true, the command is printed (with a "[would run]" prefix) instead of
// executed, and the returned (output, err) is always ("", nil) so the caller's
// happy path proceeds. Use this for any side-effecting command in migrate.
func (a *App) run(ctx context.Context, exec Executor, label, cmd string) (string, error) {
	if a.DryRun {
		fmt.Fprintf(a.Stdout, "  %s %s\n  %s %s\n", dim("[would run]"), label, dim("$"), cmd)
		return "", nil
	}
	return exec.Run(ctx, cmd)
}

// dockerImageExistsCmd builds a shell test that returns success when the
// given image is present in the local docker daemon. Used by the Compose /
// Swarm migrators to decide whether to `docker save | podman load` an
// image before re-deploying with Podman.
func dockerImageExistsCmd(image string) string {
	return fmt.Sprintf("docker image inspect %s >/dev/null 2>&1", shellQuote(image))
}

// shellSafeImageName returns a filesystem-safe version of an image name for temp files.
func shellSafeImageName(image string) string {
	r := strings.NewReplacer("/", "_", ":", "_", ".", "_")
	return r.Replace(image)
}

// confirmComposeMigrateDestructive prints the destructive actions a
// Compose / Swarm migration will perform and asks the user for
// confirmation. Skipped when DryRun (the dry-run output already shows
// everything) or AssumeYes is set.
func (a *App) confirmComposeMigrateDestructive(targetName string, isSwarm bool, stackName string, to ContainerRuntime, eff EffectiveTarget) error {
	if a.DryRun || a.AssumeYes {
		return nil
	}
	fmt.Fprintf(a.Stdout, "\n%s the following destructive actions will be performed on %s:\n",
		yellow("warning"), bold(targetName))
	if isSwarm {
		fmt.Fprintf(a.Stdout, "  - docker stack rm %q\n", stackName)
		fmt.Fprintf(a.Stdout, "  - docker swarm leave --force  %s\n",
			yellow("(affects the entire swarm if this node is part of a multi-node cluster!)"))
	} else {
		fmt.Fprintf(a.Stdout, "  - docker compose -p %q down (v2 and legacy)\n", stackName)
	}
	fmt.Fprintf(a.Stdout, "  - run sudo chown -R $UID:$GID on every service volume host path\n")
	fmt.Fprintf(a.Stdout, "  - run sudo chown -R $UID:$GID on every target dir\n")
	fmt.Fprintf(a.Stdout, "  - docker network prune --force (removes ALL unused Docker networks)\n")
	fmt.Fprintf(a.Stdout, "  - re-deploy with qqd (%s runtime)\n", to.Name())
	fmt.Fprintf(a.Stdout, "\n%s these affect the WHOLE host, not just project %q.\n",
		yellow("warning"), eff.Target.Name)
	fmt.Fprintf(a.Stdout, "         Use --dry-run to preview without changes.\n\n")
	if !confirmPlan(a.Stdout) {
		return fmt.Errorf("aborted by user (use --yes to skip this prompt or --dry-run to preview)")
	}
	return nil
}
