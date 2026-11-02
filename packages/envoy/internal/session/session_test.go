package session_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	session "github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

func newTestEnvelope(source, topic, summary string) contracts.Envelope {
	return contracts.Envelope{
		EventID:        "test-event-1",
		Source:         source,
		SourceSession:  "ses_sender",
		SourceEventID:  "test-source-1",
		Topic:          topic,
		DedupeKey:      "test-dedupe-1",
		IssuedAt:       time.Now().UnixMilli(),
		PayloadSummary: summary,
		TraceID:        "test-trace-1",
	}
}

func newKVDeliverer(t *testing.T) (*session.SessionRegistry, session.Deliverer) {
	t.Helper()
	client := session.SetupNATS(t)
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1), session.WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}
	return sessions, session.Deliverer{}
}

// deliverThroughEntry delivers the way the listener does: the caller resolves the target's record
// (resolvedEntry) and hands it to the Deliverer, which reads no registry of its own.
func deliverThroughEntry(t *testing.T, deliverer session.Deliverer, sessions *session.SessionRegistry, item contracts.Envelope, interest store.Interest) error {
	t.Helper()
	_, err := deliverer.DeliverWithResultForEntry(item, interest, resolvedEntry(t, sessions, interest.SessionID))
	return err
}

func TestDeliver_NoRegistryEntry(t *testing.T) {
	sessions, deliverer := newKVDeliverer(t)
	interest := store.Interest{SessionID: "ses_ghost", Dir: "/test", MachineID: "m"}
	item := newTestEnvelope("agent", "notifications.agent.ses_ghost", "test message")

	err := deliverThroughEntry(t, deliverer, sessions, item, interest)
	if err == nil {
		t.Fatal("expected error for missing session, got nil")
	}
}

func TestDeliver_SkipsPush_whenSelfSubscribedSessionHasNoPort(t *testing.T) {
	// Given
	sessions, deliverer := newKVDeliverer(t)
	var entry session.SessionEntry
	if err := json.Unmarshal([]byte(`{"self_subscribed":true}`), &entry); err != nil {
		t.Fatalf("decode self-subscribed session entry: %v", err)
	}
	if err := sessions.Put("ses_omp", entry); err != nil {
		t.Fatalf("register self-subscribed session: %v", err)
	}

	// When
	err := deliverThroughEntry(t, deliverer, sessions,
		newTestEnvelope("agent", "notifications.agent.ses_omp", "test message"),
		store.Interest{SessionID: "ses_omp"},
	)

	// Then
	if err != nil {
		t.Fatalf("expected self-subscribed portless delivery to succeed, got %v", err)
	}
}

// A session whose entry records consumer delivery takes every stored message from its consumer,
// so no other path delivers it.
func TestDeliver_SkipsStoredTopics_whenSessionIsOnConsumerDelivery(t *testing.T) {
	// Given
	sessions, deliverer := newKVDeliverer(t)
	var entry session.SessionEntry
	if err := json.Unmarshal([]byte(`{"delivery":"consumer","epoch":"e1"}`), &entry); err != nil {
		t.Fatalf("decode consumer-delivery session entry: %v", err)
	}
	if err := sessions.Put("ses_consumer", entry); err != nil {
		t.Fatalf("register consumer-delivery session: %v", err)
	}

	// When
	stored, storedErr := deliverer.DeliverWithResultForEntry(
		newTestEnvelope("agent", "notifications.agent.ses_consumer", "stored message"),
		store.Interest{SessionID: "ses_consumer"},
		resolvedEntry(t, sessions, "ses_consumer"),
	)

	// Then
	if storedErr != nil || !stored.Skipped {
		t.Fatalf("stored message to a consumer-delivery session: result %+v, error %v; want skipped", stored, storedErr)
	}
}
