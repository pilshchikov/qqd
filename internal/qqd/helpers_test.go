package qqd

import (
	"context"
	"io"
	"strings"
	"testing"
)

// TestRemoveStaleQuadletsKeepsUnitReferencedByRetainedUnit covers a regression: a rollback regenerated a quadlet set that omitted a
// non-slotted base unit ("db") while a retained/rewritten unit ("server")
// still declared Requires=app-db.service. removeStaleQuadlets saw db's
// quadlet missing from newFiles and reaped it, so server then failed to start
// with "Unit app-db.service not found" and the target went down.
//
// A unit still referenced by any retained unit's After=/Requires=/Wants= must
// never be removed.
func TestRemoveStaleQuadletsKeepsUnitReferencedByRetainedUnit(t *testing.T) {
	targetExec := newMockExecutor("target-main")
	qdDir := "~/.config/containers/systemd"

	// Existing on-VM quadlet set for the whole stack, including the
	// non-slotted db base unit.
	onVM := []string{
		"app-network.network",
		"app-db.container",
		"app-server.container",
		"app-files.container",
		"app-frontend.container",
		"app-worker.container",
	}
	for _, name := range onVM {
		targetExec.files[qdDir+"/"+name] = "stub\n"
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
		"worker":      {Image: "ghcr.io/acme/worker:1"},
	}
	// Slotted (HTTP-exposed) services; db is a non-slotted TCP base unit.
	slottedSvcs := map[string]bool{"server": true, "files": true, "frontend": true, "worker": true}

	app := &App{Runtime: PodmanRuntime{}, Stdout: io.Discard}
	app.removeStaleQuadlets(context.Background(), "app", qdDir, newFiles, services, targetExec, true, slottedSvcs)

	cmds := strings.Join(targetExec.commands, "\n")
	if strings.Contains(cmds, "stop 'app-db.service'") {
		t.Fatalf("db unit is still required by retained server unit and must not be stopped:\n%s", cmds)
	}
	if _, ok := targetExec.files[qdDir+"/app-db.container"]; !ok {
		t.Fatalf("db quadlet is still required by retained server unit and must not be removed:\n%s", cmds)
	}
}
