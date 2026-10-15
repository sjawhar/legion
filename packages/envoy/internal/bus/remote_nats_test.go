package bus_test

import (
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testsocks5"
)

// A deployment of the stream still creates it and reconciles what it finds on the NATS it names.
func TestConnectOwningStreamStillReconcilesTheDeployedStream(t *testing.T) {
	uri := testnats.URL(t)
	js := deployStream(t, uri)

	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect owning the stream: %v", err)
	}
	t.Cleanup(client.Close)

	after := streamConfig(t, js)
	for _, subject := range append(slices.Clone(deployedSubjects), bus.StreamSubjects()...) {
		if !slices.Contains(after.Subjects, subject) {
			t.Fatalf("stream subjects = %v, missing %q", after.Subjects, subject)
		}
	}
}

// Owning the stream is no exemption from saying which machine's NATS this is. Nothing separates
// the deployed Dispatch from the same binary run out of a checkout - both read envoy.natsUrls from
// the user envoy.json (${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json), which on an agent machine
// names production - so a bare envoy-dispatch would otherwise reconcile production's stream
// before its own database check failed. Each deployment sets the variable instead.
func TestConnectOwningStreamRefusesANATSServerThatIsNotThisMachines(t *testing.T) {
	uri := testnats.URL(t)
	js := deployStream(t, uri)
	before := streamConfig(t, js)
	remote := remoteLookingURL(t, uri)

	_, err := bus.ConnectOwningStream([]string{remote})
	if err == nil {
		t.Fatalf("connect owning the stream on %s succeeded, want a refusal", remote)
	}
	if !errors.Is(err, bus.ErrRemoteNATS) {
		t.Fatalf("connect owning the stream on %s = %v, want a %v", remote, err, bus.ErrRemoteNATS)
	}
	if !strings.Contains(err.Error(), remote) || !strings.Contains(err.Error(), bus.AllowRemoteEnvVar) {
		t.Fatalf("refusal %q does not name both the server %s and the override %s", err, remote, bus.AllowRemoteEnvVar)
	}
	if after := streamConfig(t, js); !slices.Equal(after.Subjects, before.Subjects) {
		t.Fatalf("stream subjects = %v, want the deployed %v", after.Subjects, before.Subjects)
	}

	// The deployment says so, and the start reconciles that server as it always did.
	t.Setenv(bus.AllowRemoteEnvVar, "1")
	client, err := bus.ConnectOwningStream([]string{remote})
	if err != nil {
		t.Fatalf("connect owning the stream on %s with %s=1: %v", remote, bus.AllowRemoteEnvVar, err)
	}
	t.Cleanup(client.Close)
	after := streamConfig(t, js)
	for _, subject := range append(slices.Clone(deployedSubjects), bus.StreamSubjects()...) {
		if !slices.Contains(after.Subjects, subject) {
			t.Fatalf("stream subjects = %v, missing %q", after.Subjects, subject)
		}
	}
}

// A core connection of a caller's own (Dial) reaches no further than the rest: it is the same
// process on the same bus, and a viewer's relay dialling production by accident is the thing
// being stopped.
func TestDialRefusesANATSServerThatIsNotThisMachines(t *testing.T) {
	uri := testnats.URL(t)
	remote := remoteLookingURL(t, uri)

	_, err := bus.Dial("relay", []string{remote}, os.LookupEnv)
	if !errors.Is(err, bus.ErrRemoteNATS) {
		t.Fatalf("dial %s = %v, want a %v", remote, err, bus.ErrRemoteNATS)
	}
	if !strings.Contains(err.Error(), remote) || !strings.Contains(err.Error(), bus.AllowRemoteEnvVar) {
		t.Fatalf("refusal %q does not name both the server %s and the override %s", err, remote, bus.AllowRemoteEnvVar)
	}

	conn, err := bus.Dial("relay", []string{uri}, os.LookupEnv)
	if err != nil {
		t.Fatalf("dial this machine's NATS: %v", err)
	}
	t.Cleanup(conn.Close)
	if !conn.IsConnected() {
		t.Fatal("the dialled connection is not connected")
	}
}

// An agent's user envoy.json names production's bus, so a tool run from a checkout
// reaches production unless something stops it. The refusal names the server and the override,
// and it is read off the URL: nothing dials while the run has not said it means another machine's
// NATS.
func TestConnectRefusesANATSServerThatIsNotThisMachines(t *testing.T) {
	uri := testnats.URL(t)
	remote := remoteLookingURL(t, uri)

	_, err := bus.Connect([]string{remote})
	if err == nil {
		t.Fatalf("connect to %s succeeded, want a refusal", remote)
	}
	if !errors.Is(err, bus.ErrRemoteNATS) {
		t.Fatalf("connect to %s = %v, want a %v", remote, err, bus.ErrRemoteNATS)
	}
	if !strings.Contains(err.Error(), remote) {
		t.Fatalf("refusal %q does not name the server %s", err, remote)
	}
	if !strings.Contains(err.Error(), bus.AllowRemoteEnvVar) {
		t.Fatalf("refusal %q does not name the override %s", err, bus.AllowRemoteEnvVar)
	}
}

// Reaching another machine's NATS is a run saying so, and is still no reason to write the stream
// there.
func TestConnectReachesANonLocalNATSWhenTheRunSaysSo(t *testing.T) {
	uri := testnats.URL(t)
	js := deployStream(t, uri)
	before := streamConfig(t, js)
	t.Setenv(bus.AllowRemoteEnvVar, "1")

	remote := remoteLookingURL(t, uri)
	client, err := bus.Connect([]string{remote})
	if err != nil {
		t.Fatalf("connect to %s with %s=1: %v", remote, bus.AllowRemoteEnvVar, err)
	}
	t.Cleanup(client.Close)

	after := streamConfig(t, js)
	if !slices.Equal(after.Subjects, before.Subjects) {
		t.Fatalf("stream subjects = %v, want the deployed %v", after.Subjects, before.Subjects)
	}
}

// A connect handed an environment (WithEnvironment, or Dial's environment: envoy-dispatch hands its
// settings table) reads the reach and the credential there and nowhere else, for Connect,
// ConnectOwningStream and Dial alike. Every case fails before it dials.
func TestAHandedEnvironmentReplacesTheProcessEnvironment(t *testing.T) {
	const remote = "nats://nats.example:4222"
	t.Setenv(bus.AllowRemoteEnvVar, "1")
	t.Setenv("NATS_NKEY_SEED_FILE", filepath.Join(t.TempDir(), "the process's seed"))
	handed := func(values map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) {
			value, set := values[key]
			return value, set
		}
	}
	connects := map[string]func(func(string) (string, bool)) error{
		"Connect": func(environment func(string) (string, bool)) error {
			_, err := bus.Connect([]string{remote}, bus.WithEnvironment(environment))
			return err
		},
		"ConnectOwningStream": func(environment func(string) (string, bool)) error {
			_, err := bus.ConnectOwningStream([]string{remote}, bus.WithEnvironment(environment))
			return err
		},
		"Dial": func(environment func(string) (string, bool)) error {
			_, err := bus.Dial("handed", []string{remote}, environment)
			return err
		},
	}
	for name, connect := range connects {
		if err := connect(handed(nil)); !errors.Is(err, bus.ErrRemoteNATS) {
			t.Errorf("%s with no opt-in handed: %v, want a refusal whatever the process says", name, err)
		}
		err := connect(handed(map[string]string{bus.AllowRemoteEnvVar: "1", "NATS_NKEY_SEED": "the handed seed"}))
		if err == nil || !strings.Contains(err.Error(), "NATS_NKEY_SEED does not hold a valid nkey seed") {
			t.Errorf("%s with the opt-in and a seed handed: %v, want the handed seed refused", name, err)
		}
	}
}

// A proxy chooses the server a connection reaches, so with one set a loopback URL no longer names
// this machine's NATS: Connect, ConnectOwningStream and Dial refuse it unless the run says it means
// another machine's, naming both variables, and nothing dials while they refuse. Said, the same
// connect goes through the proxy.
func TestASetProxyIsRemoteReach(t *testing.T) {
	uri := testnats.URL(t)
	if !bus.LocalNATSURL(uri) {
		t.Fatalf("the test server's URL %s is not a loopback URL", uri)
	}
	fake := testsocks5.Start(t, hostOf(t, uri))
	connects := map[string]func(func(string) (string, bool)) error{
		"Connect": func(environment func(string) (string, bool)) error {
			client, err := bus.Connect([]string{uri}, bus.WithEnvironment(environment))
			if err == nil {
				client.Close()
			}
			return err
		},
		"ConnectOwningStream": func(environment func(string) (string, bool)) error {
			client, err := bus.ConnectOwningStream([]string{uri}, bus.WithEnvironment(environment))
			if err == nil {
				client.Close()
			}
			return err
		},
		"Dial": func(environment func(string) (string, bool)) error {
			conn, err := bus.Dial("proxied", []string{uri}, environment)
			if err == nil {
				conn.Close()
			}
			return err
		},
	}
	for name, connect := range connects {
		err := connect(lookupOf(map[string]string{bus.SOCKS5ProxyEnvVar: fake.Addr}))
		if !errors.Is(err, bus.ErrRemoteNATS) {
			t.Fatalf("%s to %s through a proxy, no opt-in: %v, want a %v", name, uri, err, bus.ErrRemoteNATS)
		}
		if !strings.Contains(err.Error(), bus.SOCKS5ProxyEnvVar) || !strings.Contains(err.Error(), bus.AllowRemoteEnvVar) {
			t.Fatalf("%s refusal %q does not name both %s and %s", name, err, bus.SOCKS5ProxyEnvVar, bus.AllowRemoteEnvVar)
		}
	}
	if accepted := fake.Accepted(); accepted != 0 {
		t.Fatalf("the proxy accepted %d connections while every connect was refused, want none", accepted)
	}

	for name, connect := range connects {
		if err := connect(proxied(fake.Addr)); err != nil {
			t.Fatalf("%s to %s through a proxy with %s=1: %v", name, uri, bus.AllowRemoteEnvVar, err)
		}
	}
	if got := len(fake.Requests()); got != len(connects) {
		t.Fatalf("the proxy received %d requests, want one for each of the %d connects", got, len(connects))
	}
	// The count the refusals above read as zero counts these connects, so a refusal that dialled the
	// proxy first would have shown there.
	if accepted := fake.Accepted(); accepted < int64(len(connects)) {
		t.Fatalf("the proxy accepted %d connections after %d connects through it, want at least %d", accepted, len(connects), len(connects))
	}
}

// remoteLookingURL reaches the test's own NATS, as the same user, by an address of this machine
// that is not loopback, which is what a NATS on another machine looks like to the refusal. No test
// reaches a NATS it did not start.
func remoteLookingURL(t *testing.T, uri string) string {
	t.Helper()
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("read the test server's URL %q: %v", uri, err)
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("list this machine's addresses: %v", err)
	}
	for _, address := range addresses {
		network, ok := address.(*net.IPNet)
		if !ok || network.IP.IsLoopback() || network.IP.To4() == nil {
			continue
		}
		parsed.Host = net.JoinHostPort(network.IP.String(), parsed.Port())
		return parsed.String()
	}
	t.Skip("no non-loopback IPv4 address on this machine to reach the test server by")
	return ""
}
