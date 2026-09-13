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
	client, err := bus.Connect([]string{uri})
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
	client, err := bus.Connect([]string{uri})
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
