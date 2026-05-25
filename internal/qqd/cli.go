package qqd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Execute runs the qqd CLI command dispatcher.
func Execute(args []string, stdout io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if len(args) == 0 {
		printGlobalHelp(stdout)
		return nil
	}

	if isHelpToken(args[0]) {
		printGlobalHelp(stdout)
		return nil
	}

	if args[0] == "help" {
		if len(args) == 1 {
			printGlobalHelp(stdout)
			return nil
		}
		printCommandHelp(stdout, args[1])
		return nil
	}

	switch args[0] {
	case "init":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "init")
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		app := NewApp()
		app.Stdout = stdout
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts, err := parseCommonOpts(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadProjectConfig(opts.CfgPaths, wd)
		if err != nil {
			return err
		}
		app.ForceUnlock = opts.ForceUnlock
		return app.Init(ctx, cfg, opts.Target, opts.Services, opts.Rebuild)
	case "plan":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "plan")
			return nil
		}
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts, output, err := parsePlanArgs(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadProjectConfig(opts.CfgPaths, wd)
		if err != nil {
			return err
		}
		mode := planMode{Rebuild: opts.Rebuild}
		if output == "json" {
			return writeDeployPlanJSON(stdout, cfg, opts.Target, opts.Services, mode)
		}
		printDeployPlan(stdout, "plan", cfg, opts.Target, opts.Services, mode)
		return nil
	case "deploy":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "deploy")
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		app := NewApp()
		app.Stdout = stdout
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts, err := parseCommonOpts(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadProjectConfig(opts.CfgPaths, wd)
		if err != nil {
			return err
		}
		pm := planMode{Rebuild: opts.Rebuild, NoBuild: opts.NoBuild, ConfigOnly: opts.ConfigOnly}
		if opts.DryRun {
			printDeployPlan(stdout, "deploy (dry-run)", cfg, opts.Target, opts.Services, pm)
			InitColor(stdout)
			fmt.Fprintln(stdout, dim("dry-run: no changes applied"))
			return nil
		}
		if !opts.Approve {
			printDeployPlan(stdout, "deploy", cfg, opts.Target, opts.Services, pm)
			if !confirmPlan(stdout) {
				fmt.Fprintln(stdout, "aborted")
				return nil
			}
		}
		app.ForceUnlock = opts.ForceUnlock
		if opts.ConfigOnly {
			return app.DeployConfigOnly(ctx, cfg, opts.Target, opts.Services)
		}
		if opts.NoBuild {
			app.NoBuild = true
		}
		return app.Deploy(ctx, cfg, opts.Target, opts.Services, opts.Rebuild)
	case "build":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "build")
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		app := NewApp()
		app.Stdout = stdout
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts, err := parseCommonOpts(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadProjectConfig(opts.CfgPaths, wd)
		if err != nil {
			return err
		}
		app.ForceUnlock = opts.ForceUnlock
		return app.Build(ctx, cfg, opts.Target, opts.Services, opts.Rebuild)
	case "status":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "status")
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		app := NewApp()
		app.Stdout = stdout
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfgPaths, target, outputFormat, err := parseStatusArgs(args[1:])
		if err != nil {
			return err
		}
		app.OutputFormat = outputFormat
		cfg, err := loadProjectConfig(cfgPaths, wd)
		if err != nil {
			return err
		}
		return app.Status(ctx, cfg, target)
	case "logs":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "logs")
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		app := NewApp()
		app.Stdout = stdout
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfgPaths, target, services, _, err := parseCommonArgs(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadProjectConfig(cfgPaths, wd)
		if err != nil {
			return err
		}
		if target == "" {
			if len(cfg.Targets) != 1 {
				return errors.New("logs requires -t when config has multiple targets")
			}
			for t := range cfg.Targets {
				target = t
			}
		}
		return app.Logs(ctx, cfg, target, services)
	case "rollback":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "rollback")
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		app := NewApp()
		app.Stdout = stdout
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts, err := parseCommonOpts(args[1:])
		if err != nil {
			return err
		}
		service := ""
		if len(opts.Services) > 1 {
			return errors.New("rollback accepts at most one service")
		}
		if len(opts.Services) == 1 {
			service = opts.Services[0]
		}
		cfg, err := loadProjectConfig(opts.CfgPaths, wd)
		if err != nil {
			return err
		}
		app.ForceUnlock = opts.ForceUnlock
		return app.Rollback(ctx, cfg, opts.Target, service)
	case "history":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "history")
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		app := NewApp()
		app.Stdout = stdout
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfgPaths, target, err := parseTargetOnlyArgs(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadProjectConfig(cfgPaths, wd)
		if err != nil {
			return err
		}
		return app.History(ctx, cfg, target)
	case "stop":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "stop")
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		app := NewApp()
		app.Stdout = stdout
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfgPaths, target, services, _, err := parseCommonArgs(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadProjectConfig(cfgPaths, wd)
		if err != nil {
			return err
		}
		return app.Stop(ctx, cfg, target, services)
	case "start":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "start")
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		app := NewApp()
		app.Stdout = stdout
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfgPaths, target, services, _, err := parseCommonArgs(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadProjectConfig(cfgPaths, wd)
		if err != nil {
			return err
		}
		return app.Start(ctx, cfg, target, services)
	case "destroy":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "destroy")
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		app := NewApp()
		app.Stdout = stdout
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts, err := parseTargetOnlyOpts(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadProjectConfig(opts.CfgPaths, wd)
		if err != nil {
			return err
		}
		app.ForceUnlock = opts.ForceUnlock
		return app.Destroy(ctx, cfg, opts.Target)
	case "clean":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "clean")
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		app := NewApp()
		app.Stdout = stdout
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfgPaths, target, err := parseTargetOnlyArgs(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadProjectConfig(cfgPaths, wd)
		if err != nil {
			return err
		}
		return app.Clean(ctx, cfg, target)
	case "update":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "update")
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		app := NewApp()
		app.Stdout = stdout
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts, err := parseCommonOpts(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadProjectConfig(opts.CfgPaths, wd)
		if err != nil {
			return err
		}
		updates := map[string]string{}
		if len(opts.Services) == 0 {
			for name, svc := range cfg.Services {
				if svc.Dockerfile == "" {
					continue
				}
				_, tag, ok := splitImageTag(svc.Image)
				if !ok {
					continue
				}
				next, err := bumpVersion(tag)
				if err != nil {
					continue
				}
				updates[name] = next
			}
			if len(updates) == 0 {
				return errors.New("no buildable services with bumpable version tags")
			}
		} else {
			for _, a := range opts.Services {
				svc, ver, has := strings.Cut(a, "=")
				svc = strings.TrimSpace(svc)
				if svc == "" {
					return fmt.Errorf("invalid update argument: %s", a)
				}
				serviceCfg, ok := cfg.Services[svc]
				if !ok {
					return fmt.Errorf("service %s not found", svc)
				}
				if has {
					if strings.TrimSpace(ver) == "" {
						return fmt.Errorf("invalid explicit version for %s", svc)
					}
					updates[svc] = strings.TrimSpace(ver)
					continue
				}
				_, tag, ok := splitImageTag(serviceCfg.Image)
				if !ok {
					return fmt.Errorf("service %s image has no tag: %s", svc, serviceCfg.Image)
				}
				next, err := bumpVersion(tag)
				if err != nil {
					return fmt.Errorf("service %s: %w", svc, err)
				}
				updates[svc] = next
			}
		}
		absCfg, err := resolveLocalPath(wd, opts.CfgPaths[0])
		if err != nil {
			return err
		}
		// Show the plan BEFORE writing the config file.
		InitColor(stdout)
		serviceList := make([]string, 0, len(updates))
		for _, s := range sortedKeys(updates) {
			serviceList = append(serviceList, s)
		}
		if !opts.Approve {
			printUpdatePlan(stdout, cfg, opts.Target, updates, opts.Rebuild)
			if !confirmPlan(stdout) {
				fmt.Fprintln(stdout, "aborted")
				return nil
			}
		}
		// Only write config AFTER approval.
		if err := updateConfigVersions(absCfg, updates); err != nil {
			return err
		}
		newCfg, err := loadProjectConfig(opts.CfgPaths, wd)
		if err != nil {
			return err
		}
		return app.Deploy(ctx, newCfg, opts.Target, serviceList, opts.Rebuild)
	case "doctor":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "doctor")
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		app := NewApp()
		app.Stdout = stdout
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfgPaths, target, err := parseTargetOnlyArgs(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadProjectConfig(cfgPaths, wd)
		if err != nil {
			return err
		}
		return app.Doctor(ctx, cfg, target)
	case "validate":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "validate")
			return nil
		}
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfgPaths, _, err := parseTargetOnlyArgs(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadProjectConfig(cfgPaths, wd)
		if err != nil {
			return err
		}
		msgs := ValidateConfig(cfg)
		if len(msgs) == 0 {
			fmt.Fprintln(stdout, "config valid: no errors or warnings")
			return nil
		}
		hasErrors := false
		for _, m := range msgs {
			fmt.Fprintln(stdout, m)
			if strings.HasPrefix(m, "error:") {
				hasErrors = true
			}
		}
		if hasErrors {
			return errors.New("validation failed")
		}
		return nil
	case "man":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "man")
			return nil
		}
		return openManPage()
	case "import":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "import")
			return nil
		}
		opts, err := parseImportComposeArgs(args[1:])
		if err != nil {
			return err
		}
		return ImportCompose(opts)
	case "migrate":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "migrate")
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		app := NewApp()
		app.Stdout = stdout
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		mOpts, err := parseUnifiedMigrateArgs(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadProjectConfig(mOpts.CfgPaths, wd)
		if err != nil {
			return err
		}
		from := strings.ToLower(mOpts.From)
		to := strings.ToLower(mOpts.To)
		if to == "" {
			to = "podman"
		}
		if to != "podman" {
			return fmt.Errorf("unsupported --to value %q (qqd deploys with Podman only)", mOpts.To)
		}
		app.DryRun = mOpts.DryRun
		app.AssumeYes = mOpts.Yes
		switch from {
		case "compose", "swarm":
			return app.MigrateCompose(ctx, cfg, MigrateComposeOpts{
				CfgPaths:  mOpts.CfgPaths,
				Target:    mOpts.Target,
				StackName: mOpts.Stack,
				Runtime:   to,
			})
		default:
			return fmt.Errorf("unsupported --from value %q (use: compose, swarm)", from)
		}
	case "convert":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "convert")
			return nil
		}
		input, output, format, err := parseConvertArgs(args[1:])
		if err != nil {
			return err
		}
		return ConvertConfig(input, output, format)
	case "docs":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "docs")
			return nil
		}
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts, err := parseDocsArgs(args[1:])
		if err != nil {
			return err
		}
		return writeGeneratedDocs(opts, wd, stdout)
	case "manifest":
		if wantsHelp(args[1:]) {
			printCommandHelp(stdout, "manifest")
			return nil
		}
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts, err := parseManifestArgs(args[1:])
		if err != nil {
			return err
		}
		return runManifestCommand(opts, wd, stdout)
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New(globalUsage())
}

// globalUsage builds the top-level usage block from commandSpecs() so help,
// generated docs, and tests all share one command inventory.
func globalUsage() string {
	var b strings.Builder
	b.WriteString("usage:\n  qqd [--help]\n  qqd help [command]\n")
	for _, spec := range commandSpecs() {
		b.WriteString("  ")
		b.WriteString(spec.Usage)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// printGlobalHelp writes full CLI help text.
func printGlobalHelp(w io.Writer) {
	fmt.Fprintln(w, globalUsage())
	fmt.Fprintln(w)
	fmt.Fprintln(w, "commands:")
	for _, c := range commandSpecs() {
		fmt.Fprintf(w, "  %-9s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "set host = \"local\" in a target to run on the local machine instead of SSH")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "use `qqd help <command>` for command details")
}

// printCommandHelp writes detailed help for one command.
func printCommandHelp(w io.Writer, cmd string) {
	spec, ok := commandSpecByName(cmd)
	if !ok {
		fmt.Fprintf(w, "unknown command %q\n\n", cmd)
		printGlobalHelp(w)
		return
	}
	fmt.Fprintf(w, "usage: %s\n", spec.Usage)
	if len(spec.Details) > 0 {
		fmt.Fprintln(w)
		for _, line := range spec.Details {
			fmt.Fprintln(w, line)
		}
	}
}

// isHelpToken reports whether a token is a help switch.
func isHelpToken(s string) bool {
	return s == "-h" || s == "--help"
}

// wantsHelp reports whether any argument requests help output.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if isHelpToken(a) {
			return true
		}
	}
	return false
}

// openManPage opens the installed qqd manual page.
func openManPage() error {
	if _, err := exec.LookPath("man"); err != nil {
		return errors.New("`man` command is not available")
	}
	if err := runMan("qqd"); err == nil {
		return nil
	}
	home, _ := os.UserHomeDir()
	localPage := filepath.Join(home, ".local", "share", "man", "man1", "qqd.1")
	if _, err := os.Stat(localPage); err == nil {
		return runMan(localPage)
	}
	return errors.New("could not open man page; run ./install.sh and ensure MANPATH includes ~/.local/share/man")
}

// runMan executes `man` with stdio attached to the current terminal.
func runMan(page string) error {
	cmd := exec.Command("man", page)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// commonOpts holds all flags parsed from the common argument set.
type commonOpts struct {
	CfgPaths    []string
	Target      string
	Services    []string
	Rebuild     bool
	NoBuild     bool // --no-build: skip building dockerfile services
	Approve     bool // --approve: skip interactive confirmation
	DryRun      bool // --dry-run: show plan without executing
	ConfigOnly  bool // --config-only: skip sync+build, only update quadlets and restart
	ForceUnlock bool // --force-unlock: take the deploy lock even if another holder is recorded
}

// parseCommonArgs parses the common -c/-t/--rebuild/--approve argument set.
// Flags and positional arguments may be interspersed in any order
// (e.g. "server -c config.conf" and "-c config.conf server" are equivalent).
func parseCommonArgs(args []string) (cfgPaths []string, target string, services []string, rebuild bool, err error) {
	opts, err := parseCommonOpts(args)
	if err != nil {
		return nil, "", nil, false, err
	}
	return opts.CfgPaths, opts.Target, opts.Services, opts.Rebuild, nil
}

// parseCommonOpts parses all common options including --approve.
func parseCommonOpts(args []string) (commonOpts, error) {
	var opts commonOpts
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "-c" && i+1 < len(args):
			opts.CfgPaths = append(opts.CfgPaths, args[i+1])
			i += 2
		case arg == "-t" && i+1 < len(args):
			opts.Target = args[i+1]
			i += 2
		case arg == "--rebuild":
			opts.Rebuild = true
			i++
		case arg == "--approve":
			opts.Approve = true
			i++
		case arg == "--dry-run":
			opts.DryRun = true
			i++
		case arg == "--config-only":
			opts.ConfigOnly = true
			i++
		case arg == "--no-build":
			opts.NoBuild = true
			i++
		case arg == "--force-unlock":
			opts.ForceUnlock = true
			i++
		default:
			opts.Services = append(opts.Services, arg)
			i++
		}
	}
	if len(opts.CfgPaths) == 0 {
		return commonOpts{}, errors.New("-c <config> is required")
	}
	sort.Strings(opts.Services)
	return opts, nil
}

// parseTargetOnlyArgs parses common args and rejects positional service args.
func parseTargetOnlyArgs(args []string) (cfgPaths []string, target string, err error) {
	opts, err := parseTargetOnlyOpts(args)
	if err != nil {
		return nil, "", err
	}
	return opts.CfgPaths, opts.Target, nil
}

// parseTargetOnlyOpts is parseTargetOnlyArgs but returns the full opts so
// callers that care about flags like --force-unlock can read them.
func parseTargetOnlyOpts(args []string) (commonOpts, error) {
	opts, err := parseCommonOpts(args)
	if err != nil {
		return commonOpts{}, err
	}
	if len(opts.Services) > 0 {
		return commonOpts{}, errors.New("this command does not accept service names")
	}
	return opts, nil
}

// parsePlanArgs parses common args plus the --output flag for `qqd plan`.
// outputFormat defaults to "" (text); the only accepted value is "json".
func parsePlanArgs(args []string) (commonOpts, string, error) {
	var output string
	// Pull --output / --output=json out before commonOpts sees it.
	var passthrough []string
	i := 0
	for i < len(args) {
		switch {
		case args[i] == "--output" && i+1 < len(args):
			output = args[i+1]
			i += 2
		case strings.HasPrefix(args[i], "--output="):
			output = strings.TrimPrefix(args[i], "--output=")
			i++
		default:
			passthrough = append(passthrough, args[i])
			i++
		}
	}
	if output != "" && output != "json" {
		return commonOpts{}, "", fmt.Errorf("unsupported --output format %q (supported: json)", output)
	}
	opts, err := parseCommonOpts(passthrough)
	if err != nil {
		return commonOpts{}, "", err
	}
	return opts, output, nil
}

// parseStatusArgs parses -c, -t, and --output flags for the status command.
// outputFormat defaults to "" (text); the only accepted value is "json".
func parseStatusArgs(args []string) (cfgPaths []string, target string, outputFormat string, err error) {
	var positional []string
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "-c" && i+1 < len(args):
			cfgPaths = append(cfgPaths, args[i+1])
			i += 2
		case arg == "-t" && i+1 < len(args):
			target = args[i+1]
			i += 2
		case arg == "--output" && i+1 < len(args):
			outputFormat = args[i+1]
			i += 2
		default:
			positional = append(positional, arg)
			i++
		}
	}
	if len(cfgPaths) == 0 {
		return nil, "", "", errors.New("-c <config> is required")
	}
	if len(positional) > 0 {
		return nil, "", "", errors.New("this command does not accept service names")
	}
	if outputFormat != "" && outputFormat != "json" {
		return nil, "", "", fmt.Errorf("unsupported output format %q (supported: json)", outputFormat)
	}
	return cfgPaths, target, outputFormat, nil
}

// parseSingleServiceArgs parses common args and requires exactly one service arg.
func parseSingleServiceArgs(cmd string, args []string) (cfgPaths []string, target, service string, err error) {
	cfgPaths, target, services, _, err := parseCommonArgs(args)
	if err != nil {
		return nil, "", "", err
	}
	if len(services) != 1 {
		return nil, "", "", fmt.Errorf("%s requires exactly one service", cmd)
	}
	return cfgPaths, target, services[0], nil
}

// unifiedMigrateOpts holds all possible migrate arguments.
type unifiedMigrateOpts struct {
	CfgPaths []string
	Target   string
	From     string // compose, swarm, docker
	To       string // podman (empty = podman)
	Stack    string // stack name for compose/swarm
	DryRun   bool   // --dry-run: print actions instead of executing
	Yes      bool   // --yes: skip the destructive-action confirmation prompt
}

// parseUnifiedMigrateArgs parses the unified `qqd migrate` command options.
func parseUnifiedMigrateArgs(args []string) (unifiedMigrateOpts, error) {
	var opts unifiedMigrateOpts
	i := 0
	for i < len(args) {
		switch {
		case args[i] == "-c" && i+1 < len(args):
			opts.CfgPaths = append(opts.CfgPaths, args[i+1])
			i += 2
		case args[i] == "-t" && i+1 < len(args):
			opts.Target = args[i+1]
			i += 2
		case args[i] == "--from" && i+1 < len(args):
			opts.From = args[i+1]
			i += 2
		case args[i] == "--to" && i+1 < len(args):
			opts.To = args[i+1]
			i += 2
		case args[i] == "--stack" && i+1 < len(args):
			opts.Stack = args[i+1]
			i += 2
		case args[i] == "--dry-run":
			opts.DryRun = true
			i++
		case args[i] == "--yes" || args[i] == "-y":
			opts.Yes = true
			i++
		default:
			return unifiedMigrateOpts{}, fmt.Errorf("unknown argument: %s", args[i])
		}
	}
	if len(opts.CfgPaths) == 0 {
		return unifiedMigrateOpts{}, fmt.Errorf("-c <config> is required")
	}
	if opts.From == "" {
		return unifiedMigrateOpts{}, fmt.Errorf("--from is required (compose, swarm)")
	}
	return opts, nil
}

// parseImportComposeArgs parses the `qqd import-compose` command options.
func parseImportComposeArgs(args []string) (ComposeImportOpts, error) {
	var opts ComposeImportOpts
	i := 0
	for i < len(args) {
		switch {
		case (args[i] == "-f" || args[i] == "--file") && i+1 < len(args):
			opts.ComposeFile = args[i+1]
			i += 2
		case args[i] == "--env" && i+1 < len(args):
			opts.EnvFile = args[i+1]
			i += 2
		case (args[i] == "-o" || args[i] == "--output") && i+1 < len(args):
			opts.Output = args[i+1]
			i += 2
		case args[i] == "--format" && i+1 < len(args):
			opts.Format = args[i+1]
			i += 2
		case args[i] == "--host" && i+1 < len(args):
			opts.Host = args[i+1]
			i += 2
		case args[i] == "--user" && i+1 < len(args):
			opts.User = args[i+1]
			i += 2
		case args[i] == "--ssh-key" && i+1 < len(args):
			opts.SSHKey = args[i+1]
			i += 2
		case args[i] == "--repo-dir" && i+1 < len(args):
			opts.RepoDir = args[i+1]
			i += 2
		case args[i] == "--runtime":
			return ComposeImportOpts{}, fmt.Errorf("--runtime was removed; qqd imports for Podman only")
		case args[i] == "--ignore" && i+1 < len(args):
			opts.Ignore = args[i+1]
			i += 2
		case args[i] == "--name" && i+1 < len(args):
			opts.ProjectName = args[i+1]
			i += 2
		default:
			return ComposeImportOpts{}, fmt.Errorf("unknown argument: %s", args[i])
		}
	}
	if opts.ComposeFile == "" {
		return ComposeImportOpts{}, fmt.Errorf("-f <docker-compose.yaml> is required")
	}
	return opts, nil
}

// parseConvertArgs parses the `qqd convert` command options.
func parseConvertArgs(args []string) (input, output, format string, err error) {
	i := 0
	for i < len(args) {
		switch {
		case (args[i] == "-c" || args[i] == "--input") && i+1 < len(args):
			input = args[i+1]
			i += 2
		case (args[i] == "-o" || args[i] == "--output") && i+1 < len(args):
			output = args[i+1]
			i += 2
		case args[i] == "--format" && i+1 < len(args):
			format = args[i+1]
			i += 2
		default:
			return "", "", "", fmt.Errorf("unknown argument: %s", args[i])
		}
	}
	if input == "" {
		return "", "", "", fmt.Errorf("-c <input-config> is required")
	}
	return input, output, format, nil
}

// printDeployPlan displays the planned actions for deploy/update.
// planMode controls what the plan displays.
type planMode struct {
	Rebuild    bool
	NoBuild    bool
	ConfigOnly bool
}

func printDeployPlan(w io.Writer, cmd string, cfg ProjectConfig, targetName string, cliServices []string, mode planMode) {
	InitColor(w)
	fmt.Fprintf(w, "\n%s %s\n", boldCyan("[plan]"), bold(cfg.Name))

	// Risks accumulate across targets so we can show them in one block at the
	// bottom — easier to scan than a per-target sprinkle.
	var allRisks []PlanRisk

	// Show project-level settings
	runtime := "podman"
	if cfg.Runtime != "" {
		runtime = cfg.Runtime
	}
	syncMode := "git"
	if cfg.Sync != "" {
		syncMode = cfg.Sync
	}
	if mode.ConfigOnly {
		syncMode = "none (config-only)"
	} else if mode.NoBuild {
		syncMode = syncMode + " (no-build)"
	}
	fmt.Fprintf(w, "  runtime: %s  sync: %s\n", dim(runtime), dim(syncMode))

	targets := targetOrder(cfg, targetName)
	for _, name := range targets {
		eff, err := resolveTarget(cfg, name, cliServices)
		if err != nil {
			fmt.Fprintf(w, "  target %s: %s\n", name, red(err.Error()))
			continue
		}
		fmt.Fprintf(w, "\n  target %s %s\n", bold(name), dim("("+eff.Target.Host+")"))

		for _, svcName := range sortedKeys(eff.Services) {
			svc := eff.Services[svcName]

			var action string
			if mode.ConfigOnly {
				action = "config update"
			} else if mode.NoBuild && svc.Dockerfile != "" {
				action = "skip build"
			} else if svc.Dockerfile != "" {
				if mode.Rebuild {
					action = "rebuild"
				} else {
					action = "build (if changed)"
				}
			} else {
				action = "pull (if missing)"
			}

			// Deploy mode
			deployMode := "restart if changed"
			if isReplicated(svc) {
				deployMode = fmt.Sprintf("rolling (%d replicas)", effectiveReplicas(svc))
			} else if isServiceHTTPExposed(svcName, eff.Expose) {
				deployMode = "zero-downtime slot"
			}

			actionColor := dim(action)
			if strings.Contains(action, "build") || strings.Contains(action, "rebuild") {
				actionColor = yellow(action)
			} else if strings.Contains(action, "skip") {
				actionColor = dim(action)
			} else if action == "config update" {
				actionColor = green(action)
			}

			fmt.Fprintf(w, "    %s  %s  %s  %s\n",
				bold(svcName),
				dim(svc.Image),
				actionColor,
				dim("["+deployMode+"]"))

			if len(svc.Env) > 0 {
				for _, k := range sortedKeys(svc.Env) {
					v := svc.Env[k]
					if isSecretKey(k) {
						v = redactValue(v)
					}
					fmt.Fprintf(w, "      %s=%s\n", dim(k), dim(v))
				}
			}
		}

		if hasExposedServices(eff.Expose) {
			proxyName := "traefik"
			if cfg.Proxy != "" {
				proxyName = cfg.Proxy
			}
			fmt.Fprintf(w, "    %s  %s proxy\n", bold("proxy"), proxyName)
		}

		allRisks = append(allRisks, detectPlanRisks(cfg, eff, mode)...)
	}

	if len(allRisks) > 0 {
		fmt.Fprintf(w, "\n  %s\n", bold("risks"))
		for _, r := range allRisks {
			fmt.Fprintln(w, formatRiskLine(r))
		}
	}

	// Flags summary
	if mode.Rebuild {
		fmt.Fprintf(w, "\n  %s\n", yellow("--rebuild: force rebuild all images"))
	}
	if mode.NoBuild {
		fmt.Fprintf(w, "\n  %s\n", dim("--no-build: skip all image builds"))
	}
	if mode.ConfigOnly {
		fmt.Fprintf(w, "\n  %s\n", green("--config-only: update config and restart only (no sync, no build)"))
	}

	partial := len(cliServices) > 0
	if partial {
		fmt.Fprintf(w, "  %s: %s\n", dim("partial deploy"), strings.Join(cliServices, ", "))
	} else {
		fmt.Fprintf(w, "  %s\n", dim("full deploy (deleted services will be removed)"))
	}
	fmt.Fprintln(w)
}

// printUpdatePlan displays a focused plan for the update command, showing only
// the services being updated with their old→new image versions.
func printUpdatePlan(w io.Writer, cfg ProjectConfig, targetName string, updates map[string]string, rebuild bool) {
	InitColor(w)
	fmt.Fprintf(w, "\n%s %s\n", boldCyan("[update]"), bold(cfg.Name))

	runtime := "podman"
	if cfg.Runtime != "" {
		runtime = cfg.Runtime
	}
	syncMode := "git"
	if cfg.Sync != "" {
		syncMode = cfg.Sync
	}
	fmt.Fprintf(w, "  runtime: %s  sync: %s\n", dim(runtime), dim(syncMode))

	targets := targetOrder(cfg, targetName)
	for _, name := range targets {
		t, ok := cfg.Targets[name]
		if !ok {
			continue
		}
		targetServices := map[string]bool{}
		if len(t.Services) > 0 {
			for _, s := range t.Services {
				targetServices[s] = true
			}
		} else {
			for s := range cfg.Services {
				targetServices[s] = true
			}
		}

		// Only show target if it has at least one updated service.
		var relevant []string
		for _, s := range sortedKeys(updates) {
			if targetServices[s] {
				relevant = append(relevant, s)
			}
		}
		if len(relevant) == 0 {
			continue
		}

		fmt.Fprintf(w, "\n  target %s %s\n", bold(name), dim("("+t.Host+")"))
		// Resolve expose for deploy mode display
		eff, _ := resolveTarget(cfg, name, nil)

		for _, svcName := range relevant {
			svc := cfg.Services[svcName]
			oldImage := svc.Image
			repo, _, _ := splitImageTag(oldImage)
			newImage := repo + ":" + updates[svcName]
			action := "pull"
			if svc.Dockerfile != "" {
				if rebuild {
					action = "rebuild"
				} else {
					action = "build"
				}
			}
			deployMode := "restart"
			if isReplicated(svc) {
				deployMode = fmt.Sprintf("rolling (%d replicas)", effectiveReplicas(svc))
			} else if eff.Expose.Entries != nil && isServiceHTTPExposed(svcName, eff.Expose) {
				deployMode = "zero-downtime slot"
			}
			fmt.Fprintf(w, "    %s  %s %s %s  %s  %s\n",
				bold(svcName),
				dim(oldImage),
				dim("->"),
				green(newImage),
				yellow(action),
				dim("["+deployMode+"]"))
		}
	}

	if rebuild {
		fmt.Fprintf(w, "\n  %s\n", yellow("--rebuild: force rebuild all images"))
	}
	fmt.Fprintln(w)
}

// confirmPlan asks the user to confirm the plan. Returns true if confirmed.
func confirmPlan(w io.Writer) bool {
	fmt.Fprintf(w, "%s [y/N]: ", bold("proceed?"))
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes"
}

type ioDiscard struct{}

// Write implements io.Writer by discarding all bytes.
func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}
