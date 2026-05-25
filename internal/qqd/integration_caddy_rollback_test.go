package qqd

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestIntegrationCaddyHTTPRouting deploys a single HTTP service behind a Caddy
// proxy and verifies the proxy routes traffic to the upstream. This is the
// minimum proof that the Caddy provider works end-to-end.
//
// What it checks:
//   - The Caddy proxy unit becomes active.
//   - The generated Caddyfile is bind-mounted at /etc/caddy/Caddyfile (verified
//     via container exec).
//   - HTTP requests to the published port reach the upstream.
//
// Skipped unless QQD_INTEGRATION=1.
func TestIntegrationCaddyHTTPRouting(t *testing.T) {
	skipIfNoIntegration(t)
	exec := podmanExec(t)
	ctx := context.Background()
	integCleanup(t, exec)
	t.Cleanup(func() { integCleanup(t, exec) })

	app := integApp(t, exec)

	cfg := ProjectConfig{
		Name:  integProject,
		Sync:  "upload",
		Proxy: "caddy",
		Build: BuildConfig{Strategy: "local"},
		Services: map[string]ServiceConfig{
			"web": {Image: "docker.io/library/httpd:2.4"},
		},
		Targets: map[string]TargetConfig{
			"main": {
				Name:    "main",
				Host:    exec.Host,
				User:    exec.User,
				SSHKey:  exec.SSHKey,
				SSHPort: exec.SSHPort,
				RepoDir: "/tmp/qqd-test-repo",
				Expose: ExposeConfig{Entries: []ExposeEntry{
					{HostPort: 19080, Routes: map[string]string{"/": "web:80"}},
				}},
			},
		},
	}

	if err := app.Init(ctx, cfg, "main", nil, false); err != nil {
		t.Fatalf("Init with Caddy proxy failed: %v", err)
	}

	assertUnitActive(t, ctx, exec, integProject+"-web.service")
	assertUnitActive(t, ctx, exec, integProject+"-proxy.service")
	assertContainerRunning(t, ctx, exec, integProject+"-proxy")

	// Verify the Caddyfile route output and the absence of the old static
	// caddy.json on the live target.
	assertFileExists(t, ctx, exec, integConfDir+"/caddy-routes/Caddyfile")
	assertFileNotExists(t, ctx, exec, integConfDir+"/caddy.json")
	assertFileContains(t, ctx, exec, integConfDir+"/caddy-routes/Caddyfile", ":19080")

	// And the bind mount inside the proxy container should expose the same file.
	out, err := exec.Run(ctx, "podman exec "+integProject+"-proxy cat /etc/caddy/Caddyfile")
	if err != nil {
		t.Fatalf("cannot read Caddyfile inside proxy container: %v", err)
	}
	if !strings.Contains(out, ":19080") {
		t.Fatalf("Caddyfile inside proxy container does not contain expected port:\n%s", out)
	}

	// Finally, traffic should flow through the proxy to httpd.
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 20*time.Second)
}

// TestIntegrationRollbackAfterFailedHealth simulates a deploy whose new
// version's health check never goes ready. The deploy should fail and qqd
// should auto-rollback to the previous release (recorded by the prior
// successful deploy).
//
// We trigger the failure by pointing the health check at a port the container
// doesn't listen on. The container starts but never becomes ready, so the
// rolling/slot wait times out and rollback triggers.
//
// Skipped unless QQD_INTEGRATION=1.
func TestIntegrationRollbackAfterFailedHealth(t *testing.T) {
	skipIfNoIntegration(t)
	exec := podmanExec(t)
	ctx := context.Background()
	integCleanup(t, exec)
	t.Cleanup(func() { integCleanup(t, exec) })

	app := integApp(t, exec)

	makeCfg := func(image string, badHealth bool) ProjectConfig {
		health := HealthConfig{Path: "/", Port: 80}
		if badHealth {
			// Port 9999 is not listening inside httpd. Health check will never pass.
			health = HealthConfig{Path: "/", Port: 9999}
		}
		return ProjectConfig{
			Name:  integProject,
			Sync:  "upload",
			Build: BuildConfig{Strategy: "local"},
			Services: map[string]ServiceConfig{
				"web": {Image: image, Health: health},
			},
			Targets: map[string]TargetConfig{
				"main": {
					Name:    "main",
					Host:    exec.Host,
					User:    exec.User,
					SSHKey:  exec.SSHKey,
					SSHPort: exec.SSHPort,
					RepoDir: "/tmp/qqd-test-repo",
					Expose: ExposeConfig{Entries: []ExposeEntry{
						{HostPort: 19080, Routes: map[string]string{"/": "web:80"}},
					}},
				},
			},
		}
	}

	// Deploy v1 with a working health check.
	good := makeCfg("docker.io/library/httpd:2.4", false)
	if err := app.Init(ctx, good, "main", nil, false); err != nil {
		t.Fatalf("Init v1 failed: %v", err)
	}
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 20*time.Second)

	// Capture the pre-rollback image so we can verify it is what we end up
	// with after the rollback completes.
	preImage, err := exec.Run(ctx, "podman inspect --format '{{.Image}}' "+integProject+"-web")
	if err != nil {
		t.Fatalf("cannot inspect v1 container: %v", err)
	}
	preImage = strings.TrimSpace(preImage)

	// Deploy v2 with a bad health check. The deploy must fail.
	bad := makeCfg("docker.io/library/httpd:2.4-alpine", true)
	if err := app.Deploy(ctx, bad, "main", nil, false); err == nil {
		t.Fatal("Deploy with failing health check should have errored, but succeeded")
	}

	// Auto-rollback should have restored the v1 image.
	waitForHTTP(t, ctx, exec, "http://localhost:19080/", 200, 30*time.Second)
	postImage, err := exec.Run(ctx, "podman inspect --format '{{.Image}}' "+integProject+"-web")
	if err != nil {
		t.Fatalf("cannot inspect post-rollback container: %v", err)
	}
	postImage = strings.TrimSpace(postImage)
	if postImage != preImage {
		t.Fatalf("after auto-rollback, image should match pre-rollback (%s); got: %s", preImage, postImage)
	}
}
