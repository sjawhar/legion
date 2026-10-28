package bus_test

import (
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
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
	err = client.RequestCoreTo(contracts.AgentSubject("ses_absent"), item, 300*time.Millisecond)
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
	if err := client.RequestCoreTo(subject, item, 2*time.Second); err != nil {
		t.Fatalf("RequestCoreTo with a live responder = %v, want nil", err)
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
	if err := client.RequestCoreTo(subject, item, 2*time.Second); err != nil {
		t.Fatalf("RequestCoreTo against a healthy server = %v, want nil", err)
	}

	// The server's traffic stops in both directions while its connection stays up.
	thaw := srv.Freeze(t)
	// Cleanups run last-in first-out, so this thaw precedes startNATS's shutdown.
	t.Cleanup(thaw)

	const window = 500 * time.Millisecond
	started := time.Now()
	err = client.RequestCoreTo(subject, item, window)
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

// The listener's role lane forwards an envelope to its holder with RequestCoreTo, under
// contracts.RoleForwardDedupePrefix and the envelope's own dedupe key, to an agent subject the
// stream captures. A forward of an envelope whose key names its event - Dispatch's, or a webhook's,
// whose key the prefix hides from contracts.DedupeKeyNamesTheUpstreamEvent - is stored once
// however often it is sent. Two distinct envelopes that share a key their caller chose are two
// messages, and the stream stores both.
func TestARoleForwardIsStoredOnceOnlyWhenItsEnvelopeNamesItsEvent(t *testing.T) {
	_, url, client := requestClient(t)
	const topic = "notifications.role.forward-dedupe"
	chosen := func(name string) contracts.Envelope {
		return contracts.Envelope{EventID: "evt-" + name, Source: "agent", SourceEventID: "src-" + name, Topic: topic, DedupeKey: "shared-key"}
	}
	dispatch := contracts.Envelope{EventID: "dispatch-7", Source: "dispatch", SourceEventID: "7", Topic: topic, DedupeKey: "dispatch-7"}
	webhook := contracts.Envelope{EventID: "evt-webhook", Source: "github", SourceEventID: "guid-1", Topic: topic, DedupeKey: "github.guid-1"}
	for _, tc := range []struct {
		name, holder string
		sends        []contracts.Envelope
		want         uint64
	}{
		{"two envelopes under one key their caller chose", "ses_forward_chosen", []contracts.Envelope{chosen("first"), chosen("second")}, 2},
		{"a dispatch envelope sent twice", "ses_forward_dispatch", []contracts.Envelope{dispatch, dispatch}, 1},
		{"a webhook envelope sent twice", "ses_forward_webhook", []contracts.Envelope{webhook, webhook}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subject := contracts.AgentSubject(tc.holder)
			responder(t, url, subject)
			for index, item := range tc.sends {
				item.DedupeKey = contracts.RoleForwardDedupePrefix + item.DedupeKey
				item.IssuedAt = contracts.NowMillis()
				if err := client.RequestCoreTo(subject, item, 2*time.Second); err != nil {
					t.Fatalf("forward %d returned %v, want nil", index+1, err)
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
				t.Fatalf("the stream holds %d copies on %s after %d forwards, want %d", got, subject, len(tc.sends), tc.want)
			}
		})
	}
}
