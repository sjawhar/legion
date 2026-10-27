package kvwatch_test

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/kvwatch/kvwatchtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// mirror is a cache the way the stores keep one: a put sets its key, a delete or purge marker
// removes it. It also logs every entry applied, as "<op> <key>@<revision>", so a test reads exactly
// what each generation applied.
type mirror struct {
	mu      sync.Mutex
	values  map[string]string
	applied []string
	resets  int
}

func newMirror() *mirror { return &mirror{values: map[string]string{}} }

func (m *mirror) apply(entry natsgo.KeyValueEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry.Operation() == natsgo.KeyValuePut {
		m.values[entry.Key()] = string(entry.Value())
	} else {
		delete(m.values, entry.Key())
	}
	m.applied = append(m.applied, fmt.Sprintf("%s %s@%d", entry.Operation(), entry.Key(), entry.Revision()))
}

func (m *mirror) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values = map[string]string{}
	m.resets++
}

// appliedSince is what was applied after the first n entries.
func (m *mirror) appliedSince(n int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.applied[n:])
}

func (m *mirror) appliedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.applied)
}

func (m *mirror) snapshot() (map[string]string, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.values), m.resets
}

// bucketValues is every live key of kv's bucket and its value.
func bucketValues(t *testing.T, kv bus.KeyValue) map[string]string {
	t.Helper()
	values := map[string]string{}
	keys, err := kv.ListKeys()
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	for key := range keys.Keys() {
		entry, err := kv.Get(key)
		if err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		values[key] = string(entry.Value())
	}
	return values
}

// requireWarmUp waits for the "test cache" watcher's nth warm-up line (1 is the first start's) and
// fails the test unless it says outcome after entries puts and markers delete or purge markers.
func requireWarmUp(t *testing.T, records func() []map[string]any, n int, outcome string, entries, markers int) {
	t.Helper()
	var lines []map[string]any
	eventually(t, fmt.Sprintf("warm-up line %d", n), func() bool {
		lines = lines[:0]
		for _, r := range records() {
			if r["msg"] == "test cache cache warm-up" {
				lines = append(lines, r)
			}
		}
		return len(lines) >= n
	})
	line := lines[n-1]
	if line["outcome"] != outcome || line["entries"] != float64(entries) || line["delete_markers"] != float64(markers) {
		t.Fatalf("warm-up line %d outcome=%v entries=%v delete_markers=%v, want %s, %d and %d: %v",
			n, line["outcome"], line["entries"], line["delete_markers"], outcome, entries, markers, line)
	}
}

// putKeys puts each key with its own name as the value.
func putKeys(t *testing.T, kv bus.KeyValue, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, err := kv.Put(key, []byte(key)); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
}

// startWatching starts a watcher of kv into a mirror, logging through a JSON logger, and waits for
// its first scan to complete with entries keys.
func startWatching(t *testing.T, kv bus.KeyValue, entries int) (*kvwatch.Watcher, *mirror, func() []map[string]any) {
	t.Helper()
	logger, records := jsonLogger(t)
	into := newMirror()
	w := kvwatch.New("test cache", kv, into.apply, into.reset, kvwatch.WithLogger(logger))
	t.Cleanup(w.Stop)
	w.Start()
	requireWarmUp(t, records, 1, "completed", entries, 0)
	return w, into, records
}

// A Rewatch after a first scan that completed, onto the same bucket, resumes after the last
// revision it applied: with nothing written since, it applies nothing, and its warm-up line says it
// resumed. A rewatch of a large bucket then costs what changed, not the bucket.
func TestARewatchWithNoWritesResumesAndAppliesNothing(t *testing.T) {
	conn, kv := bucket(t, testnats.URL(t))
	putKeys(t, kv, "a", "b", "c")
	w, into, records := startWatching(t, kv, 3)

	before := into.appliedCount()
	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	requireWarmUp(t, records, 2, "resumed", 0, 0)
	if applied := into.appliedSince(before); len(applied) != 0 {
		t.Fatalf("a rewatch with no writes applied %v, want nothing", applied)
	}

	// The resumed watcher follows the bucket as a full one does.
	putKeys(t, w.KV(), "d")
	eventually(t, "the resumed watcher's delivery", func() bool { return len(into.appliedSince(before)) == 1 })
	if values, resets := into.snapshot(); !maps.Equal(values, bucketValues(t, kv)) || resets != 0 {
		t.Fatalf("cache %v after %d resets, want the bucket %v and no reset", values, resets, bucketValues(t, kv))
	}
}

// A watcher whose connection closed misses every write made until a Rewatch replaces it. The
// Rewatch on the replacement connection applies exactly what was written meanwhile, puts, a delete
// and a purge, and nothing it had already applied, and the cache then equals the bucket.
func TestARewatchAfterALostConnectionAppliesOnlyWhatWasWritten(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	putKeys(t, kv, "kept", "deleted", "purged", "updated")
	w, into, records := startWatching(t, kv, 4)

	conn.Close()
	eventually(t, "the ended watcher's error", func() bool { return w.Err() != nil })
	before := into.appliedCount()
	liveConn, live := bucket(t, uri)
	putKeys(t, live, "new", "updated")
	if err := live.Delete("deleted"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := live.Purge("purged"); err != nil {
		t.Fatalf("purge: %v", err)
	}

	if err := w.Rewatch(liveConn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	requireWarmUp(t, records, 2, "resumed", 2, 2)
	want := []string{"KeyValuePutOp new@5", "KeyValuePutOp updated@6", "KeyValueDeleteOp deleted@7", "KeyValuePurgeOp purged@8"}
	if applied := into.appliedSince(before); !slices.Equal(applied, want) {
		t.Fatalf("the rewatch applied %v, want exactly %v", applied, want)
	}
	if values, resets := into.snapshot(); !maps.Equal(values, bucketValues(t, live)) || resets != 0 {
		t.Fatalf("cache %v after %d resets, want the bucket %v and no reset", values, resets, bucketValues(t, live))
	}
	if err := w.Err(); err != nil {
		t.Fatalf("the resumed watcher kept the old one's error: %v", err)
	}
}

// A recreated bucket numbers its revisions from 1 again, so nothing applied from the old one says
// where to resume in it: the Rewatch empties the cache and reads the new bucket in full.
func TestARewatchOntoARecreatedBucketReadsItInFull(t *testing.T) {
	conn, kv := bucket(t, testnats.URL(t))
	putKeys(t, kv, "ghost", "other")
	w, into, records := startWatching(t, kv, 2)

	js := jetStream(t, conn)
	if err := js.DeleteKeyValue("kvwatch-test"); err != nil {
		t.Fatalf("delete bucket: %v", err)
	}
	recreated := bus.KeyValue{KeyValue: testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: "kvwatch-test"})}
	putKeys(t, recreated, "fresh")
	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	requireWarmUp(t, records, 2, "completed", 1, 0)
	if values, resets := into.snapshot(); !maps.Equal(values, bucketValues(t, recreated)) || resets != 1 {
		t.Fatalf("cache %v after %d resets, want the recreated bucket %v after one reset", values, resets, bucketValues(t, recreated))
	}
}

// A stream whose sequence space went backwards under an unchanged creation time -- a JetStream
// restore of an older snapshot keeps the snapshot's -- holds no revision past its last sequence, so
// resuming after the revision the cache applied would skip the revisions the stream numbers again.
// The Rewatch reads the bucket in full instead, as before; it is not a recreated bucket, so the
// cache is not emptied.
func TestARewatchOntoAStreamBehindTheAppliedRevisionReadsItInFull(t *testing.T) {
	_, uri := testnats.Start(t)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js := jetStream(t, conn)
	// The keeper keeps the account's streams directory from being removed under the restore
	// (TestARewatchOntoABucketRestoredWithAnOlderStreamRefillsTheCache says why).
	if _, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "kvwatch-test-account-keeper"}); err != nil {
		t.Fatalf("create the account keeper bucket: %v", err)
	}
	created, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "kvwatch-test"})
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	kv := bus.KeyValue{KeyValue: created}
	putKeys(t, kv, "one")
	snap := snapshotStream(t, conn, "KV_kvwatch-test")
	putKeys(t, kv, "two", "three")
	w, into, records := startWatching(t, kv, 3)
	before, err := bus.ReadStreamState(kv)
	if err != nil {
		t.Fatalf("stream state: %v", err)
	}

	if err := js.DeleteKeyValue("kvwatch-test"); err != nil {
		t.Fatalf("delete the bucket: %v", err)
	}
	restoreStream(t, conn, "KV_kvwatch-test", snap)
	after, err := bus.ReadStreamState(kv)
	if err != nil {
		t.Fatalf("stream state after the restore: %v", err)
	}
	if !after.Created.Equal(before.Created) || after.LastSeq != 1 {
		t.Fatalf("restored stream created %v with last sequence %d, want the original's %v and 1", after.Created, after.LastSeq, before.Created)
	}

	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	requireWarmUp(t, records, 2, "completed", 1, 0)
	if values, resets := into.snapshot(); values["one"] != "one" || resets != 0 {
		t.Fatalf("cache %v after %d resets, want one read again and no reset", values, resets)
	}
}

// A first scan that timed out left the cache short of the bucket, so the revision it applied up to
// says nothing about the keys it never delivered: the next Rewatch reads the bucket in full. Once
// that read completes, the one after it resumes.
func TestARewatchAfterATimedOutFirstScanReadsInFull(t *testing.T) {
	conn, kv := bucket(t, testnats.URL(t))
	putKeys(t, kv, "a", "b", "c")
	logger, records := jsonLogger(t)
	into := newMirror()
	w := kvwatch.New("test cache", bus.KeyValue{KeyValue: kvwatchtest.TimingOut(kv, 1)}, into.apply, into.reset, kvwatch.WithLogger(logger))
	t.Cleanup(w.Stop)
	w.Start()
	requireWarmUp(t, records, 1, "timed out", 1, 0)
	// The timed-out watcher goes on to deliver the rest, so the revision it applied up to is the
	// bucket's last: only the timeout says the scan was short.
	eventually(t, "the rest of the timed-out scan", func() bool { return into.appliedCount() == 3 })

	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	requireWarmUp(t, records, 2, "completed", 3, 0)

	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("second rewatch: %v", err)
	}
	requireWarmUp(t, records, 3, "resumed", 0, 0)
}

// A resumed watcher whose connection closes ends on its own like any other: its end is recorded as
// the watcher's error, naming the closed connection, and logged, so /healthz and the self-health
// rebuild see the cache is frozen rather than a watcher that hangs with nothing reporting it.
func TestAResumedWatcherWhoseConnectionClosesRecordsItsEnd(t *testing.T) {
	conn, kv := bucket(t, testnats.URL(t))
	putKeys(t, kv, "a")
	w, _, records := startWatching(t, kv, 1)
	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	requireWarmUp(t, records, 2, "resumed", 0, 0)

	conn.Close()
	eventually(t, "the resumed watcher's recorded end", func() bool { return w.Err() != nil })
	if err := w.Err(); !errors.Is(err, natsgo.ErrConnectionClosed) {
		t.Fatalf("recorded %v, want the closed connection", err)
	}
	eventually(t, "the stop's WARN", func() bool { return len(matching(records(), "WARN", "test cache watcher stopped")) == 1 })
}
