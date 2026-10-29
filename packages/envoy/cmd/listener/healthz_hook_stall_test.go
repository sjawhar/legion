package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// hookStallClock is the clock the test's bus client reads for the time-based parts of its
// reconnect reporting (bus.WithReconnectErrClock), so the test sees bus.RepairGrace pass without
// waiting a minute for it. The bus reads it from its own goroutines while the test advances it.
type hookStallClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *hookStallClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *hookStallClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// errHookStallRewatch is what the test's reconnect hook always fails with, written as the listener's
// own rewatch failure reads, so the answer is checked against the text an operator would see.
var errHookStallRewatch = errors.New("rewatch interest cache: watch role claims KV bucket: context deadline exceeded")

// /healthz answers 503 unhealthy, naming "KV watch rebuild", once the bus's reconnect hooks have
// been due a whole bus.RepairGrace without a run that succeeded, and 200 healthy while the stall is
// under it. A rewatch that keeps failing after a reconnect ends no watcher and clears no cache, so
// before this branch every other check here passed - WatchErr stayed nil, the bucket ping answered,
// and the durable was bound - and /healthz called a listener whose caches follow buckets they no
// longer watch healthy, which is the regression this catches (INFRA-743). The negative control is
// the same request on the same rig after a connection event and a failed run, with the stall still
// under the grace: the bus runs the hooks again within seconds, so that must stay 200 healthy
// rather than fail the container health check over every reconnect.
func TestHealthzAnswersAHookStallPastTheRepairGrace(t *testing.T) {
	clock := &hookStallClock{now: time.Now()}
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)},
		bus.WithReplicas(1),
		bus.WithReconnectErrClock(clock.Now))
	if err != nil {
		t.Fatalf("connect the listener's bus: %v", err)
	}
	t.Cleanup(client.Close)
	const consumer = "listener-healthz-hook-stall"
	durable, err := startListenerSubscription(client, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
	if err != nil {
		t.Fatalf("bind the durable: %v", err)
	}
	t.Cleanup(func() { _ = durable.Unsubscribe() })
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	ready, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := registry.WaitForCacheReady(ready); err != nil {
		t.Fatalf("wait for the interest cache: %v", err)
	}
	if err := sessions.WaitForCacheReady(ready); err != nil {
		t.Fatalf("wait for the session cache: %v", err)
	}
	var deps atomic.Pointer[listenerDeps]
	deps.Store(&listenerDeps{client: client, registry: registry, sessions: sessions, caches: listenerCaches(registry, sessions, nil), consumer: consumer})
	get := func() (int, map[string]any) {
		recorder := httptest.NewRecorder()
		healthzHandler(&deps).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var body map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		return recorder.Code, body
	}
	if code, body := get(); code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("/healthz before any reconnect = %d %v, want 200 healthy", code, body)
	}

	// The hook stands for the listener's own rewatch of its caches, failing however often it is
	// run, as a rewatch whose bucket read never answers does.
	client.AddReconnectHook(func(*natsgo.Conn) error { return errHookStallRewatch })
	if err := client.Conn.ForceReconnect(); err != nil {
		t.Fatalf("reconnect in place: %v", err)
	}
	waitFor(t, 30*time.Second, "the failed run of the reconnect hooks to be recorded", func() bool {
		_, hookErr := client.HookStall()
		return hookErr != nil
	})
	if code, body := get(); code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("/healthz with the rebuild failing under bus.RepairGrace = %d %v, want 200 healthy", code, body)
	}

	clock.Advance(bus.RepairGrace)
	code, body := get()
	if code != http.StatusServiceUnavailable || body["status"] != "unhealthy" {
		t.Fatalf("/healthz a whole bus.RepairGrace into the stall = %d %v, want 503 unhealthy", code, body)
	}
	reported := fmt.Sprint(body["error"])
	if !strings.Contains(reported, "KV watch rebuild") || !strings.Contains(reported, errHookStallRewatch.Error()) {
		t.Fatalf("/healthz a whole bus.RepairGrace into the stall reported %q, want it to name the KV watch rebuild and %q", reported, errHookStallRewatch)
	}
	t.Logf("/healthz a whole bus.RepairGrace into the stall: %d %s", code, reported)
}
