package qqd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"
)

// Release represents a single deployment record stored on the target.
type Release struct {
	ID         string            `json:"id"`          // unique identifier (timestamp-based)
	Timestamp  string            `json:"timestamp"`   // RFC3339 deploy time
	Services   map[string]string `json:"services"`    // service name → image ref
	ConfigHash string            `json:"config_hash"` // sha256 of merged config content
}

const maxReleases = 10
const releaseDir = "~/.config/qqd/%s/releases"

// releaseID generates a release ID from current time.
func releaseID() string {
	return time.Now().UTC().Format("20060102-150405.000")
}

func releaseImagesFromServices(services map[string]ServiceConfig) map[string]string {
	images := map[string]string{}
	for name, svc := range services {
		images[name] = svc.Image
	}
	return images
}

func newReleaseFromImages(images map[string]string) Release {
	return Release{
		ID:        releaseID(),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Services:  maps.Clone(images),
	}
}

// newRelease creates a release record from current deployment state.
func newRelease(services map[string]ServiceConfig) Release {
	return newReleaseFromImages(releaseImagesFromServices(services))
}

// saveRelease writes a release record to the target.
func saveRelease(ctx context.Context, exec Executor, project string, rel Release) error {
	dir := fmt.Sprintf(releaseDir, project)
	if _, err := exec.Run(ctx, fmt.Sprintf("mkdir -p %s", dir)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rel, "", "  ")
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%s/%s.json", dir, rel.ID)
	heredoc := fmt.Sprintf("cat > %s <<'QD_EOF'\n%s\nQD_EOF", path, string(data))
	_, err = exec.Run(ctx, heredoc)
	return err
}

// listReleases returns all releases on a target, sorted newest first.
func listReleases(ctx context.Context, exec Executor, project string) ([]Release, error) {
	dir := fmt.Sprintf(releaseDir, project)
	out, err := exec.Run(ctx, fmt.Sprintf("ls -1 %s 2>/dev/null || true", dir))
	if err != nil {
		return nil, err
	}
	var releases []Release
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		if name == "" || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := fmt.Sprintf("%s/%s", dir, name)
		data, err := exec.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null || true", path))
		if err != nil {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		var rel Release
		if err := json.Unmarshal([]byte(data), &rel); err != nil {
			continue
		}
		releases = append(releases, rel)
	}
	// Sort by ID descending (newest first)
	sort.Slice(releases, func(i, j int) bool {
		return releases[i].ID > releases[j].ID
	})
	return releases, nil
}

// previousRelease returns the release before the most recent one.
func previousRelease(ctx context.Context, exec Executor, project string) (Release, error) {
	releases, err := listReleases(ctx, exec, project)
	if err != nil {
		return Release{}, err
	}
	if len(releases) < 2 {
		return Release{}, fmt.Errorf("no previous release available (found %d releases)", len(releases))
	}
	return releases[1], nil
}

func latestRelease(ctx context.Context, exec Executor, project string) (Release, bool, error) {
	releases, err := listReleases(ctx, exec, project)
	if err != nil {
		return Release{}, false, err
	}
	if len(releases) == 0 {
		return Release{}, false, nil
	}
	return releases[0], true, nil
}

func releaseImagesForDeploy(ctx context.Context, exec Executor, project string, deployed map[string]ServiceConfig, fullDeploy bool) (map[string]string, error) {
	images := releaseImagesFromServices(deployed)
	if fullDeploy {
		return images, nil
	}

	latest, ok, err := latestRelease(ctx, exec, project)
	if err != nil {
		return nil, err
	}
	if !ok {
		return images, nil
	}

	merged := maps.Clone(latest.Services)
	for name, image := range images {
		merged[name] = image
	}
	return merged, nil
}

// trimReleases removes old releases beyond maxReleases.
func trimReleases(ctx context.Context, exec Executor, project string) {
	releases, err := listReleases(ctx, exec, project)
	if err != nil || len(releases) <= maxReleases {
		return
	}
	dir := fmt.Sprintf(releaseDir, project)
	for _, rel := range releases[maxReleases:] {
		exec.Run(ctx, fmt.Sprintf("rm -f %s/%s.json", dir, rel.ID))
	}
}
