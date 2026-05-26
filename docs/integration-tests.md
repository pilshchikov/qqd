# Integration Tests

`qqd` ships a set of integration tests that exercise the full deploy lifecycle against a real Podman runtime. They are skipped by default because they require a Podman machine reachable over SSH, but the implementation under test is the same one users run in production - they are far higher signal than the unit tests.

This page documents how to run them, what they cover, and the gaps that still need closing.

## How to run

```bash
# 1. Start podman machine (macOS)
podman machine init     # one-time
podman machine start

# 2. Seed all of the machine's host keys into ~/.ssh/known_hosts
#    (see below — required because the Go SSH client may negotiate
#     ecdsa/rsa even if you only cached the ed25519 key)
PORT=$(podman machine inspect --format '{{.SSHConfig.Port}}')
KEY=$(podman machine inspect --format '{{.SSHConfig.IdentityPath}}')
ssh -p "$PORT" -i "$KEY" -o BatchMode=yes core@localhost \
  'cat /etc/ssh/ssh_host_ed25519_key.pub /etc/ssh/ssh_host_ecdsa_key.pub /etc/ssh/ssh_host_rsa_key.pub' \
  | sed "s|^|[localhost]:$PORT |" >> ~/.ssh/known_hosts

# 3. Run the integration suite
QQD_INTEGRATION=1 go test ./internal/qqd/ -count=1 -run TestIntegration -v
```

On Linux with rootless Podman already configured, the same `QQD_INTEGRATION=1` env var enables the suite. The harness reaches the runtime over SSH (for parity with the SSH path that production deploys use), so you'll need an SSH-reachable Podman target.

**known_hosts gotcha.** The Go SSH client (`golang.org/x/crypto/ssh/knownhosts`) is stricter than openssh: it errors with `knownhosts: key mismatch` if it finds *any* matching entry whose host key type doesn't match what the server negotiates. A common cause is a stale bare-`localhost` line (no port) in `~/.ssh/known_hosts` from older tooling — remove it, then seed all three algorithms for `[localhost]:<podman-machine-port>` with the snippet above. See [Setup Guide → SSH known_hosts](setup.md#ssh-known_hosts-strict-host-key-checking) for the full troubleshooting recipe.

To run a single test:

```bash
QQD_INTEGRATION=1 go test ./internal/qqd/ -count=1 -run TestIntegrationCaddyHTTPRouting -v
```

Tests use the project name `qqd-test` and a published port range starting at `19080`. The cleanup function (`integCleanup` in `integration_test.go`) tears down state before AND after each test, but if a test panics mid-run you may need to manually clean stale containers / units.

## What is covered today

| Test | Coverage |
|---|---|
| `TestIntegrationInitDeploy` | Full init -> deploy -> idempotent re-deploy with httpd + alpine |
| `TestIntegrationZeroDowntime` | Single-replica HTTP service blue-green slot switch |
| `TestIntegrationTCPPassthroughNotSlotted` | TCP-exposed service restarts in place, not with a slot |
| `TestIntegrationMultiServiceHTTPRouting` | Path-based routing across multiple services |
| `TestIntegrationDependsOn` | systemd dependencies between services |
| `TestIntegrationDestroy` | `qqd destroy` removes units + containers cleanly |
| `TestIntegrationDestroyAfterSlotDeploy` | Destroy after a slotted deploy doesn't leak slot files |
| `TestIntegrationConfigChangeRestartsProxy` | Proxy restart is triggered by route changes |
| `TestIntegrationZeroDowntimeSlotDeploy` | Slot deploy keeps old slot serving until new one is healthy |
| `TestIntegrationStatusSlotDeploy` | `qqd status` reports the active slot, not the inactive one |
| `TestIntegrationDependsOnSlottedService` | Slotted service still satisfies dependents |
| `TestIntegrationMultilineEnvInQuadlet` | Multi-line env values survive Quadlet generation |
| `TestIntegrationStaleSlotCleanup` | Old slot files are removed when their service is removed |
| `TestIntegrationCaddyHTTPRouting` | **(new)** Caddy proxy: deploy, Caddyfile mounted, traffic flows |
| `TestIntegrationRollbackAfterFailedHealth` | **(new)** Auto-rollback when v2 health check never passes |

## Gaps

The following are explicitly NOT covered yet. PRs welcome.

- **TLS routing.** The Traefik and Caddy providers have unit tests for TLS config rendering, but nothing verifies that `https://...` actually serves the right cert end-to-end.
- **Rollback after failed start.** `TestIntegrationRollbackAfterFailedHealth` covers the health-timeout path. We don't yet have a test for "container fails to start" (image pull error, OOM, immediate exit).
- **Multi-target deploy.** All tests use a single target. Cross-target service placement and per-target overlays aren't exercised end-to-end.
- **Build-host strategy.** Tests use `Build.Strategy: "local"` only.
- **Caddy TCP rejection.** Unit tests cover the validation rejection (`TestValidateRejectsCaddyTCPPassthrough`). No integration test is needed because the deploy is rejected at config-load time and never reaches a real Caddy container.

## CI

There is **no CI workflow that runs the integration suite** today. The standard `ci.yml` runs only unit tests (the ones that pass without `QQD_INTEGRATION=1`).

Adding integration to CI requires either:

- A self-hosted runner with rootless Podman pre-installed.
- A GitHub Actions hosted runner with Podman installed during the job. The cost is roughly 2-5 minutes of CI time per run.

If you want to volunteer either, file a PR. Until then, the integration suite is run manually before release changes are merged.

## Writing new integration tests

Look at `TestIntegrationCaddyHTTPRouting` in `internal/qqd/integration_caddy_rollback_test.go` for a small, modern template. Conventions:

- Always start with `skipIfNoIntegration(t)`.
- Always call `integCleanup(t, exec)` once eagerly and once via `t.Cleanup` to ensure state is clean before AND after.
- Use `integProject` (`qqd-test`) as the project name so cleanup matches.
- Use `integApp(t, exec)` to build the App with the integration executor wired in.
- Prefer `waitForHTTP` over fixed `time.Sleep`; the harness times out cleanly.
- Use `assertUnitActive`, `assertContainerRunning`, `assertFileExists` / `assertFileContains` rather than re-implementing checks.
