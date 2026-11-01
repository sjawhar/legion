package kvwatch_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/goroutinetest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// natsContextWaits counts the goroutines nats.go parks on a subscription's context for its life
// (nats.go v1.50.0 js.go:2052-2058), one per watch until its context is cancelled. The trace names
// the package nats%2ego, so the function is matched without its package.
func natsContextWaits() int {
	n := 0
	for _, goroutine := range goroutinetest.Traces() {
		if strings.Contains(goroutine, ".(*js).subscribe.func") && strings.Contains(goroutine, "/js.go:") {
			n++
		}
	}
	return n
}

// deliveryBlocked reports whether a jetstream watcher's delivery is parked on its full updates
// buffer: nats.go's message callback blocked in its send, holding the watcher's lock. That is the
// moment nats.go's legacy watcher could panic when its subscription ended (subscription_end_test.go).
func deliveryBlocked() bool {
	for _, goroutine := range goroutinetest.Traces() {
		header, _, _ := strings.Cut(goroutine, "\n")
		if strings.Contains(header, "[chan send") && strings.Contains(goroutine, "jetstream.(*kvs).WatchFiltered.func") {
			return true
		}
	}
	return false
}

// A watcher's subscription can end while its scan is under way and its idle wait armed: a watch
// that replaces it stops it, the connection it reads through closes, or End stops it at shutdown.
// Each is done here at the worst moment for nats.go's legacy watcher, with the cache holding the
// scan's first record, so the 256-entry updates buffer is full and nats.go's delivery is parked on
// it holding the watcher's lock. None may panic the process; the idle wait of that scan is
// disarmed, and its callback, run late as an already-started time.AfterFunc callback can be, ends
// nothing: no timed-out warm-up, no re-read; and the goroutine nats.go parks on the watch's context
// is released once the watcher has been stopped or has ended.
func TestASubscriptionEndingMidScanDisarmsTheIdleWaitAndReleasesItsContext(t *testing.T) {
	const records = 600
	for _, test := range []struct {
		name string
		// end ends the subscription of w, whose scan is held, and returns once it has asked for
		// that end. release lets the held record go.
		end func(t *testing.T, w *kvwatch.Watcher, conn *natsgo.Conn, uri string, release func())
		// replaced is set when the end leaves a newer watcher running, with a context of its own.
		replaced bool
		// stopped is the terminal error the end leaves, "" for none.
		stopped string
	}{
		{name: "a replacing watch stops it", replaced: true, end: func(t *testing.T, w *kvwatch.Watcher, _ *natsgo.Conn, uri string, release func()) {
			_, other := bucket(t, uri)
			done := make(chan error, 1)
			// The switch waits for the held apply (applyMu), so the release comes once the watch is
			// parked on it, and the watch stops the replaced watcher while its scan is under way.
			go func() { done <- kvwatch.WatchOn(w, other) }()
			eventually(t, "the replacing watch waiting for the held apply", waitingForTheApplyLock)
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the replacing watch: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the replacing watch never returned")
			}
		}},
		{name: "its connection closes", stopped: "test cache watcher stopped", end: func(t *testing.T, _ *kvwatch.Watcher, conn *natsgo.Conn, _ string, release func()) {
			conn.Close()
			release()
		}},
		{name: "End stops it at shutdown", end: func(t *testing.T, w *kvwatch.Watcher, _ *natsgo.Conn, _ string, release func()) {
			endWithin(t, w)
			release()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			uri := testnats.URL(t)
			_, seed := bucket(t, uri)
			for i := range records {
				if _, err := seed.Put(fmt.Sprintf("k%04d", i), []byte("v")); err != nil {
					t.Fatalf("put: %v", err)
				}
			}
			conn, kv := bucket(t, uri)
			baseline := natsContextWaits()

			held, released := make(chan struct{}), make(chan struct{})
			var holdOnce, releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(released) }) }
			t.Cleanup(release)
			into := withApply{seen: newSeen(), before: func(natsgo.KeyValueEntry) {
				holdOnce.Do(func() {
					close(held)
					<-released
				})
			}}
			timers := &pending{}
			now := newClock()
			logger, logged := jsonLogger(t)
			w := kvwatch.New("test cache", kv, into, kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithNow(now.Now),
				kvwatch.WithStreamProbeInterval(0), kvwatch.WithLogger(logger))
			t.Cleanup(w.Stop)
			w.Start()
			wait(t, held, "the scan's first record")
			idle := timers.last(t, kvwatch.ScanIdleTimeout)
			eventually(t, "nats.go's delivery to park on the full updates buffer", deliveryBlocked)
			if natsContextWaits() <= baseline {
				t.Fatal("no goroutine of nats.go's waits on the running watch's context: the release checked below would prove nothing")
			}

			test.end(t, w, conn, uri, release)

			live := 0
			if test.replaced {
				live = 1
			}
			eventuallyWithin(t, 10*time.Second, "nats.go's goroutine on the ended watch's context to be released",
				func() bool { return natsContextWaits() <= baseline+live })
			if test.stopped != "" {
				eventually(t, "the ended watcher's error", func() bool { return w.Err() != nil })
			}
			if !idle.stopped.Load() {
				t.Fatal("the ended scan's idle wait was left armed")
			}

			// The idle wait's callback runs late, past the timeout, as a time.AfterFunc callback
			// already started when its Stop came would.
			rereads := len(timers.armed(5 * time.Second))
			now.advance(2 * kvwatch.ScanIdleTimeout)
			idle.fire()
			time.Sleep(200 * time.Millisecond)
			if timedOut := matching(logged(), "WARN", "test cache cache warm-up"); len(timedOut) != 0 {
				t.Fatalf("the late idle callback timed the ended scan out: %v", timedOut)
			}
			if got := len(timers.armed(5 * time.Second)); got != rereads {
				t.Fatalf("the late idle callback armed %d re-reads, want none", got-rereads)
			}
			var recorded string
			if err := w.Err(); err != nil {
				recorded = err.Error()
			}
			if recorded != test.stopped {
				t.Fatalf("Err after the end = %q, want %q", recorded, test.stopped)
			}
		})
	}
}
