# Setup Guide

## Requirements

### Your machine (where you run qqd)

- **Go 1.24+** - to build qqd from source
- **SSH client** - qqd uses `ssh` and `scp` to communicate with remote targets
- **`~/.ssh/known_hosts`** - qqd's SSH executor performs strict host key verification by default (set `insecure_host_key: true` per target to opt out). See [SSH known_hosts](#ssh-known_hosts-strict-host-key-checking) below for the most common pitfalls.

### Target host (where services run)

- **Podman 4.0+** (rootless) - container runtime with Quadlet support
- **systemd** - user-level systemd services (`systemctl --user`)
- **git** - for syncing the project repo (not needed with `sync = "upload"`)
- **bash** - command execution shell

## Installing Podman on Targets

```bash
# AlmaLinux / RHEL / CentOS 9
sudo dnf install -y podman git

# Ubuntu / Debian
sudo apt install -y podman git

# macOS (Podman machine)
brew install podman
podman machine init && podman machine start
```

## Enable Lingering

systemd user services stop when the user logs out, unless lingering is enabled:

```bash
sudo loginctl enable-linger <username>
```

Symptom if you skip this: `qqd init` reports the service activated, then containers vanish a few seconds after the SSH session closes. `systemctl --user status …` from a fresh login shows nothing because the user manager itself exited with the previous session.

## Privileged Ports

To bind services on ports below 1024 (like 80, 443) in rootless mode:

```bash
sudo sysctl -w net.ipv4.ip_unprivileged_port_start=80
echo "net.ipv4.ip_unprivileged_port_start=80" | sudo tee /etc/sysctl.d/99-unprivileged-ports.conf
```

## Private Registry Auth

If pulling from a private registry, login on the target before running qqd:

```bash
echo "$TOKEN" | podman login ghcr.io -u <user> --password-stdin
```

## SSH known_hosts (strict host key checking)

qqd's SSH executor uses `golang.org/x/crypto/ssh/knownhosts`, which is stricter than openssh in two ways most users hit:

**1. Stale bare-hostname entry → `knownhosts: key mismatch`.** If `~/.ssh/known_hosts` has a line like `localhost ssh-ed25519 …` (no `[host]:port` brackets, no port) from earlier tooling, and the host you now connect to presents a different key, the library reports `key mismatch` even when the port-qualified entry (`[localhost]:57593 …`) is correct. Find the conflicting line and remove it:

```bash
grep -nE '^localhost ' ~/.ssh/known_hosts          # finds the bare entry
ssh-keygen -R localhost                            # removes it (back up first if unsure)
```

**2. Algorithm negotiation → `knownhosts: key mismatch`.** Most SSH servers offer ed25519, ecdsa, and rsa host keys. openssh's `ssh-keyscan` and `accept-new` only cache the *first* algorithm; the Go SSH client may then negotiate a *different* one and report `key mismatch` because the cached entry is the wrong type. Cache all three host-key algorithms for the target up front:

```bash
# Replace <USER>@<HOST> and -i / -p as appropriate
ssh -i <KEY> -p <PORT> <USER>@<HOST> \
  'cat /etc/ssh/ssh_host_ed25519_key.pub /etc/ssh/ssh_host_ecdsa_key.pub /etc/ssh/ssh_host_rsa_key.pub' \
  | sed "s|^|[<HOST>]:<PORT> |" >> ~/.ssh/known_hosts
```

For an SSH host that listens on the default port 22, drop the `[…]:<PORT>` brackets and use plain `<HOST>` as the prefix. To bypass host key checking entirely for a single target (not recommended for production), set `insecure_host_key: true` on that target in your config.

**3. EC2 instance recreated → `WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!`.** If the IP/hostname is reassigned to a new VM the stored key is now wrong. After you've verified you trust the new host (e.g. via cloud console fingerprint), `ssh-keygen -R <host>` removes the stale line; then seed the new keys as above.

## Rootless Podman on RHEL 8 / older hosts

Rootless Podman 4.x on RHEL 8 (and other distros still defaulting to cgroups v1 hybrid mode) needs three host-side fixes before qqd-managed Quadlets will start. Symptoms cluster around the systemd unit reporting `Active: failed (Result: exit-code) … status=126`, with journal entries like:

- `mkdir /sys/fs/cgroup/pids/.../session.scope/runtime: permission denied` (cgroups v1)
- `runc create failed: … openat2 …/pids.max: no such file or directory` (runc + cgroups v2 rootless)
- `crun: the requested cgroup controller 'pids' is not available` (controllers not delegated to user manager)

Apply these in order on the target:

**1. Switch to unified cgroups v2 (reboot required).**

```bash
sudo grubby --update-kernel=ALL --args="systemd.unified_cgroup_hierarchy=1"
sudo reboot
# After reboot:
stat -fc %T /sys/fs/cgroup/      # should print cgroup2fs
```

**2. Install `crun` and make it the default runtime.** RHEL 8's bundled `runc` is too old for cgroups v2 rootless.

```bash
sudo dnf install -y crun
mkdir -p ~/.config/containers
cat > ~/.config/containers/containers.conf <<'EOF'
[engine]
runtime = "crun"
EOF
```

**3. Delegate cgroup controllers to the per-user systemd manager.** By default on RHEL 8, `user@<uid>.service` gets no controllers in v2 mode, so rootless containers can't create the pids/memory cgroups they need.

```bash
sudo mkdir -p /etc/systemd/system/user@.service.d
sudo tee /etc/systemd/system/user@.service.d/delegate.conf >/dev/null <<'EOF'
[Service]
Delegate=memory pids cpu io
EOF
sudo systemctl daemon-reload
sudo systemctl restart user@$(id -u <username>).service
# Verify:
cat /sys/fs/cgroup/user.slice/user-$(id -u <username>).slice/user@$(id -u <username>).service/cgroup.controllers
# Should print: cpu io memory pids
```

After all three: `qqd init -c …` succeeds, containers start under `user.slice/user-<uid>.slice/user@<uid>.service/…`, and they survive SSH disconnect (assuming [lingering](#enable-lingering) is also enabled).

**Distros that already work out of the box:** RHEL 9 / AlmaLinux 9 / Rocky 9, Fedora ≥ 31, Ubuntu ≥ 22.04, Debian ≥ 11, Amazon Linux 2023. There you typically only need `enable-linger`.

## macOS (Podman Machine)

On macOS, Podman runs containers inside a Linux VM. The `podman` CLI transparently forwards commands, but qqd also writes files on the target filesystem (Quadlet files, Traefik config, volume directories). With `host = "local"`, those writes go to your Mac, not the VM.

To deploy into the Podman Machine, SSH to the VM directly:

```hocon
targets {
  podman-vm {
    host = "localhost"
    user = "core"
    ssh_key = "~/.local/share/containers/podman/machine/machine"
    ssh_port = <port>
    repo_dir = "/var/tmp/my-app/repo"
    dirs = ["/var/tmp/my-app/data"]
  }
}
```

Find the SSH port:

```bash
podman machine inspect --format '{{.SSHConfig.Port}}'
```

## Volume Mounts

For service volumes, write the simple mount you mean:

```hocon
volumes = ["/host/data:/container/data"]
```

qqd adds Podman bind flags for host-path mounts when it renders the container:

- `:z` for shared SELinux relabeling on RHEL, AlmaLinux, and Fedora
- `:U` only when the service declares a non-root `user` or the image declares a non-root `USER`

If you provide explicit mount options, qqd keeps them and adds only the missing qqd-managed flags. Named volumes are left unchanged. If an image switches to a non-root user only inside its entrypoint, qqd cannot detect that ahead of time; add `:U` explicitly for that service.

## Podman DNS

Podman uses aardvark-dns for container name resolution within networks. The DNS server runs at the network gateway IP (typically `10.89.0.1` for the first custom network).

qqd's Traefik proxy connects to the project's Podman network and uses container DNS names directly.

Discover the gateway:

```bash
podman network inspect <network-name> | grep gateway
```

## Image Naming

Podman enforces fully-qualified image names in non-interactive (SSH) mode. Always use full registry paths:

```hocon
# Correct
image = "docker.io/library/postgres:18.1"
image = "ghcr.io/org/repo/service:1.0"

# Wrong - will fail with "short-name resolution enforced"
image = "postgres:18.1"
```
