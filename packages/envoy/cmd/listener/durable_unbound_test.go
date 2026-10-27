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
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// /healthz on the pod receiving the durable answers 503 unhealthy once the durable has been
// recreated server-side, because a recreated durable delivers to a fresh inbox while this pod's
// subscription still holds the old one: the pod receives nothing at all. Without this the durable
// still exists, every health surface answers 200 healthy, and delivery to every session stops
// with no signal anywhere that the listener has gone deaf.
func TestHealthzReportsARecreatedDurableUnhealthy(t *testing.T) {
	rig, get := setupUnboundDurableFixture(t, "unbound-health", func(_ context.Context, msg *natsgo.Msg) { _ = msg.Ack() })
	if code, body := get(); code != http.StatusOK || body["status"] != "healthy" || body["role"] != "active" {
		t.Fatalf("before the durable is recreated: /healthz = %d %v, want 200 healthy, role active", code, body)
	}

	recreateListenerDurable(t, rig.client, rig.consumer)

	code, body := get()
	want := "durable consumer unavailable: not bound to this listener's subscription"
	if code != http.StatusServiceUnavailable || body["status"] != "unhealthy" || fmt.Sprint(body["error"]) != want {
		t.Fatalf("with the durable recreated: /healthz = %d %v, want 503 unhealthy with error %q", code, body, want)
	}
}

// setupUnboundDurableFixture is the pod a recreated durable is tested against: machineID's lease
// holder receiving the durable into handle, both registries open with their caches ready, and the
// listenerDeps /healthz reads. It returns the rig and a get() that serves one /healthz request,
// which is every piece the tests in this file share.
func setupUnboundDurableFixture(t *testing.T, machineID string, handle durableHandler) (*receiverRig, func() (int, map[string]any)) {
	t.Helper()
	rig := startReceiverRig(t, testnats.URL(t), machineID, "pod-a", handle, testReceiveTiming(), testLeaseTiming())
	rig.awaitState(t, receiveActive, 10*time.Second)
	client := rig.client
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
	subscribeRoleLane(t, client)
	state := &atomic.Pointer[listenerDeps]{}
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions, caches: []listenerCache{{name: "interest", cache: registry}, {name: "session", cache: sessions}}, receiver: rig.receiver})
	return rig, func() (int, map[string]any) { return serveHealthz(state) }
}

// startUnboundDurableMonitor runs the self-health monitor over the rig's durable the way the
// listener does, with the probe and the rebuild both the receive loop's. It returns the flag the
// monitor's terminate callback sets and a stop function that ends the monitor and waits for it,
// so each test keeps only its own logger, handler and assertions.
func startUnboundDurableMonitor(t *testing.T, rig *receiverRig, logger *logging.Logger) (*atomic.Bool, func()) {
	t.Helper()
	caches := []listenerCache{}
	ctx, cancel := context.WithCancel(context.Background())
	probe := func() error { return rig.receiver.probe(ctx) }
	terminated := &atomic.Bool{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSelfHealthMonitor(
			ctx,
			logger,
			func() error { return checkSelfHealth(caches, probe) },
			func(err error) bool { return isUnrecoverableSelfHealthFailure(err, rig.client, caches) },
			rig.client.Connected,
			func() error {
				return rebuildListenerDependencies(ctx, rig.client, caches, probe, rig.receiver.resubscribe)
			},
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

// The self-health monitor repairs a durable recreated under the holder: without the resubscribe,
// the subscription stays on the deleted consumer's inbox and every message published after the
// recreation is lost while the listener reports itself healthy.
func TestTheSelfHealthMonitorResubscribesARecreatedDurable(t *testing.T) {
	handle, received := recordingHandler()
	rig, _ := setupUnboundDurableFixture(t, "unbound-resubscribe", handle)

	recreateListenerDurable(t, rig.client, rig.consumer)
	publishDurable(t, rig.client, "after the recreation")

	terminated, stop := startUnboundDurableMonitor(t, rig, logging.New("test"))

	// The probe's confirming read alone costs 250ms, so the repair takes several intervals.
	if message := awaitReceived(t, received, 10*time.Second, "the message published after the recreation"); message.data != "after the recreation" {
		t.Fatalf("delivered %q", message.data)
	}
	if terminated.Load() {
		t.Fatal("the monitor terminated the listener instead of resubscribing its durable")
	}
	info, err := rig.client.JS().ConsumerInfo(bus.Stream, rig.consumer)
	if err != nil {
		t.Fatalf("consumer info after the resubscribe: %v", err)
	}
	if report := rig.receiver.report(); !report.SubscriptionValid || info.Config.DeliverSubject != report.Subject {
		t.Fatalf("after the resubscribe the durable delivers to %q, want this listener's subscription subject %q (valid %v)", info.Config.DeliverSubject, report.Subject, report.SubscriptionValid)
	}

	stop()
}

// A durable that is received never fails its own probe: the check compares the consumer's deliver
// subject against the live subscription's, so a mistake there would make every healthy holder
// resubscribe every interval and eventually terminate.
func TestABoundDurableNeverFailsItsProbe(t *testing.T) {
	rig, get := setupUnboundDurableFixture(t, "unbound-control", func(_ context.Context, msg *natsgo.Msg) { _ = msg.Ack() })

	// The monitor logs from its own goroutine, so the buffer the test reads back is guarded.
	logs := &lockedBuffer{}
	terminated, stop := startUnboundDurableMonitor(t, rig, logging.NewWithWriter("test", logs))

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
		t.Fatalf("a received durable failed its probe; the monitor logged: %s", captured)
	}
	if terminated.Load() {
		t.Fatal("the monitor terminated a listener whose durable was received throughout")
	}
}
