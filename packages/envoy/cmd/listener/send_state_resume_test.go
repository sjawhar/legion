package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
)

// presenceRelayRig is relayRig with the known-session store open and the /v1 routes mounted over
// it, as main mounts them once every store is open. newSessionConsumerRigOn builds its mux before
// a test can add that store, and the delete route takes the store it was built with, so the
// routes are mounted again here over the same dependencies rather than the store being set under
// a mux that already closed over nil.
type presenceRelayRig struct {
	*relayRig
	known  *session.KnownSessions
	routes *http.ServeMux
}

func newPresenceRelayRig(t *testing.T) *presenceRelayRig {
	t.Helper()
	rig := newRelayRig(t)
	known, err := session.OpenKnownSessions(rig.harness.client.Conn, session.WithKnownSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("the known-session cache did not become ready: %v", err)
	}
	rig.deps.known = known
	routes := http.NewServeMux()
	registerV1Routes(routes, rig.deps, "test-machine", rig.harness.config.logger)
	return &presenceRelayRig{relayRig: rig, known: known, routes: routes}
}

// resumeRegistration is the registration of a session in consumer mode at epoch that declares the
// presence check, as a plugin that answers that check sends it on every heartbeat.
func resumeRegistration(epoch string) map[string]any {
	return map[string]any{
		"presence":           true,
		"client_instance_id": "ci-" + epoch,
		"delivery":           map[string]any{"consumer": true, "epoch": epoch},
	}
}

// Acceptance A3, end to end on one NATS server: a session that registers with its consumer and
// its relay subscription and declares the presence check reads connected; the same session, after
// a clean stop, reads stopped with a last_seen rather than the 404 that would tell its sender to
// route around it; and the message stored while it was stopped reaches it exactly once when it
// registers again under the same id.
//
// The "exactly once" is watched for longer than the session consumer's 60-second ack wait
// (bus.SessionConsumerAckWait), which is how long the server waits for a pulled message's
// acknowledgement before it delivers that message again: a relay that handed the frame over
// without acknowledging it would show up as a second copy inside that window and nowhere else.
// The whole test therefore takes a little over 70 seconds, nearly all of it that watch; it needs
// a -timeout past that.
func TestASessionThatStopsAndResumesTakesItsMessageExactlyOnce(t *testing.T) {
	rig := newPresenceRelayRig(t)
	const sessionID = "ses_stop_and_resume"
	started := time.Now()

	plugin := startSessionStandIn(t, rig.url, sessionID, answerDone)
	presence := startPresenceStandIn(t, rig.url, sessionID, "ci-epoch-1")
	registerThrough(t, rig.routes, sessionID, resumeRegistration("epoch-1"))

	connected := sendThroughRoutes(t, rig.routes, sessionID, map[string]any{"message": "while you are here"})
	if connected.status != http.StatusOK || connected.State != sessionStateConnected {
		t.Fatalf("a send to the registered session: status %d, body %s; want 200 connected",
			connected.status, connected.raw)
	}
	if asked := presence.asked.Load(); asked != 1 {
		t.Fatalf("the plugin was asked %d times for its presence, want once", asked)
	}
	// The message sent while it was there is relayed and answered now, so the one collected
	// after the resume below can only be the message stored while it was stopped.
	if frames := plugin.collect(t, 1, 10*time.Second); frames[0].item.EventID != connected.EventID {
		t.Fatalf("the connected session took %s, want the message just sent, %s",
			frames[0].item.EventID, connected.EventID)
	}

	stopThrough(t, rig.routes, sessionID)
	if _, err := rig.harness.sessions.Get(sessionID); err == nil {
		t.Fatalf("%s still holds a session entry, so it did not stop", sessionID)
	}
	record, known := rig.known.Get(sessionID)
	if !known {
		t.Fatalf("the clean stop left no known-session record of %s", sessionID)
	}

	stopped := sendThroughRoutes(t, rig.routes, sessionID, map[string]any{"message": "while you were away"})
	if stopped.status != http.StatusOK || stopped.State != sessionStateStopped {
		t.Fatalf("a send to the stopped session: status %d, body %s; want 200 stopped, never 404",
			stopped.status, stopped.raw)
	}
	if stopped.LastSeen != record.LastSeen {
		t.Fatalf("the stopped answer's last_seen = %d, want the record's %d: %s",
			stopped.LastSeen, record.LastSeen, stopped.raw)
	}

	// It comes back under the same id, on a new NATS connection, which is a new epoch: the relay
	// restarts its consumer at the ack floor and hands it what it never acknowledged.
	registerThrough(t, rig.routes, sessionID, resumeRegistration("epoch-2"))
	resumed := plugin.collect(t, 1, 30*time.Second)
	if resumed[0].item.EventID != stopped.EventID {
		t.Fatalf("the resumed session took %s, want the message stored while it was stopped, %s",
			resumed[0].item.EventID, stopped.EventID)
	}

	// Longer than the ack wait, so a frame the relay never acknowledged would be redelivered
	// inside this window.
	plugin.none(t, bus.SessionConsumerAckWait+10*time.Second)
	t.Logf("the stop-and-resume test took %s, of which %s was the watch for a second copy",
		time.Since(started).Round(time.Second), bus.SessionConsumerAckWait+10*time.Second)
}
