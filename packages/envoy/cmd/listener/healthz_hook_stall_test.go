package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
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

// failReconnectHooks makes every later run of the pod's reconnect hooks fail and reconnects its
// bus client in place, waiting until the bus has recorded the failed run. The hook stands for the
// listener's own rewatch of its caches, failing however often it is run, as a rewatch whose bucket
// read never answers does. The reconnect also leaves the pod's durable subscription on the old
// connection until its receive loop rebuilds it, which is a 503 of its own, so this waits for the
// pod to receive again and leaves the stall as the only thing wrong with it.
func failReconnectHooks(t *testing.T, rig *receiverRig) {
	t.Helper()
	rig.client.AddReconnectHook(func(*natsgo.Conn) error { return errHookStallRewatch })
	if err := rig.client.Conn.ForceReconnect(); err != nil {
		t.Fatalf("reconnect in place: %v", err)
	}
	waitFor(t, 30*time.Second, "the failed run of the reconnect hooks to be recorded", func() bool {
		_, hookErr := rig.client.HookStall()
		return hookErr != nil
	})
	waitFor(t, 30*time.Second, "the pod to receive its durable again", func() bool {
		report := rig.receiver.report()
		return report.State == receiveActive && report.SubscriptionValid
	})
}

// /healthz answers 200 "degraded", naming "KV watch rebuild", once the bus's reconnect hooks have
// been due a whole bus.RepairGrace without a run that succeeded, and 200 "healthy" while the
// stall is under it. A rewatch that keeps failing after a reconnect ends no watcher and clears no
// cache, so every other check here passes - WatchErr stays nil, the bucket ping answers, and the
// durable is bound - and the regression this catches is /healthz calling a listener healthy whose
// caches follow buckets they no longer watch (INFRA-743). The answer stays 200 because the probes
// act on it: a 503 would replace the only listener there is (the ECS container health check is
// curl -sf /healthz) into the same failing rebuild, whose empty caches then lose the fanout
// messages this task's stale ones still place. There are two negative controls. One is the same
// request on the same rig after a connection event and a failed run, with the stall still under
// the grace: the bus runs the hooks again within seconds, so that must stay 200 healthy rather
// than report every reconnect. The other is a whole grace of clock before any reconnect, below.
func TestHealthzAnswersAHookStallPastTheRepairGrace(t *testing.T) {
	rig, clock, get := hookStallFixture(t, "hook-stall")
	if code, body := get(); code != http.StatusOK || body["status"] != "healthy" || body["role"] != "active" {
		t.Fatalf("/healthz before any reconnect = %d %v, want 200 healthy, role active", code, body)
	}

	// A whole grace of clock with no connection event leaves the stall at zero: it begins at an
	// event the hooks have yet to cover, not at the connection that first ran them. This is the
	// lock on that, because a change marking the hooks due at the first connection would have
	// every listener answer degraded a minute after it started, with nothing wrong and no
	// reconnect anywhere, and every other test in the tree would stay green.
	clock.Advance(bus.RepairGrace)
	if stall, hookErr := rig.client.HookStall(); stall != 0 || hookErr != nil {
		t.Fatalf("HookStall a whole bus.RepairGrace after the first connection = %v %v, want 0 and no error", stall, hookErr)
	}
	if code, body := get(); code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("/healthz a whole bus.RepairGrace after the first connection = %d %v, want 200 healthy", code, body)
	}

	failReconnectHooks(t, rig)
	if code, body := get(); code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("/healthz with the rebuild failing under bus.RepairGrace = %d %v, want 200 healthy", code, body)
	}

	clock.Advance(bus.RepairGrace)
	code, body := get()
	if code != http.StatusOK || body["status"] != "degraded" || body["role"] != "active" {
		t.Fatalf("/healthz a whole bus.RepairGrace into the stall = %d %v, want 200 degraded, role active", code, body)
	}
	reported := fmt.Sprint(body["error"])
	if !strings.Contains(reported, "KV watch rebuild") || !strings.Contains(reported, errHookStallRewatch.Error()) {
		t.Fatalf("/healthz a whole bus.RepairGrace into the stall reported %q, want it to name the KV watch rebuild and %q", reported, errHookStallRewatch)
	}
	t.Logf("/healthz a whole bus.RepairGrace into the stall: %d %s", code, reported)
}

// hookStallFixture is the pod a hook stall is tested against, on the package's shared NATS
// server: machineID's lease holder, built by setupUnboundDurableFixture over a bus client whose
// reconnect reporting reads the clock the test moves. It returns the rig, that clock and a get()
// serving one /healthz request, which is every piece the tests in this file share.
func hookStallFixture(t *testing.T, machineID string) (*receiverRig, *hookStallClock, func() (int, map[string]any)) {
	t.Helper()
	clock := &hookStallClock{now: time.Now()}
	rig, get := setupUnboundDurableFixture(t, machineID,
		func(_ context.Context, msg *natsgo.Msg) { _ = msg.Ack() },
		bus.WithReconnectErrClock(clock.Now))
	return rig, clock, get
}

// A stall past bus.RepairGrace is the last thing /healthz decides, below every 503 check, so a
// durable the listener no longer holds keeps its refusal: a recreated durable delivers to an inbox
// this subscription knows nothing about, so the listener receives nothing at all, and the reason
// the stall answers 200 - this task's stale caches still place the fanout messages a replacement's
// empty ones would lose - does not hold for a listener that is deaf. The regression this catches is
// the stall deciding first and reporting such a listener as merely degraded, which every probe that
// reads /healthz then leaves in service (INFRA-743).
func TestHealthzReportsARecreatedDurableThroughAHookStall(t *testing.T) {
	rig, clock, get := hookStallFixture(t, "hook-stall-unbound")

	failReconnectHooks(t, rig)
	clock.Advance(bus.RepairGrace)
	if code, body := get(); code != http.StatusOK || body["status"] != "degraded" {
		t.Fatalf("/healthz a whole bus.RepairGrace into the stall, durable still bound = %d %v, want 200 degraded", code, body)
	}

	recreateListenerDurable(t, rig.client, rig.consumer)

	code, body := get()
	want := "durable consumer unavailable: " + errDurableUnbound.Error()
	if code != http.StatusServiceUnavailable || body["status"] != "unhealthy" || fmt.Sprint(body["error"]) != want {
		t.Fatalf("/healthz with the durable recreated a whole bus.RepairGrace into the stall = %d %v, want 503 unhealthy with error %q", code, body, want)
	}
	t.Logf("/healthz with the durable recreated a whole bus.RepairGrace into the stall: %d %s", code, fmt.Sprint(body["error"]))
}
