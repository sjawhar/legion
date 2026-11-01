package bus_test

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The listener's consumer policy, which cmd/listener/durable.go stamps onto the machine's durable
// (applyListenerConsumerPolicy, consumerAckWait, consumerMaxAckPending, consumerMaxDeliver). That
// package is a main package, so the three settings this test's server behaviour depends on are
// repeated here rather than imported; MaxDeliver is the one that turns a lost delivery count into
// a lost message.
const (
	backlogAckWait       = 6 * time.Second
	backlogMaxAckPending = 256
	backlogMaxDeliver    = 20
)

// backlogMessages is the backlog the cut happens under. It is several hundred because the server
// pushes up to backlogMaxAckPending messages into the stream's send queue in one burst as interest
// appears: a cut under a backlog that deep leaves messages in that queue when the subscription
// goes, which is what a server carrying the delivery-count underflow loses.
const backlogMessages = 600

// A backlog outlives the cut of the durable's only subscriber: every message reaches the
// subscriber that binds next, within a few ack waits, and none is dropped.
//
// nats-server v2.10.29 through v2.14.0 lose such a message for good. The stream's send loop can
// still hand a message to a deliver subject whose last subscriber has just gone; the send reports
// nothing delivered, and didNotDeliver then calls decDeliveryCount, which subtracts one from an
// entry a first delivery never made (`o.rdc[sseq] -= 1`, server/consumer.go:3533-3539 at
// v2.10.29). The count underflows to 2^64-1, so at the ack wait hasMaxDeliveries (`:1738`) reads
// it as past the durable's MaxDeliver, drops the message from pending and redelivers it to nobody.
// It sends no advisory either, since the count is not equal to MaxDeliver. A consumer with no
// MaxDeliver set is unaffected, which is why the listener's policy is the one that loses messages.
// nats-server v2.15.0 fixed it: decDeliveryCount removes the entry rather than leaving an
// underflowed one behind (`:4866-4884` at v2.15.2, the version this module builds its in-process
// test server from), and the message is redelivered as any undelivered one is. The production hubs
// run v2.15.1.
//
// It fails on v2.10.29, where the resubscribed consumer never sees the messages the cut swallowed,
// and passes on v2.15.2.
func TestEveryMessageOfABacklogSurvivesTheCutOfItsOnlySubscriber(t *testing.T) {
	uri := testnats.URL(t)
	admin, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	js := admin.JS()

	consumer := bus.ListenerDurable("backlog-cut")
	if _, err := js.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        consumer,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  ">",
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        backlogAckWait,
		MaxAckPending:  backlogMaxAckPending,
		MaxDeliver:     backlogMaxDeliver,
	}); err != nil {
		t.Fatalf("create the durable: %v", err)
	}

	subject := contracts.AgentSubject("ses_backlog_cut")
	for i := range backlogMessages {
		if _, err := js.PublishAsync(subject, []byte(strconv.Itoa(i))); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(30 * time.Second):
		t.Fatalf("the backlog of %d messages was not stored", backlogMessages)
	}

	// The subscriber the cut takes. It acknowledges nothing, so every message the server hands it
	// stays outstanding and is redelivered to the next subscriber at the ack wait: whatever the
	// next subscriber never sees was lost by the server, not acknowledged here. The cut is a hard
	// connection close at the first message, while the server is still pushing the burst that
	// interest in the deliver subject started.
	cutConn, err := natsgo.Connect(uri)
	if err != nil {
		t.Fatalf("connect the subscriber the cut takes: %v", err)
	}
	defer cutConn.Close()
	cutJS, err := cutConn.JetStream()
	if err != nil {
		t.Fatalf("jetstream on the subscriber the cut takes: %v", err)
	}
	pushing := make(chan struct{})
	var pushed sync.Once
	if _, err := cutJS.Subscribe("", func(*natsgo.Msg) {
		pushed.Do(func() { close(pushing) })
	}, natsgo.Bind(bus.Stream, consumer), natsgo.ManualAck()); err != nil {
		t.Fatalf("bind the durable: %v", err)
	}
	select {
	case <-pushing:
	case <-time.After(30 * time.Second):
		t.Fatalf("the durable delivered nothing of the backlog")
	}
	cutConn.Close()

	waitFor(t, 30*time.Second, "the server to drop the cut subscriber's binding", func() bool {
		info, err := js.ConsumerInfo(bus.Stream, consumer)
		return err == nil && !info.PushBound
	})

	// The subscriber that binds next must be handed every message of the backlog, each either
	// never delivered before or redelivered at the ack wait of the delivery the cut swallowed.
	nextConn, err := natsgo.Connect(uri)
	if err != nil {
		t.Fatalf("connect the subscriber that binds next: %v", err)
	}
	defer nextConn.Close()
	nextJS, err := nextConn.JetStream()
	if err != nil {
		t.Fatalf("jetstream on the subscriber that binds next: %v", err)
	}
	var mu sync.Mutex
	received := make(map[string]bool, backlogMessages)
	all := make(chan struct{})
	var complete sync.Once
	if _, err := nextJS.Subscribe("", func(msg *natsgo.Msg) {
		mu.Lock()
		received[string(msg.Data)] = true
		done := len(received) == backlogMessages
		mu.Unlock()
		// A failed acknowledgement costs a redelivery, which this handler takes again; only a
		// message the server never delivers again fails the test.
		_ = msg.Ack()
		if done {
			complete.Do(func() { close(all) })
		}
	}, natsgo.Bind(bus.Stream, consumer), natsgo.ManualAck()); err != nil {
		t.Fatalf("bind the durable after the cut: %v", err)
	}

	// Ten ack waits: a message the cut interrupted returns one ack wait after its delivery, and
	// the rest follow as the consumer's window frees, so the whole backlog is through in two or
	// three. The margin is for a loaded runner, not for the server's own timing.
	select {
	case <-all:
	case <-time.After(10 * backlogAckWait):
		mu.Lock()
		missing := make([]string, 0, backlogMessages-len(received))
		for i := range backlogMessages {
			if body := strconv.Itoa(i); !received[body] {
				missing = append(missing, body)
			}
		}
		mu.Unlock()
		state := "unreadable"
		if info, err := js.ConsumerInfo(bus.Stream, consumer); err == nil {
			state = fmt.Sprintf("%d pending, %d unacknowledged, %d redelivered, ack floor %d",
				info.NumPending, info.NumAckPending, info.NumRedelivered, info.AckFloor.Stream)
		}
		t.Fatalf("the cut lost %d of %d messages: the durable never delivered %v again (%s)",
			len(missing), backlogMessages, missing, state)
	}
}
