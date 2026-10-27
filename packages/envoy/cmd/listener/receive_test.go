package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
// sees each of them in well under a second; the durable's own ack wait stays the policy's.
func testReceiveTiming() receiveConfig {
	return receiveConfig{
		ackWait:         consumerAckWait,
		bindPoll:        50 * time.Millisecond,
		alarmAfter:      time.Hour,
		alarmEvery:      time.Hour,
		inProgressEvery: inProgressEvery,
		inflightGrace:   300 * time.Millisecond,
	}
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
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1), bus.WithName("receive-test "+instance))
	if err != nil {
		t.Fatalf("connect %s: %v", instance, err)
	}
	stateConn := testnats.Connect(t, uri)
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
	if err := stray.receiver.begin(info.Config.DeliverSubject); err != nil {
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

// A graceful stop unsubscribes, gives the message in hand its grace and then cancels its handling,
// NAKs every queued message and marks the lease released: a standby takes the lease at its next
// poll and receives the stopped pod's messages at once, in the order they came, with no ack wait.
func TestAGracefulStopNAKsWhatItHeldAndReleasesTheLeaseToTheStandby(t *testing.T) {
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
	}, testReceiveTiming(), testLeaseTiming())
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
		t.Fatalf("the stop took %s, want the 300 ms grace and the release", took)
	}
	if handling.Err() == nil {
		t.Fatal("the stop left the message in hand's handling running past its grace")
	}
	if status := active.lease.Status(); status.Holding {
		t.Fatal("the stopped pod still holds the lease")
	}

	for _, want := range []string{"first", "second", "third"} {
		message := awaitReceived(t, received, 5*time.Second, "the standby's "+want)
		if message.data != want || message.delivered != 2 {
			t.Fatalf("the standby received %q as delivery %d, want %q redelivered (delivery 2)", message.data, message.delivered, want)
		}
		if after := message.at.Sub(stopped); after >= consumerAckWait {
			t.Fatalf("the standby received %q %s after the stop, want it at once rather than after the ack wait", want, after)
		}
	}
	if status := standby.lease.Status(); !status.Holding || status.How != leaseTakenReleased {
		t.Fatalf("the standby's lease after the stop: %+v, want held, taken from a released record", status)
	}
	if got := activeHandled.Load(); got != 1 {
		t.Fatalf("the stopped pod handled %d messages, want only the first", got)
	}
}

// A pod that takes the lease over an expired record nobody released waits one ack wait before it
// subscribes, so what the dead holder had received comes back before newer messages; the dead
// holder's subscription, still counted by the server, does not stop it subscribing then. A pod that
// takes a released record subscribes at once.
func TestAnExpiredTakeOverWaitsOneAckWaitBeforeReceiving(t *testing.T) {
	uri := testnats.URL(t)
	timing := testReceiveTiming()
	timing.ackWait = 2 * time.Second

	const expired = "box-expired"
	putLeaseRecord(t, uri, expired, `{"holder":"pod-dead","boot":"0123456789abcdef"}`)
	setup := testnats.Connect(t, uri)
	t.Cleanup(setup.Close)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	info, err := ensureListenerDurable(context.Background(), client, bus.ListenerDurable(expired))
	if err != nil {
		t.Fatalf("create the durable: %v", err)
	}
	// The dead holder's subscription: a node lost without a FIN keeps its interest on the server.
	if _, err := setup.Subscribe(info.Config.DeliverSubject, func(*natsgo.Msg) {}); err != nil {
		t.Fatalf("subscribe as the dead holder: %v", err)
	}
	waitFor(t, 5*time.Second, "the durable to read push-bound", func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, bus.ListenerDurable(expired))
		return err == nil && info.PushBound
	})

	handle, received := recordingHandler()
	rig := startReceiverRig(t, uri, expired, "pod-b", handle, timing, testLeaseTiming())
	waitFor(t, 10*time.Second, "pod-b to take the expired lease", rig.lease.Holding)
	taken := time.Now()
	if status := rig.lease.Status(); status.How != leaseTakenExpired {
		t.Fatalf("pod-b took the lease from a %s record, want expired", status.How)
	}
	if state := rig.receiver.report().State; state != receiveAwaitingAckWait && state != receivePreparing {
		t.Fatalf("right after the take-over the receive loop is %s, want it waiting one ack wait", state)
	}
	receiving := rig.awaitState(t, receiveActive, 10*time.Second)
	if waited := receiving.Sub(taken); waited < timing.ackWait-100*time.Millisecond {
		t.Fatalf("pod-b subscribed %s after taking an expired lease, want one ack wait (%s)", waited, timing.ackWait)
	}
	publishDurable(t, client, "after the take-over")
	if message := awaitReceived(t, received, 5*time.Second, "pod-b"); message.data != "after the take-over" {
		t.Fatalf("pod-b received %q", message.data)
	}

	const released = "box-released"
	putLeaseRecord(t, uri, released, `{"holder":"pod-gone","boot":"0123456789abcdef","released":true}`)
	control := startReceiverRig(t, uri, released, "pod-c", handle, timing, testLeaseTiming())
	waitFor(t, 10*time.Second, "pod-c to take the released lease", control.lease.Holding)
	taken = time.Now()
	if waited := control.awaitState(t, receiveActive, 10*time.Second).Sub(taken); waited >= timing.ackWait {
		t.Fatalf("pod-c subscribed %s after taking a released lease, want at once", waited)
	}
}

// durableAlarms counts the ERROR records of the take-over alarm in a rig's log.
func durableAlarms(logs *cmdtest.LockedBuffer) int {
	count := 0
	for line := range strings.SplitSeq(logs.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == durableTakeoverAlarmLine && record["level"] == "ERROR" {
			count++
		}
	}
	return count
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

	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: standby.client, registry: registry, sessions: sessions, caches: caches, receiver: standby.receiver})
	recorder := httptest.NewRecorder()
	healthzHandler(&state).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	if recorder.Code != http.StatusOK || body["status"] != "healthy" || body["role"] != "standby" || body["active_instance"] != "pod-a" {
		t.Fatalf("the standby's /healthz = %d %v, want 200 healthy, role standby, active_instance pod-a", recorder.Code, body)
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
	return "jetstream {}\nauthorization {\n  users = [ { nkey: " + strconv.Quote(user) + ", permissions: { publish: { allow: [\">\"], deny: [" +
		strconv.Quote("$JS.API.STREAM.*."+"KV_"+bus.ListenerStateBucket) + ", " +
		strconv.Quote("$JS.API.CONSUMER.*."+"KV_"+bus.ListenerStateBucket+".>") + ", " +
		strconv.Quote("$KV."+bus.ListenerStateBucket+".>") + "] } } } ]\n}\n"
}

// A listener whose NATS user lacks the grant for the listener state bucket does not exit: it logs
// one ERROR naming the bucket and the grant, runs without a lease as the machine's one deliverer,
// delivers, and says in /healthz that the lease is unavailable.
func TestAListenerWithoutTheLeaseGrantDeliversWithoutALease(t *testing.T) {
	seed, public := testnats.User(t)
	_, uri := testnats.StartConfig(t, noLeaseConfig(public), []string{"-js"}, false)
	listener := &listenerProcess{port: cmdtest.FreeTCPPort(t)}
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
