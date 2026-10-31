package bus_test

import (
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A forward counts once the stream stores it: no session subscribes to its agent subject any more
// (INFRA-771), so StartRequestStoredTo's wait returns nil on the stream's acknowledgement, well
// inside the window, and stores the forward once however often it is sent (its Nats-Msg-Id). On a
// subject no stream captures and nobody subscribes it returns ErrForwardNotStored.
func TestStartRequestStoredToCountsTheStreamsAcknowledgementAlone(t *testing.T) {
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	item := contracts.Envelope{
		EventID:        "evt_stored_only",
		Source:         "agent",
		SourceEventID:  "src_stored_only",
		Topic:          "notifications.role.stored-only",
		DedupeKey:      "publish.stored-only",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "a forward to a serving port",
		TraceID:        "trace_stored_only",
	}
	const window = 2 * time.Second
	subject := contracts.AgentSubject("ses_serving_port")
	for range 2 {
		started := time.Now()
		forward, err := client.StartRequestStoredTo(subject, item, window)
		if err != nil {
			t.Fatalf("StartRequestStoredTo: %v", err)
		}
		if err := forward.Wait(); err != nil {
			t.Fatalf("the stored forward's wait = %v, want nil on the stream's acknowledgement", err)
		}
		if elapsed := time.Since(started); elapsed >= window {
			t.Fatalf("the stored forward's wait took %s, the whole window; want it to end on the acknowledgement", elapsed)
		}
	}
	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if stored := info.State.Subjects[subject]; stored != 1 {
		t.Fatalf("the stream stores %d copies of the forward sent twice, want 1", stored)
	}

	const uncaptured = "test.uncaptured.serving-port"
	forward, err := client.StartRequestStoredTo(uncaptured, item, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("StartRequestStoredTo on %s: %v", uncaptured, err)
	}
	if err := forward.Wait(); !errors.Is(err, bus.ErrForwardNotStored) {
		t.Fatalf("the wait of a forward no stream stores = %v, want ErrForwardNotStored", err)
	}
}

// A forward whose flush times out is refused by StartRequestStoredTo itself, before any wait, and
// within the window the caller gave rather than the client's default 10 s flush: the forward is
// still buffered on this side of a stalled connection and is not known to have reached anyone, so
// the listener reports it delivery_failed at once rather than holding the role send open. Driven
// for real: the server's traffic is frozen, so the client stays CONNECTED, buffers the publish,
// and its flush waits for a PONG that cannot come until the window runs out.
func TestStartRequestStoredToIsBoundedByItsWindowWhenTheFlushTimesOut(t *testing.T) {
	srv, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	subject := contracts.AgentSubject("ses_stalled")
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
	// Control: the same subject and envelope are stored while the server runs.
	forward, err := client.StartRequestStoredTo(subject, item, 2*time.Second)
	if err != nil {
		t.Fatalf("StartRequestStoredTo against a healthy server: %v", err)
	}
	if err := forward.Wait(); err != nil {
		t.Fatalf("the stored forward's wait against a healthy server = %v, want nil", err)
	}

	// The server's traffic stops in both directions while its connection stays up.
	thaw := srv.Freeze(t)
	// Cleanups run last-in first-out, so this thaw precedes startNATS's shutdown.
	t.Cleanup(thaw)

	const window = 500 * time.Millisecond
	started := time.Now()
	_, err = client.StartRequestStoredTo(subject, item, window)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("StartRequestStoredTo against a frozen server returned no error; the forward cannot have been flushed")
	}
	if !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("StartRequestStoredTo error = %v, want a wrapped nats.ErrTimeout from the flush", err)
	}
	if elapsed < window || elapsed > 2*time.Second {
		t.Fatalf("StartRequestStoredTo returned after %s; the flush must be bounded by the %s window, not the client's 10s default", elapsed, window)
	}
}
