package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// receiveTestSubject is a subject the stream stores, so a publish on it reaches the durable.
const receiveTestSubject = "notifications.github.acme.widgets.push.branch.main"

// testReceiveTiming is the receive loop's production timing with its waits shortened, so a test
// sees each of them in well under a second; the settle hold's caps stay the production ones, one
// of the durable's own ack waits and a second after a hard take-over and the hand-back delay and a
// second after a graceful one, and so do a graceful stop's hand-back delay and linger cap. A
// stopping holder waits 250 ms, five of testLeaseTiming's standby polls, for another pod to take
// its released lease; a test of the hand-over itself takes handBackTiming.
func testReceiveTiming() receiveConfig {
	production := productionReceiveConfig()
	return receiveConfig{
		settlePoll:        50 * time.Millisecond,
		settleCap:         production.settleCap,
		handBackSettleCap: production.handBackSettleCap,
		bindPoll:          50 * time.Millisecond,
		alarmAfter:        time.Hour,
		alarmEvery:        time.Hour,
		inProgressEvery:   inProgressEvery,
		inflightGrace:     300 * time.Millisecond,
		handBackDelay:     handBackDelay,
		lingerPoll:        50 * time.Millisecond,
		lingerMax:         lingerMax,
		lingerUntaken:     250 * time.Millisecond,
	}
}

// handBackTiming is testReceiveTiming with the production wait for another pod to take the
// released lease, for a stopping pod whose hand-over a test watches.
func handBackTiming() receiveConfig {
	timing := testReceiveTiming()
	timing.lingerUntaken = lingerUntaken
	return timing
}

// testLeaseTiming is a lease that a standby takes within a second of its holder going quiet.
func testLeaseTiming() leaseConfig {
	return leaseConfig{renewEvery: 100 * time.Millisecond, ttl: 600 * time.Millisecond, margin: 200 * time.Millisecond, standbyPoll: 50 * time.Millisecond}
}

// receivedMessage is one message a test receiver's handler took, and the delivery count the
// durable's metadata gave it.
type receivedMessage struct {
	data      string
	delivered uint64
	at        time.Time
}

// receiverRig is one pod's receiving, in process: a bus client of its own, the listener state
// bucket on a connection of its own as main opens it, the pod's lease and its receive loop.
type receiverRig struct {
	client   *bus.Client
	lease    *lease
	receiver *durableReceiver
	logs     *cmdtest.LockedBuffer
	consumer string
}

// newReceiverRig builds a pod named instance of machineID's listener on the NATS at uri, whose
// receive loop hands each message to handle, without starting its lease or its receive loop.
func newReceiverRig(t *testing.T, uri, machineID, instance string, handle durableHandler, timing receiveConfig, leaseTiming leaseConfig) *receiverRig {
	t.Helper()
	return newReceiverRigVia(t, uri, uri, machineID, instance, handle, timing, leaseTiming)
}

// newReceiverRigVia is newReceiverRig with the bus client connected at busURI (a relay, say) and
// the listener state bucket's connection at stateURI.
func newReceiverRigVia(t *testing.T, busURI, stateURI, machineID, instance string, handle durableHandler, timing receiveConfig, leaseTiming leaseConfig) *receiverRig {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{busURI}, bus.WithReplicas(1), bus.WithName("receive-test "+instance))
	if err != nil {
		t.Fatalf("connect %s: %v", instance, err)
	}
	stateConn := testnats.Connect(t, stateURI)
	kv, err := openListenerState(stateConn, 1, listenerStateRequestWait)
	if err != nil {
		t.Fatalf("open the listener state bucket for %s: %v", instance, err)
	}
	logs := &cmdtest.LockedBuffer{}
	logger := logging.NewWithWriter(machineID, logs)
	rig := &receiverRig{
		client:   client,
		lease:    newLease(kv, machineID, instance, leaseTiming, time.Now, logger),
		logs:     logs,
		consumer: bus.ListenerDurable(machineID),
	}
	rig.receiver = newDurableReceiver(durableReceiverOptions{
		client:   client,
		consumer: rig.consumer,
		lease:    rig.lease,
		handle:   handle,
		refused:  func(err error) { t.Errorf("%s refused the durable: %v", instance, err) },
		logger:   logger,
		timing:   timing,
	})
	client.AddReconnectHook(rig.receiver.reconnected)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rig.receiver.Stop(ctx)
		client.Close()
		stateConn.Close()
		if t.Failed() {
			t.Logf("%s log:\n%s", instance, logs.String())
		}
	})
	return rig
}

// startReceiverRig is newReceiverRig with the lease and the receive loop started.
func startReceiverRig(t *testing.T, uri, machineID, instance string, handle durableHandler, timing receiveConfig, leaseTiming leaseConfig) *receiverRig {
	t.Helper()
	rig := newReceiverRig(t, uri, machineID, instance, handle, timing, leaseTiming)
	rig.lease.Start(context.Background())
	rig.receiver.Start()
	return rig
}

// awaitState waits until the rig's receive loop stands at state and returns when it did.
func (rig *receiverRig) awaitState(t *testing.T, state receiveState, within time.Duration) time.Time {
	t.Helper()
	waitFor(t, within, "the receive loop to be "+state.String(), func() bool { return rig.receiver.report().State == state })
	return time.Now()
}

// recordingHandler acks each message and sends what it took to the returned channel.
func recordingHandler() (durableHandler, <-chan receivedMessage) {
	received := make(chan receivedMessage, 64)
	return func(_ context.Context, msg *natsgo.Msg) {
		received <- describeReceived(msg)
		_ = msg.Ack()
	}, received
}

func describeReceived(msg *natsgo.Msg) receivedMessage {
	message := receivedMessage{data: string(msg.Data), at: time.Now()}
	if meta, err := msg.Metadata(); err == nil {
		message.delivered = meta.NumDelivered
	}
	return message
}

func publishDurable(t *testing.T, client *bus.Client, data string) {
	t.Helper()
	if _, err := client.JS().Publish(receiveTestSubject, []byte(data)); err != nil {
		t.Fatalf("publish %q: %v", data, err)
	}
}

func awaitReceived(t *testing.T, received <-chan receivedMessage, within time.Duration, what string) receivedMessage {
	t.Helper()
	select {
	case message := <-received:
		return message
	case <-time.After(within):
		t.Fatalf("%s: nothing received within %s", what, within)
		return receivedMessage{}
	}
}

func noneReceived(t *testing.T, received <-chan receivedMessage, within time.Duration, what string) {
	t.Helper()
	select {
	case message := <-received:
		t.Fatalf("%s: received %q (delivery %d)", what, message.data, message.delivered)
	case <-time.After(within):
	}
}

// subscribeListenerDurable creates consumer as the holder does and subscribes handler to its
// deliver subject on client's connection, as the holder's receive loop does, with neither the
// lease nor the local queue: for a test of the delivery handler over a real durable.
func subscribeListenerDurable(client *bus.Client, consumer string, handler natsgo.MsgHandler) (*natsgo.Subscription, error) {
	info, err := ensureListenerDurable(context.Background(), client, consumer)
	if err != nil {
		return nil, err
	}
	return client.Conn.Subscribe(info.Config.DeliverSubject, handler)
}

// putLeaseRecord writes machineID's lease record as another pod would have left it.
func putLeaseRecord(t *testing.T, uri, machineID, record string) {
	t.Helper()
	conn := testnats.Connect(t, uri)
	defer conn.Close()
	kv, err := openListenerState(conn, 1, listenerStateRequestWait)
	if err != nil {
		t.Fatalf("open the listener state bucket: %v", err)
	}
	if _, err := kv.Put("lease."+bus.ListenerDurable(machineID), []byte(record)); err != nil {
		t.Fatalf("write the lease record: %v", err)
	}
}

// A pod that is subscribed to the durable's deliver subject without holding the lease (the
// window before its unsubscribe lands, or a subscription nats.go sent again after a reconnect in
// place) drops what it receives with no ack and no NAK, so the durable redelivers it after its ack
// wait to the pod that holds the lease then.
func TestADurableMessageReceivedWithoutTheLeaseIsDroppedAndRedeliveredToTheHolder(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-stray"
	var strayHandled atomic.Int32
	stray := newReceiverRig(t, uri, machineID, "pod-stray", func(_ context.Context, msg *natsgo.Msg) {
		strayHandled.Add(1)
		_ = msg.Ack()
	}, testReceiveTiming(), testLeaseTiming())
	info, err := ensureListenerDurable(context.Background(), stray.client, stray.consumer)
	if err != nil {
		t.Fatalf("create the durable: %v", err)
	}
	if err := stray.receiver.begin(info.Config.DeliverSubject, settleHold{}, 0); err != nil {
		t.Fatalf("subscribe the stray pod: %v", err)
	}

	published := time.Now()
	publishDurable(t, stray.client, "dropped")
	waitFor(t, 5*time.Second, "the durable to deliver the message to the stray pod", func() bool {
		info, err := stray.client.JS().ConsumerInfo(bus.Stream, stray.consumer)
		return err == nil && info.NumAckPending == 1 && info.NumPending == 0
	})
	// A NAK would have the durable deliver it again at once, to the stray pod itself.
	time.Sleep(time.Second)
	info, err = stray.client.JS().ConsumerInfo(bus.Stream, stray.consumer)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	if info.NumAckPending != 1 || info.NumRedelivered != 0 || info.Delivered.Consumer != 1 {
		t.Fatalf("after the stray pod received the message: ack pending %d, redelivered %d, delivered %d; want it held unacked, delivered once",
			info.NumAckPending, info.NumRedelivered, info.Delivered.Consumer)
	}
	if got := strayHandled.Load(); got != 0 {
		t.Fatalf("the pod without the lease handled %d messages, want none", got)
	}
	stray.receiver.end(context.Background())

	handle, received := recordingHandler()
	startReceiverRig(t, uri, machineID, "pod-holder", handle, testReceiveTiming(), testLeaseTiming())
	message := awaitReceived(t, received, 15*time.Second, "the holder")
	if message.data != "dropped" || message.delivered != 2 {
		t.Fatalf("the holder received %q as delivery %d, want the dropped message's redelivery (delivery 2)", message.data, message.delivered)
	}
	if after := message.at.Sub(published); after < consumerAckWait-time.Second {
		t.Fatalf("the holder received the dropped message %s after its first delivery, want it held until the %s ack wait ran out", after, consumerAckWait)
	}
	if got := strayHandled.Load(); got != 0 {
		t.Fatalf("the pod without the lease handled %d messages, want none", got)
	}
}

// The holder keeps every message it has received alive with InProgress, the one in hand and the
// ones queued behind it, so a prompt that takes longer than the 6 s ack wait is not redelivered
// while it runs, and neither is anything queued behind it.
func TestInProgressKeepsTheHoldersMessagesPastTheAckWait(t *testing.T) {
	uri := testnats.URL(t)
	release := make(chan struct{})
	inHand := make(chan struct{})
	received := make(chan receivedMessage, 64)
	var calls atomic.Int32
	rig := startReceiverRig(t, uri, "box-slow", "pod-a", func(_ context.Context, msg *natsgo.Msg) {
		if calls.Add(1) == 1 {
			close(inHand)
			<-release
		}
		received <- describeReceived(msg)
		_ = msg.Ack()
	}, testReceiveTiming(), testLeaseTiming())
	rig.awaitState(t, receiveActive, 10*time.Second)

	publishDurable(t, rig.client, "in hand")
	publishDurable(t, rig.client, "queued")
	select {
	case <-inHand:
	case <-time.After(5 * time.Second):
		t.Fatal("the first message never reached the handler")
	}
	time.Sleep(consumerAckWait + 3*time.Second)
	close(release)

	for _, want := range []string{"in hand", "queued"} {
		message := awaitReceived(t, received, 5*time.Second, want)
		if message.data != want || message.delivered != 1 {
			t.Fatalf("received %q as delivery %d, want %q delivered once", message.data, message.delivered, want)
		}
	}
	noneReceived(t, received, 2*time.Second, "after both messages were acked")
	if got := calls.Load(); got != 2 {
		t.Fatalf("the handler ran %d times, want 2", got)
	}
}

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

// bindAsADeadHolder creates machineID's durable and subscribes to its deliver subject on a
// connection of its own that never acks, as a holder whose node was lost without a FIN: the server
// keeps its interest, so the durable reads push-bound, and goes on pushing messages to it. It
// returns a client to publish and read the durable with.
func bindAsADeadHolder(t *testing.T, uri, machineID string) *bus.Client {
	t.Helper()
	client, _ := bindAsAHolder(t, uri, machineID, func(*natsgo.Msg) {})
	return client
}

// bindAsAHolder is bindAsADeadHolder with handle taking each message the holder receives, and the
// holder's subscription returned beside the client.
func bindAsAHolder(t *testing.T, uri, machineID string, handle natsgo.MsgHandler) (*bus.Client, *natsgo.Subscription) {
	t.Helper()
	setup := testnats.Connect(t, uri)
	t.Cleanup(setup.Close)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	consumer := bus.ListenerDurable(machineID)
	info, err := ensureListenerDurable(context.Background(), client, consumer)
	if err != nil {
		t.Fatalf("create the durable: %v", err)
	}
	sub, err := setup.Subscribe(info.Config.DeliverSubject, handle)
	if err != nil {
		t.Fatalf("subscribe as the holder: %v", err)
	}
	waitFor(t, 5*time.Second, "the durable to read push-bound", func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
		return err == nil && info.PushBound
	})
	return client, sub
}

// leaveARetryOutstanding publishes data to machineID's durable and has a holder on a connection
// of its own NAK it for a retry a minute later, as the delivery handler NAKs a prompt that failed,
// then go: the message stays outstanding, counted in the durable's NumAckPending, for that minute.
// It returns a client to publish and read the durable with.
func leaveARetryOutstanding(t *testing.T, uri, machineID, data string) *bus.Client {
	t.Helper()
	naked := make(chan struct{}, 1)
	client, holder := bindAsAHolder(t, uri, machineID, func(msg *natsgo.Msg) {
		if msg.NakWithDelay(time.Minute) == nil {
			select {
			case naked <- struct{}{}:
			default:
			}
		}
	})
	publishDurable(t, client, data)
	select {
	case <-naked:
	case <-time.After(5 * time.Second):
		t.Fatalf("the holder never NAKed %q", data)
	}
	if err := holder.Unsubscribe(); err != nil {
		t.Fatalf("unsubscribe the holder: %v", err)
	}
	waitFor(t, 5*time.Second, "the retry to stay outstanding with no subscriber", func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, bus.ListenerDurable(machineID))
		return err == nil && !info.PushBound && info.NumAckPending == 1 && info.NumPending == 0
	})
	return client
}

// publishToTheDeadHolder publishes data and waits until the durable has pushed it to the dead
// holder alone, and returns when it had.
func publishToTheDeadHolder(t *testing.T, client *bus.Client, machineID, data string) time.Time {
	t.Helper()
	publishDurable(t, client, data)
	waitFor(t, 5*time.Second, "the dead holder to have "+data, func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, bus.ListenerDurable(machineID))
		return err == nil && info.NumAckPending == 1 && info.NumPending == 0
	})
	return time.Now()
}

// settleHoldEnd waits for the rig's log record of its settle hold's end and returns it.
func (rig *receiverRig) settleHoldEnd(t *testing.T, within time.Duration) map[string]any {
	t.Helper()
	return rig.logRecord(t, "settle hold ended; delivering the held messages in stream order", within)
}

// awaitRecord waits until the rig's lease record satisfies cond, as its renewals write it.
func (rig *receiverRig) awaitRecord(t *testing.T, what string, cond func(leaseRecord) bool) {
	t.Helper()
	var last leaseRecord
	deadline := time.Now().Add(5 * time.Second)
	for {
		if entry, err := rig.lease.kv.Get(rig.lease.key); err == nil {
			if record, err := decodeLeaseRecord(entry.Value()); err == nil {
				last = record
				if cond(record) {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the lease record is not %s within 5s: %+v", what, last.leaseFields)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A pod that takes the lease over an expired record nobody released subscribes at once, beside the
// dead holder's binding the server still counts, and holds what it receives while a message the
// dead holder had is outstanding: that message comes back once its ack wait runs out, after a newer
// one reached the new holder, and is still delivered first. The hold ends as soon as it has every
// outstanding message, well before its cap, and the record then says the new holder subscribed
// beside a stale binding and is no longer unsettled.
func TestASettleHoldWaitsForAMessageOutstandingWithADeadHolderAndDeliversInStreamOrder(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-expired"
	putLeaseRecord(t, uri, machineID, `{"holder":"pod-dead","boot":"0123456789abcdef"}`)
	client := bindAsADeadHolder(t, uri, machineID)
	pushed := publishToTheDeadHolder(t, client, machineID, "older")

	handle, received := recordingHandler()
	rig := startReceiverRig(t, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	waitFor(t, 10*time.Second, "pod-b to take the expired lease", rig.lease.Holding)
	if status := rig.lease.Status(); status.How != leaseTakenExpired || !status.Settle {
		t.Fatalf("pod-b's lease = %+v, want taken over an expired record, settling", status)
	}
	subscribed := rig.awaitState(t, receiveActive, 5*time.Second)
	if after := subscribed.Sub(pushed); after >= consumerAckWait {
		t.Fatalf("pod-b subscribed %s after the older message went to the dead holder, want at once, before its redelivery", after)
	}
	if !strings.Contains(rig.logs.String(), "receiving beside it") {
		t.Fatalf("pod-b did not log receiving beside the dead holder's binding:\n%s", rig.logs.String())
	}
	// The newer message reaches pod-b at once; the older one only when its ack wait runs out.
	publishDurable(t, client, "newer")

	first := awaitReceived(t, received, consumerAckWait+5*time.Second, "pod-b's first message")
	second := awaitReceived(t, received, 5*time.Second, "pod-b's second message")
	if first.data != "older" || first.delivered != 2 || second.data != "newer" || second.delivered != 1 {
		t.Fatalf("pod-b delivered %q (delivery %d) then %q (delivery %d), want the older message redelivered, then the newer one",
			first.data, first.delivered, second.data, second.delivered)
	}
	if after := first.at.Sub(pushed); after < consumerAckWait-time.Second || after > consumerAckWait+3*time.Second {
		t.Fatalf("pod-b delivered %s after the older message went to the dead holder, want once its %s ack wait brought it back", after, consumerAckWait)
	}
	if end := rig.settleHoldEnd(t, time.Second); end["outcome"] != "settled" || end["messages"] != float64(2) {
		t.Fatalf("the settle hold ended %v, want settled holding both messages", end)
	}
	rig.awaitRecord(t, "marked stale_binding and settled", func(record leaseRecord) bool {
		return record.Holder == "pod-b" && record.StaleBinding && !record.Unsettled
	})
}

// A term that must settle with nothing outstanding ends its hold at its first read of the durable,
// and delivers what is published then at once. The expired record it took carried stale_binding;
// finding the durable not push-bound, the new holder clears it.
func TestASettleHoldWithNothingOutstandingEndsAtOnce(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-expired-idle"
	putLeaseRecord(t, uri, machineID, `{"holder":"pod-dead","boot":"0123456789abcdef","stale_binding":true}`)
	handle, received := recordingHandler()
	rig := startReceiverRig(t, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	subscribed := rig.awaitState(t, receiveActive, 10*time.Second)
	if status := rig.lease.Status(); status.How != leaseTakenExpired || !status.Settle {
		t.Fatalf("pod-b's lease = %+v, want taken over an expired record, settling", status)
	}
	end := rig.settleHoldEnd(t, time.Second)
	if held, _ := end["held_ms"].(float64); end["outcome"] != "settled" || end["messages"] != float64(0) || held > 500 {
		t.Fatalf("the settle hold ended %v, want settled at once with nothing held", end)
	}
	publishDurable(t, rig.client, "fresh")
	published := time.Now()
	if message := awaitReceived(t, received, 5*time.Second, "pod-b"); message.data != "fresh" || message.at.Sub(published) > time.Second {
		t.Fatalf("pod-b delivered %q %s after its publish (%s after it subscribed), want it at once",
			message.data, message.at.Sub(published), message.at.Sub(subscribed))
	}
	rig.awaitRecord(t, "free of both flags", func(record leaseRecord) bool {
		return record.Holder == "pod-b" && !record.StaleBinding && !record.Unsettled
	})
}

// The settle hold ends at its cap even while a message is still outstanding, then delivers what it
// holds; the outstanding message follows when it comes back. In production the cap is one of the
// durable's ack waits and a second, 7 s, after a hard take-over, and the hand-back delay and a
// second, 3 s, after a graceful one.
func TestASettleHoldEndsAtItsCap(t *testing.T) {
	if got := productionReceiveConfig().settleCap; got != 7*time.Second {
		t.Fatalf("the production settle cap is %s, want 7s: a %s ack wait and a second", got, consumerAckWait)
	}
	if got := productionReceiveConfig().handBackSettleCap; got != 3*time.Second {
		t.Fatalf("the production settle cap after a hand-back is %s, want 3s: the %s hand-back delay and a second", got, handBackDelay)
	}
	uri := testnats.URL(t)
	const machineID = "box-expired-cap"
	putLeaseRecord(t, uri, machineID, `{"holder":"pod-dead","boot":"0123456789abcdef"}`)
	client := bindAsADeadHolder(t, uri, machineID)
	publishToTheDeadHolder(t, client, machineID, "older")

	timing := testReceiveTiming()
	timing.settleCap = 2 * time.Second
	handle, received := recordingHandler()
	rig := startReceiverRig(t, uri, machineID, "pod-b", handle, timing, testLeaseTiming())
	subscribed := rig.awaitState(t, receiveActive, 10*time.Second)
	publishDurable(t, client, "newer")

	first := awaitReceived(t, received, timing.settleCap+2*time.Second, "pod-b's first message")
	if after := first.at.Sub(subscribed); first.data != "newer" || after < timing.settleCap-100*time.Millisecond || after >= consumerAckWait {
		t.Fatalf("pod-b delivered %q %s after it subscribed, want the newer message once the %s cap ended the hold", first.data, after, timing.settleCap)
	}
	if end := rig.settleHoldEnd(t, time.Second); end["outcome"] != "cap" || end["messages"] != float64(1) || end["ack_pending"] != float64(2) {
		t.Fatalf("the settle hold ended %v, want at the cap holding one message of two outstanding", end)
	}
	if second := awaitReceived(t, received, consumerAckWait+3*time.Second, "pod-b's second message"); second.data != "older" || second.delivered != 2 {
		t.Fatalf("pod-b then delivered %q (delivery %d), want the older message's redelivery", second.data, second.delivered)
	}
	rig.awaitRecord(t, "settled", func(record leaseRecord) bool { return record.Holder == "pod-b" && !record.Unsettled })
}

// A settle hold lasts at most as long as the previous holder can still have messages outstanding,
// counted from the subscribe: one ack wait and a second after a take-over over an expired record or
// from a released one carrying stale_binding, the hand-back delay and a second after a graceful
// hand-back (a released record carrying unsettled alone). A message NAKed for a retry a minute
// later stays outstanding past either, so the hold runs to its cap and then delivers what it holds;
// at the old cap of two ack waits and a second, 13 s, a graceful take-over delivered 14 s after the
// stop and a hard one 19 s after the kill.
func TestASettleHoldsCapIsWhatThePreviousHolderCanStillHaveOutstanding(t *testing.T) {
	for _, row := range []struct {
		name, machineID, record string
		how                     leaseAcquisition
		limit                   time.Duration
	}{
		{"expired", "box-cap-expired", `{"holder":"pod-dead","boot":"0123456789abcdef"}`, leaseTakenExpired, consumerAckWait + settleMargin},
		{"released with a stale binding", "box-cap-stale", `{"holder":"pod-gone","boot":"0123456789abcdef","released":true,"stale_binding":true}`, leaseTakenReleased, consumerAckWait + settleMargin},
		{"handed back", "box-cap-handed-back", `{"holder":"pod-gone","boot":"0123456789abcdef","released":true,"unsettled":true}`, leaseTakenReleased, handBackDelay + settleMargin},
	} {
		t.Run(row.name, func(t *testing.T) {
			uri := testnats.URL(t)
			putLeaseRecord(t, uri, row.machineID, row.record)
			client := leaveARetryOutstanding(t, uri, row.machineID, "retry")
			handle, received := recordingHandler()
			rig := startReceiverRig(t, uri, row.machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
			subscribed := rig.awaitState(t, receiveActive, 10*time.Second)
			if status := rig.lease.Status(); status.How != row.how || !status.Settle {
				t.Fatalf("pod-b's lease = %+v, want taken from a record %s, settling", status, row.how)
			}
			publishDurable(t, client, "fresh")

			first := awaitReceived(t, received, 15*time.Second, "pod-b's first message")
			if after := first.at.Sub(subscribed); first.data != "fresh" || after < row.limit-200*time.Millisecond || after > row.limit+700*time.Millisecond {
				t.Fatalf("pod-b delivered %q %s after it subscribed, want the fresh message once the %s cap ended the hold", first.data, after.Round(time.Millisecond), row.limit)
			}
			if end := rig.settleHoldEnd(t, time.Second); end["outcome"] != "cap" || end["messages"] != float64(1) || end["ack_pending"] != float64(2) {
				t.Fatalf("the settle hold ended %v, want at the cap holding one message of two outstanding", end)
			}
			noneReceived(t, received, time.Second, "while the retry's delay runs")
		})
	}
}

// A graceful stop of a pod during a settle hold over an expired record, the dead holder's binding
// gone with its connection (a SIGKILL), marks the released record stale_binding as well as
// unsettled: the message the dead holder had comes back one ack wait after its delivery, so the
// next holder's hold must wait that long too, not the hand-back delay a record carrying unsettled
// alone allows. The next holder then delivers it before the newer message the stopped pod handed
// back.
func TestAGracefulStopDuringAnAckWaitHoldMarksTheRecordStale(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-stop-in-ack-wait-hold"
	putLeaseRecord(t, uri, machineID, `{"holder":"pod-dead","boot":"0123456789abcdef"}`)
	client, dead := bindAsAHolder(t, uri, machineID, func(*natsgo.Msg) {})
	publishToTheDeadHolder(t, client, machineID, "older")
	if err := dead.Unsubscribe(); err != nil {
		t.Fatalf("unsubscribe the dead holder: %v", err)
	}
	waitFor(t, 5*time.Second, "the dead holder's binding to go", func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, bus.ListenerDurable(machineID))
		return err == nil && !info.PushBound
	})

	stopping := startReceiverRig(t, uri, machineID, "pod-b", func(_ context.Context, msg *natsgo.Msg) {
		t.Errorf("pod-b delivered %q during its hold", msg.Data)
	}, testReceiveTiming(), testLeaseTiming())
	stopping.awaitState(t, receiveActive, 10*time.Second)
	if status := stopping.lease.Status(); status.How != leaseTakenExpired || !status.Settle {
		t.Fatalf("pod-b's lease = %+v, want taken over an expired record, settling", status)
	}
	publishDurable(t, client, "newer")
	waitFor(t, 5*time.Second, "pod-b to hold the newer message", func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, bus.ListenerDurable(machineID))
		return err == nil && info.NumAckPending == 2 && info.NumPending == 0
	})
	time.Sleep(200 * time.Millisecond) // past pod-b's intake of what the server sent it
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopping.receiver.Stop(ctx)
	if end := handBackEnd(t, stopping); end["handed_back"] != float64(1) || end["linger_end"] != "not taken" {
		t.Fatalf("pod-b's hand-back ended %v, want the newer message handed back and the linger ended with no pod to take the lease", end)
	}
	stopping.awaitRecord(t, "released, unsettled, with a stale binding", func(record leaseRecord) bool {
		return record.Holder == "pod-b" && record.Released && record.Unsettled && record.StaleBinding
	})

	handle, received := recordingHandler()
	next := startReceiverRig(t, uri, machineID, "pod-c", handle, testReceiveTiming(), testLeaseTiming())
	if must := next.logRecord(t, "listener lease term must settle; receiving at once and holding what arrives until the durable's outstanding messages are in hand", 5*time.Second); must["cap"] != (consumerAckWait + settleMargin).String() {
		t.Fatalf("pod-c's settle hold = %v, want the %s cap of a hard take-over", must, consumerAckWait+settleMargin)
	}
	for _, want := range []string{"older", "newer"} {
		if message := awaitReceived(t, received, consumerAckWait+5*time.Second, "pod-c's "+want); message.data != want {
			t.Fatalf("pod-c delivered %q, want %q: the messages in the order they were published", message.data, want)
		}
	}
	if end := next.settleHoldEnd(t, time.Second); end["outcome"] != "settled" {
		t.Fatalf("pod-c's settle hold ended %v, want settled", end)
	}
}

// A stopping pod that gets back, alone, a message it already handed back (the server redelivered it
// before any other pod subscribed) NAKs it with the hand-back delay again rather than dropping it,
// so the pod that takes the lease next delivers it after that delay instead of one ack wait later,
// which its hold, the hand-back delay and a second, does not wait for.
func TestAStoppingPodHandsARepeatBackAgain(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-repeat"
	inHand := make(chan struct{}, 1)
	active := startReceiverRig(t, uri, machineID, "pod-a", func(ctx context.Context, msg *natsgo.Msg) {
		inHand <- struct{}{}
		<-ctx.Done()
		_ = msg.Nak()
	}, handBackTiming(), testLeaseTiming())
	active.awaitState(t, receiveActive, 10*time.Second)
	// pod-b runs its lease alone at first: it takes the released record and subscribes only once
	// pod-a has let go, so every redelivery during pod-a's linger reaches pod-a alone.
	handle, received := recordingHandler()
	next := newReceiverRig(t, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	next.lease.Start(context.Background())
	waitFor(t, 5*time.Second, "pod-b to read pod-a's lease", func() bool {
		return next.lease.Status().ActiveInstance == "pod-a"
	})
	publishDurable(t, active.client, "first")
	select {
	case <-inHand:
	case <-time.After(5 * time.Second):
		t.Fatal("pod-a never took the message")
	}

	stopped := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active.receiver.Stop(ctx)
	end := handBackEnd(t, active)
	if end["handed_back"] != float64(1) || end["linger_end"] != "timeout" || end["dropped_repeats"] != float64(0) {
		t.Fatalf("pod-a's hand-back ended %v, want the message handed back, its repeat handed back again rather than dropped, and the linger run to its cap", end)
	}

	next.receiver.Start()
	message := awaitReceived(t, received, 15*time.Second, "pod-b's message")
	if after := message.at.Sub(stopped); message.data != "first" || after >= consumerAckWait {
		t.Fatalf("pod-b delivered %q %s after pod-a's stop, want the repeat handed back again, before an ack wait", message.data, after.Round(time.Millisecond))
	}
}

// A released record that is unsettled (its holder handed its messages back, and lingers on its
// subscription) or carries stale_binding (its holder subscribed beside an expired holder's
// lingering binding) tells the next holder that a binding it finds is a pod of this build's, not a
// pre-lease build's: it subscribes at once beside the binding and settles, delivering a message the
// binding had before a newer one. A released record with neither flag still counts a binding as a
// pre-lease build's, and waits it out.
func TestAReleasedRecordUnsettledOrWithAStaleBindingSettlesBesideTheBinding(t *testing.T) {
	for _, row := range []struct {
		name, machineID, record string
		settle                  bool
		// handBack has the binding NAK the first message it receives with the hand-back delay,
		// as a graceful stop does before it releases the record unsettled.
		handBack bool
	}{
		{"unsettled", "box-released-unsettled", `{"holder":"pod-gone","boot":"0123456789abcdef","released":true,"unsettled":true}`, true, true},
		{"stale binding", "box-released-stale", `{"holder":"pod-gone","boot":"0123456789abcdef","released":true,"stale_binding":true}`, true, false},
		{"neither flag", "box-released-plain", `{"holder":"pod-gone","boot":"0123456789abcdef","released":true}`, false, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			uri := testnats.URL(t)
			putLeaseRecord(t, uri, row.machineID, row.record)
			var handedBack atomic.Bool
			client, _ := bindAsAHolder(t, uri, row.machineID, func(msg *natsgo.Msg) {
				if row.handBack && handedBack.CompareAndSwap(false, true) {
					_ = msg.NakWithDelay(handBackDelay)
				}
			})
			pushed := publishToTheDeadHolder(t, client, row.machineID, "older")
			handle, received := recordingHandler()
			rig := startReceiverRig(t, uri, row.machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
			waitFor(t, 10*time.Second, "pod-b to take the released lease", rig.lease.Holding)
			if status := rig.lease.Status(); status.How != leaseTakenReleased || status.Settle != row.settle {
				t.Fatalf("pod-b's lease = %+v, want taken from a released record, settle %v", status, row.settle)
			}
			if !row.settle {
				rig.awaitState(t, receiveAwaitingBinding, 5*time.Second)
				noneReceived(t, received, time.Second, "pod-b while the binding stands")
				if state := rig.receiver.report().State; state != receiveAwaitingBinding {
					t.Fatalf("pod-b's receive loop is %s, want it waiting out the binding", state)
				}
				return
			}
			if after := rig.awaitState(t, receiveActive, 5*time.Second).Sub(pushed); after >= consumerAckWait {
				t.Fatalf("pod-b subscribed %s after the older message went to the binding, want at once", after)
			}
			if !strings.Contains(rig.logs.String(), "receiving beside it") {
				t.Fatalf("pod-b did not log receiving beside the binding:\n%s", rig.logs.String())
			}
			publishDurable(t, client, "newer")
			first := awaitReceived(t, received, consumerAckWait+5*time.Second, "pod-b's first message")
			second := awaitReceived(t, received, 5*time.Second, "pod-b's second message")
			if first.data != "older" || second.data != "newer" {
				t.Fatalf("pod-b delivered %q then %q, want the older message first", first.data, second.data)
			}
			rig.awaitRecord(t, "marked stale_binding and settled", func(record leaseRecord) bool {
				return record.Holder == "pod-b" && record.StaleBinding && !record.Unsettled
			})
		})
	}
}

// A graceful stop during a settle hold hands back what it holds and releases the record still
// unsettled and carrying stale_binding, ending its linger once no other pod takes the lease, so the
// next holder settles in its turn: beside the binding it holds the handed-back messages until the
// message the binding had comes back, and delivers all three in the order they were published.
func TestAGracefulStopDuringASettleHoldHandsTheHoldOn(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-stop-in-hold"
	putLeaseRecord(t, uri, machineID, `{"holder":"pod-dead","boot":"0123456789abcdef"}`)
	client := bindAsADeadHolder(t, uri, machineID)
	publishToTheDeadHolder(t, client, machineID, "older")

	var stoppedHandled atomic.Int32
	stopping := startReceiverRig(t, uri, machineID, "pod-b", func(_ context.Context, msg *natsgo.Msg) {
		stoppedHandled.Add(1)
		_ = msg.Ack()
	}, testReceiveTiming(), testLeaseTiming())
	stopping.awaitState(t, receiveActive, 10*time.Second)
	publishDurable(t, client, "newer one")
	publishDurable(t, client, "newer two")
	waitFor(t, 5*time.Second, "pod-b to hold both newer messages", func() bool {
		return strings.Contains(stopping.logs.String(), "receiving beside it") && func() bool {
			info, err := client.JS().ConsumerInfo(bus.Stream, bus.ListenerDurable(machineID))
			return err == nil && info.NumAckPending == 3 && info.NumPending == 0
		}()
	})
	time.Sleep(200 * time.Millisecond) // past pod-b's intake of what the server sent it
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopping.receiver.Stop(ctx)
	if got := stoppedHandled.Load(); got != 0 {
		t.Fatalf("pod-b delivered %d messages during its hold, want none", got)
	}
	if end := handBackEnd(t, stopping); end["handed_back"] != float64(2) || end["linger_end"] != "not taken" {
		t.Fatalf("pod-b's hand-back ended %v, want the two messages it held handed back and the linger ended with no pod to take the lease", end)
	}
	stopping.awaitRecord(t, "released, unsettled, with a stale binding", func(record leaseRecord) bool {
		return record.Holder == "pod-b" && record.Released && record.Unsettled && record.StaleBinding
	})

	handle, received := recordingHandler()
	next := startReceiverRig(t, uri, machineID, "pod-c", handle, testReceiveTiming(), testLeaseTiming())
	waitFor(t, 5*time.Second, "pod-c to take the released lease", next.lease.Holding)
	if status := next.lease.Status(); status.How != leaseTakenReleased || !status.Settle {
		t.Fatalf("pod-c's lease = %+v, want taken from a released record, settling", status)
	}
	for _, want := range []string{"older", "newer one", "newer two"} {
		if message := awaitReceived(t, received, consumerAckWait+5*time.Second, "pod-c's "+want); message.data != want {
			t.Fatalf("pod-c delivered %q, want %q: the messages in the order they were published", message.data, want)
		}
	}
	if end := next.settleHoldEnd(t, time.Second); end["outcome"] != "settled" {
		t.Fatalf("pod-c's settle hold ended %v, want settled", end)
	}
}

// During a settle hold, what the term holds is taken in stream order, a message the durable sent
// twice counted once; afterwards the held batch comes first and the queue behind it in arrival
// order.
func TestASettleHoldsMessagesAreTakenInStreamOrder(t *testing.T) {
	sub := &natsgo.Subscription{}
	message := func(stream uint64) *natsgo.Msg {
		return &natsgo.Msg{Sub: sub, Reply: "$JS.ACK.ENVOY_NOTIFICATIONS.listener-box.1." + strconv.FormatUint(stream, 10) + ".1.0.0"}
	}
	newTerm := func() *receiveTerm {
		return &receiveTerm{queue: make(chan *natsgo.Msg, 8), unsettled: map[*natsgo.Msg]struct{}{}, closing: make(chan struct{}), holding: true}
	}
	five, three, again, nine := message(5), message(3), message(3), message(9)
	term := newTerm()
	for _, m := range []*natsgo.Msg{five, three, again} {
		term.intake(m)
	}
	if held := term.gather(false); held != 2 {
		t.Fatalf("the hold counts %d messages, want 2: stream sequence 3 twice and 5", held)
	}
	term.intake(nine)
	term.intake(message(1))
	taken := term.takeQueued()
	var sequences []uint64
	for _, m := range taken {
		meta, _ := m.Metadata()
		sequences = append(sequences, meta.Sequence.Stream)
	}
	if !reflect.DeepEqual(sequences, []uint64{1, 3, 3, 5, 9}) || len(term.unsettled) != 0 {
		t.Fatalf("taken during the hold: stream sequences %v, %d left unsettled; want 1 3 3 5 9 and none", sequences, len(term.unsettled))
	}

	term = newTerm()
	term.intake(five)
	term.intake(three)
	term.gather(true)
	term.intake(nine)
	term.intake(message(1))
	sequences = nil
	for _, m := range term.takeQueued() {
		meta, _ := m.Metadata()
		sequences = append(sequences, meta.Sequence.Stream)
	}
	if !reflect.DeepEqual(sequences, []uint64{3, 5, 9, 1}) {
		t.Fatalf("taken after the hold: stream sequences %v, want the held 3 5, then the queue's 9 1", sequences)
	}
}

// A held batch is delivered in stream order; a message whose JetStream metadata cannot be read
// keeps the place it arrived in, and the others are ordered around it.
func TestSortByStreamSequenceKeepsAnUnreadableMessageInPlace(t *testing.T) {
	sub := &natsgo.Subscription{}
	message := func(stream uint64) *natsgo.Msg {
		return &natsgo.Msg{Sub: sub, Reply: "$JS.ACK.ENVOY_NOTIFICATIONS.listener-box.1." + strconv.FormatUint(stream, 10) + ".1.0.0"}
	}
	unreadable := &natsgo.Msg{Sub: sub, Reply: "_INBOX.not-jetstream"}
	five, three, nine, one := message(5), message(3), message(9), message(1)
	batch := []*natsgo.Msg{five, unreadable, three, nine, one}
	sortByStreamSequence(batch)
	want := []*natsgo.Msg{one, unreadable, three, five, nine}
	for index := range want {
		if batch[index] != want[index] {
			t.Fatalf("position %d holds %q, want %q", index, batch[index].Reply, want[index].Reply)
		}
	}
}

// durableAlarms counts the ERROR records of the take-over alarm in a rig's log.
func durableAlarms(logs *cmdtest.LockedBuffer) int {
	return len(durableAlarmRecords(logs))
}

// durableAlarmRecords are the ERROR records of the take-over alarm in a rig's log.
func durableAlarmRecords(logs *cmdtest.LockedBuffer) []map[string]any {
	var records []map[string]any
	for line := range strings.SplitSeq(logs.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == durableTakeoverAlarmLine && record["level"] == "ERROR" {
			records = append(records, record)
		}
	}
	return records
}

// During the first roll onto the lease, the durable is push-bound by a build from before it
// (nats.Bind). The pod that takes the lease from an absent record waits that binding out, renewing
// its lease, alarms at ERROR once it has waited past the alarm threshold and again at each repeat,
// neither deletes the durable to take it nor rewrites its policy under the old build (whose
// nats.Bind refuses a durable whose filter is not its own), and receives as soon as the old build
// lets go, correcting the policy then.
func TestAPreLeaseBindingIsWaitedOutAndAlarmedOn(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-pre-lease"
	consumer := bus.ListenerDurable(machineID)
	old, binding := durableHeldElsewhere(t, uri, consumer)
	before, err := old.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}

	timing := testReceiveTiming()
	timing.alarmAfter, timing.alarmEvery = 300*time.Millisecond, 200*time.Millisecond
	handle, received := recordingHandler()
	rig := startReceiverRig(t, uri, machineID, "pod-new", handle, timing, testLeaseTiming())
	rig.awaitState(t, receiveAwaitingBinding, 10*time.Second)
	waitFor(t, 10*time.Second, "the alarm and its repeat", func() bool { return durableAlarms(rig.logs) >= 2 })
	if !rig.lease.Holding() {
		t.Fatal("the waiting pod let its lease lapse")
	}
	if state := rig.receiver.report().State; state != receiveAwaitingBinding {
		t.Fatalf("the receive loop is %s while the old build holds the durable, want it waiting", state)
	}
	if !binding.IsValid() {
		t.Fatal("the old build's binding is gone")
	}
	info, err := old.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil || !info.Created.Equal(before.Created) || !info.PushBound || !reflect.DeepEqual(info.Config, before.Config) {
		t.Fatalf("the durable while the new pod waits: %+v %v, want the old build's as it made it, still push-bound", info, err)
	}

	old.Close()
	rig.awaitState(t, receiveActive, 5*time.Second)
	publishDurable(t, rig.client, "after the old build")
	if message := awaitReceived(t, received, 5*time.Second, "the new pod"); message.data != "after the old build" {
		t.Fatalf("the new pod received %q", message.data)
	}
	info, err = rig.client.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil || !info.Created.Equal(before.Created) || info.Config.FilterSubject != consumerFilterAll || info.Config.AckWait != consumerAckWait {
		t.Fatalf("the durable once the new pod receives: %+v %v, want the same durable under the consumer policy", info, err)
	}
}

// consumerInfoDeniedNATS runs a NATS server with JetStream and two users, and returns a URL for
// each: granted holds every permission, and denied every one but reading a consumer of the stream
// (CONSUMER.INFO), so a pod whose bus client connects as denied cannot read its durable while its
// lease renews on a granted connection. NATS answers a request it denies with nothing, so each
// read waits out the JetStream wait (10 s) before it fails.
func consumerInfoDeniedNATS(t *testing.T) (granted, denied string) {
	t.Helper()
	config := fmt.Sprintf(`jetstream {}
authorization {
  users = [
    { user: granted, password: granted },
    { user: denied, password: denied, permissions: { publish: { allow: [">"], deny: [%q] } } }
  ]
}
`, "$JS.API.CONSUMER.INFO."+bus.Stream+".*")
	_, uri := testnats.StartConfig(t, config, []string{"-js"}, false)
	return strings.Replace(uri, "nats://", "nats://granted:granted@", 1), strings.Replace(uri, "nats://", "nats://denied:denied@", 1)
}

// A lease holder that cannot prepare its durable (here it may not read it) says so in /healthz,
// 200 degraded with the reason, and once its failures have lasted the alarm bound, timed from the
// start of the first, logs the alarm line at ERROR with that reason and gives the lease up: the
// other pod takes it at its next poll and delivers what was published meanwhile, and the stuck pod,
// a standby again, does not take it back from a pod that receives. Before, the holder retried
// forever and renewed its lease, so no standby ever tried, /healthz answered 200 healthy and no
// alarm fired.
func TestAHolderThatCannotPrepareItsDurableAlarmsAndGivesTheLeaseUp(t *testing.T) {
	granted, denied := consumerInfoDeniedNATS(t)
	const machineID = "box-stuck"
	// The bound is past one failed read, so /healthz is read between the first failure and the
	// give-up.
	timing := testReceiveTiming()
	timing.alarmAfter = 15 * time.Second
	stuckHandle, stuckReceived := recordingHandler()
	stuck := newReceiverRigVia(t, denied, granted, machineID, "pod-stuck", stuckHandle, timing, testLeaseTiming())
	stuck.lease.Start(context.Background())
	stuck.receiver.Start()
	stuck.awaitState(t, receivePreparing, 10*time.Second)
	handle, received := recordingHandler()
	standby := startReceiverRig(t, granted, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	waitFor(t, 5*time.Second, "pod-b to read pod-stuck's lease", func() bool {
		return standby.lease.Status().ActiveInstance == "pod-stuck"
	})
	publishDurable(t, standby.client, "while the holder is stuck")

	get := healthzFor(t, stuck.client, stuck.receiver)
	var code int
	var body map[string]any
	waitFor(t, 15*time.Second, "pod-stuck's /healthz to report its failing durable", func() bool {
		code, body = get()
		return body["status"] != "healthy"
	})
	if code != http.StatusOK || body["status"] != "degraded" || body["role"] != "standby" || body["receive"] != receivePreparing.String() ||
		!strings.Contains(fmt.Sprint(body["error"]), "read the durable consumer") {
		t.Fatalf("pod-stuck's /healthz while its durable read fails = %d %v, want 200 degraded, role standby, receive %q and an error naming the failed read",
			code, body, receivePreparing.String())
	}
	if durableAlarms(stuck.logs) != 0 {
		t.Fatal("pod-stuck alarmed before its failures lasted the alarm bound")
	}

	message := awaitReceived(t, received, 30*time.Second, "pod-b once pod-stuck gave the lease up")
	if message.data != "while the holder is stuck" {
		t.Fatalf("pod-b received %q", message.data)
	}
	alarms := durableAlarmRecords(stuck.logs)
	if len(alarms) != 1 || !strings.Contains(fmt.Sprint(alarms[0]["reason"]), "read the durable consumer") {
		t.Fatalf("pod-stuck's alarms: %v, want one at ERROR whose reason names the failed read", alarms)
	}
	if waited, _ := alarms[0]["waited_ms"].(float64); time.Duration(waited)*time.Millisecond < timing.alarmAfter {
		t.Fatalf("pod-stuck alarmed after %v ms of failures, want the %s bound", alarms[0]["waited_ms"], timing.alarmAfter)
	}
	// pod-stuck polls every 50 ms as a standby: a second is twenty chances to take the lease back.
	time.Sleep(time.Second)
	if stuck.lease.Holding() || !standby.lease.Holding() {
		t.Fatalf("after the give-up: pod-stuck holding %v, pod-b holding %v; want pod-b alone", stuck.lease.Holding(), standby.lease.Holding())
	}
	if code, body := get(); code != http.StatusOK || body["status"] != "healthy" || body["role"] != "standby" || body["active_instance"] != "pod-b" {
		t.Fatalf("pod-stuck's /healthz after the give-up = %d %v, want 200 healthy, a standby naming pod-b", code, body)
	}
	noneReceived(t, stuckReceived, time.Millisecond, "pod-stuck")
}

// A pod that cannot prepare its durable and has no other pod to give the lease to tries again: a
// sole lease holder gives the lease up at the alarm bound and takes it back once no other pod has,
// and a listener without a lease, the machine's one deliverer, keeps retrying and answers /healthz
// 503 unhealthy past the bound, so its runtime counts it unready, as the bounded subscribe that
// exited did.
func TestAPodThatCannotPrepareItsDurableAloneTriesAgain(t *testing.T) {
	granted, denied := consumerInfoDeniedNATS(t)
	timing := testReceiveTiming()
	timing.alarmAfter, timing.alarmEvery = 5*time.Second, time.Millisecond
	t.Run("sole holder", func(t *testing.T) {
		t.Parallel()
		handle, _ := recordingHandler()
		rig := newReceiverRigVia(t, denied, granted, "box-stuck-alone", "pod-a", handle, timing, testLeaseTiming())
		rig.lease.Start(context.Background())
		rig.receiver.Start()
		waitFor(t, 20*time.Second, "pod-a to give its lease up and take it again", func() bool {
			status := rig.lease.Status()
			return status.Term >= 2 && status.Holding
		})
		if alarms := durableAlarmRecords(rig.logs); len(alarms) != 1 || !strings.Contains(fmt.Sprint(alarms[0]["reason"]), "read the durable consumer") {
			t.Fatalf("pod-a's alarms: %v, want one at ERROR whose reason names the failed read", alarms)
		}
		if status := rig.lease.Status(); status.How != leaseTakenReleased {
			t.Fatalf("pod-a's second term: %+v, want taken from the record it released", status)
		}
	})
	t.Run("without a lease", func(t *testing.T) {
		t.Parallel()
		client, err := bus.ConnectOwningStream([]string{denied}, bus.WithReplicas(1), bus.WithName("receive-test without a lease"))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		logs := &cmdtest.LockedBuffer{}
		handle, _ := recordingHandler()
		receiver := newDurableReceiver(durableReceiverOptions{
			client:   client,
			consumer: bus.ListenerDurable("box-stuck-unleased"),
			handle:   handle,
			refused:  func(err error) { t.Errorf("refused the durable: %v", err) },
			logger:   logging.NewWithWriter("box-stuck-unleased", logs),
			timing:   timing,
		})
		receiver.Start()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			receiver.Stop(ctx)
			client.Close()
			if t.Failed() {
				t.Logf("log:\n%s", logs.String())
			}
		})
		get := healthzFor(t, client, receiver)
		var code int
		var body map[string]any
		waitFor(t, 15*time.Second, "/healthz to report the failing durable", func() bool {
			code, body = get()
			return body["status"] != "healthy"
		})
		if code != http.StatusServiceUnavailable || body["status"] != "unhealthy" || body["receive"] != receivePreparing.String() ||
			!strings.Contains(fmt.Sprint(body["error"]), "read the durable consumer") {
			t.Fatalf("/healthz once the durable read has failed past the bound = %d %v, want 503 unhealthy, receive %q and an error naming the failed read",
				code, body, receivePreparing.String())
		}
		waitFor(t, time.Second, "the alarm", func() bool { return durableAlarms(logs) > 0 })
		if alarms := durableAlarmRecords(logs); len(alarms) != 1 || !strings.Contains(fmt.Sprint(alarms[0]["reason"]), "read the durable consumer") {
			t.Fatalf("alarms: %v, want one at ERROR whose reason names the failed read", alarms)
		}
		if state := receiver.report().State; state != receivePreparing {
			t.Fatalf("the receive loop is %s, want it still preparing the durable", state)
		}
	})
}

// Every durable an earlier listener created filters on its build's stream subjects. The holder
// corrects it to the consumer policy's filter, every subject, and it goes on delivering what is
// published afterwards on every subject the stream stores. nats-server 2.10 accepts an update that
// clears a push durable's filter instead, and then delivers nothing published after it.
func TestADurableFilteredOnTheStreamSubjectsDeliversEverySubjectOnceCorrected(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-filtered"
	consumer := bus.ListenerDurable(machineID)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	if _, err := client.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        consumer,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubjects: bus.StreamSubjects(),
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        60 * time.Second,
	}); err != nil {
		t.Fatalf("create the filtered durable: %v", err)
	}

	handle, received := recordingHandler()
	rig := startReceiverRig(t, uri, machineID, "pod-a", handle, testReceiveTiming(), testLeaseTiming())
	rig.awaitState(t, receiveActive, 10*time.Second)
	info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil || info.Config.FilterSubject != consumerFilterAll || len(info.Config.FilterSubjects) != 0 {
		t.Fatalf("the corrected durable: %+v %v, want the filter %q alone", info, err, consumerFilterAll)
	}

	want := map[string]bool{}
	for _, pattern := range bus.StreamSubjects() {
		subject := strings.ReplaceAll(strings.ReplaceAll(pattern, ">", "regression"), "*", "x")
		if _, err := client.JS().Publish(subject, []byte(subject)); err != nil {
			t.Fatalf("publish on %s: %v", subject, err)
		}
		want[subject] = true
	}
	for range len(want) {
		message := awaitReceived(t, received, 5*time.Second, "the corrected durable")
		if !want[message.data] {
			t.Fatalf("the corrected durable delivered %q twice or unpublished", message.data)
		}
		delete(want, message.data)
	}
}

// A standby's self-health probe has no durable to check: it stays healthy, and the monitor keeps
// the standby up however long it waits, well past the three intervals after which a terminal
// failure would end it. /healthz answers it 200 healthy, as a standby naming the active pod.
func TestAStandbyOutlivesThreeSelfHealthIntervals(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-standby"
	handle, _ := recordingHandler()
	startReceiverRig(t, uri, machineID, "pod-a", handle, testReceiveTiming(), testLeaseTiming()).awaitState(t, receiveActive, 10*time.Second)
	standby := startReceiverRig(t, uri, machineID, "pod-b", handle, testReceiveTiming(), testLeaseTiming())
	waitFor(t, 5*time.Second, "the standby to read pod-a's lease", func() bool {
		return standby.lease.Status().ActiveInstance == "pod-a"
	})

	registry, err := store.Open(standby.client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open interest registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(standby.client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	caches := listenerCaches(registry, sessions, nil)
	logs := &cmdtest.LockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	var terminated atomic.Bool
	done := make(chan struct{})
	const interval = 50 * time.Millisecond
	probe := func() error { return standby.receiver.probe(ctx) }
	go func() {
		defer close(done)
		runSelfHealthMonitor(ctx, logging.NewWithWriter("test", logs),
			func() error { return checkSelfHealth(caches, probe) },
			func(err error) bool { return isUnrecoverableSelfHealthFailure(err, standby.client, caches) },
			standby.client.Connected,
			func() error {
				return rebuildListenerDependencies(ctx, standby.client, caches, probe, standby.receiver.resubscribe)
			},
			func() { terminated.Store(true) },
			interval, 3)
	}()
	time.Sleep(15 * interval)
	cancel()
	<-done
	if terminated.Load() {
		t.Fatal("the self-health monitor terminated a standby")
	}
	if strings.Contains(logs.String(), "self-health probe failed") {
		t.Fatalf("a standby failed its self-health probe:\n%s", logs.String())
	}
	if state := standby.receiver.report().State; state != receiveStandby {
		t.Fatalf("the standby's receive loop is %s", state)
	}

	subscribeRoleLane(t, standby.client)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: standby.client, registry: registry, sessions: sessions, caches: caches, receiver: standby.receiver})
	code, body := serveHealthz(&state)
	if code != http.StatusOK || body["status"] != "healthy" || body["role"] != "standby" || body["active_instance"] != "pod-a" {
		t.Fatalf("the standby's /healthz = %d %v, want 200 healthy, role standby, active_instance pod-a", code, body)
	}
	if _, ok := body["num_pending"]; ok {
		t.Fatalf("the standby's /healthz reports the durable's lag: %v", body)
	}
}

// The holder whose connection the bus replaces (nats.go gave up on it) subscribes again on the new
// connection, and keeps delivering.
func TestTheHolderReceivesOnAConnectionTheBusReplaced(t *testing.T) {
	uri := testnats.URL(t)
	handle, received := recordingHandler()
	rig := startReceiverRig(t, uri, "box-replaced", "pod-a", handle, testReceiveTiming(), testLeaseTiming())
	rig.awaitState(t, receiveActive, 10*time.Second)
	closed := rig.client.Conn
	closed.Close()
	waitFor(t, 30*time.Second, "the subscription to move onto the replacement connection", func() bool {
		report := rig.receiver.report()
		rig.receiver.mu.Lock()
		moved := rig.receiver.conn != closed
		rig.receiver.mu.Unlock()
		return moved && report.State == receiveActive && report.SubscriptionValid
	})
	publisher := testnats.Connect(t, uri)
	t.Cleanup(publisher.Close)
	js, err := publisher.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	if _, err := js.Publish(receiveTestSubject, []byte("after the replacement")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if message := awaitReceived(t, received, 10*time.Second, "the holder"); message.data != "after the replacement" {
		t.Fatalf("the holder received %q", message.data)
	}
}

// noLeaseConfig is a NATS server whose one nkey user may publish to anything but the listener
// state bucket: the grant of a hub that has not yet granted the bucket to its listeners.
func noLeaseConfig(user string) string {
	return bucketDeniedConfig(user, bus.ListenerStateBucket)
}

// A listener whose NATS user lacks the grant for the listener state bucket does not exit: it logs
// one ERROR naming the bucket and the grant, runs without a lease as the machine's one deliverer,
// delivers, and says in /healthz that the lease is unavailable.
func TestAListenerWithoutTheLeaseGrantDeliversWithoutALease(t *testing.T) {
	seed, public := testnats.User(t)
	_, uri := testnats.StartConfig(t, noLeaseConfig(public), []string{"-js"}, false)
	held := cmdtest.HoldPort(t)
	listener := &listenerProcess{port: held.Port}
	cmd := exec.Command(cmdtest.Build(t, "envoy-listener"))
	cmd.Env = []string{
		"PORT=" + strconv.Itoa(listener.port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=box-ungranted",
		"NATS_URLS=" + uri,
		"NATS_NKEY_SEED=" + seed,
		"ENVOY_API_TOKEN=" + listenerTestToken,
		"ENVOY_HOST_BRIDGE=127.0.0.1",
	}
	held.Pass(cmd, "ENVOY_LISTEN_FD")
	listener.Process = cmdtest.Start(t, "the listener box-ungranted", cmd)
	listener.waitHealthy(t)

	unavailable := func(record map[string]any) bool { return record["level"] == "ERROR" }
	if got := countRecords(listener, "listener lease unavailable: the NATS user lacks the grant for KV bucket envoy_listener; delivering without a lease", unavailable); got != 1 {
		t.Fatalf("the listener logged the missing grant %d times at ERROR, want once:\n%s", got, listener.Output.String())
	}
	status, answer := callListener(t, listener.port, http.MethodGet, "/healthz", "")
	var health map[string]any
	_ = json.Unmarshal([]byte(answer), &health)
	if status != http.StatusOK || health["role"] != "active" || health["lease"] != listenerLeaseUnavailable {
		t.Fatalf("/healthz without the grant = %d %v, want 200, role active, lease %q", status, health, listenerLeaseUnavailable)
	}

	topic := "notifications.github.acme.ungranted.pr.1"
	serving := startServingPort(t, "ses_ungranted", http.StatusNoContent)
	registerServingSession(t, listener, serving, "127.0.0.1", topic)
	publisher, err := bus.Connect([]string{uri}, bus.WithEnvironment(func(name string) (string, bool) {
		if name == "NATS_NKEY_SEED" {
			return seed, true
		}
		return "", false
	}))
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)
	item := newEnvelope("github", topic, "pull request 1 was opened")
	publish(t, publisher, item)
	requirePromptCarries(t, serving.awaitPrompt(t, 10*time.Second), item)
	if listener.Cmd.ProcessState != nil {
		t.Fatalf("the listener exited:\n%s", listener.Output.String())
	}
}
