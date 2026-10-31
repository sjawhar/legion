package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// healthzRoleViewRig is a listener's /healthz over a live bus, a complete role claim view and the
// pod that receives machineID's durable: the state every test here starts from, with the handler
// reached as the listener mounts it. It answers the request and the decoded body, and names the
// durable that pod holds, since the lease's own receive loop binds it.
func healthzRoleViewRig(t *testing.T, machineID string) (func() (int, map[string]any), *store.Registry, *bus.Client, string) {
	t.Helper()
	client := setupTestNATS(t)
	t.Cleanup(client.Close)
	rig := startReceiverRig(t, client.Conn.ConnectedUrl(), machineID, "pod-a", func(_ context.Context, msg *natsgo.Msg) { _ = msg.Ack() }, testReceiveTiming(), testLeaseTiming())
	rig.awaitState(t, receiveActive, 10*time.Second)
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
	subscribeRoleLane(t, client)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions, caches: listenerCaches(registry, sessions, nil), receiver: rig.receiver})
	get := func() (int, map[string]any) { return serveHealthz(&state) }
	if code, body := get(); code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("the rig's own /healthz = %d %v, want 200 healthy", code, body)
	}
	return get, registry, client, rig.consumer
}

// breakRoleClaimView leaves registry's role claim view where a first scan nats.go's idle timer
// ended leaves it: the watcher runs, the buckets answer, and the view refuses every read.
func breakRoleClaimView(t *testing.T, registry *store.Registry, client *bus.Client) {
	t.Helper()
	raw, err := client.JS().KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	timeOutRoleClaimScan(t, registry, raw)
	if _, err := registry.RolesHeldBy("ses_healthz_role_view"); err == nil {
		t.Fatal("the role claim view still answers a read, so /healthz has nothing to report")
	}
}

// A listener whose role claim view completes no scan serves: its buckets answer, its watchers run,
// and the two routes that report a session's roles - GET /v1/sessions and GET
// /v1/registry/<session_id> - answer 503 all the same, because that view refuses every read until
// its watcher has delivered every claim. /healthz says so, as a 200 "degraded" naming the role
// claim view, where before it answered "healthy": Ping reads the buckets and the watchers'
// terminal errors, and a scan that has not completed is neither. It is 200, so a readiness check
// (curl -f or -sf against /healthz) still passes, and the view fills itself through its watcher's
// own rescan - a 503 here would take a serving listener out of a load balancer for a state that
// repairs itself. The answer keeps the fields the healthy one carries, so a reader of the session
// counts or the durable's backlog loses none of them while the view fills.
func TestHealthzReportsARoleClaimViewThatCompletesNoScanDegraded(t *testing.T) {
	get, registry, client, _ := healthzRoleViewRig(t, "healthz-role-view")
	breakRoleClaimView(t, registry, client)

	code, body := get()
	if code != http.StatusOK || body["status"] != "degraded" {
		t.Fatalf("/healthz over a role claim view that completed no scan = %d %v, want 200 degraded", code, body)
	}
	reported := fmt.Sprint(body["error"])
	if !strings.Contains(reported, "role claim view unavailable") || !strings.Contains(reported, store.ErrRoleViewNotReady.Error()) {
		t.Fatalf("/healthz degraded with error %q, want it naming the role claim view and %q", reported, store.ErrRoleViewNotReady)
	}
	for _, field := range []string{"session_cache_ready", "session_cache_size", "live_topics", "ci_cache", "num_pending", "num_ack_pending"} {
		if _, ok := body[field]; !ok {
			t.Errorf("the degraded answer %v has no %s; a reader of that field loses it while the view fills", body, field)
		}
	}
}

// The role claim view repairs itself and a lost durable consumer does not, so a listener whose
// view is incomplete must still report the durable as unhealthy: a readiness check passes a 200,
// so reporting that fault as the view's degraded 200 would leave a listener that receives nothing
// in service. The view is therefore reported last, after every check that answers 503.
func TestHealthzStillReportsALostDurableWhileTheRoleClaimViewIsIncomplete(t *testing.T) {
	get, registry, client, consumer := healthzRoleViewRig(t, "healthz-role-view-durable")
	breakRoleClaimView(t, registry, client)
	if err := client.JS().DeleteConsumer(bus.Stream, consumer); err != nil {
		t.Fatalf("delete the durable: %v", err)
	}

	code, body := get()
	if code != http.StatusServiceUnavailable || body["status"] != "unhealthy" {
		t.Fatalf("/healthz with the durable gone and the role claim view incomplete = %d %v, want 503 unhealthy", code, body)
	}
	if reported := fmt.Sprint(body["error"]); !strings.Contains(reported, "durable consumer unavailable") {
		t.Fatalf("/healthz answered %q, want it naming the durable consumer: the lost durable is the fault that does not repair itself", reported)
	}
}
