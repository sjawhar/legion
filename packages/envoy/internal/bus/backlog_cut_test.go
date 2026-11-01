package bus_test

import (
	"fmt"
	"maps"
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

// backlogMessages is the backlog the cut happens under, and backlogCutAfter how many of them the
// subscriber takes before it is cut: enough that the consumer is in full flow, with hundreds
// still to hand over.
const (
	backlogMessages = 600
	backlogCutAfter = 128
)

// Every message of a backlog survives the cut of the durable's only subscriber: the subscriber
// that binds next is handed each one, within a few ack waits, and none is dropped.
//
// nats-server v2.10.29 through v2.14.0 lose such a message for good. The stream's send loop can
// still hand a message to a deliver subject whose last subscriber has just gone (stream.go's
// internal loop, which calls didNotDeliver for a send that reached nobody); didNotDeliver then
// calls decDeliveryCount, which subtracts one from an entry a first delivery never made
// (`o.rdc[sseq] -= 1`, server/consumer.go:3533-3539 at v2.10.29, :4672-4677 at v2.14.0). The
// count underflows to 2^64-1, so at the ack wait hasMaxDeliveries (`:1738` at v2.10.29) reads it
// as past the durable's MaxDeliver, drops the message from pending and redelivers it to nobody.
// It sends no advisory either, since the count is not equal to MaxDeliver. A consumer with no
// MaxDeliver set is unaffected, which is why the listener's policy is the one that loses
// messages. nats-server v2.15.0 fixed it: decDeliveryCount removes the entry rather than leaving
// an underflowed one behind (consumer.go:4865-4884 at v2.15.0, :4866-4885 at v2.15.2, the version
// this module builds its in-process test server from), and the message comes back at its ack wait
// as any undelivered one does. The production hubs run v2.15.1.
//
// The cut is timed to leave messages in that send queue, which is what makes the loss repeatable
// rather than a matter of luck. The subscriber acknowledges each message as it arrives, so the
// consumer never stops at its window and goes on queueing the backlog, and the connection is
// closed after backlogCutAfter messages, with hundreds still to come. On v2.10.29 the
// subscription is gone while the send queue still holds part of what the consumer queued, and
// every message in it is lost: 9, 29 and 57 of the 600 in three runs, each counted by the
// durable as a redelivery and none of them delivered again. On v2.15.2 none is lost.
func TestEveryMessageOfABacklogSurvivesTheCutOfItsOnlySubscriber(t *testing.T) {
	uri := testnats.URL(t)
	admin, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	js := admin.JS()

	consumer := bus.ListenerDurable("backlog-cut")
	durable, err := js.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        consumer,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  ">",
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        backlogAckWait,
		MaxAckPending:  backlogMaxAckPending,
		MaxDeliver:     backlogMaxDeliver,
	})
	if err != nil {
		t.Fatalf("create the durable: %v", err)
	}
	deliver := durable.Config.DeliverSubject

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

	// The subscriber the cut takes: a core subscription to the deliver subject, as the listener's
	// receive loop makes (durableReceiver). It acknowledges each message as it arrives, so the
	// consumer keeps sending rather than stopping at its window, and the cut lands while it is
	// still handing the backlog over.
	cutConn, err := natsgo.Connect(uri)
	if err != nil {
		t.Fatalf("connect the subscriber the cut takes: %v", err)
	}
	defer cutConn.Close()
	var mu sync.Mutex
	seen := make(map[string]bool, backlogMessages)
	enough := make(chan struct{})
	var reached sync.Once
	if _, err := cutConn.Subscribe(deliver, func(msg *natsgo.Msg) {
		mu.Lock()
		seen[string(msg.Data)] = true
		taken := len(seen)
		mu.Unlock()
		_ = msg.Ack()
		if taken >= backlogCutAfter {
			reached.Do(func() { close(enough) })
		}
	}); err != nil {
		t.Fatalf("subscribe to the deliver subject: %v", err)
	}
	select {
	case <-enough:
	case <-time.After(30 * time.Second):
		mu.Lock()
		taken := len(seen)
		mu.Unlock()
		t.Fatalf("the durable delivered %d of the backlog's %d messages, want at least %d before the cut",
			taken, backlogMessages, backlogCutAfter)
	}
	cutConn.Close()
	mu.Lock()
	got := make(map[string]bool, backlogMessages)
	maps.Copy(got, seen)
	mu.Unlock()

	waitFor(t, 30*time.Second, "the server to drop the cut subscriber's binding", func() bool {
		info, err := js.ConsumerInfo(bus.Stream, consumer)
		return err == nil && !info.PushBound
	})

	// The subscriber that binds next must be handed every message the cut subscriber did not
	// take: one never delivered, or one redelivered at the ack wait of the delivery the cut
	// interrupted. A message in neither hand was lost by the server.
	nextConn, err := natsgo.Connect(uri)
	if err != nil {
		t.Fatalf("connect the subscriber that binds next: %v", err)
	}
	defer nextConn.Close()
	var gotMu sync.Mutex
	all := make(chan struct{})
	var complete sync.Once
	if _, err := nextConn.Subscribe(deliver, func(msg *natsgo.Msg) {
		gotMu.Lock()
		got[string(msg.Data)] = true
		done := len(got) == backlogMessages
		gotMu.Unlock()
		// A failed acknowledgement costs a redelivery, which this handler takes again; only a
		// message the server never delivers again fails the test.
		_ = msg.Ack()
		if done {
			complete.Do(func() { close(all) })
		}
	}); err != nil {
		t.Fatalf("subscribe to the deliver subject after the cut: %v", err)
	}

	// Ten ack waits: a message the cut interrupted returns one ack wait after its delivery, and
	// the rest follow as the consumer's window frees, so the whole backlog is through in two or
	// three. The margin is for a loaded runner, not for the server's own timing.
	select {
	case <-all:
	case <-time.After(10 * backlogAckWait):
		gotMu.Lock()
		missing := make([]string, 0, backlogMessages-len(got))
		for i := range backlogMessages {
			if body := strconv.Itoa(i); !got[body] {
				missing = append(missing, body)
			}
		}
		gotMu.Unlock()
		state := "unreadable"
		if info, err := js.ConsumerInfo(bus.Stream, consumer); err == nil {
			state = fmt.Sprintf("%d pending, %d unacknowledged, %d redelivered, ack floor %d",
				info.NumPending, info.NumAckPending, info.NumRedelivered, info.AckFloor.Stream)
		}
		t.Fatalf("the cut lost %d of %d messages: the durable never delivered %v again (%s)",
			len(missing), backlogMessages, missing, state)
	}
}
