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
