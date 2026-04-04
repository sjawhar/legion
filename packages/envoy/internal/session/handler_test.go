package session

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/store"
)

// TestHandleAgentMessage_DeliveredExactlyOnce tests that the agent message handler
// delivers via interest OR fallback, never both. This catches the missing-return bug
// where both paths executed for the same message.
func TestHandleAgentMessage_DeliveredExactlyOnce(t *testing.T) {
	var deliveryCount atomic.Int32

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveryCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer mock.Close()

	port := mockPort(mock.URL)
	dir := t.TempDir()
	writeRegistryEntry(t, dir, 1, port, "ses_target")

	deliverer := newDeliverer(dir)

	// Session exists in BOTH interest registry AND file registry — handler should use
	// interest path and NOT fall through to fallback
	interest := &store.Interest{
		SessionID: "ses_target",
		Dir:       "/test",
		MachineID: "test-machine",
	}

	item := newTestEnvelope("agent", "notifications.agent.ses_target", "test message")

	result := HandleAgentMessage(item, "ses_target", "test-machine", interest, &deliverer)

	if result.Err != nil {
		t.Fatalf("expected success, got error: %v", result.Err)
	}
	if !result.Delivered {
		t.Fatal("expected message to be delivered")
	}
	if count := deliveryCount.Load(); count != 1 {
		t.Fatalf("expected exactly 1 delivery, got %d — handler delivered via both interest and fallback paths", count)
	}
}

// TestHandleAgentMessage_FallbackWhenNoInterest tests that when there's no interest
// entry but a file registry entry exists, the handler delivers via fallback.
func TestHandleAgentMessage_FallbackWhenNoInterest(t *testing.T) {
	var deliveryCount atomic.Int32

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveryCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer mock.Close()

	port := mockPort(mock.URL)
	dir := t.TempDir()
	writeRegistryEntry(t, dir, 1, port, "ses_target")

	deliverer := newDeliverer(dir)

	// No interest entry — handler should fall through to file registry
	result := HandleAgentMessage(
		newTestEnvelope("agent", "notifications.agent.ses_target", "test"),
		"ses_target", "test-machine", nil, &deliverer,
	)

	if result.Err != nil {
		t.Fatalf("expected success via fallback, got error: %v", result.Err)
	}
	if !result.Delivered {
		t.Fatal("expected delivery via fallback")
	}
	if count := deliveryCount.Load(); count != 1 {
		t.Fatalf("expected exactly 1 delivery via fallback, got %d", count)
	}
}

// TestHandleAgentMessage_UnknownSession tests that when neither interest nor
// file registry has the session, the handler signals no delivery (ACK, don't NAK).
func TestHandleAgentMessage_UnknownSession(t *testing.T) {
	dir := t.TempDir() // empty
	deliverer := newDeliverer(dir)

	result := HandleAgentMessage(
		newTestEnvelope("agent", "notifications.agent.ses_unknown", "test"),
		"ses_unknown", "test-machine", nil, &deliverer,
	)

	if result.Delivered {
		t.Fatal("should not deliver to unknown session")
	}
	if result.ShouldNAK {
		t.Fatal("unknown session should ACK (skip), not NAK (retry forever)")
	}
}

// TestHandleAgentMessage_WrongMachine tests that when the interest exists but
// for a different machine, the handler falls through to file registry.
func TestHandleAgentMessage_WrongMachine(t *testing.T) {
	var deliveryCount atomic.Int32

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveryCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer mock.Close()

	port := mockPort(mock.URL)
	dir := t.TempDir()
	writeRegistryEntry(t, dir, 1, port, "ses_target")

	deliverer := newDeliverer(dir)

	// Interest exists but for a different machine
	interest := &store.Interest{
		SessionID: "ses_target",
		Dir:       "/test",
		MachineID: "other-machine",
	}

	result := HandleAgentMessage(
		newTestEnvelope("agent", "notifications.agent.ses_target", "test"),
		"ses_target", "test-machine", interest, &deliverer,
	)

	if result.Err != nil {
		t.Fatalf("expected success via fallback, got: %v", result.Err)
	}
	if count := deliveryCount.Load(); count != 1 {
		t.Fatalf("expected 1 delivery via fallback, got %d", count)
	}
}

