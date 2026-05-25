package qqd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ComposeImportOpts holds options for importing a docker-compose.yaml.
type ComposeImportOpts struct {
	ComposeFile string
	EnvFile     string
	Output      string
	Format      string // "yaml" (default), "json", or "hocon"
	Ignore      string // comma-separated service names to skip
	Host        string
	User        string
	SSHKey      string
	RepoDir     string
	ProjectName string
}

// ImportCompose reads a docker-compose.yaml, parses it, and generates a qqd config file.
func ImportCompose(opts ComposeImportOpts) error {
	data, err := os.ReadFile(opts.ComposeFile)
	if err != nil {
		return fmt.Errorf("read compose file: %w", err)
	}

	parser := configParserForFile(opts.ComposeFile, data)
	raw, err := parser.Parse(data)
	if err != nil {
		return fmt.Errorf("parse compose file: %w", err)
	}

	envVars := map[string]string{}
	if opts.EnvFile != "" {
		envVars, err = loadEnvFile(opts.EnvFile)
		if err != nil {
			return fmt.Errorf("load env file: %w", err)
		}
	}

	projectName := opts.ProjectName
	if projectName == "" {
		dir := filepath.Base(filepath.Dir(absPath(opts.ComposeFile)))
		projectName = sanitizeProjectName(dir)
	}

	servicesRaw, ok := asMap(raw["services"])
	if !ok {
		return fmt.Errorf("compose file has no 'services' section")
	}

	cfg := map[string]any{
		"name": projectName,
		"sync": "upload",
	}

	services := map[string]any{}
	var targetEnv map[string]any
	if len(envVars) > 0 {
		targetEnv = map[string]any{}
		for k, v := range envVars {
			targetEnv[k] = v
		}
	}

	ignoreSet := map[string]bool{}
	if opts.Ignore != "" {
		for _, s := range strings.Split(opts.Ignore, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				ignoreSet[s] = true
			}
		}
	}

	type portMapping struct {
		hostPort      int
		containerPort int
		service       string
	}
	var httpPorts []portMapping
	var tcpPorts []portMapping

	for svcName := range ignoreSet {
		if svcRaw, ok := asMap(servicesRaw[svcName]); ok {
			if portsRaw := svcRaw["ports"]; portsRaw != nil {
				if ports, ok := portsRaw.([]any); ok && len(ports) > 0 {
					fmt.Printf("note: ignored service %q had ports %v - you may need to add these to expose config manually\n", svcName, ports)
				}
			}
		}
	}

	for _, svcName := range sortedMapKeys(servicesRaw) {
		if ignoreSet[svcName] {
			continue
		}
		svcRaw, ok := asMap(servicesRaw[svcName])
		if !ok {
			continue
		}

		svc := map[string]any{}

		if img, ok := asString(svcRaw["image"]); ok {
			expanded := expandComposeVars(img, envVars)
			svc["image"] = normalizeImageName(expanded)
		}

		if buildRaw := svcRaw["build"]; buildRaw != nil {
			if buildMap, ok := asMap(buildRaw); ok {
				if df, ok := asString(buildMap["dockerfile"]); ok {
					svc["dockerfile"] = df
				}
				if ctx, ok := asString(buildMap["context"]); ok && ctx != "." {
					svc["context"] = ctx
				}
			} else if buildStr, ok := asString(buildRaw); ok {
				svc["context"] = buildStr
			}
		}

		// Image is required - if no image but has build, generate a local image name
		if _, hasImage := svc["image"]; !hasImage {
			svc["image"] = fmt.Sprintf("%s-%s:latest", projectName, svcName)
		}

		if cmdRaw := svcRaw["command"]; cmdRaw != nil {
			switch cmd := cmdRaw.(type) {
			case string:
				if strings.Contains(cmd, " ") {
					parts := strings.Fields(cmd)
					arr := make([]any, len(parts))
					for j, p := range parts {
						arr[j] = p
					}
					svc["command"] = arr
				} else {
					svc["command"] = cmd
				}
			case []any:
				svc["command"] = cmd
			}
		}

		if depsRaw := svcRaw["depends_on"]; depsRaw != nil {
			switch deps := depsRaw.(type) {
			case []any:
				svc["depends_on"] = deps
			case map[string]any:
				// depends_on with conditions: extract just names
				var names []any
				for name := range deps {
					names = append(names, name)
				}
				sort.Slice(names, func(i, j int) bool {
					return fmt.Sprint(names[i]) < fmt.Sprint(names[j])
				})
				svc["depends_on"] = names
			}
		}

		if volsRaw := svcRaw["volumes"]; volsRaw != nil {
			if vols, ok := volsRaw.([]any); ok {
				var cleanVols []any
				for _, v := range vols {
					if vs, ok := v.(string); ok {
						expanded := expandComposeVars(vs, envVars)
						cleanVols = append(cleanVols, expanded)
					}
				}
				if len(cleanVols) > 0 {
					svc["volumes"] = cleanVols
				}
			}
		}

		if envRaw := svcRaw["environment"]; envRaw != nil {
			env := map[string]any{}
			switch e := envRaw.(type) {
			case []any:
				for _, item := range e {
					if s, ok := item.(string); ok {
						k, v, _ := strings.Cut(s, "=")
						expanded := expandComposeVars(v, envVars)
						expanded = rewriteServiceRefs(expanded, projectName, servicesRaw)
						env[k] = expanded
					}
				}
			case map[string]any:
				for k, v := range e {
					if vs, ok := v.(string); ok {
						expanded := expandComposeVars(vs, envVars)
						expanded = rewriteServiceRefs(expanded, projectName, servicesRaw)
						env[k] = expanded
					} else {
						env[k] = fmt.Sprint(v)
					}
				}
			}
			if len(env) > 0 {
				svc["env"] = env
			}
		}

		if portsRaw := svcRaw["ports"]; portsRaw != nil {
			if ports, ok := portsRaw.([]any); ok {
				for _, p := range ports {
					if ps, ok := p.(string); ok {
						host, container := parseComposePort(ps)
						if host > 0 && container > 0 {
							// HTTP-ish ports (80, 443, 8080, 9999, etc)
							if isHTTPPort(container) {
								httpPorts = append(httpPorts, portMapping{host, container, svcName})
							} else {
								tcpPorts = append(tcpPorts, portMapping{host, container, svcName})
							}
						}
					}
				}
			}
		}

		if user, ok := asString(svcRaw["user"]); ok {
			svc["user"] = user
		}

		services[svcName] = svc
	}

	cfg["services"] = services

	target := map[string]any{}
	if opts.Host != "" {
		target["host"] = opts.Host
	} else {
		target["host"] = "CHANGE_ME"
	}
	if opts.User != "" {
		target["user"] = opts.User
	} else {
		target["user"] = "CHANGE_ME"
	}
	if opts.SSHKey != "" {
		target["ssh_key"] = opts.SSHKey
	}
	if opts.RepoDir != "" {
		target["repo_dir"] = opts.RepoDir
	} else {
		target["repo_dir"] = fmt.Sprintf("/home/%s/%s", opts.User, projectName)
	}

	var dirs []any
	dirsSeen := map[string]bool{}
	for _, svcName := range sortedMapKeys(services) {
		svcMap, ok := services[svcName].(map[string]any)
		if !ok {
			continue
		}
		volsRaw, ok := svcMap["volumes"]
		if !ok {
			continue
		}
		vols, ok := volsRaw.([]any)
		if !ok {
			continue
		}
		for _, v := range vols {
			vs, ok := v.(string)
			if !ok {
				continue
			}
			hostPath := strings.SplitN(vs, ":", 2)[0]
			if hostPath == "" || hostPath == "." || dirsSeen[hostPath] {
				continue
			}
			// Include absolute paths and ${VAR} template paths
			if hostPath[0] == '/' || strings.HasPrefix(hostPath, "${") {
				dirsSeen[hostPath] = true
				dirs = append(dirs, hostPath)
			}
		}
	}
	if len(dirs) > 0 {
		target["dirs"] = dirs
	}

	if targetEnv != nil {
		target["env"] = targetEnv
	}

	if len(httpPorts) > 0 || len(tcpPorts) > 0 {
		expose := map[string]any{}
		for _, p := range httpPorts {
			portKey := fmt.Sprintf("%d", p.hostPort)
			routes, ok := expose[portKey].(map[string]any)
			if !ok {
				routes = map[string]any{}
			}
			routes["/"] = fmt.Sprintf("%s:%d", p.service, p.containerPort)
			expose[portKey] = routes
		}
		for _, p := range tcpPorts {
			portKey := fmt.Sprintf("%d", p.hostPort)
			expose[portKey] = fmt.Sprintf("%s:%d", p.service, p.containerPort)
		}
		target["expose"] = expose
	}

	cfg["targets"] = map[string]any{
		"main": target,
	}

	format := strings.ToLower(opts.Format)
	if format == "" {
		if opts.Output != "" {
			switch strings.ToLower(filepath.Ext(opts.Output)) {
			case ".json":
				format = "json"
			case ".conf", ".hocon":
				format = "hocon"
			default:
				format = "yaml"
			}
		} else {
			format = "yaml"
		}
	}

	var output string
	switch format {
	case "json":
		output = generateJSONFromMap(cfg)
	case "hocon", "conf":
		output = generateHOCONFromMap(cfg, 0)
	default:
		output = generateYAMLFromMap(cfg, 0)
	}

	if opts.Output != "" {
		if err := os.WriteFile(opts.Output, []byte(output), 0o644); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
		fmt.Printf("generated qqd config (%s): %s\n", format, opts.Output)
		fmt.Printf("review the config and run: qqd deploy -c %s\n", opts.Output)
	} else {
		fmt.Print(output)
	}

	return nil
}

// generateYAMLFromMap produces YAML from a nested map.
func generateYAMLFromMap(m map[string]any, indent int) string {
	var b strings.Builder
	prefix := strings.Repeat("  ", indent)

	// Ordered keys for deterministic output
	keys := sortedMapKeys(m)
	// Put name first, then services, then targets
	priorityOrder := []string{"name", "repo", "branch", "sync", "runtime", "proxy", "services", "targets"}
	ordered := make([]string, 0, len(keys))
	added := map[string]bool{}
	for _, pk := range priorityOrder {
		for _, k := range keys {
			if k == pk {
				ordered = append(ordered, k)
				added[k] = true
			}
		}
	}
	for _, k := range keys {
		if !added[k] {
			ordered = append(ordered, k)
		}
	}

	for _, k := range ordered {
		v := m[k]
		switch val := v.(type) {
		case map[string]any:
			b.WriteString(fmt.Sprintf("%s%s:\n", prefix, k))
			b.WriteString(generateYAMLFromMap(val, indent+1))
		case []any:
			b.WriteString(fmt.Sprintf("%s%s:\n", prefix, k))
			for _, item := range val {
				if itemMap, ok := item.(map[string]any); ok {
					b.WriteString(fmt.Sprintf("%s  -\n", prefix))
					b.WriteString(generateYAMLFromMap(itemMap, indent+2))
				} else {
					b.WriteString(fmt.Sprintf("%s  - %s\n", prefix, yamlEmitScalar(item)))
				}
			}
		default:
			b.WriteString(fmt.Sprintf("%s%s: %s\n", prefix, k, yamlEmitScalar(v)))
		}
	}
	return b.String()
}

// yamlEmitScalar preserves source scalar types for config conversion.
func yamlEmitScalar(v any) string {
	switch val := v.(type) {
	case nil:
		return "null"
	case bool:
		if val {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		// Preserve "looks like an int" round-trip, since parseJSON normalizes
		// integer-valued floats to int.
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'g', -1, 64)
	case string:
		return yamlQuoteString(val)
	default:
		return yamlQuoteString(fmt.Sprint(val))
	}
}

// yamlQuoteString quotes a string for YAML output if the parser would otherwise
// reinterpret it as a non-string scalar or fail on it.
func yamlQuoteString(s string) string {
	if s == "" {
		return `""`
	}
	// Quote if it would parse as a non-string scalar (bool/null/number) or is
	// a YAML reserved word.
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "null", "~":
		return fmt.Sprintf("%q", s)
	}
	if _, err := strconv.Atoi(s); err == nil {
		return fmt.Sprintf("%q", s)
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return fmt.Sprintf("%q", s)
	}
	if strings.ContainsAny(s, ":{}[]#&*!|>'\",@`") || strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// yamlQuote is retained for callers that already have a stringified value.
// New code should call yamlEmitScalar with the original typed value instead.
func yamlQuote(s string) string {
	return yamlQuoteString(s)
}

// expandComposeVars expands ${VAR:-default} and ${VAR} in compose values.
func expandComposeVars(s string, env map[string]string) string {
	result := s
	for strings.Contains(result, "${") {
		start := strings.Index(result, "${")
		end := strings.Index(result[start:], "}")
		if end < 0 {
			break
		}
		end += start
		varExpr := result[start+2 : end]
		varName, defaultVal, hasDefault := strings.Cut(varExpr, ":-")
		if !hasDefault {
			varName = varExpr
		}
		if val, ok := env[varName]; ok && val != "" {
			result = result[:start] + val + result[end+1:]
		} else if hasDefault {
			result = result[:start] + defaultVal + result[end+1:]
		} else {
			result = result[:start] + "${" + varName + "}" + result[end+1:]
			break // prevent infinite loop
		}
	}
	return result
}

// parseComposePort parses "host:container" or "host:container/tcp" port strings.
func parseComposePort(s string) (host, container int) {
	s = strings.TrimSuffix(s, "/tcp")
	s = strings.TrimSuffix(s, "/udp")
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	fmt.Sscanf(parts[0], "%d", &host)
	fmt.Sscanf(parts[1], "%d", &container)
	return host, container
}

func isHTTPPort(port int) bool {
	return port == 80 || port == 443 || port == 8080 || port == 8443 || port == 3000 || port == 9999 || port == 9998
}

// sanitizeProjectName creates a safe project name from a directory name.
func sanitizeProjectName(name string) string {
	name = strings.ToLower(name)
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, name)
	return strings.Trim(name, "-")
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// generateJSONFromMap produces indented JSON from a nested map.
func generateJSONFromMap(m map[string]any) string {
	data, err := jsonMarshalIndent(m)
	if err != nil {
		return "{}"
	}
	return string(data) + "\n"
}

// jsonMarshalIndent marshals with sorted keys.
func jsonMarshalIndent(v any) ([]byte, error) {
	var b strings.Builder
	enc := jsonEncoder{w: &b}
	enc.encode(v, 0)
	return []byte(b.String()), nil
}

type jsonEncoder struct{ w *strings.Builder }

func (e *jsonEncoder) encode(v any, indent int) {
	prefix := strings.Repeat("  ", indent)
	switch val := v.(type) {
	case map[string]any:
		e.w.WriteString("{\n")
		keys := sortedMapKeys(val)
		for i, k := range keys {
			e.w.WriteString(fmt.Sprintf("%s  %q: ", prefix, k))
			e.encode(val[k], indent+1)
			if i < len(keys)-1 {
				e.w.WriteString(",")
			}
			e.w.WriteString("\n")
		}
		e.w.WriteString(prefix + "}")
	case []any:
		e.w.WriteString("[\n")
		for i, item := range val {
			e.w.WriteString(prefix + "  ")
			e.encode(item, indent+1)
			if i < len(val)-1 {
				e.w.WriteString(",")
			}
			e.w.WriteString("\n")
		}
		e.w.WriteString(prefix + "]")
	case string:
		e.w.WriteString(fmt.Sprintf("%q", val))
	case int:
		e.w.WriteString(fmt.Sprintf("%d", val))
	case bool:
		if val {
			e.w.WriteString("true")
		} else {
			e.w.WriteString("false")
		}
	case nil:
		e.w.WriteString("null")
	default:
		e.w.WriteString(fmt.Sprintf("%q", fmt.Sprint(val)))
	}
}

// generateHOCONFromMap produces HOCON config from a nested map.
func generateHOCONFromMap(m map[string]any, indent int) string {
	var b strings.Builder
	prefix := strings.Repeat("  ", indent)

	keys := sortedMapKeys(m)
	priorityOrder := []string{"name", "repo", "branch", "sync", "runtime", "proxy", "services", "targets"}
	ordered := make([]string, 0, len(keys))
	added := map[string]bool{}
	for _, pk := range priorityOrder {
		for _, k := range keys {
			if k == pk {
				ordered = append(ordered, k)
				added[k] = true
			}
		}
	}
	for _, k := range keys {
		if !added[k] {
			ordered = append(ordered, k)
		}
	}

	for i, k := range ordered {
		v := m[k]
		switch val := v.(type) {
		case map[string]any:
			b.WriteString(fmt.Sprintf("%s%s {\n", prefix, k))
			b.WriteString(generateHOCONFromMap(val, indent+1))
			b.WriteString(fmt.Sprintf("%s}\n", prefix))
		case []any:
			b.WriteString(fmt.Sprintf("%s%s = [", prefix, k))
			for j, item := range val {
				if j > 0 {
					b.WriteString(", ")
				}
				b.WriteString(hoconQuote(fmt.Sprint(item)))
			}
			b.WriteString("]\n")
		default:
			b.WriteString(fmt.Sprintf("%s%s = %s\n", prefix, k, hoconQuote(fmt.Sprint(v))))
		}
		// Add blank line between top-level sections
		if indent == 0 && i < len(ordered)-1 {
			nextKey := ""
			if i+1 < len(ordered) {
				nextKey = ordered[i+1]
			}
			if isHOCONSection(k) || isHOCONSection(nextKey) {
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

func hoconQuote(s string) string {
	if s == "" || s == "true" || s == "false" {
		return fmt.Sprintf("%q", s)
	}
	// Numbers don't need quoting
	isNum := true
	for _, c := range s {
		if !((c >= '0' && c <= '9') || c == '.' || c == '-') {
			isNum = false
			break
		}
	}
	if isNum {
		return s
	}
	if strings.ContainsAny(s, " \t\n{}[]=$\"#") {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q", s)
}

func isHOCONSection(key string) bool {
	return key == "services" || key == "targets" || key == "hooks" || key == "build"
}

// ConvertConfig reads a qqd config file and writes it in a different format.
func ConvertConfig(inputPath, outputPath, format string) error {
	data, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", inputPath, err)
	}

	parser := configParserForFile(inputPath, data)
	m, err := parser.Parse(data)
	if err != nil {
		return fmt.Errorf("parse %s: %w", inputPath, err)
	}
	if runtime, ok := asString(m["runtime"]); ok && strings.EqualFold(strings.TrimSpace(runtime), "docker") {
		return fmt.Errorf("convert: runtime \"docker\" is no longer supported; remove the `runtime` field or set it to \"podman\"")
	}

	if format == "" && outputPath != "" {
		switch strings.ToLower(filepath.Ext(outputPath)) {
		case ".json":
			format = "json"
		case ".conf", ".hocon":
			format = "hocon"
		default:
			format = "yaml"
		}
	}
	if format == "" {
		format = "yaml"
	}

	var output string
	switch strings.ToLower(format) {
	case "json":
		output = generateJSONFromMap(m)
	case "hocon", "conf":
		output = generateHOCONFromMap(m, 0)
	default:
		output = generateYAMLFromMap(m, 0)
	}

	if outputPath != "" {
		if err := os.WriteFile(outputPath, []byte(output), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", outputPath, err)
		}
		fmt.Printf("converted %s -> %s (%s)\n", inputPath, outputPath, format)
	} else {
		fmt.Print(output)
	}
	return nil
}

func rewriteServiceRefs(value, project string, composeServices map[string]any) string {
	if _, ok := composeServices[value]; ok {
		return project + "-" + value
	}
	for svcName := range composeServices {
		old := svcName + ":"
		replacement := project + "-" + svcName + ":"
		if strings.Contains(value, old) {
			idx := strings.Index(value, old)
			if idx == 0 || !isAlphaNum(value[idx-1]) {
				value = value[:idx] + replacement + value[idx+len(old):]
			}
		}
	}
	return value
}

func isAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '-' || b == '_'
}

// ensureVolumeFlags adds :z,U to bind mount volumes that don't already have flags.
// This is needed for Podman rootless mode (z = SELinux relabel, U = chown to container user).
func ensureVolumeFlags(vol string) string {
	parts := strings.SplitN(vol, ":", 3)
	if len(parts) < 2 {
		return vol // not a bind mount
	}
	hostPath := parts[0]
	containerPath := parts[1]
	// Skip if host path looks like a named volume (no / or $)
	if hostPath != "" && hostPath[0] != '/' && hostPath[0] != '$' && hostPath[0] != '~' && hostPath[0] != '.' {
		return vol
	}
	if len(parts) == 3 {
		// Already has flags - add z,U if not present
		flags := parts[2]
		if !strings.Contains(flags, "U") {
			flags += ",U"
		}
		if !strings.Contains(flags, "z") && !strings.Contains(flags, "Z") {
			flags += ",z"
		}
		return hostPath + ":" + containerPath + ":" + flags
	}
	// No flags - add z,U
	return hostPath + ":" + containerPath + ":z,U"
}

// normalizeImageName adds docker.io/ prefix to short image names
// that Podman can't resolve non-interactively.
// "postgres:16.1" -> "docker.io/library/postgres:16.1"
// "victoriametrics/victoria-metrics:v1.135.0" -> "docker.io/victoriametrics/victoria-metrics:v1.135.0"
// "ghcr.io/org/image:tag" -> unchanged (has registry domain)
func normalizeImageName(image string) string {
	if strings.HasPrefix(image, "${") {
		return image // template var, don't touch
	}
	// Check if image already has a registry domain (contains a dot before the first slash)
	slashIdx := strings.Index(image, "/")
	if slashIdx > 0 {
		prefix := image[:slashIdx]
		if strings.Contains(prefix, ".") || prefix == "localhost" {
			return image // already has registry (ghcr.io, docker.io, quay.io, etc.)
		}
		// org/image format like "victoriametrics/victoria-metrics:v1.135.0"
		return "docker.io/" + image
	}
	// No slash - short name like "postgres:16.1"
	return "docker.io/library/" + image
}

// absPath returns the absolute path, ignoring errors.
func absPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
