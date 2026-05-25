# Production Checklist

Run through this before pointing `qqd` at a production target the first time. Skip steps at your own risk; this list exists because every item has tripped a real deploy somewhere.

The list is ordered: each step builds confidence for the next. If a step fails, fix it before proceeding.

## Decide and document

- [ ] **Confirm the runtime.** qqd deploys with Podman. Leave `runtime` omitted or set `runtime: podman`; `runtime: docker` is rejected. To migrate from a running Compose or Swarm stack, use `qqd migrate --from compose` (or `--from swarm`) with `--dry-run` first.
- [ ] **Pick a proxy.** Traefik (HTTP + TLS + raw TCP) or Caddy (HTTP + TLS only; `qqd validate` rejects raw TCP). If any service needs raw TCP (Postgres, Redis, custom protocols), use Traefik.
- [ ] **Document the target's prerequisites.** SSH user, Podman version, systemd version, expected user lingering state. Read [docs/setup.md](setup.md).
- [ ] **Decide on host key verification.** `qqd` enforces host-key verification by default. Pre-populate `~/.ssh/known_hosts` for the SSH user that runs `qqd` (CI, your laptop). Do **not** set `insecure_host_key: true` on production targets.

## Verify the local toolchain

- [ ] Install `qqd` from a release: `curl -fsSL https://raw.githubusercontent.com/pilshchikov/qqd/main/install.sh | sh`. The installer verifies the SHA-256 checksum of the downloaded binary; abort if it complains.
- [ ] `qqd --version` prints the version you expect.
- [ ] `qqd --help` shows every command and flag you plan to use.

## Verify the target

Run these from the machine that will deploy. Each is read-only.

- [ ] `qqd doctor -c app.yaml -t prod` — passes without error. Fix any reported issues before proceeding.
- [ ] `qqd validate -c app.yaml -c secrets.yaml` — passes without error. (Warnings about mutable image tags are tolerable for first deploy; treat them as TODOs.)
- [ ] SSH to the target manually and run `systemctl --user is-active default.target`. Make sure the user systemd session is healthy.
- [ ] Check that user lingering is enabled so services survive your SSH logout: `loginctl show-user $USER | grep Linger`. Enable with `sudo loginctl enable-linger $USER`.

## Plan the deploy

- [ ] `qqd plan -c app.yaml -c secrets.yaml -t prod` — review every line. Pay attention to:
  - Build vs pull actions per service.
  - Deploy strategy per service ("zero-downtime slot", "rolling", "restart if changed").
  - **Risks block** at the bottom. `info` and `warn` should be acknowledged; `danger` should be fixed before deploying. The plan exposes missing health checks, mutable image tags, and Caddy TCP danger.
- [ ] `qqd plan -c app.yaml -c secrets.yaml --output json -t prod | jq '.risks'` — wire this into CI as a gate that fails on `danger`:
  ```bash
  qqd plan -c app.yaml --output json -t prod \
    | jq -e '.risks | map(select(.level=="danger")) | length == 0'
  ```

## Confirm safety nets

- [ ] **Health checks** are configured for every HTTP-exposed service (`health: { path: …, port: … }`). Without them, blue-green and rolling deploys can't wait for readiness and may cut traffic to a starting container. See [docs/safety-model.md](safety-model.md).
- [ ] **Rollback image retention.** `qqd` keeps the last 10 releases by default. If you `qqd clean` aggressively, old images may be gone when rollback needs them. Test: `qqd history -c app.yaml -t prod` after a couple of deploys.
- [ ] **Volume backup policy** is in place. `qqd rollback` does NOT restore volume data. If a deploy corrupts the database, rollback restores the previous container image but the corrupted data remains. Snapshot or replicate before risky deploys.
- [ ] **Image tags are immutable.** `:latest`, `:main`, `:edge` mean rollback may pull a different image than was originally deployed. Pin to a digest (`@sha256:…`) or a versioned tag for production services.
- [ ] **`pre_deploy` and `post_deploy` hooks** are idempotent. They run on every deploy and can re-fire on auto-rollback.

## First deploy

- [ ] First deploy is `qqd init`, not `qqd deploy`. `init` does the one-time setup (clone, build, install units).
- [ ] Watch `journalctl --user -u 'my-project-*'` on the target while the deploy runs.
- [ ] Verify routing: `curl` the published port from inside and outside the target.
- [ ] `qqd status -c app.yaml -t prod` — every service shows "active" and the expected image.
- [ ] `qqd history -c app.yaml -t prod` — there's a release entry.

## Routine deploy

- [ ] Always `qqd plan` before `qqd deploy` for non-trivial changes. The plan catches new risks (e.g. a tag downgrade from immutable to `:latest`).
- [ ] Use `--approve` only in CI / scripts. Interactive use should keep the confirmation prompt.
- [ ] Deploys take the per-target lock. If two CI workflows can race, configure GitHub Actions concurrency groups. Lock state lives at `~/.qqd/locks/<project>.lock` on the target; clear stale locks with `--force-unlock` only when you're sure no other deploy is running.

## Migration (Compose/Swarm → Podman)

- [ ] **Always** start with `qqd migrate --dry-run`. Read every `[would run]` line. Verify the volume paths it lists for `chown -R`.
- [ ] If migrating from Swarm, understand that `docker swarm leave --force` will run by default. If your target is part of a multi-node swarm, this affects the whole cluster.
- [ ] Run with `--yes` only after a clean dry-run and a brief outage window. Migration is not a routine command.
- [ ] After migration, run the full first-deploy checklist above against the new runtime.

## Emergency commands

Keep these in your runbook:

```bash
qqd status -c app.yaml -t prod        # what is running, image, uptime
qqd history -c app.yaml -t prod       # last 10 releases with timestamps
qqd logs -c app.yaml -t prod <svc>    # container logs
qqd rollback -c app.yaml -t prod      # restore previous release (image + config)
qqd doctor -c app.yaml -t prod        # diagnose target health

# Manual recovery on the target:
ssh user@prod
systemctl --user status 'my-project-*'        # Podman
journalctl --user -u my-project-web.service   # Podman
```

If a deploy hangs or `qqd` crashes mid-deploy, check whether the lock dir was left behind: `ssh user@target ls -la ~/.qqd/locks/<project>.lock`. If yes, and you're sure no other deploy is running, the next `qqd` invocation can clear it with `--force-unlock`.

## Pre-release check (for `qqd` maintainers, not users)

Before merging to `main` or `master`:

- [ ] `go test ./internal/qqd/ -count=1 -race` passes.
- [ ] `go vet ./...` passes.
- [ ] `./qqd docs -o docs/cli-reference.md && git diff --exit-code docs/cli-reference.md` is clean.
- [ ] `QQD_INTEGRATION=1 go test ./internal/qqd/ -count=1 -run TestIntegration -v` passes against a real Podman target.
- [ ] `make release VERSION=vYYYY.MM.DD.N` builds direct binaries and `checksums.txt`.
- [ ] `install.sh` succeeds against the published release with checksum verification.
- [ ] [docs/claims.md](claims.md) is reviewed and updated if any row's evidence level changed.
