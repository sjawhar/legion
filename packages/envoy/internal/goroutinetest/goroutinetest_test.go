package goroutinetest_test

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/goroutinetest"
)

// A test that waits for a goroutine to return passes at once, checking nothing, if Live never finds
// a goroutine that is still running, so Live must find one while it runs and lose it once it returns.
func TestLiveFollowsAGoroutineUntilItReturns(t *testing.T) {
	ids := make(chan uint64)
	release := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		ids <- goroutinetest.ID()
		<-release
	}()
	id := <-ids
	if !goroutinetest.Live(id) {
		t.Fatalf("goroutine %d is parked on a channel, and Live reports it returned", id)
	}
	close(release)
	<-returned
	deadline := time.Now().Add(5 * time.Second)
	for goroutinetest.Live(id) {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine %d returned 5 s ago, and Live still reports it running", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
