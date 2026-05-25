# Limitations

`qqd` is a deployment CLI for VMs. It does a specific set of things well and explicitly does not do others. Read this page before adopting `qqd` for a workload that depends on something not on the "what it does" list.

## What `qqd` does

- Deploy containerized services to one or more VMs over SSH.
- Manage container lifecycle on the target via one of two backends, picked per-target:
  - **systemd**: Podman Quadlet `.container` units driven by `systemctl --user`. Strongest reboot-survival semantics.
  - **direct**: `podman run --restart=always` with `qqd.*` container labels. No `systemctl` required - works on macOS local, distroless / Alpine without OpenRC, and nested CI. See [docs/lifecycle.md](lifecycle.md).
- Build images on the target, on a build host, or pull pre-built images from a registry.
- Route HTTP and TCP traffic via Traefik or Caddy, configured automatically.
- Track release history per target and roll back the previous deploy with one command.
- Import a `docker-compose.yaml` and migrate from a running Compose or Swarm stack into Podman.
- Provide deployment plans, validation, and target diagnostics.

## What `qqd` does not do

### Orchestration

- **No cross-host scheduling.** Each service is placed on a target you name explicitly. There is no scheduler, no autoscaler, no bin-packer.
- **No automatic failover between targets.** If a target host is down, services on it are down. `qqd` will not redeploy them elsewhere.
- **No service mesh.** Inter-service communication is per-target Podman DNS. There is no cross-host service discovery beyond what your proxy or DNS provides.

### State and data

- **`qqd` is not a backup tool.** Volumes are mounted, not snapshotted. Rollback restores the previous container image, not the data inside the volume.
- **`qqd` is not a database migration runner.** If your deploy includes a schema migration, run it from a `pre_deploy` or `post_deploy` hook. `qqd` will not undo it on rollback.
- **No volume migration.** When moving services between targets or from Docker to Podman, you are responsible for the data in volumes. `qqd migrate` chowns mount points for rootless Podman; it does not move them.

### Build and CI

- **No remote build cache.** Builds reuse the target's Podman cache; build-host and build-on-target strategies do not share cache between hosts.
- **No multi-arch image build.** `qqd build` produces an image for the target architecture only. Use a CI pipeline if you need multi-arch images.

### Lifecycle backend tradeoffs

- **Reboot ordering.** Under `lifecycle: direct`, Podman restarts every container with `--restart=always` after a host reboot, in arbitrary order. systemd's ordered dependency boot is only available under `lifecycle: systemd`. For tightly coupled stacks that fail without strict boot order, use systemd. For stacks where services retry their dependencies anyway (the typical case), either backend is fine.
- **HTTP zero-downtime today.** Under `lifecycle: systemd`, single-instance HTTP-exposed services use slot-based blue-green for true zero traffic drop. Under `lifecycle: direct`, the same services use atomic container replace (`podman rm -f && podman run`); there is a brief sub-second window where the proxy can't reach the service. Acceptable for demo / dev / CI; for production zero-downtime guarantees, use systemd or pin replicas >= 2 (rolling restart applies in both modes).
- **Quadlet-only features** (auto-update, sd-notify, socket activation) are unavailable in direct mode. qqd's deploy flow does not expose these today, but raw env / volume tricks that opt into them stop working.

### Proxy

- **Raw TCP passthrough is not supported on Caddy.** Caddy's built-in `reverse_proxy` is HTTP-only. `qqd validate` rejects any config that combines `proxy: caddy` with a raw TCP expose entry, so the deploy never starts. Use Traefik (`proxy: traefik`) for non-HTTP workloads. See [docs/proxy-caddy.md](proxy-caddy.md).
- **No per-route metrics dashboard.** `qqd` configures the proxy but does not expose its dashboard or metrics endpoint by default.

### Observability

- **No log shipping.** Container logs go to journald. If you need centralized logging, ship them yourself with `journald-to-X` tools.
- **No metrics collection.** `qqd doctor` is a one-shot health check, not a metrics agent.

### Rollback scope

`qqd rollback` restores:
- The service image (back to the previous release's image).
- The generated systemd units.
- The proxy config (so traffic goes to the previous version).

`qqd rollback` does **not** restore:
- Volume contents.
- Database schema or data changes.
- External side effects from `pre_deploy` / `post_deploy` hooks (sent emails, queued jobs, registered webhooks).
- Anything outside the project's container/unit/proxy state.

If your service depends on any of the above, treat rollback as a "stop the bleeding" command and follow up with whatever data recovery your stack requires.

## Currently asserted but not yet proven

The following are claimed in the README but not fully validated by tests as of this writing. Treat them as best-effort until [docs/claims.md](claims.md) marks them green.

- Caddy provider TLS parity with Traefik. HTTPS configuration is covered by unit tests; an end-to-end HTTP integration test exists (`TestIntegrationCaddyHTTPRouting`), but no end-to-end TLS test yet.
- Rolling restart correctness under every service shape without race conditions.
- Migration safety for non-trivial Compose/Swarm stacks. `qqd migrate --dry-run` previews actions and the command now prompts before destructive operations, but no fixtures exercise multi-service real-world Compose files yet.

If you depend on any of these in production, do your own validation. Issues and integration test contributions are welcome.
