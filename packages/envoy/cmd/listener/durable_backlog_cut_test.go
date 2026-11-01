package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// backlogCutMessages is the backlog the durable pushes at the subscriber that vanishes. It is
	// more than consumerMaxAckPending, so the push stops at the ack-pending ceiling and the rest
	// of the backlog waits for the next subscriber's acks: every message must reach a subscriber
	// either way.
	backlogCutMessages = 300
	// backlogCutPayload makes the push long enough for the cut to land inside it: the server has
	// consumerMaxAckPending of these to write before it stops for want of acks, and the cut costs
	// a round trip. The row checks that it did land inside the push.
	backlogCutPayload = 256 * 1024
	// backlogCutIdle is how long the row waits with no message arriving before it reads the rest
	// as lost. The server pushes what the vanished subscriber never acked one ack wait
	// (consumerAckWait) after that push and the rest as the next subscriber acks, and that
	// subscriber acks at once, so a gap this long with messages outstanding is the server holding
	// nothing for them.
	backlogCutIdle = 30 * time.Second
	// backlogCutWait caps the whole wait, for a machine on which every part of this is slow: the
	// row ends as soon as the backlog is in, so it costs this only when something is wrong.
	backlogCutWait = 3 * time.Minute
)

// Every message of a backlog the durable pushes reaches a subscriber exactly once, even when the
// only subscriber of the machine's durable vanishes mid-push: the messages already pushed come
// back one ack wait later, and the rest follow as the next subscriber acks them.
//
// The listener's consumer policy makes that a server-version requirement. A push consumer's
// delivery loop reads the end of the deliver subject's interest from a channel, which it does not
// read while it has messages to push and room under MaxAckPending, so the messages it pushes in
// between reach a subject nothing holds. nats-server reports each of those writes undelivered
// (`server/stream.go` internalLoop, `!didDeliver` -> `consumer.didNotDeliver`) and takes one off
// that message's delivery count. Through v2.14.x the subtraction underflows a first delivery,
// which has no count yet: v2.10.29's decDeliveryCount (`server/consumer.go:3533-3538`) writes
// `o.rdc[sseq] -= 1` into a map of unsigned counts, leaving 2^64-1, so with MaxDeliver set
// (consumerMaxDeliver) hasMaxDeliveries (`:1738-1765`) drops the message at the ack wait with no
// redelivery and no advisory, and nothing brings it back. v2.15.0 fixed that subtraction, and
// v2.15.2's decDeliveryCount (`server/consumer.go:4866-4883`) leaves a first delivery's count
// alone, so the message comes back one ack wait later like every other outstanding message. That
// is why the listener requires nats-server v2.15.0 or later (`cmd/listener/README.md`, "The
// delivery lease"); on anything older this row fails with messages missing.
func TestEveryBackloggedMessagePushedAtAVanishingSubscriberComesBackExactlyOnce(t *testing.T) {
	uri := testnats.URL(t)
	const machineID = "box-backlog-cut"
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	consumer := bus.ListenerDurable(machineID)
	config := natsgo.ConsumerConfig{Durable: consumer, DeliverSubject: natsgo.NewInbox()}
	applyListenerConsumerPolicy(&config)
	if _, err := client.JS().AddConsumer(bus.Stream, &config); err != nil {
		t.Fatalf("create the durable: %v", err)
	}

	// The durable is created before the backlog and has no subscriber, so the server pushes
	// nothing until one subscribes (an inactive push consumer gathers no messages): the whole
	// backlog is queued when the push starts.
	filler := strings.Repeat("x", backlogCutPayload)
	for id := range backlogCutMessages {
		if _, err := client.JS().Publish(receiveTestSubject, []byte(strconv.Itoa(id)+" "+filler)); err != nil {
			t.Fatalf("publish message %d of the backlog: %v", id, err)
		}
	}

	// The only subscriber: it acks nothing, so the push stops at the ack-pending ceiling, and it
	// reads, so the server is not the one to end the connection.
	vanishing := testnats.Connect(t, uri)
	t.Cleanup(vanishing.Close)
	if _, err := vanishing.Subscribe(config.DeliverSubject, func(*natsgo.Msg) {}); err != nil {
		t.Fatalf("subscribe the vanishing subscriber: %v", err)
	}
	if err := vanishing.Flush(); err != nil {
		t.Fatalf("flush the vanishing subscriber's subscription: %v", err)
	}
	pushing := backlogCutPushing(t, client, consumer)
	// The cut: the connection goes while the server is still pushing the backlog at it, so what
	// the server pushes next reaches a deliver subject nothing holds.
	vanishing.Close()
	if pushing >= consumerMaxAckPending {
		t.Fatalf("the durable had pushed %d messages, its whole ack-pending ceiling, before the cut landed: the row needs the server pushing as its only subscriber goes", pushing)
	}
	// The next subscriber comes only once the server has read the cut, so every message it gets is
	// one the server pushes with the deliver subject bound again, never one of the push it was
	// making as the subscriber went.
	waitFor(t, 30*time.Second, "the durable to lose its only subscriber", func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
		return err == nil && !info.PushBound
	})

	var mu sync.Mutex
	seen := map[int]int{}
	last := time.Now()
	var readErr error
	complete := make(chan struct{})
	var completed sync.Once
	next := testnats.Connect(t, uri)
	t.Cleanup(next.Close)
	if _, err := next.Subscribe(config.DeliverSubject, func(msg *natsgo.Msg) {
		id, idErr := strconv.Atoi(strings.SplitN(string(msg.Data), " ", 2)[0])
		ack := msg.Ack()
		mu.Lock()
		defer mu.Unlock()
		last = time.Now()
		if idErr != nil || ack != nil {
			if readErr == nil {
				readErr = fmt.Errorf("read the id: %v; ack: %v", idErr, ack)
			}
			return
		}
		seen[id]++
		if len(seen) == backlogCutMessages {
			completed.Do(func() { close(complete) })
		}
	}); err != nil {
		t.Fatalf("subscribe the next subscriber: %v", err)
	}
	if err := next.Flush(); err != nil {
		t.Fatalf("flush the next subscriber's subscription: %v", err)
	}
	// The backlog is in, or nothing has arrived for backlogCutIdle, or the cap is spent: a run on
	// a loaded machine takes its time and still ends as soon as the last message lands.
	waited := time.Now()
	for deadline := waited.Add(backlogCutWait); ; {
		select {
		case <-complete:
		case <-time.After(250 * time.Millisecond):
			mu.Lock()
			idle, got := time.Since(last), len(seen)
			mu.Unlock()
			if got < backlogCutMessages && idle < backlogCutIdle && time.Now().Before(deadline) {
				continue
			}
		}
		break
	}

	mu.Lock()
	defer mu.Unlock()
	if readErr != nil {
		t.Fatalf("the next subscriber could not read or ack a message of the backlog: %v", readErr)
	}
	var missing, twice []int
	for id := range backlogCutMessages {
		switch count := seen[id]; {
		case count == 0:
			missing = append(missing, id)
		case count > 1:
			twice = append(twice, id)
		}
	}
	if len(missing) > 0 || len(twice) > 0 {
		t.Fatalf("of %d messages, %d of them pushed when the cut landed, the next subscriber got %d in %s, the last of them %s ago: %d are missing (%v) and %d arrived more than once (%v). A missing message is the server dropping a first delivery it pushed at no subscriber: nats-server before v2.15.0 underflows that message's delivery count, and MaxDeliver then drops it at the ack wait",
			backlogCutMessages, pushing, len(seen), time.Since(waited).Round(time.Second), time.Since(last).Round(time.Second), len(missing), backlogCutIDs(missing), len(twice), backlogCutIDs(twice))
	}
}

// backlogCutPushing reads the durable until it has pushed a message and reports how many it has
// outstanding then, which is how far the push had got when the caller cut the connection. It
// reads as fast as the server answers, since the whole push is over in well under a second.
func backlogCutPushing(t *testing.T, client *bus.Client, consumer string) int {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; {
		info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
		if err == nil && info.NumAckPending > 0 {
			return info.NumAckPending
		}
		if time.Now().After(deadline) {
			t.Fatalf("the durable pushed none of its %d messages at its only subscriber: %v", backlogCutMessages, err)
			return 0
		}
	}
}

// backlogCutIDs is the first ten ids of list, so a failure naming hundreds of missing messages
// stays one line, with the count beside it.
func backlogCutIDs(list []int) []int {
	if len(list) > 10 {
		return list[:10]
	}
	return list
}
