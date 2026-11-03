package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
)

// The pushes the durable's handler hands to the serving-port workers are messages of its receive
// term, and end with that term (servePortWorkers.endTerm, receive_stop.go). Without that, a worker
// outlives the term that fed it: a pod that stopped gracefully goes on pushing a message its
// successor knows nothing of, and marks the hand-over clean though it still holds it, so the
// successor's hold ends at 3 s and delivers later messages first; and a pod that lost the lease
// keeps the same message alive with keepInProgress and pushes it for as long as it runs, beside
// the pod that now holds the lease.

// startWorkerReceiverRig is a pod of machineID receiving its machine's durable through cfg's
// handler, with cfg's serving-port workers wired to its term as main wires them
// (durableReceiverOptions.workers). leaseTiming is the pod's, so a row that has its pod give the
// lease up can keep it a standby while it reads what the pod did. wrap, nil for none, goes around
// the durable's handler, for a row that hands each message to it under a context of its own.
func startWorkerReceiverRig(t *testing.T, uri, machineID, instance string, cfg listenerDeliveryHandlerConfig, timing receiveConfig, leaseTiming leaseConfig, wrap func(durableHandler) durableHandler) *receiverRig {
	t.Helper()
	handler := jetStreamDeliveryHandler(cfg)
	if wrap != nil {
		handler = wrap(handler)
	}
	rig := newReceiverRig(t, uri, machineID, instance, handler, timing, leaseTiming)
	rig.receiver.workers = cfg.servePorts
	rig.lease.Start(context.Background())
	rig.receiver.Start()
	return rig
}

// awaitDurablePending waits until the rig's durable holds pending messages unacknowledged.
func awaitDurablePending(t *testing.T, client *bus.Client, rig *receiverRig, pending int) {
	t.Helper()
	waitFor(t, 10*time.Second, "the durable to hold the message unacknowledged", func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, rig.consumer)
		return err == nil && info.NumAckPending == pending
	})
}

// A graceful stop hands back the message its serving-port worker holds, in the batch with
// everything else the term held: the worker's push is stopped and its message NAKed with the
// hand-back delay, so the standby is given it within the hand-over rather than an ack wait later,
// or never, the stopping pod's worker having kept it alive and pushed it on its own.
func TestAGracefulStopHandsBackTheMessageItsServingPortWorkerHolds(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	cfg := wireTestDeliveryWorkers(t, harness.config)
	port := startScriptedPort(t, "ses_worker_handback_"+id.New())
	release := port.hold()
	t.Cleanup(release)
	registerServingPortSession(t, harness, port.sessionID, port.port)

	uri := harness.client.Conn.ConnectedUrl()
	active := startWorkerReceiverRig(t, uri, "test-machine", "pod-a", cfg, handBackTiming(), testLeaseTiming(), nil)
	active.awaitState(t, receiveActive, 10*time.Second)
	handle, received := recordingHandler()
	standby := startReceiverRig(t, uri, "test-machine", "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	waitFor(t, 5*time.Second, "the standby to read pod-a's lease", func() bool {
		return standby.lease.Status().ActiveInstance == "pod-a"
	})

	item := newEnvelope("agent", contracts.AgentSubject(port.sessionID), "the message the worker holds")
	publish(t, harness.client, item)
	// The handler has handed the push over and returned; the message is unacknowledged because
	// its push is, waiting on the port that never answers.
	awaitDurablePending(t, harness.client, active, 1)

	stopped := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active.receiver.Stop(ctx)

	end := handBackEnd(t, active)
	if end["handed_back"] != float64(1) {
		t.Fatalf("the stop handed %v messages back, want the one its worker held: %v", end["handed_back"], end)
	}
	message := awaitReceived(t, received, 5*time.Second, "the standby's copy of the message the worker held")
	if !strings.Contains(message.data, item.EventID) {
		t.Fatalf("the standby was given %q, want the message the worker held (%s)", message.data, item.EventID)
	}
	if after := message.at.Sub(stopped); after >= consumerAckWait {
		t.Fatalf("the standby was given the worker's message %s after the stop, want it after the hand-back delay rather than the ack wait", after)
	}
}

// wrappedHandlingKey is the key the row below puts a value under in a message's handling context,
// one of the two ways a handler can wrap it.
type wrappedHandlingKey struct{}

// The same hand-back, with the durable's handler wrapping each message's handling context in a
// value and a timeout of its own before it delivers it: the pushes it hands over still belong to
// the term, so its stop takes them back and hands their messages on. A term identified by the
// context the handler was given, rather than by the handle that context carries
// (servePortTermHandle), loses every push of a wrapped one: the stop finds no term, reports
// itself idle, and the worker goes on pushing a message the successor knows nothing of.
func TestAGracefulStopHandsBackTheWorkerMessageOfAHandlerThatWrappedItsContext(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	cfg := wireTestDeliveryWorkers(t, harness.config)
	port := startScriptedPort(t, "ses_worker_wrapped_"+id.New())
	release := port.hold()
	t.Cleanup(release)
	registerServingPortSession(t, harness, port.sessionID, port.port)

	wrap := func(next durableHandler) durableHandler {
		return func(ctx context.Context, msg *nats.Msg) {
			ctx, cancel := context.WithTimeout(context.WithValue(ctx, wrappedHandlingKey{}, "wrapped"), time.Minute)
			defer cancel()
			next(ctx, msg)
		}
	}
	uri := harness.client.Conn.ConnectedUrl()
	active := startWorkerReceiverRig(t, uri, "test-machine", "pod-a", cfg, handBackTiming(), testLeaseTiming(), wrap)
	active.awaitState(t, receiveActive, 10*time.Second)
	handle, received := recordingHandler()
	standby := startReceiverRig(t, uri, "test-machine", "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	waitFor(t, 5*time.Second, "the standby to read pod-a's lease", func() bool {
		return standby.lease.Status().ActiveInstance == "pod-a"
	})

	item := newEnvelope("agent", contracts.AgentSubject(port.sessionID), "the message the worker of a wrapped handler holds")
	publish(t, harness.client, item)
	awaitDurablePending(t, harness.client, active, 1)

	stopped := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active.receiver.Stop(ctx)

	end := handBackEnd(t, active)
	if end["handed_back"] != float64(1) {
		t.Fatalf("the stop handed %v messages back, want the one the worker of the wrapped handler held: %v", end["handed_back"], end)
	}
	message := awaitReceived(t, received, 5*time.Second, "the standby's copy of the message the worker held")
	if !strings.Contains(message.data, item.EventID) {
		t.Fatalf("the standby was given %q, want the message the worker held (%s)", message.data, item.EventID)
	}
	if after := message.at.Sub(stopped); after >= consumerAckWait {
		t.Fatalf("the standby was given the worker's message %s after the stop, want it after the hand-back delay rather than the ack wait", after)
	}
}

// A lost lease drops the pushes of its term: the ex-holder's worker stops pushing and gives its
// message up unsettled, so the pod that holds the lease now is given it by the server once its ack
// wait runs out. A worker that ran on would push that message beside the new holder's delivery of
// it, for as long as this pod lives, and keep it alive with keepInProgress so no other pod is ever
// given it.
func TestALostLeaseDropsTheServingPortPushesOfItsTerm(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	cfg := wireTestDeliveryWorkers(t, harness.config)
	// The port refuses every push, so the worker keeps the message at the head of the session's
	// queue and pushes it again, 1 s after the first refusal and 2 s after the second.
	port := startScriptedPort(t, "ses_worker_lost_"+id.New(),
		http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError)
	registerServingPortSession(t, harness, port.sessionID, port.port)

	uri := harness.client.Conn.ConnectedUrl()
	leaseTiming := testLeaseTiming()
	// The pod polls slowly as a standby, so it does not take the lease it gave up back, and start
	// a term that pushes again, while the row reads what the lost one left.
	leaseTiming.standbyPoll = 3 * time.Second
	active := startWorkerReceiverRig(t, uri, "test-machine", "pod-a", cfg, testReceiveTiming(), leaseTiming, nil)
	active.awaitState(t, receiveActive, 10*time.Second)

	item := newEnvelope("agent", contracts.AgentSubject(port.sessionID), "the message the lost term was pushing")
	publish(t, harness.client, item)
	if push := port.next(t, 10*time.Second); !strings.Contains(push.text, item.Payload) {
		t.Fatalf("the port was pushed %q, want the published message", push.text)
	}

	active.lease.GiveUp(active.lease.Status().Term)
	waitFor(t, 10*time.Second, "pod-a to lose the lease", func() bool { return !active.lease.Holding() })

	// Two of the worker's retries fall inside this window.
	port.noPush(t, 3500*time.Millisecond)
	dropped := active.logRecord(t, "dropped the serving-port pushes of the term whose lease was lost", 5*time.Second)
	if dropped["dropped_pushes"] != float64(1) {
		t.Fatalf("the lost term dropped %v serving-port pushes, want the one its worker held: %v", dropped["dropped_pushes"], dropped)
	}
}

// A stop whose serving-port push ignores its cancel marks nothing, though every other part of the
// hand-over went through: that worker still holds a message of the term, which the stop can
// account for no better than a handler that ignored its own cancel, so the pod that takes the
// record over settles for the ack wait rather than the hand-back's 3 s.
func TestAStopWhoseServingPortPushIgnoresItsCancelMarksNoHandBack(t *testing.T) {
	pushing := make(chan struct{}, 1)
	harness := newListenerDeliveryHarness(t, listenerTransport(func(request *http.Request) (*http.Response, error) {
		// A serve process that answers no cancel: the request runs its course whatever the push's
		// context says, well past the stop's wait for the workers (servePortTermStopWait).
		select {
		case pushing <- struct{}{}:
		default:
		}
		time.Sleep(2 * time.Second)
		return listenerDeliveryResponse(http.StatusNoContent), nil
	}))
	cfg := wireTestDeliveryWorkers(t, harness.config)
	const sessionID = "ses_worker_uncancellable"
	registerServingPortSession(t, harness, sessionID, 4567)

	uri := harness.client.Conn.ConnectedUrl()
	active := startWorkerReceiverRig(t, uri, "test-machine", "pod-a", cfg, handBackTiming(), testLeaseTiming(), nil)
	active.awaitState(t, receiveActive, 10*time.Second)
	handle, _ := recordingHandler()
	standby := startReceiverRig(t, uri, "test-machine", "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	waitFor(t, 5*time.Second, "the standby to read pod-a's lease", func() bool {
		return standby.lease.Status().ActiveInstance == "pod-a"
	})

	publish(t, harness.client, newEnvelope("agent", contracts.AgentSubject(sessionID), "the push that ignores its cancel"))
	// The row is about the mark's gate on a worker still pushing, so the stop has to come while
	// the worker's push is in the transport: a message only pending on the durable is one the
	// stop can still take from the queue and hand back before any push began, and the pod would
	// then mark the hand-over clean, rightly.
	select {
	case <-pushing:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never pushed the message to the session's port")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active.receiver.Stop(ctx)

	end := handBackEnd(t, active)
	if end["linger_end"] != string(lingerEndReceiving) {
		t.Fatalf("the stop's linger ended %v, want the standby receiving: this row is about the mark's worker gate, which only a hand-over that went through otherwise reaches: %v", end["linger_end"], end)
	}
	if end["hand_back_marked"] != false || end["no_mark"] != noMarkWorkerPushing {
		t.Fatalf("the stop's mark: %v, want none, because a serving-port worker still held a message of the term", end)
	}
}
