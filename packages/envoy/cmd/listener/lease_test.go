package main

import (
	"context"
	"errors"
	"io"
	"regexp"
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
	if !status.Holding || status.Term != 1 || status.How != leaseTakenAbsent || status.ActiveInstance != "pod-a" || status.Settle {
		t.Fatalf("status = %+v, want holding term 1 taken from an absent record by pod-a, not settling", status)
	}
	if !drainChanges(a) {
		t.Fatal("the acquisition signalled no change")
	}
	record, entry := readLease(t, kv)
	if record.Holder != "pod-a" || record.Released || record.DeliverSubject != "_INBOX.listener-box-a" || record.Unsettled || record.StaleBinding {
		t.Fatalf("record = %+v, want pod-a's unreleased record naming its deliver subject, neither unsettled nor with a stale binding", record.leaseFields)
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

// overwrittenTakeKV is the listener state bucket as a standby sees it when another pod's write
// lands on the key right after its Create, before it reads that write back.
type overwrittenTakeKV struct {
	natsgo.KeyValue
}

func (k overwrittenTakeKV) Create(key string, value []byte) (uint64, error) {
	revision, err := k.KeyValue.Create(key, value)
	if err == nil {
		_, err = k.KeyValue.Put(key, []byte(`{"holder":"pod-b","boot":"fedcba9876543210"}`))
	}
	return revision, err
}

// A take whose read back by key finds another revision than its write does not hold the lease:
// another pod wrote first. The standby reads the record again at once, and finds pod-b's.
func TestLeaseTakeOverwrittenBeforeItsReadBackIsNotHeld(t *testing.T) {
	kv := leaseBucket(t)
	a := testLease(bus.KeyValue{KeyValue: overwrittenTakeKV{KeyValue: kv.KeyValue}}, "pod-a", productionLeaseConfig(), newFakeLeaseClock().now)
	if wait := a.step(); wait != 0 {
		t.Fatalf("step after an overwritten take waits %s, want 0: read the record again at once", wait)
	}
	if status := a.Status(); status.Holding || status.Term != 0 {
		t.Fatalf("status = %+v, want not holding, no term", status)
	}
	if drainChanges(a) {
		t.Fatal("the overwritten take signalled a change")
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-b" {
		t.Fatalf("record = %+v, want pod-b's write", record.leaseFields)
	}
}

// A standby takes an unreleased record nobody renews only once the revision it saw has stayed
// unchanged for a full ttl on its own clock, and reports it taken over an expired record, a term
// that must settle, which its acquiring write marks unsettled.
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
	if !status.Holding || status.How != leaseTakenExpired || status.ActiveInstance != "pod-b" || !status.Settle {
		t.Fatalf("status at the ttl = %+v, want pod-b holding over an expired record, a term that must settle", status)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-b" || !record.Unsettled {
		t.Fatalf("record = %+v, want pod-b's, unsettled", record.leaseFields)
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

// A release with nothing handed back (no Unsettle: a holder stopped before it subscribed) marks
// the record released; a standby's next poll takes it at once, with no TTL wait, reports it taken
// from a released record, and keeps the machine's service_since.
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
	if !status.Holding || status.How != leaseTakenReleased || status.Settle {
		t.Fatalf("standby status after the release = %+v, want holding, taken from a released record, not settling", status)
	}
	if !status.ServiceSince.Equal(since) {
		t.Fatalf("service_since after a graceful hand-over = %v, want the released record's %v", status.ServiceSince, since)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-b" || record.Released || !record.ServiceSince.Equal(since) {
		t.Fatalf("record = %+v, want pod-b's unreleased record keeping service_since %v", record.leaseFields, since)
	}
}

// A Release whose context has already ended writes the release all the same once the lease's loop
// has stopped: the receiver's own stop calls Release again with the shutdown context a graceful
// stop can have overrun (durableReceiver.Stop, after the hand-back's own release timed out at
// handBackReleaseWait), and a record left unreleased is taken only once its TTL runs out, with
// the ack wait's cap - a graceful stop delivering like a hard one. The rule: a loop that has
// already stopped is read before ctx, so a stop that did everything it owed writes the release
// whatever its context says, rather than letting Go pick between two ready channels.
func TestLeaseReleaseWritesWithAnExpiredContextAfterTheLoopStopped(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	// Both channels are closed at every call, so a select over the two would take the context's
	// branch about half the time; 50 runs make that certain.
	for run := range 50 {
		l := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
		ctx, stop := context.WithCancel(context.Background())
		l.Start(ctx)
		awaitLease(t, "pod-a to hold the lease", 5*time.Second, l.Holding)
		stop()
		<-l.done
		expired, cancel := context.WithCancel(context.Background())
		cancel()

		if err := l.Release(expired); err != nil {
			t.Fatalf("release %d with an expired context: %v", run+1, err)
		}
		record, _ := readLease(t, kv)
		if !record.Released || record.Holder != "pod-a" {
			t.Fatalf("release %d with an expired context left the record %+v, want it marked released", run+1, record.leaseFields)
		}
	}
}

// stalledRenewKV is the listener state bucket with one renewal held: once the test arms it, the
// next write of the lease record blocks until the test lets it go, as a bucket call that hangs
// past a release's bound does. Every later write lands, so the release that follows the held
// renewal writes the record.
type stalledRenewKV struct {
	natsgo.KeyValue
	armed  atomic.Bool
	held   chan struct{}
	letGo  chan struct{}
	stalls sync.Once
}

func newStalledRenewKV(kv natsgo.KeyValue) *stalledRenewKV {
	return &stalledRenewKV{KeyValue: kv, held: make(chan struct{}), letGo: make(chan struct{})}
}

func (k *stalledRenewKV) Update(key string, value []byte, revision uint64) (uint64, error) {
	if k.armed.CompareAndSwap(true, false) {
		k.stalls.Do(func() { close(k.held) })
		<-k.letGo
	}
	return k.KeyValue.Update(key, value, revision)
}

// A Release the lease's loop outlasts records the term's ack-wait debt before it gives up, so the
// release that lands later writes ack_wait_owed: a graceful stop whose first release timed out
// (durableReceiver.handBack) has let go of the deliver subject by the time the second one writes
// the record, and no linger follows that write, so a standby that takes it moments later must
// hold for an ack wait rather than count the write a graceful hand-back.
func TestLeaseAReleaseThatTimedOutLeavesTheTermOwingTheAckWait(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	// The clock never moves, so the term's validity never runs out: the debt this row is about is
	// the timed-out release's, not a lapse's.
	stalled := newStalledRenewKV(kv.KeyValue)
	a := testLease(bus.KeyValue{KeyValue: stalled}, "pod-a", testLeaseTiming(), clock.now)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	a.Start(ctx)
	awaitLease(t, "pod-a to hold the lease", 5*time.Second, a.Holding)
	// A term that settled and then handed its messages back on a graceful stop: unsettled, owing
	// its successor nothing of its own.
	status := a.Status()
	a.Settled(status.Term)
	a.Unsettle(status.Term)

	stalled.armed.Store(true)
	select {
	case <-stalled.held:
	case <-time.After(5 * time.Second):
		t.Fatal("no renewal was held within 5s")
	}
	releaseCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	err := a.Release(releaseCtx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("release while the loop is held = %v, want the context's deadline", err)
	}
	close(stalled.letGo)
	<-a.done
	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("release after the loop stopped: %v", err)
	}

	record, _ := readLease(t, kv)
	if !record.Released || !record.Unsettled || !record.AckWaitOwed {
		t.Fatalf("record after a release that timed out = %+v, want released, unsettled and owing an ack wait", record.leaseFields)
	}
	b := testLease(kv, "pod-b", testLeaseTiming(), clock.now)
	b.step()
	next := b.Status()
	if !next.Holding || next.How != leaseTakenReleased || !next.Settle || next.HandedBack {
		t.Fatalf("pod-b status = %+v, want held from a released record, settling, no hand-back: no linger followed that write", next)
	}
	receiver := &durableReceiver{durableReceiverOptions: durableReceiverOptions{timing: productionReceiveConfig()}}
	if hold := receiver.settleHold(next.HandedBack); hold != consumerAckWait+settleMargin {
		t.Fatalf("pod-b's settle hold is capped at %s, want the %s of a term owed an ack wait", hold, consumerAckWait+settleMargin)
	}
}
