package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
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

func TestResolveBootConfigDisablesNATS(t *testing.T) {
	boot, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":            "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":    "agent-token",
		"DISPATCH_IDENTITY":       "header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS": "sjawhar",
		"DISPATCH_NATS_DISABLED":  "1",
	}))
	if err != nil {
		t.Fatalf("resolve boot config: %v", err)
	}
	if !boot.NATSDisabled {
		t.Fatal("DISPATCH_NATS_DISABLED=1 did not disable NATS")
	}
}

func TestDispatchHandlerReportsDisabledNATS(t *testing.T) {
	handler := dispatchHandler(http.NewServeMux(), nil, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if nats, found := health["nats"]; !found || nats != nil {
		t.Fatalf("healthz nats = %#v, want null", nats)
	}
}

func TestDispatchHandlerReportsDisconnectedNATS(t *testing.T) {
	handler := dispatchHandler(http.NewServeMux(), nil, &bus.Client{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if nats, ok := health["nats"].(bool); !ok || nats {
		t.Fatalf("healthz nats = %#v, want false", nats)
	}
}

func envGetter(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}
