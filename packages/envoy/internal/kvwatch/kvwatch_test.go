package kvwatch_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// seen records the keys a watcher delivered and how many times the cache was reset.
type seen struct {
	mu     sync.Mutex
	keys   map[string]bool
	resets int
}

func newSeen() *seen { return &seen{keys: map[string]bool{}} }

func (s *seen) apply(entry natsgo.KeyValueEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[entry.Key()] = true
}

func (s *seen) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = map[string]bool{}
	s.resets++
}

func (s *seen) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys[key]
}

func (s *seen) resetCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resets
}

// bucket opens the test bucket on its own connection, which the test can close to end a watcher
// the way a lost connection does.
func bucket(t *testing.T, uri string) (*natsgo.Conn, natsgo.KeyValue) {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.KeyValue("kvwatch-test")
	if err != nil {
		kv, err = js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "kvwatch-test"})
	}
	if err != nil {
		t.Fatalf("bucket: %v", err)
	}
	return conn, kv
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The cache is ready once the first watcher has delivered every existing key, and not before.
func TestTheCacheIsReadyAfterTheFirstScan(t *testing.T) {
	_, uri := testnats.Start(t)
	_, kv := bucket(t, uri)
	if _, err := kv.Put("before-start", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	into := newSeen()
	w := kvwatch.New("test cache", kv, into.apply, into.reset)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := w.WaitReady(ctx); !errors.Is(err, context.DeadlineExceeded) || w.Ready() {
		t.Fatalf("WaitReady before Start = %v (ready %v), want the context's deadline", err, w.Ready())
	}

	w.Start()
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := w.WaitReady(ctx)
		cancel()
		if err != nil {
			t.Fatalf("WaitReady after Start: %v", err)
		}
	}
	if !w.Ready() || !into.has("before-start") {
		t.Fatalf("ready %v with the existing key applied %v, want both", w.Ready(), into.has("before-start"))
	}
}

// A first start that fails releases readiness and records its error, so a caller waiting for the
// cache sees an empty cache that /healthz reports rather than a hang.
func TestAFailedFirstStartIsReadyAndRecordsItsError(t *testing.T) {
	_, uri := testnats.Start(t)
	conn, kv := bucket(t, uri)
	conn.Close()
	w := kvwatch.New("test cache", kv, newSeen().apply, func() {})

	w.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady after a failed first start: %v", err)
	}
	if w.Err() == nil {
		t.Fatal("a failed first start recorded no error")
	}
}

// A first start that fails after a Rewatch has armed a live watcher records nothing: the error is
// the first start's, not the current watcher's, and recording it would mark a live cache dead.
func TestAFirstStartFailingAfterARewatchLeavesTheLiveWatcherHealthy(t *testing.T) {
	_, uri := testnats.Start(t)
	firstConn, first := bucket(t, uri)
	liveConn, _ := bucket(t, uri)
	firstConn.Close()
	into := newSeen()
	w := kvwatch.New("test cache", first, into.apply, into.reset)
	live, err := w.Rewatch(liveConn)
	if err != nil {
		t.Fatalf("rewatch: %v", err)
	}

	w.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := w.Err(); err != nil {
		t.Fatalf("the failed first start marked the live watcher dead: %v", err)
	}
	if _, err := live.Put("after-start", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	eventually(t, "the live watcher's delivery", func() bool { return into.has("after-start") })
}

// A watcher that ends on its own - its connection closed - leaves the cache frozen, and Err says so
// until a Rewatch replaces it.
func TestAWatcherThatEndsOnItsOwnRecordsItsError(t *testing.T) {
	_, uri := testnats.Start(t)
	conn, kv := bucket(t, uri)
	into := newSeen()
	w := kvwatch.New("test cache", kv, into.apply, into.reset)
	w.Start()
	eventually(t, "the first scan", w.Ready)
	conn.Close()
	eventually(t, "the ended watcher's error", func() bool { return w.Err() != nil })

	liveConn, _ := bucket(t, uri)
	live, err := w.Rewatch(liveConn)
	if err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	if err := w.Err(); err != nil {
		t.Fatalf("a replacement watcher kept the old one's error: %v", err)
	}
	if _, err := live.Put("after-rewatch", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	eventually(t, "the replacement watcher's delivery", func() bool { return into.has("after-rewatch") })
	if into.resetCount() != 0 {
		t.Fatalf("a rewatch onto the same bucket reset the cache %d times", into.resetCount())
	}
}

// A watcher Rewatch replaced ends without recording anything: only the current watcher's end is
// the cache's.
func TestAReplacedWatcherEndingRecordsNothing(t *testing.T) {
	_, uri := testnats.Start(t)
	firstConn, first := bucket(t, uri)
	secondConn, _ := bucket(t, uri)
	w := kvwatch.New("test cache", first, newSeen().apply, func() {})
	w.Start()
	eventually(t, "the first scan", w.Ready)
	if _, err := w.Rewatch(secondConn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	firstConn.Close()
	time.Sleep(500 * time.Millisecond)
	if err := w.Err(); err != nil {
		t.Fatalf("the replaced watcher's end was recorded: %v", err)
	}
}

// A bucket deleted and created again numbers its revisions from 1, so a cache fenced by the old
// revisions would drop the new bucket's entries and keep keys it no longer holds. A Rewatch onto
// the recreated bucket resets the cache before the new watcher fills it.
func TestARewatchOntoARecreatedBucketResetsTheCache(t *testing.T) {
	_, uri := testnats.Start(t)
	conn, kv := bucket(t, uri)
	if _, err := kv.Put("ghost", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	into := newSeen()
	w := kvwatch.New("test cache", kv, into.apply, into.reset)
	w.Start()
	eventually(t, "the ghost key", func() bool { return into.has("ghost") })

	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue("kvwatch-test"); err != nil {
		t.Fatalf("delete bucket: %v", err)
	}
	recreated, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "kvwatch-test"})
	if err != nil {
		t.Fatalf("recreate bucket: %v", err)
	}
	if _, err := recreated.Put("fresh", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	eventually(t, "the recreated bucket's key", func() bool { return into.has("fresh") })
	if into.has("ghost") || into.resetCount() != 1 {
		t.Fatalf("after the rewatch: ghost kept %v, resets %d, want the ghost gone after one reset", into.has("ghost"), into.resetCount())
	}
}

// A bucket deleted and created again while its watcher runs need not end the watcher: nats.go's
// ordered consumer can reset onto the new stream and go on without delivering its first revisions,
// so the cache stays frozen behind a live watcher. Check, the store's health probe, reports it as
// the watcher's terminal error, and the Rewatch the listener then runs resets the cache.
func TestCheckReportsABucketRecreatedUnderALiveWatcher(t *testing.T) {
	_, uri := testnats.Start(t)
	conn, kv := bucket(t, uri)
	if _, err := kv.Put("ghost", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	into := newSeen()
	w := kvwatch.New("test cache", kv, into.apply, into.reset)
	w.Start()
	eventually(t, "the ghost key", func() bool { return into.has("ghost") })
	if err := w.Check(kv); err != nil || w.Err() != nil {
		t.Fatalf("Check on the watched bucket = %v, Err %v, want both nil", err, w.Err())
	}

	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue("kvwatch-test"); err != nil {
		t.Fatalf("delete bucket: %v", err)
	}
	if _, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "kvwatch-test"}); err != nil {
		t.Fatalf("recreate bucket: %v", err)
	}
	if err := w.Check(kv); err != nil {
		t.Fatalf("Check on the recreated bucket: %v", err)
	}
	// The watcher may also have ended on its own by now, which records its own terminal error; either
	// way the recreated bucket reaches Err.
	if err := w.Err(); err == nil {
		t.Fatal("Check on a recreated bucket left Err nil, so self-health would never rebuild the watcher")
	}

	if _, err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	if err := w.Err(); err != nil {
		t.Fatalf("Err after the rewatch = %v, want nil", err)
	}
	if into.has("ghost") || into.resetCount() != 1 {
		t.Fatalf("after the rewatch: ghost kept %v, resets %d, want the ghost gone after one reset", into.has("ghost"), into.resetCount())
	}
}

// After Stop, the current watcher's end records nothing and a later Rewatch arms nothing, so a
// reconnect hook still running at shutdown neither reports an error nor starts a watcher.
func TestStopMakesTheEndSilentAndRewatchANoOp(t *testing.T) {
	_, uri := testnats.Start(t)
	conn, kv := bucket(t, uri)
	into := newSeen()
	w := kvwatch.New("test cache", kv, into.apply, into.reset)
	w.Start()
	eventually(t, "the first scan", w.Ready)
	w.Stop()
	conn.Close()
	time.Sleep(500 * time.Millisecond)
	if err := w.Err(); err != nil {
		t.Fatalf("a stopped watcher's end was recorded: %v", err)
	}

	liveConn, _ := bucket(t, uri)
	live, err := w.Rewatch(liveConn)
	if err != nil {
		t.Fatalf("a Rewatch after Stop returned an error: %v", err)
	}
	if _, err := live.Put("after-stop", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if into.has("after-stop") {
		t.Fatal("a Rewatch after Stop armed a watcher")
	}
}

// An entry still buffered in a replaced watcher's updates never reaches the cache: once a Rewatch
// has replaced the watcher, its late deliveries are dropped and the new watcher's scan delivers
// each key once. Without that, a watcher replaced for a recreated bucket would apply old-bucket
// entries after the reset that emptied the cache for the new one.
func TestAReplacedWatchersBufferedEntryIsDropped(t *testing.T) {
	_, uri := testnats.Start(t)
	_, kv := bucket(t, uri)
	secondConn, _ := bucket(t, uri)

	var mu sync.Mutex
	counts := map[string]int{}
	count := func(key string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[key]
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var blockOnce, releaseOnce sync.Once
	releaseApply := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseApply)
	apply := func(entry natsgo.KeyValueEntry) {
		if entry.Key() == "late1" {
			blocked := false
			blockOnce.Do(func() { blocked = true })
			if blocked {
				close(entered)
				<-release
			}
		}
		mu.Lock()
		counts[entry.Key()]++
		mu.Unlock()
	}

	w := kvwatch.New("test cache", kv, apply, func() {})
	w.Start()
	t.Cleanup(w.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if _, err := kv.Put("late1", []byte("1")); err != nil {
		t.Fatalf("put late1: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first watcher never applied late1")
	}
	// late2 waits in the first watcher's updates while its apply of late1 is held.
	if _, err := kv.Put("late2", []byte("2")); err != nil {
		t.Fatalf("put late2: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := w.Rewatch(secondConn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	releaseApply()

	eventually(t, "the new watcher's scan to apply late2", func() bool { return count("late2") > 0 })
	time.Sleep(300 * time.Millisecond)
	if got := count("late2"); got != 1 {
		t.Fatalf("late2 applied %d times, want once: the replaced watcher's buffered copy reached the cache", got)
	}
}
