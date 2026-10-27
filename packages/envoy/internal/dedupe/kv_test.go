package dedupe

import (
	"encoding/json"
	"errors"
	"regexp"
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

func TestKV_ThisBootsUnresolvedAttemptIsInFlightUntilAttemptedAge(t *testing.T) {
	kv := openTestBucket(t)
	clock := newTestClock()
	a := newTestKV(kv, "boot-a", clock)

	mustAttempt(t, a, "publish.abc", "ses_target", Proceed)
	mustAttempt(t, a, "publish.abc", "ses_target", InFlight)
	clock.Advance(AttemptedAge - time.Second)
	mustAttempt(t, a, "publish.abc", "ses_target", InFlight)
	clock.Advance(2 * time.Second)
	mustAttempt(t, a, "publish.abc", "ses_target", Proceed)
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
