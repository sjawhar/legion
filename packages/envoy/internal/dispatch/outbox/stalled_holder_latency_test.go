package outbox

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/session"
)

// A role forward the stream never acknowledges holds back only its own chain: another issue's
// event, routed to a holder whose forward the stream does store, reaches that holder at once
// rather than when the stalled forward's wait runs out.
//
// A forward counts as reached on the stream's acknowledgement of the copy it stored, and each
// retry of an event whose copy the stream does not store waits the whole roleForwardAckTimeout
// for one. If no other send can start while that wait runs, every event Dispatch commits during
// it - on every issue, to every session - reaches its reader up to 5 s late, and again at each
// retry of the stuck event. The tester's live run at fd93819c measured that against the receipt
// wait this bound replaces: with two issues routed to a holder that never answered, 24 of 30
// comments on a third issue reached their holder more than 1 s late (median 2.8 s, max 5.0 s),
// against 3 of 30 on main's build.
func TestAForwardTheStreamDoesNotAcknowledgeDoesNotDelayAnotherIssuesEvents(t *testing.T) {
	const (
		stalledRole, stalledHolder = "stalled-reviewer", "ses_never_acknowledged"
		liveRole, liveHolder       = "live-reviewer", "ses_acknowledged"
		// Well under roleForwardAckTimeout, so a send that waited for the stalled holder's
		// acknowledgement fails it, and well over a send's own round trip on a local server.
		within = time.Second
	)
	rig := newRoleRig(t)
	rig.claim(t, stalledRole, stalledHolder, session.SessionEntry{SelfSubscribed: true})
	rig.claim(t, liveRole, liveHolder, session.SessionEntry{SelfSubscribed: true})
	// The stream stores and acknowledges the live holder's forwards alone, so the stalled
	// holder's wait out their whole roleForwardAckTimeout.
	rig.narrowAgentCapture(t, liveHolder)
	silent := rig.receiver(t, stalledHolder)
	live := rig.receiver(t, liveHolder)

	deps := rig.deps()
	deps.Listen = rig.database.Pool.Listener
	stop := runDeps(t, deps, 15*time.Second)
	defer stop()

	rig.routedEvent(t, "T-1", stalledRole, model.Actor{Kind: "user", ID: "alice"})
	select {
	case <-silent.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled holder never received its forward")
	}

	// That forward's wait for an acknowledgement is now running. Another issue's event is
	// committed in it.
	time.Sleep(200 * time.Millisecond)
	committed := time.Now()
	rig.routedEvent(t, "T-2", liveRole, model.Actor{Kind: "user", ID: "alice"})
	select {
	case <-live.arrived:
		if waited := time.Since(committed); waited > within {
			t.Fatalf("the live holder received T-2's forward %s after its commit, want within %s: "+
				"the stalled forward's wait held back another issue's send", waited.Round(time.Millisecond), within)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the live holder never received T-2's forward")
	}
}
