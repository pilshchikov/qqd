# Safety Model

This page describes what `qqd` guarantees, what it tries hard to do, and what can still go wrong. Read this before using `qqd` against a production target.

## Guarantees

A "guarantee" here means: there is code that enforces this, and the test suite covers it. Something failing this list is a bug.

- **Plan and `--dry-run` do not mutate the target.** No SSH command other than read-only inspection (e.g., `podman ps`, `systemctl status`) runs.
- **`qqd validate` does not connect to any target.** It is a pure-config check.
- **`qqd doctor` is read-only.** It checks SSH reachability, container runtime presence, systemd state, and disk space. It does not install anything or modify state.
- **SSH host-key verification is on by default.** Connecting to an unknown host fails unless `insecure_host_key: true` is set on the target.
- **Generated unit files are written atomically.** A unit file is staged to a temp path, then renamed - it is never half-written.
- **Idempotent deploys.** Running `qqd deploy` against an already-deployed target with no config changes is a no-op (no image rebuild, no service restart).
- **Plan and `--dry-run` redact secrets.** Values for keys matching common secret patterns (`*_TOKEN`, `*_SECRET`, `*_PASSWORD`, `*_KEY`, etc.) are masked. See `internal/qqd/redact.go` for the full pattern list.

## Best-effort behavior

A "best-effort" item is intentional and well-tested for happy paths but has known failure modes documented below.

### Zero-downtime restart

`qqd` picks a restart strategy automatically based on the service's `expose` and `replicas`:

| Service shape | Strategy | Zero downtime? |
|---|---|---|
| HTTP-exposed, single replica | Blue-green | Yes for HTTP requests; in-flight TCP connections drop on slot switch |
| HTTP-exposed, replicated | Rolling with proxy drain | Yes if health check is configured and accurate |
| Health check + replicated, no expose | Rolling restart, wait healthy between | Brief gap per replica; clients must retry |
| Anything else (no replicas, no expose) | Direct restart | No - service is unavailable for the restart window |

Failure modes:
- A wrong or missing health check turns rolling deploys into "fast restarts that look healthy." The proxy will route to a starting-up container.
- Blue-green requires the proxy to actually reload its config before the old slot stops. Verify reload behavior if you customize the proxy config.
- Long-lived TCP connections (websockets, gRPC streams) are dropped on slot switch. `qqd` does not implement connection draining at the application layer.

### Rollback

`qqd rollback` restores the previous release: image tags, generated unit files, proxy config. See [docs/limitations.md](limitations.md#rollback-scope) for what rollback does not restore.

Failure modes:
- If the previous image has been deleted from the registry and the local cache, rollback will fail at the pull step. `qqd clean` is aggressive about removing unused images - use `--keep-images N` if you rely on rollback to old releases.
- Auto-rollback triggers when a deploy fails between unit start and health check. If `qqd` itself crashes (panic, killed mid-deploy, lost SSH), there is no rollback. The target is left in whatever state the last completed step put it in.

### Migration

`qqd migrate` stops a running Compose or Swarm stack and replaces it with a Podman-backed `qqd` deploy.

Known sharp edges:
- `qqd migrate --dry-run` prints every destructive action without executing it; **always run dry-run first** on a production-like target. Confirmation is required by default; pass `--yes`/`-y` only after a clean dry-run.
- `docker swarm leave --force` is invoked unconditionally during Swarm migration. If the target is part of a multi-node swarm, this will affect the whole cluster.
- Volume `chown -R` for rootless Podman runs against any host path declared in `volumes:`. Make sure no other process is using those paths.
- `docker network prune -f` removes ALL unused networks on the host, not just those in your project.

## Failure-mode catalog

What happens when each step of `deploy` fails:

| Step fails | What `qqd` does | What you may need to do manually |
|---|---|---|
| SSH connection | Aborts before any change | Check `qqd doctor` |
| Source sync (git/rsync) | Aborts; nothing on target changed | Fix repo access or rsync source |
| Image build | Aborts; previous services still running | Inspect build output; fix Dockerfile |
| Image pull | Aborts; previous services still running | Check registry auth; check network |
| Unit file write | Atomic write fails or is incomplete | Should not happen; file a bug |
| Service start | Auto-rollback to previous image and units | Check `journalctl --user -u <unit>` |
| Health check timeout | Auto-rollback (slot/rolling) or service marked failed (direct restart) | Inspect logs; verify health endpoint |
| Proxy reload | Service is healthy but traffic may be wrong | Restart proxy container manually if stuck |
| `qqd` killed mid-deploy (Ctrl+C, SIGKILL) | No cleanup runs. Target left in the state of the last completed step | Inspect with `qqd status`; resume with `qqd deploy` (idempotent) |

## Concurrent deploys

`qqd` takes a per-project, per-target deploy lock on the remote host before any mutating command (`init`, `deploy`, `build`, `rollback`, `destroy`, `--config-only`). The lock lives at `~/.qqd/locks/<project>.lock` on the target and uses an atomic `mkdir` so it survives across SSH sessions.

If a lock is already held, `qqd` aborts with the holder's metadata (command, PID, local user, local host, timestamp). Override with `--force-unlock` if you are sure the recorded holder is dead - this is unsafe if the holder is actually still running.

What the lock does and does not protect:

- It prevents two concurrent `qqd` mutating commands against the same project/target from racing.
- It does **not** protect against direct `systemctl` / `podman` commands run by hand on the target.
- It does **not** prevent two different projects from deploying to the same target concurrently (lock is per-project).
- If `qqd` is killed mid-deploy (SIGKILL, network drop), the lock dir is left behind and must be cleared with `--force-unlock` on the next attempt. Read-only commands (`status`, `logs`, `history`, `doctor`) do not check the lock.

## What `plan` and `validate` actually check

### `qqd validate`
- Config syntactically loads (YAML/JSON/HOCON parser).
- All referenced env files exist.
- All `file::` references resolve.
- `depends_on` references existing services and is acyclic.
- Build strategy is valid (image tag set, or Dockerfile present, or build-host configured).
- TLS cert/key files exist if `tls:` is set.
- Port numbers are valid integers in range.
- Mutable image tags warn (not error).
- **`proxy: caddy` + raw TCP expose entry hard-fails.** Caddy's built-in `reverse_proxy` is HTTP-only; a raw TCP entry would silently misbehave. See [docs/proxy-caddy.md](proxy-caddy.md).

### `qqd plan`
- Everything `validate` does, plus:
- For each target: which services would be built vs pulled, which would restart, which deploy strategy each would use.
- Per-service environment after overlay merging (with secrets redacted).

### `qqd doctor`
- SSH reachability and auth.
- Podman present and version.
- systemd available with the user session when the systemd lifecycle is selected.
- Disk space on the target.
- Lingering enabled (Podman user services).

What `plan`/`validate`/`doctor` do **not** check:
- Whether the image will actually start.
- Whether the health check endpoint will respond.
- Whether the proxy can resolve the upstream hostnames.
- Whether your deploy hooks will succeed.
- Network connectivity from the target to external dependencies.

## Recovery

If a deploy leaves the target in an unclear state:

```bash
qqd status -c app.yaml -t prod         # what is running, what unit state
qqd history -c app.yaml -t prod        # last N releases with timestamps
qqd logs -c app.yaml -t prod <service> # container logs
qqd rollback -c app.yaml -t prod       # restore previous release
```

If `qqd` itself is the problem (panic, hang), it is safe to Ctrl+C - there is no daemon to leave behind, and the target is in some intermediate state that `qqd status` and `qqd history` will help you diagnose. The `clean` and `destroy` commands are sharper tools; read their help text before using them on production.
