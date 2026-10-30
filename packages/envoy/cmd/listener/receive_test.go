package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/logging"
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
	return newReceiverRigAs(t, busURI, stateURI, "", machineID, instance, handle, timing, leaseTiming)
}

// newReceiverRigAs is newReceiverRigVia with both connections made as the nkey user whose seed is
// seed, as main makes them from NATS_NKEY_SEED (the state bucket's on a connection bus.Dial makes),
// or with the URLs' own credentials when seed is "".
func newReceiverRigAs(t *testing.T, busURI, stateURI, seed, machineID, instance string, handle durableHandler, timing receiveConfig, leaseTiming leaseConfig) *receiverRig {
	t.Helper()
	options := []bus.ConnectOption{bus.WithReplicas(1), bus.WithName("receive-test " + instance)}
	var stateConn *natsgo.Conn
	if seed == "" {
		stateConn = testnats.Connect(t, stateURI)
	} else {
		environment := func(name string) (string, bool) {
			if name == "NATS_NKEY_SEED" {
				return seed, true
			}
			return "", false
		}
		options = append(options, bus.WithEnvironment(environment))
		conn, err := bus.Dial("receive-test-lease "+instance, []string{stateURI}, environment)
		if err != nil {
			t.Fatalf("connect the lease of %s: %v", instance, err)
		}
		stateConn = conn
	}
	client, err := bus.ConnectOwningStream([]string{busURI}, options...)
	if err != nil {
		stateConn.Close()
		t.Fatalf("connect %s: %v", instance, err)
	}
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

// putLeaseRecord writes machineID's lease record as another pod would have left it, and returns
// its revision: the one a pod taking that record replaces, which a clean-hand-back mark for it is
// keyed by (putHandBackMark).
func putLeaseRecord(t *testing.T, uri, machineID, record string) uint64 {
	t.Helper()
	conn := testnats.Connect(t, uri)
	defer conn.Close()
	kv, err := openListenerState(conn, 1, listenerStateRequestWait)
	if err != nil {
		t.Fatalf("open the listener state bucket: %v", err)
	}
	rev, err := kv.Put("lease."+bus.ListenerDurable(machineID), []byte(record))
	if err != nil {
		t.Fatalf("write the lease record: %v", err)
	}
	return rev
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
	if err := stray.receiver.begin(info.Config.DeliverSubject, 0, 0); err != nil {
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
