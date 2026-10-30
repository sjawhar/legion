package main

import (
	"context"
	"testing"
	"time"
)

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

// A term taken from a graceful hand-back owes its successor an ack wait until the server has its
// subscription inside the hand-back's window: the stopping pod's linger bridges the deliver
// subject to that subscription and no further. A term that stops, or gives the lease up, before
// it subscribes (a Stop during prepare, a holder stuck on its durable) releases the record owing
// the ack wait, so the next holder's hold waits one out; one whose subscription the server had
// inside the window releases it owing nothing, and the hand-over keeps the 3 s cap. One judged
// past the window, and one whose subscribe the server never got, owe it like any other settling
// term (HandBackHolds, which the receive loop asks before each subscribe and again when the
// subscribe's flush returns): what the durable pushed while the server had no subscription of
// this pod's comes back only at its ack wait, and the subscription that lands later clears no
// debt.
func TestLeaseATermTakenFromAHandBackOwesTheAckWaitUntilItSubscribes(t *testing.T) {
	clock := newFakeLeaseClock()
	for _, row := range []struct {
		name string
		// judged is whether the receive loop judged the hand-back at all, which a term that never
		// reached its subscribe does not; advance is how long after the take it judged it, and
		// subscribed what it told the lease of the server's reading of its SUB.
		judged     bool
		advance    time.Duration
		subscribed bool
		holds      bool
		owed       bool
	}{
		{"stopped before it subscribed", false, 0, false, false, true},
		{"the server had its subscription inside the window", true, 0, true, true, false},
		{"the server had its subscription past the window", true, leaseHandBackBridge, true, false, true},
		{"its subscribe never reached the server", true, 0, false, false, true},
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
			if row.judged {
				clock.advance(row.advance)
				if holds, _ := a.HandBackHolds(status.Term, row.subscribed); holds != row.holds {
					t.Fatalf("the hand-back holds %v %s after the take, subscribed %v, want %v", holds, row.advance, row.subscribed, row.holds)
				}
			}
			if row.subscribed {
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

// A term stopped while its renewals have lapsed releases the record unsettled and owing an ack
// wait, whatever its own flags stand at: the lapse dropped what it held and what the durable
// pushed it without an ack, and those come back only at their ack wait, so the next holder must
// settle for one as it would have over the expired record this release replaces. A release that
// wrote the lapsed term's own clean flags would let the standby that takes the record deliver
// new messages at once, ahead of the dropped ones.
func TestLeaseAStopDuringALapseReleasesTheRecordUnsettled(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	a.step()
	status := a.Status()
	if !status.Holding {
		t.Fatalf("pod-a status = %+v, want held from an absent record", status)
	}
	// Its settle hold ended and it subscribed: the term owes nothing and holds nothing back.
	a.Settled(status.Term)
	a.SetReceiving(status.Term)
	clock.advance(leaseRenewEvery)
	a.step()
	if record, _ := readLease(t, kv); record.Unsettled || record.AckWaitOwed {
		t.Fatalf("record of a settled term = %+v, want neither flag", record.leaseFields)
	}

	// The lease connection stalls past the term's validity: the renewals lapse, and SIGTERM
	// arrives before one resumes.
	clock.advance(leaseTTL)
	if a.Holding() {
		t.Fatal("pod-a still holds the lease past its validity")
	}
	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("release pod-a: %v", err)
	}
	if record, _ := readLease(t, kv); !record.Released || !record.Unsettled || !record.AckWaitOwed {
		t.Fatalf("record released during a lapse = %+v, want released, unsettled, owing an ack wait", record.leaseFields)
	}

	b.step()
	next := b.Status()
	if !next.Holding || next.How != leaseTakenReleased || !next.Settle || next.HandedBack {
		t.Fatalf("pod-b status = %+v, want held from a released record, settling for an ack wait, no hand-back", next)
	}
}

// A released record is a graceful hand-back only while the stopping pod's linger can still have
// bridged to the take-over (leaseHandBackBridge): a linger no pod has taken the record in ends
// lingerUntaken after the release, and what the durable pushed after that went to a lost node's
// binding alone, or to no subscriber at all, and comes back only at its ack wait. A pod that
// takes such a record later settles as after a hard take-over, however the record reads. The
// window may therefore be no longer than a linger lasts: a wider one would let a take-over at
// 1.2 s - a pod starting, or a standby whose poll at 0.7 s failed - hold for the hand-back's 3 s
// with nothing on the deliver subject to have bridged to it, and deliver the messages it then
// held before the older ones still due from the lost node's binding.
func TestLeaseTakesAReleasedRecordPastTheLingerAsNoHandBack(t *testing.T) {
	for _, row := range []struct {
		name       string
		record     string
		after      time.Duration
		handedBack bool
	}{
		{"taken while the linger holds", `{"holder":"pod-gone","boot":"0011223344556677","released":true,"unsettled":true}`, 0, true},
		{"taken past the linger, no pod running to take it", `{"holder":"pod-gone","boot":"0011223344556677","released":true,"unsettled":true}`, lingerUntaken + 200*time.Millisecond, false},
		{"taken past the linger beside a lost node's binding", `{"holder":"pod-gone","boot":"0011223344556677","released":true,"unsettled":true,"stale_binding":true}`, lingerUntaken + 200*time.Millisecond, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			kv := leaseBucket(t)
			clock := newFakeLeaseClock()
			putLease(t, kv, row.record)
			time.Sleep(row.after)
			a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
			a.step()
			status := a.Status()
			if !status.Holding || status.How != leaseTakenReleased || !status.Settle || status.HandedBack != row.handedBack {
				t.Fatalf("pod-a status = %+v, want held from a released record, settling, handed back %v", status, row.handedBack)
			}
		})
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
