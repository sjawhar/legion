package kvwatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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

// lockedInWatch matches a sync.Mutex.Lock frame whose caller is watch itself. Inside watch that is
// only applyMu: a wait on w.mu shows sync.(*RWMutex).Lock as the caller, and one on nats.go's
// connection mutex, further down the call tree (streamCreated, WatchAll), shows nats.go frames.
var lockedInWatch = regexp.MustCompile(`sync\.\(\*Mutex\)\.Lock\([^\n]*\n\t[^\n]*\n[^\n]*kvwatch\.\(\*Watcher\)\.watch\(`)

// waitingForTheApplyLock reports whether a watch is parked on applyMu: a goroutine whose wait reason
// is sync.Mutex.Lock and whose Lock frame (*Watcher).watch called. A test that holds an apply open
// waits for this before it releases the apply, so the watch is already queued for the lock the
// release frees.
func waitingForTheApplyLock() bool {
	buf := make([]byte, 1<<22)
	buf = buf[:runtime.Stack(buf, true)]
	for _, goroutine := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(goroutine, "[sync.Mutex.Lock") && lockedInWatch.MatchString(goroutine) {
			return true
		}
	}
	return false
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
	uri := testnats.URL(t)
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
	uri := testnats.URL(t)
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
	uri := testnats.URL(t)
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
	uri := testnats.URL(t)
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
	uri := testnats.URL(t)
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
	uri := testnats.URL(t)
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
	uri := testnats.URL(t)
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
	uri := testnats.URL(t)
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
// arms its watcher and then waits for that apply, because the switch takes applyMu, and the test
// releases late1 only once the Rewatch is parked on that lock. Every other apply takes 10 ms, so
// once late1's is released the first watcher applies at most an entry or two before the Rewatch,
// blocked for longer than sync.Mutex's 1 ms starvation threshold, is handed the lock; every entry
// the first watcher delivers after the switch must be dropped.
func TestAReplacedWatchersBufferedEntryIsDropped(t *testing.T) {
	uri := testnats.URL(t)
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
	rewatched := make(chan error, 1)
	go func() {
		err := w.Rewatch(secondConn)
		rewatched <- err
	}()
	eventually(t, "the Rewatch waiting for the held apply", waitingForTheApplyLock)
	releaseApply()
	select {
	case err := <-rewatched:
		if err != nil {
			t.Fatalf("Rewatch: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Rewatch never returned")
	}
	// The premise the lock wait sets up: the switch came while the first watcher still held most of
	// its queue. Read at the switch, since the new watcher's scan takes 40 x 10 ms to apply every key
	// again; if the wait released late1 before the Rewatch was parked on applyMu, the first watcher
	// applied the whole queue first and nothing below would test the dropped entries.
	appliedBeforeSwitch := 0
	for _, key := range keys {
		if count(key) > 0 {
			appliedBeforeSwitch++
		}
	}
	if appliedBeforeSwitch == queued {
		t.Fatalf("all %d queued keys were applied before the Rewatch switched: the lock wait did not hold late1 until the Rewatch was parked on applyMu", queued)
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
	uri := testnats.URL(t)
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

// heldWatchKV starts its WatchAll on the bucket, then holds it until released: the watch it
// serves has read the stream and armed its watcher, and has not yet taken the locks.
type heldWatchKV struct {
	natsgo.KeyValue
	watched chan struct{}
	release chan struct{}
	watcher natsgo.KeyWatcher
}

func (k *heldWatchKV) WatchAll(opts ...natsgo.WatchOpt) (natsgo.KeyWatcher, error) {
	watcher, err := k.KeyValue.WatchAll(opts...)
	k.watcher = watcher
	close(k.watched)
	<-k.release
	return watcher, err
}

// A watch reads its stream before it arms its watcher and takes the locks, so a slower watch can
// arrive after a newer one has already switched to a recreated bucket. The slower watch read the
// old stream and armed its watcher there; installing it would reset the cache the newer watcher
// filled and feed it the old bucket's keys. It is discarded, and the newer watcher stays current.
func TestAWatchOfTheOldStreamDoesNotReplaceANewerOne(t *testing.T) {
	_, uri := testnats.Start(t)
	conn, first := bucket(t, uri)
	if _, err := first.Put("ghost", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	held := &heldWatchKV{KeyValue: first, watched: make(chan struct{}), release: make(chan struct{})}
	released := false
	t.Cleanup(func() {
		if !released {
			close(held.release)
		}
	})
	into := newSeen()
	w := kvwatch.New("test cache", held, into.apply, into.reset)
	w.Start()
	select {
	case <-held.watched:
	case <-time.After(5 * time.Second):
		t.Fatal("the first watch never armed its watcher")
	}

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
	liveConn, _ := bucket(t, uri)
	if err := w.Rewatch(liveConn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	eventually(t, "the newer watcher's scan", func() bool { return into.has("fresh") })

	close(held.release)
	released = true
	time.Sleep(500 * time.Millisecond)
	if into.has("ghost") || !into.has("fresh") {
		t.Fatalf("after the old stream's watch arrived: ghost %v, fresh %v, resets %d; want the recreated bucket's keys only",
			into.has("ghost"), into.has("fresh"), into.resetCount())
	}
	if err := w.Check(); err != nil || w.Err() != nil {
		t.Fatalf("Check = %v, Err %v; want the newer watcher current and healthy", err, w.Err())
	}
	if _, err := w.KV().Put("after", []byte("1")); err != nil {
		t.Fatalf("put through the handle: %v", err)
	}
	eventually(t, "a write through the current handle", func() bool { return into.has("after") })
}

// A discarded watch's watcher is read until it ends. nats.go delivers into a watcher's 256-entry
// Updates buffer and blocks when it is full, and Stop only unsubscribes, so a watcher of an old
// bucket holding more keys than that, stopped with nothing reading it, keeps its delivery goroutine
// parked for the life of the process.
func TestADiscardedWatchOfTheOldStreamIsDrainedUntilItEnds(t *testing.T) {
	_, uri := testnats.Start(t)
	conn, first := bucket(t, uri)
	for i := range 400 {
		if _, err := first.Put(fmt.Sprintf("old-%d", i), []byte("1")); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	held := &heldWatchKV{KeyValue: first, watched: make(chan struct{}), release: make(chan struct{})}
	released := false
	t.Cleanup(func() {
		if !released {
			close(held.release)
		}
	})
	into := newSeen()
	w := kvwatch.New("test cache", held, into.apply, into.reset)
	w.Start()
	select {
	case <-held.watched:
	case <-time.After(5 * time.Second):
		t.Fatal("the first watch never armed its watcher")
	}
	updates := held.watcher.Updates()
	eventually(t, "the held watcher's buffer to fill", func() bool { return len(updates) == cap(updates) })

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
	liveConn, _ := bucket(t, uri)
	if err := w.Rewatch(liveConn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	close(held.release)
	released = true

	ended := held.watcher.Error()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, open := <-ended:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("the discarded watcher never ended: it was left unread (its delivery goroutine parks on a full buffer) or never stopped")
		}
	}
}

// Readiness is the current watcher's to release. A watcher a Rewatch replaced mid-scan still reads
// its own sentinel, and releasing readiness then would let a caller read a cache the current
// watcher has only partly filled.
func TestAReplacedWatchersSentinelDoesNotReleaseReadiness(t *testing.T) {
	uri := testnats.URL(t)
	_, first := bucket(t, uri)
	for i := range 10 {
		if _, err := first.Put(fmt.Sprintf("k%03d", i), []byte("1")); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var slow atomic.Bool
	var k0 atomic.Int32
	into := newSeen()
	apply := func(entry natsgo.KeyValueEntry) {
		if entry.Key() == "k000" && k0.Add(1) == 1 {
			enterOnce.Do(func() { close(entered) })
			<-release
		}
		if slow.Load() {
			time.Sleep(10 * time.Millisecond)
		}
		into.apply(entry)
	}
	w := kvwatch.New("probe", first, apply, into.reset)
	t.Cleanup(w.Stop)
	w.Start()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first watcher never applied k000")
	}
	// The first watcher is held on its first key; its buffer holds the rest of its scan and its
	// sentinel. The Rewatch's scan sees 200 keys. The first watcher is released only once the
	// Rewatch is parked on applyMu, so the switch comes before the first watcher's sentinel.
	for i := 10; i < 200; i++ {
		if _, err := first.Put(fmt.Sprintf("k%03d", i), []byte("1")); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	liveConn, _ := bucket(t, uri)
	done := make(chan error, 1)
	go func() { done <- w.Rewatch(liveConn) }()
	eventually(t, "the Rewatch waiting for the held apply", waitingForTheApplyLock)
	slow.Store(true)
	releaseOnce.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	// The premise the lock wait sets up: the switch came while the first watcher still held the rest
	// of its scan. Read at the switch, before the current watcher's scan (10 ms a key) re-applies
	// them. If the wait released k000 before the Rewatch was parked on applyMu, the first watcher
	// applied k001..k009 and read its own sentinel while it was still current, and the assertion
	// below would pass without testing the sentinel at all.
	premise := false
	for i := 1; i < 10; i++ {
		if !into.has(fmt.Sprintf("k%03d", i)) {
			premise = true
			break
		}
	}
	if !premise {
		t.Fatal("the first watcher applied all of k001..k009 before the Rewatch switched: the lock wait did not hold k000 until the Rewatch was parked on applyMu")
	}
	eventually(t, "readiness", w.Ready)
	if !into.has("k199") {
		t.Fatal("ready while the current watcher's scan was incomplete: k199 missing")
	}
}

// A bucket deleted and replaced by a stream with an older creation time -- a JetStream restore of a
// snapshot taken before the bucket was last recreated, or a recreate after the clock stepped back --
// is still a recreated bucket. The stale-watch discard compares creation times, so it must only
// apply against a live, healthy watcher; otherwise every Rewatch would discard itself and the
// cache would keep the deleted bucket's keys.
func TestARewatchOntoABucketRestoredWithAnOlderStreamRefillsTheCache(t *testing.T) {
	_, uri := testnats.Start(t)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	const stream = "KV_kvwatch-test"
	old, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "kvwatch-test"})
	if err != nil {
		t.Fatalf("create the first bucket: %v", err)
	}
	if _, err := old.Put("restored-key", []byte("1")); err != nil {
		t.Fatalf("put restored-key: %v", err)
	}
	snap := snapshotStream(t, conn, stream)

	// Delete and create the bucket again: a newer stream the watcher installs.
	if err := js.DeleteKeyValue("kvwatch-test"); err != nil {
		t.Fatalf("delete the first bucket: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	live, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "kvwatch-test"})
	if err != nil {
		t.Fatalf("create the live bucket: %v", err)
	}
	if _, err := live.Put("live-key", []byte("1")); err != nil {
		t.Fatalf("put live-key: %v", err)
	}
	into := newSeen()
	w := kvwatch.New("probe cache", live, into.apply, into.reset)
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the live bucket's key", func() bool { return into.has("live-key") })

	// Delete it and restore the older snapshot in its place.
	if err := js.DeleteKeyValue("kvwatch-test"); err != nil {
		t.Fatalf("delete the live bucket: %v", err)
	}
	restoreStream(t, conn, stream, snap)

	if err := w.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
	for tick := 1; tick <= 3; tick++ {
		if err := w.Rewatch(conn); err != nil {
			t.Fatalf("tick %d Rewatch: %v", tick, err)
		}
		time.Sleep(300 * time.Millisecond)
		_ = w.Check()
	}
	if w.Err() != nil || !into.has("restored-key") || into.has("live-key") || into.resetCount() == 0 {
		t.Fatalf("after three Rewatches onto the restored bucket: Err=%v, restored-key=%v, live-key=%v, resets=%d; "+
			"want the restored bucket's key only, after a reset", w.Err(), into.has("restored-key"), into.has("live-key"), into.resetCount())
	}
}

// snapshot is a stream snapshot as the server sent it: the stream's config and state, and the
// data chunks.
type snapshot struct {
	config, state json.RawMessage
	chunks        [][]byte
}

// snapshotStream snapshots stream through the raw JetStream API, which nats.go does not wrap.
func snapshotStream(t *testing.T, conn *natsgo.Conn, stream string) snapshot {
	t.Helper()
	inbox := natsgo.NewInbox()
	chunks, err := conn.SubscribeSync(inbox)
	if err != nil {
		t.Fatalf("snapshot %s: subscribe: %v", stream, err)
	}
	req, _ := json.Marshal(map[string]any{"deliver_subject": inbox, "no_consumers": true})
	msg, err := conn.Request("$JS.API.STREAM.SNAPSHOT."+stream, req, 5*time.Second)
	if err != nil {
		t.Fatalf("snapshot %s: request: %v", stream, err)
	}
	var resp struct {
		Config json.RawMessage               `json:"config"`
		State  json.RawMessage               `json:"state"`
		Error  *struct{ Description string } `json:"error"`
	}
	if err := json.Unmarshal(msg.Data, &resp); err != nil || resp.Error != nil {
		t.Fatalf("snapshot %s: %v %s", stream, err, msg.Data)
	}
	snap := snapshot{config: resp.Config, state: resp.State}
	for {
		m, err := chunks.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("snapshot %s: chunk %d: %v", stream, len(snap.chunks), err)
		}
		if len(m.Data) == 0 {
			return snap
		}
		snap.chunks = append(snap.chunks, append([]byte(nil), m.Data...))
		if m.Reply != "" {
			_ = conn.Publish(m.Reply, nil)
		}
	}
}

// restoreStream restores snap as stream through the raw JetStream API. The restored stream keeps
// the snapshot's creation time.
func restoreStream(t *testing.T, conn *natsgo.Conn, stream string, snap snapshot) {
	t.Helper()
	req, _ := json.Marshal(map[string]any{"config": snap.config, "state": snap.state})
	msg, err := conn.Request("$JS.API.STREAM.RESTORE."+stream, req, 5*time.Second)
	if err != nil {
		t.Fatalf("restore %s: request: %v", stream, err)
	}
	var resp struct {
		DeliverSubject string                        `json:"deliver_subject"`
		Error          *struct{ Description string } `json:"error"`
	}
	if err := json.Unmarshal(msg.Data, &resp); err != nil || resp.Error != nil {
		t.Fatalf("restore %s: %v %s", stream, err, msg.Data)
	}
	for i, chunk := range snap.chunks {
		if _, err := conn.Request(resp.DeliverSubject, chunk, 5*time.Second); err != nil {
			t.Fatalf("restore %s: chunk %d: %v", stream, i, err)
		}
	}
	if _, err := conn.Request(resp.DeliverSubject, nil, 10*time.Second); err != nil {
		t.Fatalf("restore %s: end of data: %v", stream, err)
	}
}
