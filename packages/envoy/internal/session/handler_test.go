package session_test

import (
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
	deliverer.MachineID = "test-machine"
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

// TestHandleAgentMessage_StoredInterestOnAnotherMachine tests that a session whose stored
// subscription names another listener is that listener's, though its session entry names none:
// the handler reports it owned elsewhere, requests nothing and does not NAK, since the listener
// its topics are fanned out on delivers the message from its own durable.
func TestHandleAgentMessage_StoredInterestOnAnotherMachine(t *testing.T) {
	var requests []string
	sessions, deliverer := newKVDeliverer(t)
	deliverer.MachineID = "test-machine"
	deliverer.HTTPClient = reachableClient(&requests)
	if err := sessions.Put("ses_target", session.SessionEntry{Port: 4096, Dir: "/test"}); err != nil {
		t.Fatalf("failed to register session: %v", err)
	}

	interest := &store.Interest{
		SessionID: "ses_target",
		Dir:       "/test",
		MachineID: "other-machine",
	}

	result := session.HandleAgentMessage(
		newTestEnvelope("agent", "notifications.agent.ses_target", "test"),
		"ses_target", "test-machine", interest, &deliverer,
	)

	if !result.OwnedElsewhere || result.Delivered || result.ShouldNAK || result.Err != nil {
		t.Fatalf("result = %+v, want owned elsewhere with no delivery, NAK or error", result)
	}
	if len(requests) != 0 {
		t.Fatalf("requested %q, want no request", requests)
	}
}

// reachableClient answers every request as a serving port would (204), as a session the listener
// could reach does, and records each URL it was asked for in requests.
func reachableClient(requests *[]string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		*requests = append(*requests, req.URL.String())
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
}

// TestHandleAgentMessage_WrongMachineKVAcks tests that with no stored subscription, when the KV
// session registry says a session is another listener's (machine-B), the registry path reports it
// owned elsewhere, neither delivers nor NAKs, and requests nothing even though the session's port
// answers: the session is its owner's to deliver, and every listener on the bus reads each inbox
// message, so a second delivery would arrive twice.
func TestHandleAgentMessage_WrongMachineKVAcks(t *testing.T) {
	var requests []string
	sessions, deliverer := newKVDeliverer(t)
	deliverer.MachineID = "machine-A"
	deliverer.HTTPClient = reachableClient(&requests)
	if err := sessions.Put("ses_target", session.SessionEntry{Port: 4096, MachineID: "machine-B", Host: "example-host-b", Dir: "/test"}); err != nil {
		t.Fatalf("failed to register session: %v", err)
	}

	// No interest — handler falls to registry lookup path
	result := session.HandleAgentMessage(
		newTestEnvelope("agent", "notifications.agent.ses_target", "test"),
		"ses_target", "machine-A", nil, &deliverer,
	)

	if !result.OwnedElsewhere || result.Delivered || result.ShouldNAK || result.Err != nil {
		t.Fatalf("result = %+v, want owned elsewhere with no delivery, NAK or error", result)
	}
	if len(requests) != 0 {
		t.Fatalf("requested %q, want no request", requests)
	}
}

// TestHandleAgentMessage_RehomedSessionStaysWithItsStoredInterest tests that a session whose
// stored subscription names this listener (machine-A) is delivered by it after the session
// re-registered through another listener (machine-B): the stored MachineID survives the
// re-registration and is the one machine-A matches the session's topics on, so both paths agree
// machine-A owns it. Delivery follows machine-A's own address rule, here the recorded host.
func TestHandleAgentMessage_RehomedSessionStaysWithItsStoredInterest(t *testing.T) {
	var requests []string
	sessions, deliverer := newKVDeliverer(t)
	deliverer.MachineID = "machine-A"
	deliverer.HostBridge = ""
	deliverer.HTTPClient = reachableClient(&requests)
	if err := sessions.Put("ses_target", session.SessionEntry{Port: 4096, MachineID: "machine-B", Host: "example-host-b", Dir: "/test"}); err != nil {
		t.Fatalf("failed to register session: %v", err)
	}

	interest := &store.Interest{
		SessionID: "ses_target",
		Dir:       "/test",
		MachineID: "machine-A",
	}

	result := session.HandleAgentMessage(
		newTestEnvelope("agent", "notifications.agent.ses_target", "test"),
		"ses_target", "machine-A", interest, &deliverer,
	)

	if !result.Delivered || result.OwnedElsewhere || result.ShouldNAK || result.Err != nil {
		t.Fatalf("result = %+v, want delivered", result)
	}
	if want := "http://example-host-b:4096/session/ses_target/prompt_async"; len(requests) != 1 || requests[0] != want {
		t.Fatalf("requested %q, want exactly [%q]", requests, want)
	}
}
