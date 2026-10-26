package kvwatch_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// fakeWatcher is a jetstream.KeyWatcher the test drives: it delivers what the test hands it
// (deliver) and ends when the test ends it (end), as nats.go's ends when its subscription does.
// Stop counts its calls, closes stopped at the first and answers stopErr; it does not end the
// watcher, so a test tells the stop apart from the end it would bring about, and a watcher stopped
// but not yet ended still has to be read. ctx is the context the watch handed it, the one its
// subscription would live in.
type fakeWatcher struct {
	updates chan jetstream.KeyValueEntry
	stopErr error
	stops   atomic.Int32
	stopped chan struct{}
	stopOne sync.Once
	endOne  sync.Once
	mu      sync.Mutex
	ctx     context.Context
}

func newFakeWatcher(t *testing.T) *fakeWatcher {
	f := &fakeWatcher{updates: make(chan jetstream.KeyValueEntry), stopped: make(chan struct{})}
	t.Cleanup(f.end)
	return f
}

func (f *fakeWatcher) Updates() <-chan jetstream.KeyValueEntry { return f.updates }

func (f *fakeWatcher) Stop() error {
	f.stops.Add(1)
	f.stopOne.Do(func() { close(f.stopped) })
	return f.stopErr
}

// end ends the watcher with no nil, as nats.go closes the updates when the subscription ends.
func (f *fakeWatcher) end() {
	f.endOne.Do(func() { close(f.updates) })
}

// deliver hands the watcher's reader entry (nil is the scan's end marker), returning once the
// reader has taken it.
func (f *fakeWatcher) deliver(t *testing.T, entry jetstream.KeyValueEntry) {
	t.Helper()
	select {
	case f.updates <- entry:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out handing the watcher's reader an entry: nothing reads it")
	}
}

// step is the watch that hands out f, keeping the context it was created in.
func (f *fakeWatcher) step() watchStep {
	return func(ctx context.Context, _ jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.ctx = ctx
		return f, nil
	}
}

// stopCount fails the test unless f was stopped want times, naming when.
func (f *fakeWatcher) stopCount(t *testing.T, want int32, when string) {
	t.Helper()
	if got := f.stops.Load(); got != want {
		t.Fatalf("%s: the watcher was stopped %d times, want %d", when, got, want)
	}
}

// released fails the test unless the context f's watch was created in is cancelled, which is what
// releases the goroutine nats.go parks on it for the subscription's life.
func (f *fakeWatcher) released(t *testing.T, when string) {
	t.Helper()
	f.mu.Lock()
	ctx := f.ctx
	f.mu.Unlock()
	if ctx == nil || ctx.Err() == nil {
		t.Fatalf("%s: the watch's context is still live, so nats.go's goroutine on it would outlive the watch", when)
	}
}

// fakeEntry is a put of key that no bucket holds, a record only a fake watcher delivers.
type fakeEntry struct{ key string }

func (e fakeEntry) Bucket() string                  { return testBucketName }
func (e fakeEntry) Key() string                     { return e.key }
func (e fakeEntry) Value() []byte                   { return []byte("1") }
func (e fakeEntry) Revision() uint64                { return 1 }
func (e fakeEntry) Created() time.Time              { return time.Time{} }
func (e fakeEntry) Delta() uint64                   { return 0 }
func (e fakeEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValuePut }

// statusKV answers a read of its bucket's stream with info, the stream as an earlier read found
// it, so a watch on it reads a stream older than the bucket's.
type statusKV struct {
	jetstream.JetStream
	info *jetstream.StreamInfo
}

func (k statusKV) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	stream, err := k.JetStream.Stream(ctx, name)
	if err != nil {
		return nil, err
	}
	return staleStream{Stream: stream, info: k.info}, nil
}

// staleStream is a stream whose info is the earlier read's.
type staleStream struct {
	jetstream.Stream
	info *jetstream.StreamInfo
}

func (s staleStream) CachedInfo() *jetstream.StreamInfo { return s.info }

// endWithin runs End within 10 s.
func endWithin(t *testing.T, w *kvwatch.Watcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w.End(ctx)
}

// scanningFirst starts a watcher feeding into whose first watch is f, and returns once its consume
// has taken a record of the scan f never ends, so the watcher installed f and is reading it
// mid-scan.
func scanningFirst(t *testing.T, kv bus.KeyValue, f *fakeWatcher, into kvwatch.Cache, options ...kvwatch.Option) *kvwatch.Watcher {
	t.Helper()
	options = append([]kvwatch.Option{kvwatch.WithStreamProbeInterval(0)}, options...)
	w := kvwatch.New("test cache", scripted(kv, f.step()).handle(), into, options...)
	t.Cleanup(w.Stop)
	w.Start()
	f.deliver(t, fakeEntry{key: "first"})
	return w
}

// The jetstream watcher has no timer that could send once its subscription has ended, so a watch
// that replaces a watcher still scanning, as the reconnect hook does after NATS restarts mid-scan,
// stops it at once and cancels the context its subscription lives in, which releases nats.go's
// goroutine on it. Stopped is not ended: the watcher is still read, what it delivers is dropped,
// and its end, when nats.go closes it, records nothing and logs nothing. End, which ends what
// kvwatch still holds, does not stop it again.
func TestAReplacedWatcherStillScanningIsStoppedAtOnceAndReadUntilItEnds(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	logger, records := jsonLogger(t)
	scanning := newFakeWatcher(t)
	into := newSeen()
	w := scanningFirst(t, kv, scanning, into, kvwatch.WithLogger(logger))

	if err := kvwatch.WatchOn(w, scripted(kv).handle()); err != nil {
		t.Fatalf("the replacing watch: %v", err)
	}
	scanning.stopCount(t, 1, "replaced mid-scan, when the replacing watch returned")
	scanning.released(t, "replaced mid-scan")
	eventually(t, "the replacement's read to complete", w.Following)

	scanning.deliver(t, fakeEntry{key: "from-the-replaced-watcher"})
	scanning.deliver(t, nil)
	scanning.end()
	time.Sleep(200 * time.Millisecond)
	if into.has("from-the-replaced-watcher") || w.Err() != nil || len(warnings(records())) != 0 || !w.Following() {
		t.Fatalf("after the replaced watcher delivered and ended: its record applied %v, Err %v, logged %q, Following %v; want nothing applied, recorded or logged, the copy following",
			into.has("from-the-replaced-watcher"), w.Err(), warnings(records()), w.Following())
	}
	endWithin(t, w)
	scanning.stopCount(t, 1, "after End")
}

// A watch that is discarded rather than installed -- its stream is older than the running
// watcher's, or it re-reads a generation since overtaken -- is stopped at once, its context
// cancelled, and read until it ends, what it delivers dropped. End does not stop it again.
func TestADiscardedWatcherIsStoppedAtOnceAndReadUntilItEnds(t *testing.T) {
	for _, test := range []struct {
		name string
		// older, when set, runs ahead of the watcher's start: it recreates the bucket and returns
		// the old bucket's status, which the discarded watch then reads. Unset, the discarded
		// watch is a re-read of a generation a newer watch overtook.
		older func(t *testing.T, conn *natsgo.Conn, kv bus.KeyValue) *jetstream.StreamInfo
	}{
		{
			name: "a watch of an older stream",
			older: func(t *testing.T, conn *natsgo.Conn, kv bus.KeyValue) *jetstream.StreamInfo {
				stream, err := kv.Streams().Stream(context.Background(), "KV_"+kv.Bucket())
				if err != nil {
					t.Fatalf("stream: %v", err)
				}
				old := stream.CachedInfo()
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
			var old *jetstream.StreamInfo
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
				stale := scripted(bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), statusKV{JetStream: kv.Streams(), info: old}), discarded.step())
				if err := kvwatch.WatchOn(w, stale.handle()); err != nil {
					t.Fatalf("the older stream's watch: %v", err)
				}
			} else {
				overtaken := kvwatch.Generation(w)
				if err := kvwatch.WatchOn(w, scripted(kv).handle()); err != nil {
					t.Fatalf("the overtaking watch: %v", err)
				}
				if err := kvwatch.RereadOn(w, scripted(kv, discarded.step()).handle(), overtaken); err != nil {
					t.Fatalf("the overtaken re-read: %v", err)
				}
			}
			discarded.stopCount(t, 1, "discarded, when its watch returned")
			discarded.released(t, "discarded")
			reads := len(into.reads())
			discarded.deliver(t, fakeEntry{key: "from-the-discarded-watcher"})
			discarded.deliver(t, nil)
			if got := len(into.reads()); got != reads || w.Err() != nil || into.has("from-the-discarded-watcher") {
				t.Fatalf("after the discarded watch delivered: %d reads begun (%d before it), Err %v, its record applied %v; want it discarded",
					got, reads, w.Err(), into.has("from-the-discarded-watcher"))
			}
			endWithin(t, w)
			discarded.stopCount(t, 1, "after End")
		})
	}
}
