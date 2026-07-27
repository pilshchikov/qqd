package qqd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

// TestPartialDeployAutoRollbackKeepsUntouchedServices covers the auto-rollback
// after a failed `qqd deploy <service>`: it used to re-run the install claiming
// a full deploy, so every service that was not named on the CLI looked like it
// had been dropped from the config and its unit was stopped and deleted.
func TestPartialDeployAutoRollbackKeepsUntouchedServices(t *testing.T) {
	targetExec := newMockExecutor("target-main")

	serverImage := "ghcr.io/acme/server:2"
	prevServer := "ghcr.io/acme/server:1"
	workerImage := "ghcr.io/acme/worker:1"
	dbImage := "docker.io/library/postgres:16"
	for _, img := range []string{serverImage, prevServer, workerImage, dbImage} {
		targetExec.existingImage[img] = true
	}

	targetExec.files[testQdDir+"/app-network.network"] = markedQuadlet("app", "[Network]\n")
	targetExec.files[testQdDir+"/app-server.container"] = markedQuadlet("app", "[Container]\nContainerName=app-server\nImage="+prevServer+"\n")
	targetExec.files[testQdDir+"/app-worker.container"] = markedQuadlet("app", "[Container]\nContainerName=app-worker\nImage="+workerImage+"\n")
	targetExec.files[testQdDir+"/app-db.container"] = markedQuadlet("app", "[Container]\nContainerName=app-db\nImage="+dbImage+"\n")

	targetExec.files["~/.config/qqd/app/releases/20260417-100000.000.json"] = fmt.Sprintf(
		`{"id":"20260417-100000.000","timestamp":"2026-04-17T10:00:00Z","services":{"server":"%s","worker":"%s","db":"%s"}}`,
		prevServer, workerImage, dbImage)

	// Fail the unit start of the partial deploy so auto-rollback runs.
	targetExec.failCmds = map[string]int{"systemctl --user start ": 1}

	cfg := ProjectConfig{
		Name: "app",
		Services: map[string]ServiceConfig{
			"server": {Image: serverImage},
			"worker": {Image: workerImage},
			"db":     {Image: dbImage},
		},
		Targets: map[string]TargetConfig{
			"main": {Name: "main", Host: "192.0.2.10", User: "deploy"},
		},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}

	if err := app.Deploy(context.Background(), cfg, "main", []string{"server"}, false); err == nil {
		t.Fatal("expected the partial deploy to fail")
	}

	cmds := strings.Join(targetExec.commands, "\n")
	for _, name := range []string{"app-worker.container", "app-db.container"} {
		if _, ok := targetExec.files[testQdDir+"/"+name]; !ok {
			t.Errorf("%s was not part of `deploy server`; the rollback must not remove it:\n%s", name, cmds)
		}
	}
	for _, unit := range []string{"app-worker.service", "app-db.service"} {
		if strings.Contains(cmds, "stop '"+unit+"'") {
			t.Errorf("rollback stopped %s, which was not part of `deploy server`:\n%s", unit, cmds)
		}
	}
}

// TestDestroyLeavesOtherProjectQuadlets covers `qqd destroy` globbing
// "<project>-*", which also matched the unit files of a project whose name
// starts with this project's name.
func TestDestroyLeavesOtherProjectQuadlets(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.files[testQdDir+"/app.network"] = markedQuadlet("app", "[Network]\n")
	targetExec.files[testQdDir+"/app-server.container"] = markedQuadlet("app", "[Container]\nContainerName=app-server\n")
	for _, name := range []string{"app-metrics.network", "app-metrics-server.container"} {
		targetExec.files[testQdDir+"/"+name] = markedQuadlet("app-metrics", "[Container]\n")
	}

	cfg := ProjectConfig{
		Name:     "app",
		Services: map[string]ServiceConfig{"server": {Image: "ghcr.io/acme/server:1"}},
		Targets:  map[string]TargetConfig{"main": {Name: "main", Host: "192.0.2.10", User: "deploy"}},
	}
	app := &App{
		ExecFactory: mockFactory{
			targets: map[string]*mockExecutor{"main": targetExec},
			local:   newMockExecutor("local"),
		},
		Stdout:    io.Discard,
		DrainWait: -1,
	}
	if err := app.Destroy(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Destroy failed: %v", err)
	}

	for _, name := range []string{"app-metrics.network", "app-metrics-server.container"} {
		if _, ok := targetExec.files[testQdDir+"/"+name]; !ok {
			t.Errorf("%s belongs to project app-metrics and must survive destroying app", name)
		}
	}
	for _, name := range []string{"app.network", "app-server.container"} {
		if _, ok := targetExec.files[testQdDir+"/"+name]; ok {
			t.Errorf("%s belongs to project app and should have been removed", name)
		}
	}
}

// TestDetectActiveSlotPrefersActiveUnit covers an interrupted deploy leaving two
// slot files behind: picking the first one in directory order can select the
// slot that is not running, after which the running slot is reaped as stale.
func TestDetectActiveSlotPrefersActiveUnit(t *testing.T) {
	exec := newMockExecutor("target-main")
	// "0000aaaa" sorts before "ffff9999", so directory order alone picks the
	// dead slot.
	dead, live := "0000aaaa", "ffff9999"
	exec.files[testQdDir+"/app-server-"+dead+".container"] = markedQuadlet("app", "[Container]\n")
	exec.files[testQdDir+"/app-server-"+live+".container"] = markedQuadlet("app", "[Container]\n")
	exec.unitStates = map[string]string{"app-server-" + dead + ".service": "inactive"}

	got := detectActiveSlot(context.Background(), exec, "app", "server", testQdDir, ".container", "systemctl --user")
	if got != live {
		t.Fatalf("detectActiveSlot = %q, want the running slot %q", got, live)
	}
}

// TestRollingRestartWithDrainRestoresRoutesOnFailure covers a failed rolling
// restart leaving the drained replica out of the proxy's route set, which
// silently runs the service with reduced capacity until the next deploy.
func TestRollingRestartWithDrainRestoresRoutesOnFailure(t *testing.T) {
	exec := newMockExecutor("target-main")
	exec.failCmds = map[string]int{"systemctl --user restart ": 1}

	svc := ServiceConfig{Image: "ghcr.io/acme/server:1", Replicas: 2}
	allServices := map[string]ServiceConfig{"server": svc}
	cfg := ProjectConfig{Name: "app", Services: allServices}
	eff := EffectiveTarget{
		Target: TargetConfig{Name: "main", Host: "192.0.2.10", User: "deploy"},
		Expose: ExposeConfig{Entries: []ExposeEntry{
			{HostPort: 80, Routes: map[string]string{"/": "server:8080"}},
		}},
		Services: allServices,
	}
	app := &App{Runtime: PodmanRuntime{}, Stdout: io.Discard, DrainWait: -1}

	err := app.rollingRestartWithDrain(context.Background(), cfg, eff, exec, "server", svc, allServices)
	if err == nil {
		t.Fatal("expected the rolling restart to fail")
	}
	routes := exec.files[app.proxy().DynamicConfigPath("app")]
	for _, replica := range []string{"app-server-1:8080", "app-server-2:8080"} {
		if !strings.Contains(routes, replica) {
			t.Fatalf("replica %s must be back in the route set after a failed restart:\n%s", replica, routes)
		}
	}
}

// TestCleanKeepsImagesReferencedByReleases covers `qqd clean` deleting the image
// a stored release points at — the image `qqd rollback` needs.
func TestCleanKeepsImagesReferencedByReleases(t *testing.T) {
	customExec := &cleanMockExecutor{
		mockExecutor: newMockExecutor("target-main"),
		imagesOutput: "ghcr.io/acme/server:1.44\nghcr.io/acme/server:1.45\nghcr.io/acme/server:1.20",
	}
	customExec.files["~/.config/qqd/app/releases/20260417-100000.000.json"] =
		`{"id":"20260417-100000.000","timestamp":"2026-04-17T10:00:00Z","services":{"server":"ghcr.io/acme/server:1.44"}}`

	cfg := ProjectConfig{
		Name:     "app",
		Services: map[string]ServiceConfig{"server": {Image: "ghcr.io/acme/server:1.45"}},
		Targets:  map[string]TargetConfig{"main": {Name: "main", Host: "192.0.2.20", User: "deploy"}},
	}
	app := &App{
		ExecFactory: cleanMockFactory{exec: customExec},
		Stdout:      io.Discard,
		DrainWait:   -1,
	}
	if err := app.Clean(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("Clean failed: %v", err)
	}
	cmds := strings.Join(customExec.commands, "\n")
	if strings.Contains(cmds, "ghcr.io/acme/server:1.44") {
		t.Fatalf("the image of the previous release must be kept for rollback:\n%s", cmds)
	}
	if !strings.Contains(cmds, "rmi") || !strings.Contains(cmds, "ghcr.io/acme/server:1.20") {
		t.Fatalf("images no release refers to should still be pruned:\n%s", cmds)
	}
}

func TestFormatExecArgsEscapesSystemdSpecifiers(t *testing.T) {
	got := formatExecArgs([]string{"printf", "%s-%H", "--tag=100%"})
	want := `printf %%s-%%H "--tag=100%%"`
	if got != want {
		t.Fatalf("formatExecArgs = %q, want %q", got, want)
	}
}

func TestValidateConfigRejectsHostPortCollisions(t *testing.T) {
	services := map[string]ServiceConfig{"server": {Image: "ghcr.io/acme/server:1"}}
	cfg := ProjectConfig{
		Name:     "app",
		Services: services,
		Targets: map[string]TargetConfig{
			"main": {
				Name: "main", Host: "192.0.2.10", User: "deploy",
				Expose: ExposeConfig{
					Dashboard: 8080,
					Entries: []ExposeEntry{
						{HostPort: 8080, Routes: map[string]string{"/": "server:3000"}},
					},
				},
			},
		},
	}
	msgs := ValidateConfig(cfg)
	found := false
	for _, m := range msgs {
		if strings.HasPrefix(m, "error:") && strings.Contains(m, "8080") {
			found = true
		}
	}
	if !found {
		t.Fatalf("dashboard reusing an exposed host port must be an error, got %v", msgs)
	}
}

func TestCheckUploadRepoDir(t *testing.T) {
	for _, bad := range []string{"", "/", "~", "$HOME", "${HOME}", "/srv", "."} {
		if err := checkUploadRepoDir(bad); err == nil {
			t.Errorf("repo_dir %q should be rejected for upload sync", bad)
		}
	}
	for _, ok := range []string{"/srv/app/src", "~/app/repo", "/home/deploy/app", "~/src"} {
		if err := checkUploadRepoDir(ok); err != nil {
			t.Errorf("repo_dir %q should be accepted: %v", ok, err)
		}
	}
}
