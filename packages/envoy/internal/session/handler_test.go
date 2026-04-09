package session

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/store"
)

// TestHandleAgentMessage_DeliveredExactlyOnce tests that the agent message handler
// delivers via interest OR registry lookup, never both.
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

	deliverer := newFileDeliverer(dir)

	// Session exists in interest registry AND file registry — handler should use
	// interest path and NOT fall through to registry lookup
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
		t.Fatalf("expected exactly 1 delivery, got %d — handler delivered via both paths", count)
	}
}

// TestHandleAgentMessage_FallbackWhenNoInterest tests that when there's no interest
// entry but a registry entry exists, the handler delivers via the registry lookup.
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

	deliverer := newFileDeliverer(dir)

	// No interest entry — handler should fall through to registry lookup
	result := HandleAgentMessage(
		newTestEnvelope("agent", "notifications.agent.ses_target", "test"),
		"ses_target", "test-machine", nil, &deliverer,
	)

	if result.Err != nil {
		t.Fatalf("expected success via registry lookup, got error: %v", result.Err)
	}
	if !result.Delivered {
		t.Fatal("expected delivery via registry lookup")
	}
	if count := deliveryCount.Load(); count != 1 {
		t.Fatalf("expected exactly 1 delivery via registry lookup, got %d", count)
	}
}

// TestHandleAgentMessage_UnknownSession tests that when no registry has the session,
// the handler signals no delivery (ACK, don't NAK).
func TestHandleAgentMessage_UnknownSession(t *testing.T) {
	dir := t.TempDir() // empty
	deliverer := newFileDeliverer(dir)

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
// for a different machine, the handler falls through to registry lookup.
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

	deliverer := newFileDeliverer(dir)

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
		t.Fatalf("expected success via registry lookup, got: %v", result.Err)
	}
	if count := deliveryCount.Load(); count != 1 {
		t.Fatalf("expected 1 delivery via registry lookup, got %d", count)
	}
}

// TestHandleAgentMessage_WrongMachineKVAcks tests that when the KV session
// registry says a session is on another machine, the handler returns no-delivery
// (ACK) rather than NAK.
func TestHandleAgentMessage_WrongMachineKVAcks(t *testing.T) {
	var deliveryCount atomic.Int32

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveryCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer mock.Close()

	port := mockPort(mock.URL)

	client := setupNATS(t)
	sessions, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}
	// KV says session is on machine-B
	sessions.Put("ses_target", SessionEntry{Port: port, MachineID: "machine-B", Dir: "/test"})

	deliverer := Deliverer{
		MachineID:    "machine-A",
		HostBridge:   "127.0.0.1",
		RequestLimit: 5 * time.Second,
		Sessions:     sessions,
	}

	// No interest or wrong-machine interest — handler falls to registry lookup path
	result := HandleAgentMessage(
		newTestEnvelope("agent", "notifications.agent.ses_target", "test"),
		"ses_target", "machine-A", nil, &deliverer,
	)

	// Should ACK (not NAK) — another listener owns this session
	if result.Delivered {
		t.Fatal("expected no delivery (session on different machine)")
	}
	if result.ShouldNAK {
		t.Fatal("wrong machine should ACK, not NAK")
	}
	if count := deliveryCount.Load(); count != 0 {
		t.Fatalf("expected 0 deliveries, got %d", count)
	}
}

// TestHandleAgentMessage_InterestPathWrongMachineKVAcks tests that even when
// the interest path matches this machine, if the KV session registry says the
// session moved to another machine, the handler returns no-delivery (ACK).
func TestHandleAgentMessage_InterestPathWrongMachineKVAcks(t *testing.T) {
	var deliveryCount atomic.Int32

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveryCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer mock.Close()

	port := mockPort(mock.URL)

	client := setupNATS(t)
	sessions, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}
	// KV says session moved to machine-B
	sessions.Put("ses_target", SessionEntry{Port: port, MachineID: "machine-B", Dir: "/test"})

	deliverer := Deliverer{
		MachineID:    "machine-A",
		HostBridge:   "127.0.0.1",
		RequestLimit: 5 * time.Second,
		Sessions:     sessions,
	}

	// Interest says machine-A (stale interest), but KV says machine-B
	interest := &store.Interest{
		SessionID: "ses_target",
		Dir:       "/test",
		MachineID: "machine-A",
	}

	result := HandleAgentMessage(
		newTestEnvelope("agent", "notifications.agent.ses_target", "test"),
		"ses_target", "machine-A", interest, &deliverer,
	)

	if result.Delivered {
		t.Fatal("expected no delivery (KV says session moved to machine-B)")
	}
	if result.ShouldNAK {
		t.Fatal("wrong machine should ACK, not NAK")
	}
	if count := deliveryCount.Load(); count != 0 {
		t.Fatalf("expected 0 deliveries, got %d", count)
	}
}
