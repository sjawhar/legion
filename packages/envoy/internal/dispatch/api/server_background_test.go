package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The background wait holds for the work a request left running past its client (detached): a
// broadcast's remaining deliveries, a keyed write's handler. Production shutdown calls it before
// it closes the database pool, so a wait that skipped them would close the pool under a worker
// still writing.
func TestTheBackgroundWaitHoldsForWorkARequestLeftRunning(t *testing.T) {
	served := register(http.NewServeMux(), t.Context(), Deps{})
	started, release := make(chan struct{}), make(chan struct{})
	served.detached.goDetached(func() {
		close(started)
		<-release
	})
	<-started

	waited := make(chan struct{})
	go func() {
		defer close(waited)
		served.waitBackground()
	}()
	select {
	case <-waited:
		t.Fatal("the background wait returned while a detached worker was still running")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("the background wait never returned once the detached worker ended")
	}
}

// The background wait holds for every live socket's handler, and so every subscription it ran,
// until the socket ends: production shutdown waits for it before it closes the database pool,
// which a subscription still reading would otherwise lose under it.
func TestTheBackgroundWaitHoldsForAnOpenLiveSocket(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	client := rig.dial(t, asUser("alice"))
	rig.sockets(t, 1)

	waited := make(chan struct{})
	go func() {
		defer close(waited)
		rig.served.waitBackground()
	}()
	select {
	case <-waited:
		t.Fatal("the background wait returned while a live socket's handler was still running")
	case <-time.After(200 * time.Millisecond):
	}
	_ = client.conn.Close()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("the background wait never returned once the live socket closed")
	}
}

// The background wait holds for the event stream relay, which an events subscription starts and
// which runs on the server's lifetime after its last subscriber leaves, until that lifetime ends.
func TestTheBackgroundWaitHoldsForTheEventStreamRelay(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	client := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, client, "events", nil)
	_ = client.conn.Close()
	rig.sockets(t, 0)
	requireBackgroundWaitHoldsUntilTheLifetimeEnds(t, rig, "the event stream relay")
}

// The background wait holds for the delivery hub, which a delivery view starts and which runs on
// the server's lifetime after its last view closes, until that lifetime ends.
func TestTheBackgroundWaitHoldsForTheDeliveryHub(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	seedHubSettings(t, rig.database)
	client := rig.dial(t, asUser("alice"))
	client.send(t, deliverySubscribe("p", presetView(deliveryLiveTimeline, 7, "")))
	client.snapshot(t, 10*time.Second, "p")
	_ = client.conn.Close()
	rig.sockets(t, 0)
	requireBackgroundWaitHoldsUntilTheLifetimeEnds(t, rig, "the delivery hub")
}

// requireBackgroundWaitHoldsUntilTheLifetimeEnds requires rig's background wait, with no socket
// open and nothing detached running, to hold while the server's lifetime lasts, for what (a
// worker that lifetime runs), and to return once the lifetime ends.
func requireBackgroundWaitHoldsUntilTheLifetimeEnds(t *testing.T, rig *liveSocketRig, what string) {
	t.Helper()
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		rig.served.waitBackground()
	}()
	select {
	case <-waited:
		t.Fatalf("the background wait returned while %s still ran on the server's lifetime", what)
	case <-time.After(200 * time.Millisecond):
	}
	rig.stop()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatalf("the background wait never returned once the server's lifetime ended (%s)", what)
	}
}

// A server with no lifetime is refused where it is built, not left running the hub, the relay and
// every live socket on a context that never ends: each would outlive the process's shutdown, or
// the test that built it, and the background wait would never return once a subscription had
// started the relay.
func TestRegisterRefusesANilLifetime(t *testing.T) {
	defer func() {
		refusal, ok := recover().(string)
		if !ok || !strings.Contains(refusal, "the lifetime parameter is required") {
			t.Fatalf("api.Register with no lifetime panicked with %v, want the refusal naming the parameter", refusal)
		}
	}()
	//nolint:staticcheck // the nil lifetime is what this test passes.
	Register(http.NewServeMux(), nil, Deps{})
	t.Fatal("api.Register with no lifetime returned, want a panic")
}
