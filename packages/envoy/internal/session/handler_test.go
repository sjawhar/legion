package session_test

import (
	"testing"

	session "github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// resolvedEntry is the session record the listener resolves before it hands a message to
// HandleAgentMessage: the registry's entry for sessionID, or nil when it holds none. Delivery no
// longer reads the registry itself, so every test here resolves it the way its caller does.
func resolvedEntry(t *testing.T, sessions *session.SessionRegistry, sessionID string) *session.SessionEntry {
	t.Helper()
	entry, err := sessions.Get(sessionID)
	if err != nil {
		return nil
	}
	return &entry
}

// TestHandleAgentMessage_UnknownSession tests that when no registry has the session,
// the handler signals no delivery (ACK, don't NAK).
func TestHandleAgentMessage_UnknownSession(t *testing.T) {
	newSessionRegistry(t)

	result := session.HandleAgentMessage(
		newTestEnvelope("agent", "notifications.agent.ses_unknown", "test"),
		"ses_unknown", "test-machine", nil, nil,
	)

	if result.Delivered {
		t.Fatal("should not deliver to unknown session")
	}
	if result.ShouldNAK {
		t.Fatal("unknown session should ACK (skip), not NAK (retry forever)")
	}
	if result.Reason != session.ReasonNoSessionRecord {
		t.Fatalf("expected ReasonNoSessionRecord for a session with no record, got %v", result.Reason)
	}
}

// TestHandleAgentMessage_StoredInterestOnAnotherMachine tests that a session whose stored
// subscription names another listener is that listener's, though its session entry names none:
// the handler reports it owned elsewhere and does not NAK, since the listener its topics are
// fanned out on delivers the message from its own durable.
func TestHandleAgentMessage_StoredInterestOnAnotherMachine(t *testing.T) {
	sessions := newSessionRegistry(t)
	if err := sessions.Put("ses_target", session.SessionEntry{Dir: "/test"}); err != nil {
		t.Fatalf("failed to register session: %v", err)
	}

	interest := &store.Interest{
		SessionID: "ses_target",
		Dir:       "/test",
		MachineID: "other-machine",
	}

	result := session.HandleAgentMessage(
		newTestEnvelope("agent", "notifications.agent.ses_target", "test"),
		"ses_target", "test-machine", interest, resolvedEntry(t, sessions, "ses_target"),
	)

	if !result.OwnedElsewhere || result.Delivered || result.ShouldNAK || result.Err != nil {
		t.Fatalf("result = %+v, want owned elsewhere with no delivery, NAK or error", result)
	}
}
