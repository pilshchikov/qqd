# Operations Guide

Step-by-step commands for managing services directly on the target host - useful when qqd is unavailable or for debugging.

All commands below run on the target. SSH in first:

```bash
ssh -i ~/.ssh/key.pem user@host
```

Replace `<project>` with your project name (e.g. `my-app`), `<service>` with the service name.

## Manual Deploy

```bash
# 1. Sync source code
cd /home/user/project
git fetch --all && git reset --hard origin/main

# 2. Build (if Dockerfile-based)
cd /home/user/project/backend
podman build -t ghcr.io/org/repo/server:1.1 -f Dockerfile .

# 2 (alt). Pull pre-built image
podman pull ghcr.io/org/repo/server:1.1

# 3. Update Quadlet file (change Image= line)
vi ~/.config/containers/systemd/<project>-<service>.container

# For replicated services, update ALL replica files:
vi ~/.config/containers/systemd/<project>-<service>-1.container
vi ~/.config/containers/systemd/<project>-<service>-2.container

# 4. Reload and restart
systemctl --user daemon-reload
systemctl --user restart <project>-<service>.service

# 5. Verify
systemctl --user is-active <project>-<service>.service
podman ps --filter name=<project>-<service>
```

## Manual Blue-Green Deploy

Slotted services have files named `<project>-<service>-<hash>.container` where `<hash>` is 8 hex chars.

```bash
# 1. Find current slot
ls ~/.config/containers/systemd/<project>-<service>-*.container

# 2. Build/pull new image
podman build -t ghcr.io/org/repo/server:1.1 -f Dockerfile .

# 3. Create new slot (copy + edit Image= and ContainerName=)
cp ~/.config/containers/systemd/<project>-<service>-OLD.container \
   ~/.config/containers/systemd/<project>-<service>-NEW.container
vi ~/.config/containers/systemd/<project>-<service>-NEW.container

# 4. Start new slot alongside old
systemctl --user daemon-reload
systemctl --user start <project>-<service>-NEW.service

# 5. Switch Traefik routes (auto-reloads, no restart)
vi ~/.config/qqd/<project>/dynamic/routes.yml

# 6. Drain and stop old slot
sleep 3
systemctl --user stop <project>-<service>-OLD.service
rm ~/.config/containers/systemd/<project>-<service>-OLD.container
systemctl --user daemon-reload
```

## Update Environment Variables

```bash
vi ~/.config/containers/systemd/<project>-<service>.container
# Change or add Environment= lines under [Container]
systemctl --user daemon-reload
systemctl --user restart <project>-<service>.service
```

## Stop / Start

```bash
# Stop a service
systemctl --user stop <project>-<service>.service

# Stop all project services
systemctl --user stop '<project>-*.service'

# Start (network first)
systemctl --user start <project>.network
systemctl --user start <project>-<service>.service
```

## Destroy (Full Removal)

```bash
systemctl --user stop '<project>-*.service' 2>/dev/null || true
systemctl --user disable '<project>-*.service' 2>/dev/null || true
rm -f ~/.config/containers/systemd/<project>.network
rm -f ~/.config/containers/systemd/<project>-*.container
rm -rf ~/.config/qqd/<project>
systemctl --user daemon-reload
```

## Clean Up Images and Containers

```bash
# Remove stopped project containers
podman ps -a --filter name='^<project>-' --filter status=exited --format '{{.Names}}' \
  | xargs -r podman rm -f

# Remove a specific old image
podman rmi ghcr.io/org/repo/server:1.0

# Prune dangling images
podman image prune -f
```

## Rolling Restart (Manual)

```bash
for i in 1 2 3; do
  echo "Restarting replica $i..."
  systemctl --user restart <project>-<service>-$i.service
  sleep 10
  podman inspect --format '{{.State.Health.Status}}' <project>-<service>-$i
done
```

## Restart Traefik Proxy

```bash
# Full restart (after changing traefik.yml or adding/removing ports)
systemctl --user restart <project>-proxy.service

# Route changes only (Traefik watches the file, no restart needed)
vi ~/.config/qqd/<project>/dynamic/routes.yml
```

## Create Directories for Volumes

```bash
mkdir -p /home/user/data/postgres
mkdir -p /home/user/data/uploads
```

---

# Diagnostics

## Services

```bash
# List all project units
systemctl --user list-units '<project>-*'

# Detailed status
systemctl --user status <project>-server.service

# Show generated systemd unit from Quadlet
systemctl --user cat <project>-server.service
```

## Containers

```bash
podman ps                          # running
podman ps -a                       # all including stopped
podman ps --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"
podman inspect <project>-server    # image, env, mounts, state
```

## Logs

```bash
journalctl --user -u <project>-server.service -f           # stream
journalctl --user -u <project>-server.service -n 100       # last 100 lines
journalctl --user -u <project>-server.service --since "10 minutes ago"
podman logs <project>-server                                # direct
podman logs --tail 50 <project>-server
```

## Quadlet Files

```bash
ls ~/.config/containers/systemd/
cat ~/.config/containers/systemd/<project>-server.container
cat ~/.config/containers/systemd/<project>-proxy.container
cat ~/.config/qqd/<project>/traefik.yml
cat ~/.config/qqd/<project>/dynamic/routes.yml
cat ~/.config/containers/systemd/<project>.network
```

## Images

```bash
podman images
podman image exists ghcr.io/org/repo/service:1.0
podman system df
```

## Network and DNS

```bash
podman network ls
podman network inspect <project>
podman exec <project>-server getent hosts <project>-db
```

## Health

```bash
curl -sf http://localhost/api/health
podman inspect --format '{{.State.Health.Status}}' <project>-server-1

for i in 1 2 3; do
  echo "$i: $(podman inspect --format '{{.State.Health.Status}}' <project>-server-$i)"
done
```

## Troubleshooting

```bash
# SELinux denials
sudo ausearch -m avc -ts recent

# Lingering
loginctl show-user $(whoami) | grep Linger

# Unprivileged port boundary
sysctl net.ipv4.ip_unprivileged_port_start

# Registry auth
podman login --get-login ghcr.io
```
