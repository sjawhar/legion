package main

import (
	"context"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// signalAnswerWithin is how long the receive signal, and a round of the lease that reads it, may
// take while the receive connection's locks are held. The signal loads two atomics and a round of
// the lease is a call on the lease's own connection, which the cut below does not touch, so this
// is scheduling and one bucket round trip. The bound it stands for is the lease's own: renewals
// are due every renewEvery and the release is due at receiveLost, which an answer that waits out
// a reconnect dial (the connect timeout, 5 s) misses.
const signalAnswerWithin = 100 * time.Millisecond

// receiverLockHeldFor is how long the row below holds the receiver's lock across a subscribe, so
// a signal that takes that lock fails the row rather than hanging it.
const receiverLockHeldFor = 2 * time.Second

// The delivery lease reads this pod's receiving through durableReceiver.receiveUp from its own
// loop (lease.SetReceiveSignal, lease.receiveDown), the loop that renews the record and releases
// it once the receive connection has been down for receiveLost. Neither the signal nor that loop
// may wait on the receive connection: nats.go holds its connection's lock across every reconnect
// dial and the connect init after it, which a route that drops the pod's packets leaves hanging
// for the connect timeout, and durableReceiver.begin holds the receiver's lock across the
// subscribe that waits for that same lock. Before this row the signal read bus.Client.Connected,
// which read nats.Conn.Status, and took the receiver's lock, so the lease loop stalled for as
// long as each dial hung: its renewals lapsed instead of its release landing, and a standby took
// the expired record seconds later
// (TestTwoPodsReceiveConnectionLostToADroppingRouteReleasesTheLeaseWithin15s).
func TestTheReceiveSignalAnswersWhileTheReceiveConnectionsLocksAreHeld(t *testing.T) {
	uri := testnats.URL(t)
	relay := startListenerRelay(t, uri)
	handle, _ := recordingHandler()
	const machineID = "box-signal"
	// The bus client reaches NATS through the relay and the lease's bucket does not, as the
	// listener's two connections stand in production: the one can be lost while the other renews.
	rig := newReceiverRigVia(t, relay.relayed, uri, machineID, "pod-a", handle, testReceiveTiming(), testLeaseTiming())
	// The lease judges this pod's receiving by that one signal, as main wires it.
	rig.lease.SetReceiveSignal(rig.receiver.receiveUp)
	info, err := ensureListenerDurable(context.Background(), rig.client, rig.consumer)
	if err != nil {
		t.Fatalf("create the durable: %v", err)
	}
	subject := info.Config.DeliverSubject
	rig.lease.step()
	if !rig.lease.Holding() {
		t.Fatal("pod-a did not take the lease")
	}
	term := rig.lease.Status().Term
	if err := rig.receiver.begin(subject, 0, term); err != nil {
		t.Fatalf("subscribe to the durable's deliver subject: %v", err)
	}
	if !rig.receiver.receiveUp() {
		t.Fatal("the receive signal reads down while the pod receives over a healthy connection")
	}

	// From here the relay moves no byte in either direction, with every socket left open, and
	// every connection the pod opens is accepted and left without the server's INFO: that is what
	// a route that drops the pod's packets leaves each reconnect dial, and nats.go holds the
	// connection's lock across it.
	t.Cleanup(relay.freeze())
	relay.drop()
	awaitHeldConnectionLock(t, rig.client.Conn)
	holdReceiverLockAcrossASubscribe(t, rig)

	down := awaitSignalDown(t, rig.receiver)
	t.Logf("the signal read down %s after the cut, with both locks held", down.Round(time.Millisecond))

	// The lease's loop runs through all of that: it reads the signal at the start of every round
	// (lease.step) and its release lands on its own connection, which the cut did not touch.
	released := false
	for deadline := time.Now().Add(5 * time.Second); !released && time.Now().Before(deadline); {
		round := time.Now()
		wait := rig.lease.step()
		if took := time.Since(round); took > signalAnswerWithin {
			t.Fatalf("a round of the lease took %s while the receive connection's locks were held, want it within %s", took, signalAnswerWithin)
		}
		released = !rig.lease.Holding()
		if !released {
			time.Sleep(min(wait, 20*time.Millisecond))
		}
	}
	if !released {
		t.Fatalf("the holder never released the lease with its receive connection down; its lines:\n%s", rig.logs.String())
	}
	const line = "listener lease released: the receive connection is down"
	if !strings.Contains(rig.logs.String(), line) {
		t.Fatalf("the holder let the lease go without the line %q:\n%s", line, rig.logs.String())
	}
}

// awaitSignalDown returns how long the receive signal took to read down, and fails the row as
// soon as one read of it takes longer than signalAnswerWithin: the lease reads it from the loop
// that must release the record on time.
func awaitSignalDown(t *testing.T, receiver *durableReceiver) time.Duration {
	t.Helper()
	start := time.Now()
	for deadline := start.Add(10 * time.Second); time.Now().Before(deadline); {
		read := time.Now()
		up := receiver.receiveUp()
		if took := time.Since(read); took > signalAnswerWithin {
			t.Fatalf("the receive signal took %s to answer while the receive connection's locks were held, want it within %s", took, signalAnswerWithin)
		}
		if !up {
			return time.Since(start)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the receive signal never read down while the pod's receive connection was cut")
	return 0
}

// holdReceiverLockAcrossASubscribe holds the receiver's lock across a subscribe on the receive
// connection, as durableReceiver.begin holds it, and returns once it is held. The subscribe waits
// for the connection's lock, which nats.go holds across its hanging reconnect dial, so the hold
// is the one begin leaves; it is let go after receiverLockHeldFor whatever the subscribe does, so
// a signal that waits for the lock fails this row rather than hanging it.
func holdReceiverLockAcrossASubscribe(t *testing.T, rig *receiverRig) {
	t.Helper()
	held := make(chan struct{})
	go func() {
		rig.receiver.mu.Lock()
		defer rig.receiver.mu.Unlock()
		close(held)
		subscribed := make(chan struct{})
		go func() {
			defer close(subscribed)
			sub, err := rig.client.Conn.Subscribe(rig.consumer+".signal-row", func(*natsgo.Msg) {})
			if err == nil {
				_ = sub.Unsubscribe()
			}
		}()
		select {
		case <-subscribed:
		case <-time.After(receiverLockHeldFor):
		}
	}()
	<-held
}

// awaitHeldConnectionLock returns once a read of conn's state has gone unanswered for
// signalAnswerWithin, which is nats.go holding the connection's lock across a reconnect dial that
// hangs (doReconnect, processConnectInit).
func awaitHeldConnectionLock(t *testing.T, conn *natsgo.Conn) {
	t.Helper()
	awaitHeldLock(t, "nats.go never held the connection's lock across a hanging reconnect dial", func() { _ = conn.Status() })
}

// awaitHeldLock returns once read, which takes the lock it watches, has gone unanswered for
// signalAnswerWithin, and fails with complaint when none has within 30 s. Each read is left on
// its own goroutine: it returns when the lock is let go.
func awaitHeldLock(t *testing.T, complaint string, read func()) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		done := make(chan struct{})
		go func() {
			defer close(done)
			read()
		}()
		select {
		case <-done:
			time.Sleep(10 * time.Millisecond)
		case <-time.After(signalAnswerWithin):
			return
		}
	}
	t.Fatal(complaint)
}
