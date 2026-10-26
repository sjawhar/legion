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

const (
	// probeEvery is the watcher's default stream probe interval, the delay of every probe wait.
	probeEvery = 2 * time.Second
	// rereadNow is the delay of the re-read a read of the stream arms when it finds the bucket
	// recreated: none.
	rereadNow = time.Duration(0)
	// staleConfirmation is past the age of a stream confirmation the watcher applies a record on
	// without reading the stream first (4 s).
	staleConfirmation = 5 * time.Second

	recreatedUnderWatcher = "test cache bucket was recreated under its watcher; its cache stops following and is re-read from the new bucket, kept until the read completes"
	deletedUnderWatcher   = "test cache bucket was deleted under its watcher; its cache stops following and is kept until the bucket is created again and read"
)

// pending stands in for time.AfterFunc: it keeps every wait the watcher arms, with its delay, so a
// test fires a probe or a re-read itself, and records which waits the watcher stopped.
type pending struct {
	mu    sync.Mutex
	waits []*pendingWait
}

// pendingWait is one wait a pending armed.
type pendingWait struct {
	delay   time.Duration
	fire    func()
	stopped atomic.Bool
}

func (w *pendingWait) Stop() bool {
	w.stopped.Store(true)
	return true
}

// afterFunc is the AfterFunc WithAfterFunc takes.
func (p *pending) afterFunc() kvwatch.AfterFunc {
	return func(d time.Duration, f func()) kvwatch.Timer {
		p.mu.Lock()
		defer p.mu.Unlock()
		armed := &pendingWait{delay: d, fire: f}
		p.waits = append(p.waits, armed)
		return armed
	}
}

// armed is every wait of delay d, in the order the watcher armed them.
func (p *pending) armed(d time.Duration) []*pendingWait {
	p.mu.Lock()
	defer p.mu.Unlock()
	var found []*pendingWait
	for _, armed := range p.waits {
		if armed.delay == d {
			found = append(found, armed)
		}
	}
	return found
}

// last is the wait of delay d the watcher armed last, failing the test when it armed none.
func (p *pending) last(t *testing.T, d time.Duration) *pendingWait {
	t.Helper()
	armed := p.armed(d)
	if len(armed) == 0 {
		t.Fatalf("the watcher armed no wait of %v", d)
	}
	return armed[len(armed)-1]
}

// countingKV counts the reads of its bucket's stream (Stream, the STREAM.INFO a watcher reads).
type countingKV struct {
	jetstream.JetStream
	statuses atomic.Int32
}

func (k *countingKV) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	k.statuses.Add(1)
	return k.JetStream.Stream(ctx, name)
}

func (k *countingKV) reads() int { return int(k.statuses.Load()) }

// injecting is a watch whose watcher delivers the bucket's own records and, besides them, each
// record the test sends on records, as the consumer nats.go moved onto a recreated bucket would.
func injecting(records <-chan jetstream.KeyValueEntry) watchStep {
	return func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		watcher, err := kv.WatchAll(ctx)
		if err != nil {
			return nil, err
		}
		updates := make(chan jetstream.KeyValueEntry)
		go func() {
			defer close(updates)
			for {
				select {
				case entry, ok := <-watcher.Updates():
					if !ok {
						return
					}
					updates <- entry
				case entry := <-records:
					updates <- entry
				}
			}
		}()
		return &forwardedWatcher{KeyWatcher: watcher, updates: updates}, nil
	}
}

// recreate deletes the test bucket and creates it again holding keys, and returns the new bucket.
func recreate(t *testing.T, conn *natsgo.Conn, keys ...string) natsgo.KeyValue {
	t.Helper()
	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	for _, key := range keys {
		if _, err := recreated.Put(key, []byte("new")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	return recreated
}

// readWholeInEpochOne fails the test unless the watcher has read the recreated bucket whole as
// the second read, of epoch 1, which swept every key only the old bucket held.
func readWholeInEpochOne(t *testing.T, w *kvwatch.Watcher, into *seen, kept, swept string) {
	t.Helper()
	eventually(t, "the recreated bucket's read to complete", w.Following)
	if !into.has(kept) || into.has(swept) || w.Epoch() != 1 {
		t.Fatalf("after the re-read: %s %v, %s %v, Epoch %d; want the new bucket's key only, in epoch 1",
			kept, into.has(kept), swept, into.has(swept), w.Epoch())
	}
	reads, done := into.reads(), into.completions()
	if len(reads) != 2 || !reads[1].Recreated || reads[1].Epoch != 1 || len(done) != 2 || done[1].Epoch != 1 {
		t.Fatalf("reads begun %+v, completed %+v; want the first and one recreated read of epoch 1, both completed", reads, done)
	}
}

// A probe that finds the bucket's stream unchanged records when it asked as the latest
// confirmation and changes nothing else: the copy still follows, no read begins, and the next probe
// is armed. A record delivered on that fresh confirmation is applied without another read.
func TestAProbeOfTheSameStreamOnlyConfirmsIt(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	counted := &countingKV{JetStream: kv.Streams()}
	into := newSeen()
	now := newClock()
	timers := &pending{}
	w := kvwatch.New("test cache", bus.NewKeyValue(kv.KeyValue, kv.WatchHandle(), counted), into,
		kvwatch.WithNow(now.Now), kvwatch.WithAfterFunc(timers.afterFunc()))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)
	if probes := len(timers.armed(probeEvery)); probes != 1 {
		t.Fatalf("the first read armed %d stream probes of %v, want one", probes, probeEvery)
	}

	now.advance(time.Minute)
	before := counted.reads()
	timers.last(t, probeEvery).fire()
	if got := counted.reads() - before; got != 1 {
		t.Fatalf("the probe read the stream %d times, want once", got)
	}
	if !w.Following() || len(into.reads()) != 1 || len(timers.armed(rereadNow)) != 0 || len(timers.armed(probeEvery)) != 2 {
		t.Fatalf("after a probe of the same stream: Following %v, %d reads begun, %d re-reads armed, %d probes armed; want following, one read, no re-read, the next probe armed",
			w.Following(), len(into.reads()), len(timers.armed(rereadNow)), len(timers.armed(probeEvery)))
	}

	if _, err := kv.Put("after-the-probe", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	eventually(t, "the record written after the probe", func() bool { return into.has("after-the-probe") })
	if got := counted.reads() - before; got != 1 {
		t.Fatalf("the stream was read %d times in all after the probe, want only the probe's: its confirmation was fresh for the record", got)
	}
}

// A record delivered while the copy follows its bucket is applied at once on a fresh confirmation
// of the bucket's stream, and waits for one read of the stream on one older than 4 s; that read
// confirms the stream again, so the next record is applied at once.
func TestARecordOnAnOldConfirmationWaitsForAReadOfTheStream(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	counted := &countingKV{JetStream: kv.Streams()}
	var mu sync.Mutex
	readsAt := map[string]int{}
	into := withApply{seen: newSeen(), before: func(entry natsgo.KeyValueEntry) {
		mu.Lock()
		defer mu.Unlock()
		readsAt[entry.Key()] = counted.reads()
	}}
	readsBefore := func(key string) int {
		mu.Lock()
		defer mu.Unlock()
		return readsAt[key]
	}
	now := newClock()
	w := kvwatch.New("test cache", bus.NewKeyValue(kv.KeyValue, kv.WatchHandle(), counted), into,
		kvwatch.WithNow(now.Now), kvwatch.WithAfterFunc((&pending{}).afterFunc()))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)
	base := counted.reads()

	put := func(key string) {
		t.Helper()
		if _, err := kv.Put(key, []byte("1")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		eventually(t, key+" to be applied", func() bool { return into.has(key) })
	}
	put("fresh")
	if got := readsBefore("fresh") - base; got != 0 {
		t.Fatalf("a record on a fresh confirmation waited for %d reads of the stream, want none", got)
	}
	now.advance(staleConfirmation)
	put("stale")
	if got := readsBefore("stale") - base; got != 1 || !w.Following() {
		t.Fatalf("a record on a %v-old confirmation was applied after %d reads of the stream, Following %v; want one read before it, still following",
			staleConfirmation, got, w.Following())
	}
	put("confirmed-again")
	if got := readsBefore("confirmed-again") - base; got != 1 {
		t.Fatalf("a record after the gate's read was applied after %d reads of the stream in all, want only that one", got)
	}
}

// A record nats.go's moved consumer delivers from a recreated bucket, while the copy still
// follows on an old confirmation, is applied only after the read of the stream it waits for has
// found the new bucket and stopped the copy following; the watcher then reads the new bucket whole.
func TestARecordFromARecreatedBucketIsAppliedOnlyOnceTheCopyStoppedFollowing(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	if _, err := kv.Put("ghost", []byte("old")); err != nil {
		t.Fatalf("put: %v", err)
	}
	records := make(chan jetstream.KeyValueEntry)
	var (
		w           *kvwatch.Watcher
		mu          sync.Mutex
		followingAt = map[string]bool{}
	)
	into := newSeen()
	cache := withApply{seen: into, before: func(entry natsgo.KeyValueEntry) {
		following := w.Following()
		mu.Lock()
		defer mu.Unlock()
		followingAt[entry.Key()] = following
	}}
	now := newClock()
	timers := &pending{}
	w = kvwatch.New("test cache", scripted(kv, injecting(records)).handle(), cache,
		kvwatch.WithNow(now.Now), kvwatch.WithAfterFunc(timers.afterFunc()))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	recreated := recreate(t, conn, "fresh")
	entry := jetStreamEntry(t, wrap(t, conn, recreated), "fresh")
	now.advance(staleConfirmation)
	records <- entry
	eventually(t, "the new bucket's record to be applied", func() bool { return into.has("fresh") })
	mu.Lock()
	whileFollowing := followingAt["fresh"]
	mu.Unlock()
	if whileFollowing || w.Following() {
		t.Fatalf("the new bucket's record was applied with Following %v (now %v); want the copy stopped following first", whileFollowing, w.Following())
	}
	if rereads := len(timers.armed(rereadNow)); rereads != 1 || len(into.reads()) != 1 {
		t.Fatalf("after the record: %d re-reads armed, %d reads begun; want one re-read armed, not yet begun", rereads, len(into.reads()))
	}

	timers.last(t, rereadNow).fire()
	readWholeInEpochOne(t, w, into, "fresh", "ghost")
}

// A probe that finds another stream behind the bucket -- deleted and created again under a watcher
// that goes on running -- stops the copy following at once and starts one re-read, however many
// probes find it before that read installs. The re-read is a new epoch, and its completion sweeps
// the keys only the old bucket held. The finding is logged once, at INFO.
func TestAProbeThatFindsANewStreamReadsTheNewBucketInANewEpoch(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	if _, err := kv.Put("ghost", []byte("old")); err != nil {
		t.Fatalf("put: %v", err)
	}
	into := newSeen()
	timers := &pending{}
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", kv, into, kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithLogger(logger))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	recreate(t, conn, "fresh")
	timers.last(t, probeEvery).fire()
	if w.Following() || !into.has("ghost") || w.NeedsRead() {
		t.Fatalf("after the probe found a new stream: Following %v, ghost kept %v, NeedsRead %v; want not following, the copy kept, a re-read due",
			w.Following(), into.has("ghost"), w.NeedsRead())
	}
	timers.last(t, probeEvery).fire()
	if rereads := len(timers.armed(rereadNow)); rereads != 1 || len(into.reads()) != 1 {
		t.Fatalf("two probes of the new stream armed %d re-reads and began %d reads, want one re-read armed, not yet begun", rereads, len(into.reads()))
	}
	if lines := matching(records(), "INFO", recreatedUnderWatcher); len(lines) != 1 || lines[0]["bucket"] != testBucketName {
		t.Fatalf("the recreate was logged %v, want one INFO line naming bucket %s", lines, testBucketName)
	}

	timers.last(t, rereadNow).fire()
	readWholeInEpochOne(t, w, into, "fresh", "ghost")
}

// A probe that finds no stream behind the bucket stops the copy following, which cannot follow a
// missing bucket, and reads nothing; probing goes on, and the probe that finds the bucket created
// again starts the re-read, which reads it whole in a new epoch.
func TestAProbeReadsABucketDeletedThenCreatedAgainOnceItIsBack(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	if _, err := kv.Put("ghost", []byte("old")); err != nil {
		t.Fatalf("put: %v", err)
	}
	into := newSeen()
	timers := &pending{}
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", kv, into, kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithLogger(logger))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	timers.last(t, probeEvery).fire()
	timers.last(t, probeEvery).fire()
	if w.Following() || !into.has("ghost") || len(timers.armed(rereadNow)) != 0 {
		t.Fatalf("after two probes of the deleted bucket: Following %v, ghost kept %v, %d re-reads armed; want not following, the copy kept, no re-read",
			w.Following(), into.has("ghost"), len(timers.armed(rereadNow)))
	}
	if lines := matching(records(), "INFO", deletedUnderWatcher); len(lines) != 1 {
		t.Fatalf("the delete was logged %v, want one INFO line", lines)
	}

	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	if _, err := recreated.Put("fresh", []byte("new")); err != nil {
		t.Fatalf("put: %v", err)
	}
	timers.last(t, probeEvery).fire()
	if rereads := len(timers.armed(rereadNow)); rereads != 1 {
		t.Fatalf("the probe of the bucket created again armed %d re-reads, want one", rereads)
	}
	timers.last(t, rereadNow).fire()
	readWholeInEpochOne(t, w, into, "fresh", "ghost")
}

// A probe belongs to the generation that armed it. A watch that installs stops the replaced
// generation's probe and arms its own, and Stop stops the current one; a probe callback that runs
// anyway, as a time.AfterFunc one already started can, reads nothing and arms nothing.
func TestStopAndANewerWatchDisarmTheProbe(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	counted := &countingKV{JetStream: kv.Streams()}
	timers := &pending{}
	w := kvwatch.New("test cache", bus.NewKeyValue(kv.KeyValue, kv.WatchHandle(), counted), newSeen(), kvwatch.WithAfterFunc(timers.afterFunc()))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read's completion", w.Following)
	replaced := timers.last(t, probeEvery)

	if err := kvwatch.WatchOn(w, bus.NewKeyValue(kv.KeyValue, kv.WatchHandle(), counted)); err != nil {
		t.Fatalf("the newer watch: %v", err)
	}
	eventually(t, "the newer read's completion", w.Following)
	current := timers.last(t, probeEvery)
	if current == replaced || !replaced.stopped.Load() {
		t.Fatalf("after a newer watch: its own probe armed %v, the replaced one stopped %v; want both", current != replaced, replaced.stopped.Load())
	}
	before, probes := counted.reads(), len(timers.armed(probeEvery))
	replaced.fire()
	if counted.reads() != before || len(timers.armed(probeEvery)) != probes {
		t.Fatal("the replaced generation's probe read the stream or armed another probe")
	}

	w.Stop()
	if !current.stopped.Load() {
		t.Fatal("Stop left the probe armed")
	}
	current.fire()
	if counted.reads() != before || len(timers.armed(probeEvery)) != probes {
		t.Fatal("a probe after Stop read the stream or armed another probe")
	}
}

// A read whose scan ended after its bucket was deleted and created again under it completes
// nothing: the check of the stream before the sweep finds the new bucket, so the cache removes
// nothing on a read that may mix two streams, the copy does not follow, and the watcher reads the
// new bucket, whose completion sweeps what only the old bucket and the copy held.
func TestACompletionWhoseStreamChangedUnderTheScanSweepsNothing(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	if _, err := kv.Put("ghost", []byte("old")); err != nil {
		t.Fatalf("put: %v", err)
	}
	into := newSeen()
	into.hold("held-by-the-copy")
	timers := &pending{}
	held := newHeldEnd(t)
	w := kvwatch.New("test cache", scripted(kv, held.step()).handle(), into, kvwatch.WithAfterFunc(timers.afterFunc()))
	t.Cleanup(w.Stop)
	w.Start()
	wait(t, held.reached, "the first read to deliver every record")

	recreate(t, conn, "fresh")
	held.let()
	wait(t, held.handled, "the first read's end to be handled")
	if len(into.completions()) != 0 || !into.has("held-by-the-copy") || !into.has("ghost") || w.Following() {
		t.Fatalf("after the scan ended on a recreated bucket: %d reads completed, held key kept %v, ghost kept %v, Following %v; want none completed, everything kept, not following",
			len(into.completions()), into.has("held-by-the-copy"), into.has("ghost"), w.Following())
	}
	if rereads := len(timers.armed(rereadNow)); rereads != 1 || len(into.reads()) != 1 {
		t.Fatalf("%d re-reads armed and %d reads begun, want one re-read armed, not yet begun", rereads, len(into.reads()))
	}

	timers.last(t, rereadNow).fire()
	eventually(t, "the recreated bucket's read to complete", w.Following)
	if !into.has("fresh") || into.has("ghost") || into.has("held-by-the-copy") || w.Epoch() != 1 {
		t.Fatalf("after the re-read: fresh %v, ghost %v, held key %v, Epoch %d; want the new bucket's key only, in epoch 1",
			into.has("fresh"), into.has("ghost"), into.has("held-by-the-copy"), w.Epoch())
	}
	if done := into.completions(); len(done) != 1 || done[0].Epoch != 1 || !done[0].Recreated {
		t.Fatalf("reads completed %+v, want only the recreated bucket's, of epoch 1", done)
	}
}
