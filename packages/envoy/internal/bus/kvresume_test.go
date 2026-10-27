package bus_test

import (
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// resumeBucket opens a fresh bucket on its own connection, which a test can close.
func resumeBucket(t *testing.T) (*natsgo.Conn, bus.KeyValue) {
	t.Helper()
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := bus.EnsureKeyValue(js, &natsgo.KeyValueConfig{Bucket: "resume-test"})
	if err != nil {
		t.Fatalf("bucket: %v", err)
	}
	return conn, kv
}

// nextUpdate is the watcher's next delivery, failing the test when none comes or Updates closes.
func nextUpdate(t *testing.T, watcher natsgo.KeyWatcher) natsgo.KeyValueEntry {
	t.Helper()
	select {
	case entry, open := <-watcher.Updates():
		if !open {
			t.Fatal("Updates closed")
		}
		return entry
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery within 5s")
		return nil
	}
}

// awaitClosed waits for Updates to close, failing the test on an entry or when it stays open.
func awaitClosed(t *testing.T, watcher natsgo.KeyWatcher) {
	t.Helper()
	select {
	case entry, open := <-watcher.Updates():
		if open {
			t.Fatalf("delivered %v, want Updates closed", entry)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Updates still open 5s after the watch ended: the watcher hangs")
	}
}

// A resumed watch delivers every message the bucket's stream holds from its revision on, each with
// its key, value, revision and operation as the legacy API names them (a delete marker as
// KeyValueDelete, a purge marker as KeyValuePurge), then the nil marker, then later writes.
func TestAResumedWatchDeliversFromItsRevisionWithLegacyOperations(t *testing.T) {
	conn, kv := resumeBucket(t)
	if _, err := kv.Put("kept", []byte("k")); err != nil {
		t.Fatalf("put kept: %v", err)
	}
	if _, err := kv.Put("deleted", []byte("d")); err != nil {
		t.Fatalf("put deleted: %v", err)
	}
	if _, err := kv.Put("purged", []byte("p")); err != nil {
		t.Fatalf("put purged: %v", err)
	}
	if err := kv.Delete("deleted"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := kv.Purge("purged"); err != nil {
		t.Fatalf("purge: %v", err)
	}

	type delivery struct {
		key      string
		value    string
		revision uint64
		op       natsgo.KeyValueOp
	}
	// One revision per key: the stream holds kept's put (1), deleted's marker (4) and purged's (5).
	for _, test := range []struct {
		from uint64
		want []delivery
	}{
		{from: 1, want: []delivery{
			{"kept", "k", 1, natsgo.KeyValuePut},
			{"deleted", "", 4, natsgo.KeyValueDelete},
			{"purged", "", 5, natsgo.KeyValuePurge},
		}},
		{from: 2, want: []delivery{
			{"deleted", "", 4, natsgo.KeyValueDelete},
			{"purged", "", 5, natsgo.KeyValuePurge},
		}},
		{from: 5, want: []delivery{
			{"purged", "", 5, natsgo.KeyValuePurge},
		}},
	} {
		watcher, err := kv.WatchAllFrom(conn, test.from)
		if err != nil {
			t.Fatalf("from %d: WatchAllFrom: %v", test.from, err)
		}
		for _, want := range test.want {
			entry := nextUpdate(t, watcher)
			if entry == nil {
				t.Fatalf("from %d: nil marker before %s", test.from, want.key)
			}
			got := delivery{entry.Key(), string(entry.Value()), entry.Revision(), entry.Operation()}
			if got != want || entry.Bucket() != "resume-test" {
				t.Fatalf("from %d: delivered %+v of bucket %q, want %+v of resume-test", test.from, got, entry.Bucket(), want)
			}
		}
		if entry := nextUpdate(t, watcher); entry != nil {
			t.Fatalf("from %d: delivered %s at %d after the pending messages, want the nil marker", test.from, entry.Key(), entry.Revision())
		}
		if err := watcher.Stop(); err != nil {
			t.Fatalf("from %d: stop: %v", test.from, err)
		}
	}

	// A resume past the last revision has nothing pending: the marker comes at once, then the next
	// write.
	watcher, err := kv.WatchAllFrom(conn, 6)
	if err != nil {
		t.Fatalf("WatchAllFrom past the last revision: %v", err)
	}
	t.Cleanup(func() { _ = watcher.Stop() })
	if watcher.Context() == nil {
		t.Fatal("Context is nil")
	}
	if entry := nextUpdate(t, watcher); entry != nil {
		t.Fatalf("a resume past the last revision delivered %s at %d, want the nil marker", entry.Key(), entry.Revision())
	}
	if _, err := kv.Put("after", []byte("a")); err != nil {
		t.Fatalf("put after: %v", err)
	}
	if entry := nextUpdate(t, watcher); entry == nil || entry.Key() != "after" || entry.Revision() != 6 || entry.Operation() != natsgo.KeyValuePut {
		t.Fatalf("delivered %v after the marker, want after's put at 6", entry)
	}
}

// A resumed watch whose connection closes ends: Updates closes, after Error has delivered the cause,
// so a reader that finds Updates closed reads why there, as kvwatch does. A watch that never ended
// would leave a cache frozen with nothing reporting it.
func TestAResumedWatchEndsWithAnErrorWhenItsConnectionCloses(t *testing.T) {
	conn, kv := resumeBucket(t)
	if _, err := kv.Put("key", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	watcher, err := kv.WatchAllFrom(conn, 1)
	if err != nil {
		t.Fatalf("WatchAllFrom: %v", err)
	}
	if entry := nextUpdate(t, watcher); entry == nil || entry.Key() != "key" {
		t.Fatalf("delivered %v, want key", entry)
	}
	if entry := nextUpdate(t, watcher); entry != nil {
		t.Fatalf("delivered %s, want the nil marker", entry.Key())
	}

	conn.Close()
	awaitClosed(t, watcher)
	select {
	case err, open := <-watcher.Error():
		if !open || !errors.Is(err, natsgo.ErrConnectionClosed) {
			t.Fatalf("Error delivered %v (open %v), want the closed connection", err, open)
		}
	default:
		t.Fatal("Updates closed with no error waiting on Error")
	}
	if _, open := <-watcher.Error(); open {
		t.Fatal("Error delivered a second error")
	}
}

// A resumed watch stopped on purpose ends with no error: the end is its caller's.
func TestAStoppedResumedWatchEndsWithNoError(t *testing.T) {
	conn, kv := resumeBucket(t)
	if _, err := kv.Put("key", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	watcher, err := kv.WatchAllFrom(conn, 2)
	if err != nil {
		t.Fatalf("WatchAllFrom: %v", err)
	}
	if entry := nextUpdate(t, watcher); entry != nil {
		t.Fatalf("delivered %s, want the nil marker", entry.Key())
	}
	if err := watcher.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	awaitClosed(t, watcher)
	if err, open := <-watcher.Error(); open {
		t.Fatalf("a stopped watch delivered %v on Error", err)
	}
}

// Revision 0 is no revision: nats.go would read it as no resume at all and replay the bucket in
// full, so WatchAllFrom refuses it rather than do what the caller did not ask for.
func TestAResumeFromRevisionZeroIsRefused(t *testing.T) {
	conn, kv := resumeBucket(t)
	if watcher, err := kv.WatchAllFrom(conn, 0); err == nil {
		_ = watcher.Stop()
		t.Fatal("WatchAllFrom(0) opened a watch")
	}
}
