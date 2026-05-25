package qqd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func needsLocalBuild(ctx context.Context, exec Executor, services map[string]ServiceConfig, rebuild bool) bool {
	for _, name := range sortedKeys(services) {
		svc := services[name]
		if svc.Dockerfile == "" {
			continue
		}
		if rebuild || isMutableTag(svc.Image) {
			return true
		}
		exists, err := imageExists(ctx, exec, svc.Image)
		if err != nil || !exists {
			return true
		}
	}
	return false
}

// buildContextPaths returns unique context subdirectory paths for services that
// need building. Returns nil if any buildable service lacks a context, meaning
// the full project directory must be uploaded.
func buildContextPaths(ctx context.Context, exec Executor, services map[string]ServiceConfig, rebuild bool) []string {
	seen := map[string]bool{}
	var paths []string
	for _, name := range sortedKeys(services) {
		svc := services[name]
		if svc.Dockerfile == "" {
			continue
		}
		if !rebuild && !isMutableTag(svc.Image) {
			exists, err := imageExists(ctx, exec, svc.Image)
			if err == nil && exists {
				continue
			}
		}
		if svc.Context == "" || svc.Context == "." {
			return nil
		}
		if !seen[svc.Context] {
			seen[svc.Context] = true
			paths = append(paths, svc.Context)
		}
	}
	return paths
}

// Returns a list of remote paths that were uploaded (for cleanup after build).
// Returns nil when no upload occurred (git sync or upload skipped).
func (a *App) ensureRepoAndDirs(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, targetExec Executor, rebuild bool) ([]string, error) {
	for _, d := range eff.Target.Dirs {
		if !strings.HasPrefix(d, "/") {
			return nil, fmt.Errorf("target %s dir must be absolute: %s", eff.Target.Name, d)
		}
	}

	// Pure image-pull deploys (no repo_dir, no sync, no build context)
	// have nothing to sync. We still honor `dirs:` for volume mounts.
	if eff.Target.RepoDir == "" && cfg.Sync == "" && !cfg.needsSource() {
		if len(eff.Target.Dirs) > 0 {
			fmt.Fprintf(a.Stdout, "  creating directories %s\n", dim(fmt.Sprintf("(%d)", len(eff.Target.Dirs))))
			if _, err := targetExec.Run(ctx, fmt.Sprintf("mkdir -p %s", joinQuoted(eff.Target.Dirs))); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	fmt.Fprintf(a.Stdout, "  ensuring repo dir %s\n", dim(eff.Target.RepoDir))
	if _, err := targetExec.Run(ctx, fmt.Sprintf("mkdir -p %s", shellQuote(eff.Target.RepoDir))); err != nil {
		return nil, err
	}

	syncMode := cfg.Sync
	if syncMode == "" {
		syncMode = "git"
	}

	var uploaded []string
	switch syncMode {
	case "git":
		if err := a.syncGit(ctx, cfg, eff, targetExec); err != nil {
			return nil, err
		}
	case "upload":
		if a.NoBuild || !needsLocalBuild(ctx, targetExec, eff.Services, rebuild) {
			fmt.Fprintf(a.Stdout, "  %s upload %s\n", green("skipping"), dim("(all images exist)"))
		} else {
			contexts := buildContextPaths(ctx, targetExec, eff.Services, rebuild)
			if err := a.syncUpload(ctx, cfg, eff, contexts, targetExec); err != nil {
				return nil, err
			}
			if contexts == nil {
				uploaded = []string{eff.Target.RepoDir}
			} else {
				for _, c := range contexts {
					uploaded = append(uploaded, eff.Target.RepoDir+"/"+c)
				}
			}
		}
	default:
		return nil, fmt.Errorf("unsupported sync mode %q (expected \"git\" or \"upload\")", syncMode)
	}

	if len(eff.Target.Dirs) > 0 {
		fmt.Fprintf(a.Stdout, "  creating directories %s\n", dim(fmt.Sprintf("(%d)", len(eff.Target.Dirs))))
		if _, err := targetExec.Run(ctx, fmt.Sprintf("mkdir -p %s", joinQuoted(eff.Target.Dirs))); err != nil {
			return nil, err
		}
	}
	return uploaded, nil
}

// syncGit clones (if needed) and fast-forwards the repo on the target.
func (a *App) syncGit(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, targetExec Executor) error {
	ghTokenRaw := expandVars(cfg.GHToken, eff.Target.Env)
	ghToken, err := resolveGHToken(ctx, a.ExecFactory.Local(), ghTokenRaw)
	if err != nil {
		return err
	}
	repo := injectGHToken(cfg.Repo, ghToken)
	sp := startSpinner(a.Stdout, "cloning repo (if needed)")
	cloneCmd := fmt.Sprintf("if [ ! -d %s/.git ]; then git clone --single-branch --branch %s %s %s; fi",
		shellQuote(eff.Target.RepoDir),
		shellQuote(cfg.Branch),
		shellQuote(repo),
		shellQuote(eff.Target.RepoDir),
	)
	if _, err := targetExec.Run(ctx, cloneCmd); err != nil {
		sp.stop()
		return err
	}
	sp.stop()
	sp = startSpinner(a.Stdout, fmt.Sprintf("syncing to origin/%s", cfg.Branch))
	syncCmd := fmt.Sprintf("cd %s && git fetch --all && git reset --hard origin/%s", shellQuote(eff.Target.RepoDir), shellQuote(cfg.Branch))
	if _, err := targetExec.Run(ctx, syncCmd); err != nil {
		sp.stop()
		return err
	}
	sp.stop()
	return nil
}

// syncUpload rsyncs local source to the target via SSH. When contexts is nil,
// the entire working directory is uploaded. When contexts contains paths, only
// those subdirectories are uploaded (scoped by each service's context field).
// targetExec is used to create remote directories before rsync.
func (a *App) syncUpload(ctx context.Context, cfg ProjectConfig, eff EffectiveTarget, contexts []string, targetExec Executor) error {
	localBase := cfg.InvocationWD
	if localBase == "" {
		return fmt.Errorf("upload sync: cannot determine local directory")
	}

	type syncPair struct{ local, remote string }
	var pairs []syncPair
	if contexts == nil {
		pairs = append(pairs, syncPair{localBase, eff.Target.RepoDir})
	} else {
		for _, c := range contexts {
			pairs = append(pairs, syncPair{
				filepath.Join(localBase, c),
				eff.Target.RepoDir + "/" + c,
			})
		}
	}

	isLocal := eff.Target.Host == "local"
	for _, p := range pairs {
		// Ensure remote directory exists before rsync
		if _, err := targetExec.Run(ctx, fmt.Sprintf("mkdir -p %s", shellQuote(p.remote))); err != nil {
			return fmt.Errorf("mkdir remote dir: %w", err)
		}

		localDir := p.local
		if !strings.HasSuffix(localDir, "/") {
			localDir += "/"
		}

		rsyncArgs := []string{
			"rsync", "-az", "--delete",
			"--exclude=.git",
		}
		gitignorePath := filepath.Join(cfg.InvocationWD, ".gitignore")
		if _, err := os.Stat(gitignorePath); err == nil {
			rsyncArgs = append(rsyncArgs, fmt.Sprintf("--exclude-from=%s", shellQuote(gitignorePath)))
		}
		var dest, label string
		if isLocal {
			dest = shellQuote(p.remote + "/")
			label = fmt.Sprintf("copying to %s", p.remote)
		} else {
			sshCmd := "ssh -o StrictHostKeyChecking=yes"
			if eff.Target.InsecureHostKey {
				sshCmd = "ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
			}
			if eff.Target.SSHKey != "" {
				sshCmd += fmt.Sprintf(" -i %s", eff.Target.SSHKey)
			}
			if eff.Target.SSHPort > 0 {
				sshCmd += fmt.Sprintf(" -p %d", eff.Target.SSHPort)
			}
			rsyncArgs = append(rsyncArgs, fmt.Sprintf("-e '%s'", sshCmd))
			dest = fmt.Sprintf("%s@%s:%s/", eff.Target.User, eff.Target.Host, p.remote)
			label = fmt.Sprintf("uploading to %s:%s", eff.Target.Host, p.remote)
		}
		rsyncArgs = append(rsyncArgs, shellQuote(localDir), dest)

		sp := startSpinner(a.Stdout, label)
		local := a.ExecFactory.Local()
		if _, err := local.Run(ctx, strings.Join(rsyncArgs, " ")); err != nil {
			sp.stop()
			return fmt.Errorf("rsync upload: %w", err)
		}
		sp.stop()
	}
	return nil
}

// cleanupUploadedSource removes uploaded source paths from the target after
// images have been built. Paths may be the full repo dir or individual context
// subdirectories depending on how the upload was scoped.
func (a *App) cleanupUploadedSource(ctx context.Context, exec Executor, paths []string) {
	for _, p := range paths {
		sp := startSpinner(a.Stdout, fmt.Sprintf("cleaning up uploaded source %s", p))
		exec.Run(ctx, fmt.Sprintf("rm -rf %s", shellQuote(p)))
		sp.stop()
	}
}
