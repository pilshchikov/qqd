package qqd

import "testing"

func TestRedactValue(t *testing.T) {
	got := redactValue("my-secret-token")
	want := "m*************n"
	if got != want {
		t.Fatalf("redactValue(%q) = %q, want %q", "my-secret-token", got, want)
	}
}

func TestRedactValueShort(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"a", "*"},
		{"ab", "**"},
		{"abc", "***"},
		{"abcd", "****"},
	}
	for _, tc := range cases {
		got := redactValue(tc.input)
		if got != tc.want {
			t.Fatalf("redactValue(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestRedactValueEmpty(t *testing.T) {
	got := redactValue("")
	if got != "" {
		t.Fatalf("redactValue(\"\") = %q, want empty", got)
	}
}

func TestIsSecretKey(t *testing.T) {
	secrets := []string{
		"DB_TOKEN",
		"API_SECRET",
		"USER_PASSWORD",
		"SSH_KEY",
		"OAUTH_CREDENTIAL",
		"MY_API_KEY",
		"REGISTRY_APIKEY",
		"SECRET_VALUE",
		"TOKEN_HEADER",
		"PASSWORD_HASH",
	}
	for _, key := range secrets {
		if !isSecretKey(key) {
			t.Fatalf("isSecretKey(%q) should be true", key)
		}
	}
}

func TestIsSecretKeyNonSecret(t *testing.T) {
	nonSecrets := []string{
		"PORT",
		"HOST",
		"DB_HOST",
		"LOG_LEVEL",
		"APP_NAME",
		"REPLICAS",
		"MEMORY",
		"CPU",
	}
	for _, key := range nonSecrets {
		if isSecretKey(key) {
			t.Fatalf("isSecretKey(%q) should be false", key)
		}
	}
}
