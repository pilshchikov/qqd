# Lifecycle Backends

`qqd` manages container processes through one of two backends. The choice is
controlled by the per-target `lifecycle` field; auto-detection picks one
when you don't.

## TL;DR

| `lifecycle:` | Mechanism                                                                | Use this when                                  |
| ------------ | ------------------------------------------------------------------------ | ---------------------------------------------- |
| `auto`       | Probes the target. systemd if `systemctl` works, else direct.            | Default. Good for everywhere.                  |
| `systemd`    | Writes Podman Quadlet units, manages via `systemctl --user`. | Linux VMs with systemd. Strongest reboot survival. |
| `direct`     | Drives `podman run --restart=always` directly with `qqd.*` labels. No `systemctl`. | macOS local with Podman Machine, minimal Linux without systemd, nested CI. |

The same `qqd` commands (`init`, `deploy`, `status`, `rollback`, `update`,
`destroy`, `logs`, ...) produce the same observable outcome under either
backend. The mechanism differs; the result does not.

## When to use which

### `lifecycle: auto` (default)

The first mutating command for each target probes the SSH executor with
`command -v systemctl >/dev/null 2>&1 && systemctl --version >/dev/null 2>&1`.
On success the target uses systemd. On failure it falls back to direct.
Result is cached per-target for the duration of the qqd invocation.

Pick this when you want the same config to work on a Linux VM, a Mac, and
a CI runner without per-environment forks.

### `lifecycle: systemd`

Force systemd. Useful when:

- You want Quadlet auto-update, sd-notify, socket activation, ordered
  dependency boot, or other systemd-only features.
- You want `qqd doctor` to *fail* on a target that lost systemd, instead
  of silently degrading to direct.

This is what `qqd` did exclusively before the direct backend existed; if
you have a working systemd deploy today, set this and behavior is
guaranteed unchanged.

### `lifecycle: direct`

Force the direct backend. Useful when:

- Your target has no systemd reachable from the SSH user (macOS local,
  distroless / Alpine without OpenRC, scratch containers running CI jobs).
- You're recording a demo or running an integration test on a developer
  laptop and want a single config that works everywhere.
- You want labels on every container so external tooling (Prometheus
  cAdvisor, log aggregators) can join container metadata back to the qqd
  project / service / release.

## What's the same

- `qqd plan` shows the same per-service decisions and the same risks
  list. Direct mode adds one extra `info`-level risk noting that reboot
  survival relies on the runtime's `--restart` policy instead of systemd.
- `qqd deploy`, `qqd update`, `qqd init` install the same containers
  under the same names with the same images, env, volumes, ports, and
  health checks.
- `qqd status` returns the same JSON shape (with one extra
  `target.backend` field that reads `"systemd"` or `"direct"`).
- `qqd rollback` restores the previous release the same way: pull the old
  image, recreate the container with the previous spec.
- `qqd logs` already uses `podman logs` directly, so it
  works under either backend with no change.
- `qqd destroy` removes everything qqd installed for the project on the
  target. Under direct mode it lists containers by `qqd.project=<name>`
  label and removes them; under systemd it stops + disables units and
  removes the unit files.

## What's different

### Reboot survival

- **systemd**: services come back in dependency order on host boot. If
  service A `Requires=` service B, A waits for B. systemd is the
  authoritative orchestrator.
- **direct**: every container is started with
  `--restart=always`. The
  container daemon restarts everything in *arbitrary* order. For tightly
  coupled services there can be a brief window where one service is up
  and another isn't yet. Real-world impact is usually zero (services are
  expected to retry their dependencies anyway), but if your stack assumes
  ordered boot, prefer systemd.

### Zero-downtime blue-green

- **systemd**: HTTP-exposed single-instance services use a slot-based
  blue-green: a `<svc>-<slot>` unit is started, the proxy file watcher
  picks up the new backend, the old slot unit is stopped. Zero traffic
  drop in the steady state.
- **direct (today)**: HTTP-exposed services use atomic container replace
  (`podman rm -f <svc> && podman run ... --name <svc>`). There is a
  brief window (typically <1s) where the proxy can't reach `<svc>`. For
  most demo / dev / CI workloads this is acceptable; for production
  zero-downtime guarantees, prefer `lifecycle: systemd`. Future versions
  may add direct-mode blue-green.

### Quadlet-only features

Quadlet's auto-update, sd-notify, and socket activation features are not
available in direct mode. They are not used by qqd's deploy flow today
either, but if you opt into them via raw env / volume tricks you'll lose
them when the backend is direct.

## What goes into a labeled container

In direct mode, every container qqd creates carries these labels:

```text
qqd.project       = <project name>
qqd.service       = <logical service name>      e.g. "api"
qqd.replica       = "1" / "2" / ...
qqd.role          = "app" or "proxy"
qqd.image         = <image:tag>
qqd.deploy_id     = <release id assigned at deploy time>
qqd.config_hash   = <16-char sha256 of the effective spec>
qqd.image_id      = <sha256 of the resolved image>  (when known)
```

This makes `podman ps --filter label=qqd.project=myapp` the canonical
"what is qqd running for this project on this host" query.

## Diagnosing the choice

`qqd doctor` prints the selected backend per target along with the
reason:

```text
target=alpha host=local
  ✓ lifecycle backend: direct (auto: systemctl missing)
  ✓ ssh connectivity
  ✓ podman
  ...
```

`qqd status` includes the backend in its header line and (for JSON
output) under each `targets[].backend`.

`qqd plan` lists `lifecycle: direct` as an info-level risk so you see in
the plan whether systemd or direct will apply.

## Migrating between backends

Switching `lifecycle` mid-flight on a deployed target requires a deliberate
hand-off:

- **systemd → direct**: run `qqd destroy` (which under the still-systemd
  setting tears down units), change the field to `direct`, run
  `qqd deploy`. The new containers come up with `qqd.*` labels.
- **direct → systemd**: same shape: `qqd destroy` removes the labeled
  containers, change the field to `systemd`, `qqd deploy` writes unit
  files and starts via systemctl.

There is no in-place migration today. If you're hitting this on a live
target, plan for a short outage during the swap.

## See also

- [docs/configuration.md](configuration.md) — full config reference.
- [docs/safety-model.md](safety-model.md) — what qqd does and does not
  guarantee, broken out by command.
- [docs/limitations.md](limitations.md) — known gaps, including the
  direct-mode blue-green and reboot-ordering tradeoffs above.
