# Claim Matrix

This page lists each headline claim about `qqd` and the current evidence backing it. The goal is to be honest about what is continuously verified vs. tested by hand vs. asserted but unproven.

## Status legend

A deployment tool's users decide what to trust based on **how the proof is renewed**, not just whether it exists once. So the legend distinguishes evidence levels.

- **Proven in CI** — covered by tests or builds in GitHub Actions. PR CI runs build, race-enabled unit tests, `go vet`, and generated-doc drift checks; the release workflow runs tests and release binary builds on pushes to `main` or `master`.
- **Proven by opt-in integration** — covered by tests under `internal/qqd/integration*_test.go` that run only when `QQD_INTEGRATION=1` is set against a real Podman target. Not run by CI today; run by hand before release changes are merged. See [docs/integration-tests.md](integration-tests.md).
- **Implemented** — code and unit tests exist; no end-to-end proof against a real target.
- **Partial** — supported for some cases with documented gaps.
- **Documented only** — docs describe the intended behavior, but neither code coverage nor tests prove it.

Today, **"Proven in CI" should be rare** — it's reserved for things GitHub Actions continuously exercises (config parsing, release binary builds, lock primitives). Most runtime/proxy/deploy claims sit at "Implemented" or "Proven by opt-in integration."

## Top-line claims

| Claim | Status | Evidence | Gap |
|---|---|---|---|
| Single binary | Proven in CI | `go build ./cmd/qqd` produces one self-contained CLI binary; only `golang.org/x/crypto` external dep; PR CI builds it and the release workflow cross-builds linux/darwin x amd64/arm64 | None |
| Direct release binaries | Proven in CI | `.github/workflows/release.yml` runs `make release` on every push to `main` or `master`; assets are direct executables (`qqd_linux_amd64`, etc.) plus `checksums.txt` | None |
| Latest-release installer | Implemented | `install.sh` resolves the latest GitHub release, downloads the matching direct binary for the user's OS/architecture, verifies `checksums.txt`, and installs it as `qqd` | Needs a published release to test end-to-end from GitHub |
| No long-running daemon | Proven by opt-in integration | No qqd daemon is installed; integration cleanup verifies no managed process is left behind after commands, and the lock implementation has unit tests | None |
| SSH-based deploys | Implemented | `internal/qqd/executor.go` has unit tests; manual validation in `test-deploy/` scratch dir; no CI runner with a real SSH target | Add a CI workflow that exercises SSH against a localhost sshd container |
| YAML, JSON, or HOCON config | Proven in CI | All three parsers have unit tests; YAML parser rejects unsupported features (anchors, aliases, multi-line scalars, tags, merge keys, flow maps, multi-document) instead of silently misinterpreting; round-trip tests catch emitter/parser drift; subset documented in [yaml-subset.md](yaml-subset.md) | Custom YAML parser by design (zero deps). If your config needs full YAML, use JSON. |
| Podman runtime | Proven by opt-in integration | Quadlet generation has unit tests; opt-in integration suite covers init, deploy, blue-green slot, rolling, depends-on, destroy, status, stale-slot cleanup, Caddy HTTP routing, and rollback-after-failed-health under `QQD_INTEGRATION=1` | No CI runner for the integration suite yet (run manually before release) |
| Traefik proxy | Proven by opt-in integration | Provider in `proxy.go` with unit tests; covered indirectly by every integration test that uses the default proxy (HTTP routing, multi-service, depends-on) | No dedicated TLS or TCP-passthrough integration test |
| Caddy proxy | Proven by opt-in integration | Provider in `caddy.go` after audit; Caddyfile-only model documented in [proxy-caddy.md](proxy-caddy.md); dead static-JSON config removed; file renamed routes.json → Caddyfile to match content; `TestIntegrationCaddyHTTPRouting` proves end-to-end deploy + traffic flow under `QQD_INTEGRATION=1` | TCP passthrough is NOT supported on Caddy: `qqd validate` rejects the combination at config-load time. No end-to-end Caddy TLS integration test yet. |
| Zero-downtime deploys | Partial | Blue-green and rolling logic in `slot.go` and `restart.go` with unit tests; integration suite covers slot deploy, rolling drain, and rollback-after-failure | "Zero downtime" applies only to specific service shapes (HTTP-exposed or replicated with health check); see [safety-model.md](safety-model.md). No end-to-end test that measures dropped requests. |
| Auto rollback on failed health | Proven by opt-in integration | `release.go` saves releases; deploy logic triggers rollback on health-check failure; `TestIntegrationRollbackAfterFailedHealth` proves the rollback restores the previous image and serves traffic | No coverage for "container fails to start" rollback path (image pull error, OOM, immediate exit) |
| Manual rollback | Implemented | `qqd rollback` restores previous release; rollback scope (image and proxy config; not data) documented in [limitations.md](limitations.md) | No dedicated integration test for `qqd rollback` (auto-rollback is exercised; manual is not) |
| Per-target release history | Proven in CI | Per-target release files; `release_test.go` covers save, list, trim, rollback selection | None |
| Multi-target service placement | Implemented | Per-target `services` and `expose` overlays; `config_test.go` exercises overlay merging | No integration test deploys to multiple targets |
| Compose import | Implemented | `compose_import.go` with unit tests on small fixtures | No fixtures from real-world Compose files (anchors, env interpolation, multi-stage, named networks) |
| Migration from Compose / Swarm to Podman | Partial | `compose_migrate.go` has unit tests for the dry-run path; `--dry-run` and explicit destructive-action confirmation now in place | No fixture tests against real Compose/Swarm files; `docker swarm leave --force` still affects the whole cluster (not scoped to one stack); no `--skip-network-prune` or `--skip-swarm-leave` opt-outs yet |
| `plan` shows what would happen | Proven in CI | `qqd plan` lists service changes, env, and risks; risk detection (mutable tags, missing health, Caddy TCP, etc.) is unit-tested in `plan_risks_test.go`; `--output json` emits a stable schema | None |
| `validate` catches config errors | Proven in CI | `validate.go` with unit tests; rejects Caddy + raw TCP, missing TLS material, undefined depends_on, cycles, out-of-range ports, missing health-port inference, build-strategy gaps, mutable tags (warn) | See [safety-model.md](safety-model.md) for the exact list |
| `doctor` diagnoses target issues | Implemented | `doctor.go` with unit tests | No targeted modes (`--runtime`, `--proxy`, `--service`, `--json`) |
| Safety first (`plan`, `validate`, `doctor`, `--dry-run`, deploy lock) | Proven in CI | All four exist; per-target deploy lock (`lock.go`) tested in `lock_test.go`; `plan` surfaces info/warn/danger risks (`plan_risks.go`) and supports `--output json` for CI gates; `validate` rejects known-broken combinations | None |

## Runtime support

| Feature | Podman |
|---|---|
| Quadlet `.container` units | Yes |
| Direct lifecycle with `podman run` | Yes |
| Generated unit example in docs | Yes (in `setup.md`) |
| Opt-in integration coverage | Yes |
| CI integration coverage | No (gap) |
| Documented setup guide | Yes ([setup.md](setup.md)) |
| Volume ownership handling | Yes (auto chown for rootless during Docker-to-Podman migration) |

## Per-proxy support matrix

| Feature | Traefik v3 | Caddy v2 |
|---|---|---|
| HTTP routing by path | Proven by opt-in integration | Proven by opt-in integration |
| TLS termination | Implemented (unit-tested) | Implemented (unit-tested) |
| TCP passthrough | Implemented | **Not supported** — `qqd validate` rejects |
| Multiple replicas (load balance) | Implemented | Implemented |
| Reload on route change | Tested (file watcher) | systemctl restart of proxy unit (admin API not used) |
| CI integration coverage | No (gap) | No (gap) |
| Documented troubleshooting | Partial (in [operations.md](operations.md)) | Yes ([proxy-caddy.md](proxy-caddy.md)) |

## How to read this page

This is a living document. When a status changes (gap closed, new test added, regression discovered), update it in the same PR. Today's bar to graduate from "Proven by opt-in integration" to "Proven in CI" is wiring the integration suite into a CI runner.
