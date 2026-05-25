package qqd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// loadEnvFile reads a .env file and returns key=value pairs.
// Supported formats:
//
//	KEY=value
//	KEY="quoted value"
//	KEY='single quoted value'
//	# comments
//	empty lines are skipped
//	export KEY=value (optional export prefix)
func loadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open env file %s: %w", path, err)
	}
	defer f.Close()

	env := map[string]string{}
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Strip optional "export " prefix.
		line = strings.TrimPrefix(line, "export ")

		// Split on first '='.
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("env file %s line %d: invalid format (expected KEY=value)", path, lineNum)
		}

		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("env file %s line %d: empty key", path, lineNum)
		}

		value = strings.TrimSpace(value)

		// Unquote double-quoted or single-quoted values.
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		env[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read env file %s: %w", path, err)
	}
	return env, nil
}
