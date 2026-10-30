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
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// A listener whose role claim view completes no scan serves: its buckets answer, its watchers run,
// and the two routes that report a session's roles - GET /v1/sessions and GET
// /v1/registry/<session_id> - answer 503 all the same, because that view refuses every read until
// its watcher has delivered every claim. /healthz says so, as a 200 "degraded" naming the role
// claim view, where before it answered "healthy": Ping reads the buckets and the watchers'
// terminal errors, and a scan that has not completed is neither. It is 200, so the deployment's
// probes (curl -f and -sf against /healthz: infra/host/listener.go, infra/host/watchdog.go and the
// compose files) still pass, and the view fills itself through its watcher's own rescan - a 503
// here would take a serving listener out of a load balancer for a state that repairs itself. The
// same mux still answers /livez the 404 it always has, reading nothing: this report is readiness,
// never a liveness failure.
func TestHealthzReportsARoleClaimViewThatCompletesNoScanDegraded(t *testing.T) {
	const consumer = "listener-healthz-role-view"
	client := setupTestNATS(t)
	sub, err := startListenerSubscription(client, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
	if err != nil {
		t.Fatalf("bind the durable: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open interest registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readyCancel()
	if err := registry.WaitForCacheReady(readyCtx); err != nil {
		t.Fatalf("wait for the registry's caches: %v", err)
	}
	if err := sessions.WaitForCacheReady(readyCtx); err != nil {
		t.Fatalf("wait for the session cache: %v", err)
	}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions, caches: listenerCaches(registry, sessions, nil), consumer: consumer})
	// The mux the listener builds over the same handler, so /livez is answered here as it is in
	// production: by no route of this listener's at all.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler(&state))
	get := func(path string) (int, map[string]any) {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		var body map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		return recorder.Code, body
	}
	if code, body := get("/healthz"); code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("control: /healthz over a complete role claim view = %d %v, want 200 healthy", code, body)
	}
	liveCode, _ := get("/livez")

	raw, err := client.JS().KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	timeOutRoleClaimScan(t, registry, raw)
	if _, err := registry.RolesHeldBy("ses_healthz_role_view"); err == nil {
		t.Fatal("the role claim view still answers a read, so /healthz has nothing to report")
	}

	code, body := get("/healthz")
	if code != http.StatusOK || body["status"] != "degraded" {
		t.Fatalf("/healthz over a role claim view that completed no scan = %d %v, want 200 degraded", code, body)
	}
	reported := fmt.Sprint(body["error"])
	if !strings.Contains(reported, "role claim view unavailable") || !strings.Contains(reported, store.ErrRoleViewNotReady.Error()) {
		t.Fatalf("/healthz degraded with error %q, want it naming the role claim view and %q", reported, store.ErrRoleViewNotReady)
	}
	if code, body := get("/livez"); code != liveCode {
		t.Fatalf("/livez answered %d %v with the role claim view reported degraded, want the %d it answered before: a readiness report is not a liveness failure", code, body, liveCode)
	}
}
