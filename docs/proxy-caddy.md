# Caddy Proxy Provider

`qqd` supports [Caddy v2](https://caddyserver.com/) as an alternative to the default Traefik provider. Set `proxy: caddy` in your project config:

```yaml
name: my-app
proxy: caddy
```

This page documents what works on Caddy, what's different from Traefik, and what's not supported.

## Configuration model

`qqd` configures Caddy with a single bind-mounted **Caddyfile** (no separate JSON static config). The file lives at:

```
~/.config/qqd/<project>/caddy-routes/Caddyfile
```

and is mounted into the container as `/etc/caddy/Caddyfile`. Listen ports, routes, and TLS are all declared in this one file.

The Caddy admin API is **not enabled** by `qqd`. Reload happens by `systemctl restart`-ing the proxy unit, which kills and respawns the container. This is fine for the deploy cadence `qqd` is designed for; it is not a hot-reload mechanism for high-frequency config changes.

## What works

| Feature | Caddy | Notes |
|---|---|---|
| HTTP routing by path | Yes | Same `expose:` syntax as Traefik. Longest-path-first priority. |
| HTTP routing on multiple ports | Yes | One Caddy server block per host port. |
| TLS termination | Yes | Provide `tls.certs_dir` + `tls.server_name` in the expose entry. Caddy reads the cert and key from `<certs_dir>/live/<server_name>/{fullchain,privkey}.pem`. When `server_name` is a list, the first entry names the cert directory; Caddy's site blocks are port-based, so every hostname in the cert is served. |
| Multiple replicas (load balance) | Yes | Caddy's built-in `reverse_proxy` round-robins between upstreams. |
| Blue-green slot switching | Yes | `qqd` writes a new Caddyfile pointing at the new slot, then restarts the proxy. |
| Rolling drain | Yes | `qqd` excludes the draining replica from the upstream list, restarts the proxy, then restarts the replica. |

## What's different from Traefik

- **No admin API.** Traefik exposes a dashboard and admin endpoint by default; Caddy does not, under `qqd`. If you need a dashboard, use Traefik (set `proxy: traefik` or omit the field).
- **No file-watcher reload.** Traefik's dynamic provider watches `/etc/traefik/dynamic` and reloads on file change without a restart. Caddy under `qqd` reloads via `systemctl restart`, which is one extra second of downtime per config change.
- **One file vs two.** Traefik uses `traefik.yml` (static) plus a `dynamic/` directory; Caddy uses one Caddyfile. This makes Caddy easier to inspect by hand on the target.

## What is NOT supported on Caddy

### Raw TCP passthrough

Caddy's built-in `reverse_proxy` is HTTP-only. **`qqd validate` now rejects** any config that combines `proxy: caddy` with a raw TCP expose entry, so you cannot accidentally deploy a known-broken setup. The error looks like:

```
error: target "main" port 5432 is configured as raw TCP passthrough (target "db:5432") but proxy is Caddy;
       Caddy's built-in reverse_proxy is HTTP-only.
       Use proxy: traefik, or change this entry to an HTTP route. See docs/proxy-caddy.md.
```

If your service is not HTTP (Postgres, Redis, MySQL, raw TCP), use Traefik:

```yaml
proxy: traefik
```

This is a fundamental limitation of the default Caddy image. Adding TCP passthrough would require building a custom image with the `layer4` plugin compiled in. We are not doing that automatically because it doubles the image surface area for a feature most users don't need.

## How to verify

`qqd` ships unit tests for Caddyfile generation in `internal/qqd/caddy_test.go` and an opt-in end-to-end test, `TestIntegrationCaddyHTTPRouting`, that deploys a real Caddy container and asserts traffic flows. Run the integration suite with `QQD_INTEGRATION=1` (see [docs/integration-tests.md](integration-tests.md) for setup).

TLS routing has unit-level coverage but no dedicated end-to-end Caddy TLS test yet. Verify your specific config locally with:

```bash
qqd plan -c app.yaml
```

and inspect the generated Caddyfile after a deploy:

```bash
ssh <target> cat ~/.config/qqd/<project>/caddy-routes/Caddyfile
```

## File reference

| Path on target | Purpose |
|---|---|
| `~/.config/qqd/<project>/caddy-routes/Caddyfile` | Generated Caddyfile, bind-mounted into the container |
| `~/.config/containers/systemd/<project>-proxy.container` | Quadlet unit for the Caddy container |

## Switching between Traefik and Caddy

Changing `proxy:` and re-deploying replaces the proxy container. Existing route configuration is regenerated. There is no migration step needed.

```yaml
# from this:
proxy: traefik

# to this:
proxy: caddy
```

Then:

```bash
qqd deploy -c app.yaml
```

The next deploy notices the proxy provider changed, writes new config files (`Caddyfile` instead of `traefik.yml` + `dynamic/`), and restarts the proxy unit.
