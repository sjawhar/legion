package config

import (
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// listeningDescriptor opens a listening loopback socket and returns its descriptor number and
// address, for ENVOY_LISTEN_FD to name in this process.
func listeningDescriptor(t *testing.T) (string, *net.TCPAddr) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	file, err := listener.(*net.TCPListener).File()
	_ = listener.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return strconv.Itoa(int(file.Fd())), listener.Addr().(*net.TCPAddr)
}

// With ENVOY_LISTEN_FD and neither ENVOY_LISTEN_HOST nor PORT given (empty is not given), the
// listener's address is the socket's own, not 127.0.0.1 on the default port.
func TestLoadTakesTheInheritedSocketsAddress(t *testing.T) {
	descriptor, addr := listeningDescriptor(t)
	t.Setenv("ENVOY_MACHINE_ID", "test-machine")
	t.Setenv("NATS_URLS", "nats://127.0.0.1:4222")
	t.Setenv("ENVOY_LISTEN_HOST", "")
	t.Setenv("PORT", "")
	t.Setenv("ENVOY_LISTEN_FD", descriptor)

	service, err := Load(9020)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if service.ListenSocket == nil || service.ListenAddress() != addr.String() {
		t.Fatalf("Load gave socket %v at %s, want the inherited socket at %s", service.ListenSocket, service.ListenAddress(), addr)
	}
}

// ENVOY_LISTEN_HOST and PORT, when given beside ENVOY_LISTEN_FD, must name the socket's address.
func TestLoadRefusesASettingThatIsNotTheInheritedSocketsAddress(t *testing.T) {
	descriptor, addr := listeningDescriptor(t)
	other := strconv.Itoa(addr.Port%65535 + 1)
	for _, test := range []struct{ host, port, refused string }{
		{host: "127.0.0.1", port: strconv.Itoa(addr.Port), refused: ""},
		{host: "0.0.0.0", port: strconv.Itoa(addr.Port), refused: "ENVOY_LISTEN_HOST=0.0.0.0"},
		{host: "127.0.0.1", port: "0", refused: "PORT=0"},
		{host: "127.0.0.1", port: other, refused: "PORT=" + other},
	} {
		t.Run(test.host+" "+test.port, func(t *testing.T) {
			t.Setenv("ENVOY_MACHINE_ID", "test-machine")
			t.Setenv("NATS_URLS", "nats://127.0.0.1:4222")
			t.Setenv("ENVOY_LISTEN_HOST", test.host)
			t.Setenv("PORT", test.port)
			t.Setenv("ENVOY_LISTEN_FD", descriptor)

			_, err := Load(9020)
			if test.refused == "" {
				if err != nil {
					t.Fatalf("Load refused settings that name the socket: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.refused) || !strings.Contains(err.Error(), "ENVOY_LISTEN_FD="+descriptor) {
				t.Fatalf("Load error = %v, want a refusal naming %s and ENVOY_LISTEN_FD=%s", err, test.refused, descriptor)
			}
		})
	}
}

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
