package dedupe

import (
	"encoding/json"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	testDurable       = "listener-box-a"
	testTakeoverAfter = 35 * time.Second
)

// openTestBucket opens the listener state bucket on the test's NATS as the listener does.
func openTestBucket(t *testing.T) bus.KeyValue {
	t.Helper()
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	kv, err := bus.EnsureKeyValue(js, &nats.KeyValueConfig{
		Bucket:   bus.ListenerStateBucket,
		History:  1,
		TTL:      10 * time.Minute,
		Storage:  nats.FileStorage,
		Replicas: 1,
	})
	if err != nil {
		t.Fatalf("open the listener state bucket: %v", err)
	}
	return kv
}

// newTestKV makes boot's ledger over kv, aged against clock.
func newTestKV(kv bus.KeyValue, boot string, clock *testClock) *KV {
	return NewKV(kv, KVConfig{Durable: testDurable, Boot: boot, TakeoverAfter: testTakeoverAfter, Now: clock.Now})
}

func readRecord(t *testing.T, kv bus.KeyValue, key string) kvRecord {
	t.Helper()
	entry, err := kv.Get(key)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	var record kvRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		t.Fatalf("decode %s = %q: %v", key, entry.Value(), err)
	}
	return record
}

func TestKV_AFirstAttemptCreatesAnAttemptedRecordUnderTheMachinesDurable(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	ledger := newTestKV(kv, "boot-a", clock)

	mustAttempt(t, ledger, "publish.abc", "ses_target", Proceed)
	key := ledger.Key("publish.abc", "ses_target")
	if !regexp.MustCompile(`^delivery\.listener-box-a\.[0-9a-f]{32}\.ses_target$`).MatchString(key) {
		t.Fatalf("ledger key = %q, want delivery.<durable>.<32 hex>.<session id>", key)
	}
	record := readRecord(t, kv, key)
	if record.State != stateAttempted || record.Boot != "boot-a" || record.At == 0 {
		t.Fatalf("record = %+v, want attempted by boot-a with its time", record)
	}
}

func TestKV_ASessionIDThatIsNoKeyTokenIsEncoded(t *testing.T) {
	kv := openTestBucket(t)
	ledger := newTestKV(kv, "boot-a", newTestClock())
	for _, sessionID := range []string{"ses:colon", "ses.dotted.id", "ses with space", string(make([]byte, 200))} {
		mustAttempt(t, ledger, "publish.abc", sessionID, Proceed)
		mustAttempt(t, ledger, "publish.abc", sessionID, InFlight)
		if key := ledger.Key("publish.abc", sessionID); !regexp.MustCompile(`^delivery\.listener-box-a\.[0-9a-f]{32}\.=[0-9a-f]{32}$`).MatchString(key) {
			t.Fatalf("ledger key of %q = %q, want its session token hashed", sessionID, key)
		}
	}
	if ledger.Key("publish.abc", "ses:colon") == ledger.Key("publish.abc", "ses.dotted.id") {
		t.Fatal("two session ids share a ledger key")
	}
}

func TestKV_ADeliveredRecordIsSkippedByEveryBootUntilDeliveredAge(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	a := newTestKV(kv, "boot-a", clock)
	b := newTestKV(kv, "boot-b", clock)

	if err := a.Record(mustAttempt(t, a, "publish.abc", "ses_target", Proceed)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if record := readRecord(t, kv, a.Key("publish.abc", "ses_target")); record.State != stateDelivered || record.Boot != "boot-a" {
		t.Fatalf("record after Record = %+v, want delivered by boot-a", record)
	}
	mustAttempt(t, a, "publish.abc", "ses_target", Delivered)
	mustAttempt(t, b, "publish.abc", "ses_target", Delivered)
	mustAttempt(t, b, "publish.abc", "ses_other", Proceed)

	clock.Advance(DeliveredAge + time.Second)
	mustAttempt(t, b, "publish.abc", "ses_target", Proceed)
	if record := readRecord(t, kv, a.Key("publish.abc", "ses_target")); record.State != stateAttempted || record.Boot != "boot-b" {
		t.Fatalf("record after the take-over = %+v, want attempted by boot-b", record)
	}
}

// This process's attempt that nothing has settled is its prompt still running: InFlight until it is
// TakeoverAfter old, the longest a prompt takes and a margin, and taken over after that, as another
// boot's would be.
func TestKV_ThisProcesssUnsettledAttemptIsInFlightUntilTakeoverAfter(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	a := newTestKV(kv, "boot-a", clock)

	mustAttempt(t, a, "publish.abc", "ses_target", Proceed)
	mustAttempt(t, a, "publish.abc", "ses_target", InFlight)
	clock.Advance(testTakeoverAfter - time.Second)
	mustAttempt(t, a, "publish.abc", "ses_target", InFlight)
	clock.Advance(2 * time.Second)
	mustAttempt(t, a, "publish.abc", "ses_target", Proceed)
}

// faultyKV passes every call through to the bucket but the faults it names: lostCreateReply stores
// each Create and then answers it nats.ErrTimeout, as a write whose reply the connection lost does;
// failPut and failDelete answer nats.ErrTimeout and write nothing.
type faultyKV struct {
	nats.KeyValue
	lostCreateReply, failPut, failDelete bool
}

func (f faultyKV) Create(key string, value []byte) (uint64, error) {
	revision, err := f.KeyValue.Create(key, value)
	if err == nil && f.lostCreateReply {
		return 0, nats.ErrTimeout
	}
	return revision, err
}

func (f faultyKV) Put(key string, value []byte) (uint64, error) {
	if f.failPut {
		return 0, nats.ErrTimeout
	}
	return f.KeyValue.Put(key, value)
}

func (f faultyKV) Delete(key string, opts ...nats.DeleteOpt) error {
	if f.failDelete {
		return nats.ErrTimeout
	}
	return f.KeyValue.Delete(key, opts...)
}

// A create the bucket stored but whose reply was lost leaves an attempted record of this boot that
// no prompt of this process is behind: the retry takes it over and proceeds, rather than skipping as
// in flight a pair nothing prompted.
func TestKV_ARetryAfterACreateWhoseReplyWasLostProceeds(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	ledger := newTestKV(bus.KeyValue{KeyValue: faultyKV{KeyValue: kv.KeyValue, lostCreateReply: true}}, "boot-a", clock)

	if _, err := ledger.Attempt("publish.abc", "ses_target"); !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("Attempt whose create reply was lost: %v, want nats.ErrTimeout", err)
	}
	if record := readRecord(t, kv, ledger.Key("publish.abc", "ses_target")); record.State != stateAttempted || record.Boot != "boot-a" {
		t.Fatalf("record the lost create stored = %+v, want attempted by boot-a", record)
	}
	mustAttempt(t, ledger, "publish.abc", "ses_target", Proceed)
	mustAttempt(t, ledger, "publish.abc", "ses_target", InFlight)
}

// A clear the bucket refuses leaves the attempted record of this boot in place, but the attempt is
// given up all the same: the next attempt for the pair takes the record over and proceeds.
func TestKV_AnAttemptWhoseClearFailedIsTakenOverByTheNextAttempt(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	ledger := newTestKV(bus.KeyValue{KeyValue: faultyKV{KeyValue: kv.KeyValue, failDelete: true}}, "boot-a", clock)

	first := mustAttempt(t, ledger, "publish.abc", "ses_target", Proceed)
	if err := ledger.Clear(first); !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("Clear on a bucket that refuses deletes: %v, want nats.ErrTimeout", err)
	}
	retry := mustAttempt(t, ledger, "publish.abc", "ses_target", Proceed)
	if retry.revision <= first.revision {
		t.Fatalf("the retry's revision %d does not replace the uncleared attempt's %d", retry.revision, first.revision)
	}
	mustAttempt(t, ledger, "publish.abc", "ses_target", InFlight)
}

// A prompt whose answer never came (a slow server can take prompt_async and answer 204 after the
// client gave up, #389) is kept: the pair is InFlight until the attempt is AttemptedAge old, well
// past the takeover age, and taken over after that.
func TestKV_AKeptAttemptIsInFlightUntilAttemptedAge(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	a := newTestKV(kv, "boot-a", clock)

	a.Keep(mustAttempt(t, a, "publish.abc", "ses_target", Proceed))
	clock.Advance(testTakeoverAfter + time.Second)
	mustAttempt(t, a, "publish.abc", "ses_target", InFlight)
	clock.Advance(AttemptedAge - testTakeoverAfter - 2*time.Second)
	mustAttempt(t, a, "publish.abc", "ses_target", InFlight)
	clock.Advance(2 * time.Second)
	mustAttempt(t, a, "publish.abc", "ses_target", Proceed)
}

// A delivery whose delivered record the bucket refuses was delivered all the same: the attempt is
// kept, and a repeat within AttemptedAge is not prompted again.
func TestKV_ADeliveryWhoseRecordFailedIsKept(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	ledger := newTestKV(bus.KeyValue{KeyValue: faultyKV{KeyValue: kv.KeyValue, failPut: true}}, "boot-a", clock)

	if err := ledger.Record(mustAttempt(t, ledger, "publish.abc", "ses_target", Proceed)); !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("Record on a bucket that refuses puts: %v, want nats.ErrTimeout", err)
	}
	clock.Advance(testTakeoverAfter + time.Second)
	mustAttempt(t, ledger, "publish.abc", "ses_target", InFlight)
}

// What this process holds of its own attempts is swept out once they are past every age they hold
// for, so it stays as small as the attempts of the last AttemptedAge.
func TestKV_AWriteSweepsTheAttemptsThisProcessHoldsPastTheirAge(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	a := newTestKV(kv, "boot-a", clock)

	mustAttempt(t, a, "publish.unsettled", "ses_target", Proceed)
	a.Keep(mustAttempt(t, a, "publish.kept", "ses_target", Proceed))
	clock.Advance(AttemptedAge + sweepEvery)
	mustAttempt(t, a, "publish.fresh", "ses_target", Proceed)
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.held[Key{DedupeKey: "publish.fresh", SessionID: "ses_target"}]; !ok || len(a.held) != 1 {
		t.Fatalf("attempts held after the sweep = %v, want only the fresh one", a.held)
	}
}

// Attempts of one process for one pair run one after the other: exactly one proceeds, and every
// other finds that attempt held.
func TestKV_ConcurrentAttemptsOfOneProcessProceedOnce(t *testing.T) {
	kv := openTestBucket(t)
	a := newTestKV(kv, "boot-a", newTestClock())
	verdicts := make(chan Verdict, 20)
	var wg sync.WaitGroup
	for range cap(verdicts) {
		wg.Go(func() {
			attempt, err := a.Attempt("publish.abc", "ses_target")
			if err != nil {
				t.Errorf("Attempt: %v", err)
			}
			verdicts <- attempt.Verdict
		})
	}
	wg.Wait()
	close(verdicts)
	counts := map[Verdict]int{}
	for verdict := range verdicts {
		counts[verdict]++
	}
	if counts[Proceed] != 1 || counts[InFlight] != cap(verdicts)-1 {
		t.Fatalf("verdicts = %v, want one proceed and the rest in flight", counts)
	}
}

func TestKV_AnotherBootsYoungAttemptIsDeferredUntilItCanBeTakenOver(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	a := newTestKV(kv, "boot-a", clock)
	b := newTestKV(kv, "boot-b", clock)

	mustAttempt(t, a, "publish.abc", "ses_target", Proceed)
	clock.Advance(10 * time.Second)
	deferred := mustAttempt(t, b, "publish.abc", "ses_target", Deferred)
	// The record's age is its KV timestamp against the test clock, which stood still from before
	// the write while real time moved, so the age is a little short of the 10 s the clock advanced.
	if want := testTakeoverAfter - 10*time.Second; deferred.RetryIn < want-time.Second || deferred.RetryIn > want+5*time.Second {
		t.Fatalf("RetryIn = %s, want about %s: the time until boot-a's attempt is %s old", deferred.RetryIn, want, testTakeoverAfter)
	}
	if err := b.Clear(deferred); err != nil {
		t.Fatalf("Clear of a deferred answer: %v", err)
	}
	if record := readRecord(t, kv, a.Key("publish.abc", "ses_target")); record.Boot != "boot-a" {
		t.Fatalf("a deferred answer changed the record: %+v", record)
	}
}

func TestKV_AGoneBootsYoungAttemptIsTakenOverAtOnce(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	a := newTestKV(kv, "boot-a", clock)
	gone := map[string]bool{}
	b := NewKV(kv, KVConfig{Durable: testDurable, Boot: "boot-b", TakeoverAfter: testTakeoverAfter, Now: clock.Now,
		GoneBoot: func(boot string) bool { return gone[boot] }})

	mustAttempt(t, a, "publish.abc", "ses_target", Proceed)
	key := a.Key("publish.abc", "ses_target")
	before, err := kv.Get(key)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	clock.Advance(time.Second)
	mustAttempt(t, b, "publish.abc", "ses_target", Deferred)

	// Boot A no longer holds the lease: boot B took it over from A.
	gone["boot-a"] = true
	taken := mustAttempt(t, b, "publish.abc", "ses_target", Proceed)
	after, err := kv.Get(key)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	if after.Revision() <= before.Revision() || taken.revision != after.Revision() {
		t.Fatalf("revision after the take-over = %d (attempt's %d), want past boot-a's %d and the attempt's own",
			after.Revision(), taken.revision, before.Revision())
	}
	if record := readRecord(t, kv, key); record.State != stateAttempted || record.Boot != "boot-b" {
		t.Fatalf("record after the take-over = %+v, want attempted by boot-b", record)
	}
	mustAttempt(t, b, "publish.abc", "ses_target", InFlight)
}

func TestKV_AnotherBootsOldAttemptIsTakenOverAndItsClearLeavesTheNewOne(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	a := newTestKV(kv, "boot-a", clock)
	b := newTestKV(kv, "boot-b", clock)

	stale := mustAttempt(t, a, "publish.abc", "ses_target", Proceed)
	clock.Advance(testTakeoverAfter + time.Second)
	taken := mustAttempt(t, b, "publish.abc", "ses_target", Proceed)
	key := a.Key("publish.abc", "ses_target")
	if record := readRecord(t, kv, key); record.State != stateAttempted || record.Boot != "boot-b" {
		t.Fatalf("record after the take-over = %+v, want attempted by boot-b", record)
	}

	// Boot A's prompt fails late: its clear is at the revision it wrote, which boot B replaced.
	if err := a.Clear(stale); err != nil {
		t.Fatalf("Clear of the replaced attempt: %v", err)
	}
	if record := readRecord(t, kv, key); record.Boot != "boot-b" {
		t.Fatalf("the replaced attempt's clear removed boot-b's record: %+v", record)
	}
	mustAttempt(t, b, "publish.abc", "ses_target", InFlight)

	if err := b.Record(taken); err != nil {
		t.Fatalf("Record: %v", err)
	}
	mustAttempt(t, a, "publish.abc", "ses_target", Delivered)
}

func TestKV_ClearLetsTheNextAttemptProceed(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	a := newTestKV(kv, "boot-a", clock)
	b := newTestKV(kv, "boot-b", clock)

	attempt := mustAttempt(t, a, "publish.abc", "ses_target", Proceed)
	if err := a.Clear(attempt); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, err := kv.Get(a.Key("publish.abc", "ses_target")); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("read after Clear: %v, want the key gone", err)
	}
	retry := mustAttempt(t, b, "publish.abc", "ses_target", Proceed)

	// A clear after the pair was recorded delivered leaves the delivery.
	if err := b.Record(retry); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := b.Clear(retry); err != nil {
		t.Fatalf("Clear after Record: %v", err)
	}
	mustAttempt(t, a, "publish.abc", "ses_target", Delivered)
}

// Every age is the KV timestamp's, the server's clock every pod shares, never the time a writer
// put in the value: a record whose value claims to be an hour old still holds when the bucket wrote
// it just now.
func TestKV_AgesAreReadFromTheEntrysKVTimestamp(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	b := newTestKV(kv, "boot-b", clock)
	key := b.Key("publish.abc", "ses_target")
	value, _ := json.Marshal(kvRecord{State: stateAttempted, Boot: "boot-a", At: clock.Now().Add(-time.Hour).UnixMilli()})
	if _, err := kv.Put(key, value); err != nil {
		t.Fatalf("write boot-a's record: %v", err)
	}
	mustAttempt(t, b, "publish.abc", "ses_target", Deferred)
}

func TestKV_ARecordThisBuildCannotReadIsTreatedAsAnotherBootsAttempt(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	a := newTestKV(kv, "boot-a", clock)
	if _, err := kv.Put(a.Key("publish.abc", "ses_target"), []byte("not json")); err != nil {
		t.Fatalf("write an unreadable record: %v", err)
	}
	mustAttempt(t, a, "publish.abc", "ses_target", Deferred)
	clock.Advance(testTakeoverAfter + time.Second)
	mustAttempt(t, a, "publish.abc", "ses_target", Proceed)
}

// failingKV fails every call the ledger makes, as a bucket the server does not answer does.
type failingKV struct {
	nats.KeyValue
	err error
}

func (f failingKV) Create(string, []byte) (uint64, error)  { return 0, f.err }
func (f failingKV) Get(string) (nats.KeyValueEntry, error) { return nil, f.err }
func (f failingKV) Put(string, []byte) (uint64, error)     { return 0, f.err }
func (f failingKV) Delete(string, ...nats.DeleteOpt) error { return f.err }

func TestKV_ABucketThatDoesNotAnswerIsAnError(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	healthy := newTestKV(kv, "boot-a", clock)
	attempt := mustAttempt(t, healthy, "publish.abc", "ses_target", Proceed)

	ledger := newTestKV(bus.KeyValue{KeyValue: failingKV{KeyValue: kv.KeyValue, err: nats.ErrTimeout}}, "boot-a", clock)
	if _, err := ledger.Attempt("publish.abc", "ses_other"); !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("Attempt on a bucket that times out: %v, want nats.ErrTimeout", err)
	}
	if err := ledger.Record(attempt); !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("Record on a bucket that times out: %v, want nats.ErrTimeout", err)
	}
	if err := ledger.Clear(attempt); !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("Clear on a bucket that times out: %v, want nats.ErrTimeout", err)
	}
}
