package bus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A core publish whose context is cancelled while it waits for NATS to reconnect, with its
// deadline still ahead, sends nothing even when the reconnect lands right after the cancel. The
// two pre-emission checks cannot see it: publishCoreTo reads the context before that wait, and
// publishConfirmed is handed only the deadline. What answers it is ensureConnWithContext itself,
// whose wait selects on the context and so returns the cancellation the moment it happens, while
// the connection is still down; this test is what keeps that so, since a wait that stopped
// consulting the context would return nil once the reconnect landed, and the message would go out
// to a caller that had been told nothing and had gone.
//
// The control is the same publish under a live context, blocked in the same wait and released by
// the same reconnect: it must be received, so the cancelled case's silence is the cancel's doing
// and not a publish that never reached the wait.
func TestAPublishCancelledWhileWaitingForAReconnectSendsNothing(t *testing.T) {
	uri := testnats.URL(t)
	proxy := startRelayProxy(t, uri)
	client, err := bus.ConnectOwningStream([]string{proxy.url()})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	topic := contracts.RoleTopicPrefix + "cancel-while-reconnecting"
	subscriber := testnats.Connect(t, uri)
	t.Cleanup(subscriber.Close)
	received, err := subscriber.SubscribeSync(topic)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	envelope := contracts.Envelope{
		EventID:        "evt-cancel-while-reconnecting",
		Source:         "agent",
		SourceEventID:  "source-cancel-while-reconnecting",
		Topic:          topic,
		DedupeKey:      "publish.cancel-while-reconnecting",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "published for a caller that left during the reconnect",
		TraceID:        "trace-cancel-while-reconnecting",
	}

	// publishDuringReconnect cuts the link so the client is reconnecting, starts the publish under
	// ctx, and once it has had time to reach the reconnect wait calls release, then lets the link
	// forward again and waits out the reconnect before it returns the publish's answer.
	publishDuringReconnect := func(ctx context.Context, release func()) error {
		t.Helper()
		proxy.refuse.Store(true)
		proxy.drop()
		waitFor(t, 5*time.Second, "the client to start reconnecting", client.Conn.IsReconnecting)
		answer := make(chan error, 1)
		go func() { answer <- client.PublishWithin(ctx, envelope) }()
		time.Sleep(300 * time.Millisecond)
		release()
		proxy.refuse.Store(false)
		waitFor(t, 10*time.Second, "the client to reconnect", client.Conn.IsConnected)
		select {
		case err := <-answer:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("the publish never returned")
			return nil
		}
	}

	// The deadline is a minute out, so publishBound's 5 s is the one publishCoreTo keeps: still
	// ahead when the cancel lands 300 ms in and when the reconnect follows about a second later.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	publishErr := publishDuringReconnect(ctx, cancel)
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the publisher's connection: %v", err)
	}
	if message, err := received.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("the subscriber received %s from a publish cancelled during the reconnect wait (publish answered %v)", string(message.Data), publishErr)
	}
	if !errors.Is(publishErr, context.Canceled) {
		t.Fatalf("publish cancelled during the reconnect wait = %v, want an error naming %v", publishErr, context.Canceled)
	}

	live, cancelLive := context.WithTimeout(context.Background(), time.Minute)
	defer cancelLive()
	if err := publishDuringReconnect(live, func() {}); err != nil {
		t.Fatalf("publish under a live context released by the reconnect: %v", err)
	}
	if _, err := received.NextMsg(5 * time.Second); err != nil {
		t.Fatalf("the subscriber received nothing from the live publish released by the reconnect: %v", err)
	}
}
