package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A graceful stop gives the message in hand its grace and then cancels its handling, NAKs every
// message it has not handled with the hand-back delay, in stream order, and marks the lease
// released and unsettled, keeping its subscription until the standby receives: the standby takes
// the lease at its next poll, settles, and delivers the stopped pod's messages in the order they
// were published once the hand-back delay has run, with no ack wait.
func TestAGracefulStopHandsWhatItHeldBackAndReleasesTheLeaseToTheStandby(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-handover"
	inHand := make(chan context.Context, 1)
	var activeHandled atomic.Int32
	active := startReceiverRig(t, uri, machineID, "pod-a", func(ctx context.Context, msg *natsgo.Msg) {
		activeHandled.Add(1)
		inHand <- ctx
		// A prompt that never answers: it ends only when the stop cancels it, and the handler
		// then NAKs it, as the delivery handler does a cancelled prompt.
		<-ctx.Done()
		_ = msg.Nak()
	}, handBackTiming(), testLeaseTiming())
	active.awaitState(t, receiveActive, 10*time.Second)
	handle, received := recordingHandler()
	standby := startReceiverRig(t, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	waitFor(t, 5*time.Second, "the standby to read pod-a's lease", func() bool {
		return standby.lease.Status().ActiveInstance == "pod-a"
	})

	for _, data := range []string{"first", "second", "third"} {
		publishDurable(t, active.client, data)
	}
	var handling context.Context
	select {
	case handling = <-inHand:
	case <-time.After(5 * time.Second):
		t.Fatal("the active pod never took the first message")
	}
	waitFor(t, 5*time.Second, "the active pod to hold all three messages", func() bool {
		info, err := active.client.JS().ConsumerInfo(bus.Stream, active.consumer)
		return err == nil && info.NumAckPending == 3
	})

	stopped := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active.receiver.Stop(ctx)
	if took := time.Since(stopped); took > 3*time.Second {
		t.Fatalf("the stop took %s, want the 300 ms grace, the release and the standby's take-over", took)
	}
	if handling.Err() == nil {
		t.Fatal("the stop left the message in hand's handling running past its grace")
	}
	if status := active.lease.Status(); status.Holding {
		t.Fatal("the stopped pod still holds the lease")
	}
	end := handBackEnd(t, active)
	if end["handed_back"] != float64(3) || end["linger_end"] != "receiving holder seen" {
		t.Fatalf("the stopped pod's hand-back ended %v, want all three handed back and the linger ended by the standby receiving", end)
	}

	for _, want := range []string{"first", "second", "third"} {
		message := awaitReceived(t, received, 5*time.Second, "the standby's "+want)
		if message.data != want || message.delivered < 2 {
			t.Fatalf("the standby received %q as delivery %d, want %q redelivered", message.data, message.delivered, want)
		}
		if after := message.at.Sub(stopped); after >= consumerAckWait {
			t.Fatalf("the standby received %q %s after the stop, want it after the hand-back delay rather than the ack wait", want, after)
		}
	}
	noneReceived(t, received, time.Second, "after the three handed back")
	if status := standby.lease.Status(); !status.Holding || status.How != leaseTakenReleased || !status.Settle {
		t.Fatalf("the standby's lease after the stop: %+v, want held, taken from a released record, settling", status)
	}
	if got := activeHandled.Load(); got != 1 {
		t.Fatalf("the stopped pod handled %d messages, want only the first", got)
	}
}

// handBackEnd waits for the rig's log record of its hand-back's end and returns it.
func handBackEnd(t *testing.T, rig *receiverRig) map[string]any {
	t.Helper()
	return rig.logRecord(t, "handed the durable's messages back to the next holder", 5*time.Second)
}

// logRecord waits for the rig's log record whose msg is msg and returns it.
func (rig *receiverRig) logRecord(t *testing.T, msg string, within time.Duration) map[string]any {
	t.Helper()
	var found map[string]any
	waitFor(t, within, "the log record "+msg, func() bool {
		for line := range strings.SplitSeq(rig.logs.String(), "\n") {
			var record map[string]any
			if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == msg {
				found = record
				return true
			}
		}
		return false
	})
	return found
}

// A graceful stop of a holder whose link delivers a second late, with messages the server has
// already pushed to it still on the wire: the stopping pod keeps its subscription, hands back each
// message that reaches it after the stop, and lets go only once the standby receives, so the
// standby delivers every one of them, in the order they were published and before a newer one,
// after the hand-back delay rather than an ack wait. Unsubscribing at the stop leaves those
// messages to come back one ack wait later, behind newer ones; a plain drain of the subscription
// loses one now and then to nats-server, which drops a message whose first delivery is still in its
// send queue when the deliver subject's last subscriber goes.
func TestAGracefulStopOverASlowLinkHandsBackWhatWasOnTheWire(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-slow-link"
	relay := startListenerRelay(t, uri)
	var activeHandled atomic.Int32
	active := newReceiverRigVia(t, relay.relayed, uri, machineID, "pod-a", func(_ context.Context, msg *natsgo.Msg) {
		activeHandled.Add(1)
		_ = msg.Ack()
	}, handBackTiming(), testLeaseTiming())
	active.lease.Start(context.Background())
	active.receiver.Start()
	active.awaitState(t, receiveActive, 10*time.Second)
	handle, received := recordingHandler()
	standby := startReceiverRig(t, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	waitFor(t, 5*time.Second, "the standby to read pod-a's lease", func() bool {
		return standby.lease.Status().ActiveInstance == "pod-a"
	})

	relay.delay(time.Second)
	published := []string{"wire one", "wire two", "wire three", "wire four", "wire five"}
	for _, data := range published {
		publishDurable(t, standby.client, data)
	}
	waitFor(t, 5*time.Second, "the server to have pushed all five to pod-a", func() bool {
		info, err := standby.client.JS().ConsumerInfo(bus.Stream, standby.consumer)
		return err == nil && info.NumAckPending == len(published) && info.NumPending == 0
	})

	stopped := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active.receiver.Stop(ctx)
	t.Logf("pod-a's stop took %s", time.Since(stopped).Round(time.Millisecond))
	if got := activeHandled.Load(); got != 0 {
		t.Fatalf("pod-a handled %d messages, want none: the stop came while all five were on the wire", got)
	}
	end := handBackEnd(t, active)
	if end["handed_back"] != float64(len(published)) || end["linger_end"] != "receiving holder seen" {
		t.Fatalf("pod-a's hand-back ended %v, want all five handed back and the linger ended by the standby receiving", end)
	}
	publishDurable(t, standby.client, "newer")

	for _, want := range append(published, "newer") {
		message := awaitReceived(t, received, 10*time.Second, "the standby's "+want)
		if message.data != want {
			t.Fatalf("the standby delivered %q, want %q: the messages in the order they were published", message.data, want)
		}
		if after := message.at.Sub(stopped); after >= consumerAckWait {
			t.Fatalf("the standby received %q %s after the stop, want it before an ack wait", want, after)
		}
	}
	noneReceived(t, received, time.Second, "after every message was delivered once")
}

// A graceful stop keeps the stopping pod's subscription until the standby's lease record says it
// receives, so the durable is push-bound at every instant of the hand-over, and ends its linger as
// soon as the record says so, well before its cap.
func TestAGracefulStopKeepsItsSubscriptionUntilTheStandbyReceives(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-linger"
	activeHandle, _ := recordingHandler()
	active := startReceiverRig(t, uri, machineID, "pod-a", activeHandle, handBackTiming(), testLeaseTiming())
	active.awaitState(t, receiveActive, 10*time.Second)
	handle, _ := recordingHandler()
	standby := startReceiverRig(t, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	waitFor(t, 5*time.Second, "the standby to read pod-a's lease", func() bool {
		return standby.lease.Status().ActiveInstance == "pod-a"
	})
	observer := testnats.Connect(t, uri)
	js, err := observer.JetStream()
	if err != nil {
		t.Fatalf("observer JetStream: %v", err)
	}

	var samples, unbound atomic.Int32
	sampling, sampled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(sampled)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			info, err := js.ConsumerInfo(bus.Stream, active.consumer)
			if err == nil {
				samples.Add(1)
				if !info.PushBound {
					unbound.Add(1)
				}
			}
			select {
			case <-sampling:
				return
			case <-tick.C:
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active.receiver.Stop(ctx)
	standby.awaitRecord(t, "pod-b's, receiving", func(record leaseRecord) bool {
		return record.Holder == "pod-b" && !record.Released && record.Receiving
	})
	standby.awaitState(t, receiveActive, time.Second)
	time.Sleep(200 * time.Millisecond) // a few samples past the standby's subscription
	close(sampling)
	<-sampled
	if got := unbound.Load(); got != 0 {
		t.Fatalf("the durable read not push-bound in %d of %d samples during the hand-over, want push-bound throughout", got, samples.Load())
	}
	end := handBackEnd(t, active)
	lingered, _ := end["lingered_ms"].(float64)
	if end["linger_end"] != "receiving holder seen" || lingered >= float64(lingerMax.Milliseconds()) {
		t.Fatalf("pod-a's hand-back ended %v, want its linger ended by the standby receiving, before the %s cap", end, lingerMax)
	}
	if state := active.receiver.report().State; state != receiveStandby {
		t.Fatalf("pod-a's receive loop after its stop: %s, want standby", state)
	}
}

// A graceful stop with no other pod to take the lease keeps its subscription for two standby polls
// after its release, then, the record still its own, drains it and returns, leaving the record
// released and unsettled, so whichever pod takes it next settles.
func TestAGracefulStopWithNoOtherPodEndsItsLingerUntaken(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-alone"
	handle, _ := recordingHandler()
	active := startReceiverRig(t, uri, machineID, "pod-a", handle, handBackTiming(), testLeaseTiming())
	active.awaitState(t, receiveActive, 10*time.Second)

	stopped := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active.receiver.Stop(ctx)
	took := time.Since(stopped)
	if took < lingerUntaken || took > lingerUntaken+time.Second {
		t.Fatalf("the stop took %s, want the %s wait for another pod to take the lease and a drain", took, lingerUntaken)
	}
	end := handBackEnd(t, active)
	lingered, _ := end["lingered_ms"].(float64)
	if end["linger_end"] != "not taken" || lingered < float64(lingerUntaken.Milliseconds()) {
		t.Fatalf("pod-a's hand-back ended %v, want its linger ended by no pod taking the lease after %s", end, lingerUntaken)
	}
	active.awaitRecord(t, "released, unsettled, not receiving", func(record leaseRecord) bool {
		return record.Holder == "pod-a" && record.Released && record.Unsettled && !record.Receiving
	})
	if active.lease.Holding() {
		t.Fatal("the stopped pod still holds the lease")
	}
	waitFor(t, 2*time.Second, "the durable to have no subscriber left", func() bool {
		info, err := active.client.JS().ConsumerInfo(bus.Stream, active.consumer)
		return err == nil && !info.PushBound
	})
}

// A graceful stop whose released lease another pod takes but never receives on keeps its
// subscription to the linger cap, lingerMax after it handed its messages back, then drains it and
// returns.
func TestAGracefulStopLingersToItsCapForATakerThatNeverReceives(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-mute-taker"
	handle, _ := recordingHandler()
	active := startReceiverRig(t, uri, machineID, "pod-a", handle, handBackTiming(), testLeaseTiming())
	active.awaitState(t, receiveActive, 10*time.Second)
	// pod-b runs a lease and no receive loop: it takes the released record and never subscribes.
	mute := newReceiverRig(t, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	mute.lease.Start(context.Background())
	waitFor(t, 5*time.Second, "pod-b to read pod-a's lease", func() bool {
		return mute.lease.Status().ActiveInstance == "pod-a"
	})

	stopped := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active.receiver.Stop(ctx)
	took := time.Since(stopped)
	if took < lingerMax || took > lingerMax+time.Second {
		t.Fatalf("the stop took %s, want the %s linger cap and a drain", took, lingerMax)
	}
	end := handBackEnd(t, active)
	if end["linger_end"] != "timeout" {
		t.Fatalf("pod-a's hand-back ended %v, want its linger to have run to the %s cap", end, lingerMax)
	}
	if !mute.lease.Holding() {
		t.Fatal("pod-b does not hold the lease pod-a released")
	}
}

// A linger whose cap has already run out when it starts - the deadline is set before the NAK
// batch, and a flush and a release that both ran to their bounds leave it in the past - still
// keeps the subscription for the floor it guarantees its successor, lingerUntaken and a poll
// after the release: a take-over that subscribes inside the hand-back window
// (leaseHandBackBridge, a poll inside that floor) settles for the hand-back delay alone, on the
// premise that this subscription was still on the deliver subject when it did. A linger that
// returned "timeout" at its first read would leave the deliver subject to a lost node's binding,
// or to no subscriber at all, while the next holder held for 3 s.
func TestAStoppingPodsLingerOutlastsTheHandBackWindowThoughItsCapHasRun(t *testing.T) {
	uri := testnats.URL(t)
	handle, _ := recordingHandler()
	active := newReceiverRig(t, uri, "box-linger-floor", "pod-a", handle, handBackTiming(), testLeaseTiming())
	lingered, how := active.receiver.linger(time.Now())
	if how != "timeout" || lingered < leaseHandBackBridge {
		t.Fatalf("a linger past its cap ended %q after %s, want it held for the %s hand-back window first", how, lingered, leaseHandBackBridge)
	}
}

// A successor that takes the released record inside the hand-back window but subscribes past it
// settles for the ack wait's cap, not the hand-back's. The stopping pod's flush and release run
// to their bounds here, so its linger's cap is in the past and it holds the deliver subject only
// for the floor it guarantees (lingerUntaken and a poll after the release): what the durable
// pushes between that and the successor's subscribe reaches no subscriber of this machine's and
// comes back only at its ack wait. The successor judges itself by the same window and holds for
// 7 s, so that message lands in its held batch and is delivered in stream order, before the newer
// one published after it subscribed. The rule: the hand-back's 3 s cap rests on a subscription
// the stopping pod's linger bridged to, so it is the term's subscribe, not its take-over, that
// the window is measured to.
func TestASuccessorThatSubscribesPastTheHandBackWindowSettlesForTheAckWait(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-late-subscriber"
	// A flush and a release that both ran to their bounds leave the linger's deadline in the
	// past, which a zero cap stands for: the linger then ends at its floor whatever the successor
	// is doing.
	timing := handBackTiming()
	timing.lingerMax = 0
	active := startReceiverRig(t, uri, machineID, "pod-a", func(ctx context.Context, msg *natsgo.Msg) {
		// A prompt that never answers: the stop cancels it and the handler NAKs it, so pod-a's
		// linger hands that message back with the delay too.
		<-ctx.Done()
		_ = msg.Nak()
	}, timing, testLeaseTiming())
	active.awaitState(t, receiveActive, 10*time.Second)
	handle, received := recordingHandler()
	standby := newReceiverRig(t, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())

	published := []string{"in hand", "handed back"}
	for _, data := range published {
		publishDurable(t, active.client, data)
	}
	waitFor(t, 5*time.Second, "pod-a to hold both messages", func() bool {
		info, err := active.client.JS().ConsumerInfo(bus.Stream, active.consumer)
		return err == nil && info.NumAckPending == len(published)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		active.receiver.Stop(ctx)
	}()
	released := awaitReleased(t, active)

	// pod-b takes the released record inside the hand-back window, as a standby's poll does.
	time.Sleep(time.Until(released.Add(850 * time.Millisecond)))
	standby.lease.Start(context.Background())
	waitFor(t, 2*time.Second, "pod-b to take the released lease", standby.lease.Holding)
	took := standby.lease.Status()
	if since := time.Since(released); !took.HandedBack || since >= leaseHandBackBridge {
		t.Fatalf("pod-b took the record %s after the release, %+v: the row needs a take-over inside the %s window", since, took, leaseHandBackBridge)
	}
	<-stopped
	// pod-a's stop has returned, so its linger outlasted the window and it has let go of the
	// deliver subject: what the durable pushes now reaches a binding of a lost node alone.
	pushToALostBinding(t, uri, active.client, active.consumer, "lost", len(published)+1)

	// pod-b subscribes past the window, as one whose prepare was slow does.
	time.Sleep(time.Until(released.Add(1300 * time.Millisecond)))
	standby.receiver.Start()
	subscribed := standby.awaitState(t, receiveActive, 10*time.Second)
	if since := subscribed.Sub(released); since <= leaseHandBackBridge {
		t.Fatalf("pod-b subscribed %s after the release, want it past the %s window the row is about", since, leaseHandBackBridge)
	}
	publishDurable(t, standby.client, "newer")

	for _, want := range append(append([]string{}, published...), "lost", "newer") {
		message := awaitReceived(t, received, consumerAckWait+5*time.Second, "pod-b's "+want)
		if message.data != want {
			t.Fatalf("pod-b delivered %q, want %q: the messages in the order they were published", message.data, want)
		}
	}
	noneReceived(t, received, time.Second, "after every message was delivered once")
	if standby.lease.Status().HandedBack {
		t.Fatal("pod-b still counts its term a hand-back though it subscribed past the window")
	}
	passed := standby.logRecord(t, "hand-back window passed before subscribing; holding for the ack wait", time.Second)
	if passed["cap"] != (consumerAckWait + settleMargin).String() {
		t.Fatalf("pod-b's hold after the window passed = %v, want the %s cap of a term owed an ack wait", passed, consumerAckWait+settleMargin)
	}
	if end := standby.settleHoldEnd(t, time.Second); end["outcome"] != "settled" || end["messages"] != float64(4) {
		t.Fatalf("pod-b's settle hold ended %v, want settled holding all four messages", end)
	}
}

// A successor whose subscribe the server never got - its SUB is still unsent when the flush times
// out - settles for the ack wait's cap and goes on owing its own successor one, though it took the
// record inside the hand-back's window. The window is what the stopping pod's linger guarantees
// the deliver subject its subscription for, and a SUB the server has not read bridges nothing to:
// what the durable pushed meanwhile went to no subscriber of this machine's and comes back only at
// its ack wait. A resubscribe that lands later records the term receiving, which no longer clears
// that debt, so the record this term releases tells the pod after it to wait an ack wait out too.
func TestASuccessorWhoseSubscribeTheServerNeverGotSettlesForTheAckWait(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-unflushed-sub"
	relay := startListenerRelay(t, uri)
	// A message outstanding for a minute, so the successor's settle hold runs to its cap rather
	// than ending at its first read and clearing the term's debt with it.
	leaveARetryOutstanding(t, uri, machineID, "retry")
	handle, _ := recordingHandler()
	standby := newReceiverRigVia(t, relay.relayed, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	// A request on pod-b's own connection, so nats.go has subscribed its reply inbox before the
	// relay is armed: from here the only SUB on that link is the one begin sends.
	if _, err := standby.client.JS().ConsumerInfo(bus.Stream, standby.consumer); err != nil {
		t.Fatalf("read the durable on pod-b's connection: %v", err)
	}
	// The relay swallows that SUB and everything pod-b sends after it, the flush's PING included,
	// so the subscribe's flush times out and the server never counts the subscription.
	relay.arm("SUB ", 1)
	// The record is written last, so pod-b takes it inside leaseHandBackBridge of the release, as
	// a standby takes a graceful hand-back.
	putLeaseRecord(t, uri, machineID, `{"holder":"pod-gone","boot":"0123456789abcdef","released":true,"unsettled":true}`)
	standby.lease.Start(context.Background())
	standby.receiver.Start()

	if acquired := standby.logRecord(t, "listener lease acquired", 15*time.Second); acquired["handed_back"] != true {
		t.Fatalf("pod-b took the record %v, want it taken as a graceful hand-back: the row needs a take inside the %s window", acquired, leaseHandBackBridge)
	}
	if settling := standby.logRecord(t, "listener lease term must settle; receiving at once and holding what arrives until the durable's outstanding messages are in hand", 5*time.Second); settling["cap"] != (handBackDelay + settleMargin).String() {
		t.Fatalf("pod-b's hold before it subscribed = %v, want the %s cap of a hand-back", settling, handBackDelay+settleMargin)
	}
	passed := standby.logRecord(t, "hand-back window passed before subscribing; holding for the ack wait", 15*time.Second)
	if passed["cap"] != (consumerAckWait + settleMargin).String() {
		t.Fatalf("pod-b's hold after its subscribe went unflushed = %v, want the %s cap of a term owed an ack wait", passed, consumerAckWait+settleMargin)
	}
	if standby.lease.Status().HandedBack {
		t.Fatal("pod-b still counts its term a hand-back though the server never got its subscription")
	}

	// The link comes back and pod-b resubscribes, which records the term receiving: the debt the
	// term took on stands all the same, and its release hands it to the pod that takes the record
	// next.
	relay.arm("", 0)
	relay.drop()
	// The connection itself is what the wait reads: a request issued on the dropped link would
	// hold its own JetStream deadline, long past the term's settle hold.
	conn := standby.client.Conn
	waitFor(t, 10*time.Second, "pod-b to notice the dropped link", func() bool { return !conn.IsConnected() })
	waitFor(t, 15*time.Second, "pod-b to reconnect", conn.IsConnected)
	if err := standby.receiver.resubscribe(context.Background()); err != nil {
		t.Fatalf("resubscribe pod-b: %v", err)
	}
	if err := standby.lease.Release(context.Background()); err != nil {
		t.Fatalf("release pod-b's lease: %v", err)
	}
	entry, err := standby.lease.kv.Get(standby.lease.key)
	if err != nil {
		t.Fatalf("read the lease record: %v", err)
	}
	record, err := decodeLeaseRecord(entry.Value())
	if err != nil {
		t.Fatalf("decode the lease record %s: %v", entry.Value(), err)
	}
	if !record.Released || !record.Unsettled || !record.AckWaitOwed {
		t.Fatalf("the record pod-b released = %+v, want released, unsettled and owing an ack wait", record.leaseFields)
	}
}

// awaitReleased waits for the rig's lease record to read released and returns when it did, within
// a few milliseconds of the write: a row that times a successor's take-over against the release
// needs that instant more closely than a renewal's poll gives it.
func awaitReleased(t *testing.T, rig *receiverRig) time.Time {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if entry, err := rig.lease.kv.Get(rig.lease.key); err == nil {
			if record, err := decodeLeaseRecord(entry.Value()); err == nil && record.Released {
				return time.Now()
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the stopping pod did not release its lease within 10s")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// pushToALostBinding publishes data to consumer's durable while no pod of the machine is
// subscribed, into a connection of the test's own that takes the push and goes without an ack, as
// a lost node's binding does: nothing hands it back, so it comes back only at its ack wait. It
// returns once the durable has pushed it and holds pending messages outstanding in all.
func pushToALostBinding(t *testing.T, uri string, client *bus.Client, consumer, data string, pending int) {
	t.Helper()
	info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	conn := testnats.Connect(t, uri)
	defer conn.Close()
	sub, err := conn.SubscribeSync(info.Config.DeliverSubject)
	if err != nil {
		t.Fatalf("subscribe as a lost node's binding: %v", err)
	}
	publishDurable(t, client, data)
	if _, err := sub.NextMsg(5 * time.Second); err != nil {
		t.Fatalf("the durable did not push %q to the lost binding: %v", data, err)
	}
	if err := sub.Unsubscribe(); err != nil {
		t.Fatalf("unsubscribe the lost node's binding: %v", err)
	}
	waitFor(t, 5*time.Second, "the pushed message to stay outstanding with no subscriber", func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
		return err == nil && info.NumAckPending == pending && info.NumPending == 0
	})
}
