# Zero-Downtime Deployments

When a service's image changes, qqd automatically selects a deployment strategy based on the service's configuration. No flags or annotations needed.

## Strategy Selection

| Service type | Strategy | How it works |
|---|---|---|
| HTTP-exposed, non-replicated | **Blue-green** | Start new container alongside old, switch Traefik, stop old |
| Exposed, replicated | **Rolling with drain** | Remove replica from Traefik, restart, wait healthy, add back |
| Non-exposed, replicated + health | **Rolling restart** | Restart one replica at a time, wait healthy |
| Everything else | **Direct restart** | `systemctl restart` |

## Blue-Green Deployment

For HTTP-routed non-replicated services, qqd runs two containers alternately - a "blue" slot and a "green" slot.

### Flow

1. First `init` creates a standard container (`my-app-server`)
2. On `deploy` with image change, qqd creates `my-app-server-blue` alongside the old one
3. Waits for readiness (health check or `startup_delay`, default 5s)
4. Rewrites Traefik dynamic routes to point at the new container - Traefik auto-reloads
5. Brief drain wait for in-flight requests
6. Stops the old container, removes its Quadlet file
7. Next image change creates the green slot, switches, removes blue - and so on

### Status

`qqd status` shows the active slot:

```
  server (blue): active ghcr.io/org/app/server:1.1 (2026-02-27 03:27:21 UTC, up 25m)
```

### Applicability

Blue-green only applies to **HTTP-routed** services (path routes in `expose`). TCP passthrough services use direct restart since they typically have stateful or long-lived connections. Services without an `expose` entry also use direct restart.

If a service depends on a blue-green service (`depends_on`), its Quadlet file is automatically updated to reference the active slot unit.

## Rolling Restart

For replicated services, qqd restarts one replica at a time. If a health check is configured, it waits for each replica to become healthy before moving to the next.

For exposed replicated services, qqd additionally drains traffic before restarting - it removes the replica from Traefik's routing, waits briefly, then restarts.

## Health Checks

```hocon
services {
  server {
    health { path = "/api/health", port = 8080 }
  }
}
```

Containers get a Podman health check (`curl -sf http://localhost:<port><path>`). During rolling restarts, qqd polls the health status and only proceeds to the next replica when the current one is healthy.

## Startup Delay

For services **without** a health check, qqd waits `startup_delay` seconds (default 5) before switching traffic:

```hocon
services {
  server {
    startup_delay = 10
  }
}
```

This applies to both blue-green and rolling deploys when no health check is defined.
