package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	leaseTestMachine = "box-a"
	leaseTestKey     = "lease.listener-box-a"
)

// fakeLeaseClock is a monotonic clock a test moves by hand. Two leases sharing one run on one
// clock, as two pods whose clocks run at the same rate.
type fakeLeaseClock struct {
	mu sync.Mutex
	at time.Time
}

func newFakeLeaseClock() *fakeLeaseClock {
	return &fakeLeaseClock{at: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeLeaseClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeLeaseClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

// leaseBucket opens the listener state bucket on a JetStream account of the test's own.
func leaseBucket(t *testing.T) bus.KeyValue {
	t.Helper()
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	kv, err := openListenerState(conn, 1, listenerStateRequestWait)
	if err != nil {
		t.Fatalf("open the listener state bucket: %v", err)
	}
	return kv
}

func testLease(kv bus.KeyValue, instance string, cfg leaseConfig, now func() time.Time) *lease {
	return newLease(kv, leaseTestMachine, instance, cfg, now, logging.NewWithWriter(leaseTestMachine, io.Discard))
}

// readLease reads the lease record as the bucket holds it.
func readLease(t *testing.T, kv bus.KeyValue) (leaseRecord, natsgo.KeyValueEntry) {
	t.Helper()
	entry, err := kv.Get(leaseTestKey)
	if err != nil {
		t.Fatalf("read the lease record: %v", err)
	}
	record, err := decodeLeaseRecord(entry.Value())
	if err != nil {
		t.Fatalf("decode the lease record %s: %v", entry.Value(), err)
	}
	return record, entry
}

// putLease writes a lease record as another pod would have, and returns its revision.
func putLease(t *testing.T, kv bus.KeyValue, record string) uint64 {
	t.Helper()
	rev, err := kv.Put(leaseTestKey, []byte(record))
	if err != nil {
		t.Fatalf("write the lease record: %v", err)
	}
	return rev
}

// drainChanges empties a lease's change signal and reports whether it held one.
func drainChanges(l *lease) bool {
	select {
	case <-l.Changes():
		return true
	default:
		return false
	}
}

func awaitLease(t *testing.T, what string, within time.Duration, cond func() bool) time.Time {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return time.Now()
}

// A standby that finds no record takes the lease with Create, reports it taken from an absent
// record, and starts the machine's service at the server's timestamp of its acquiring write.
func TestLeaseTakesAnAbsentRecordAndStartsServiceSince(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	before := time.Now()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	a.SetDeliverSubject("_INBOX.listener-box-a")

	if wait := a.step(); wait != leaseRenewEvery {
		t.Fatalf("step after taking the lease waits %s, want the renewal interval %s", wait, leaseRenewEvery)
	}
	after := time.Now()
	status := a.Status()
	if !status.Holding || status.Term != 1 || status.How != leaseTakenAbsent || status.ActiveInstance != "pod-a" {
		t.Fatalf("status = %+v, want holding term 1 taken from an absent record by pod-a", status)
	}
	if !drainChanges(a) {
		t.Fatal("the acquisition signalled no change")
	}
	record, entry := readLease(t, kv)
	if record.Holder != "pod-a" || record.Released || record.DeliverSubject != "_INBOX.listener-box-a" {
		t.Fatalf("record = %+v, want pod-a's unreleased record naming its deliver subject", record.leaseFields)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(record.Boot) {
		t.Fatalf("boot = %q, want 16 hex digits", record.Boot)
	}
	// The acquiring write is followed at once by a renewal storing its timestamp, so the record's
	// own write is that renewal and service_since is the earlier write's time.
	if record.ServiceSince.IsZero() || !record.ServiceSince.Equal(record.AcquiredAt) || record.ServiceSince.After(entry.Created()) {
		t.Fatalf("service_since %v, acquired_at %v, record written %v: want service_since = acquired_at, the acquiring write's time",
			record.ServiceSince, record.AcquiredAt, entry.Created())
	}
	if record.ServiceSince.Before(before.Add(-time.Second)) || record.ServiceSince.After(after.Add(time.Second)) {
		t.Fatalf("service_since %v is not the server's time of a write made between %v and %v", record.ServiceSince, before, after)
	}
	if !status.ServiceSince.Equal(record.ServiceSince) {
		t.Fatalf("status service_since %v, record %v", status.ServiceSince, record.ServiceSince)
	}
}

// A standby takes an unreleased record nobody renews only once the revision it saw has stayed
// unchanged for a full ttl on its own clock, and reports it taken over an expired record.
func TestLeaseStandbyTakesAnUnreleasedRecordOnlyAfterTheTTL(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	a.step()

	firstSeen := clock.now()
	if wait := b.step(); wait != leaseStandbyPoll || b.Holding() {
		t.Fatalf("standby's first poll waits %s holding %v, want the poll interval and no hold", wait, b.Holding())
	}
	if active := b.Status().ActiveInstance; active != "pod-a" {
		t.Fatalf("standby names active instance %q, want pod-a", active)
	}
	for clock.now().Sub(firstSeen) < leaseTTL {
		b.step()
		if b.Holding() {
			t.Fatalf("standby took the lease %s after first seeing the record, before the ttl %s", clock.now().Sub(firstSeen), leaseTTL)
		}
		clock.advance(leaseStandbyPoll)
	}
	b.step()
	status := b.Status()
	if !status.Holding || status.How != leaseTakenExpired || status.ActiveInstance != "pod-b" {
		t.Fatalf("status at the ttl = %+v, want pod-b holding over an expired record", status)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-b" {
		t.Fatalf("record holder = %q, want pod-b", record.Holder)
	}
}

// A holder that renews keeps the lease however long a standby polls.
func TestLeaseRenewalKeepsTheLease(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	a.step()
	_, first := readLease(t, kv)
	for elapsed := time.Duration(0); elapsed < 3*leaseTTL; elapsed += leaseStandbyPoll {
		if elapsed%leaseRenewEvery == 0 {
			a.step()
		}
		b.step()
		if !a.Holding() || b.Holding() {
			t.Fatalf("at %s: holder holding %v, standby holding %v; want the holder alone", elapsed, a.Holding(), b.Holding())
		}
		clock.advance(leaseStandbyPoll)
	}
	if term := a.Status().Term; term != 1 {
		t.Fatalf("holder's term = %d after renewing, want 1", term)
	}
	if _, last := readLease(t, kv); last.Revision() <= first.Revision() {
		t.Fatalf("record revision %d after renewals, want past %d", last.Revision(), first.Revision())
	}
}

// A holder whose renewal finds another pod's record (a compare-and-swap conflict) steps down,
// signals the loss, and polls as a standby naming the new holder.
func TestLeaseHolderStepsDownOnACASConflict(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	a.step()
	drainChanges(a)
	b.step()
	clock.advance(leaseTTL)
	b.step()
	if !b.Holding() {
		t.Fatal("standby did not take the record the holder stopped renewing")
	}

	if wait := a.step(); wait != 0 || a.Holding() {
		t.Fatalf("old holder's renewal waits %s holding %v, want it to step down and poll at once", wait, a.Holding())
	}
	if !drainChanges(a) {
		t.Fatal("stepping down signalled no change")
	}
	a.step()
	if status := a.Status(); status.Holding || status.ActiveInstance != "pod-b" {
		t.Fatalf("old holder's status = %+v, want a standby naming pod-b", status)
	}
	if !b.Holding() {
		t.Fatal("the new holder lost the lease to the old holder's renewal")
	}
}

// A renewal that landed on the server while its reply was lost leaves the record at a revision the
// holder does not know. The holder's next renewal conflicts with its own write: it must keep the
// lease, not step down and take it back as an expired record a TTL later.
func TestLeaseRenewalWhoseReplyWasLostKeepsTheLease(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	a.step()
	drainChanges(a)
	entry, err := kv.Get(a.key)
	if err != nil {
		t.Fatal(err)
	}
	// The lost renewal: the holder's own record written again, one revision past the holder's.
	if _, err := kv.Update(a.key, entry.Value(), entry.Revision()); err != nil {
		t.Fatal(err)
	}
	term := a.Status().Term
	clock.advance(leaseRenewEvery)
	if wait := a.step(); wait != 0 || !a.Holding() {
		t.Fatalf("conflicting renewal waits %s holding %v, want an immediate renewal at the record's revision, still holding", wait, a.Holding())
	}
	a.step()
	if status := a.Status(); !status.Holding || status.Term != term {
		t.Fatalf("status after the lost reply = %+v, want the same term %d still held", status, term)
	}
	if drainChanges(a) {
		t.Fatal("a renewal whose reply was lost signalled a change: the receive loop would drop and resubscribe")
	}
}

// A graceful release marks the record released; a standby's next poll takes it at once, with no
// TTL wait, reports it taken from a released record, and keeps the machine's service_since.
func TestLeaseReleasedRecordIsTakenAtTheNextPollKeepingServiceSince(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	a.step()
	since := a.Status().ServiceSince
	b.step()
	drainChanges(a)

	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if a.Holding() || !drainChanges(a) {
		t.Fatalf("after release: holding %v, want no hold and a change signalled", a.Holding())
	}
	record, _ := readLease(t, kv)
	if !record.Released || record.Holder != "pod-a" || !record.ServiceSince.Equal(since) {
		t.Fatalf("released record = %+v, want pod-a's record marked released keeping service_since %v", record.leaseFields, since)
	}
	if active := a.Status().ActiveInstance; active != "" {
		t.Fatalf("released lease names active instance %q, want none", active)
	}

	b.step() // the same instant: no time passes, let alone a ttl
	status := b.Status()
	if !status.Holding || status.How != leaseTakenReleased {
		t.Fatalf("standby status after the release = %+v, want holding, taken from a released record", status)
	}
	if !status.ServiceSince.Equal(since) {
		t.Fatalf("service_since after a graceful hand-over = %v, want the released record's %v", status.ServiceSince, since)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-b" || record.Released || !record.ServiceSince.Equal(since) {
		t.Fatalf("record = %+v, want pod-b's unreleased record keeping service_since %v", record.leaseFields, since)
	}
}

// The same hand-over on running leases, at production timing: the standby holds within a poll of
// the release, far inside the ttl it would wait for an unreleased record.
func TestLeaseRunningStandbyTakesAReleasedLeaseWithoutTheTTL(t *testing.T) {
	kv := leaseBucket(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := testLease(kv, "pod-a", productionLeaseConfig(), time.Now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), time.Now)
	a.Start(ctx)
	awaitLease(t, "pod-a holds", 5*time.Second, a.Holding)
	b.Start(ctx)
	defer func() { _ = b.Release(context.Background()) }()
	awaitLease(t, "pod-b sees pod-a", 5*time.Second, func() bool { return b.Status().ActiveInstance == "pod-a" })
	drainChanges(b)

	released := time.Now()
	if err := a.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	took := awaitLease(t, "pod-b holds", leaseTTL, b.Holding)
	if elapsed := took.Sub(released); elapsed > leaseStandbyPoll+time.Second {
		t.Fatalf("standby took the released lease %s after the release, want within a poll (%s) and no ttl wait", elapsed, leaseStandbyPoll)
	}
	if !drainChanges(b) {
		t.Fatal("the take signalled no change")
	}
	if how := b.Status().How; how != leaseTakenReleased {
		t.Fatalf("taken from %s, want released", how)
	}
}

// The holder's validity ends before any standby can take the record: validity runs to the start
// of its last successful renewal + ttl - margin, and a standby waits a full ttl from when it
// first saw that renewal's revision.
func TestLeaseHolderValidityEndsBeforeAStandbyCanTake(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	start := clock.now()
	a.step()
	b.step()
	clock.advance(leaseRenewEvery)
	renewed := clock.now()
	a.step() // the holder's last renewal; it stalls from here on
	var lastHeld, firstTaken time.Time
	for clock.now().Sub(start) <= 3*leaseTTL {
		if a.Holding() {
			lastHeld = clock.now()
		}
		b.step()
		if b.Holding() && firstTaken.IsZero() {
			firstTaken = clock.now()
		}
		clock.advance(10 * time.Millisecond)
	}
	if want := renewed.Add(leaseTTL - leaseMargin); !lastHeld.Before(want) || lastHeld.Before(want.Add(-10*time.Millisecond)) {
		t.Fatalf("holder last held at +%s, want until just before +%s (its renewal + ttl - margin)",
			lastHeld.Sub(start), want.Sub(start))
	}
	if firstTaken.IsZero() || firstTaken.Before(renewed.Add(leaseTTL)) {
		t.Fatalf("standby took at +%s, want no earlier than +%s (the renewal it saw + ttl)",
			firstTaken.Sub(start), renewed.Add(leaseTTL).Sub(start))
	}
	if gap := firstTaken.Sub(lastHeld); gap < leaseMargin {
		t.Fatalf("holder held until +%s and standby took at +%s: gap %s, want at least the margin %s",
			lastHeld.Sub(start), firstTaken.Sub(start), gap, leaseMargin)
	}
}

// faultyLeaseKV fails every Update while fail is set, as a bucket the holder cannot reach.
type faultyLeaseKV struct {
	natsgo.KeyValue
	fail *atomic.Bool
}

func (kv faultyLeaseKV) Update(key string, value []byte, last uint64) (uint64, error) {
	if kv.fail.Load() {
		return 0, natsgo.ErrTimeout
	}
	return kv.KeyValue.Update(key, value, last)
}

// A failed renewal is retried every renewEvery, sooner when the validity ends first; the holder
// keeps the lease through an outage shorter than its validity, loses it (signalled) at the
// validity's end in a longer one, and resumes as a new term taken over an expired record when a
// renewal after the outage finds its own revision still current.
func TestLeaseRenewalAfterAnOutageResumes(t *testing.T) {
	kv := leaseBucket(t)
	var fail atomic.Bool
	clock := newFakeLeaseClock()
	a := testLease(bus.KeyValue{KeyValue: faultyLeaseKV{KeyValue: kv.KeyValue, fail: &fail}}, "pod-a", productionLeaseConfig(), clock.now)
	a.step()
	drainChanges(a)

	fail.Store(true)
	clock.advance(leaseRenewEvery)
	if wait := a.step(); wait != leaseRenewEvery || !a.Holding() {
		t.Fatalf("a failed renewal inside the validity waits %s holding %v, want a retry after %s and the hold kept", wait, a.Holding(), leaseRenewEvery)
	}
	fail.Store(false)
	clock.advance(leaseRenewEvery)
	renewed := clock.now()
	a.step()
	if status := a.Status(); !status.Holding || status.Term != 1 || drainChanges(a) {
		t.Fatalf("after a short outage: %+v, want the same term held and no change signalled", status)
	}

	fail.Store(true)
	clock.advance(leaseRenewEvery)
	a.step()
	clock.advance(leaseRenewEvery)
	a.step()
	clock.advance(leaseRenewEvery + 700*time.Millisecond) // validity ends 300 ms from here
	if wait := a.step(); wait != 300*time.Millisecond {
		t.Fatalf("a failed renewal 300ms before the validity ends waits %s, want 300ms", wait)
	}
	if !a.Holding() {
		t.Fatal("holder lost the lease inside its validity")
	}
	clock.advance(300 * time.Millisecond)
	if a.Holding() {
		t.Fatalf("holder still holds at +%s after its last renewal, past ttl - margin", clock.now().Sub(renewed))
	}
	a.step()
	if !drainChanges(a) {
		t.Fatal("the lapse signalled no change")
	}

	fail.Store(false)
	clock.advance(leaseRenewEvery)
	a.step()
	status := a.Status()
	if !status.Holding || status.Term != 2 || status.How != leaseTakenExpired || !drainChanges(a) {
		t.Fatalf("after the outage: %+v, want term 2 held, taken over an expired record, and a change signalled", status)
	}
}

// service_since starts again when the previous record's last write is more than two ttl older
// than the acquiring write, since no pod of the machine was serving in between.
func TestLeaseServiceSinceStartsAgainOverAnUnreleasedRecordOlderThanTwoTTL(t *testing.T) {
	kv := leaseBucket(t)
	cfg := leaseConfig{renewEvery: 20 * time.Millisecond, ttl: 100 * time.Millisecond, margin: 20 * time.Millisecond, standbyPoll: 10 * time.Millisecond}
	clock := newFakeLeaseClock()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	putLease(t, kv, fmt.Sprintf(`{"holder":"pod-old","boot":"0011223344556677","service_since":%q,"released":false}`, old.Format(time.RFC3339Nano)))
	time.Sleep(2*cfg.ttl + 100*time.Millisecond)

	b := testLease(kv, "pod-b", cfg, clock.now)
	b.step()
	clock.advance(cfg.ttl)
	b.step()
	status := b.Status()
	if !status.Holding || status.How != leaseTakenExpired {
		t.Fatalf("status = %+v, want held over an expired record", status)
	}
	record, _ := readLease(t, kv)
	if status.ServiceSince.Equal(old) || !record.ServiceSince.Equal(record.AcquiredAt) {
		t.Fatalf("service_since %v (acquired %v) after a gap, want the acquiring write's time, not the old %v",
			record.ServiceSince, record.AcquiredAt, old)
	}
}

// A take-over of a record that expired recently keeps its service_since: the machine kept
// serving across it.
func TestLeaseServiceSinceIsKeptOverARecentlyExpiredRecord(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	putLease(t, kv, fmt.Sprintf(`{"holder":"pod-old","boot":"0011223344556677","service_since":%q,"released":false}`, old.Format(time.RFC3339Nano)))

	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	b.step()
	clock.advance(leaseTTL)
	b.step()
	status := b.Status()
	if !status.Holding || status.How != leaseTakenExpired || !status.ServiceSince.Equal(old) {
		t.Fatalf("status = %+v, want held over an expired record keeping service_since %v", status, old)
	}
	if record, _ := readLease(t, kv); !record.ServiceSince.Equal(old) {
		t.Fatalf("record service_since = %v, want %v", record.ServiceSince, old)
	}
}

// Every rewrite keeps the JSON keys of the record this build does not know.
func TestLeaseRewriteKeepsUnknownFields(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	putLease(t, kv, `{"holder":"pod-new-build","boot":"0011223344556677","released":true,"future":{"k":[1,2]}}`)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	b.step()
	if !b.Holding() {
		t.Fatal("standby did not take the released record")
	}
	clock.advance(leaseRenewEvery)
	b.step()
	_, entry := readLease(t, kv)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(entry.Value(), &fields); err != nil {
		t.Fatalf("decode %s: %v", entry.Value(), err)
	}
	if got := string(fields["future"]); got != `{"k":[1,2]}` {
		t.Fatalf("record %s: future = %s, want the newer build's field kept", entry.Value(), got)
	}
	if got := string(fields["holder"]); got != `"pod-b"` {
		t.Fatalf("record %s: holder = %s, want pod-b", entry.Value(), got)
	}
}

// The bucket is created with the configured replicas, file storage, a 10 minute MaxAge and one
// value per key.
func TestListenerStateBucketIsCreatedAtTheConfiguredReplicas(t *testing.T) {
	t.Run("one replica", func(t *testing.T) {
		kv := leaseBucket(t)
		status, err := kv.Status()
		if err != nil {
			t.Fatalf("read the bucket's status: %v", err)
		}
		bucket, ok := status.(*natsgo.KeyValueBucketStatus)
		if !ok {
			t.Fatalf("status is %T", status)
		}
		config := bucket.StreamInfo().Config
		if bucket.Bucket() != "envoy_listener" || bus.ListenerStateBucket != "envoy_listener" {
			t.Fatalf("bucket %q, constant %q, want envoy_listener", bucket.Bucket(), bus.ListenerStateBucket)
		}
		if config.Replicas != 1 || config.Storage != natsgo.FileStorage || config.MaxAge != 10*time.Minute || bucket.History() != 1 {
			t.Fatalf("replicas %d, storage %v, max age %s, history %d; want 1, file, 10m, 1",
				config.Replicas, config.Storage, config.MaxAge, bucket.History())
		}
	})
	// A single test server cannot hold three replicas, and refuses the create for exactly that,
	// which shows the configured count is what the create asks for.
	t.Run("three replicas reach the server", func(t *testing.T) {
		conn := testnats.Connect(t, testnats.URL(t))
		t.Cleanup(conn.Close)
		_, err := openListenerState(conn, 3, listenerStateRequestWait)
		if err == nil || !strings.Contains(err.Error(), "replicas > 1 not supported") {
			t.Fatalf("open at 3 replicas on one server: %v, want the server's refusal of replicas > 1", err)
		}
		if errors.Is(err, errListenerStateRefused) {
			t.Fatalf("a refused replica count reads as a missing grant: %v", err)
		}
	})
}

// A user whose grant lacks the bucket is refused with errListenerStateRefused, naming the bucket
// and the grant; once the grant has it the same connection opens the bucket and takes the lease;
// a connection error is not the refusal.
func TestListenerStateBucketRefusalIsTheMissingGrant(t *testing.T) {
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, "_INBOX.>", "$JS.API.INFO")
	// The server's violation is expected; the handler keeps nats.go from printing it.
	conn, err := testnats.ConnectSeed(t, grant.URL, seed, natsgo.ErrorHandler(func(*natsgo.Conn, *natsgo.Subscription, error) {}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	_, err = openListenerState(conn, 1, 500*time.Millisecond)
	if !errors.Is(err, errListenerStateRefused) || !errors.Is(err, natsgo.ErrPermissionViolation) {
		t.Fatalf("open without the grant: %v, want errListenerStateRefused wrapping the server's violation", err)
	}
	for _, want := range []string{"envoy_listener", "grant", `Publish to "$JS.API.STREAM.INFO.KV_envoy_listener"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}

	grant.Grant(t, "_INBOX.>", "$JS.API.>", "$KV.envoy_listener.>")
	kv, err := openListenerState(conn, 1, listenerStateRequestWait)
	if err != nil {
		t.Fatalf("open with the grant: %v", err)
	}
	l := testLease(kv, "pod-a", productionLeaseConfig(), time.Now)
	l.step()
	if !l.Holding() {
		t.Fatal("the lease was not taken under the grant")
	}

	conn.Close()
	if _, err := openListenerState(conn, 1, 500*time.Millisecond); err == nil || errors.Is(err, errListenerStateRefused) {
		t.Fatalf("open on a closed connection: %v, want a connection error that is not the refusal", err)
	}
}

// Only a violation of a subject of the listener state bucket, as the open's own error or as a
// last error that changed across the open, is the refusal.
func TestListenerStateBucketRefusalClassification(t *testing.T) {
	violation := func(subject string) error {
		return fmt.Errorf("%w: Permissions Violation for Publish to %q", natsgo.ErrPermissionViolation, subject)
	}
	stale := violation("$JS.API.STREAM.INFO.KV_envoy_listener")
	for _, tc := range []struct {
		name               string
		err, before, after error
		refused            bool
	}{
		{name: "stream info refused", err: natsgo.ErrTimeout, after: violation("$JS.API.STREAM.INFO.KV_envoy_listener"), refused: true},
		{name: "stream create refused", err: natsgo.ErrTimeout, after: violation("$JS.API.STREAM.CREATE.KV_envoy_listener"), refused: true},
		{name: "direct get refused", err: natsgo.ErrTimeout, after: violation("$JS.API.DIRECT.GET.KV_envoy_listener.$KV.envoy_listener.lease.listener-box-a"), refused: true},
		{name: "key write refused", err: natsgo.ErrTimeout, after: violation("$KV.envoy_listener.lease.listener-box-a"), refused: true},
		{name: "the open's own error", err: violation("$JS.API.STREAM.INFO.KV_envoy_listener"), refused: true},
		{name: "another bucket", err: natsgo.ErrTimeout, after: violation("$JS.API.STREAM.INFO.KV_envoy_listener_other")},
		{name: "another bucket's keys", err: natsgo.ErrTimeout, after: violation("$KV.envoy_listeners.x")},
		{name: "a subscription refusal", err: natsgo.ErrTimeout, after: fmt.Errorf("%w: Permissions Violation for Subscription to %q", natsgo.ErrPermissionViolation, "$KV.envoy_listener.>")},
		{name: "unchanged last error", err: natsgo.ErrTimeout, before: stale, after: stale},
		{name: "a timeout alone", err: natsgo.ErrTimeout},
		{name: "not a violation", err: natsgo.ErrTimeout, after: errors.New(`Permissions Violation for Publish to "$KV.envoy_listener.x"`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := listenerStateRefusal(tc.err, tc.before, tc.after) != nil; got != tc.refused {
				t.Fatalf("refused = %v, want %v", got, tc.refused)
			}
		})
	}
}
