package main

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// receiveSignalAnswerBound is how long one read of the receive signal may take. The lease reads
// it once a round of its loop (lease.receiveDown), and that loop renews the lease every second
// and must release the record within leaseReceiveLost of the connection going down, so a read
// that takes longer than a tenth of a second is a read the lease cannot afford.
const receiveSignalAnswerBound = 100 * time.Millisecond

// receiveSignalHangWatch is how long the row below reads the signal while the client's reconnect
// dials hang: past the two to three seconds the client's pings take to declare the frozen
// connection stale and over several of nats.go's reconnect attempts, each of which holds the
// connection's lock for the connect timeout (5 s) and is tried again a second later.
const receiveSignalHangWatch = 14 * time.Second

// The lease reads the receive signal from its own loop, the loop that renews the lease and
// releases the record of a receive connection that is down, so the signal must answer while the
// very connection it judges is stuck. nats.go holds a connection's own lock across each
// reconnect dial and its connect init, for the connect timeout at a time, when the route to the
// server accepts the dial and answers nothing.
//
// The relay here freezes the pod's bus client: the connection it holds stops moving bytes in
// either direction, which the client's pings read as a cut (bus.WithSilentCutDetection), and
// every connection it opens from then on is accepted and never answered, as a route that drops
// packets leaves a dial. The pod's lease bucket is on a connection of its own, which is not
// frozen, as it is in production. Before INFRA-557's lock-free signal, receiveUp read
// nats.Conn.Status() and took the receive loop's lock, which the loop itself holds across a
// subscribe, and a read landing in one of those dials waited seconds for it.
func TestTheReceiveSignalAnswersWhileTheConnectionsReconnectDialHangs(t *testing.T) {
	uri := testnats.URL(t)
	relay := startListenerRelay(t, uri)
	handle, _ := recordingHandler()
	rig := newReceiverRigVia(t, relay.relayed, uri, "box-signal-hang", "pod-a", handle,
		testReceiveTiming(), testLeaseTiming(), bus.WithSilentCutDetection())
	rig.lease.Start(context.Background())
	rig.receiver.Start()
	rig.awaitState(t, receiveActive, 20*time.Second)

	thaw := relay.freeze()
	t.Cleanup(thaw)
	cut := time.Now()
	var slowest, down time.Duration
	for time.Since(cut) < receiveSignalHangWatch {
		start := time.Now()
		up := rig.receiver.receiveUp()
		took := time.Since(start)
		if took > slowest {
			slowest = took
		}
		if took > receiveSignalAnswerBound {
			t.Fatalf("the receive signal took %s to answer, %s after the cut, past the %s bound: the lease's loop waits that out before it can renew or release",
				took.Round(time.Millisecond), time.Since(cut).Round(time.Millisecond), receiveSignalAnswerBound)
		}
		if !up && down == 0 {
			down = time.Since(cut)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if down == 0 {
		t.Fatalf("the receive signal read up throughout %s of a cut receive connection; it must read down for the holder to release the record", receiveSignalHangWatch)
	}
	t.Logf("the signal read down %s after the cut and took %s to answer at its slowest, over %s of hanging reconnect dials",
		down.Round(time.Millisecond), slowest.Round(time.Millisecond), receiveSignalHangWatch)
}

// The signal waits for the receive loop's own lock no more than it waits for nats.go: the loop
// holds that lock across its subscribes (durableReceiver.begin, resubscribe), which nats.go
// answers only once it has the connection's lock, so a signal that took it would wait out a
// reconnect dial that hangs just as surely. The lock is held here for longer than the bound,
// which is all the signal may take.
func TestTheReceiveSignalAnswersWhileTheReceiveLoopHoldsItsLock(t *testing.T) {
	uri := testnats.URL(t)
	handle, _ := recordingHandler()
	rig := newReceiverRig(t, uri, "box-signal-lock", "pod-a", handle, testReceiveTiming(), testLeaseTiming())
	const held = 2 * time.Second
	locked := make(chan struct{})
	go func() {
		rig.receiver.mu.Lock()
		close(locked)
		time.Sleep(held)
		rig.receiver.mu.Unlock()
	}()
	<-locked

	start := time.Now()
	up := rig.receiver.receiveUp()
	took := time.Since(start)
	if took > receiveSignalAnswerBound {
		t.Fatalf("the receive signal took %s to answer while the receive loop held its lock for %s, past the %s bound", took.Round(time.Millisecond), held, receiveSignalAnswerBound)
	}
	if !up {
		t.Fatal("the receive signal reads down while the client is connected and no term is receiving")
	}
}
