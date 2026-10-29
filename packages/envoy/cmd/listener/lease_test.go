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

// stale_binding is set by a holder that subscribed beside a push binding it did not wait out;
// every renewal, the release and the next take-over carry it, and that take-over's term must
// settle though the record was released; a holder that finds the durable not push-bound clears
// it. A call for a term that is no longer current changes nothing.
func TestLeaseStaleBindingIsSetCarriedAndCleared(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	a.step()
	a.SetStaleBinding(a.Status().Term, true)
	for renewal := range 2 {
		clock.advance(leaseRenewEvery)
		a.step()
		if record, _ := readLease(t, kv); !record.StaleBinding || record.Unsettled {
			t.Fatalf("record after renewal %d = %+v, want stale_binding set and not unsettled", renewal+1, record.leaseFields)
		}
	}
	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if record, _ := readLease(t, kv); !record.Released || !record.StaleBinding || record.Unsettled {
		t.Fatalf("released record = %+v, want released carrying stale_binding", record.leaseFields)
	}

	b.step()
	status := b.Status()
	if !status.Holding || status.How != leaseTakenReleased || !status.Settle {
		t.Fatalf("status = %+v, want pod-b holding from a released record, a term that must settle for its stale binding", status)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-b" || !record.StaleBinding || !record.Unsettled {
		t.Fatalf("record = %+v, want pod-b's carrying stale_binding, unsettled", record.leaseFields)
	}
	b.SetStaleBinding(status.Term-1, false)
	b.Settled(status.Term - 1)
	clock.advance(leaseRenewEvery)
	b.step()
	if record, _ := readLease(t, kv); !record.StaleBinding || !record.Unsettled {
		t.Fatalf("record after calls for an earlier term = %+v, want both flags kept", record.leaseFields)
	}
	b.SetStaleBinding(status.Term, false)
	b.Settled(status.Term)
	clock.advance(leaseRenewEvery)
	b.step()
	if record, _ := readLease(t, kv); record.StaleBinding || record.Unsettled {
		t.Fatalf("record after the durable read not push-bound and the hold ended = %+v, want both flags cleared", record.leaseFields)
	}
}

// unsettled is written from the acquiring write of a term that must settle. A release before that
// term's settle hold ends keeps it, so the next holder's term must settle too, though it takes a
// released record; once the hold ends it is cleared, and a release then, with nothing handed back
// (no Unsettle), hands over a term that need not settle.
func TestLeaseUnsettledIsHandedOnByAReleaseBeforeTheHoldEnds(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	putLease(t, kv, `{"holder":"pod-dead","boot":"0011223344556677","released":false}`)
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	c := testLease(kv, "pod-c", productionLeaseConfig(), clock.now)
	a.step()
	clock.advance(leaseTTL)
	a.step()
	if status := a.Status(); !status.Holding || status.How != leaseTakenExpired || !status.Settle {
		t.Fatalf("pod-a status = %+v, want held over an expired record, settling", status)
	}
	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("release pod-a: %v", err)
	}
	if record, _ := readLease(t, kv); !record.Released || !record.Unsettled || record.StaleBinding {
		t.Fatalf("record released mid-hold = %+v, want released, still unsettled", record.leaseFields)
	}

	b.step()
	status := b.Status()
	if !status.Holding || status.How != leaseTakenReleased || !status.Settle {
		t.Fatalf("pod-b status = %+v, want held from a released record, settling for the unsettled term before it", status)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-b" || !record.Unsettled {
		t.Fatalf("record = %+v, want pod-b's, unsettled", record.leaseFields)
	}
	b.Settled(status.Term)
	clock.advance(leaseRenewEvery)
	b.step()
	if record, _ := readLease(t, kv); record.Unsettled {
		t.Fatalf("record after pod-b's hold ended = %+v, want unsettled cleared", record.leaseFields)
	}
	if err := b.Release(context.Background()); err != nil {
		t.Fatalf("release pod-b: %v", err)
	}

	c.step()
	if status := c.Status(); !status.Holding || status.How != leaseTakenReleased || status.Settle {
		t.Fatalf("pod-c status = %+v, want held from a released record, not settling", status)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-c" || record.Unsettled || record.StaleBinding {
		t.Fatalf("record = %+v, want pod-c's with neither flag", record.leaseFields)
	}
}

// ack_wait_owed is the ack wait the releasing term still owed its successor: a term that must
// settle owes one from its acquiring write until its settle hold ends (Settled). Every release
// writes the debt as the term stands and every acquiring write writes it false. A released record
// carrying unsettled is a graceful hand-back (leaseStatus.HandedBack) unless it carries
// ack_wait_owed, with stale_binding or without. A call for a term that is no longer current
// changes nothing.
func TestLeaseAckWaitOwedIsTheTermsDebtAndDecidesTheHandBack(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	putLease(t, kv, `{"holder":"pod-dead","boot":"0011223344556677","released":false}`)
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	c := testLease(kv, "pod-c", productionLeaseConfig(), clock.now)
	a.step()
	clock.advance(leaseTTL)
	a.step()
	status := a.Status()
	if !status.Holding || status.How != leaseTakenExpired || !status.Settle || status.HandedBack {
		t.Fatalf("pod-a status = %+v, want held over an expired record, settling, no hand-back", status)
	}
	a.SetStaleBinding(status.Term, true)
	// A term that is no hand-back goes on owing the ack wait past its subscribe: what the dead
	// holder had comes back at its ack wait, whatever this pod has subscribed to.
	a.SetReceiving(status.Term)
	clock.advance(leaseRenewEvery)
	a.step()
	if record, _ := readLease(t, kv); record.AckWaitOwed {
		t.Fatalf("record after pod-a's renewal = %+v, want ack_wait_owed left to the release", record.leaseFields)
	}
	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("release pod-a: %v", err)
	}
	if record, _ := readLease(t, kv); !record.Released || !record.Unsettled || !record.AckWaitOwed || !record.StaleBinding {
		t.Fatalf("record released mid-hold = %+v, want released, unsettled, owing an ack wait, with a stale binding", record.leaseFields)
	}

	b.step()
	next := b.Status()
	if !next.Holding || next.How != leaseTakenReleased || !next.Settle || next.HandedBack {
		t.Fatalf("pod-b status = %+v, want held from a released record, settling, no hand-back: an ack wait is owed", next)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-b" || record.AckWaitOwed || !record.StaleBinding || !record.Unsettled {
		t.Fatalf("record = %+v, want pod-b's, unsettled, with a stale binding, owing nothing", record.leaseFields)
	}
	b.Settled(next.Term - 1)
	b.Unsettle(next.Term)
	if err := b.Release(context.Background()); err != nil {
		t.Fatalf("release pod-b: %v", err)
	}
	if record, _ := readLease(t, kv); !record.AckWaitOwed {
		t.Fatalf("record released after a Settled for an earlier term = %+v, want the debt kept", record.leaseFields)
	}

	b2 := testLease(kv, "pod-b2", productionLeaseConfig(), clock.now)
	b2.step()
	settled := b2.Status()
	if !settled.Holding || !settled.Settle || settled.HandedBack {
		t.Fatalf("pod-b2 status = %+v, want held from a released record, settling, no hand-back", settled)
	}
	b2.Settled(settled.Term)
	b2.Unsettle(settled.Term)
	if err := b2.Release(context.Background()); err != nil {
		t.Fatalf("release pod-b2: %v", err)
	}
	if record, _ := readLease(t, kv); !record.Released || !record.Unsettled || record.AckWaitOwed || !record.StaleBinding {
		t.Fatalf("record handed back after the hold ended = %+v, want released, unsettled, with a stale binding, owing nothing", record.leaseFields)
	}

	c.step()
	if status := c.Status(); !status.Holding || status.How != leaseTakenReleased || !status.Settle || !status.HandedBack {
		t.Fatalf("pod-c status = %+v, want held from a released record, settling for a graceful hand-back", status)
	}
}

// A term taken from a graceful hand-back owes its successor an ack wait until it has subscribed:
// the stopping pod's linger bridges the deliver subject to that subscription and no further. A
// term that stops, or gives the lease up, before it subscribes (a Stop during prepare, a holder
// stuck on its durable) releases the record owing the ack wait, so the next holder's hold waits
// one out; one that subscribed first releases it owing nothing, and the hand-over keeps the 3 s
// cap.
func TestLeaseATermTakenFromAHandBackOwesTheAckWaitUntilItSubscribes(t *testing.T) {
	clock := newFakeLeaseClock()
	for _, row := range []struct {
		name      string
		subscribe bool
		owed      bool
	}{
		{"stopped before it subscribed", false, true},
		{"stopped after it subscribed", true, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			kv := leaseBucket(t)
			putLease(t, kv, `{"holder":"pod-gone","boot":"0011223344556677","released":true,"unsettled":true}`)
			a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
			a.step()
			status := a.Status()
			if !status.Holding || status.How != leaseTakenReleased || !status.Settle || !status.HandedBack {
				t.Fatalf("pod-a status = %+v, want held from a graceful hand-back, settling", status)
			}
			if row.subscribe {
				a.SetReceiving(status.Term)
			}
			if err := a.Release(context.Background()); err != nil {
				t.Fatalf("release pod-a: %v", err)
			}
			if record, _ := readLease(t, kv); !record.Released || !record.Unsettled || record.AckWaitOwed != row.owed {
				t.Fatalf("record released %s = %+v, want released, unsettled, ack_wait_owed %v", row.name, record.leaseFields, row.owed)
			}
		})
	}
}

// A released record is a graceful hand-back only while the stopping pod's linger can still have
// bridged to the take-over (leaseHandBackBridge): its linger ends lingerUntaken after the release
// when no other pod has taken the record, and what the durable pushed after that went to a lost
// node's binding alone, or to no subscriber at all, and comes back only at its ack wait. A pod
// that takes such a record later settles as after a hard take-over, however the record reads.
func TestLeaseTakesAReleasedRecordPastTheLingerAsNoHandBack(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	putLease(t, kv, `{"holder":"pod-gone","boot":"0011223344556677","released":true,"unsettled":true,"stale_binding":true}`)
	time.Sleep(leaseHandBackBridge + 100*time.Millisecond)
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	a.step()
	status := a.Status()
	if !status.Holding || status.How != leaseTakenReleased || !status.Settle || status.HandedBack {
		t.Fatalf("pod-a status = %+v, want held from a released record past its linger, settling, no hand-back", status)
	}
}

// receiving is written false by every acquiring write, set by SetReceiving once the holder's
// subscription is up and kept by every renewal, and written false by the release. A graceful stop
// that hands messages back marks its term unsettled (Unsettle), so its release carries unsettled
// and the next holder's term settles. A call for a term that is no longer current changes nothing.
// Successor tells the pod that released whether another pod has taken the record and whether that
// holder is receiving.
func TestLeaseReceivingIsWrittenAndAGracefulReleaseCarriesUnsettled(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	putLease(t, kv, `{"holder":"pod-gone","boot":"0011223344556677","released":true,"receiving":true}`)
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	a.step()
	status := a.Status()
	if !status.Holding || status.How != leaseTakenReleased || status.Settle {
		t.Fatalf("pod-a status = %+v, want held from a released record, not settling", status)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-a" || record.Receiving || record.Unsettled {
		t.Fatalf("record after pod-a's acquisition = %+v, want pod-a's, not receiving, not unsettled", record.leaseFields)
	}
	a.SetReceiving(status.Term - 1)
	clock.advance(leaseRenewEvery)
	a.step()
	if record, _ := readLease(t, kv); record.Receiving {
		t.Fatalf("record after SetReceiving for an earlier term = %+v, want not receiving", record.leaseFields)
	}
	a.SetReceiving(status.Term)
	for renewal := range 2 {
		clock.advance(leaseRenewEvery)
		a.step()
		if record, _ := readLease(t, kv); !record.Receiving || record.Unsettled {
			t.Fatalf("record after renewal %d = %+v, want receiving, not unsettled", renewal+1, record.leaseFields)
		}
	}
	a.Unsettle(status.Term - 1)
	clock.advance(leaseRenewEvery)
	a.step()
	if record, _ := readLease(t, kv); record.Unsettled {
		t.Fatalf("record after Unsettle for an earlier term = %+v, want not unsettled", record.leaseFields)
	}

	a.Unsettle(status.Term)
	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("release pod-a: %v", err)
	}
	if record, _ := readLease(t, kv); !record.Released || !record.Unsettled || record.Receiving {
		t.Fatalf("record after the graceful release = %+v, want released, unsettled, not receiving", record.leaseFields)
	}
	if taken, receiving, err := a.Successor(); err != nil || taken || receiving {
		t.Fatalf("pod-a reads a successor taken %v, receiving %v (%v) over its own released record, want none", taken, receiving, err)
	}

	b.step()
	next := b.Status()
	if !next.Holding || next.How != leaseTakenReleased || !next.Settle {
		t.Fatalf("pod-b status = %+v, want held from a released record, settling for the messages handed back", next)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-b" || !record.Unsettled || record.Receiving {
		t.Fatalf("record after pod-b's acquisition = %+v, want pod-b's, unsettled, not receiving", record.leaseFields)
	}
	if taken, receiving, err := a.Successor(); err != nil || !taken || receiving {
		t.Fatalf("pod-a reads a successor taken %v, receiving %v (%v) before pod-b subscribed, want taken, not receiving", taken, receiving, err)
	}
	b.SetReceiving(next.Term)
	clock.advance(leaseRenewEvery)
	b.step()
	if taken, receiving, err := a.Successor(); err != nil || !taken || !receiving {
		t.Fatalf("pod-a reads a successor taken %v, receiving %v (%v) once pod-b's record says so, want taken and receiving", taken, receiving, err)
	}
	if taken, receiving, err := b.Successor(); err != nil || taken || receiving {
		t.Fatalf("pod-b reads a successor taken %v, receiving %v (%v) over its own record, want none", taken, receiving, err)
	}
}

// A running lease writes receiving at once on SetReceiving rather than at its next renewal, so a
// pod that stopped gracefully lets go of the deliver subject a renewal sooner.
func TestLeaseSetReceivingIsWrittenAtOnce(t *testing.T) {
	kv := leaseBucket(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := testLease(kv, "pod-a", productionLeaseConfig(), time.Now)
	a.Start(ctx)
	defer func() { _ = a.Release(context.Background()) }()
	awaitLease(t, "pod-a holds", 5*time.Second, a.Holding)
	time.Sleep(100 * time.Millisecond) // past the acquisition's own renewal, a second from the next

	set := time.Now()
	a.SetReceiving(a.Status().Term)
	written := awaitLease(t, "the record says receiving", leaseRenewEvery, func() bool {
		record, _ := readLease(t, kv)
		return record.Receiving
	})
	if took := written.Sub(set); took > leaseRenewEvery/2 {
		t.Fatalf("receiving written %s after SetReceiving, want at once rather than at the next renewal", took)
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
	if !status.Holding || status.Term != 2 || status.How != leaseTakenExpired || !status.Settle || !drainChanges(a) {
		t.Fatalf("after the outage: %+v, want term 2 held, taken over an expired record, settling, and a change signalled", status)
	}
	if record, _ := readLease(t, kv); !record.Unsettled {
		t.Fatalf("record after the resume = %+v, want unsettled", record.leaseFields)
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

// formerBootNames lists the boots of a record's former_boots, oldest first.
func formerBootNames(record leaseRecord) []string {
	names := make([]string, 0, len(record.FormerBoots))
	for _, former := range record.FormerBoots {
		names = append(names, former.Boot)
	}
	return names
}

func sameFormerBoot(x, y formerBoot) bool {
	return x.Boot == y.Boot && x.Stopped.Equal(y.Stopped)
}

// An acquisition over an expired record and one over a released record each add that record's
// boot to former_boots, stopped at the acquiring write's timestamp, and carry the record's own
// former_boots on, so a chain A -> B -> C names A and B. Renewals and a release leave it as it
// stands, and FormerHolder reads it.
func TestLeaseFormerBootsAreWrittenOnEveryTakeOverAndCarriedAlongAChain(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	c := testLease(kv, "pod-c", productionLeaseConfig(), clock.now)

	a.step()
	if record, _ := readLease(t, kv); len(record.FormerBoots) != 0 {
		t.Fatalf("former_boots over an absent record = %v, want none", formerBootNames(record))
	}
	b.step()
	clock.advance(leaseTTL)
	b.step()
	if !b.Holding() {
		t.Fatal("pod-b did not take the expired record")
	}
	expired, _ := readLease(t, kv)
	if len(expired.FormerBoots) != 1 || expired.FormerBoots[0].Boot != a.boot || !expired.FormerBoots[0].Stopped.Equal(expired.AcquiredAt) {
		t.Fatalf("former_boots after the expired take-over = %+v (acquired_at %v), want pod-a's boot %s stopped at acquired_at",
			expired.FormerBoots, expired.AcquiredAt, a.boot)
	}
	if !b.FormerHolder(a.boot) || b.FormerHolder(b.boot) || b.FormerHolder(c.boot) {
		t.Fatalf("pod-b's FormerHolder: pod-a %v, pod-b %v, pod-c %v; want pod-a alone",
			b.FormerHolder(a.boot), b.FormerHolder(b.boot), b.FormerHolder(c.boot))
	}

	for range 3 {
		clock.advance(leaseRenewEvery)
		b.step()
	}
	c.step()
	if err := b.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	released, _ := readLease(t, kv)
	if !released.Released || len(released.FormerBoots) != 1 || !sameFormerBoot(released.FormerBoots[0], expired.FormerBoots[0]) {
		t.Fatalf("released record's former_boots = %+v, want the acquiring write's %+v unchanged by renewals and the release",
			released.FormerBoots, expired.FormerBoots)
	}

	c.step()
	if !c.Holding() {
		t.Fatal("pod-c did not take the released record")
	}
	chained, _ := readLease(t, kv)
	if got := formerBootNames(chained); len(got) != 2 || got[0] != a.boot || got[1] != b.boot {
		t.Fatalf("former_boots after A -> B -> C = %v, want [%s %s]", got, a.boot, b.boot)
	}
	if !chained.FormerBoots[0].Stopped.Equal(expired.FormerBoots[0].Stopped) || !chained.FormerBoots[1].Stopped.Equal(chained.AcquiredAt) {
		t.Fatalf("former_boots after A -> B -> C = %+v (acquired_at %v), want pod-a's stop kept and pod-b's at acquired_at",
			chained.FormerBoots, chained.AcquiredAt)
	}
	if !c.FormerHolder(a.boot) || !c.FormerHolder(b.boot) || c.FormerHolder(c.boot) {
		t.Fatal("pod-c's FormerHolder does not name pod-a and pod-b alone")
	}
}

// An acquisition keeps only the former boots that stopped less than leaseFormerBootAge before
// it, never names its own boot, and keeps the newest leaseFormerBootsMax.
func TestLeaseFormerBootsArePrunedByAgeAndCount(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	now := time.Now()
	stopped := func(ago time.Duration) string { return now.Add(-ago).UTC().Format(time.RFC3339Nano) }

	putLease(t, kv, fmt.Sprintf(`{"holder":"pod-old","boot":"00000000000000aa","released":true,"former_boots":[`+
		`{"boot":"00000000000000a1","stopped":%q},{"boot":%q,"stopped":%q},{"boot":"00000000000000a2","stopped":%q}]}`,
		stopped(leaseFormerBootAge+time.Minute), b.boot, stopped(20*time.Second), stopped(10*time.Second)))
	b.step()
	if !b.Holding() {
		t.Fatal("pod-b did not take the released record")
	}
	record, _ := readLease(t, kv)
	if got := formerBootNames(record); len(got) != 2 || got[0] != "00000000000000a2" || got[1] != "00000000000000aa" {
		t.Fatalf("former_boots = %v, want [00000000000000a2 00000000000000aa]: the old entry and pod-b's own boot pruned", got)
	}
	if b.FormerHolder("00000000000000a1") {
		t.Fatal("FormerHolder names a boot that stopped longer than leaseFormerBootAge before the take-over")
	}

	c := testLease(kv, "pod-c", productionLeaseConfig(), clock.now)
	entries := make([]string, 0, 20)
	for i := range 20 {
		entries = append(entries, fmt.Sprintf(`{"boot":"%016x","stopped":%q}`, i, stopped(time.Duration(20-i)*time.Second)))
	}
	putLease(t, kv, `{"holder":"pod-old","boot":"00000000000000bb","released":true,"former_boots":[`+strings.Join(entries, ",")+`]}`)
	c.step()
	if !c.Holding() {
		t.Fatal("pod-c did not take the released record")
	}
	record, _ = readLease(t, kv)
	got := formerBootNames(record)
	if len(got) != leaseFormerBootsMax || got[0] != fmt.Sprintf("%016x", 20-leaseFormerBootsMax+1) || got[len(got)-1] != "00000000000000bb" {
		t.Fatalf("former_boots = %v, want the newest %d: entries %d to 19, then the released holder's boot",
			got, leaseFormerBootsMax, 20-leaseFormerBootsMax+1)
	}
}

// A holder's own lapse and resume is no take-over: it adds nothing to former_boots and keeps the
// list it acquired with.
func TestLeaseFormerBootsAreNotChangedByAResumeAfterALapse(t *testing.T) {
	kv := leaseBucket(t)
	var fail atomic.Bool
	clock := newFakeLeaseClock()
	putLease(t, kv, `{"holder":"pod-old","boot":"00000000000000aa","released":true}`)
	a := testLease(bus.KeyValue{KeyValue: faultyLeaseKV{KeyValue: kv.KeyValue, fail: &fail}}, "pod-a", productionLeaseConfig(), clock.now)
	a.step()
	acquired, _ := readLease(t, kv)
	if got := formerBootNames(acquired); len(got) != 1 || got[0] != "00000000000000aa" {
		t.Fatalf("former_boots after the take-over = %v, want [00000000000000aa]", got)
	}

	fail.Store(true)
	for a.Holding() {
		clock.advance(leaseRenewEvery)
		a.step()
	}
	fail.Store(false)
	clock.advance(leaseRenewEvery)
	a.step()
	if status := a.Status(); !status.Holding || status.Term != 2 {
		t.Fatalf("status after the outage = %+v, want term 2 held", status)
	}
	resumed, _ := readLease(t, kv)
	if len(resumed.FormerBoots) != 1 || !sameFormerBoot(resumed.FormerBoots[0], acquired.FormerBoots[0]) {
		t.Fatalf("former_boots after the resume = %+v, want the acquisition's %+v", resumed.FormerBoots, acquired.FormerBoots)
	}
	if a.FormerHolder(a.boot) {
		t.Fatal("a resumed holder names its own boot a former holder")
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
