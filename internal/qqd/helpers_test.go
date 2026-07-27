package qqd

import (
	"context"
	"io"
	"strings"
	"testing"
)

const testQdDir = "~/.config/containers/systemd"

// markedQuadlet returns file content as qqd writes it: ownership marker first.
func markedQuadlet(project, body string) string {
	return withProjectMarker(project, body)
}

// TestRemoveStaleQuadletsKeepsUnitReferencedByRetainedUnit covers a regression
// where a regenerated quadlet set omitted a non-slotted base unit ("db") while a
// retained/rewritten unit ("server") still declared Requires=app-db.service.
// removeStaleQuadlets saw db's quadlet missing from newFiles and reaped it, so
// server then failed to start with "Unit app-db.service not found" and the
// target went down.
//
// A unit still referenced by any retained unit's After=/Requires=/Wants= must
// never be removed.
func TestRemoveStaleQuadletsKeepsUnitReferencedByRetainedUnit(t *testing.T) {
	targetExec := newMockExecutor("target-main")

	// Existing on-target quadlet set for the whole stack, including the
	// non-slotted db base unit.
	onTarget := []string{
		"app-network.network",
		"app-db.container",
		"app-server.container",
		"app-files.container",
		"app-frontend.container",
		"app-worker.container",
	}
	for _, name := range onTarget {
		targetExec.files[testQdDir+"/"+name] = markedQuadlet("app", "stub\n")
	}

	// Regenerated file set for this pass OMITS db, but the retained server
	// unit still requires it.
	serverContent := "[Unit]\n" +
		"After=app-db.service\n" +
		"Requires=app-db.service\n\n" +
		"[Container]\nContainerName=app-server\n"
	newFiles := []QuadletFile{
		{Name: "app-network.network", Content: "[Network]\n"},
		{Name: "app-server.container", Content: serverContent},
		{Name: "app-files.container", Content: "[Container]\nContainerName=app-files\n"},
		{Name: "app-frontend.container", Content: "[Container]\nContainerName=app-frontend\n"},
		{Name: "app-worker.container", Content: "[Container]\nContainerName=app-worker\n"},
	}

	// Full target service set (as a rollback resolves), including db.
	services := map[string]ServiceConfig{
		"db":       {Image: "docker.io/library/postgres:16"},
		"server":   {Image: "ghcr.io/acme/server:1"},
		"files":    {Image: "ghcr.io/acme/files:1"},
		"frontend": {Image: "ghcr.io/acme/frontend:1"},
		"worker":   {Image: "ghcr.io/acme/worker:1"},
	}
	// Slotted (HTTP-exposed) services; db is a non-slotted TCP base unit.
	slottedSvcs := map[string]bool{"server": true, "files": true, "frontend": true, "worker": true}

	app := &App{Runtime: PodmanRuntime{}, Stdout: io.Discard}
	app.removeStaleQuadlets(context.Background(), "app", testQdDir, newFiles, services, services, targetExec, true, slottedSvcs)

	cmds := strings.Join(targetExec.commands, "\n")
	if strings.Contains(cmds, "stop 'app-db.service'") {
		t.Fatalf("db unit is still required by retained server unit and must not be stopped:\n%s", cmds)
	}
	if _, ok := targetExec.files[testQdDir+"/app-db.container"]; !ok {
		t.Fatalf("db quadlet is still required by retained server unit and must not be removed:\n%s", cmds)
	}
}

// TestRemoveStaleQuadletsIgnoresOtherProjectWithSharedPrefix covers a deploy of
// project "app" reaping the units of the separate project "app-metrics", whose
// name starts with the same prefix. Ownership must come from the marker inside
// the file, not from the filename.
func TestRemoveStaleQuadletsIgnoresOtherProjectWithSharedPrefix(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.files[testQdDir+"/app-network.network"] = markedQuadlet("app", "[Network]\n")
	targetExec.files[testQdDir+"/app-server.container"] = markedQuadlet("app", "[Container]\nContainerName=app-server\n")
	for _, name := range []string{"app-metrics-db.container", "app-metrics-server.container"} {
		targetExec.files[testQdDir+"/"+name] = markedQuadlet("app-metrics", "[Container]\n")
	}

	newFiles := []QuadletFile{
		{Name: "app-network.network", Content: markedQuadlet("app", "[Network]\n")},
		{Name: "app-server.container", Content: markedQuadlet("app", "[Container]\nContainerName=app-server\n")},
	}
	services := map[string]ServiceConfig{"server": {Image: "ghcr.io/acme/server:1"}}

	app := &App{Runtime: PodmanRuntime{}, Stdout: io.Discard}
	app.removeStaleQuadlets(context.Background(), "app", testQdDir, newFiles, services, services, targetExec, true, nil)

	cmds := strings.Join(targetExec.commands, "\n")
	for _, name := range []string{"app-metrics-db.container", "app-metrics-server.container"} {
		if _, ok := targetExec.files[testQdDir+"/"+name]; !ok {
			t.Errorf("%s belongs to project app-metrics and must not be removed by a deploy of app:\n%s", name, cmds)
		}
	}
	if strings.Contains(cmds, "app-metrics-db.service") {
		t.Errorf("must not touch another project's units:\n%s", cmds)
	}
}

// TestRemoveStaleQuadletsKeepsNumericSuffixService covers a service whose name
// ends in "-<digits>" being mistaken for replica N of a shorter service name.
// Here `qqd deploy api` must leave the unrelated service "api-2" alone; the old
// filename parsing resolved app-api-2.container to "api", saw "api" in this
// deploy's service set, and reaped the live api-2 unit.
func TestRemoveStaleQuadletsKeepsNumericSuffixService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.files[testQdDir+"/app-api.container"] = markedQuadlet("app", "[Container]\nContainerName=app-api\n")
	targetExec.files[testQdDir+"/app-api-2.container"] = markedQuadlet("app", "[Container]\nContainerName=app-api-2\n")

	allServices := map[string]ServiceConfig{
		"api":   {Image: "ghcr.io/acme/api:1"},
		"api-2": {Image: "ghcr.io/acme/api:2"},
	}
	deployServices := map[string]ServiceConfig{"api": allServices["api"]}
	newFiles := []QuadletFile{
		{Name: "app-network.network", Content: markedQuadlet("app", "[Network]\n")},
		{Name: "app-api.container", Content: markedQuadlet("app", "[Container]\nContainerName=app-api\nImage=ghcr.io/acme/api:1\n")},
	}

	app := &App{Runtime: PodmanRuntime{}, Stdout: io.Discard}
	app.removeStaleQuadlets(context.Background(), "app", testQdDir, newFiles, deployServices, allServices, targetExec, false, nil)

	if _, ok := targetExec.files[testQdDir+"/app-api-2.container"]; !ok {
		t.Fatalf("quadlet of the configured service api-2 was reaped as a replica of api:\n%s", strings.Join(targetExec.commands, "\n"))
	}
}

// TestRemoveStaleQuadletsRemovesDroppedService keeps the reaper doing its job: a
// service this project wrote that is gone from the config is removed on a full
// deploy.
func TestRemoveStaleQuadletsRemovesDroppedService(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.files[testQdDir+"/app-server.container"] = markedQuadlet("app", "[Container]\nContainerName=app-server\n")
	targetExec.files[testQdDir+"/app-legacy.container"] = markedQuadlet("app", "[Container]\nContainerName=app-legacy\n")

	services := map[string]ServiceConfig{"server": {Image: "ghcr.io/acme/server:1"}}
	newFiles := []QuadletFile{
		{Name: "app-server.container", Content: markedQuadlet("app", "[Container]\nContainerName=app-server\n")},
	}

	app := &App{Runtime: PodmanRuntime{}, Stdout: io.Discard}
	app.removeStaleQuadlets(context.Background(), "app", testQdDir, newFiles, services, services, targetExec, true, nil)

	if _, ok := targetExec.files[testQdDir+"/app-legacy.container"]; ok {
		t.Fatal("a dropped service's own quadlet should still be reaped on a full deploy")
	}
	if _, ok := targetExec.files[testQdDir+"/app-server.container"]; !ok {
		t.Fatal("the live server quadlet must be kept")
	}
}

// TestRemoveStaleQuadletsSkipsUnmarkedForeignFile makes sure a unit file qqd
// never wrote is left alone even when its name looks like one of ours.
func TestRemoveStaleQuadletsSkipsUnmarkedForeignFile(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	targetExec.files[testQdDir+"/app-handwritten.container"] = "[Container]\nContainerName=app-handwritten\n"

	services := map[string]ServiceConfig{"server": {Image: "ghcr.io/acme/server:1"}}
	app := &App{Runtime: PodmanRuntime{}, Stdout: io.Discard}
	app.removeStaleQuadlets(context.Background(), "app", testQdDir, nil, services, services, targetExec, true, nil)

	if _, ok := targetExec.files[testQdDir+"/app-handwritten.container"]; !ok {
		t.Fatal("a file without this project's marker must not be removed")
	}
}

func TestServiceForQuadlet(t *testing.T) {
	services := map[string]ServiceConfig{
		"api":    {Image: "i"},
		"api-2":  {Image: "i"},
		"server": {Image: "i"},
	}
	cases := []struct {
		file string
		want string
		ok   bool
	}{
		{"app-server.container", "server", true},
		{"app-server-1.container", "server", true},
		{"app-server-a1b2c3d4.container", "server", true},
		{"app-api-2.container", "api-2", true}, // configured service, not api replica 2
		{"app-api-3.container", "api", true},   // no such service: api replica 3
		{"app-proxy.container", "", false},     // proxy is not a service
		{"app-network.network", "", false},     // network file
		{"app-metrics-db.container", "", false}, // different project sharing our prefix
		{"other-server.container", "", false},  // different project entirely
		{"app-unknown.container", "", false},   // not in the service set
		{"app-server-zz.container", "", false}, // neither replica index nor slot hash
	}
	for _, c := range cases {
		got, ok := serviceForQuadlet("app", c.file, ".container", services)
		if got != c.want || ok != c.ok {
			t.Errorf("serviceForQuadlet(%q) = (%q, %v), want (%q, %v)", c.file, got, ok, c.want, c.ok)
		}
	}
}

func TestProjectOwnsContainer(t *testing.T) {
	services := map[string]ServiceConfig{"server": {Image: "i"}, "db": {Image: "i"}}
	owned := []string{"app-server", "app-server-1", "app-server-a1b2c3d4", "app-db", "app-proxy"}
	for _, name := range owned {
		if !projectOwnsContainer("app", name, services) {
			t.Errorf("%q should be recognised as project app's container", name)
		}
	}
	foreign := []string{"app-metrics-db", "app-metrics-server", "other-server", "app-unknown", "postgres"}
	for _, name := range foreign {
		if projectOwnsContainer("app", name, services) {
			t.Errorf("%q must not be treated as project app's container", name)
		}
	}
}

// TestRemoteWriteCmdEscapesDelimiterInContent covers content that contains the
// heredoc delimiter on a line of its own: with a fixed delimiter the write is
// truncated there and the remainder is handed to the remote shell as commands.
func TestRemoteWriteCmdEscapesDelimiterInContent(t *testing.T) {
	content := "Environment=\"NOTE=first\nQD_EOF\necho surprise\n\"\n"
	cmd := remoteWriteCmd("~/x.container", content)

	header := cmd[:strings.Index(cmd, "\n")]
	if strings.Contains(header, "<<'QD_EOF'") {
		t.Fatalf("delimiter must not be one that appears in the content: %s", header)
	}
	delim := header[strings.Index(header, "<<'")+3 : len(header)-1]
	if !strings.HasPrefix(delim, "QD_EOF_") {
		t.Fatalf("unexpected delimiter %q", delim)
	}
	if containsLine(content, delim) {
		t.Fatalf("chosen delimiter %q still appears as a line of the content", delim)
	}
	body := strings.TrimSuffix(cmd[strings.Index(cmd, "\n")+1:], delim)
	if body != content {
		t.Fatalf("content must round-trip verbatim, got %q", body)
	}
}

func TestRemoteWriteCmdTerminatesDelimiterOnOwnLine(t *testing.T) {
	cmd := remoteWriteCmd("~/x.container", "Image=x") // no trailing newline
	if !strings.HasSuffix(cmd, "\nQD_EOF") {
		t.Fatalf("closing delimiter must sit on its own line: %q", cmd)
	}
}

func TestQuadletFilesCarryProjectMarker(t *testing.T) {
	services := map[string]ServiceConfig{"server": {Image: "ghcr.io/acme/server:1"}}
	files := renderQuadletFiles("app", services, services, ExposeConfig{}, TraefikProvider{}, PodmanRuntime{}, "deploy")
	if len(files) == 0 {
		t.Fatal("expected rendered files")
	}
	for _, f := range files {
		if !strings.HasPrefix(f.Content, "# qqd-project=app\n") {
			t.Errorf("%s is missing the ownership marker:\n%s", f.Name, f.Content)
		}
	}
}
