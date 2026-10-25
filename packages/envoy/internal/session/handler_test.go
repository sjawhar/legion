package session_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	session "github.com/sjawhar/envoy/internal/session"
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
	sessions, deliverer := newKVDeliverer(t)
	if err := sessions.Put("ses_target", session.SessionEntry{Port: port, Dir: "/test"}); err != nil {
		t.Fatalf("failed to register session: %v", err)
	}

	// Session exists in interest registry AND session registry — handler should use
	// interest path and NOT fall through to registry lookup
	interest := &store.Interest{
		SessionID: "ses_target",
		Dir:       "/test",
		MachineID: "test-machine",
	}

	item := newTestEnvelope("agent", "notifications.agent.ses_target", "test message")

	result := session.HandleAgentMessage(item, "ses_target", "test-machine", interest, &deliverer)

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
	sessions, deliverer := newKVDeliverer(t)
	if err := sessions.Put("ses_target", session.SessionEntry{Port: port, Dir: "/test"}); err != nil {
		t.Fatalf("failed to register session: %v", err)
	}

	// No interest entry — handler should fall through to registry lookup
	result := session.HandleAgentMessage(
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
	_, deliverer := newKVDeliverer(t)

	result := session.HandleAgentMessage(
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
// for a different machine, the handler falls through to registry lookup (since
// interest.MachineID != current machineID). The session is found on the current
// machine and delivery succeeds.
func TestHandleAgentMessage_WrongMachine(t *testing.T) {
	var deliveryCount atomic.Int32

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveryCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer mock.Close()

	port := mockPort(mock.URL)
	sessions, deliverer := newKVDeliverer(t)
	if err := sessions.Put("ses_target", session.SessionEntry{Port: port, Dir: "/test"}); err != nil {
		t.Fatalf("failed to register session: %v", err)
	}

	// Interest exists but for a different machine
	interest := &store.Interest{
		SessionID: "ses_target",
		Dir:       "/test",
		MachineID: "other-machine",
	}

	result := session.HandleAgentMessage(
		newTestEnvelope("agent", "notifications.agent.ses_target", "test"),
		"ses_target", "test-machine", interest, &deliverer,
	)

	// Interest is for different machine, so handler falls through to registry lookup.
	// Registry finds session on current machine, delivery succeeds.
	if !result.Delivered {
		t.Fatalf("expected delivery via registry lookup, got Delivered=%v, NAK=%v", result.Delivered, result.ShouldNAK)
	}
	if count := deliveryCount.Load(); count != 1 {
		t.Fatalf("expected 1 delivery via registry lookup, got %d", count)
	}
}

// unreachableClient fails every request, as a session machine the listener cannot reach does,
// and records each URL it was asked for in attempts.
func unreachableClient(attempts *[]string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		*attempts = append(*attempts, req.URL.String())
		return nil, errors.New("connection refused")
	})}
}

// TestHandleAgentMessage_WrongMachineKVAcks tests that when the KV session
// registry says a session is on another machine, the handler attempts delivery
// at the session's recorded host. Since that host is unreachable, delivery fails
// and the handler ACKs (doesn't NAK) because another listener may own this session.
func TestHandleAgentMessage_WrongMachineKVAcks(t *testing.T) {
	var attempts []string
	sessions, deliverer := newKVDeliverer(t)
	deliverer.MachineID = "machine-A"
	deliverer.HTTPClient = unreachableClient(&attempts)
	if err := sessions.Put("ses_target", session.SessionEntry{Port: 4096, MachineID: "machine-B", Host: "example-host-b", Dir: "/test"}); err != nil {
		t.Fatalf("failed to register session: %v", err)
	}

	// No interest — handler falls to registry lookup path
	result := session.HandleAgentMessage(
		newTestEnvelope("agent", "notifications.agent.ses_target", "test"),
		"ses_target", "machine-A", nil, &deliverer,
	)

	// Remote delivery fails (example-host-b unreachable), but handler ACKs
	// because another listener may own this session.
	if result.ShouldNAK {
		t.Fatal("expected ACK (not NAK) when remote delivery fails")
	}
	if result.Delivered {
		t.Fatal("expected no delivery (session on different machine)")
	}
	want := "http://example-host-b:4096/session/ses_target/prompt_async"
	if len(attempts) != 1 || attempts[0] != want {
		t.Fatalf("attempted %q, want exactly [%q]", attempts, want)
	}
}

// TestHandleAgentMessage_InterestPathWrongMachineKVAcks tests that when
// the interest path matches this machine, but the KV session registry says the
// session moved to another machine, the handler attempts delivery at the
// session's recorded host. Since that host is unreachable, delivery fails and NAKs for retry.
func TestHandleAgentMessage_InterestPathWrongMachineKVAcks(t *testing.T) {
	var attempts []string
	sessions, deliverer := newKVDeliverer(t)
	deliverer.MachineID = "machine-A"
	deliverer.HTTPClient = unreachableClient(&attempts)
	if err := sessions.Put("ses_target", session.SessionEntry{Port: 4096, MachineID: "machine-B", Host: "example-host-b", Dir: "/test"}); err != nil {
		t.Fatalf("failed to register session: %v", err)
	}

	// Interest says machine-A (stale interest), but KV says machine-B
	interest := &store.Interest{
		SessionID: "ses_target",
		Dir:       "/test",
		MachineID: "machine-A",
	}

	result := session.HandleAgentMessage(
		newTestEnvelope("agent", "notifications.agent.ses_target", "test"),
		"ses_target", "machine-A", interest, &deliverer,
	)

	// Interest path matches, but KV says session moved to machine-B.
	// Handler attempts delivery at its recorded host, which fails, so NAK for retry.
	if !result.ShouldNAK {
		t.Fatalf("expected NAK when remote delivery fails, got NAK=%v, Delivered=%v", result.ShouldNAK, result.Delivered)
	}
	if result.Delivered {
		t.Fatal("expected no delivery (KV says session moved to machine-B)")
	}
	want := "http://example-host-b:4096/session/ses_target/prompt_async"
	if len(attempts) != 1 || attempts[0] != want {
		t.Fatalf("attempted %q, want exactly [%q]", attempts, want)
	}
}
