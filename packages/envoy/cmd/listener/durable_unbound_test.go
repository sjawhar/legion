package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// /healthz answers 503 unhealthy once the listener's durable consumer has been recreated
// server-side, because a recreated durable delivers to a fresh inbox while this listener's
// subscription still holds the old one: the listener receives nothing at all. Without this the
// durable still exists, every health surface answers 200 healthy, and delivery to every session
// stops with no signal anywhere that the listener has gone deaf.
func TestHealthzReportsARecreatedDurableUnhealthy(t *testing.T) {
	consumer := "listener-unbound-health"
	client, get := setupUnboundDurableFixture(t, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
	if code, body := get(); code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("before the durable is recreated: /healthz = %d %v, want 200 healthy", code, body)
	}

	recreateListenerDurable(t, client, consumer)

	code, body := get()
	want := "durable consumer unavailable: not bound to this listener's subscription"
	if code != http.StatusServiceUnavailable || body["status"] != "unhealthy" || fmt.Sprint(body["error"]) != want {
		t.Fatalf("with the durable recreated: /healthz = %d %v, want 503 unhealthy with error %q", code, body, want)
	}
}

// setupUnboundDurableFixture is the listener a recreated durable is tested against: handler bound
// to the durable, both registries open with their caches ready, and the listenerDeps /healthz
// reads. It returns the client and a get() that serves one /healthz request, which is every piece
// the tests in this file share.
func setupUnboundDurableFixture(t *testing.T, consumer string, handler natsgo.MsgHandler) (*bus.Client, func() (int, map[string]any)) {
	t.Helper()
	client := setupTestNATS(t)
	sub, err := startListenerSubscription(client, consumer, handler)
	if err != nil {
		t.Fatalf("bind the durable: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open interest registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readyCancel()
	if err := registry.WaitForCacheReady(readyCtx); err != nil {
		t.Fatalf("wait for interest cache: %v", err)
	}
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	if err := sessions.WaitForCacheReady(readyCtx); err != nil {
		t.Fatalf("wait for session cache: %v", err)
	}
	state := &atomic.Pointer[listenerDeps]{}
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions, caches: []listenerCache{{name: "interest", cache: registry}, {name: "session", cache: sessions}}, consumer: consumer})
	get := func() (int, map[string]any) {
		recorder := httptest.NewRecorder()
		healthzHandler(state).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var body map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		return recorder.Code, body
	}
	return client, get
}

// startUnboundDurableMonitor runs the self-health monitor over client's durable the way the
// listener does, with the probe and the rebuild both reading consumer. It returns the flag the
// monitor's terminate callback sets and a stop function that ends the monitor and waits for it,
// so each test keeps only its own logger, handler and assertions.
func startUnboundDurableMonitor(t *testing.T, client *bus.Client, consumer string, logger *logging.Logger, handler natsgo.MsgHandler) (*atomic.Bool, func()) {
	t.Helper()
	caches := []listenerCache{}
	ctx, cancel := context.WithCancel(context.Background())
	probe := func() error { return durableProbe(ctx, client, consumer) }
	terminated := &atomic.Bool{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSelfHealthMonitor(
			ctx,
			logger,
			func() error { return checkSelfHealth(caches, probe) },
			func(err error) bool { return isUnrecoverableSelfHealthFailure(err, client, caches) },
			client.Connected,
			func() error { return rebuildListenerDependencies(ctx, client, caches, probe, consumer, handler) },
			func() { terminated.Store(true) },
			50*time.Millisecond,
			3,
		)
	}()
	stop := func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return terminated, stop
}

// recreateListenerDurable is what `nats consumer rm` then `nats consumer add` does: the same
// durable under the same name and config, delivering to a fresh inbox the listener's live
// subscription knows nothing about.
func recreateListenerDurable(t *testing.T, client *bus.Client, consumer string) {
	t.Helper()
	info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	config := info.Config
	if err := client.JS().DeleteConsumer(bus.Stream, consumer); err != nil {
		t.Fatalf("delete the durable: %v", err)
	}
	config.DeliverSubject = natsgo.NewInbox()
	if _, err := client.JS().AddConsumer(bus.Stream, &config); err != nil {
		t.Fatalf("recreate the durable: %v", err)
	}
}

// The self-health monitor repairs a durable recreated under the listener: without the rebind, the
// subscription stays attached to the deleted consumer's inbox and every message published after
// the recreation is lost while the listener reports itself healthy.
func TestTheSelfHealthMonitorRebindsARecreatedDurable(t *testing.T) {
	consumer := "listener-unbound-rebind"
	var delivered atomic.Int64
	handler := func(msg *natsgo.Msg) {
		delivered.Add(1)
		_ = msg.Ack()
	}
	client, _ := setupUnboundDurableFixture(t, consumer, handler)

	recreateListenerDurable(t, client, consumer)
	if _, err := client.JS().Publish("notifications.github.acme.widgets.push.branch.main", []byte(`{}`)); err != nil {
		t.Fatalf("publish after the recreation: %v", err)
	}

	terminated, stop := startUnboundDurableMonitor(t, client, consumer, logging.New("test"), handler)

	// The probe's confirming read alone costs 250ms, so the repair takes several intervals.
	deadline := time.Now().Add(10 * time.Second)
	for delivered.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the message published after the recreation was never delivered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if terminated.Load() {
		t.Fatal("the monitor terminated the listener instead of rebinding its durable")
	}
	info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil {
		t.Fatalf("consumer info after the rebind: %v", err)
	}
	if !info.PushBound || info.Config.DeliverSubject != client.SubSubject() {
		t.Fatalf("after the rebind the durable delivers to %q (push bound %v), want this listener's subscription subject %q", info.Config.DeliverSubject, info.PushBound, client.SubSubject())
	}

	stop()
}

// A durable that is bound never fails its own probe: the unbound check compares the consumer's
// deliver subject against the live subscription's, so a mistake there would make every healthy
// listener rebuild itself every interval and eventually terminate.
func TestABoundDurableNeverFailsItsProbe(t *testing.T) {
	consumer := "listener-unbound-control"
	client, get := setupUnboundDurableFixture(t, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })

	// The monitor logs from its own goroutine, so the buffer the test reads back is guarded.
	logs := &lockedBuffer{}
	terminated, stop := startUnboundDurableMonitor(t, client, consumer, logging.NewWithWriter("test", logs), nil)

	deadline := time.Now().Add(15 * 50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if code, body := get(); code != http.StatusOK || body["status"] != "healthy" {
			t.Fatalf("with nothing wrong: /healthz = %d %v, want 200 healthy", code, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()

	captured := logs.String()
	if strings.Contains(captured, "self-health probe failed") {
		t.Fatalf("a bound durable failed its probe; the monitor logged: %s", captured)
	}
	if terminated.Load() {
		t.Fatal("the monitor terminated a listener whose durable was bound throughout")
	}
}
