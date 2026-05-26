package qqd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BuildStrategy handles image acquisition for a specific build approach.
// Implement this interface to add support for a custom build pipeline
// (e.g. GitLab CI, Jenkins, Nix, or a custom image registry workflow).
type BuildStrategy interface {
	EnsureImages(ctx context.Context, app *App, cfg ProjectConfig, eff EffectiveTarget, targetExec Executor, buildOnly, rebuild bool) ([]string, error)
}

// localBuildStrategy builds/pulls images directly on the target host.
type localBuildStrategy struct{}

func (localBuildStrategy) EnsureImages(ctx context.Context, app *App, cfg ProjectConfig, eff EffectiveTarget, targetExec Executor, _, rebuild bool) ([]string, error) {
	return app.ensureImagesLocal(ctx, cfg, eff, targetExec, rebuild)
}

// buildHostStrategy builds on a dedicated host and delivers to targets.
type buildHostStrategy struct{}

func (buildHostStrategy) EnsureImages(ctx context.Context, app *App, cfg ProjectConfig, eff EffectiveTarget, targetExec Executor, buildOnly, rebuild bool) ([]string, error) {
	return app.ensureImagesBuildHost(ctx, cfg, eff, targetExec, buildOnly, rebuild)
}

// githubActionsStrategy triggers a GitHub Actions workflow and pulls produced images.
type githubActionsStrategy struct{}

func (githubActionsStrategy) EnsureImages(ctx context.Context, app *App, cfg ProjectConfig, eff EffectiveTarget, targetExec Executor, buildOnly, _ bool) ([]string, error) {
	return app.ensureImagesGitHubActions(ctx, cfg, eff, targetExec, buildOnly)
}

// defaultBuildStrategies returns the built-in strategy registry.
func defaultBuildStrategies() map[string]BuildStrategy {
	return map[string]BuildStrategy{
		"local":          localBuildStrategy{},
		"build-host":     buildHostStrategy{},
		"github-actions": githubActionsStrategy{},
	}
}

// buildStrategy returns the strategy for a given name, checking App.BuildStrategies
// first (for user-registered strategies), then falling back to built-in defaults.
func (a *App) buildStrategy(name string) (BuildStrategy, error) {
	if name == "" {
		name = "local"
	}
	name = strings.ToLower(name)
	if a.BuildStrategies != nil {
		if s, ok := a.BuildStrategies[name]; ok {
			return s, nil
		}
	}
	defaults := defaultBuildStrategies()
	if s, ok := defaults[name]; ok {
		return s, nil
	}
	return nil, fmt.Errorf("unsupported build strategy %q", name)
}

// ensureImages dispatches image acquisition by configured strategy.
func (a *App) ensureImages(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, targetExec Executor, buildOnly, rebuild bool) ([]string, error) {
	s, err := a.buildStrategy(eff.Build.Strategy)
	if err != nil {
		return nil, err
	}
	return s.EnsureImages(ctx, a, cfg, eff, targetExec, buildOnly, rebuild)
}

// ensureImagesLocal builds/pulls missing images directly on target host.
// By default, existing images are skipped. Use rebuild=true to force rebuilding.
func (a *App) ensureImagesLocal(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, targetExec Executor, rebuild bool) ([]string, error) {
	root := projectRoot(eff.Target, cfg)
	var changed []string
	for _, name := range sortedKeys(eff.Services) {
		svc := eff.Services[name]
		mutable := isMutableTag(svc.Image)
		if svc.Dockerfile != "" {
			if a.NoBuild {
				fmt.Fprintf(a.Stdout, "  %s %s: %s\n", bold(name), dim("("+imageTag(svc.Image)+")"), dim("skipping build (--no-build)"))
				continue
			}
			if !rebuild && !mutable {
				exists, err := imageExists(ctx, targetExec, svc.Image, a.rt())
				if err != nil {
					return nil, err
				}
				if exists {
					fmt.Fprintf(a.Stdout, "  %s %s: %s\n", bold(name), dim("("+imageTag(svc.Image)+")"), green("exists, skipping (use --rebuild to force)"))
					continue
				}
			}
			if err := a.runHook(ctx, targetExec, "pre_build", name, svc.Hooks.PreBuild); err != nil {
				return nil, err
			}
			oldID := imageID(ctx, targetExec, svc.Image, a.rt())
			fmt.Fprintf(a.Stdout, "  %s %s: %s from %s\n", bold(name), dim("("+imageTag(svc.Image)+")"), yellow("building"), dim(svc.Dockerfile))
			cmd := buildImageCommand(root, svc, eff.Build, a.rt())
			if err := targetExec.RunStream(ctx, cmd, a.Stdout); err != nil {
				return nil, err
			}
			newID := imageID(ctx, targetExec, svc.Image, a.rt())
			if oldID != newID {
				fmt.Fprintf(a.Stdout, "  %s %s: %s\n", bold(name), dim("("+imageTag(svc.Image)+")"), yellow("image changed"))
				changed = append(changed, name)
			} else {
				fmt.Fprintf(a.Stdout, "  %s %s: %s\n", bold(name), dim("("+imageTag(svc.Image)+")"), green("image unchanged"))
			}
			if err := a.runHook(ctx, targetExec, "post_build", name, svc.Hooks.PostBuild); err != nil {
				return nil, err
			}
		} else {
			if !mutable {
				sp := startSpinner(a.Stdout, fmt.Sprintf("checking %s (%s)", name, svc.Image))
				exists, err := imageExists(ctx, targetExec, svc.Image, a.rt())
				sp.stop()
				if err != nil {
					return nil, err
				}
				if exists {
					fmt.Fprintf(a.Stdout, "  %s %s: %s\n", bold(name), dim("("+imageTag(svc.Image)+")"), green("exists, skipping"))
					continue
				}
			}
			if err := a.runHook(ctx, targetExec, "pre_build", name, svc.Hooks.PreBuild); err != nil {
				return nil, err
			}
			fmt.Fprintf(a.Stdout, "  %s: %s %s\n", bold(name), yellow("pulling"), dim(svc.Image))
			if err := targetExec.RunStream(ctx, fmt.Sprintf(a.crt()+" pull %s", shellQuote(svc.Image)), a.Stdout); err != nil {
				return nil, err
			}
			changed = append(changed, name)
			if err := a.runHook(ctx, targetExec, "post_build", name, svc.Hooks.PostBuild); err != nil {
				return nil, err
			}
		}
	}
	return changed, nil
}

// ensureImagesBuildHost builds on a dedicated host and delivers to targets.
func (a *App) ensureImagesBuildHost(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, targetExec Executor, buildOnly, rebuild bool) ([]string, error) {
	buildCfg := eff.Build
	if buildCfg.Host == "" || buildCfg.User == "" || buildCfg.RepoDir == "" {
		return nil, fmt.Errorf("build-host strategy requires host, user, repo_dir")
	}
	buildExec, err := a.ExecFactory.ForBuildHost(buildCfg)
	if err != nil {
		return nil, fmt.Errorf("build host: %w", err)
	}
	defer buildExec.Close()
	buildBranch := cfg.Branch
	if buildCfg.Branch != "" {
		buildBranch = buildCfg.Branch
	}
	ghTokenRaw := expandVars(cfg.GHToken, eff.Target.Env)
	ghToken, err := resolveGHToken(ctx, a.ExecFactory.Local(), ghTokenRaw)
	if err != nil {
		return nil, err
	}
	buildRepo := injectGHToken(cfg.Repo, ghToken)
	if buildCfg.Repo != "" {
		buildRepo = buildCfg.Repo
	}
	sp := startSpinner(a.Stdout, "syncing build host repo")
	if _, err := buildExec.Run(ctx, fmt.Sprintf("mkdir -p %s", shellQuote(buildCfg.RepoDir))); err != nil {
		sp.stop()
		return nil, err
	}
	cloneCmd := fmt.Sprintf("if [ ! -d %s/.git ]; then git clone --single-branch --branch %s %s %s; fi",
		shellQuote(buildCfg.RepoDir), shellQuote(buildBranch), shellQuote(buildRepo), shellQuote(buildCfg.RepoDir))
	if _, err := buildExec.Run(ctx, cloneCmd); err != nil {
		sp.stop()
		return nil, err
	}
	if _, err := buildExec.Run(ctx, fmt.Sprintf("cd %s && git fetch --all && git reset --hard origin/%s", shellQuote(buildCfg.RepoDir), shellQuote(buildBranch))); err != nil {
		sp.stop()
		return nil, err
	}
	sp.stop()
	root := buildCfg.RepoDir
	if cfg.Path != "" {
		root = filepath.ToSlash(filepath.Join(root, cfg.Path))
	}
	var changed []string
	delivery := buildCfg.Delivery
	if delivery == "" {
		delivery = "direct"
	}
	if delivery == "registry" && buildCfg.Registry != "" && buildCfg.RegistryUser != "" && buildCfg.RegistryToken != "" {
		loginCmd := fmt.Sprintf("printf %%s %s | "+a.crt()+" login %s -u %s --password-stdin",
			shellQuote(buildCfg.RegistryToken), shellQuote(buildCfg.Registry), shellQuote(buildCfg.RegistryUser))
		if _, err := buildExec.Run(ctx, loginCmd); err != nil {
			return nil, err
		}
		if !buildOnly {
			if _, err := targetExec.Run(ctx, loginCmd); err != nil {
				return nil, err
			}
		}
	}
	for _, name := range sortedKeys(eff.Services) {
		svc := eff.Services[name]
		mutable := isMutableTag(svc.Image)
		if svc.Dockerfile == "" {
			if !buildOnly {
				needsPull := mutable
				if !needsPull {
					exists, err := imageExists(ctx, targetExec, svc.Image, a.rt())
					if err != nil {
						return nil, err
					}
					needsPull = !exists
				}
				if needsPull {
					if err := targetExec.RunStream(ctx, fmt.Sprintf(a.crt()+" pull %s", shellQuote(svc.Image)), a.Stdout); err != nil {
						return nil, err
					}
					changed = append(changed, name)
				}
			}
			continue
		}
		if !rebuild && !mutable && !buildOnly {
			exists, err := imageExists(ctx, targetExec, svc.Image, a.rt())
			if err != nil {
				return nil, err
			}
			if exists {
				fmt.Fprintf(a.Stdout, "  %s: %s\n", bold(name), green("exists on target, skipping (use --rebuild to force)"))
				continue
			}
		}
		if err := a.runHook(ctx, buildExec, "pre_build", name, svc.Hooks.PreBuild); err != nil {
			return nil, err
		}
		oldID := imageID(ctx, buildExec, svc.Image, a.rt())
		fmt.Fprintf(a.Stdout, "  %s %s: %s from %s\n", bold(name), dim("("+imageTag(svc.Image)+")"), yellow("building"), dim(svc.Dockerfile))
		if err := buildExec.RunStream(ctx, buildImageCommand(root, svc, buildCfg, a.rt()), a.Stdout); err != nil {
			return nil, err
		}
		newID := imageID(ctx, buildExec, svc.Image, a.rt())
		if oldID != newID {
			changed = append(changed, name)
		}
		if err := a.runHook(ctx, buildExec, "post_build", name, svc.Hooks.PostBuild); err != nil {
			return nil, err
		}
		if buildOnly {
			continue
		}
		switch delivery {
		case "registry":
			if err := buildExec.RunStream(ctx, fmt.Sprintf(a.crt()+" push %s", shellQuote(svc.Image)), a.Stdout); err != nil {
				return nil, err
			}
			if err := targetExec.RunStream(ctx, fmt.Sprintf(a.crt()+" pull %s", shellQuote(svc.Image)), a.Stdout); err != nil {
				return nil, err
			}
			if !contains(changed, name) {
				changed = append(changed, name)
			}
		case "direct":
			if !mutable {
				targetHas, err := imageExists(ctx, targetExec, svc.Image, a.rt())
				if err != nil {
					return nil, err
				}
				if targetHas {
					continue
				}
			}
			remoteTar := fmt.Sprintf("/tmp/qqd-%s-%s.tar", cfg.Name, name)
			localTmp, err := os.CreateTemp("", "qqd-image-*.tar")
			if err != nil {
				return nil, err
			}
			localPath := localTmp.Name()
			localTmp.Close()
			defer os.Remove(localPath)
			if _, err := buildExec.Run(ctx, fmt.Sprintf(a.crt()+" save %s -o %s", shellQuote(svc.Image), shellQuote(remoteTar))); err != nil {
				return nil, err
			}
			if err := buildExec.CopyFrom(ctx, remoteTar, localPath); err != nil {
				return nil, err
			}
			if _, err := buildExec.Run(ctx, fmt.Sprintf("rm -f %s", shellQuote(remoteTar))); err != nil {
				return nil, err
			}
			if err := targetExec.CopyTo(ctx, localPath, remoteTar); err != nil {
				return nil, err
			}
			if _, err := targetExec.Run(ctx, fmt.Sprintf(a.crt()+" load -i %s && rm -f %s", shellQuote(remoteTar), shellQuote(remoteTar))); err != nil {
				return nil, err
			}
			if !contains(changed, name) {
				changed = append(changed, name)
			}
		default:
			return nil, fmt.Errorf("unsupported build-host delivery %q", delivery)
		}
	}
	return changed, nil
}

// ensureImagesGitHubActions triggers GH workflow and pulls produced images.
func (a *App) ensureImagesGitHubActions(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, targetExec Executor, buildOnly bool) ([]string, error) {
	buildCfg := eff.Build
	if buildCfg.Repo == "" || buildCfg.Workflow == "" {
		return nil, fmt.Errorf("github-actions strategy requires repo and workflow")
	}
	branch := cfg.Branch
	if buildCfg.Branch != "" {
		branch = buildCfg.Branch
	}
	local := a.ExecFactory.Local()
	prefix := ""
	if buildCfg.GitHubToken != "" {
		prefix = "GH_TOKEN=" + shellQuote(buildCfg.GitHubToken) + " "
	}
	if _, err := local.Run(ctx, fmt.Sprintf("%sgh workflow run %s --repo %s --ref %s", prefix, shellQuote(buildCfg.Workflow), shellQuote(buildCfg.Repo), shellQuote(branch))); err != nil {
		return nil, err
	}
	runIDOut, err := local.Run(ctx, fmt.Sprintf("%sgh run list --repo %s --workflow %s --limit 1 --json databaseId --jq '.[0].databaseId'",
		prefix, shellQuote(buildCfg.Repo), shellQuote(buildCfg.Workflow)))
	if err != nil {
		return nil, err
	}
	runID := strings.TrimSpace(runIDOut)
	if runID == "" {
		return nil, fmt.Errorf("failed to determine GitHub Actions run id")
	}
	if _, err := local.Run(ctx, fmt.Sprintf("%sgh run watch %s --repo %s --exit-status", prefix, shellQuote(runID), shellQuote(buildCfg.Repo))); err != nil {
		return nil, err
	}
	if buildOnly {
		return nil, nil
	}
	var changed []string
	if buildCfg.Registry != "" && buildCfg.RegistryUser != "" && buildCfg.RegistryToken != "" {
		loginCmd := fmt.Sprintf("printf %%s %s | "+a.crt()+" login %s -u %s --password-stdin",
			shellQuote(buildCfg.RegistryToken), shellQuote(buildCfg.Registry), shellQuote(buildCfg.RegistryUser))
		if _, err := targetExec.Run(ctx, loginCmd); err != nil {
			return nil, err
		}
	}
	for _, name := range sortedKeys(eff.Services) {
		svc := eff.Services[name]
		if !isMutableTag(svc.Image) {
			exists, err := imageExists(ctx, targetExec, svc.Image, a.rt())
			if err != nil {
				return nil, err
			}
			if exists {
				continue
			}
		}
		if err := a.runHook(ctx, targetExec, "pre_build", name, svc.Hooks.PreBuild); err != nil {
			return nil, err
		}
		if err := targetExec.RunStream(ctx, fmt.Sprintf(a.crt()+" pull %s", shellQuote(svc.Image)), a.Stdout); err != nil {
			return nil, err
		}
		changed = append(changed, name)
		if err := a.runHook(ctx, targetExec, "post_build", name, svc.Hooks.PostBuild); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

// imageExists checks whether a given image reference is available on host.
func imageExists(ctx context.Context, exec Executor, image string, rt ...ContainerRuntime) (bool, error) {
	cmd := "podman image exists " + shellQuote(image)
	if len(rt) > 0 && rt[0] != nil {
		cmd = rt[0].ImageExistsCmd(image)
	}
	if _, err := exec.Run(ctx, cmd); err != nil {
		return false, nil
	}
	return true, nil
}

// imageID returns the short image ID for a tag, or "" if the tag doesn't exist.
func imageID(ctx context.Context, exec Executor, image string, rt ...ContainerRuntime) string {
	cmd := "podman"
	if len(rt) > 0 && rt[0] != nil {
		cmd = rt[0].Cmd()
	}
	out, err := exec.Run(ctx, fmt.Sprintf("%s image inspect --format '{{.Id}}' %s 2>/dev/null", cmd, shellQuote(image)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func imageConfigUser(ctx context.Context, exec Executor, image string, rt ContainerRuntime) (string, bool) {
	cmd := "podman"
	if rt != nil {
		cmd = rt.Cmd()
	}
	out, err := exec.Run(ctx, fmt.Sprintf("%s image inspect --format '{{.Config.User}}' %s 2>/dev/null || true", cmd, shellQuote(image)))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

func userNeedsVolumeOwnershipMapping(user string) bool {
	user = strings.TrimSpace(user)
	if user == "" {
		return false
	}
	main := user
	if i := strings.IndexAny(main, ":."); i >= 0 {
		main = main[:i]
	}
	switch strings.ToLower(main) {
	case "", "0", "root":
		return false
	default:
		return true
	}
}

func imageTag(image string) string {
	_, tag, ok := splitImageTag(image)
	if !ok {
		return image
	}
	return tag
}

// isMutableTag returns true if the image tag may change without the tag name
// changing (e.g. "latest"). Such tags should always be re-pulled or rebuilt.
func isMutableTag(image string) bool {
	_, tag, ok := splitImageTag(image)
	if !ok {
		return true // no tag at all, treat as mutable
	}
	return tag == "latest"
}

// buildImageCommand creates a resource-limited container build command.
// When svc.Context is set, the build runs from root/context with -f relative
// to that context dir. Otherwise falls back to running from root with the
// full dockerfile path.
func buildImageCommand(root string, svc ServiceConfig, build BuildConfig, rt ...ContainerRuntime) string {
	cmd := "podman"
	if len(rt) > 0 && rt[0] != nil {
		cmd = rt[0].Cmd()
	}
	buildParts := []string{cmd + " build"}
	if build.CPU > 0 {
		buildParts = append(buildParts, fmt.Sprintf("--cpu-period=100000 --cpu-quota=%d", build.CPU*100000))
	}
	if build.Memory != "" {
		buildParts = append(buildParts, fmt.Sprintf("--memory=%s", shellQuote(build.Memory)))
	}
	contextDir := root
	dockerfilePath := svc.Dockerfile
	if svc.Context != "" {
		contextDir = filepath.ToSlash(filepath.Join(root, svc.Context))
		// Make dockerfile relative to context
		rel, err := filepath.Rel(svc.Context, svc.Dockerfile)
		if err == nil {
			dockerfilePath = rel
		}
	}
	buildParts = append(buildParts,
		fmt.Sprintf("-t %s", shellQuote(svc.Image)),
		fmt.Sprintf("-f %s", shellQuote(dockerfilePath)),
		".",
	)
	return fmt.Sprintf("cd %s && %s", shellQuote(contextDir), strings.Join(buildParts, " "))
}
