package bus_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// reconnectWaitFixture is a client that reaches NATS through a proxy the test can cut, a subscriber
// on topic connected to NATS directly, and the envelope to publish there. onTick is what the
// client's reconnect wait runs after each of its ticks (bus.WithReconnectWaitTick); nil runs
// nothing.
type reconnectWaitFixture struct {
	proxy    *relayProxy
	client   *bus.Client
	received *nats.Subscription
	envelope contracts.Envelope
	onTick   atomic.Pointer[func()]
}

func newReconnectWaitFixture(t *testing.T, name string) *reconnectWaitFixture {
	t.Helper()
	uri := testnats.URL(t)
	f := &reconnectWaitFixture{proxy: startRelayProxy(t, uri)}
	client, err := bus.ConnectOwningStream([]string{f.proxy.url()}, bus.WithReconnectWaitTick(func() {
		if tick := f.onTick.Load(); tick != nil {
			(*tick)()
		}
	}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	f.client = client
	topic := contracts.RoleTopicPrefix + name
	subscriber := testnats.Connect(t, uri)
	t.Cleanup(subscriber.Close)
	f.received, err = subscriber.SubscribeSync(topic)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	f.envelope = contracts.Envelope{
		EventID:        "evt-" + name,
		Source:         "agent",
		SourceEventID:  "source-" + name,
		Topic:          topic,
		DedupeKey:      "publish." + name,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "published for a caller that left during the reconnect",
		TraceID:        "trace-" + name,
	}
	return f
}

// startPublishWhileReconnecting cuts the link so the client is reconnecting, sets onTick as what
// the reconnect wait runs after each tick, and starts the publish under ctx, returning where its
// answer arrives. onTick runs on the publish's goroutine, so it must not fail the test itself.
func (f *reconnectWaitFixture) startPublishWhileReconnecting(t *testing.T, ctx context.Context, onTick func()) <-chan error {
	t.Helper()
	f.proxy.refuse.Store(true)
	f.proxy.drop()
	waitFor(t, 5*time.Second, "the client to start reconnecting", f.client.Conn.IsReconnecting)
	f.onTick.Store(&onTick)
	answer := make(chan error, 1)
	go func() { answer <- f.client.PublishWithin(ctx, f.envelope) }()
	return answer
}

// awaitAnswer returns the publish's answer and clears onTick, failing the test if the publish has
// not answered within 15 s.
func (f *reconnectWaitFixture) awaitAnswer(t *testing.T, answer <-chan error) error {
	t.Helper()
	defer f.onTick.Store(nil)
	select {
	case err := <-answer:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("the publish never returned")
		return nil
	}
}

// requireNothingReceived fails the test if the subscriber receives anything from the publisher.
// The publisher's connection is flushed first, so a message it sent has reached the server, and
// been handed on, before the subscriber is read.
func (f *reconnectWaitFixture) requireNothingReceived(t *testing.T, what string, publishErr error) {
	t.Helper()
	if err := f.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the publisher's connection: %v", err)
	}
	if message, err := f.received.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("the subscriber received %s from %s (publish answered %v)", string(message.Data), what, publishErr)
	}
}

// A core publish whose context is cancelled while it waits for NATS to reconnect, with its
// deadline still ahead, sends nothing even when the reconnect lands right after the cancel. What
// answers it here is ensureConnWithContext's wait, whose select consults the context between ticks
// and so returns the cancellation while the connection is still down; publishConfirmed's read of
// the context immediately before the publish is the backstop, pinned on its own by
// TestAPublishCancelledAsTheReconnectCompletesSendsNothing.
//
// The cancel lands only once the publish is inside the wait: the wait's first tick closes waiting,
// and the test cancels once waiting is closed, then lets the link forward again so the reconnect
// follows.
//
// The control is the same publish under a live context, blocked in the same wait and released by
// the same reconnect: it must be received, so the cancelled case's silence is the cancel's doing
// and not a publish that never reached the wait.
func TestAPublishCancelledWhileWaitingForAReconnectSendsNothing(t *testing.T) {
	f := newReconnectWaitFixture(t, "cancel-while-reconnecting")

	// publishDuringReconnect starts the publish under ctx while the client is reconnecting, calls
	// release once the publish is inside the reconnect wait, then lets the link forward again and
	// returns the publish's answer once the client has reconnected.
	publishDuringReconnect := func(ctx context.Context, release func()) error {
		t.Helper()
		waiting := make(chan struct{})
		var once sync.Once
		answer := f.startPublishWhileReconnecting(t, ctx, func() { once.Do(func() { close(waiting) }) })
		select {
		case <-waiting:
		case err := <-answer:
			t.Fatalf("the publish answered %v without entering the reconnect wait", err)
		case <-time.After(5 * time.Second):
			t.Fatal("the publish never entered the reconnect wait")
		}
		release()
		f.proxy.refuse.Store(false)
		err := f.awaitAnswer(t, answer)
		waitFor(t, 10*time.Second, "the client to reconnect", f.client.Conn.IsConnected)
		return err
	}

	// The deadline is a minute out, so publishBound's 5 s is the one publishCoreTo keeps: still
	// ahead when the cancel lands one tick into the wait and when the reconnect follows about a
	// second later.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	publishErr := publishDuringReconnect(ctx, cancel)
	f.requireNothingReceived(t, "a publish cancelled during the reconnect wait", publishErr)
	if !errors.Is(publishErr, context.Canceled) {
		t.Fatalf("publish cancelled during the reconnect wait = %v, want an error naming %v", publishErr, context.Canceled)
	}

	live, cancelLive := context.WithTimeout(context.Background(), time.Minute)
	defer cancelLive()
	if err := publishDuringReconnect(live, func() {}); err != nil {
		t.Fatalf("publish under a live context released by the reconnect: %v", err)
	}
	if _, err := f.received.NextMsg(5 * time.Second); err != nil {
		t.Fatalf("the subscriber received nothing from the live publish released by the reconnect: %v", err)
	}
}

// A core publish whose context is cancelled at the moment its wait for a reconnect ends, after the
// wait's last look at the context and with the connection already back, sends nothing.
// ensureConnWithContext then returns nil, since a usable connection is all it checks once its wait
// has ended; publishConfirmed's read of the context immediately before the publish is the only
// thing that can refuse it. Without that read the message goes out to a caller that had been told
// nothing and had gone.
//
// The wait's first tick is where it happens: the tick lets the link forward again, waits for the
// client to reconnect, and then cancels, so the wait reads a connected connection next and returns.
// The control is the same publish with the same tick and no cancel: it must be received, so the
// cancelled case's silence is the cancel's doing and not a publish that never got past the wait.
func TestAPublishCancelledAsTheReconnectCompletesSendsNothing(t *testing.T) {
	f := newReconnectWaitFixture(t, "cancel-as-reconnected")

	// publishAsTheReconnectCompletes publishes under ctx while the client is reconnecting; the
	// wait's first tick reconnects the client and then calls release. It fails the test unless the
	// tick ran and saw the client reconnected, since a publish that answered without that is not the
	// case this test is about.
	publishAsTheReconnectCompletes := func(ctx context.Context, release func()) error {
		t.Helper()
		var once sync.Once
		var reconnected atomic.Bool
		ticked := make(chan struct{})
		answer := f.startPublishWhileReconnecting(t, ctx, func() {
			once.Do(func() {
				defer close(ticked)
				f.proxy.refuse.Store(false)
				for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
					if f.client.Conn.IsConnected() {
						reconnected.Store(true)
						break
					}
				}
				release()
			})
		})
		err := f.awaitAnswer(t, answer)
		select {
		case <-ticked:
		default:
			t.Fatalf("the publish answered %v without entering the reconnect wait", err)
		}
		if !reconnected.Load() {
			t.Fatalf("the client did not reconnect within the wait's tick (publish answered %v)", err)
		}
		return err
	}

	// The deadline is a minute out, so publishBound's 5 s is the one publishCoreTo keeps, and it is
	// still ahead when the reconnect completes about a second into the wait.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	publishErr := publishAsTheReconnectCompletes(ctx, cancel)
	f.requireNothingReceived(t, "a publish cancelled as the reconnect completed", publishErr)
	if !errors.Is(publishErr, context.Canceled) {
		t.Fatalf("publish cancelled as the reconnect completed = %v, want an error naming %v", publishErr, context.Canceled)
	}

	live, cancelLive := context.WithTimeout(context.Background(), time.Minute)
	defer cancelLive()
	if err := publishAsTheReconnectCompletes(live, func() {}); err != nil {
		t.Fatalf("publish under a live context as the reconnect completed: %v", err)
	}
	if _, err := f.received.NextMsg(5 * time.Second); err != nil {
		t.Fatalf("the subscriber received nothing from the live publish as the reconnect completed: %v", err)
	}
}
