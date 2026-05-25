package qqd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// parseJSON parses JSON bytes into a map[string]any.
func parseJSON(data []byte) (map[string]any, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	// json.Unmarshal uses float64 for numbers — convert to int where possible.
	return normalizeJSONMap(raw), nil
}

// normalizeJSONMap converts float64 values to int where they are whole numbers,
// so that the rest of the config decoder (which uses asInt) works uniformly.
func normalizeJSONMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = normalizeJSONValue(v)
	}
	return out
}

func normalizeJSONValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return normalizeJSONMap(val)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = normalizeJSONValue(item)
		}
		return out
	case float64:
		if val == float64(int(val)) {
			return int(val)
		}
		return val
	default:
		return v
	}
}

// parseYAML parses a simple YAML document into a map[string]any.
//
// Supported: maps, arrays (- item), inline flow sequences ([a, b]), strings
// (quoted and unquoted), integers, booleans, null/~, comments (# and inline #),
// quoted keys, nested indentation.
//
// NOT supported (these will produce a clear error rather than silent
// misinterpretation, where detectable):
//   - anchors and aliases (&foo, *foo)
//   - multi-line scalars (|, >, |-, >+)
//   - tags (!!str, !MyType)
//   - merge keys (<<: *anchor)
//   - flow maps ({a: 1, b: 2})
//   - documents separators (--- and ...)
//
// See docs/yaml-subset.md for the full supported-syntax reference.
func parseYAML(data []byte) (map[string]any, error) {
	if err := detectUnsupportedYAML(data); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	result, _, err := parseYAMLMap(lines, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	return result, nil
}

// detectUnsupportedYAML rejects syntax outside the parser's documented subset.
func detectUnsupportedYAML(data []byte) error {
	for i, raw := range strings.Split(string(data), "\n") {
		line := stripYAMLInlineComment(raw)
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if trimmed == "---" || trimmed == "..." {
			return fmt.Errorf("line %d: multi-document YAML (--- / ...) is not supported", i+1)
		}
		if strings.HasPrefix(trimmed, "<<:") {
			return fmt.Errorf("line %d: YAML merge keys (<<:) are not supported", i+1)
		}
		// Look at the value side of `key:` lines for unsupported markers.
		if colon := strings.Index(trimmed, ":"); colon > 0 {
			rest := strings.TrimSpace(trimmed[colon+1:])
			// Strip leading quotes from rest before scanning for markers; markers
			// inside quoted strings (e.g. an env var literal "&foo") are fine.
			unquoted, isQuoted := stripOuterQuotes(rest)
			if isQuoted {
				continue
			}
			switch {
			case strings.HasPrefix(unquoted, "&"):
				return fmt.Errorf("line %d: YAML anchors (&name) are not supported; inline the value", i+1)
			case strings.HasPrefix(unquoted, "*"):
				return fmt.Errorf("line %d: YAML aliases (*name) are not supported; inline the value", i+1)
			case unquoted == "|" || unquoted == ">" ||
				strings.HasPrefix(unquoted, "|-") || strings.HasPrefix(unquoted, "|+") ||
				strings.HasPrefix(unquoted, ">-") || strings.HasPrefix(unquoted, ">+"):
				return fmt.Errorf("line %d: multi-line scalars (|, >) are not supported; use a quoted single-line string", i+1)
			case strings.HasPrefix(unquoted, "!!") || strings.HasPrefix(unquoted, "!<"):
				return fmt.Errorf("line %d: YAML tags (!!str, !<...>) are not supported", i+1)
			case strings.HasPrefix(unquoted, "{") && strings.HasSuffix(unquoted, "}"):
				return fmt.Errorf("line %d: YAML flow maps ({a: 1, b: 2}) are not supported; expand to indented map", i+1)
			}
		}
	}
	return nil
}

// stripOuterQuotes returns the unquoted content if s is fully wrapped in single
// or double quotes, plus a flag indicating whether quotes were present.
func stripOuterQuotes(s string) (string, bool) {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1], true
		}
	}
	return s, false
}

// parseYAMLMap parses lines starting at idx with the given indentation level.
// Returns the parsed map and the next line index to process.
func parseYAMLMap(lines []string, idx, indent int) (map[string]any, int, error) {
	result := map[string]any{}
	for idx < len(lines) {
		line := lines[idx]
		stripped := strings.TrimRight(line, " \t\r")

		// Skip blank lines and comments
		trimmed := strings.TrimSpace(stripped)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			idx++
			continue
		}

		// Calculate indentation
		lineIndent := len(stripped) - len(strings.TrimLeft(stripped, " "))
		if lineIndent < indent {
			return result, idx, nil // dedent — return to parent
		}
		if lineIndent > indent && len(result) == 0 {
			indent = lineIndent // first line sets actual indent
		}
		if lineIndent != indent {
			return result, idx, nil // different indent level — return to parent
		}

		// Must be a key: value line
		colonIdx := strings.Index(trimmed, ":")
		if colonIdx < 0 {
			return nil, idx, fmt.Errorf("line %d: expected 'key:' got %q", idx+1, trimmed)
		}

		key := strings.TrimSpace(trimmed[:colonIdx])
		// Strip quotes from keys (YAML allows "key": value)
		if len(key) >= 2 && ((key[0] == '"' && key[len(key)-1] == '"') || (key[0] == '\'' && key[len(key)-1] == '\'')) {
			key = key[1 : len(key)-1]
		}
		rest := strings.TrimSpace(trimmed[colonIdx+1:])

		// Strip inline comments from rest (but not inside quotes)
		rest = stripYAMLInlineComment(rest)

		if rest == "" {
			// Value is a nested map or array — look at next lines
			idx++
			if idx < len(lines) {
				nextTrimmed := strings.TrimSpace(lines[idx])
				if strings.HasPrefix(nextTrimmed, "- ") || nextTrimmed == "-" {
					// Array
					arr, nextIdx, err := parseYAMLArray(lines, idx, lineIndent+2)
					if err != nil {
						return nil, idx, err
					}
					result[key] = arr
					idx = nextIdx
				} else {
					// Nested map
					child, nextIdx, err := parseYAMLMap(lines, idx, lineIndent+2)
					if err != nil {
						return nil, idx, err
					}
					result[key] = child
					idx = nextIdx
				}
			}
		} else {
			result[key] = parseYAMLScalar(rest)
			idx++
		}
	}
	return result, idx, nil
}

// parseYAMLArray parses a YAML array (lines starting with "- ").
func parseYAMLArray(lines []string, idx, indent int) ([]any, int, error) {
	var result []any
	for idx < len(lines) {
		stripped := strings.TrimRight(lines[idx], " \t\r")
		trimmed := strings.TrimSpace(stripped)

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			idx++
			continue
		}

		lineIndent := len(stripped) - len(strings.TrimLeft(stripped, " "))
		if lineIndent < indent-2 {
			return result, idx, nil // dedent
		}

		if !strings.HasPrefix(trimmed, "- ") && trimmed != "-" {
			return result, idx, nil // not an array item
		}

		item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
		item = stripYAMLInlineComment(item)
		result = append(result, parseYAMLScalar(item))
		idx++
	}
	return result, idx, nil
}

// parseYAMLScalar converts a YAML scalar string to a Go value.
func parseYAMLScalar(s string) any {
	if s == "" {
		return ""
	}
	// Flow sequence: ["a", "b", "c"]
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		return parseYAMLFlowSequence(s)
	}
	// Quoted strings
	if (strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"")) ||
		(strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'")) {
		return s[1 : len(s)-1]
	}
	// Booleans
	switch strings.ToLower(s) {
	case "true", "yes":
		return true
	case "false", "no":
		return false
	case "null", "~":
		return nil
	}
	// Integers
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return s
}

// parseYAMLFlowSequence parses an inline YAML array like ["a", "b", "c"].
func parseYAMLFlowSequence(s string) []any {
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []any{}
	}
	var result []any
	for _, item := range splitFlowItems(inner) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		result = append(result, parseYAMLScalar(item))
	}
	return result
}

// splitFlowItems splits comma-separated items respecting quotes.
func splitFlowItems(s string) []string {
	var items []string
	var current strings.Builder
	inSingle, inDouble := false, false
	for _, c := range s {
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			current.WriteRune(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			current.WriteRune(c)
		case c == ',' && !inSingle && !inDouble:
			items = append(items, current.String())
			current.Reset()
		default:
			current.WriteRune(c)
		}
	}
	if current.Len() > 0 {
		items = append(items, current.String())
	}
	return items
}

// stripYAMLInlineComment removes trailing # comments from a YAML value,
// but only if the # is not inside quotes.
func stripYAMLInlineComment(s string) string {
	inSingle := false
	inDouble := false
	for i, c := range s {
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return strings.TrimSpace(s[:i])
			}
		}
	}
	return s
}
