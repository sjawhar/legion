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

func newSessionRegistry(t *testing.T) *session.SessionRegistry {
	t.Helper()
	client := session.SetupNATS(t)
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1), session.WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}
	return sessions
}

// deliverThroughEntry resolves the way the listener's core path does: the caller reads the
// target's record (resolvedEntry) and hands it to session.Resolve, which reads no registry of its
// own, under the core path's stream sequence of 0.
func deliverThroughEntry(t *testing.T, sessions *session.SessionRegistry, item contracts.Envelope, interest store.Interest) error {
	t.Helper()
	_, err := session.Resolve(item, interest.SessionID, resolvedEntry(t, sessions, interest.SessionID), 0)
	return err
}

func TestDeliver_NoRegistryEntry(t *testing.T) {
	sessions := newSessionRegistry(t)
	interest := store.Interest{SessionID: "ses_ghost", Dir: "/test", MachineID: "m"}
	item := newTestEnvelope("agent", "notifications.agent.ses_ghost", "test message")

	err := deliverThroughEntry(t, sessions, item, interest)
	if err == nil {
		t.Fatal("expected error for missing session, got nil")
	}
}

<<<<<<< conflict 1 of 2
+++++++ zwxwlryn 92c90c86 "merge: main into INFRA-771, past f5bdbada so worker-image/build has repo-paths.sh"
// An entry the previous build wrote for a self-subscribed session with no serving port names a
// session on the core path: it holds no consumer, nothing can be pushed to it, and no
// subscription of its own takes its messages any more. The delivery is ErrNoPushTarget, which
// raises a delivery exception, rather than the silent skip that rule once made.
func TestDeliver_NoPushTarget_whenSelfSubscribedSessionHasNoPort(t *testing.T) {
%%%%%%% diff from: rmmmynwt 8bfdf258 "fix(dispatch): a version that restores the approved text keeps its approval (#2703)"
\\\\\\\        to: lmonpkut e2b93abb "ci: the envoy-go lane gets 60 minutes, and its test steps time out inside it (#2739)"
-func TestDeliver_SkipsPush_whenSelfSubscribedSessionHasNoPort(t *testing.T) {
+// A self-subscribed session takes its messages off its own NATS subscription, so the resolution
+// succeeds with nothing for the caller to do.
+func TestDeliver_SucceedsForASelfSubscribedSession(t *testing.T) {
>>>>>>> conflict 1 of 2 ends
	// Given
	sessions := newSessionRegistry(t)
	var entry session.SessionEntry
	if err := json.Unmarshal([]byte(`{"self_subscribed":true}`), &entry); err != nil {
		t.Fatalf("decode self-subscribed session entry: %v", err)
	}
	if err := sessions.Put("ses_omp", entry); err != nil {
		t.Fatalf("register self-subscribed session: %v", err)
	}

	// When
	err := deliverThroughEntry(t, sessions,
		newTestEnvelope("agent", "notifications.agent.ses_omp", "test message"),
		store.Interest{SessionID: "ses_omp"},
	)

	// Then
	if !errors.Is(err, session.ErrNoPushTarget) {
		t.Fatalf("expected ErrNoPushTarget for a self-subscribed session with no port, got %v", err)
	}
}

// A session whose entry records consumer delivery takes every stored message from its consumer,
// so no other path delivers it.
func TestDeliver_SkipsStoredTopics_whenSessionIsOnConsumerDelivery(t *testing.T) {
	// Given
	sessions := newSessionRegistry(t)
	var entry session.SessionEntry
	if err := json.Unmarshal([]byte(`{"delivery":"consumer","epoch":"e1"}`), &entry); err != nil {
		t.Fatalf("decode consumer-delivery session entry: %v", err)
	}
	if err := sessions.Put("ses_consumer", entry); err != nil {
		t.Fatalf("register consumer-delivery session: %v", err)
	}

	// When
	stored, storedErr := session.Resolve(
		newTestEnvelope("agent", "notifications.agent.ses_consumer", "stored message"),
<<<<<<< conflict 2 of 2
%%%%%%% diff from: rmmmynwt 8bfdf258 "fix(dispatch): a version that restores the approved text keeps its approval (#2703)"
\\\\\\\        to: zwxwlryn 92c90c86 "merge: main into INFRA-771, past f5bdbada so worker-image/build has repo-paths.sh"
 		store.Interest{SessionID: "ses_consumer"},
 		resolved,
 	)
 	liveErr := deliverThroughEntry(t, deliverer, sessions,
-		newTestEnvelope("agent", "notifications.team.deploy", "live message"),
+		newTestEnvelope("agent", "notifications.lxspec.deploy", "live message"),
 		store.Interest{SessionID: "ses_consumer"},
+++++++ lmonpkut e2b93abb "ci: the envoy-go lane gets 60 minutes, and its test steps time out inside it (#2739)"
		"ses_consumer",
		resolvedEntry(t, sessions, "ses_consumer"),
		0,
>>>>>>> conflict 2 of 2 ends
	)

	// Then
	if storedErr != nil || !stored.Skipped {
		t.Fatalf("stored message to a consumer-delivery session: result %+v, error %v; want skipped", stored, storedErr)
	}
}
