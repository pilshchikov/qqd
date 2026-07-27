package qqd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
)

func containerName(project, service string) string {
	return fmt.Sprintf("%s-%s", project, service)
}

func containerUnit(project, service string) string {
	return fmt.Sprintf("%s-%s.service", project, service)
}

func networkUnit(project string) string {
	return fmt.Sprintf("%s-network.service", project)
}

func allContainerUnits(project string, services map[string]ServiceConfig) []string {
	var out []string
	for _, name := range sortedKeys(services) {
		svc := services[name]
		out = append(out, serviceUnits(project, name, svc)...)
	}
	return out
}

func serviceUnits(project, service string, svc ServiceConfig) []string {
	if isReplicated(svc) {
		units := make([]string, 0, effectiveReplicas(svc))
		for i := 1; i <= effectiveReplicas(svc); i++ {
			units = append(units, fmt.Sprintf("%s-%s-%d.service", project, service, i))
		}
		return units
	}
	return []string{containerUnit(project, service)}
}

// projectMarkerPrefix starts the comment qqd writes as the first line of every
// quadlet file it generates. Ownership is proven by file content, not by
// filename prefix: two projects whose names share a prefix ("app" and
// "app-metrics") produce unit files that look alike, so a filename-only check
// let a deploy of one project stop and delete the other project's units.
const projectMarkerPrefix = "# qqd-project="

// projectMarker returns the ownership marker line for a project.
func projectMarker(project string) string {
	return projectMarkerPrefix + project + "\n"
}

// withProjectMarker prepends the ownership marker to generated quadlet content.
func withProjectMarker(project, content string) string {
	marker := projectMarker(project)
	if strings.HasPrefix(content, marker) {
		return content
	}
	return marker + content
}

// quadletMarkers maps quadlet filename to the project that owns it, read from
// the marker line of every file in qdDir in one round trip. Files written by
// older qqd versions carry no marker and are absent from the result.
func quadletMarkers(ctx context.Context, exec Executor, qdDir string) map[string]string {
	owners := map[string]string{}
	// qdDir may contain "~" and must stay unquoted for tilde expansion.
	out, err := exec.Run(ctx, fmt.Sprintf("grep -H '^%s' %s/* 2>/dev/null || true", projectMarkerPrefix, qdDir))
	if err != nil {
		return owners
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		filePath, marker, ok := strings.Cut(line, ":")
		if !ok || !strings.HasPrefix(marker, projectMarkerPrefix) {
			continue
		}
		owners[path.Base(filePath)] = strings.TrimSpace(strings.TrimPrefix(marker, projectMarkerPrefix))
	}
	return owners
}

// serviceForQuadlet resolves a quadlet filename to the service it belongs to.
// Matching runs against the configured service set rather than parsing the
// filename, so a service legitimately named "api-2" is not mistaken for replica
// 2 of "api", and a unit belonging to a different project that merely shares
// this project's name prefix resolves to nothing.
// Reports false for the network and proxy files.
func serviceForQuadlet(project, filename, ext string, services map[string]ServiceConfig) (string, bool) {
	prefix := project + "-"
	base := strings.TrimSuffix(filename, ext)
	if !strings.HasPrefix(base, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(base, prefix)
	if rest == "" || rest == "proxy" || rest == "network" {
		return "", false
	}
	if _, ok := services[rest]; ok {
		return rest, true
	}
	// Longest name first: with services "api" and "api-2" both configured,
	// "api-2" must win over "api" + suffix "2".
	names := sortedKeys(services)
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	for _, name := range names {
		suffix, ok := strings.CutPrefix(rest, name+"-")
		if !ok {
			continue
		}
		if isReplicaSuffix(suffix) || isValidSlotHash(suffix) {
			return name, true
		}
	}
	return "", false
}

// isReplicaSuffix reports whether s is a replica index (one or more digits).
func isReplicaSuffix(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// projectOwnsContainer reports whether a runtime container name belongs to this
// project: the proxy, a configured service, or one of that service's replica /
// slot variants. Used instead of a "<project>-" name filter, which also matches
// the containers of a project whose name starts with this project's name.
func projectOwnsContainer(project, container string, services map[string]ServiceConfig) bool {
	if container == project+"-proxy" {
		return true
	}
	_, ok := serviceForQuadlet(project, container, "", services)
	return ok
}

// joinQuoted shell-quotes and joins values for command construction.
func joinQuoted(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, shellQuote(v))
	}
	return strings.Join(quoted, " ")
}

func fileExistsRemote(ctx context.Context, exec Executor, path string) bool {
	// Don't use shellQuote here: paths containing ~ must remain unquoted for tilde expansion.
	// These paths are generated internally and are safe from injection.
	_, err := exec.Run(ctx, fmt.Sprintf("test -f %s", path))
	return err == nil
}

// heredocDelimiter returns a heredoc delimiter that does not appear as a line of
// content. A fixed delimiter is unsafe: any value that contains the delimiter on
// its own line (an env var, a `file::` secret, a generated route) would end the
// heredoc early, truncate the written file, and hand the remainder of the
// content to the remote shell as commands. Deterministic so identical content
// always produces an identical command.
func heredocDelimiter(content string) string {
	delim := "QD_EOF"
	for containsLine(content, delim) {
		sum := sha256.Sum256([]byte(delim + content))
		delim = "QD_EOF_" + hex.EncodeToString(sum[:4])
	}
	return delim
}

// containsLine reports whether needle appears as a complete line of s.
func containsLine(s, needle string) bool {
	for _, line := range strings.Split(s, "\n") {
		if line == needle {
			return true
		}
	}
	return false
}

// remoteWriteBody returns the delimiter plus newline-terminated content for a
// heredoc write. Content is always newline-terminated so the closing delimiter
// lands on a line of its own.
func remoteWriteBody(content string) (delim, body string) {
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return heredocDelimiter(content), content
}

// remoteWriteCmd builds a shell command that writes content to path.
func remoteWriteCmd(path, content string) string {
	delim, body := remoteWriteBody(content)
	return fmt.Sprintf("cat > %s <<'%s'\n%s%s", path, delim, body, delim)
}

// remoteWriteCmdSudo is remoteWriteCmd with an optional sudo prefix.
func remoteWriteCmdSudo(sudo, path, content string) string {
	if sudo == "" {
		return remoteWriteCmd(path, content)
	}
	delim, body := remoteWriteBody(content)
	return fmt.Sprintf("%ssh -c 'cat > %s' <<'%s'\n%s%s", sudo, path, delim, body, delim)
}

// atomicWriteRemote writes content to path atomically via a temp file + rename.
// This prevents file watchers (like Traefik's) from seeing a truncated file, and
// the rename only runs when the write itself succeeded.
func atomicWriteRemote(ctx context.Context, exec Executor, path, content string) error {
	tmp := path + ".tmp"
	delim, body := remoteWriteBody(content)
	cmd := fmt.Sprintf("cat > %s <<'%s' && mv %s %s\n%s%s", tmp, delim, tmp, path, body, delim)
	_, err := exec.Run(ctx, cmd)
	return err
}

// removeStaleQuadlets stops and removes project container quadlet files that are
// no longer needed. This handles cases like replica count changes or switching
// between replicated and non-replicated mode. Only files for services included
// in the current deployment are considered — files for other services are left
// untouched so that partial deploys (e.g. "qqd deploy server") don't break
// unrelated services. When fullDeploy is true (no service names on the CLI),
// files for services no longer in the config are also removed, since a full
// deploy means the config is the complete source of truth — but only when the
// file's marker proves this project wrote it.
// Slot files (hash-based) for services in
// slottedSvcs are managed by slotDeploy and are left untouched.
func (a *App) removeStaleQuadlets(ctx context.Context, project, qdDir string, newFiles []QuadletFile, deployServices, allServices map[string]ServiceConfig, exec Executor, fullDeploy bool, slottedSvcs map[string]bool) {
	newNames := map[string]bool{}
	for _, f := range newFiles {
		newNames[f.Name] = true
	}
	// Units still referenced by a retained/written quadlet's After=/Requires=/Wants=
	// must never be removed, even if their own quadlet isn't in newFiles this pass.
	// Reaping such a base unit (e.g. a non-slotted db) breaks every unit that
	// requires it and can take the whole stack down during a partial deploy or rollback.
	referenced := referencedUnits(newFiles)
	out, err := exec.Run(ctx, fmt.Sprintf("ls -1 %s 2>/dev/null || true", qdDir))
	if err != nil {
		return
	}
	ext := a.rt().UnitExtension()
	prefix := project + "-"
	var candidates []string
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		if name == "" || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ext) {
			continue
		}
		if newNames[name] {
			continue
		}
		// Slot files are managed by slotDeploy / cleanStaleSlots.
		if isSlotFile(project, name, ext, slottedSvcs) {
			continue
		}
		candidates = append(candidates, name)
	}
	if len(candidates) == 0 {
		return
	}
	owners := quadletMarkers(ctx, exec, qdDir)

	for _, name := range candidates {
		owner, marked := owners[name]
		if marked && owner != project {
			continue // belongs to another project whose name shares our prefix
		}
		svcName, known := serviceForQuadlet(project, name, ext, allServices)
		if known {
			// Skip the standard-named file for slotted services — slotDeploy manages the transition.
			// Only skip the exact standard name (e.g., proj-server.container), not replica files
			// (e.g., proj-server-1.container) which should still be removed when switching from replicated.
			if slottedSvcs[svcName] && name == fmt.Sprintf("%s-%s%s", project, svcName, ext) {
				continue
			}
			if _, inDeploy := deployServices[svcName]; !inDeploy && !fullDeploy {
				continue // service not in this deploy run — leave it alone
			}
		} else {
			// Matches no configured service: either a service that was dropped
			// from the config, or a file qqd never wrote. Reap it only on a full
			// deploy and only when the marker proves this project owns it.
			if !fullDeploy || !marked {
				continue
			}
		}
		unit := strings.TrimSuffix(name, ext) + ".service"
		if referenced[unit] {
			continue // still required by a retained unit — removing it would break the dependant
		}
		fmt.Fprintf(a.Stdout, "  %s stale unit %s\n", yellow("removing"), dim(unit))
		exec.Run(ctx, fmt.Sprintf(a.sctl()+" stop %s 2>/dev/null || true", shellQuote(unit)))
		exec.Run(ctx, fmt.Sprintf("rm -f %s/%s", qdDir, shellQuote(name)))
	}
}

// projectQuadletFiles returns the quadlet files in qdDir that belong to this
// project: the network and proxy files, files that resolve to a configured
// service, and any file whose marker names this project. Files owned by a
// different project — including one whose name shares this project's prefix —
// are never returned.
func (a *App) projectQuadletFiles(ctx context.Context, project, qdDir string, services map[string]ServiceConfig, exec Executor) []string {
	out, err := exec.Run(ctx, fmt.Sprintf("ls -1 %s 2>/dev/null || true", qdDir))
	if err != nil {
		return nil
	}
	ext := a.rt().UnitExtension()
	netFile := a.rt().NetworkFileName(project)
	proxyFile := project + "-proxy" + ext
	owners := quadletMarkers(ctx, exec, qdDir)

	var files []string
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		owner, marked := owners[name]
		if marked && owner != project {
			continue
		}
		if name == netFile || name == proxyFile {
			files = append(files, name)
			continue
		}
		if !strings.HasSuffix(name, ext) {
			continue
		}
		if _, known := serviceForQuadlet(project, name, ext, services); known || marked {
			files = append(files, name)
		}
	}
	return files
}

// referencedUnits returns the set of systemd unit names referenced by any
// After=/Requires=/Wants= directive across the given quadlet files. These are
// the dependencies retained/written units rely on; removing one would break the
// unit that declares it.
func referencedUnits(files []QuadletFile) map[string]bool {
	refs := map[string]bool{}
	for _, f := range files {
		for _, line := range strings.Split(f.Content, "\n") {
			line = strings.TrimSpace(line)
			var deps string
			switch {
			case strings.HasPrefix(line, "After="):
				deps = strings.TrimPrefix(line, "After=")
			case strings.HasPrefix(line, "Requires="):
				deps = strings.TrimPrefix(line, "Requires=")
			case strings.HasPrefix(line, "Wants="):
				deps = strings.TrimPrefix(line, "Wants=")
			default:
				continue
			}
			for _, unit := range strings.Fields(deps) {
				refs[unit] = true
			}
		}
	}
	return refs
}

// contains reports whether needle exists in values.
func contains(values []string, needle string) bool {
	for _, v := range values {
		if v == needle {
			return true
		}
	}
	return false
}

// mergeUnique merges two string slices, removing duplicates.
func mergeUnique(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
