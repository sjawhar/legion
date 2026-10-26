package kvwatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/goroutinetest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// seen is a recording Cache that keeps a copy the way the stores do: each delivered record updates
// it, a read marks each key it delivers, and a completed read sweeps every key it left unmarked.
// It records every read that began and every read that completed.
type seen struct {
	mu        sync.Mutex
	keys      map[string]bool
	marks     kvwatch.Marks
	begun     []kvwatch.Read
	completed []kvwatch.Read
}

func newSeen() *seen { return &seen{keys: map[string]bool{}} }

func (s *seen) Apply(entry kvwatch.Entry, _ kvwatch.Read) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks.Mark(entry.Key)
	if entry.Op == kvwatch.OpPut {
		s.keys[entry.Key] = true
		return
	}
	delete(s.keys, entry.Key)
}

func (s *seen) BeginRead(read kvwatch.Read) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks.Begin()
	s.begun = append(s.begun, read)
}

func (s *seen) CompleteRead(read kvwatch.Read) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kvwatch.Sweep(&s.marks, s.keys, nil)
	s.marks.End()
	s.completed = append(s.completed, read)
}

// hold puts key in the copy as if an earlier read had left it there.
func (s *seen) hold(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[key] = true
}

func (s *seen) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys[key]
}

// reads is every read that began, and completions every read that completed, in order.
func (s *seen) reads() []kvwatch.Read {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]kvwatch.Read(nil), s.begun...)
}

func (s *seen) completions() []kvwatch.Read {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]kvwatch.Read(nil), s.completed...)
}

// withApply is a seen that runs before on each record before it takes it, so a test can hold an
// apply open or count what was applied when. before reads the record as a nats.KeyValueEntry
// (legacyView), the shape the tests' hooks were written against.
type withApply struct {
	*seen
	before func(natsgo.KeyValueEntry)
}

func (c withApply) Apply(entry kvwatch.Entry, read kvwatch.Read) {
	c.before(legacyView{entry})
	c.seen.Apply(entry, read)
}

// legacyView is a record a watcher delivered, as the nats.KeyValueEntry a hook reads.
type legacyView struct{ entry kvwatch.Entry }

func (e legacyView) Bucket() string     { return testBucketName }
func (e legacyView) Key() string        { return e.entry.Key }
func (e legacyView) Value() []byte      { return e.entry.Value }
func (e legacyView) Revision() uint64   { return e.entry.Revision }
func (e legacyView) Created() time.Time { return e.entry.Created }
func (e legacyView) Delta() uint64      { return 0 }
func (e legacyView) Operation() natsgo.KeyValueOp {
	switch e.entry.Op {
	case kvwatch.OpDelete:
		return natsgo.KeyValueDelete
	case kvwatch.OpPurge:
		return natsgo.KeyValuePurge
	}
	return natsgo.KeyValuePut
}

// clock is the injected clock a stale episode is measured on (kvwatch.WithNow).
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// scriptedKV is a bucket's jetstream handle whose watches go as the steps of its script say, and
// through the handle itself once the script has run out. It counts the watches it was asked for.
type scriptedKV struct {
	jetstream.KeyValue
	legacy  natsgo.KeyValue
	streams jetstream.JetStream
	mu      sync.Mutex
	steps   []watchStep
	watched int
}

// watchStep is one watch of a scriptedKV, on the bucket's jetstream handle kv, in ctx, the
// context the watcher's subscription lives in.
type watchStep func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error)

// scripted is kv whose watches go as steps say.
func scripted(kv bus.KeyValue, steps ...watchStep) *scriptedKV {
	return &scriptedKV{KeyValue: kv.WatchHandle(), legacy: kv.Legacy(), streams: kv.Streams(), steps: steps}
}

func (k *scriptedKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	k.mu.Lock()
	k.watched++
	var step watchStep
	if len(k.steps) > 0 {
		step, k.steps = k.steps[0], k.steps[1:]
	}
	k.mu.Unlock()
	if step == nil {
		return k.KeyValue.Watch(ctx, keys, opts...)
	}
	return step(ctx, k.KeyValue)
}

func (k *scriptedKV) watches() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.watched
}

// handle is k as the bus.KeyValue a watcher takes.
func (k *scriptedKV) handle() bus.KeyValue { return bus.NewKeyValue(k.legacy, k, k.streams) }

// stallsAfter is a watch whose scan delivers `after` records and then nothing more: it drops the
// rest of the scan and its nil, as a scan that stalls, so only the watcher's idle wait ends it. What
// the bucket delivers after the scan is forwarded, as a live watcher's updates are.
func stallsAfter(after int) watchStep {
	return func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		watcher, err := kv.WatchAll(ctx)
		if err != nil {
			return nil, err
		}
		updates := make(chan jetstream.KeyValueEntry)
		go func() {
			defer close(updates)
			scanning, forwarded := true, 0
			for entry := range watcher.Updates() {
				switch {
				case entry == nil && scanning:
					scanning = false
				case scanning && forwarded == after:
				default:
					forwarded++
					updates <- entry
				}
			}
		}()
		return &forwardedWatcher{KeyWatcher: watcher, updates: updates}, nil
	}
}

// fails is a watch whose create is refused with err.
func fails(err error) watchStep {
	return func(context.Context, jetstream.KeyValue) (jetstream.KeyWatcher, error) { return nil, err }
}

// endsEarly is a watch whose subscription ends after `after` entries, before its scan's nil, as an
// ordered consumer nats.go could not recreate ends it.
func endsEarly(after int) watchStep {
	return func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		watcher, err := kv.WatchAll(ctx)
		if err != nil {
			return nil, err
		}
		updates := make(chan jetstream.KeyValueEntry)
		go func() {
			defer close(updates)
			for range after {
				entry, ok := <-watcher.Updates()
				if !ok || entry == nil {
					break
				}
				updates <- entry
			}
			_ = watcher.Stop()
			for range watcher.Updates() {
			}
		}()
		return &forwardedWatcher{KeyWatcher: watcher, updates: updates}, nil
	}
}

// heldEnd holds one read's end: its watcher delivers the bucket's records, then waits with the
// scan's nil in hand until the test releases it. reached closes when the nil is held; the watcher
// may still be applying the record it took last. handled closes once the watcher has taken the nil
// and is waiting for its next record: the watcher then sends one more nil, which it can take only
// after it has handled the first (a nil past the first is one it ignores), so a test knows the
// read's end was handled, however it was.
type heldEnd struct {
	reached, release, handled chan struct{}
	once                      sync.Once
}

func newHeldEnd(t *testing.T) *heldEnd {
	h := &heldEnd{reached: make(chan struct{}), release: make(chan struct{}), handled: make(chan struct{})}
	t.Cleanup(h.let)
	return h
}

// let releases the held nil.
func (h *heldEnd) let() { h.once.Do(func() { close(h.release) }) }

// step is the watch that holds its end on h.
func (h *heldEnd) step() watchStep {
	return func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		watcher, err := kv.WatchAll(ctx)
		if err != nil {
			return nil, err
		}
		updates := make(chan jetstream.KeyValueEntry)
		go func() {
			defer close(updates)
			held := false
			for entry := range watcher.Updates() {
				if entry == nil && !held {
					held = true
					close(h.reached)
					<-h.release
					updates <- nil
					updates <- nil
					close(h.handled)
					continue
				}
				updates <- entry
			}
		}()
		return &forwardedWatcher{KeyWatcher: watcher, updates: updates}, nil
	}
}

// wait waits for ch, which names what, failing the test after 10 s.
func wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// forwardedWatcher is a watcher whose updates a step's goroutine forwards from the real one.
type forwardedWatcher struct {
	jetstream.KeyWatcher
	updates chan jetstream.KeyValueEntry
}

func (w *forwardedWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updates }

// bucket opens the test bucket on its own connection, which the test can close to end a watcher
// the way a lost connection does.
func bucket(t *testing.T, uri string) (*natsgo.Conn, bus.KeyValue) {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := bus.EnsureKeyValue(conn, js, &natsgo.KeyValueConfig{Bucket: "kvwatch-test"})
	if err != nil {
		t.Fatalf("bucket: %v", err)
	}
	return conn, kv
}

// wrap is kv, a legacy handle opened on conn (a recreated bucket), as the bus.KeyValue a watcher
// takes.
func wrap(t *testing.T, conn *natsgo.Conn, kv natsgo.KeyValue) bus.KeyValue {
	t.Helper()
	wrapped, err := bus.WrapKeyValue(conn, kv)
	if err != nil {
		t.Fatalf("wrap the bucket handle: %v", err)
	}
	return wrapped
}

// jetStreamEntry reads key through kv's jetstream handle, as a watcher would deliver it.
func jetStreamEntry(t *testing.T, kv bus.KeyValue, key string) jetstream.KeyValueEntry {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entry, err := kv.WatchHandle().Get(ctx, key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return entry
}

// lockedInWatch matches a sync.Mutex.Lock frame whose caller is watch itself. Inside watch that is
// only applyMu: a wait on w.mu shows sync.(*RWMutex).Lock as the caller, and one on nats.go's
// connection mutex, further down the call tree (bus.ReadStreamState, WatchAll), shows nats.go frames.
var lockedInWatch = regexp.MustCompile(`sync\.\(\*Mutex\)\.Lock\([^\n]*\n\t[^\n]*\n[^\n]*kvwatch\.\(\*Watcher\)\.watch\(`)

// waitingForTheApplyLock reports whether a watch is parked on applyMu: a goroutine whose wait reason
// is sync.Mutex.Lock and whose Lock frame (*Watcher).watch called. A test that holds an apply open
// waits for this before it releases the apply, so the watch is already queued for the lock the
// release frees.
func waitingForTheApplyLock() bool {
	for _, goroutine := range goroutinetest.Traces() {
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

// The cache is ready once the first watcher has delivered every existing key, and not before. Its
// copy can be served only once that first read has completed: before it, Servable refuses.
func TestTheCacheIsReadyAfterTheFirstScan(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	if _, err := kv.Put("before-start", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	into := newSeen()
	w := kvwatch.New("test cache", kv, into)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := w.WaitReady(ctx); !errors.Is(err, context.DeadlineExceeded) || w.Ready() {
		t.Fatalf("WaitReady before Start = %v (ready %v), want the context's deadline", err, w.Ready())
	}
	if err := w.Servable(); !errors.Is(err, kvwatch.ErrRefilling) {
		t.Fatalf("Servable before any read = %v, want %v", err, kvwatch.ErrRefilling)
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
	if err := w.Servable(); err != nil || !w.Following() || w.NeedsRead() {
		t.Fatalf("after the first complete read: Servable %v, Following %v, NeedsRead %v; want nil, true, false",
			err, w.Following(), w.NeedsRead())
	}
	if reads, done := into.reads(), into.completions(); len(reads) != 1 || len(done) != 1 || reads[0].Recreated || reads[0].Epoch != 0 {
		t.Fatalf("reads begun %+v, completed %+v; want one read of epoch 0, completed", reads, done)
	}
}

// A first start that fails releases readiness and records its error, so a caller waiting for the
// cache sees an empty cache that /healthz reports rather than a hang.
func TestAFailedFirstStartIsReadyAndRecordsItsError(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	conn.Close()
	w := kvwatch.New("test cache", kv, newSeen())

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
	w := kvwatch.New("test cache", first, into)
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
	}
	w := kvwatch.New("test cache", first, withApply{seen: into, before: apply})
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
	w := kvwatch.New("test cache", kv, into)
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
	for _, read := range into.reads() {
		if read.Recreated || read.Epoch != 0 {
			t.Fatalf("a rewatch onto the same bucket began read %+v, want every read of epoch 0", read)
		}
	}
}

// A watcher Rewatch replaced ends without recording anything: only the current watcher's end is
// the cache's.
func TestAReplacedWatcherEndingRecordsNothing(t *testing.T) {
	uri := testnats.URL(t)
	firstConn, first := bucket(t, uri)
	secondConn, _ := bucket(t, uri)
	w := kvwatch.New("test cache", first, newSeen())
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
// revisions would drop the new bucket's entries. A Rewatch onto the recreated bucket begins a read
// of a new epoch, and the cache keeps its copy through that read: a key only the old bucket held
// is still there while the read runs, a key of the new bucket appears as it is delivered, and only
// the read's completion removes what the new bucket does not hold. Until then the copy is served,
// within its stale bound, rather than emptied. The recreate is logged, in the words the refill rig
// reads.
func TestARewatchOntoARecreatedBucketKeepsTheCopyUntilTheReadCompletes(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	for _, key := range []string{"ghost", "both"} {
		if _, err := kv.Put(key, []byte("old")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	into := newSeen()
	now := newClock()
	logger, records := jsonLogger(t)
	// No periodic probe: the test drives the read of the recreated bucket itself.
	w := kvwatch.New("test cache", kv, into, kvwatch.WithNow(now.Now), kvwatch.WithLogger(logger), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	for _, key := range []string{"both", "fresh"} {
		if _, err := recreated.Put(key, []byte("new")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	end := newHeldEnd(t)
	if err := kvwatch.WatchOn(w, scripted(wrap(t, conn, recreated), end.step()).handle()); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	wait(t, end.reached, "the new read to deliver every record")
	eventually(t, "the new bucket's key, as delivered", func() bool { return into.has("fresh") })

	if !into.has("ghost") || !into.has("both") || !into.has("fresh") {
		t.Fatalf("while the new read runs: ghost %v, both %v, fresh %v; want the old copy kept and the new key delivered",
			into.has("ghost"), into.has("both"), into.has("fresh"))
	}
	if w.Following() || w.Servable() != nil || len(into.completions()) != 1 {
		t.Fatalf("while the new read runs: Following %v, Servable %v, %d reads completed; want not following, served, one",
			w.Following(), w.Servable(), len(into.completions()))
	}
	reads := into.reads()
	if last := reads[len(reads)-1]; len(reads) != 2 || !last.Recreated || last.Epoch != 1 || w.Epoch() != 1 {
		t.Fatalf("reads begun %+v, Epoch %d; want a second read, recreated, of epoch 1", reads, w.Epoch())
	}
	recreateLine := "test cache bucket was recreated; its cache is re-read from the new bucket and kept until the read completes"
	if line := record(t, records(), recreateLine); line["level"] != "INFO" || line["bucket"] != testBucketName {
		t.Fatalf("the recreate line = %v, want INFO naming bucket %s", line, testBucketName)
	}

	end.let()
	wait(t, end.handled, "the new read's end to be handled")
	if into.has("ghost") || !into.has("both") || !into.has("fresh") || !w.Following() {
		t.Fatalf("after the new read completed: ghost %v, both %v, fresh %v, Following %v; want the ghost swept and the rest kept",
			into.has("ghost"), into.has("both"), into.has("fresh"), w.Following())
	}
	if done := into.completions(); len(done) != 2 || done[1].Epoch != 1 {
		t.Fatalf("reads completed %+v, want the new epoch's read second", done)
	}
}

// A bucket deleted and created again while its watcher runs need not end the watcher: nats.go's
// ordered consumer can reset onto the new stream and go on without delivering its first revisions,
// so the cache stays frozen behind a live watcher. Check, the store's health probe, reports it as
// the watcher's terminal error and the copy stops following, though it keeps every key; the Rewatch
// the listener then runs reads the new bucket, and only that read's completion removes the key the
// new bucket does not hold.
func TestCheckReportsABucketRecreatedUnderALiveWatcher(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	if _, err := kv.Put("ghost", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	into := newSeen()
	now := newClock()
	// No periodic probe, which would find the recreate before Check does.
	w := kvwatch.New("test cache", kv, into, kvwatch.WithNow(now.Now), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)
	if err := w.Check(); err != nil || w.Err() != nil {
		t.Fatalf("Check on the watched bucket = %v, Err %v, want both nil", err, w.Err())
	}

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	if err := w.Check(); err != nil {
		t.Fatalf("Check on the recreated bucket: %v", err)
	}
	// The watcher may also have ended on its own by now, which records its own terminal error; either
	// way the recreated bucket reaches Err.
	if err := w.Err(); err == nil {
		t.Fatal("Check on a recreated bucket left Err nil, so self-health would never rebuild the watcher")
	}
	if !into.has("ghost") || w.Following() || w.Servable() != nil {
		t.Fatalf("after Check flagged the bucket: ghost %v, Following %v, Servable %v; want the copy kept, not following, served within the bound",
			into.has("ghost"), w.Following(), w.Servable())
	}

	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	if err := w.Err(); err != nil {
		t.Fatalf("Err after the rewatch = %v, want nil", err)
	}
	eventually(t, "the new read's completion", w.Following)
	if into.has("ghost") || w.Epoch() != 1 {
		t.Fatalf("after the new read completed: ghost kept %v, Epoch %d; want the ghost swept in epoch 1", into.has("ghost"), w.Epoch())
	}
}

// After Stop, the current watcher's end records nothing and a later Rewatch arms nothing, so a
// reconnect hook still running at shutdown neither reports an error nor starts a watcher.
func TestStopMakesTheEndSilentAndRewatchANoOp(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	into := newSeen()
	w := kvwatch.New("test cache", kv, into)
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

// eventuallyWithin is eventually with its own deadline, for a wait that spans a NATS restart.
func eventuallyWithin(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// staleBound is the default stale bound, which these tests move the injected clock past.
const staleBound = 2 * time.Minute

// refuses fails the test unless Servable answers ErrRefilling, naming when.
func refuses(t *testing.T, w *kvwatch.Watcher, when string) {
	t.Helper()
	if err := w.Servable(); !errors.Is(err, kvwatch.ErrRefilling) {
		t.Fatalf("Servable %s = %v, want %v", when, err, kvwatch.ErrRefilling)
	}
}

// serves fails the test unless Servable answers nil, naming when.
func serves(t *testing.T, w *kvwatch.Watcher, when string) {
	t.Helper()
	if err := w.Servable(); err != nil {
		t.Fatalf("Servable %s = %v, want nil", when, err)
	}
}

// A read whose end is held, then released only after something took its right to complete -- a
// newer read, a recreated bucket Check found, a disconnect, a Stop -- completes nothing: the
// cache's CompleteRead is not called for it and the copy does not follow its bucket again, so a
// copy past its stale bound stays refused. Only a read that may still complete restores serving.
func TestAHeldReadReleasedTooLateNeitherCompletesNorRestoresServing(t *testing.T) {
	for _, test := range []struct {
		name string
		// overtake takes the held read's right to complete. It returns the read that then
		// restores serving, when there is one.
		overtake func(t *testing.T, w *kvwatch.Watcher, conn *natsgo.Conn, kv bus.KeyValue) *heldEnd
	}{
		{name: "a newer read", overtake: func(t *testing.T, w *kvwatch.Watcher, _ *natsgo.Conn, kv bus.KeyValue) *heldEnd {
			newer := newHeldEnd(t)
			if err := kvwatch.WatchOn(w, scripted(kv, newer.step()).handle()); err != nil {
				t.Fatalf("the newer read: %v", err)
			}
			wait(t, newer.reached, "the newer read to deliver every record")
			return newer
		}},
		{name: "Check finding the bucket recreated", overtake: func(t *testing.T, w *kvwatch.Watcher, conn *natsgo.Conn, _ bus.KeyValue) *heldEnd {
			js := jetStream(t, conn)
			deleteTestBucket(t, js)
			testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
			if err := w.Check(); err != nil || w.Err() == nil {
				t.Fatalf("Check on the recreated bucket = %v, Err %v; want the recreate recorded", err, w.Err())
			}
			return nil
		}},
		{name: "a disconnect", overtake: func(t *testing.T, w *kvwatch.Watcher, _ *natsgo.Conn, _ bus.KeyValue) *heldEnd {
			w.Disconnected()
			return nil
		}},
		{name: "Stop", overtake: func(t *testing.T, w *kvwatch.Watcher, _ *natsgo.Conn, _ bus.KeyValue) *heldEnd {
			w.Stop()
			return nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			uri := testnats.URL(t)
			conn, kv := bucket(t, uri)
			if _, err := kv.Put("kept", []byte("1")); err != nil {
				t.Fatalf("put: %v", err)
			}
			into := newSeen()
			now := newClock()
			// No periodic probe, which would read a recreated bucket before Check finds it.
			w := kvwatch.New("test cache", kv, into, kvwatch.WithNow(now.Now), kvwatch.WithStreamProbeInterval(0))
			t.Cleanup(w.Stop)
			w.Start()
			eventually(t, "the first read's completion", w.Following)

			held := newHeldEnd(t)
			if err := kvwatch.WatchOn(w, scripted(kv, held.step()).handle()); err != nil {
				t.Fatalf("the held read: %v", err)
			}
			wait(t, held.reached, "the held read to deliver every record")
			now.advance(staleBound + time.Second)
			refuses(t, w, "with a read held past the bound")

			restorer := test.overtake(t, w, conn, kv)
			held.let()
			wait(t, held.handled, "the held read's end to be handled")
			if done := len(into.completions()); done != 1 || w.Following() {
				t.Fatalf("after the overtaken read's end: %d reads completed, Following %v; want only the first, not following", done, w.Following())
			}
			refuses(t, w, "after the overtaken read's end")

			if restorer == nil {
				return
			}
			restorer.let()
			wait(t, restorer.handled, "the newer read's end to be handled")
			if done := len(into.completions()); done != 2 || !w.Following() || !into.has("kept") {
				t.Fatalf("after the newer read's end: %d reads completed, Following %v, kept %v; want two, following, kept",
					done, w.Following(), into.has("kept"))
			}
			serves(t, w, "after the newer read completed")
		})
	}
}

// One stale episode runs from the first moment the copy stopped following until a read completes,
// across any number of reads that fail: one that timed out, one that ended early and a Rewatch that
// never installed. The bound counts from the first, so the copy is refused once that is past the
// bound however recent the last failure, and one complete read restores serving.
func TestAStaleEpisodeOutlastsFailedReadsUntilOneCompletes(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	if _, err := kv.Put("kept", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	into := newSeen()
	now := newClock()
	arm := &armed{}
	w := kvwatch.New("test cache", kv, into, kvwatch.WithNow(now.Now), kvwatch.WithAfterFunc(arm.afterFunc()), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	if err := kvwatch.WatchOn(w, scripted(kv, stallsAfter(0)).handle()); err != nil {
		t.Fatalf("the read that times out: %v", err)
	}
	now.advance(kvwatch.ScanIdleTimeout)
	arm.fireIdle(t)
	eventually(t, "the timed-out read's re-read to be armed", func() bool { return arm.waits() == 1 })
	now.advance(staleBound - 30*time.Second - kvwatch.ScanIdleTimeout)
	serves(t, w, "within the bound of the first failed read")

	if err := kvwatch.WatchOn(w, scripted(kv, endsEarly(0)).handle()); err != nil {
		t.Fatalf("the read that ends early: %v", err)
	}
	eventually(t, "the early end's error", func() bool { return w.Err() != nil })
	closed := testnats.Connect(t, uri)
	closed.Close()
	if err := w.Rewatch(closed); err == nil {
		t.Fatal("a Rewatch on a closed connection succeeded")
	}
	now.advance(31 * time.Second)
	refuses(t, w, "past the bound of the first failed read")
	if !w.NeedsRead() {
		t.Fatal("NeedsRead = false for a copy no read is under way to repair")
	}

	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	eventually(t, "the complete read", w.Following)
	serves(t, w, "after one complete read")
	if done := len(into.completions()); done != 2 || !into.has("kept") {
		t.Fatalf("%d reads completed, kept %v; want the first and the last only, kept", done, into.has("kept"))
	}
}

// A Rewatch that fails before it installs anything changes nothing: the listener's self-health
// rebuild rewatches every cache, healthy ones included, so a following copy stays following and
// served however long ago its read completed.
func TestAFailedRewatchLeavesAFollowingWatcherServing(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	into := newSeen()
	now := newClock()
	w := kvwatch.New("test cache", kv, into, kvwatch.WithNow(now.Now))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	closed := testnats.Connect(t, uri)
	closed.Close()
	if err := w.Rewatch(closed); err == nil {
		t.Fatal("a Rewatch on a closed connection succeeded")
	}
	now.advance(time.Hour)
	if !w.Following() || w.NeedsRead() || w.Err() != nil {
		t.Fatalf("after the failed Rewatch: Following %v, NeedsRead %v, Err %v; want following, nothing needed, no error",
			w.Following(), w.NeedsRead(), w.Err())
	}
	serves(t, w, "an hour after a failed Rewatch of a following copy")
	if reads := len(into.reads()); reads != 1 {
		t.Fatalf("%d reads began, want only the first", reads)
	}
}

// A read whose scan delivers nothing for the idle timeout removes nothing, and the watcher reads
// the bucket again itself after rereadDelay: the watcher still runs, so nothing else -- no terminal
// error, no self-health rebuild -- would start that read. A re-read whose setup fails is armed
// again; a newer read or a Stop makes the armed one read nothing.
func TestATimedOutReadIsReadAgainByTheWatcher(t *testing.T) {
	// timedOut starts a watcher whose first read stalls after one record and times out at its
	// idle wait, its later watches as steps says, with the copy holding a key the bucket does not,
	// and waits for the re-read to be armed.
	timedOut := func(t *testing.T, steps ...watchStep) (*kvwatch.Watcher, *seen, *armed, *scriptedKV, *natsgo.Conn) {
		t.Helper()
		uri := testnats.URL(t)
		conn, kv := bucket(t, uri)
		for _, key := range []string{"one", "two", "three"} {
			if _, err := kv.Put(key, []byte("1")); err != nil {
				t.Fatalf("put %s: %v", key, err)
			}
		}
		into := newSeen()
		into.hold("gone-from-the-bucket")
		arm := &armed{}
		now := newClock()
		handle := scripted(kv, append([]watchStep{stallsAfter(1)}, steps...)...)
		w := kvwatch.New("test cache", handle.handle(), into, kvwatch.WithAfterFunc(arm.afterFunc()),
			kvwatch.WithNow(now.Now), kvwatch.WithStreamProbeInterval(0))
		t.Cleanup(w.Stop)
		w.Start()
		eventually(t, "the scan's one record", func() bool { return into.has("one") })
		now.advance(kvwatch.ScanIdleTimeout)
		arm.fireIdle(t)
		eventually(t, "the re-read to be armed", func() bool { return arm.waits() == 1 })
		if got := arm.asked(); got != 5*time.Second {
			t.Fatalf("the re-read waits %v, want 5s", got)
		}
		if len(into.completions()) != 0 || !into.has("gone-from-the-bucket") || w.Err() != nil || w.NeedsRead() {
			t.Fatalf("after the timed-out read: %d completed, kept %v, Err %v, NeedsRead %v; want none completed, the copy kept, no error, a re-read armed",
				len(into.completions()), into.has("gone-from-the-bucket"), w.Err(), w.NeedsRead())
		}
		refuses(t, w, "before any read completed")
		return w, into, arm, handle, conn
	}
	// completed waits for the re-read to complete and checks it swept the key the bucket does not
	// hold.
	completed := func(t *testing.T, w *kvwatch.Watcher, into *seen) {
		t.Helper()
		eventually(t, "the re-read's completion", w.Following)
		serves(t, w, "after the re-read")
		if into.has("gone-from-the-bucket") || !into.has("one") || !into.has("three") || len(into.completions()) != 1 {
			t.Fatalf("after the re-read: gone %v, one %v, three %v, %d completed; want the bucket's keys only, one read completed",
				into.has("gone-from-the-bucket"), into.has("one"), into.has("three"), len(into.completions()))
		}
	}

	t.Run("it reads again", func(t *testing.T) {
		w, into, arm, handle, _ := timedOut(t)
		arm.fireIt(t)
		completed(t, w, into)
		if got := handle.watches(); got != 2 {
			t.Fatalf("the handle was watched %d times, want the first read and the re-read", got)
		}
	})

	t.Run("a re-read whose setup fails is armed again", func(t *testing.T) {
		w, into, arm, _, _ := timedOut(t, fails(errors.New("the watch was refused")))
		arm.fireIt(t)
		if got := arm.waits(); got != 2 || w.NeedsRead() {
			t.Fatalf("after the failed re-read: %d waits armed, NeedsRead %v; want the re-read armed again", got, w.NeedsRead())
		}
		arm.fireIt(t)
		completed(t, w, into)
	})

	t.Run("Stop disarms it", func(t *testing.T) {
		w, _, arm, handle, _ := timedOut(t)
		w.Stop()
		arm.fireIt(t)
		if got := handle.watches(); got != 1 || w.NeedsRead() {
			t.Fatalf("after Stop: the handle was watched %d times, NeedsRead %v; want no re-read", got, w.NeedsRead())
		}
	})

	t.Run("a newer read disarms it", func(t *testing.T) {
		w, into, arm, _, conn := timedOut(t)
		if err := w.Rewatch(conn); err != nil {
			t.Fatalf("rewatch: %v", err)
		}
		completed(t, w, into)
		arm.fireIt(t)
		if reads := len(into.reads()); reads != 2 {
			t.Fatalf("%d reads began, want the timed-out one and the Rewatch's only", reads)
		}
	})
}

// A read whose watcher ends before its scan's nil -- the subscription ended under it -- removes
// nothing either: the copy keeps every key until a read completes.
func TestAReadThatEndsEarlyCompletesNothing(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	for _, key := range []string{"one", "two"} {
		if _, err := kv.Put(key, []byte("1")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	into := newSeen()
	into.hold("gone-from-the-bucket")
	w := kvwatch.New("test cache", scripted(kv, endsEarly(1)).handle(), into,
		kvwatch.WithAfterFunc((&armed{}).afterFunc()))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the early end's error", func() bool { return w.Err() != nil })
	if len(into.completions()) != 0 || !into.has("gone-from-the-bucket") || !w.NeedsRead() {
		t.Fatalf("after the early end: %d completed, kept %v, NeedsRead %v; want none completed, the copy kept, a read needed",
			len(into.completions()), into.has("gone-from-the-bucket"), w.NeedsRead())
	}
	refuses(t, w, "before any read completed")

	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	eventually(t, "the complete read", w.Following)
	if into.has("gone-from-the-bucket") || !into.has("two") {
		t.Fatalf("after the complete read: gone %v, two %v; want the bucket's keys only", into.has("gone-from-the-bucket"), into.has("two"))
	}
}

// The stale bound is at most half the bucket's TTL, read from the stream before each read: a copy
// whose records live a minute is not served 31 s into a stale episode, while one whose records live
// for good is, so a bucket recreated with a shorter TTL shortens the bound.
func TestABucketRecreatedWithAShorterTTLShortensTheStaleBound(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	into := newSeen()
	now := newClock()
	// No periodic probe: the test drives the read of the recreated bucket itself.
	w := kvwatch.New("test cache", kv, into, kvwatch.WithNow(now.Now), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	held := newHeldEnd(t)
	if err := kvwatch.WatchOn(w, scripted(kv, held.step()).handle()); err != nil {
		t.Fatalf("the held read: %v", err)
	}
	wait(t, held.reached, "the held read to deliver every record")
	now.advance(31 * time.Second)
	serves(t, w, "31 s into a stale episode on a bucket without a TTL")
	held.let()
	wait(t, held.handled, "the held read's end to be handled")

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName, TTL: time.Minute})
	shorter := newHeldEnd(t)
	if err := kvwatch.WatchOn(w, scripted(wrap(t, conn, recreated), shorter.step()).handle()); err != nil {
		t.Fatalf("the read of the recreated bucket: %v", err)
	}
	wait(t, shorter.reached, "the recreated bucket's read to deliver every record")
	if reads := into.reads(); reads[len(reads)-1].MaxAge != time.Minute {
		t.Fatalf("the recreated bucket's read = %+v, want MaxAge 1m", reads[len(reads)-1])
	}
	now.advance(31 * time.Second)
	refuses(t, w, "31 s into a stale episode on a bucket with a 1-minute TTL")
	shorter.let()
	wait(t, shorter.handled, "the recreated bucket's read end to be handled")
	serves(t, w, "after the recreated bucket's read completed")
}

// A NATS outage ends no watcher: nats.go reconnects forever and keeps the watcher's subscription
// open through it, so only the bus's disconnect hook says the copy stopped following. The hook
// starts the stale episode, the copy is refused once the bound is past (on the injected clock),
// a Rewatch that fails while the server is down leaves the episode as it was, and the read the
// reconnect hook starts once the server is back restores serving.
func TestADisconnectStartsAStaleEpisodeThatTheReadAfterTheReconnectEnds(t *testing.T) {
	srv, uri := testnats.Start(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	kv, err := bus.EnsureKeyValue(client.Conn, client.JS(), &natsgo.KeyValueConfig{Bucket: testBucketName})
	if err != nil {
		t.Fatalf("bucket: %v", err)
	}
	if _, err := kv.Put("kept", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	into := newSeen()
	now := newClock()
	w := kvwatch.New("test cache", kv, into, kvwatch.WithNow(now.Now))
	t.Cleanup(w.Stop)
	client.AddDisconnectHook(w.Disconnected)
	client.AddReconnectHook(w.Rewatch)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	srv.Stop(t)
	eventually(t, "the disconnect hook", func() bool { return !w.Following() })
	if err := w.Err(); err != nil || !w.NeedsRead() {
		t.Fatalf("during the outage: Err %v, NeedsRead %v; want the watcher still running and a read needed", err, w.NeedsRead())
	}
	serves(t, w, "within the bound of the disconnect")
	now.advance(staleBound + time.Second)
	refuses(t, w, "past the bound of the disconnect")
	if err := w.Rewatch(client.Conn); err == nil {
		t.Fatal("a Rewatch while the server is down succeeded")
	}
	refuses(t, w, "after a Rewatch failed during the outage")

	srv.Start(t)
	eventuallyWithin(t, 30*time.Second, "the reconnect hook's read to complete", w.Following)
	serves(t, w, "after the read that followed the reconnect")
	if !into.has("kept") || w.Err() != nil {
		t.Fatalf("after the reconnect: kept %v, Err %v; want the key and no error", into.has("kept"), w.Err())
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
	// appliedBeforeSwitch counts queued keys applied while late1 has been applied once: the first
	// watcher's applies before the switch. After it, that watcher's entries are dropped and the new
	// watcher's scan applies late1 first, so the count is final at the switch.
	appliedBeforeSwitch := 0
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
		if entry.Key() != "late1" && counts["late1"] == 1 {
			appliedBeforeSwitch++
		}
		counts[entry.Key()]++
		mu.Unlock()
	}

	w := kvwatch.New("test cache", kv, withApply{seen: newSeen(), before: apply})
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
	// its queue. If the wait released late1 before the Rewatch was parked on applyMu, the first
	// watcher applied the whole queue first and nothing below would test the dropped entries.
	mu.Lock()
	allBeforeSwitch := appliedBeforeSwitch == queued
	mu.Unlock()
	if allBeforeSwitch {
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
	jetstream.KeyValue
	entered, release chan struct{}
}

func (k *slowStopKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	w, err := k.KeyValue.Watch(ctx, keys, opts...)
	if err != nil {
		return nil, err
	}
	return &slowStopWatcher{KeyWatcher: w, entered: k.entered, release: k.release}, nil
}

type slowStopWatcher struct {
	jetstream.KeyWatcher
	entered, release chan struct{}
	once             sync.Once
}

func (w *slowStopWatcher) Stop() error {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.KeyWatcher.Stop()
}

// Two Rewatches overlap after the bucket was recreated -- the bus's reconnect hook and the
// listener's self-health rebuild both rewatch, and nothing serializes them. A sees the new stream,
// begins the new epoch's read and is still stopping the replaced watcher when B arms its own and
// reads the new bucket to completion, which sweeps the old bucket's key. A's read, overtaken before
// its watcher delivered anything, completes nothing when it does: the new bucket's key stays in the
// cache and the copy keeps following, with Err nil, so nothing would repair a sweep A ran late.
func TestConcurrentRewatchesOntoARecreatedBucketKeepTheNewKeys(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	if _, err := kv.Put("ghost", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	slow := &slowStopKV{KeyValue: kv.WatchHandle(), entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(slow.release) }) }
	t.Cleanup(release)
	into := newSeen()
	// No periodic probe: the test's two Rewatches are the only reads of the recreated bucket.
	w := kvwatch.New("test cache", bus.NewKeyValue(kv.Legacy(), slow, kv.Streams()), into, kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
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
	if !into.has("ghost") {
		t.Fatal("the old bucket's key was dropped when A's read began, before any read of the new bucket completed")
	}
	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("Rewatch B: %v", err)
	}
	eventually(t, "B's read to complete", w.Following)
	if !into.has("fresh") || into.has("ghost") {
		t.Fatalf("after B's read completed: fresh %v, ghost %v; want the new bucket's key only", into.has("fresh"), into.has("ghost"))
	}
	release()
	if err := <-doneA; err != nil {
		t.Fatalf("Rewatch A: %v", err)
	}
	// A's watcher runs from here, and its scan's end must complete nothing; nothing observable
	// marks that end, so the check waits as long as the rest of this file's negative checks do.
	time.Sleep(300 * time.Millisecond)
	reads, done := into.reads(), into.completions()
	if !into.has("fresh") || into.has("ghost") || !w.Following() || w.Err() != nil || len(done) != 2 {
		t.Fatalf("fresh %v, ghost %v, Following %v, Err %v, reads completed %+v: want the new bucket's key kept by B's read and A's read completing nothing",
			into.has("fresh"), into.has("ghost"), w.Following(), w.Err(), done)
	}
	if len(reads) != 3 || !reads[1].Recreated || reads[2].Recreated || reads[2].Epoch != 1 {
		t.Fatalf("reads begun %+v, want the first, A's recreated read of epoch 1, and B's read of the same epoch", reads)
	}
}

// heldWatchKV starts its watch on the bucket, then holds it until released: the watch it serves
// has read the stream and armed its watcher, and has not yet taken the locks. Once released, the
// watcher it hands out forwards the real one's updates, and ended closes once nats.go has closed
// those.
type heldWatchKV struct {
	jetstream.KeyValue
	legacy                  natsgo.KeyValue
	streams                 jetstream.JetStream
	watched, release, ended chan struct{}
	// watcher is the real watcher, set before watched closes.
	watcher jetstream.KeyWatcher
}

func holdWatch(kv bus.KeyValue) *heldWatchKV {
	return &heldWatchKV{KeyValue: kv.WatchHandle(), legacy: kv.Legacy(), streams: kv.Streams(),
		watched: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{})}
}

func (k *heldWatchKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(ctx, keys, opts...)
	k.watcher = watcher
	close(k.watched)
	<-k.release
	if err != nil {
		return nil, err
	}
	updates := make(chan jetstream.KeyValueEntry)
	go func() {
		defer close(k.ended)
		defer close(updates)
		for entry := range watcher.Updates() {
			updates <- entry
		}
	}()
	return &forwardedWatcher{KeyWatcher: watcher, updates: updates}, nil
}

// handle is k as the bus.KeyValue a watcher takes.
func (k *heldWatchKV) handle() bus.KeyValue { return bus.NewKeyValue(k.legacy, k, k.streams) }

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
	held := holdWatch(first)
	released := false
	t.Cleanup(func() {
		if !released {
			close(held.release)
		}
	})
	into := newSeen()
	w := kvwatch.New("test cache", held.handle(), into)
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
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: "kvwatch-test"})
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
		t.Fatalf("after the old stream's watch arrived: ghost %v, fresh %v, reads %+v; want the recreated bucket's keys only",
			into.has("ghost"), into.has("fresh"), into.reads())
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
// Updates buffer and blocks when it is full, holding the watcher's lock, and Stop only
// unsubscribes, so a watcher of an old bucket holding more keys than that, stopped with nothing
// reading it, keeps its subscription's goroutine parked for the life of the process.
func TestADiscardedWatchOfTheOldStreamIsDrainedUntilItEnds(t *testing.T) {
	_, uri := testnats.Start(t)
	conn, first := bucket(t, uri)
	for i := range 400 {
		if _, err := first.Put(fmt.Sprintf("old-%d", i), []byte("1")); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	held := holdWatch(first)
	released := false
	t.Cleanup(func() {
		if !released {
			close(held.release)
		}
	})
	into := newSeen()
	w := kvwatch.New("test cache", held.handle(), into)
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
	testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: "kvwatch-test"})
	liveConn, _ := bucket(t, uri)
	if err := w.Rewatch(liveConn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	close(held.release)
	released = true

	select {
	case <-held.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the discarded watcher never ended: it was left unread (its delivery goroutine parks on a full buffer) or never stopped")
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
	// firstScanApplied counts k001..k009 applied while k000 has been applied once: the first
	// watcher's applies before the switch. After it, that watcher's entries are dropped and the
	// current watcher's scan applies k000 first, so the count is final at the switch.
	var firstScanApplied atomic.Int32
	into := newSeen()
	apply := func(entry natsgo.KeyValueEntry) {
		if entry.Key() == "k000" && k0.Add(1) == 1 {
			enterOnce.Do(func() { close(entered) })
			<-release
		}
		if slow.Load() {
			time.Sleep(10 * time.Millisecond)
		}
		if key := entry.Key(); key > "k000" && key < "k010" && k0.Load() == 1 {
			firstScanApplied.Add(1)
		}
	}
	w := kvwatch.New("probe", first, withApply{seen: into, before: apply})
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
	// of its scan. If the wait released k000 before the Rewatch was parked on applyMu, the first
	// watcher applied k001..k009 and read its own sentinel while it was still current, and the
	// assertion below would pass without testing the sentinel at all.
	if firstScanApplied.Load() == 9 {
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
	// accountKeeper exists so the account is never emptied by the deletes below. nats-server
	// schedules the removal of the account's streams directory, in the background, on every stream
	// delete, not only on the last one; that removal is an os.Remove, which fails on a directory that
	// is not empty. Without the keeper, each delete below leaves the directory empty — once the
	// deleted stream's renamed copy, which goes in a goroutine of its own, is gone — so the removal
	// it schedules can succeed, and the restore after the second delete races it and fails. With
	// the keeper's stream in it, the directory is never empty, so every scheduled removal fails and
	// leaves it in place.
	const accountKeeper = "kvwatch-test-account-keeper"
	if _, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: accountKeeper}); err != nil {
		t.Fatalf("create the account keeper bucket: %v", err)
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
	live := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: "kvwatch-test"})
	if _, err := live.Put("live-key", []byte("1")); err != nil {
		t.Fatalf("put live-key: %v", err)
	}
	into := newSeen()
	w := kvwatch.New("probe cache", wrap(t, conn, live), into)
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the live bucket's key", func() bool { return into.has("live-key") })

	keeperPresent := false
	for name := range js.StreamNames() {
		keeperPresent = keeperPresent || name == "KV_"+accountKeeper
	}
	if !keeperPresent {
		t.Fatal("the account keeper bucket is gone before the delete: the delete would empty the account and the restore would race the streams-directory removal")
	}
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
	eventually(t, "the restored bucket's read to complete", w.Following)
	if w.Err() != nil || !into.has("restored-key") || into.has("live-key") || w.Epoch() == 0 {
		t.Fatalf("after three Rewatches onto the restored bucket: Err=%v, restored-key=%v, live-key=%v, Epoch=%d; "+
			"want the restored bucket's key only, read as a new epoch", w.Err(), into.has("restored-key"), into.has("live-key"), w.Epoch())
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
