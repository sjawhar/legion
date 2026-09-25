package kvwatch_test

import (
	"context"
	"errors"
	"fmt"
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
	if err := w.Rewatch(liveConn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	live := w.KV()

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

// A first start that fails while a Rewatch's watcher is still scanning leaves readiness to that
// watcher: the cache is ready only once the current watcher has delivered every existing key, so a
// caller waiting for it never reads a partial cache as complete.
func TestAFirstStartFailingDuringARewatchScanWaitsForThatScan(t *testing.T) {
	_, uri := testnats.Start(t)
	firstConn, first := bucket(t, uri)
	liveConn, live := bucket(t, uri)
	if _, err := live.Put("held", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	firstConn.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseScan := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseScan)
	into := newSeen()
	apply := func(entry natsgo.KeyValueEntry) {
		if entry.Key() == "held" {
			enterOnce.Do(func() { close(entered) })
			<-release
		}
		into.apply(entry)
	}
	w := kvwatch.New("test cache", first, apply, into.reset)
	t.Cleanup(w.Stop)
	if err := w.Rewatch(liveConn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the Rewatch's scan never reached the held key")
	}

	w.Start()
	time.Sleep(300 * time.Millisecond)
	if w.Ready() {
		t.Fatal("the cache was ready while the current watcher's scan had not delivered every key")
	}
	releaseScan()
	eventually(t, "readiness after the current watcher's scan", w.Ready)
	if !into.has("held") {
		t.Fatal("ready without the held key")
	}
	if err := w.Err(); err != nil {
		t.Fatalf("the failed first start marked the live watcher dead: %v", err)
	}
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
	if err := w.Rewatch(liveConn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	live := w.KV()
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
	if err := w.Rewatch(secondConn); err != nil {
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
	if err := w.Rewatch(conn); err != nil {
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
	if err := w.Check(); err != nil || w.Err() != nil {
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
	if err := w.Check(); err != nil {
		t.Fatalf("Check on the recreated bucket: %v", err)
	}
	// The watcher may also have ended on its own by now, which records its own terminal error; either
	// way the recreated bucket reaches Err.
	if err := w.Err(); err == nil {
		t.Fatal("Check on a recreated bucket left Err nil, so self-health would never rebuild the watcher")
	}

	if err := w.Rewatch(conn); err != nil {
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
	if err := w.Rewatch(liveConn); err != nil {
		t.Fatalf("a Rewatch after Stop returned an error: %v", err)
	}
	live := w.KV()
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
//
// The first watcher's apply of late1 is held while late2..late41 queue in its updates. The Rewatch
// arms its watcher (the bucket's stream gains a consumer) and then waits for that apply, because
// the switch takes applyMu. Every other apply takes 10 ms, so once late1's is released the first
// watcher applies at most an entry or two before the Rewatch, blocked for far longer than
// sync.Mutex's 1 ms starvation threshold, is handed the lock; every entry the first watcher
// delivers after the switch must be dropped.
func TestAReplacedWatchersBufferedEntryIsDropped(t *testing.T) {
	_, uri := testnats.Start(t)
	firstConn, kv := bucket(t, uri)
	secondConn, _ := bucket(t, uri)
	js, err := firstConn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	consumers := func() int {
		info, err := js.StreamInfo("KV_kvwatch-test")
		if err != nil {
			t.Fatalf("stream info: %v", err)
		}
		return info.State.Consumers
	}

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
		} else {
			time.Sleep(10 * time.Millisecond)
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
	const queued = 40
	keys := make([]string, 0, queued)
	for i := range queued {
		key := fmt.Sprintf("late%d", i+2)
		keys = append(keys, key)
		if _, err := kv.Put(key, []byte("1")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	// The keys wait in the first watcher's updates while its apply of late1 is held.
	time.Sleep(300 * time.Millisecond)
	before := consumers()
	rewatched := make(chan error, 1)
	go func() {
		err := w.Rewatch(secondConn)
		rewatched <- err
	}()
	eventually(t, "the Rewatch's watcher", func() bool { return consumers() > before })
	time.Sleep(300 * time.Millisecond)
	releaseApply()
	select {
	case err := <-rewatched:
		if err != nil {
			t.Fatalf("Rewatch: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Rewatch never returned")
	}

	last := keys[len(keys)-1]
	eventually(t, "the new watcher's scan to apply "+last, func() bool { return count(last) > 0 })
	time.Sleep(300 * time.Millisecond)
	once := 0
	for _, key := range keys {
		switch got := count(key); got {
		case 1:
			once++
		case 2:
		default:
			t.Fatalf("%s applied %d times, want once by the new watcher's scan and at most once before the switch", key, got)
		}
	}
	if once == 0 {
		t.Fatalf("every one of %d queued keys was applied twice: the replaced watcher's buffered copies reached the cache", queued)
	}
}

// slowStopKV hands out watchers whose Stop waits until released, standing in for the consumer
// delete that nats.go's Unsubscribe sends for a library-created consumer.
type slowStopKV struct {
	natsgo.KeyValue
	entered, release chan struct{}
}

func (k *slowStopKV) WatchAll(opts ...natsgo.WatchOpt) (natsgo.KeyWatcher, error) {
	w, err := k.KeyValue.WatchAll(opts...)
	if err != nil {
		return nil, err
	}
	return &slowStopWatcher{KeyWatcher: w, entered: k.entered, release: k.release}, nil
}

type slowStopWatcher struct {
	natsgo.KeyWatcher
	entered, release chan struct{}
	once             sync.Once
}

func (w *slowStopWatcher) Stop() error {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.KeyWatcher.Stop()
}

// Two Rewatches overlap after the bucket was recreated -- the bus's reconnect hook and the
// listener's self-health rebuild both rewatch, and nothing serializes them. A sees the new stream
// and is still stopping the replaced watcher when B arms its own and applies the new bucket's scan.
// The new bucket's key stays in the cache, after exactly one reset: B's watcher never delivers it
// again, and Err stays nil, so nothing would repair a reset that ran after B's scan.
func TestConcurrentRewatchesOntoARecreatedBucketKeepTheNewKeys(t *testing.T) {
	_, uri := testnats.Start(t)
	conn, kv := bucket(t, uri)
	if _, err := kv.Put("ghost", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	slow := &slowStopKV{KeyValue: kv, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(slow.release) }) }
	t.Cleanup(release)
	into := newSeen()
	w := kvwatch.New("test cache", slow, into.apply, into.reset)
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

	doneA := make(chan error, 1)
	go func() { err := w.Rewatch(conn); doneA <- err }()
	select {
	case <-slow.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Rewatch A never stopped the replaced watcher")
	}
	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("Rewatch B: %v", err)
	}
	eventually(t, "B's scan", func() bool { return into.has("fresh") })
	release()
	if err := <-doneA; err != nil {
		t.Fatalf("Rewatch A: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if !into.has("fresh") || into.has("ghost") || into.resetCount() != 1 {
		t.Fatalf("fresh %v, ghost %v, resets %d, Err %v: want the new bucket's key kept after one reset",
			into.has("fresh"), into.has("ghost"), into.resetCount(), w.Err())
	}
}
