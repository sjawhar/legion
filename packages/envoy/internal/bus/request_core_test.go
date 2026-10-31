package bus_test

import (
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A forward that reached the server (publish and flush both succeeded) and drew
// no receipt inside the window is the one case RequestCoreTo reports as
// ErrReceiptTimeout; the listener maps that, and only that, to receipt_timeout.
func TestRequestCoreToReportsNoReceiptAsErrReceiptTimeout(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	item := contracts.Envelope{
		EventID:        "evt_no_receipt",
		Source:         "agent",
		SourceEventID:  "src_no_receipt",
		Topic:          "notifications.role.nobody-listens",
		DedupeKey:      "publish.no-receipt",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "no subscriber on the agent subject",
		TraceID:        "trace_no_receipt",
	}
	started := time.Now()
	err = client.RequestCoreTo(contracts.AgentSubject("ses_absent"), item, "", 300*time.Millisecond)
	elapsed := time.Since(started)
	if !errors.Is(err, bus.ErrReceiptTimeout) {
		t.Fatalf("RequestCoreTo error = %v, want errors.Is ErrReceiptTimeout", err)
	}
	if elapsed < 300*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("RequestCoreTo returned after %s; want the full 300ms window and no more", elapsed)
	}
}

// A receipt that does arrive is a nil error, so the sentinel is not simply
// "the window elapsed".
func TestRequestCoreToReturnsNilOnAnEmptyReceipt(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	subject := contracts.AgentSubject("ses_present")
	responder, err := client.Conn.Subscribe(subject, func(message *natsgo.Msg) {
		if message.Reply != "" {
			_ = message.Respond(nil)
		}
	})
	if err != nil {
		t.Fatalf("subscribe responder: %v", err)
	}
	defer responder.Unsubscribe()
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	item := contracts.Envelope{
		EventID:        "evt_receipt",
		Source:         "agent",
		SourceEventID:  "src_receipt",
		Topic:          "notifications.role.somebody-listens",
		DedupeKey:      "publish.receipt",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "responder acknowledges",
		TraceID:        "trace_receipt",
	}
	if err := client.RequestCoreTo(subject, item, "", 2*time.Second); err != nil {
		t.Fatalf("RequestCoreTo with a live responder = %v, want nil", err)
	}
}

// A sender retries a forward until a receipt arrives, and the notification stream captures every
// agent subject, so each attempt would otherwise store another copy that a durable consumer and a
// replay hand out again. A sender that passes one Nats-Msg-Id for an envelope and a recipient
// (bus.ForwardMsgID) gives every attempt of that pair the same one: the stream keeps one copy per
// message and recipient, while the recipient's core subscription still gets every attempt and each
// attempt still returns on its receipt.
func TestRequestCoreToStoresOneCopyPerMessageAndRecipientAcrossAttempts(t *testing.T) {
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	holder := contracts.AgentSubject("ses_retried_holder")
	other := contracts.AgentSubject("ses_other_recipient")
	var holderFrames, otherFrames atomic.Int32
	// The responder answers only the two recipients' subjects: the sentinel below is a JetStream
	// publish on another agent subject, whose acknowledgement an empty answer would race.
	responder, err := client.Conn.Subscribe("notifications.agent.*", func(message *natsgo.Msg) {
		switch message.Subject {
		case holder:
			holderFrames.Add(1)
		case other:
			otherFrames.Add(1)
		default:
			return
		}
		if message.Reply != "" {
			_ = message.Respond(nil)
		}
	})
	if err != nil {
		t.Fatalf("subscribe responder: %v", err)
	}
	t.Cleanup(func() { _ = responder.Unsubscribe() })
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	item := contracts.Envelope{
		EventID:        "evt_retried",
		Source:         "agent",
		SourceEventID:  "src_retried",
		Topic:          "notifications.role.retried",
		DedupeKey:      "envoy.role.forward.publish.retried",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "a forward sent twice",
		TraceID:        "trace_retried",
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := client.RequestCoreTo(holder, item, bus.ForwardMsgID(item.DedupeKey, holder), 2*time.Second); err != nil {
			t.Fatalf("attempt %d: RequestCoreTo with a live responder = %v, want nil", attempt, err)
		}
	}
	// The control that keeps the count honest: the same envelope to a second recipient is a
	// message of its own there, so the stream is not simply keeping one copy per envelope.
	if err := client.RequestCoreTo(other, item, bus.ForwardMsgID(item.DedupeKey, other), 2*time.Second); err != nil {
		t.Fatalf("RequestCoreTo to the second recipient = %v, want nil", err)
	}
	if got := holderFrames.Load(); got != 2 {
		t.Fatalf("the holder's core subscription got %d frames, want both attempts", got)
	}
	if got := otherFrames.Load(); got != 1 {
		t.Fatalf("the second recipient's core subscription got %d frames, want 1", got)
	}

	// A stream stores what one connection publishes in the order it arrived, so once a later
	// JetStream publish from the same connection is acknowledged, every forward before it has been
	// stored or dropped as a duplicate.
	sentinel := item
	sentinel.DedupeKey = "publish.sentinel"
	data, err := json.Marshal(sentinel)
	if err != nil {
		t.Fatalf("marshal the sentinel: %v", err)
	}
	if _, err := client.JS().Publish(contracts.AgentSubject("sentinel"), data); err != nil {
		t.Fatalf("publish the sentinel: %v", err)
	}
	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: "notifications.agent.>"})
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if got := info.State.Subjects[holder]; got != 1 {
		t.Errorf("the stream holds %d copies on %s after two attempts, want 1", got, holder)
	}
	if got := info.State.Subjects[other]; got != 1 {
		t.Errorf("the stream holds %d copies on %s, want 1", got, other)
	}
}

// A flush that times out is the client's own error, never the receipt sentinel:
// the forward is still buffered on this side of a stalled connection and is not
// known to have reached anyone, so the listener must report it delivery_failed.
// nats.go's Flush returns the same nats.ErrTimeout value a receipt wait does,
// which is why the two exits need distinct errors. Driven for real: the server's
// traffic is frozen, so the client stays CONNECTED, buffers the publish, and its
// flush waits for a PONG that cannot come until the request window runs out.
func TestRequestCoreToFlushTimeoutIsNotErrReceiptTimeout(t *testing.T) {
	srv, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	subject := contracts.AgentSubject("ses_stalled")
	responder, err := client.Conn.Subscribe(subject, func(message *natsgo.Msg) {
		if message.Reply != "" {
			_ = message.Respond(nil)
		}
	})
	if err != nil {
		t.Fatalf("subscribe responder: %v", err)
	}
	defer responder.Unsubscribe()
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	item := contracts.Envelope{
		EventID:        "evt_stalled",
		Source:         "agent",
		SourceEventID:  "src_stalled",
		Topic:          "notifications.role.stalled-server",
		DedupeKey:      "publish.stalled",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "the server's traffic is frozen under a live connection",
		TraceID:        "trace_stalled",
	}
	// Control: the same subject and responder yield a receipt while the server runs.
	if err := client.RequestCoreTo(subject, item, "", 2*time.Second); err != nil {
		t.Fatalf("RequestCoreTo against a healthy server = %v, want nil", err)
	}

	// The server's traffic stops in both directions while its connection stays up.
	thaw := srv.Freeze(t)
	// Cleanups run last-in first-out, so this thaw precedes startNATS's shutdown.
	t.Cleanup(thaw)

	const window = 500 * time.Millisecond
	started := time.Now()
	err = client.RequestCoreTo(subject, item, "", window)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("RequestCoreTo against a frozen server returned nil; the forward cannot have been flushed")
	}
	if errors.Is(err, bus.ErrReceiptTimeout) {
		t.Fatalf("a flush timeout was reported as the receipt sentinel: %v", err)
	}
	if !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("RequestCoreTo error = %v, want a wrapped nats.ErrTimeout from the flush", err)
	}
	if elapsed < window || elapsed > 2*time.Second {
		t.Fatalf("RequestCoreTo returned after %s; the flush must be bounded by the %s window, not the client's 10s default", elapsed, window)
	}
}

// RequestCoreTo sends the forward under the MsgId its caller passes, so a forward sent again under
// one MsgId to an agent subject the stream captures is stored once, and one sent with none is
// stored each time. Which forward carries a MsgId is the listener role lane's choice
// (cmd/listener, roleTopicDelivery).
func TestARequestCoreToUnderOneMsgIDIsStoredOnce(t *testing.T) {
	_, url, client := requestClient(t)
	item := contracts.Envelope{
		EventID: "dispatch-7", Source: "dispatch", SourceEventID: "7",
		Topic: "notifications.role.forward-dedupe", DedupeKey: contracts.RoleForwardDedupePrefix + "dispatch-7",
	}
	for _, tc := range []struct {
		name, holder string
		withMsgID    bool
		want         uint64
	}{
		{"a forward sent twice under one MsgId", "ses_forward_msgid", true, 1},
		{"a forward sent twice with none", "ses_forward_no_msgid", false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subject := contracts.AgentSubject(tc.holder)
			responder(t, url, subject)
			var msgID string
			if tc.withMsgID {
				msgID = bus.ForwardMsgID(item.DedupeKey, subject)
			}
			for attempt := 1; attempt <= 2; attempt++ {
				forward := item
				forward.IssuedAt = contracts.NowMillis()
				if err := client.RequestCoreTo(subject, forward, msgID, 2*time.Second); err != nil {
					t.Fatalf("forward %d returned %v, want nil", attempt, err)
				}
			}
			// The stream takes the messages one connection sends in order, so once it has
			// acknowledged a later one, every forward before it is stored or dropped.
			if _, err := client.JS().Publish(subject+"_after", []byte("{}")); err != nil {
				t.Fatalf("publish the message after the forwards: %v", err)
			}
			info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
			if err != nil {
				t.Fatalf("read the stream: %v", err)
			}
			if got := info.State.Subjects[subject]; got != tc.want {
				t.Fatalf("the stream holds %d copies on %s after two forwards, want %d", got, subject, tc.want)
			}
		})
	}
}
