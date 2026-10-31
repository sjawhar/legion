package main

import (
	"context"
	"errors"
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

// The signal reads the bus client's installed, connected connection itself, and requires the
// receive loop's own copy and the term's subscription to name it. A run of the reconnect hooks
// stops at the first hook that fails (bus.Client.runReconnectHooks) and main registers the KV
// rewatch hook ahead of the receiver's (durableReceiver.reconnected), so a rewatch that keeps
// failing leaves the receiver's copy and the term's subscription both naming the connection the
// client has replaced. A signal that compared those two would find them the same closed
// connection and read up, so the holder would go on renewing a lease it received nothing on while
// /healthz answered 503; and once it released, a pod with no term left would read up and take
// the lease back, only to subscribe on the closed connection.
func TestAReplacedConnectionBehindAFailingHookReadsDownAndReleasesTheLease(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-replaced-behind-a-failing-hook"
	handle, _ := recordingHandler()
	// The production bound, so the moment between the close and the replacement - well under a
	// second on a local server - cannot release the lease by itself: the release this row waits
	// for is the one the replaced connection earns.
	leaseTiming := testLeaseTiming()
	leaseTiming.receiveLost = leaseReceiveLost
	rig := newReceiverRig(t, uri, machineID, "pod-a", handle, testReceiveTiming(), leaseTiming)
	// The lease judges this pod's receiving by that one signal, as main wires it.
	rig.lease.SetReceiveSignal(rig.receiver.receiveUp)
	rig.lease.Start(context.Background())
	rig.receiver.Start()
	rig.awaitState(t, receiveActive, 10*time.Second)
	if !rig.lease.Holding() {
		t.Fatal("pod-a does not hold the lease it subscribed under")
	}

	// main's hook order: the hook ahead of the receiver's fails as a rewatch whose request goes
	// unanswered does, and keeps failing, so the receiver never hears of the replacement.
	rig.hookBeforeTheReceivers(func(*natsgo.Conn) error {
		return errors.New("rewatch interest cache: nats: no responders available for request")
	})
	closed := rig.client.ConnectedConn()
	cut := time.Now()
	closed.Close()
	waitFor(t, 20*time.Second, "the client to install a connection in place of the closed one", func() bool {
		installed := rig.client.ConnectedConn()
		return installed != nil && installed != closed
	})
	if rig.receiver.receiveUp() {
		t.Fatalf("the receive signal reads up %s after the client replaced the connection the term is subscribed on:\n%s",
			time.Since(cut).Round(time.Millisecond), rig.logs.String())
	}

	// The lease reads the signal once a round, so the release is due a round after the bound,
	// counted from the cut: the signal has read down since the close.
	within := leaseTiming.receiveLost + leaseTiming.renewEvery + 2*time.Second
	released := rig.logRecord(t, "listener lease released: the receive connection is down", within)
	if took := time.Since(cut); took > within {
		t.Fatalf("the holder released %s after the cut, want it within %s", took.Round(time.Millisecond), within)
	}
	if down, ok := released["down_ms"].(float64); !ok || down < float64(leaseTiming.receiveLost.Milliseconds()) {
		t.Fatalf("the release names down_ms %v, want at least the %s bound", released["down_ms"], leaseTiming.receiveLost)
	}
	if rig.lease.Holding() {
		t.Fatal("pod-a still holds the lease it released")
	}

	// The release ends the term, so no subscription is left to compare, but the receive loop's own
	// copy of the connection still names the closed one, and every subscribe would go there: the
	// pod takes the released record no more, though no other pod is there to take it.
	deadline := time.Now().Add(10 * leaseTiming.standbyPoll)
	for time.Now().Before(deadline) {
		if rig.lease.Holding() || rig.receiver.receiveUp() {
			t.Fatalf("pod-a reads its receive signal up (%v) or holds the lease again (%v) while its receive loop names the connection the client replaced:\n%s",
				rig.receiver.receiveUp(), rig.lease.Holding(), rig.logs.String())
		}
		time.Sleep(leaseTiming.standbyPoll / 2)
	}
}
