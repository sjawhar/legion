package main

import (
	"strings"
	"testing"
)

func TestResolveBootConfigRejectsUntrustedHeaderIdentityWithOAuth(t *testing.T) {
	_, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":            "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":    "agent-token",
		"DISPATCH_IDENTITY":       "header:X-Dispatch-User",
		"DISPATCH_APP_CLIENT_ID":  "client-id",
		"DISPATCH_ALLOWED_LOGINS": "sjawhar",
	}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_IDENTITY_HEADER_TRUSTED") {
		t.Fatalf("error: got %v, want trusted header rejection", err)
	}
}

func TestResolveBootConfigRejectsCookieModeWithoutAllowlist(t *testing.T) {
	_, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":         "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN": "agent-token",
	}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_ALLOWED_LOGINS") {
		t.Fatalf("error: got %v, want missing allowlist rejection", err)
	}
}

func envGetter(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}
