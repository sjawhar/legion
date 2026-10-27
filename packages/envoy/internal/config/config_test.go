package config

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestLoadReadsSubscribeAllTopics(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value *string
		want  bool
	}{
		{"1 is on", new("1"), true},
		{"unset is off", nil, false},
		{"empty is off", new(""), false},
		{"0 is off", new("0"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENVOY_MACHINE_ID", "test-machine")
			t.Setenv("NATS_URLS", "nats://127.0.0.1:4222")
			// t.Setenv restores the variable after the test; Unsetenv then leaves it unset for this one.
			t.Setenv("ENVOY_SUBSCRIBE_ALL_TOPICS", "")
			if tc.value == nil {
				if err := os.Unsetenv("ENVOY_SUBSCRIBE_ALL_TOPICS"); err != nil {
					t.Fatalf("unset: %v", err)
				}
			} else {
				t.Setenv("ENVOY_SUBSCRIBE_ALL_TOPICS", *tc.value)
			}

			service, err := Load(9020)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if service.SubscribeAllTopics != tc.want {
				t.Fatalf("SubscribeAllTopics = %t, want %t", service.SubscribeAllTopics, tc.want)
			}
		})
	}
}

func TestLoadRefusesASubscribeAllTopicsValueOtherThan0Or1(t *testing.T) {
	for _, value := range []string{"true", " 1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ENVOY_MACHINE_ID", "test-machine")
			t.Setenv("NATS_URLS", "nats://127.0.0.1:4222")
			t.Setenv("ENVOY_SUBSCRIBE_ALL_TOPICS", value)

			_, err := Load(9020)
			if err == nil || !strings.Contains(err.Error(), "ENVOY_SUBSCRIBE_ALL_TOPICS") {
				t.Fatalf("Load error = %v, want a refusal naming ENVOY_SUBSCRIBE_ALL_TOPICS", err)
			}
		})
	}
}

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

// loadWith runs Load with ENVOY_NATS_REPLICAS and ENVOY_STREAM_MAX_BYTES as values says: a
// variable values lacks is unset.
func loadWith(t *testing.T, values map[string]string) (Service, error) {
	t.Helper()
	t.Setenv("ENVOY_MACHINE_ID", "test-machine")
	t.Setenv("NATS_URLS", "nats://127.0.0.1:4222")
	for _, name := range []string{"ENVOY_NATS_REPLICAS", "ENVOY_STREAM_MAX_BYTES"} {
		// t.Setenv restores the variable after the test; Unsetenv then leaves it unset for this one.
		t.Setenv(name, "")
		if value, ok := values[name]; ok {
			t.Setenv(name, value)
		} else if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
	return Load(9020)
}

// ENVOY_NATS_REPLICAS is the deployment's replica count, 1 when unset or blank, and
// ENVOY_STREAM_MAX_BYTES the stream's byte limit, 0 (none set) when unset or blank.
func TestLoadReadsTheReplicaCountAndTheStreamByteLimit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		values   map[string]string
		replicas int
		maxBytes int64
	}{
		{"unset", nil, 1, 0},
		{"blank", map[string]string{"ENVOY_NATS_REPLICAS": " ", "ENVOY_STREAM_MAX_BYTES": ""}, 1, 0},
		{"one", map[string]string{"ENVOY_NATS_REPLICAS": "1"}, 1, 0},
		{"three with a limit", map[string]string{"ENVOY_NATS_REPLICAS": " 3 ", "ENVOY_STREAM_MAX_BYTES": "10737418240"}, 3, 10 << 30},
		{"five", map[string]string{"ENVOY_NATS_REPLICAS": "5", "ENVOY_STREAM_MAX_BYTES": "1"}, 5, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, err := loadWith(t, tc.values)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if service.NATSReplicas != tc.replicas || service.StreamMaxBytes != tc.maxBytes {
				t.Fatalf("NATSReplicas = %d and StreamMaxBytes = %d, want %d and %d", service.NATSReplicas, service.StreamMaxBytes, tc.replicas, tc.maxBytes)
			}
		})
	}
}

// A replica count outside 1-5 or a byte limit that is not a positive number is refused, naming the
// variable and the value.
func TestLoadRefusesAReplicaCountOutside1To5AndAByteLimitThatIsNotPositive(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"ENVOY_NATS_REPLICAS", "0"},
		{"ENVOY_NATS_REPLICAS", "-1"},
		{"ENVOY_NATS_REPLICAS", "6"},
		{"ENVOY_NATS_REPLICAS", "x"},
		{"ENVOY_NATS_REPLICAS", "2.5"},
		{"ENVOY_STREAM_MAX_BYTES", "0"},
		{"ENVOY_STREAM_MAX_BYTES", "-5"},
		{"ENVOY_STREAM_MAX_BYTES", "x"},
		{"ENVOY_STREAM_MAX_BYTES", "10GB"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			_, err := loadWith(t, map[string]string{tc.name: tc.value})
			if want := fmt.Sprintf("invalid %s %q", tc.name, tc.value); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Load error = %v, want a refusal naming %s and %q", err, tc.name, tc.value)
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
