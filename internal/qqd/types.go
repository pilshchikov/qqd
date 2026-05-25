package qqd

import "maps"

// ProjectConfig is the fully decoded project-level configuration file.
//
// The `qqd:` struct tag is consumed by the reflection-based schema builder in
// manifest.go. Format: `qqd:"key=<name>;required=<yes|no|conditional>;default=<v>;type=<override>;desc=<text>"`.
// Fields without a `qqd:` tag are treated as internal and omitted from
// generated documentation and from `qqd manifest`.
type ProjectConfig struct {
	Name       string   `qqd:"key=name;required=yes;desc=Project name. Used as a prefix for every container and network qqd creates."`
	Repo       string   `qqd:"key=repo;required=conditional;desc=Git repository URL. Required when a service has a dockerfile or context AND sync != upload. Pure image-pull deploys can omit it."`
	Branch     string   `qqd:"key=branch;required=no;default=main;desc=Git branch to deploy from."`
	Path       string   `qqd:"key=path;required=no;desc=Subdirectory of the repo where this project lives. Defaults to the repo root."`
	GHToken    string   `qqd:"key=gh_token;required=no;desc=GitHub token used to inject https credentials into the repo URL. Honors file:: refs and env var names."`
	Sync       string   `qqd:"key=sync;required=no;default=git;desc=Source sync mode. \"git\" (clone/fetch on target) or \"upload\" (rsync from your machine, respects .gitignore)."`
	Runtime    string   `qqd:"key=runtime;required=no;default=podman;desc=Container runtime. Only \"podman\" is supported. \"docker\" is rejected."`
	Proxy      string   `qqd:"key=proxy;required=no;default=traefik;desc=Reverse proxy provider: \"traefik\" or \"caddy\"."`
	ProxyImage string   `qqd:"key=proxy_image;required=no;desc=Override the proxy container image (otherwise the provider default is used)."`
	EnvFiles   []string `qqd:"key=env_file;required=no;type=string or array;desc=One or more .env files to load. Later files win."`
	Build      BuildConfig                `qqd:"key=build;required=no;type=object;desc=Build strategy + resources (see Build section)."`
	Hooks      HooksConfig                `qqd:"key=hooks;required=no;type=object;desc=Project-level hooks (pre_deploy, post_deploy, pre_build, post_build)."`
	Services   map[string]ServiceConfig   `qqd:"key=services;required=yes;type=object;desc=Map of service name → ServiceConfig (see Service section)."`
	Targets    map[string]TargetConfig    `qqd:"key=targets;required=yes;type=object;desc=Map of target name → TargetConfig (see Target section)."`
	// InvocationWD is the base directory used to resolve relative paths
	// inside the config (env_file, ssh_key, file:: refs, build context,
	// rsync upload base, .gitignore lookup). It is set to the directory
	// of the first -c config file, NOT the shell cwd, so deploys behave
	// the same regardless of where the user runs qqd from. Absolute paths
	// in the config are not affected.
	InvocationWD string
}

// BuildConfig controls how images are built and delivered.
type BuildConfig struct {
	Strategy      string `qqd:"key=strategy;required=no;default=local;desc=\"local\" builds on the target, \"build-host\" builds on a dedicated SSH-reachable host, \"github-actions\" dispatches a workflow."`
	CPU           int    `qqd:"key=cpu;required=no;desc=Build container CPU limit."`
	Memory        string `qqd:"key=memory;required=no;desc=Build container memory limit (e.g. \"4g\")."`
	Host          string `qqd:"key=host;required=conditional;desc=Build server host (required for strategy=\"build-host\")."`
	User          string `qqd:"key=user;required=conditional;desc=Build server SSH user (required for strategy=\"build-host\")."`
	SSHKey        string `qqd:"key=ssh_key;required=no;desc=Build server SSH key."`
	SSHPort       int    `qqd:"key=ssh_port;required=no;default=22;desc=Build server SSH port."`
	RepoDir       string `qqd:"key=repo_dir;required=conditional;desc=Build server working dir (required for strategy=\"build-host\")."`
	Delivery      string `qqd:"key=delivery;required=no;desc=How built images reach the target (default: registry push for build-host, save+ssh for local fallback)."`
	Repo          string `qqd:"key=repo;required=no;desc=Override project repo URL for the build server / GitHub Actions strategy."`
	Workflow      string `qqd:"key=workflow;required=conditional;desc=GitHub Actions workflow filename (required for strategy=\"github-actions\")."`
	Branch        string `qqd:"key=branch;required=no;desc=Override branch for the build server / GitHub Actions strategy."`
	GitHubToken   string `qqd:"key=github_token;required=no;desc=GitHub token for the GitHub Actions strategy. Honors file:: and env-var refs."`
	Registry      string `qqd:"key=registry;required=no;desc=Image registry hostname used by the build-host delivery."`
	RegistryUser  string `qqd:"key=registry_user;required=no;desc=Registry user for push/pull authentication."`
	RegistryToken string `qqd:"key=registry_token;required=no;desc=Registry token/password. Honors file:: and env-var refs."`
}

// ResourceConfig specifies runtime resource limits for a container.
type ResourceConfig struct {
	CPUs   string `qqd:"key=cpus;required=no;desc=podman --cpus value (e.g. \"2\")."`
	Memory string `qqd:"key=memory;required=no;desc=podman --memory value (e.g. \"1g\")."`
}

// TLSConfig describes HTTPS termination at the proxy.
type TLSConfig struct {
	Port       int    `qqd:"key=port;required=no;default=443;desc=HTTPS listen port."`
	CertsDir   string `qqd:"key=certs_dir;required=yes;desc=Host directory containing the certificate files."`
	ServerName string `qqd:"key=server_name;required=yes;desc=Domain name for SNI and certificate lookup."`
}

// ExposeEntry describes one host-port listener in the centralized expose block.
type ExposeEntry struct {
	HostPort int
	Routes   map[string]string // path → "service:port" (HTTP); nil for TCP
	Target   string            // "service:port" (TCP); empty for HTTP
	TLS      *TLSConfig
}

// ExposeConfig describes the per-target reverse proxy configuration.
type ExposeConfig struct {
	Entries   []ExposeEntry
	Dashboard int // port for Traefik dashboard (0 = disabled)
}

// HealthConfig describes a health check endpoint with path and port.
type HealthConfig struct {
	Path string `qqd:"key=path;required=no;desc=HTTP path the proxy probes for readiness (e.g. \"/health\")."`
	Port int    `qqd:"key=port;required=no;desc=Port to probe. Omitted = inferred from a single HTTP expose route."`
}

// HooksConfig defines shell commands to run at deployment lifecycle points.
type HooksConfig struct {
	PreDeploy  string `qqd:"key=pre_deploy;required=no;desc=Runs before deploy starts. Project-level only."`
	PostDeploy string `qqd:"key=post_deploy;required=no;desc=Runs after deploy completes. Project-level only."`
	PreBuild   string `qqd:"key=pre_build;required=no;desc=Runs before image build / pull. Project- or service-level."`
	PostBuild  string `qqd:"key=post_build;required=no;desc=Runs after image build / pull. Project- or service-level."`
}

// ServiceConfig describes one service definition in the project config.
type ServiceConfig struct {
	Image        string            `qqd:"key=image;required=yes;desc=Fully-qualified image (e.g. docker.io/library/postgres:16.1). Podman in non-interactive mode rejects bare short names."`
	Dockerfile   string            `qqd:"key=dockerfile;required=no;desc=If set, build this image on the target instead of pulling. Relative paths resolve against the first -c config file's directory."`
	Context      string            `qqd:"key=context;required=no;desc=Build context directory. With sync=upload, only listed contexts are uploaded."`
	User         string            `qqd:"key=user;required=no;desc=Container user (e.g. \"1000:1000\")."`
	EnvFile      string            `qqd:"key=env_file;required=no;desc=Path to .env file for this service only."`
	Command      []string          `qqd:"key=command;required=no;type=string or array;desc=Override the image ENTRYPOINT / CMD."`
	DependsOn    []string          `qqd:"key=depends_on;required=no;default=[];desc=Other service names that must start first."`
	Volumes      []string          `qqd:"key=volumes;required=no;default=[];desc=Bind mounts. SELinux hosts need :z and (if container user differs from host user) :U flags."`
	Env          map[string]string `qqd:"key=env;required=no;default={};type=map string→string;desc=Environment variables. ${VAR} expands from target.env then OS env."`
	Replicas     int               `qqd:"key=replicas;required=no;default=1;desc=Number of replica containers. Names: <project>-<service>-1..N."`
	StartupDelay int               `qqd:"key=startup_delay;required=no;default=5;desc=Seconds to wait after start when no health check is configured."`
	Health       HealthConfig      `qqd:"key=health;required=no;type=object or string;desc=Health check. Either { path: \"/health\", port: 8080 } or shorthand \"/health\" (port inferred from a single HTTP expose route)."`
	Resources    ResourceConfig    `qqd:"key=resources;required=no;type=object;desc=Resource limits: { cpus: \"2\", memory: \"1g\" }."`
	Hooks        HooksConfig       `qqd:"key=hooks;required=no;type=object;desc=Per-service hooks: pre_build, post_build."`
}

// ServiceOverride contains per-target overrides for one service.
type ServiceOverride struct {
	Env map[string]string `qqd:"key=env;required=no;type=map string→string;desc=Service env overrides applied only on this target."`
}

// TargetConfig describes one deployment destination host.
type TargetConfig struct {
	Name            string                     // populated from the map key, not a user-set field
	Host            string                     `qqd:"key=host;required=yes;desc=IP or hostname. Use \"local\" to skip SSH and run on the local machine."`
	User            string                     `qqd:"key=user;required=yes*;desc=SSH user. *Not required when host=\"local\"."`
	SSHKey          string                     `qqd:"key=ssh_key;required=no;desc=Private key path. ~ expands to your home dir."`
	SSHPort         int                        `qqd:"key=ssh_port;required=no;default=22;desc=SSH port."`
	InsecureHostKey bool                       `qqd:"key=insecure_host_key;required=no;default=false;desc=Skip ~/.ssh/known_hosts verification. Off by default; only enable when you trust the path some other way."`
	RepoDir         string                     `qqd:"key=repo_dir;required=conditional;desc=Absolute path on the target for git clone / rsync. Required when sync is set or any service builds from source. Optional for pure image-pull deploys."`
	Dirs            []string                   `qqd:"key=dirs;required=no;default=[];desc=Absolute directories to mkdir -p before deploy (e.g. for volume mounts)."`
	Services        []string                   `qqd:"key=services;required=no;desc=Deploy only these services on this target. Empty = all."`
	Env             map[string]string          `qqd:"key=env;required=no;default={};type=map string→string;desc=Variables available for ${VAR} expansion in service fields."`
	Overrides       map[string]ServiceOverride `qqd:"key=overrides;required=no;type=object;desc=Per-service env overrides: { <svc>: { env: { K: V } } }."`
	Build           BuildConfig                `qqd:"key=build;required=no;type=object;desc=Per-target build config (merged on top of project-level)."`
	Expose          ExposeConfig               `qqd:"key=expose;required=no;type=object;desc=Reverse-proxy routing (see Expose section)."`
	// Lifecycle selects how qqd manages container processes on this target.
	//   "auto"    (default) probe the target: use systemd if reachable, else direct.
	//   "systemd" force systemd unit management (today's default behavior).
	//   "direct"  drive containers directly via `podman run --restart=...`
	//             with qqd labels. No systemctl.
	//             Required for targets without systemd: macOS host:"local",
	//             minimal Linux containers, nested CI.
	Lifecycle string `qqd:"key=lifecycle;required=no;default=auto;desc=\"auto\" probes systemctl and falls back to \"direct\"; \"systemd\" forces unit management; \"direct\" uses podman run --restart=… with qqd.* labels."`
}

// EffectiveTarget is a resolved target view after applying filters and overrides.
type EffectiveTarget struct {
	Target   TargetConfig
	Build    BuildConfig
	Services map[string]ServiceConfig
	Expose   ExposeConfig
}

// Clone returns a deep-enough copy for safe per-target mutation.
func (s ServiceConfig) Clone() ServiceConfig {
	out := s
	out.Command = append([]string{}, s.Command...)
	out.DependsOn = append([]string{}, s.DependsOn...)
	out.Volumes = append([]string{}, s.Volumes...)
	out.Env = maps.Clone(s.Env)
	return out
}
