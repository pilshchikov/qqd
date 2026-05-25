package qqd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var envVarRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// resolveLocalPath expands "~" and resolves relative paths against invocationWD.
func resolveLocalPath(invocationWD, p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		switch {
		case p == "~":
			p = home
		case strings.HasPrefix(p, "~/"):
			p = filepath.Join(home, p[2:])
		}
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	return filepath.Clean(filepath.Join(invocationWD, p)), nil
}

// resolveFileRef resolves "file::<path>" references by reading the file contents.
// If value doesn't start with "file::", it is returned unchanged.
// The raw file content is returned (trimmed). Newlines, quotes, and other special
// characters are preserved and handled later by formatQuadletEnv.
func resolveFileRef(invocationWD, value string) (string, error) {
	const prefix = "file::"
	if !strings.HasPrefix(value, prefix) {
		return value, nil
	}
	path, err := resolveLocalPath(invocationWD, value[len(prefix):])
	if err != nil {
		return "", fmt.Errorf("resolve file ref path: %w", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read file ref %s: %w", path, err)
	}
	return strings.TrimSpace(string(content)), nil
}

// expandVars substitutes ${VAR} placeholders using vars first, then OS env.
func expandVars(value string, vars map[string]string) string {
	return envVarRE.ReplaceAllStringFunc(value, func(match string) string {
		sub := envVarRE.FindStringSubmatch(match)
		if len(sub) != 2 {
			return match
		}
		if v, ok := vars[sub[1]]; ok {
			return v
		}
		if v := os.Getenv(sub[1]); v != "" {
			return v
		}
		return match
	})
}

// deepMergeMaps recursively merges src into dst.
func deepMergeMaps(dst, src map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range src {
		if mv, ok := v.(map[string]any); ok {
			if existing, ok := dst[k].(map[string]any); ok {
				dst[k] = deepMergeMaps(existing, mv)
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

// asMap converts supported map-like values to map[string]any.
func asMap(v any) (map[string]any, bool) {
	switch vv := v.(type) {
	case map[string]any:
		return vv, true
	case map[string]string:
		out := make(map[string]any, len(vv))
		for k, s := range vv {
			out[k] = s
		}
		return out, true
	default:
		return nil, false
	}
}

// asString converts common scalar types to their string form.
func asString(v any) (string, bool) {
	switch vv := v.(type) {
	case string:
		return vv, true
	case int:
		return strconv.Itoa(vv), true
	case int64:
		return strconv.FormatInt(vv, 10), true
	case float64:
		return strconv.FormatFloat(vv, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(vv), true
	default:
		return "", false
	}
}

// asInt converts common numeric/string values to int.
func asInt(v any) (int, bool) {
	switch vv := v.(type) {
	case int:
		return vv, true
	case int64:
		return int(vv), true
	case float64:
		return int(vv), true
	case string:
		i, err := strconv.Atoi(vv)
		if err != nil {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}

// asBool converts common bool/string values to bool.
func asBool(v any) (bool, bool) {
	switch vv := v.(type) {
	case bool:
		return vv, true
	case string:
		switch strings.ToLower(strings.TrimSpace(vv)) {
		case "true":
			return true, true
		case "false":
			return false, true
		default:
			return false, false
		}
	default:
		return false, false
	}
}

// asStringSlice normalizes supported values to []string.
func asStringSlice(v any) []string {
	switch vv := v.(type) {
	case nil:
		return nil
	case []string:
		out := append([]string{}, vv...)
		return out
	case []any:
		out := make([]string, 0, len(vv))
		for _, x := range vv {
			if s, ok := asString(x); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{vv}
	default:
		return nil
	}
}

// asStringMap normalizes map values to map[string]string.
func asStringMap(v any) map[string]string {
	m, ok := asMap(v)
	if !ok {
		return map[string]string{}
	}
	out := map[string]string{}
	for k, vv := range m {
		if s, ok := asString(vv); ok {
			out[k] = s
		}
	}
	return out
}

// mergeBuild overlays non-zero override fields onto base.
func mergeBuild(base, override BuildConfig) BuildConfig {
	out := base
	if override.Strategy != "" {
		out.Strategy = override.Strategy
	}
	if override.CPU > 0 {
		out.CPU = override.CPU
	}
	if override.Memory != "" {
		out.Memory = override.Memory
	}
	if override.Host != "" {
		out.Host = override.Host
	}
	if override.User != "" {
		out.User = override.User
	}
	if override.SSHKey != "" {
		out.SSHKey = override.SSHKey
	}
	if override.SSHPort > 0 {
		out.SSHPort = override.SSHPort
	}
	if override.RepoDir != "" {
		out.RepoDir = override.RepoDir
	}
	if override.Delivery != "" {
		out.Delivery = override.Delivery
	}
	if override.Repo != "" {
		out.Repo = override.Repo
	}
	if override.Workflow != "" {
		out.Workflow = override.Workflow
	}
	if override.Branch != "" {
		out.Branch = override.Branch
	}
	if override.GitHubToken != "" {
		out.GitHubToken = override.GitHubToken
	}
	if override.Registry != "" {
		out.Registry = override.Registry
	}
	if override.RegistryUser != "" {
		out.RegistryUser = override.RegistryUser
	}
	if override.RegistryToken != "" {
		out.RegistryToken = override.RegistryToken
	}
	return out
}

// sortedKeys returns deterministic sorted map keys.
func sortedKeys[K ~string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// shellQuote produces a single-quoted shell-safe token.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// injectGHToken inserts a GitHub token into an HTTPS GitHub URL.
// "https://github.com/org/repo.git" becomes "https://<token>@github.com/org/repo.git".
// Non-HTTPS or non-GitHub URLs are returned unchanged.
func injectGHToken(repoURL, token string) string {
	if token == "" {
		return repoURL
	}
	const prefix = "https://github.com/"
	if strings.HasPrefix(repoURL, prefix) {
		return "https://" + token + "@github.com/" + strings.TrimPrefix(repoURL, prefix)
	}
	return repoURL
}

// resolveGHToken resolves gh_token value. If it equals "gh", it runs
// `gh auth token` locally to obtain the token. Otherwise returns the value as-is.
func resolveGHToken(ctx context.Context, local Executor, raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if raw == "gh" {
		out, err := local.Run(ctx, "gh auth token")
		if err != nil {
			return "", fmt.Errorf("gh auth token: %w", err)
		}
		return strings.TrimSpace(out), nil
	}
	return raw, nil
}

// rewriteDepsForSlots returns a copy of svc with DependsOn entries rewritten
// to point at slot units. E.g. if activeSlots["server"]="a1b2c3d4",
// a dependency on "server" becomes "server-a1b2c3d4" so the rendered quadlet's
// After=/Requires= references proj-server-a1b2c3d4.service.
func rewriteDepsForSlots(svc ServiceConfig, activeSlots map[string]string) ServiceConfig {
	if len(activeSlots) == 0 || len(svc.DependsOn) == 0 {
		return svc
	}
	newDeps := make([]string, len(svc.DependsOn))
	copy(newDeps, svc.DependsOn)
	changed := false
	for i, dep := range newDeps {
		if slot, ok := activeSlots[dep]; ok {
			newDeps[i] = dep + "-" + slot
			changed = true
		}
	}
	if !changed {
		return svc
	}
	svc.DependsOn = newDeps
	return svc
}
