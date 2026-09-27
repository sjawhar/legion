package bus_test

import (
	"errors"
	"net"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A deployment of the stream still creates it and reconciles what it finds, whichever NATS its
// deployment names.
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

// An agent's ~/.config/opencode/envoy.json names production's bus, so a tool run from a checkout
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

// remoteLookingURL reaches the test's own NATS by an address of this machine that is not
// loopback, which is what a NATS on another machine looks like to the refusal. No test reaches a
// NATS it did not start.
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
		return "nats://" + net.JoinHostPort(network.IP.String(), parsed.Port())
	}
	t.Skip("no non-loopback IPv4 address on this machine to reach the test server by")
	return ""
}
