package config

import (
	"slices"
	"strings"
	"testing"
)

func TestLoadReadsAllowedHosts(t *testing.T) {
	t.Setenv("ENVOY_MACHINE_ID", "test-machine")
	t.Setenv("NATS_URLS", "nats://127.0.0.1:4222")
	t.Setenv("ENVOY_ALLOWED_HOSTS", " Hub.Internal.Example. , ,192.0.2.7,[2001:db8::1]")

	service, err := Load(9020)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"hub.internal.example", "192.0.2.7", "2001:db8::1"}
	if !slices.Equal(service.AllowedHosts, want) {
		t.Fatalf("AllowedHosts = %q, want %q", service.AllowedHosts, want)
	}
}

func TestLoadLeavesAllowedHostsEmptyWhenUnset(t *testing.T) {
	t.Setenv("ENVOY_MACHINE_ID", "test-machine")
	t.Setenv("NATS_URLS", "nats://127.0.0.1:4222")
	t.Setenv("ENVOY_ALLOWED_HOSTS", "")

	service, err := Load(9020)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(service.AllowedHosts) != 0 {
		t.Fatalf("AllowedHosts = %q, want none", service.AllowedHosts)
	}
}

func TestLoadRefusesAllowedHostsWithAPortSchemeOrPath(t *testing.T) {
	for _, entry := range []string{"hub.internal.example:9020", "http://hub.internal.example", "hub.internal.example/v1", "[2001:db8::1]:9020", ".", "hub.internal.example]", "::1]", "-hub.internal.example", "hub_name", "*", "192.0.2.10:9020", "192.0.2.10:", "cafe:80", "dead.beef:443", "1:2", "[192.0.2.10]", "[cafe]", "[abc.def]", "cafe:9020", "a:b"} {
		t.Run(entry, func(t *testing.T) {
			t.Setenv("ENVOY_MACHINE_ID", "test-machine")
			t.Setenv("NATS_URLS", "nats://127.0.0.1:4222")
			t.Setenv("ENVOY_ALLOWED_HOSTS", "ok.internal.example,"+entry)

			_, err := Load(9020)
			if err == nil || !strings.Contains(err.Error(), "ENVOY_ALLOWED_HOSTS") || !strings.Contains(err.Error(), entry) {
				t.Fatalf("Load error = %v, want a refusal naming ENVOY_ALLOWED_HOSTS and %q", err, entry)
			}
		})
	}
}

func TestHostName(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:9020":            "127.0.0.1",
		"[::1]:9020":                "::1",
		"[::1]":                     "::1",
		"LocalHost.":                "localhost",
		"Hub.Internal.Example":      "hub.internal.example",
		"hub.\u0130nternal.example": "hub.\u0130nternal.example",
		"":                          "",
	}
	for in, want := range cases {
		if got := HostName(in); got != want {
			t.Errorf("HostName(%q) = %q, want %q", in, got, want)
		}
	}
}
