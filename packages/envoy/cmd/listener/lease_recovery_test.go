package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

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
	// acquire signals the change after it releases its mutex, so the signal can land just after
	// Holding reads true.
	select {
	case <-b.Changes():
	case <-time.After(time.Second):
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
	cfg := leaseConfig{renewEvery: 20 * time.Millisecond, ttl: 100 * time.Millisecond, margin: 20 * time.Millisecond, standbyPoll: 10 * time.Millisecond, receiveLost: 50 * time.Millisecond}
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
