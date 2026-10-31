package outbox

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/session"
)

// A role holder that takes its forward and never sends the receipt holds back only its own
// chain: another issue's event, routed to a holder that answers, reaches that holder at once
// rather than when the silent holder's receipt wait runs out.
//
// A session that died holding a role stays registered and fresh for up to
// session.ClaimStaleAfter, and each retry of its issue's event waits the whole roleReceiptTimeout
// for a receipt that never comes. If no other send can start while that wait runs, every event
// Dispatch commits during it - on every issue, to every session - reaches its reader up to 5 s
// late, and again at each retry of the stuck event, until the holder goes stale. The tester's
// live run at fd93819c measured that: with two issues routed to a holder
// that never answers, 24 of 30 comments on a third issue reached their answering holder more
// than 1 s late (median 2.8 s, max 5.0 s), against 3 of 30 on main's build.
func TestAHolderThatNeverAnswersDoesNotDelayAnotherIssuesEvents(t *testing.T) {
	const (
		silentRole, silentHolder = "silent-reviewer", "ses_never_answers"
		liveRole, liveHolder     = "live-reviewer", "ses_answers"
		// Well under roleReceiptTimeout, so a send that waited for the silent holder's receipt
		// fails it, and well over a send's own round trip on a local server.
		within = time.Second
	)
	rig := newRoleRig(t)
	rig.claim(t, silentRole, silentHolder, session.SessionEntry{SelfSubscribed: true})
	rig.claim(t, liveRole, liveHolder, session.SessionEntry{SelfSubscribed: true})
	silent := rig.receiver(t, silentHolder, false)
	live := rig.receiver(t, liveHolder, true)

	deps := rig.deps()
	deps.Listen = rig.database.Pool.Listener
	stop := runDeps(t, deps, 15*time.Second)
	defer stop()

	rig.routedEvent(t, "T-1", silentRole, model.Actor{Kind: "user", ID: "alice"})
	select {
	case <-silent.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the silent holder never received its forward")
	}

	// The silent holder's receipt wait is now running. Another issue's event is committed in it.
	time.Sleep(200 * time.Millisecond)
	committed := time.Now()
	rig.routedEvent(t, "T-2", liveRole, model.Actor{Kind: "user", ID: "alice"})
	select {
	case <-live.arrived:
		if waited := time.Since(committed); waited > within {
			t.Fatalf("the answering holder received T-2's forward %s after its commit, want within %s: "+
				"the silent holder's receipt wait held back another issue's send", waited.Round(time.Millisecond), within)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the answering holder never received T-2's forward")
	}
}
