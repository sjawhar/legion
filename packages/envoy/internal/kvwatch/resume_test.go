package kvwatch_test

import (
	"context"
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

// A stream whose sequence space went backwards under an unchanged creation time holds no revision
// past its last sequence, so resuming after the revision the cache applied would skip the
// revisions the stream numbers again. The Rewatch reads the bucket in full instead; it is not a
// recreated bucket, so the cache is not emptied. A JetStream restore of an older snapshot leaves
// exactly that on a nats-server below v2.15, which keeps the snapshot's creation time; v2.15 gives
// the restored stream a creation time of its own
// (TestARewatchOntoAStreamRestoredOnThisServerReadsItInFullAfterAReset), so the state here is the
// stubbed one (kvwatchtest.StreamBehind) over a real bucket, and the branch stays for the hubs
// that still produce it.
func TestARewatchOntoAStreamBehindTheAppliedRevisionReadsItInFull(t *testing.T) {
	conn, kv := bucket(t, testnats.URL(t))
	putKeys(t, kv, "one", "two", "three")
	// The Rewatch opens the bucket from its connection, so the stubbed state is given there: the
	// real bucket, reporting the last sequence of the stream a restore of its first revision left.
	behind := bus.KeyValue{KeyValue: kvwatchtest.StreamBehind(kv, 1)}
	logger, records := jsonLogger(t)
	into := newMirror()
	w := kvwatch.New("test cache", kv, into.apply, into.reset, kvwatch.WithLogger(logger),
		kvwatch.WithBucketOpener(func(natsgo.JetStreamContext, string) (bus.KeyValue, error) { return behind, nil }))
	t.Cleanup(w.Stop)
	w.Start()
	requireWarmUp(t, records, 1, "completed", 3, 0)
	state, err := bus.ReadStreamState(behind)
	if err != nil {
		t.Fatalf("stream state: %v", err)
	}
	if state.LastSeq != 1 {
		t.Fatalf("the stubbed stream state reports last sequence %d, want the three applied revisions to sit past it", state.LastSeq)
	}

	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	// Read in full: all three keys again, where a resume after the third revision would have
	// applied none and said "resumed".
	requireWarmUp(t, records, 2, "completed", 3, 0)
	if values, resets := into.snapshot(); !maps.Equal(values, bucketValues(t, kv)) || resets != 0 {
		t.Fatalf("cache %v after %d resets, want the bucket %v and no reset", values, resets, bucketValues(t, kv))
	}
}

// What a JetStream restore of an older snapshot leaves on the server this module links: the
// restored stream carries a creation time of its own, later than the one the cache was filled
// from, and its last sequence is the snapshot's. The Rewatch reads the restored bucket in full,
// and because the creation time changed it empties the cache first, as it does for a recreated
// bucket, so the cache holds exactly what the restored stream holds.
func TestARewatchOntoAStreamRestoredOnThisServerReadsItInFullAfterAReset(t *testing.T) {
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
	if !after.Created.After(before.Created) || after.LastSeq != 1 {
		t.Fatalf("restored stream created %v with last sequence %d, want a creation time past the original's %v and 1", after.Created, after.LastSeq, before.Created)
	}

	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	requireWarmUp(t, records, 2, "completed", 1, 0)
	if values, resets := into.snapshot(); !maps.Equal(values, map[string]string{"one": "one"}) || resets != 1 {
		t.Fatalf("cache %v after %d resets, want the restored bucket's one key after one reset", values, resets)
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

// A Rewatch whose resuming watcher cannot be created repairs the watch all the same: the same
// watch falls back to the full read, logs one WARN naming the cache and the error, and leaves the
// cache equal to the bucket with no error recorded. The resume's consumer create is held to the
// legacy JetStream context's 5 s, under the 10 s the full read's own requests get, so a server
// slow enough to fail the one can still answer the other, where a failed Rewatch would leave the
// cache frozen behind a stopped watcher.
func TestARewatchWhoseResumeCannotBeCreatedReadsTheBucketInFull(t *testing.T) {
	conn, kv := bucket(t, testnats.URL(t))
	putKeys(t, kv, "a", "b", "c")
	logger, records := jsonLogger(t)
	into := newMirror()
	w := kvwatch.New("test cache", kv, into.apply, into.reset, kvwatch.WithLogger(logger),
		kvwatch.WithResumeOpener(func(bus.KeyValue, *natsgo.Conn, uint64) (natsgo.KeyWatcher, error) {
			// What a create the server does not answer within its wait leaves behind.
			return nil, fmt.Errorf("resume a watch of KV bucket %s: %w", kv.Bucket(), natsgo.ErrTimeout)
		}))
	t.Cleanup(w.Stop)
	w.Start()
	requireWarmUp(t, records, 1, "completed", 3, 0)
	putKeys(t, kv, "d")
	eventually(t, "the first watcher's delivery", func() bool { return into.appliedCount() == 4 })

	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	requireWarmUp(t, records, 2, "completed", 4, 0)
	if warned := matching(records(), "WARN", "test cache watcher could not resume; the bucket is read in full"); len(warned) != 1 {
		t.Fatalf("the failed resume logged %d WARN lines, want exactly one", len(warned))
	}
	if values, resets := into.snapshot(); !maps.Equal(values, bucketValues(t, kv)) || resets != 0 {
		t.Fatalf("cache %v after %d resets, want the bucket %v and no reset", values, resets, bucketValues(t, kv))
	}
	if err := w.Err(); err != nil {
		t.Fatalf("the repaired watcher recorded %v, want none", err)
	}
}

// endedWatcher is a resumed watch whose connection closes just after its end-of-scan marker: the
// cause of the end is on Error by the time the marker is read, which is how bus's resumedWatcher
// leaves it (its forward puts the cause there, closes Error, then closes Updates). It delivers
// the marker and then ends when the test says so.
type endedWatcher struct {
	updates chan natsgo.KeyValueEntry
	errs    chan error
	ending  chan struct{}
	once    sync.Once
}

func newEndedWatcher(cause error) *endedWatcher {
	w := &endedWatcher{
		updates: make(chan natsgo.KeyValueEntry),
		errs:    make(chan error, 1),
		ending:  make(chan struct{}),
	}
	w.errs <- cause
	go func() {
		w.updates <- nil
		<-w.ending
		close(w.errs)
		close(w.updates)
	}()
	return w
}

// end closes the watch, as the connection closing does.
func (w *endedWatcher) end() { w.once.Do(func() { close(w.ending) }) }

func (w *endedWatcher) Context() context.Context             { return context.Background() }
func (w *endedWatcher) Updates() <-chan natsgo.KeyValueEntry { return w.updates }
func (w *endedWatcher) Error() <-chan error                  { return w.errs }
func (w *endedWatcher) Stop() error                          { w.end(); return nil }

// A resumed watch has no idle timer, so a cause waiting on its Error channel at its marker is the
// end it is about to reach, never a scan nats.go gave up on. The warm-up line says it resumed, at
// INFO; the generation is complete, so the next Rewatch may resume after it; and the end records
// the closed connection rather than "<name> watcher stopped", which reading the cause here would
// have taken away from it.
func TestAResumedWatchThatEndsAtItsMarkerIsNotTimedOut(t *testing.T) {
	conn, kv := bucket(t, testnats.URL(t))
	putKeys(t, kv, "a")
	logger, records := jsonLogger(t)
	into := newMirror()
	ended := newEndedWatcher(fmt.Errorf("watch of KV bucket %s ended: %w", kv.Bucket(), natsgo.ErrConnectionClosed))
	t.Cleanup(ended.end)
	w := kvwatch.New("test cache", kv, into.apply, into.reset, kvwatch.WithLogger(logger),
		kvwatch.WithResumeOpener(func(bus.KeyValue, *natsgo.Conn, uint64) (natsgo.KeyWatcher, error) {
			return ended, nil
		}))
	t.Cleanup(w.Stop)
	w.Start()
	requireWarmUp(t, records, 1, "completed", 1, 0)

	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	requireWarmUp(t, records, 2, "resumed", 0, 0)
	if warned := matching(records(), "WARN", "test cache cache warm-up"); len(warned) != 0 {
		t.Fatalf("the resumed watch logged %d warm-up WARN lines, want none: %v", len(warned), warned)
	}
	eventually(t, "the resumed generation's completeness", func() bool {
		_, complete := w.Complete()
		return complete
	})

	ended.end()
	eventually(t, "the resumed watcher's recorded end", func() bool { return w.Err() != nil })
	if err := w.Err(); !errors.Is(err, natsgo.ErrConnectionClosed) {
		t.Fatalf("recorded %v, want the closed connection", err)
	}
}
