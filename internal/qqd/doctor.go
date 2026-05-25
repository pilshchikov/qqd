package qqd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// DoctorResult holds one diagnostic check result.
type DoctorResult struct {
	Name   string
	Status string // "ok", "warning", "error"
	Detail string
}

// Doctor runs diagnostic checks on deployment targets.
func (a *App) Doctor(ctx context.Context, cfg ProjectConfig, targetName string) error {
	InitColor(a.Stdout)

	hasErrors := false

	// Run local config validation first.
	msgs := ValidateConfig(cfg)
	configResult := DoctorResult{Name: "config valid", Status: "ok"}
	for _, m := range msgs {
		if strings.HasPrefix(m, "error:") {
			configResult.Status = "error"
			configResult.Detail = m
			break
		}
		if strings.HasPrefix(m, "warning:") && configResult.Status == "ok" {
			configResult.Status = "warning"
			configResult.Detail = m
		}
	}
	fmt.Fprintf(a.Stdout, "local checks\n")
	printDoctorResult(a, configResult)
	if configResult.Status == "error" {
		hasErrors = true
	}

	targets := targetOrder(cfg, targetName)
	for _, name := range targets {
		eff, err := resolveTarget(cfg, name, nil)
		if err != nil {
			fmt.Fprintf(a.Stdout, "\ntarget=%s %s\n", name, red(err.Error()))
			hasErrors = true
			continue
		}

		fmt.Fprintf(a.Stdout, "\ntarget=%s host=%s\n", bold(name), eff.Target.Host)

		exec, err := a.ExecFactory.ForTarget(eff.Target)
		if err != nil {
			printDoctorResult(a, DoctorResult{
				Name:   "ssh connectivity",
				Status: "error",
				Detail: err.Error(),
			})
			hasErrors = true
			continue
		}
		defer exec.Close()

		sel := a.lifecycleFor(ctx, eff.Target, exec)
		printDoctorResult(a, DoctorResult{
			Name:   "lifecycle backend",
			Status: "ok",
			Detail: fmt.Sprintf("%s (%s)", sel.Backend, sel.Reason),
		})
		results := a.runTargetChecks(ctx, exec, a.rt(), sel)
		for _, r := range results {
			printDoctorResult(a, r)
			if r.Status == "error" {
				hasErrors = true
			}
		}
	}
	if hasErrors {
		return fmt.Errorf("doctor found errors")
	}
	return nil
}

// runTargetChecks runs all diagnostic checks against a single target executor.
// Each check is independent; failures don't prevent subsequent checks from running.
//
// In direct mode the systemd-specific checks (systemctl, UnitDir, lingering)
// are skipped because the backend doesn't use systemd.
func (a *App) runTargetChecks(ctx context.Context, exec Executor, rt ContainerRuntime, sel LifecycleSelection) []DoctorResult {
	var results []DoctorResult

	results = append(results, checkSSH(ctx, exec))
	results = append(results, checkContainerRuntime(ctx, exec, rt))
	if sel.Backend == "systemd" {
		results = append(results, checkSystemd(ctx, exec, rt))
		results = append(results, checkUnitDir(ctx, exec, rt))
		if rt.Name() == "podman" {
			results = append(results, checkLingering(ctx, exec))
		}
	}
	results = append(results, checkDiskSpace(ctx, exec))

	return results
}

func checkSSH(ctx context.Context, exec Executor) DoctorResult {
	_, err := exec.Run(ctx, "echo ok")
	if err != nil {
		return DoctorResult{
			Name:   "ssh connectivity",
			Status: "error",
			Detail: err.Error(),
		}
	}
	return DoctorResult{Name: "ssh connectivity", Status: "ok"}
}

func checkContainerRuntime(ctx context.Context, exec Executor, rt ContainerRuntime) DoctorResult {
	name := rt.Cmd()
	out, err := exec.Run(ctx, name+" --version")
	if err != nil {
		return DoctorResult{
			Name:   name,
			Status: "error",
			Detail: "not installed or not in PATH",
		}
	}
	version := strings.TrimSpace(out)
	return DoctorResult{
		Name:   name,
		Status: "ok",
		Detail: version,
	}
}

func checkSystemd(ctx context.Context, exec Executor, rt ContainerRuntime) DoctorResult {
	sctl := rt.SystemctlPrefix()
	label := "systemd"
	if rt.Name() == "podman" {
		label = "systemd user"
	}
	out, err := exec.Run(ctx, sctl+" is-system-running 2>/dev/null || true")
	status := strings.TrimSpace(out)
	if err != nil {
		return DoctorResult{
			Name:   label,
			Status: "error",
			Detail: "session not reachable",
		}
	}
	switch status {
	case "running":
		return DoctorResult{Name: label, Status: "ok"}
	case "degraded":
		return DoctorResult{Name: label, Status: "warning", Detail: "some units failed"}
	case "offline", "":
		return DoctorResult{Name: label, Status: "error", Detail: "session not available"}
	default:
		return DoctorResult{Name: label, Status: "ok", Detail: status}
	}
}

func checkUnitDir(ctx context.Context, exec Executor, rt ContainerRuntime) DoctorResult {
	dir := rt.UnitDir()
	_, err := exec.Run(ctx, "test -d "+dir)
	if err != nil {
		return DoctorResult{
			Name:   "unit directory",
			Status: "warning",
			Detail: dir + " does not exist (will be created on first deploy)",
		}
	}
	return DoctorResult{Name: "unit directory", Status: "ok"}
}

func checkDiskSpace(ctx context.Context, exec Executor) DoctorResult {
	out, err := exec.Run(ctx, "df -h / | tail -1")
	if err != nil {
		return DoctorResult{
			Name:   "disk space",
			Status: "error",
			Detail: err.Error(),
		}
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) < 5 {
		return DoctorResult{
			Name:   "disk space",
			Status: "warning",
			Detail: "could not parse df output",
		}
	}
	usePct := strings.TrimSuffix(fields[4], "%")
	pct, err := strconv.Atoi(usePct)
	if err != nil {
		return DoctorResult{
			Name:   "disk space",
			Status: "warning",
			Detail: "could not parse usage percentage",
		}
	}
	if pct > 90 {
		return DoctorResult{
			Name:   "disk space",
			Status: "warning",
			Detail: fmt.Sprintf("%d%% used", pct),
		}
	}
	return DoctorResult{
		Name:   "disk space",
		Status: "ok",
		Detail: fmt.Sprintf("%d%% used", pct),
	}
}

func checkLingering(ctx context.Context, exec Executor) DoctorResult {
	out, err := exec.Run(ctx, "loginctl show-user $USER -p Linger 2>/dev/null || echo Linger=unknown")
	if err != nil {
		return DoctorResult{
			Name:   "lingering",
			Status: "warning",
			Detail: "could not check lingering status",
		}
	}
	line := strings.TrimSpace(out)
	if line == "Linger=yes" {
		return DoctorResult{Name: "lingering", Status: "ok"}
	}
	if line == "Linger=unknown" {
		return DoctorResult{
			Name:   "lingering",
			Status: "warning",
			Detail: "could not determine lingering status",
		}
	}
	return DoctorResult{
		Name:   "lingering",
		Status: "warning",
		Detail: "not enabled - services may stop on logout",
	}
}

// printDoctorResult formats and prints one diagnostic check result.
func printDoctorResult(a *App, r DoctorResult) {
	var statusStr string
	switch r.Status {
	case "ok":
		statusStr = green("ok")
	case "warning":
		statusStr = yellow("warning")
	case "error":
		statusStr = red("error")
	default:
		statusStr = r.Status
	}

	if r.Detail != "" {
		fmt.Fprintf(a.Stdout, "  %-20s %s (%s)\n", r.Name+":", statusStr, r.Detail)
	} else {
		fmt.Fprintf(a.Stdout, "  %-20s %s\n", r.Name+":", statusStr)
	}
}
