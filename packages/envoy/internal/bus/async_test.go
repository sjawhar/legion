package bus_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	orderedStreamTopic = "notifications.dispatch.issue.T-1.message.created"
	orderedRoleTopic   = "notifications.role.ordered"
)

// numberedEnvelope is the index'th of a run of dispatch envelopes on topic, carrying index as its
// source event id.
func numberedEnvelope(topic string, index int) contracts.Envelope {
	return contracts.Envelope{
		EventID:       fmt.Sprintf("dispatch-%d", index),
		Source:        "dispatch",
		SourceEventID: fmt.Sprint(index),
		Topic:         topic,
		DedupeKey:     fmt.Sprintf("dispatch-%d", index),
		IssuedAt:      contracts.NowMillis(),
	}
}

func sourceEventID(t *testing.T, data []byte) string {
	t.Helper()
	var envelope contracts.Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode an envelope: %v", err)
	}
	return envelope.SourceEventID
}

// PublishAsync sends each envelope without waiting for the answer to the one before, and NATS
// takes them in the order they were sent: the stream stores a run sent back to back in send order,
// a core subscriber of a role lane receives its run in send order, and every one is answered
// accepted.
func TestPublishAsyncKeepsSendOrder(t *testing.T) {
	const count = 500
	_, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	reader, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect the reader: %v", err)
	}
	t.Cleanup(reader.Close)
	role, err := reader.SubscribeSync(orderedRoleTopic)
	if err != nil {
		t.Fatalf("subscribe to the role lane: %v", err)
	}
	if err := reader.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	window := client.Window()
	answers := make([]<-chan error, 0, 2*count)
	for index := range count {
		answers = append(answers,
			window.PublishAsync(numberedEnvelope(orderedStreamTopic, index)),
			window.PublishAsync(numberedEnvelope(orderedRoleTopic, index)))
	}
	for index, answer := range answers {
		select {
		case err := <-answer:
			if err != nil {
				t.Fatalf("publish %d was answered %v, want accepted", index, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("publish %d was never answered", index)
		}
	}

	js, err := reader.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if info.State.Msgs != count {
		t.Fatalf("the stream holds %d messages, want %d", info.State.Msgs, count)
	}
	for index := range count {
		message, err := js.GetMsg(bus.Stream, info.State.FirstSeq+uint64(index))
		if err != nil {
			t.Fatalf("read stream message %d: %v", index, err)
		}
		if got := sourceEventID(t, message.Data); got != fmt.Sprint(index) {
			t.Fatalf("stream message %d carries envelope %s, want %d", index, got, index)
		}
	}
	for index := range count {
		message, err := role.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("role lane message %d: %v", index, err)
		}
		if got := sourceEventID(t, message.Data); got != fmt.Sprint(index) {
			t.Fatalf("role lane message %d carries envelope %s, want %d", index, got, index)
		}
	}
}

// A window whose connection is lost sends nothing more, and says so before it returns: its answer
// is in the channel at once, ErrUnavailable, which a caller tells apart from a failure of the
// envelope itself.
func TestAWindowWhoseConnectionIsLostAnswersUnavailableBeforeSending(t *testing.T) {
	server, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	window := client.Window()
	if err := <-window.PublishAsync(numberedEnvelope(orderedStreamTopic, 0)); err != nil {
		t.Fatalf("first publish answered %v", err)
	}
	server.Stop(t)
	deadline := time.Now().Add(5 * time.Second)
	for client.Connected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	for index, topic := range []string{orderedStreamTopic, orderedRoleTopic} {
		answer := window.PublishAsync(numberedEnvelope(topic, index+1))
		select {
		case err := <-answer:
			if !errors.Is(err, bus.ErrUnavailable) {
				t.Fatalf("publish to %s on a lost connection answered %v, want bus.ErrUnavailable", topic, err)
			}
		default:
			t.Fatalf("publish to %s on a lost connection returned with no answer, as if it had sent something", topic)
		}
	}
}
