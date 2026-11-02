package main

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// A reconnect of the session-consumer connection costs the sessions this listener relays nothing
// but the restart itself. The pull's request is dropped with the connection it was sent on
// (bus.ErrSessionPullReconnected), which says nothing about the session or its consumer, so the
// relay restarts at the session's ack floor at once rather than waiting out the retry backoff its
// run's earlier failures had raised (sessionRelays.relay, relayReconnected).
//
// A run's wait only ever doubles, to relayRetryMax, and a serving-port registration keeps its
// epoch, so the run is never replaced: without this, a run that had met a few failed restarts
// would cost its session 30 s at every later reconnect, which is what the fragment
// pr2404-session-pull-reconnect.yaml and AGENTS.md say it does not.
func TestASessionRelayRestartsAtOnceWhenItsPullsConnectionReconnects(t *testing.T) {
	rig, sessionID, standIn, held, floor := sessionWithAHeldFrame(t, "ses_relay_reconnect_")
	agent := contracts.AgentSubject(sessionID)

	// Three failed restarts raise this run's retry wait: it waits relayRetryFirst, then 2 s, then
	// 4 s, so the wait standing when the reconnect comes is 8 s.
	failed := failCreates(t, rig, sessionID, 3)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-2"))
	for range 3 {
		awaitCreateFailure(t, failed)
	}
	// The fourth restart creates the consumer at the floor it read, and the relay pulls it: the
	// message the session was holding comes again, and the session writes it this time, so the
	// consumer's floor covers it and the restart after the reconnect sends it no third time.
	requireFrames(t, standIn.collect(t, 1, 30*time.Second), held)
	awaitAckState(t, rig.consumers, sessionID, 0, floor+1)

	if err := rig.deps.consumers.consumers.ForceReconnect(); err != nil {
		t.Fatalf("reconnect the session-consumer connection in place: %v", err)
	}
	reconnected := time.Now()
	item, _ := rig.storeEvent(t, agent)
	requireFrames(t, standIn.collect(t, 1, 30*time.Second), item)
	if took := time.Since(reconnected); took > 2*time.Second {
		t.Fatalf("the relay sent the session's next message %s after the reconnect, want it restarted at once rather than after the 8 s its earlier failures had raised", took.Round(time.Millisecond))
	}
}
