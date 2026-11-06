package main

import (
	"sync/atomic"
	"testing"
	"time"
)

// receiveSignal is a receiver's signal the test moves by hand (lease.SetReceiveSignal): the lease
// reads it once a round, from its loop, so an atomic is all it needs.
type receiveSignal struct {
	up atomic.Bool
}

func newReceiveSignal() *receiveSignal {
	signal := &receiveSignal{}
	signal.up.Store(true)
	return signal
}

func (s *receiveSignal) get() bool { return s.up.Load() }
func (s *receiveSignal) down()     { s.up.Store(false) }
func (s *receiveSignal) backUp()   { s.up.Store(true) }

// A holder whose receive connection stays down releases the record once it has been down for the
// bound, and not before: its renewals land all the while, so nothing but the signal ends the
// term. The record it releases is unsettled, owes its successor an ack wait and carries a stale
// binding - the dead connection's subscription can still be bound at the server - so the pod that
// takes it settles for the full ack wait and is no hand-back candidate. The pod that released it
// takes it back from no poll while the connection is down, and stands by again once it is up.
func TestLeaseHolderReleasesTheRecordOnceItsReceiveConnectionHasBeenDownForTheBound(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	signal := newReceiveSignal()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	a.SetReceiveSignal(signal.get)
	a.step()
	if !a.Holding() {
		t.Fatal("pod-a did not take the lease")
	}
	drainChanges(a)

	signal.down()
	for down := time.Duration(0); down < leaseReceiveLost; down += leaseRenewEvery {
		if wait := a.step(); wait > leaseReceiveLost-down {
			t.Fatalf("the holder waits %s with its receive connection down for %s, past the bound %s", wait, down, leaseReceiveLost)
		}
		if !a.Holding() {
			t.Fatalf("the holder let the lease go %s after its receive connection went down, before the bound %s", down, leaseReceiveLost)
		}
		if record, _ := readLease(t, kv); record.Released {
			t.Fatalf("the record is released %s after the receive connection went down, before the bound %s", down, leaseReceiveLost)
		}
		clock.advance(leaseRenewEvery)
	}

	wait := a.step()
	if a.Holding() {
		t.Fatalf("the holder still holds the lease %s after its receive connection went down", leaseReceiveLost)
	}
	if wait != leaseStandbyPoll {
		t.Fatalf("the release waits %s, want the standby poll %s", wait, leaseStandbyPoll)
	}
	if !drainChanges(a) {
		t.Fatal("the release signalled no change, so the receive loop would not end its term")
	}
	record, _ := readLease(t, kv)
	if !record.Released || record.Holder != "pod-a" || !record.Unsettled || !record.AckWaitOwed || !record.StaleBinding || record.Receiving {
		t.Fatalf("record = %+v, want pod-a's released record, unsettled, owing an ack wait, with a stale binding and not receiving", record.leaseFields)
	}

	// No bounce: this pod takes no record back while its receive connection is down, however long
	// the released record lies there.
	for waited := time.Duration(0); waited < 2*leaseTTL; waited += leaseStandbyPoll {
		a.step()
		if a.Holding() {
			t.Fatalf("the pod took its own released record back %s after the release, with its receive connection still down", waited)
		}
		clock.advance(leaseStandbyPoll)
	}

	signal.backUp()
	a.step()
	if !a.Holding() {
		t.Fatal("the pod took no record once its receive connection was back")
	}
	if status := a.Status(); status.How != leaseTakenReleased || !status.Settle || status.HandBack {
		t.Fatalf("status = %+v, want a term taken from the released record that must settle and is no hand-back candidate", status)
	}
}

// A receive connection back inside the bound costs the holder nothing: the same term holds the
// lease on, the record is never released, and the next cut is timed from itself.
func TestLeaseHolderKeepsTheLeaseWhenItsReceiveConnectionComesBackInsideTheBound(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	signal := newReceiveSignal()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	a.SetReceiveSignal(signal.get)
	a.step()
	term := a.Status().Term

	signal.down()
	a.step()
	clock.advance(leaseReceiveLost - leaseRenewEvery)
	a.step()
	signal.backUp()
	clock.advance(leaseRenewEvery)

	// Past the bound, counted from the moment the connection went down: a lease that timed the
	// reconnect out would release here.
	for waited := time.Duration(0); waited < 2*leaseReceiveLost; waited += leaseRenewEvery {
		a.step()
		if !a.Holding() {
			t.Fatalf("the holder let the lease go %s after its receive connection came back", waited)
		}
		clock.advance(leaseRenewEvery)
	}
	if status := a.Status(); status.Term != term {
		t.Fatalf("the holder is in term %d after the reconnect, want the term it held across it, %d", status.Term, term)
	}
	if record, _ := readLease(t, kv); record.Released || record.Holder != "pod-a" {
		t.Fatalf("record = %+v, want pod-a's unreleased record", record.leaseFields)
	}
}

// A standby whose receive connection is down takes no record: not an absent one, and not one that
// has lain unchanged for the ttl. It reads the record all the while, so its /healthz still names
// the holder, and takes one again once the connection is back.
func TestLeaseStandbyTakesNoRecordWhileItsReceiveConnectionIsDown(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	signal := newReceiveSignal()
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	b.SetReceiveSignal(signal.get)

	signal.down()
	if wait := b.step(); wait != leaseStandbyPoll || b.Holding() {
		t.Fatalf("the standby's poll of an absent record waits %s holding %v, want the poll interval and no hold", wait, b.Holding())
	}
	if _, err := kv.Get(leaseTestKey); err == nil {
		t.Fatal("the standby created a record while its receive connection was down")
	}

	putLease(t, kv, `{"holder":"pod-gone","boot":"0011223344556677","released":false}`)
	for waited := time.Duration(0); waited < 2*leaseTTL; waited += leaseStandbyPoll {
		b.step()
		if b.Holding() {
			t.Fatalf("the standby took the expired record %s on, with its receive connection down", waited)
		}
		clock.advance(leaseStandbyPoll)
	}
	if active := b.Status().ActiveInstance; active != "pod-gone" {
		t.Fatalf("the standby names active instance %q, want the record's holder pod-gone: it reads the record while it takes none", active)
	}

	signal.backUp()
	firstSeen := clock.now()
	for clock.now().Sub(firstSeen) < leaseTTL {
		b.step()
		if b.Holding() {
			t.Fatalf("the standby took the record %s after its receive connection came back, before the ttl %s", clock.now().Sub(firstSeen), leaseTTL)
		}
		clock.advance(leaseStandbyPoll)
	}
	b.step()
	if status := b.Status(); !status.Holding || status.How != leaseTakenExpired {
		t.Fatalf("status = %+v, want pod-b holding over the expired record once its receive connection is back", status)
	}
}
