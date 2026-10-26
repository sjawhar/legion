package store

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// gate holds one call until the test opens it. reached closes once the call is held; done closes
// once it has run on, after the gate opened.
type gate struct {
	reached, release, done chan struct{}
	once                   sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{reached: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *gate) open() { g.once.Do(func() { close(g.release) }) }

// pass holds the caller at the gate until it opens, then runs through, and says it ran.
func (g *gate) pass(through func()) {
	close(g.reached)
	<-g.release
	through()
	close(g.done)
}

// waitClosed waits for ch, which names what, failing the test after 10 s.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// holdingCache is the registry's cache with a test's gates in front of its hooks: the next apply
// of a key a test holds, and the next read's completion, wait for the test. A completion held is a
// read that has delivered every record and has not yet swept, the moment a list during a read sees.
type holdingCache struct {
	kvwatch.Cache
	mu         sync.Mutex
	applies    map[string]*gate
	completion *gate
	afterSweep func()
}

func (c *holdingCache) Apply(entry kvwatch.Entry, read kvwatch.Read) {
	c.mu.Lock()
	g := c.applies[entry.Key]
	delete(c.applies, entry.Key)
	c.mu.Unlock()
	if g == nil {
		c.Cache.Apply(entry, read)
		return
	}
	g.pass(func() { c.Cache.Apply(entry, read) })
}

func (c *holdingCache) CompleteRead(read kvwatch.Read) {
	c.mu.Lock()
	g, afterSweep := c.completion, c.afterSweep
	c.completion, c.afterSweep = nil, nil
	c.mu.Unlock()
	if g == nil {
		c.Cache.CompleteRead(read)
		return
	}
	g.pass(func() {
		c.Cache.CompleteRead(read)
		if afterSweep != nil {
			afterSweep()
		}
	})
}

// holdApply holds the next apply of key.
func (c *holdingCache) holdApply(t *testing.T, key string) *gate {
	g := newGate(t)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.applies == nil {
		c.applies = map[string]*gate{}
	}
	c.applies[key] = g
	return g
}

// holdCompletion holds the next read's completion. afterSweep, when not nil, runs right after its
// sweep, before the watcher applies any later record, so it sees the copy the sweep left.
func (c *holdingCache) holdCompletion(t *testing.T, afterSweep func()) *gate {
	g := newGate(t)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.completion, c.afterSweep = g, afterSweep
	return g
}

// delayingKV is an interest bucket handle whose next Get of a key a test delays reads the bucket at
// once and answers only once the test opens its gate, so the answer arrives after whatever the
// test does in between.
type delayingKV struct {
	natsgo.KeyValue
	mu   sync.Mutex
	gets map[string]*gate
}

func (kv *delayingKV) delayGet(t *testing.T, key string) *gate {
	g := newGate(t)
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if kv.gets == nil {
		kv.gets = map[string]*gate{}
	}
	kv.gets[key] = g
	return g
}

func (kv *delayingKV) Get(key string) (natsgo.KeyValueEntry, error) {
	entry, err := kv.KeyValue.Get(key)
	kv.mu.Lock()
	g := kv.gets[key]
	delete(kv.gets, key)
	kv.mu.Unlock()
	if g != nil {
		g.pass(func() {})
	}
	return entry, err
}

// rebuiltRegistry opens a registry on the test's own buckets, waits for its first read to
// complete, and rebuilds its watcher, not yet started, over a delayingKV and with a holdingCache in
// front of its cache, so the test can hold that watcher's first read before it starts it. The
// buckets are empty, so nothing more reaches the cache through the watcher replaced here.
func rebuiltRegistry(t *testing.T, conn *natsgo.Conn) (*Registry, *holdingCache, *delayingKV) {
	t.Helper()
	reg, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	waitFor(t, 5*time.Second, reg.watcher.Following)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reg.watcher.End(ctx)
	delayed := &delayingKV{KeyValue: reg.watcher.KV().Legacy()}
	held := &holdingCache{Cache: interestCache{reg}}
	reg.watcher = kvwatch.New("interest registry", bus.NewKeyValue(delayed, reg.watcher.KV().WatchHandle(), reg.watcher.KV().Streams()), held)
	t.Cleanup(reg.StopWatch)
	return reg, held, delayed
}

// cachedIDs is the sessions whose interest the cache holds, for a hook to read right after a
// sweep.
func cachedIDs(r *Registry) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.cache))
	for id := range r.cache {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// The interest list is refused until the copy has completed a read of the bucket, while the
// cache-only Cached answers what the copy holds and never reads the bucket.
func TestTheInterestListIsRefusedUntilAReadCompletes(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg, held, delayed := rebuiltRegistry(t, conn)
	putInterest(t, delayed.KeyValue, Interest{SessionID: "ses_a", MachineID: "m1", Topics: []string{"notifications.a"}})
	completion := held.holdCompletion(t, nil)
	reg.watcher.Start()
	waitClosed(t, completion.reached, "the first read to deliver every record")

	if _, err := reg.List(); !errors.Is(err, kvwatch.ErrRefilling) {
		t.Fatalf("List before the first read completed = %v, want kvwatch.ErrRefilling", err)
	}
	if err := reg.Servable(); !errors.Is(err, kvwatch.ErrRefilling) {
		t.Fatalf("Servable before the first read completed = %v, want kvwatch.ErrRefilling", err)
	}
	if _, ok := reg.Cached("ses_a"); !ok {
		t.Fatal("Cached(ses_a) misses the interest the read delivered")
	}
	if _, ok := reg.Cached("ses_elsewhere"); ok {
		t.Fatal("Cached(ses_elsewhere) answered an interest the bucket does not hold")
	}

	completion.open()
	waitFor(t, 5*time.Second, reg.watcher.Following)
	items, err := reg.List()
	if err != nil {
		t.Fatalf("List after the read: %v", err)
	}
	if len(items) != 1 || items[0].SessionID != "ses_a" {
		t.Fatalf("List after the read = %+v, want ses_a's interest", items)
	}
}

// A read-through that a recreate of the interest bucket overtakes read the old bucket: its answer
// reaches the cache after the new bucket's read has completed, and is dropped. Applied, its
// revision would fence out the new bucket's revisions, which number from 1 again, and the copy
// would keep the old topics past every later write to the new bucket.
func TestAReadThroughOfABucketSinceRecreatedIsDropped(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg, held, delayed := rebuiltRegistry(t, conn)
	reg.watcher.Start()
	waitFor(t, 5*time.Second, reg.watcher.Following)
	// The old bucket's revisions run well past the new bucket's.
	for range 20 {
		putInterest(t, delayed.KeyValue, Interest{SessionID: "ses_filler", MachineID: "m1", Topics: []string{"notifications.filler"}})
	}
	// The watcher's apply of ses_a is held, so the cache misses it and Get reads it through.
	apply := held.holdApply(t, "ses_a")
	putInterest(t, delayed.KeyValue, Interest{SessionID: "ses_a", MachineID: "m1", Topics: []string{"notifications.old"}})
	waitClosed(t, apply.reached, "the watcher to deliver ses_a")
	read := delayed.delayGet(t, "ses_a")
	done := make(chan error, 1)
	go func() {
		_, err := reg.Get("ses_a")
		done <- err
	}()
	waitClosed(t, read.reached, "the old bucket to answer the read-through")
	apply.open()

	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.InterestBucket); err != nil {
		t.Fatalf("delete the interest bucket: %v", err)
	}
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: 1, Storage: natsgo.FileStorage})
	putInterest(t, recreated, Interest{SessionID: "ses_a", MachineID: "m1", Topics: []string{"notifications.new"}})
	if err := reg.Rewatch(conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return reg.watcher.Epoch() == 1 && reg.watcher.Following() })

	read.open()
	if err := <-done; err != nil {
		t.Fatalf("the read-through: %v", err)
	}
	if got := reg.Match("m1", "notifications.old"); len(got) != 0 {
		t.Fatalf("the old bucket's read-through reached the cache: %+v", got)
	}
	if got := reg.Match("m1", "notifications.new"); len(got) != 1 {
		t.Fatalf("ses_a's new-bucket interest = %+v, want it matched", got)
	}
	putInterest(t, recreated, Interest{SessionID: "ses_a", MachineID: "m1", Topics: []string{"notifications.newer"}})
	pollMatch(t, reg, "m1", "notifications.newer", 1, 5*time.Second)
}

// A read-through that began while the read was under way and answers only after the read's sweep
// adds an interest only when its revision passes the fence: one the bucket still holds is cached,
// and one deleted since the read-through read it is not brought back.
func TestAReadThroughSpanningTheReadsCompletionAddsOnlyWhatTheBucketHolds(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg, held, delayed := rebuiltRegistry(t, conn)
	completion := held.holdCompletion(t, nil)
	reg.watcher.Start()
	waitClosed(t, completion.reached, "the read to deliver every record")

	reads := map[string]*gate{}
	done := make(chan error, 2)
	for _, sessionID := range []string{"ses_live", "ses_gone"} {
		putInterest(t, delayed.KeyValue, Interest{SessionID: sessionID, MachineID: "m1", Topics: []string{"notifications." + sessionID}})
		reads[sessionID] = delayed.delayGet(t, sessionID)
		go func() {
			_, err := reg.Get(sessionID)
			done <- err
		}()
		waitClosed(t, reads[sessionID].reached, sessionID+"'s read-through to read the bucket")
	}
	if err := delayed.KeyValue.Delete("ses_gone"); err != nil {
		t.Fatalf("delete ses_gone: %v", err)
	}
	gone, err := delayed.KeyValue.History("ses_gone")
	if err != nil {
		t.Fatalf("history of ses_gone: %v", err)
	}
	deleted := gone[len(gone)-1].Revision()

	completion.open()
	waitFor(t, 5*time.Second, func() bool {
		reg.mu.RLock()
		defer reg.mu.RUnlock()
		return reg.watcher.Following() && reg.cacheRevisions["ses_gone"] == deleted
	})
	for _, read := range reads {
		read.open()
	}
	for range reads {
		if err := <-done; err != nil {
			t.Fatalf("a read-through: %v", err)
		}
	}
	if _, ok := reg.Cached("ses_live"); !ok {
		t.Fatal("ses_live's interest, which the bucket holds, is not cached")
	}
	if item, ok := reg.Cached("ses_gone"); ok {
		t.Fatalf("the read-through brought back ses_gone's deleted interest: %+v", item)
	}
	if got := cachedIDs(reg); !slices.Equal(got, []string{"ses_live"}) {
		t.Fatalf("cached = %v, want ses_live alone", got)
	}
}

// The reaper asks whether each session is live with no lock of the interest cache held, so a
// liveness check that waits on a bucket read stalls no write to the cache; an interest written
// again while its check was under way is kept, even when the check finds its session gone.
func TestTheReaperAsksLivenessOutsideTheCacheLockAndKeepsAnInterestWrittenSince(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg, kv := coldRegistry(t, conn)
	stale := time.Now().Add(-20 * time.Minute).UnixMilli()
	for _, sessionID := range []string{"ses_dead", "ses_rewritten"} {
		putInterest(t, kv, Interest{SessionID: sessionID, MachineID: "m1", Topics: []string{"notifications." + sessionID}, UpdatedAt: stale})
		if _, err := reg.Get(sessionID); err != nil {
			t.Fatalf("warm %s: %v", sessionID, err)
		}
	}

	asked := newGate(t)
	reaped := make(chan int, 1)
	go func() {
		count, err := reg.Reap(func(sessionID string) (bool, error) {
			if sessionID == "ses_rewritten" {
				asked.pass(func() {})
			}
			return false, nil
		}, 10*time.Minute)
		if err != nil {
			t.Errorf("Reap: %v", err)
		}
		reaped <- count
	}()
	waitClosed(t, asked.reached, "the reaper to ask after ses_rewritten")
	written := make(chan error, 1)
	go func() {
		_, err := reg.Upsert(Interest{SessionID: "ses_rewritten", MachineID: "m1"}, []string{"notifications.ses_rewritten"})
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("Upsert while the reaper waits on liveness: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Upsert waited on the reaper's liveness check: the reaper held the cache's lock")
	}
	asked.open()
	if count := <-reaped; count != 1 {
		t.Fatalf("reaped = %d, want ses_dead's interest alone", count)
	}
	if _, err := kv.Get("ses_dead"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("ses_dead's interest after the reaper: %v, want it deleted", err)
	}
	if _, err := kv.Get("ses_rewritten"); err != nil {
		t.Fatalf("ses_rewritten's interest after the reaper: %v, want it kept", err)
	}
}

// A liveness check that could not read a session's record keeps that session's interest.
func TestTheReaperKeepsAnInterestWhoseSessionItCouldNotRead(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg, kv := coldRegistry(t, conn)
	putInterest(t, kv, Interest{SessionID: "ses_unread", MachineID: "m1", Topics: []string{"notifications.unread"}, UpdatedAt: time.Now().Add(-20 * time.Minute).UnixMilli()})
	if _, err := reg.Get("ses_unread"); err != nil {
		t.Fatalf("warm ses_unread: %v", err)
	}
	count, err := reg.Reap(func(string) (bool, error) { return false, errors.New("the session bucket did not answer") }, 10*time.Minute)
	if err != nil || count != 0 {
		t.Fatalf("Reap = %d, %v; want nothing reaped", count, err)
	}
	if _, err := kv.Get("ses_unread"); err != nil {
		t.Fatalf("ses_unread's interest after the reaper: %v, want it kept", err)
	}
}
