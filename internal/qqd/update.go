package qqd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var serviceOpenRE = regexp.MustCompile(`^\s*([A-Za-z0-9_-]+)\s*\{`)
var serviceNameOnlyRE = regexp.MustCompile(`^\s*([A-Za-z0-9_-]+)\s*$`)
var imageLineRE = regexp.MustCompile(`((?:^|\{)\s*image\s*=\s*)(?:"([^"]+)"|([^\s#}]+))(.*)$`)

// updateConfigVersions updates image tags for selected services in-place.
func updateConfigVersions(configPath string, updates map[string]string) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	lines := strings.Split(string(raw), "\n")

	inServices := false
	servicesDepth := 0
	braceDepth := 0
	currentService := ""
	serviceDepth := 0
	pendingServices := false
	pendingService := ""
	updated := map[string]bool{}

	for i := range lines {
		line := lines[i]
		trim := strings.TrimSpace(line)
		open, close := countBracesOutsideStrings(line)

		if !inServices {
			if pendingServices && strings.Contains(line, "{") {
				inServices = true
				pendingServices = false
				servicesDepth = braceDepth + open - close
			} else if trim == "services" {
				pendingServices = true
			} else if strings.HasPrefix(trim, "services") && strings.Contains(line, "{") {
				inServices = true
				pendingServices = false
				servicesDepth = braceDepth + open - close
			}
		} else {
			if currentService == "" && braceDepth == servicesDepth {
				if pendingService != "" && strings.Contains(line, "{") {
					currentService = pendingService
					pendingService = ""
					serviceDepth = braceDepth + open - close
				} else if m := serviceOpenRE.FindStringSubmatch(line); len(m) == 2 {
					currentService = m[1]
					serviceDepth = braceDepth + open - close
				} else if m := serviceNameOnlyRE.FindStringSubmatch(line); len(m) == 2 {
					pendingService = m[1]
				}
			}
			if currentService != "" {
				if newVer, ok := updates[currentService]; ok {
					if m := imageLineRE.FindStringSubmatch(line); len(m) == 5 {
						image := m[2]
						quoted := true
						if image == "" {
							image = m[3]
							quoted = false
						}
						repo, _, ok := splitImageTag(image)
						if !ok {
							return fmt.Errorf("service %s image has no version tag: %s", currentService, image)
						}
						if quoted {
							lines[i] = m[1] + `"` + repo + ":" + newVer + `"` + m[4]
						} else {
							lines[i] = m[1] + repo + ":" + newVer + m[4]
						}
						updated[currentService] = true
					}
				}
				if braceDepth+open-close < serviceDepth {
					currentService = ""
					serviceDepth = 0
					pendingService = ""
				}
			}
			if braceDepth+open-close < servicesDepth {
				inServices = false
				servicesDepth = 0
				pendingServices = false
				pendingService = ""
			}
		}
		braceDepth += open - close
	}

	for svc := range updates {
		if !updated[svc] {
			return fmt.Errorf("failed to update service %s image version", svc)
		}
	}

	st, err := os.Stat(configPath)
	if err != nil {
		return err
	}
	dir := filepath.Dir(configPath)
	tmp, err := os.CreateTemp(dir, filepath.Base(configPath)+".tmp.*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(st.Mode()); err != nil {
		tmp.Close()
		return err
	}
	updatedContent := []byte(strings.Join(lines, "\n"))
	if _, err := tmp.Write(updatedContent); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, configPath)
}

// splitImageTag splits an image reference into repository and tag.
func splitImageTag(image string) (repo, tag string, ok bool) {
	colon := strings.LastIndex(image, ":")
	slash := strings.LastIndex(image, "/")
	if colon == -1 || colon < slash {
		return image, "", false
	}
	return image[:colon], image[colon+1:], true
}

// bumpVersion increments the right-most numeric component in a version tag.
func bumpVersion(tag string) (string, error) {
	lastDigit := -1
	for i := len(tag) - 1; i >= 0; i-- {
		if tag[i] >= '0' && tag[i] <= '9' {
			lastDigit = i
			break
		}
	}
	if lastDigit == -1 {
		return "", fmt.Errorf("version has no numeric component: %s", tag)
	}
	start := lastDigit
	for start >= 0 && tag[start] >= '0' && tag[start] <= '9' {
		start--
	}
	start++
	n, err := strconv.Atoi(tag[start : lastDigit+1])
	if err != nil {
		return "", err
	}
	return tag[:start] + strconv.Itoa(n+1) + tag[lastDigit+1:], nil
}

// countBracesOutsideStrings counts braces while ignoring quoted strings/comments.
func countBracesOutsideStrings(line string) (open, close int) {
	inQuote := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if ch == '"' && (i == 0 || line[i-1] != '\\') {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		if ch == '#' {
			break
		}
		if ch == '/' && i+1 < len(line) && line[i+1] == '/' {
			break
		}
		switch ch {
		case '{':
			open++
		case '}':
			close++
		}
	}
	return
}
