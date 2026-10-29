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
