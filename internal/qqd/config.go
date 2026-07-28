package qqd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ConfigParser parses a configuration source into a generic map.
// Implement this interface to add support for a new config format (e.g. TOML, HCL).
type ConfigParser interface {
	Parse(data []byte) (map[string]any, error)
}

// hoconParser parses HOCON-style config files.
type hoconParser struct{}

func (hoconParser) Parse(data []byte) (map[string]any, error) {
	return parseHOCON(string(data))
}

// jsonParser parses JSON config files.
type jsonParser struct{}

func (jsonParser) Parse(data []byte) (map[string]any, error) {
	return parseJSON(data)
}

// yamlParser parses YAML config files using a lightweight built-in parser.
type yamlParser struct{}

func (yamlParser) Parse(data []byte) (map[string]any, error) {
	return parseYAML(data)
}

func configParserForFile(path string, data []byte) ConfigParser {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return jsonParser{}
	case ".yaml", ".yml":
		return yamlParser{}
	case ".conf", ".hocon":
		return hoconParser{}
	default:
		return detectParser(data)
	}
}

func detectParser(data []byte) ConfigParser {
	trimmed := strings.TrimSpace(string(data))
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return jsonParser{}
	}
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.Contains(line, "=") || strings.HasSuffix(line, "{") {
			return hoconParser{}
		}
		if strings.HasSuffix(line, ":") || strings.Contains(line, ": ") {
			return yamlParser{}
		}
		break
	}
	return yamlParser{}
}

func loadProjectConfig(configPaths []string, invocationWD string) (ProjectConfig, error) {
	if len(configPaths) == 0 {
		return ProjectConfig{}, errors.New("at least one config path is required")
	}
	var merged map[string]any
	var configBaseDir string
	for i, cp := range configPaths {
		p, err := resolveLocalPath(invocationWD, cp)
		if err != nil {
			return ProjectConfig{}, err
		}
		if i == 0 {
			configBaseDir = filepath.Dir(p)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return ProjectConfig{}, fmt.Errorf("read config %s: %w", p, err)
		}
		parser := configParserForFile(p, raw)
		m, err := parser.Parse(raw)
		if err != nil {
			return ProjectConfig{}, fmt.Errorf("parse config %s: %w", p, err)
		}
		if merged == nil {
			merged = m
		} else {
			merged = deepMergeMaps(merged, m)
		}
	}
	// Relative paths inside the config (env_file, ssh_key, file:: refs,
	// build context, rsync upload base, .gitignore) resolve against the
	// directory of the first -c file. The shell cwd does not matter.
	cfg, err := decodeProjectConfig(merged, configBaseDir)
	if err != nil {
		return ProjectConfig{}, err
	}
	cfg.InvocationWD = configBaseDir
	return cfg, nil
}

// decodeProjectConfig converts parsed HOCON maps into strongly typed project config.
func decodeProjectConfig(raw map[string]any, invocationWD string) (ProjectConfig, error) {
	var cfg ProjectConfig
	cfg.Name, _ = asString(raw["name"])
	cfg.Repo, _ = asString(raw["repo"])
	cfg.Branch, _ = asString(raw["branch"])
	cfg.Path, _ = asString(raw["path"])
	cfg.GHToken, _ = asString(raw["gh_token"])
	cfg.Sync, _ = asString(raw["sync"])
	cfg.Runtime, _ = asString(raw["runtime"])
	switch strings.ToLower(strings.TrimSpace(cfg.Runtime)) {
	case "", "podman":
		cfg.Runtime = strings.ToLower(strings.TrimSpace(cfg.Runtime))
	case "docker":
		return ProjectConfig{}, errors.New("config: runtime \"docker\" is no longer supported; qqd deploys with Podman only. Remove the `runtime` field or set it to \"podman\"")
	default:
		return ProjectConfig{}, fmt.Errorf("config: runtime must be \"podman\" or omitted (got %q)", cfg.Runtime)
	}
	cfg.Proxy, _ = asString(raw["proxy"])
	cfg.ProxyImage, _ = asString(raw["proxy_image"])
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	if cfg.Name == "" {
		return ProjectConfig{}, errors.New("config: name is required")
	}
	cfg.EnvFiles = asStringSlice(raw["env_file"])
	cfg.Build = decodeBuild(raw["build"])
	cfg.Hooks = decodeHooks(raw["hooks"])

	servicesRaw, ok := asMap(raw["services"])
	if !ok || len(servicesRaw) == 0 {
		return ProjectConfig{}, errors.New("config: services is required")
	}
	cfg.Services = map[string]ServiceConfig{}
	for name, v := range servicesRaw {
		m, ok := asMap(v)
		if !ok {
			return ProjectConfig{}, fmt.Errorf("service %s must be object", name)
		}
		svc := ServiceConfig{
			Image:      getString(m, "image"),
			Dockerfile: getString(m, "dockerfile"),
			Context:    getString(m, "context"),
			User:       getString(m, "user"),
			EnvFile:    getString(m, "env_file"),
			DependsOn:  asStringSlice(m["depends_on"]),
			Volumes:    asStringSlice(m["volumes"]),
			Env:        asStringMap(m["env"]),
			Resources:  decodeResources(m["resources"]),
		}
		if replicas, ok := asInt(m["replicas"]); ok {
			svc.Replicas = replicas
		}
		if delay, ok := asInt(m["startup_delay"]); ok {
			svc.StartupDelay = delay
		}
		cmdRaw := m["command"]
		if cmdRaw != nil {
			if c, ok := asString(cmdRaw); ok {
				if strings.Contains(c, " ") {
					svc.Command = strings.Fields(c)
				} else {
					svc.Command = []string{c}
				}
			} else {
				svc.Command = asStringSlice(cmdRaw)
			}
		}
		svc.Health = decodeHealth(m["health"])
		svc.Hooks = decodeHooks(m["hooks"])
		if svc.Image == "" {
			return ProjectConfig{}, fmt.Errorf("service %s: image is required", name)
		}
		cfg.Services[name] = svc
	}

	// `repo` is only required when something will actually be synced from
	// source: a service has a build context, or the user explicitly opted
	// into a sync mode that isn't a pure upload. Pure image-pull deploys
	// don't need a repo at all.
	if cfg.Repo == "" && cfg.Sync != "upload" && cfg.needsSource() {
		return ProjectConfig{}, errors.New("config: repo is required when a service builds from source (or set sync = \"upload\")")
	}

	targetsRaw, ok := asMap(raw["targets"])
	if !ok || len(targetsRaw) == 0 {
		return ProjectConfig{}, errors.New("config: targets is required")
	}
	cfg.Targets = map[string]TargetConfig{}
	for name, v := range targetsRaw {
		m, ok := asMap(v)
		if !ok {
			return ProjectConfig{}, fmt.Errorf("target %s must be object", name)
		}
		sshPort, _ := asInt(m["ssh_port"])
		insecureHostKey, _ := asBool(m["insecure_host_key"])
		lifecycle := strings.ToLower(strings.TrimSpace(getString(m, "lifecycle")))
		switch lifecycle {
		case "", "auto", "systemd", "direct":
			// ok
		default:
			return ProjectConfig{}, fmt.Errorf("target %s: lifecycle must be \"auto\", \"systemd\", or \"direct\" (got %q)", name, lifecycle)
		}
		t := TargetConfig{
			Name:            name,
			Host:            getString(m, "host"),
			User:            getString(m, "user"),
			SSHKey:          getString(m, "ssh_key"),
			SSHPort:         sshPort,
			InsecureHostKey: insecureHostKey,
			RepoDir:         getString(m, "repo_dir"),
			Dirs:            asStringSlice(m["dirs"]),
			Services:        asStringSlice(m["services"]),
			Env:             asStringMap(m["env"]),
			Build:           decodeBuild(m["build"]),
			Lifecycle:       lifecycle,
		}
		expose, err := decodeExpose(m["expose"], cfg.Services)
		if err != nil {
			return ProjectConfig{}, fmt.Errorf("target %s: %w", name, err)
		}
		t.Expose = expose
		if t.Host == "" {
			return ProjectConfig{}, fmt.Errorf("target %s: host is required", name)
		}
		if t.RepoDir == "" && (cfg.Sync != "" || cfg.needsSource()) {
			return ProjectConfig{}, fmt.Errorf("target %s: repo_dir is required when sync or a service build is configured", name)
		}
		if t.Host != "local" && t.User == "" {
			return ProjectConfig{}, fmt.Errorf("target %s: user is required for remote targets", name)
		}
		if t.SSHKey != "" {
			abs, err := resolveLocalPath(invocationWD, t.SSHKey)
			if err != nil {
				return ProjectConfig{}, fmt.Errorf("target %s ssh_key: %w", name, err)
			}
			t.SSHKey = abs
		}
		t.Overrides = map[string]ServiceOverride{}
		if ovRaw, ok := asMap(m["overrides"]); ok {
			for svcName, ovAny := range ovRaw {
				ovMap, ok := asMap(ovAny)
				if !ok {
					return ProjectConfig{}, fmt.Errorf("target %s override %s must be object", name, svcName)
				}
				t.Overrides[svcName] = ServiceOverride{
					Env: asStringMap(ovMap["env"]),
				}
			}
		}
		cfg.Targets[name] = t
	}

	if cfg.Build.SSHKey != "" {
		abs, err := resolveLocalPath(invocationWD, cfg.Build.SSHKey)
		if err != nil {
			return ProjectConfig{}, fmt.Errorf("build.ssh_key: %w", err)
		}
		cfg.Build.SSHKey = abs
	}
	for name, t := range cfg.Targets {
		if t.Build.SSHKey != "" {
			abs, err := resolveLocalPath(invocationWD, t.Build.SSHKey)
			if err != nil {
				return ProjectConfig{}, fmt.Errorf("target %s build.ssh_key: %w", name, err)
			}
			t.Build.SSHKey = abs
			cfg.Targets[name] = t
		}
	}

	return cfg, nil
}

// decodeExpose parses the centralized expose block.
// Top-level keys are host port numbers:
//   - String value → TCP entry (e.g. 5432 = "db:5432")
//   - Map value → HTTP entry with path routes and optional TLS
func decodeExpose(raw any, services map[string]ServiceConfig) (ExposeConfig, error) {
	m, ok := asMap(raw)
	if !ok {
		return ExposeConfig{}, nil
	}
	var dashboard int
	if v, ok := asInt(m["dashboard"]); ok {
		dashboard = v
	}

	var entries []ExposeEntry
	for _, key := range sortedKeys(m) {
		if key == "dashboard" {
			continue
		}
		hostPort, err := strconv.Atoi(key)
		if err != nil {
			return ExposeConfig{}, fmt.Errorf("expose: key %q must be a port number", key)
		}
		v := m[key]
		// String value → TCP entry
		if target, ok := asString(v); ok {
			svc, _, err := parseTarget(target)
			if err != nil {
				return ExposeConfig{}, fmt.Errorf("expose port %d: %w", hostPort, err)
			}
			if _, exists := services[svc]; !exists {
				return ExposeConfig{}, fmt.Errorf("expose port %d: unknown service %q", hostPort, svc)
			}
			entries = append(entries, ExposeEntry{
				HostPort: hostPort,
				Target:   target,
			})
			continue
		}
		// Map value → HTTP entry
		sub, ok := asMap(v)
		if !ok {
			return ExposeConfig{}, fmt.Errorf("expose port %d: expected string or object", hostPort)
		}
		routes := map[string]string{}
		var tls *TLSConfig
		for _, path := range sortedKeys(sub) {
			if path == "tls" {
				t := decodeTLS(sub[path])
				tls = &t
				continue
			}
			target, ok := asString(sub[path])
			if !ok {
				return ExposeConfig{}, fmt.Errorf("expose port %d path %q: expected \"service:port\" string", hostPort, path)
			}
			svc, _, err := parseTarget(target)
			if err != nil {
				return ExposeConfig{}, fmt.Errorf("expose port %d path %q: %w", hostPort, path, err)
			}
			if _, exists := services[svc]; !exists {
				return ExposeConfig{}, fmt.Errorf("expose port %d path %q: unknown service %q", hostPort, path, svc)
			}
			routes[path] = target
		}
		if len(routes) == 0 {
			return ExposeConfig{}, fmt.Errorf("expose port %d: HTTP entry must have at least one route", hostPort)
		}
		if tls != nil && tls.CertsDir != "" && tls.ServerName == "" {
			return ExposeConfig{}, fmt.Errorf("expose port %d: tls requires both certs_dir and server_name", hostPort)
		}
		if tls != nil && tls.ServerName != "" && tls.CertsDir == "" {
			return ExposeConfig{}, fmt.Errorf("expose port %d: tls requires both certs_dir and server_name", hostPort)
		}
		entries = append(entries, ExposeEntry{
			HostPort: hostPort,
			Routes:   routes,
			TLS:      tls,
		})
	}
	return ExposeConfig{Entries: entries, Dashboard: dashboard}, nil
}

// parseTarget splits a "service:port" string.
func parseTarget(target string) (service string, port int, err error) {
	parts := strings.SplitN(target, ":", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("invalid target %q: expected \"service:port\"", target)
	}
	port, err = strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, fmt.Errorf("invalid target %q: port must be a number", target)
	}
	return parts[0], port, nil
}

// decodeTLS converts a map into TLSConfig.
func decodeTLS(raw any) TLSConfig {
	m, ok := asMap(raw)
	if !ok {
		return TLSConfig{}
	}
	var tls TLSConfig
	if port, ok := asInt(m["port"]); ok {
		tls.Port = port
	}
	tls.CertsDir = getString(m, "certs_dir")
	names := asStringSlice(m["server_name"])
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		tls.ServerNames = append(tls.ServerNames, n)
	}
	if len(tls.ServerNames) > 0 {
		tls.ServerName = tls.ServerNames[0]
	}
	return tls
}

// decodeResources converts a map into ResourceConfig.
func decodeResources(raw any) ResourceConfig {
	m, ok := asMap(raw)
	if !ok {
		return ResourceConfig{}
	}
	return ResourceConfig{
		CPUs:   getString(m, "cpus"),
		Memory: getString(m, "memory"),
	}
}

// decodeHealth parses health from two possible formats:
//  1. Object: health { path = "/api/health", port = 8080 }
//  2. String: health = "/api/health" — port must be specified separately or in expose
func decodeHealth(raw any) HealthConfig {
	if raw == nil {
		return HealthConfig{}
	}
	if s, ok := asString(raw); ok {
		return HealthConfig{Path: s}
	}
	if m, ok := asMap(raw); ok {
		port, _ := asInt(m["port"])
		return HealthConfig{
			Path: getString(m, "path"),
			Port: port,
		}
	}
	return HealthConfig{}
}

// decodeHooks converts a map into HooksConfig.
func decodeHooks(raw any) HooksConfig {
	m, ok := asMap(raw)
	if !ok {
		return HooksConfig{}
	}
	return HooksConfig{
		PreDeploy:  getString(m, "pre_deploy"),
		PostDeploy: getString(m, "post_deploy"),
		PreBuild:   getString(m, "pre_build"),
		PostBuild:  getString(m, "post_build"),
	}
}

// decodeBuild converts a map into BuildConfig.
func decodeBuild(raw any) BuildConfig {
	m, ok := asMap(raw)
	if !ok {
		return BuildConfig{}
	}
	out := BuildConfig{
		Strategy:      getString(m, "strategy"),
		Memory:        getString(m, "memory"),
		Host:          getString(m, "host"),
		User:          getString(m, "user"),
		SSHKey:        getString(m, "ssh_key"),
		RepoDir:       getString(m, "repo_dir"),
		Delivery:      getString(m, "delivery"),
		Repo:          getString(m, "repo"),
		Workflow:      getString(m, "workflow"),
		Branch:        getString(m, "branch"),
		GitHubToken:   getString(m, "github_token"),
		Registry:      getString(m, "registry"),
		RegistryUser:  getString(m, "registry_user"),
		RegistryToken: getString(m, "registry_token"),
	}
	if cpu, ok := asInt(m["cpu"]); ok {
		out.CPU = cpu
	}
	if port, ok := asInt(m["ssh_port"]); ok {
		out.SSHPort = port
	}
	return out
}

// getString reads and stringifies a key from a dynamic map.
func getString(m map[string]any, key string) string {
	v, _ := asString(m[key])
	return v
}

// needsSource reports whether any configured service requires source files
// on the target (i.e. has a Dockerfile or build Context). Pure image-pull
// configs return false and can skip `repo`/`repo_dir`/source sync entirely.
func (cfg ProjectConfig) needsSource() bool {
	for _, svc := range cfg.Services {
		if svc.Dockerfile != "" || svc.Context != "" {
			return true
		}
	}
	return false
}

// projectRoot returns the effective repository subdirectory for a target.
func projectRoot(target TargetConfig, cfg ProjectConfig) string {
	if cfg.Path == "" {
		return target.RepoDir
	}
	return filepath.ToSlash(filepath.Join(target.RepoDir, cfg.Path))
}

// resolveTarget computes the final target view with service filtering and substitutions.
func resolveTarget(cfg ProjectConfig, targetName string, cliServices []string) (EffectiveTarget, error) {
	t, ok := cfg.Targets[targetName]
	if !ok {
		return EffectiveTarget{}, fmt.Errorf("target %s not found", targetName)
	}
	build := mergeBuild(cfg.Build, t.Build)
	if build.Strategy == "" {
		build.Strategy = "local"
	}
	selected := map[string]bool{}
	if len(t.Services) > 0 {
		for _, s := range t.Services {
			selected[s] = true
		}
	} else {
		for name := range cfg.Services {
			selected[name] = true
		}
	}
	if len(cliServices) > 0 {
		filter := map[string]bool{}
		for _, s := range cliServices {
			if _, ok := cfg.Services[s]; !ok {
				return EffectiveTarget{}, fmt.Errorf("service %s not found", s)
			}
			filter[s] = true
		}
		for name := range selected {
			if !filter[name] {
				delete(selected, name)
			}
		}
	}

	// Load project-level env files. Later files override earlier ones.
	// Explicit target env values always take priority over file values.
	if len(cfg.EnvFiles) > 0 {
		mergedFileEnv := map[string]string{}
		for _, ef := range cfg.EnvFiles {
			efPath, err := resolveLocalPath(cfg.InvocationWD, ef)
			if err != nil {
				return EffectiveTarget{}, fmt.Errorf("resolve env_file path %s: %w", ef, err)
			}
			fileEnv, err := loadEnvFile(efPath)
			if err != nil {
				return EffectiveTarget{}, fmt.Errorf("load env_file %s: %w", ef, err)
			}
			for k, v := range fileEnv {
				mergedFileEnv[k] = v
			}
		}
		if t.Env == nil {
			t.Env = map[string]string{}
		}
		for k, v := range mergedFileEnv {
			if _, exists := t.Env[k]; !exists {
				t.Env[k] = v
			}
		}
	}

	for k, v := range t.Env {
		resolved, err := resolveFileRef(cfg.InvocationWD, v)
		if err != nil {
			return EffectiveTarget{}, fmt.Errorf("target %s env %s: %w", targetName, k, err)
		}
		t.Env[k] = resolved
	}

	resolved := map[string]ServiceConfig{}
	for name := range selected {
		base, ok := cfg.Services[name]
		if !ok {
			return EffectiveTarget{}, fmt.Errorf("target %s references unknown service %s", targetName, name)
		}
		svc := base.Clone()

		// Load service-level env_file (lower priority than explicit service env).
		if svc.EnvFile != "" {
			efPath, err := resolveLocalPath(cfg.InvocationWD, svc.EnvFile)
			if err != nil {
				return EffectiveTarget{}, fmt.Errorf("service %s env_file path: %w", name, err)
			}
			fileEnv, err := loadEnvFile(efPath)
			if err != nil {
				return EffectiveTarget{}, fmt.Errorf("service %s env_file: %w", name, err)
			}
			for k, v := range fileEnv {
				if _, exists := svc.Env[k]; !exists {
					svc.Env[k] = v
				}
			}
		}

		for k, v := range svc.Env {
			svc.Env[k] = expandVars(v, t.Env)
		}
		for i, vol := range svc.Volumes {
			svc.Volumes[i] = expandVars(vol, t.Env)
		}
		svc.Image = expandVars(svc.Image, t.Env)
		svc.Dockerfile = expandVars(svc.Dockerfile, t.Env)
		if ov, ok := t.Overrides[name]; ok {
			for k, v := range ov.Env {
				svc.Env[k] = expandVars(v, t.Env)
			}
		}
		resolved[name] = svc
	}

	for i, d := range t.Dirs {
		t.Dirs[i] = expandVars(d, t.Env)
	}
	t.RepoDir = expandVars(t.RepoDir, t.Env)

	build.RepoDir = expandVars(build.RepoDir, t.Env)
	build.SSHKey = expandVars(build.SSHKey, t.Env)
	build.Repo = expandVars(build.Repo, t.Env)
	build.Workflow = expandVars(build.Workflow, t.Env)
	build.Branch = expandVars(build.Branch, t.Env)
	build.GitHubToken = expandVars(build.GitHubToken, t.Env)
	build.Registry = expandVars(build.Registry, t.Env)
	build.RegistryUser = expandVars(build.RegistryUser, t.Env)
	build.RegistryToken = expandVars(build.RegistryToken, t.Env)
	build.Host = expandVars(build.Host, t.Env)
	build.User = expandVars(build.User, t.Env)

	// Expand vars in expose TLS certs_dir
	expose := t.Expose
	var expandedEntries []ExposeEntry
	for _, e := range expose.Entries {
		entry := e
		if entry.TLS != nil && entry.TLS.CertsDir != "" {
			tls := *entry.TLS
			tls.CertsDir = expandVars(tls.CertsDir, t.Env)
			entry.TLS = &tls
		}
		expandedEntries = append(expandedEntries, entry)
	}
	expose.Entries = expandedEntries

	// Infer health check port from expose entries for services that use the
	// shorthand `health = "/path"` (which sets Path but leaves Port == 0).
	for name, svc := range resolved {
		if svc.Health.Path != "" && svc.Health.Port == 0 {
			port, err := inferHealthPort(name, expose)
			if err != nil {
				return EffectiveTarget{}, fmt.Errorf("target %s service %s health: %w", targetName, name, err)
			}
			svc.Health.Port = port
			resolved[name] = svc
		}
	}

	return EffectiveTarget{
		Target:   t,
		Build:    build,
		Services: resolved,
		Expose:   expose,
	}, nil
}

// inferHealthPort extracts the HTTP container port for a service from expose routes.
// It rejects ambiguous or missing HTTP port mappings for shorthand health config.
func inferHealthPort(serviceName string, expose ExposeConfig) (int, error) {
	ports := map[int]bool{}
	for _, e := range expose.Entries {
		for _, target := range e.Routes {
			svc, port, err := parseTarget(target)
			if err == nil && svc == serviceName && port > 0 {
				ports[port] = true
			}
		}
	}
	if len(ports) == 0 {
		return 0, fmt.Errorf("cannot infer port from HTTP expose routes; set health.port explicitly")
	}
	if len(ports) > 1 {
		var values []int
		for p := range ports {
			values = append(values, p)
		}
		sort.Ints(values)
		return 0, fmt.Errorf("ambiguous HTTP expose ports %v; set health.port explicitly", values)
	}
	for p := range ports {
		return p, nil
	}
	return 0, fmt.Errorf("cannot infer port")
}
