package store

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// A loop's done channel closes only once a pass in flight when its context ends has finished, and
// no pass starts after the end: the listener's shutdown waits on that channel before it drains
// NATS, so a pass's consumer delete never races the drain.
func TestALoopEndsWithItsContextOnlyAfterItsPassInFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var passes atomic.Int32
	done := everyInterval(ctx, 10*time.Millisecond, func() {
		if passes.Add(1) == 1 {
			entered <- struct{}{}
			<-release
		}
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never ran a pass")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("the loop reported done with its pass still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never ended after its context")
	}
	if got := passes.Load(); got != 1 {
		t.Fatalf("the loop ran %d passes, want none after its context ended", got)
	}
}

// The interest marker collector ends with its context, and by the time it reports done its pass's
// scan has deleted the consumer nats.go created for it: the bucket keeps only the registry's own
// cache watcher's consumer.
func TestTheMarkerCollectorDeletesItsScansConsumerBeforeItEnds(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	registry := openRegistry(t, conn)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	consumers := func() int {
		count := 0
		for range js.ConsumerNames("KV_" + bus.InterestBucket) {
			count++
		}
		return count
	}
	if got := consumers(); got != 1 {
		t.Fatalf("the interest bucket has %d consumers before the collector, want the cache watcher's one", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := registry.StartInterestMarkerCollector(ctx, func() natsgo.JetStreamContext { return js }, time.Hour)
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the collector never ended after its context")
	}
	if got := consumers(); got != 1 {
		t.Fatalf("the interest bucket has %d consumers after the collector ended, want the cache watcher's one", got)
	}
}
