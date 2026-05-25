package qqd

import "strings"

// redactValue masks a secret value for display, showing only first and last char.
// e.g., "my-secret-token" -> "m*************n"
// Short values (<=4 chars) are fully masked: "****"
// Empty string returns empty string.
func redactValue(s string) string {
	if len(s) == 0 {
		return ""
	}
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return string(s[0]) + strings.Repeat("*", len(s)-2) + string(s[len(s)-1])
}

// isSecretKey returns true if a key name likely contains a secret.
// Matches common patterns like *_TOKEN, *_SECRET, *_PASSWORD, *_KEY, *_API_KEY, etc.
func isSecretKey(key string) bool {
	upper := strings.ToUpper(key)
	secretSuffixes := []string{"_TOKEN", "_SECRET", "_PASSWORD", "_KEY", "_CREDENTIAL", "_API_KEY", "_APIKEY"}
	for _, suffix := range secretSuffixes {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	secretPrefixes := []string{"SECRET_", "TOKEN_", "PASSWORD_"}
	for _, prefix := range secretPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}
