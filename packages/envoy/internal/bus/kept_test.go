package bus_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// keptClient is a client of the package's shared NATS server that owns the notification stream,
// as every test of the kept lanes needs: the kept subjects are captured by that stream alone.
func keptClient(t *testing.T) *bus.Client {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

// storedOn is how many messages the stream holds on subject.
func storedOn(t *testing.T, client *bus.Client, subject string) uint64 {
	t.Helper()
	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("stream info for %s: %v", subject, err)
	}
	return info.State.Subjects[subject]
}

// keptEnvelope is a role message the listener could not deliver: its topic is the role topic it
// was published on, which the kept copy keeps whatever subject it is stored under.
func keptEnvelope(role, eventID string) contracts.Envelope {
	return contracts.Envelope{
		EventID:        eventID,
		Source:         "agent",
		Topic:          contracts.RoleTopicPrefix + role,
		DedupeKey:      "role." + role + "." + eventID,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "a role message nobody held",
		TraceID:        "trace-" + eventID,
	}
}

// The kept lane holds the one message that matters per subject, so the listener reads it with
// last_by_subj: nothing stored on the subject is nats.ErrMsgNotFound, and a subject written three
// times answers the newest of the three, with its own sequence and subject.
func TestLastStoredMessageAnswersTheNewestMessageOnTheSubject(t *testing.T) {
	client := keptClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	subject := bus.KeptRoleSubject("last-stored-reader")

	if _, err := client.LastStoredMessage(ctx, subject); !errors.Is(err, natsgo.ErrMsgNotFound) {
		t.Fatalf("read of a subject the stream holds nothing on = %v, want nats.ErrMsgNotFound", err)
	}

	var last uint64
	for _, body := range []string{`{"n":1}`, `{"n":2}`, `{"n":3}`} {
		ack, err := client.JS().Publish(subject, []byte(body))
		if err != nil {
			t.Fatalf("publish %s: %v", body, err)
		}
		last = ack.Sequence
	}

	stored, err := client.LastStoredMessage(ctx, subject)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if stored.Sequence != last {
		t.Fatalf("sequence = %d, want the newest message's %d", stored.Sequence, last)
	}
	if stored.Subject != subject {
		t.Fatalf("subject = %q, want %q", stored.Subject, subject)
	}
	if string(stored.Data) != `{"n":3}` {
		t.Fatalf("body = %q, want the newest of the three", stored.Data)
	}
}

// A kept copy is stored on the role's kept subject while the envelope still names the role topic
// it was published on, so the next holder is handed the message as its sender sent it. Every
// listener that could not deliver that message writes its copy under one id (KeptMsgID), so the
// second write is collapsed into the first and the stream still holds one message on the subject.
func TestPublishStoredAsKeepsTheEnvelopesOwnTopicAndCollapsesRepeats(t *testing.T) {
	client := keptClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const role = "kept-writer"
	subject := bus.KeptRoleSubject(role)
	item := keptEnvelope(role, "kept-1")
	msgID := bus.KeptMsgID(item.DedupeKey, role)

	if err := client.PublishStoredAs(ctx, subject, item, msgID); err != nil {
		t.Fatalf("first kept publish: %v", err)
	}

	stored, err := client.LastStoredMessage(ctx, subject)
	if err != nil {
		t.Fatalf("read the kept copy: %v", err)
	}
	if stored.Subject != subject {
		t.Fatalf("the kept copy is stored on %q, want %q", stored.Subject, subject)
	}
	var kept contracts.Envelope
	if err := json.Unmarshal(stored.Data, &kept); err != nil {
		t.Fatalf("read the kept envelope: %v", err)
	}
	if kept.Topic != contracts.RoleTopicPrefix+role {
		t.Fatalf("the kept envelope's topic = %q, want the role topic it was published on", kept.Topic)
	}
	if kept.EventID != item.EventID {
		t.Fatalf("the kept envelope's event id = %q, want %q", kept.EventID, item.EventID)
	}

	if err := client.PublishStoredAs(ctx, subject, item, msgID); err != nil {
		t.Fatalf("second kept publish: %v", err)
	}
	if held := storedOn(t, client, subject); held != 1 {
		t.Fatalf("the stream holds %d messages on %s, want 1", held, subject)
	}
}

// Two listeners draining one role must not both advance its cursor: a write names the sequence
// its caller read, and the stream takes only the write whose expectation still holds. The loser's
// error wraps ErrCursorMoved and leaves the cursor where it was; a write at the sequence the first
// returned lands.
func TestPublishCursorAtRefusesAWriteWhoseCursorMoved(t *testing.T) {
	client := keptClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	subject := bus.KeptRoleCursorSubject("cursor-racer")

	first, err := client.PublishCursorAt(ctx, subject, []byte(`{"seq":11}`), 0)
	if err != nil {
		t.Fatalf("the first cursor write: %v", err)
	}
	if first == 0 {
		t.Fatal("the first cursor write returned no sequence")
	}

	_, err = client.PublishCursorAt(ctx, subject, []byte(`{"seq":22}`), 0)
	if !errors.Is(err, bus.ErrCursorMoved) {
		t.Fatalf("a write at a stale expectation = %v, want an error wrapping bus.ErrCursorMoved", err)
	}
	stored, err := client.LastStoredMessage(ctx, subject)
	if err != nil {
		t.Fatalf("read the cursor after the refusal: %v", err)
	}
	if string(stored.Data) != `{"seq":11}` || stored.Sequence != first {
		t.Fatalf("the refused write changed the cursor: %+v, want the first value at %d", stored, first)
	}

	second, err := client.PublishCursorAt(ctx, subject, []byte(`{"seq":33}`), first)
	if err != nil {
		t.Fatalf("the cursor write at the sequence the first returned: %v", err)
	}
	stored, err = client.LastStoredMessage(ctx, subject)
	if err != nil {
		t.Fatalf("read the cursor after the second write: %v", err)
	}
	if stored.Sequence != second || string(stored.Data) != `{"seq":33}` {
		t.Fatalf("the cursor = %+v, want the new value at %d", stored, second)
	}
}
