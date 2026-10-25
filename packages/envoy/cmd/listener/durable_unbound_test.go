package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// /healthz answers 503 unhealthy once the listener's durable consumer has been recreated
// server-side, because a recreated durable delivers to a fresh inbox while this listener's
// subscription still holds the old one: the listener receives nothing at all. Without this the
// durable still exists, every health surface answers 200 healthy, and delivery to every session
// stops with no signal anywhere that the listener has gone deaf.
func TestHealthzReportsARecreatedDurableUnhealthy(t *testing.T) {
	client := setupTestNATS(t)
	sub, err := startListenerSubscription(client, "listener-unbound-health", func(msg *natsgo.Msg) { _ = msg.Ack() })
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
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions, caches: []listenerCache{{name: "interest", cache: registry}, {name: "session", cache: sessions}}, consumer: "listener-unbound-health"})
	get := func() (int, map[string]any) {
		recorder := httptest.NewRecorder()
		healthzHandler(&state).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var body map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		return recorder.Code, body
	}
	if code, body := get(); code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("before the durable is recreated: /healthz = %d %v, want 200 healthy", code, body)
	}

	// What `nats consumer rm` then `nats consumer add` does: the same durable, a fresh inbox.
	info, err := client.JS().ConsumerInfo(bus.Stream, "listener-unbound-health")
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	config := info.Config
	if err := client.JS().DeleteConsumer(bus.Stream, "listener-unbound-health"); err != nil {
		t.Fatalf("delete the durable: %v", err)
	}
	config.DeliverSubject = natsgo.NewInbox()
	if _, err := client.JS().AddConsumer(bus.Stream, &config); err != nil {
		t.Fatalf("recreate the durable: %v", err)
	}

	code, body := get()
	want := "durable consumer unavailable: not bound to this listener's subscription"
	if code != http.StatusServiceUnavailable || body["status"] != "unhealthy" || fmt.Sprint(body["error"]) != want {
		t.Fatalf("with the durable recreated: /healthz = %d %v, want 503 unhealthy with error %q", code, body, want)
	}
}
