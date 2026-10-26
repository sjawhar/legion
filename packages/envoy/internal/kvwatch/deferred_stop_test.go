package kvwatch_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/goroutinetest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// fakeWatcher is a nats.KeyWatcher the test drives: it delivers what the test hands it (deliver)
// and ends when the test ends it (end), as nats.go's ends when its subscription does. Stop counts
// its calls, closes stopped at the first and answers stopErr; it does not end the watcher, so a
// test tells the stop apart from the end it would bring about.
type fakeWatcher struct {
	updates chan natsgo.KeyValueEntry
	errs    chan error
	stopErr error
	stops   atomic.Int32
	stopped chan struct{}
	stopOne sync.Once
	endOne  sync.Once
}

func newFakeWatcher(t *testing.T) *fakeWatcher {
	f := &fakeWatcher{updates: make(chan natsgo.KeyValueEntry), errs: make(chan error, 1), stopped: make(chan struct{})}
	t.Cleanup(f.end)
	return f
}

func (f *fakeWatcher) Context() context.Context             { return context.Background() }
func (f *fakeWatcher) Updates() <-chan natsgo.KeyValueEntry { return f.updates }
func (f *fakeWatcher) Error() <-chan error                  { return f.errs }

func (f *fakeWatcher) Stop() error {
	f.stops.Add(1)
	f.stopOne.Do(func() { close(f.stopped) })
	return f.stopErr
}

// end ends the watcher with no nil, as nats.go closes both channels when the subscription ends.
func (f *fakeWatcher) end() {
	f.endOne.Do(func() {
		close(f.updates)
		close(f.errs)
	})
}

// deliver hands the watcher's reader entry (nil is the scan's end marker), returning once the
// reader has taken it.
func (f *fakeWatcher) deliver(t *testing.T, entry natsgo.KeyValueEntry) {
	t.Helper()
	select {
	case f.updates <- entry:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out handing the watcher's reader an entry")
	}
}

// step is the watch that hands out f.
func (f *fakeWatcher) step() watchStep {
	return func(natsgo.KeyValue) (natsgo.KeyWatcher, error) { return f, nil }
}

// stopCount fails the test unless f was stopped want times, naming when.
func (f *fakeWatcher) stopCount(t *testing.T, want int32, when string) {
	t.Helper()
	if got := f.stops.Load(); got != want {
		t.Fatalf("%s: the watcher was stopped %d times, want %d", when, got, want)
	}
}

// statusKV answers Status with status, the bucket's status as an earlier read found it, so a watch
// on it reads a stream older than the bucket's.
type statusKV struct {
	natsgo.KeyValue
	status natsgo.KeyValueStatus
}

func (k statusKV) Status() (natsgo.KeyValueStatus, error) { return k.status, nil }

// stopsWaiting counts the goroutines waiting for a watcher's scan to end to stop it, or stopping it.
func stopsWaiting() int {
	n := 0
	for _, goroutine := range goroutinetest.Traces() {
		if strings.Contains(goroutine, "kvwatch.(*Watcher).stopAfterScan(") {
			n++
		}
	}
	return n
}

// noStopsWaiting waits for the deferred stops earlier tests left to finish, so a test that counts
// them counts only its own.
func noStopsWaiting(t *testing.T) {
	t.Helper()
	eventually(t, "earlier tests' deferred stops to finish", func() bool { return stopsWaiting() == 0 })
}

// entryOf puts key in the bucket and returns its entry, a record a fake watcher delivers.
func entryOf(t *testing.T, kv bus.KeyValue, key string) natsgo.KeyValueEntry {
	t.Helper()
	if _, err := kv.Put(key, []byte("1")); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	entry, err := kv.Get(key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return entry
}

// endWithin runs End within 10 s.
func endWithin(t *testing.T, w *kvwatch.Watcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w.End(ctx)
}

// scanningFirst starts a watcher whose first watch is f, and returns once its consume has taken a
// record of the scan f never ends, so the watcher installed f and is reading it mid-scan.
func scanningFirst(t *testing.T, kv bus.KeyValue, f *fakeWatcher, options ...kvwatch.Option) *kvwatch.Watcher {
	t.Helper()
	entry := entryOf(t, kv, "kept")
	options = append([]kvwatch.Option{kvwatch.WithStreamProbeInterval(0)}, options...)
	w := kvwatch.New("test cache", scripted(kv.KeyValue, f.step()).handle(), newSeen(), options...)
	t.Cleanup(w.Stop)
	w.Start()
	f.deliver(t, entry)
	return w
}

// Stopping a nats.go watcher before its scan's nil can crash the process: the idle timer that ends
// a stalled scan may already be waiting on the watcher's lock when the stop closes its channels,
// and then sends on a closed one (stopAfterScan in kvwatch.go). So a Rewatch that replaces a
// watcher still scanning, as the reconnect hook does after NATS restarts mid-scan, keeps it drained
// and stops it only once its nil has come, exactly once: End, which ends what kvwatch still holds,
// does not stop it again.
func TestAReplacedWatcherStillScanningIsStoppedOnlyOnceItsScanEnds(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	scanning := newFakeWatcher(t)
	w := scanningFirst(t, kv, scanning)

	if err := kvwatch.WatchOn(w, scripted(kv.KeyValue).handle()); err != nil {
		t.Fatalf("the replacing watch: %v", err)
	}
	eventually(t, "the replacement's read to complete", w.Following)
	scanning.deliver(t, entryOf(t, kv, "late"))
	scanning.stopCount(t, 0, "replaced mid-scan, with a record delivered since")

	scanning.deliver(t, nil)
	wait(t, scanning.stopped, "the replaced watcher's stop once its scan ended")
	endWithin(t, w)
	scanning.stopCount(t, 1, "after its scan ended and End ran")
}

// A replaced watcher whose scan has already ended cannot meet the race, so the watch that replaces
// it stops it at once, before it returns.
func TestAReplacedWatcherWhoseScanEndedIsStoppedAtOnce(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	scanned := newFakeWatcher(t)
	w := scanningFirst(t, kv, scanned)
	scanned.deliver(t, nil)
	eventually(t, "the first read's completion", w.Following)

	if err := kvwatch.WatchOn(w, scripted(kv.KeyValue).handle()); err != nil {
		t.Fatalf("the replacing watch: %v", err)
	}
	scanned.stopCount(t, 1, "when the replacing watch returned")
	endWithin(t, w)
	scanned.stopCount(t, 1, "after End")
}

// A watch that is discarded rather than installed -- its stream is older than the running
// watcher's, or it re-reads a generation since overtaken -- is drained and, like a replaced one,
// stopped only once its scan's nil has come, exactly once.
func TestADiscardedWatcherIsStoppedOnlyOnceItsScanEnds(t *testing.T) {
	for _, test := range []struct {
		name string
		// older, when set, runs ahead of the watcher's start: it recreates the bucket and returns
		// the old bucket's status, which the discarded watch then reads. Unset, the discarded
		// watch is a re-read of a generation a newer watch overtook.
		older func(t *testing.T, conn *natsgo.Conn, kv bus.KeyValue) natsgo.KeyValueStatus
	}{
		{
			name: "a watch of an older stream",
			older: func(t *testing.T, conn *natsgo.Conn, kv bus.KeyValue) natsgo.KeyValueStatus {
				old, err := kv.Status()
				if err != nil {
					t.Fatalf("status: %v", err)
				}
				js := jetStream(t, conn)
				deleteTestBucket(t, js)
				testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
				return old
			},
		},
		{name: "a re-read of an overtaken generation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			uri := testnats.URL(t)
			conn, kv := bucket(t, uri)
			var old natsgo.KeyValueStatus
			if test.older != nil {
				old = test.older(t, conn, kv)
			}
			into := newSeen()
			w := kvwatch.New("test cache", kv, into, kvwatch.WithStreamProbeInterval(0))
			t.Cleanup(w.Stop)
			w.Start()
			eventually(t, "the first read's completion", w.Following)

			discarded := newFakeWatcher(t)
			if old != nil {
				stale := scripted(statusKV{KeyValue: kv.KeyValue, status: old}, discarded.step())
				if err := kvwatch.WatchOn(w, stale.handle()); err != nil {
					t.Fatalf("the older stream's watch: %v", err)
				}
			} else {
				overtaken := kvwatch.Generation(w)
				if err := kvwatch.WatchOn(w, scripted(kv.KeyValue).handle()); err != nil {
					t.Fatalf("the overtaking watch: %v", err)
				}
				if err := kvwatch.RereadOn(w, scripted(kv.KeyValue, discarded.step()).handle(), overtaken); err != nil {
					t.Fatalf("the overtaken re-read: %v", err)
				}
			}
			reads := len(into.reads())
			discarded.deliver(t, entryOf(t, kv, "late"))
			if got := len(into.reads()); got != reads || w.Err() != nil {
				t.Fatalf("after the discarded watch: %d reads begun (%d before it), Err %v; want it discarded", got, reads, w.Err())
			}
			discarded.stopCount(t, 0, "discarded mid-scan, with a record delivered since")

			discarded.deliver(t, nil)
			wait(t, discarded.stopped, "the discarded watcher's stop once its scan ended")
			endWithin(t, w)
			discarded.stopCount(t, 1, "after its scan ended and End ran")
		})
	}
}

// The shutdown cannot wait for a scan: End deletes every consumer before the connection's drain.
// So End stops a replaced watcher whose stop still waits for its scan's end, and when that end
// comes later, the wait finds the stop taken and stops nothing more.
func TestEndStopsAReplacedWatcherWhoseStopStillWaits(t *testing.T) {
	noStopsWaiting(t)
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	logger, records := jsonLogger(t)
	scanning := newFakeWatcher(t)
	w := scanningFirst(t, kv, scanning, kvwatch.WithLogger(logger))
	if err := kvwatch.WatchOn(w, scripted(kv.KeyValue).handle()); err != nil {
		t.Fatalf("the replacing watch: %v", err)
	}
	eventually(t, "the replaced watcher's stop to wait for its scan", func() bool { return stopsWaiting() == 1 })

	endWithin(t, w)
	scanning.stopCount(t, 1, "when End returned")
	scanning.deliver(t, nil)
	eventually(t, "the deferred stop's wait to return", func() bool { return stopsWaiting() == 0 })
	scanning.stopCount(t, 1, "after its scan ended")
	if got := warnings(records()); len(got) != 0 {
		t.Fatalf("logged %q, want nothing", got)
	}
}

// A replaced watcher can end before its scan's nil (an ordered consumer nats.go could not
// recreate, a closed connection). Its end releases the wait as its nil would, and the stop that
// follows, which nats.go answers with the subscription already gone, logs nothing.
func TestAReplacedWatcherEndingBeforeItsNilReleasesTheWait(t *testing.T) {
	noStopsWaiting(t)
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	logger, records := jsonLogger(t)
	ending := newFakeWatcher(t)
	ending.stopErr = natsgo.ErrBadSubscription
	w := scanningFirst(t, kv, ending, kvwatch.WithLogger(logger))
	if err := kvwatch.WatchOn(w, scripted(kv.KeyValue).handle()); err != nil {
		t.Fatalf("the replacing watch: %v", err)
	}
	ending.stopCount(t, 0, "replaced mid-scan")

	ending.end()
	wait(t, ending.stopped, "the stop once the replaced watcher ended")
	eventually(t, "the deferred stop's wait to return", func() bool { return stopsWaiting() == 0 })
	ending.stopCount(t, 1, "after it ended")
	if got := warnings(records()); len(got) != 0 {
		t.Fatalf("logged %q, want nothing", got)
	}
	if err := w.Err(); err != nil {
		t.Fatalf("Err = %v after a replaced watcher ended, want nil", err)
	}
}
