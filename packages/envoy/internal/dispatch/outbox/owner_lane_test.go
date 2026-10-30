package outbox

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/session"
)

// laterEvent appends another human message on an issue routedEvent already seeded, so that an
// owner's second event commits while its first is still being sent.
func (r *roleRig) laterEvent(t *testing.T, key string, actor model.Actor) model.Event {
	t.Helper()
	return appendEvent(t, r.database, r.broker, model.Event{
		IssueKey: new(key), Type: "message.created", Actor: actor,
		Payload: model.Message{ID: uuid.NewString(), IssueKey: new(key), Body: "the later message"},
	})
}

// An owner is given back as its own sends settle, not when its batch ends: an issue read in the
// same batch as a silent holder's, whose own forward was answered, has its later event - one
// committed while the silent holder's receipt wait is still running - published at once.
//
// TestAHolderThatNeverAnswersDoesNotDelayAnotherIssuesEvents covers two owners in two batches;
// this covers two owners in one, which only a per-owner release reaches. Both issues' events are
// committed before the outbox runs, so its first batch reads both owners.
func TestAnOwnerOfTheSilentHoldersBatchIsReleasedBeforeTheBatchEnds(t *testing.T) {
	const (
		silentRole, silentHolder = "silent-reviewer", "ses_never_answers"
		liveRole, liveHolder     = "live-reviewer", "ses_answers"
		within                   = time.Second
	)
	alice := model.Actor{Kind: "user", ID: "alice"}
	rig := newRoleRig(t)
	rig.claim(t, silentRole, silentHolder, session.SessionEntry{SelfSubscribed: true})
	rig.claim(t, liveRole, liveHolder, session.SessionEntry{SelfSubscribed: true})
	silent := rig.receiver(t, silentHolder, false)
	live := rig.receiver(t, liveHolder, true)
	rig.routedEvent(t, "T-1", silentRole, alice)
	rig.routedEvent(t, "T-2", liveRole, alice)

	deps := rig.deps()
	deps.Listen = rig.database.Pool.Listener
	stop := runDeps(t, deps, 15*time.Second)
	defer stop()

	select {
	case <-silent.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the silent holder never received its forward")
	}
	select {
	case <-live.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the answering holder never received its issue's first forward")
	}

	// The silent holder's receipt wait is running, and every send of the other owner of its batch
	// has been answered. That owner's next event is committed in it.
	time.Sleep(200 * time.Millisecond)
	committed := time.Now()
	rig.laterEvent(t, "T-2", alice)
	select {
	case <-live.arrived:
		if waited := time.Since(committed); waited > within {
			t.Fatalf("the answering holder received its issue's later forward %s after its commit, want within %s: "+
				"the owner was held in flight until its batch ended", waited.Round(time.Millisecond), within)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the answering holder never received its issue's later forward")
	}
}

// One owner's order holds across the batches running side by side: while its first event waits on
// a holder that never answers, the batch keeps that owner in flight, so no batch beside it reads
// the owner's second event and the holder receives the first forward alone.
func TestAnOwnersSecondEventDoesNotOvertakeItsFirstWhileItsHolderIsSilent(t *testing.T) {
	const silentRole, silentHolder = "silent-reviewer", "ses_never_answers"
	alice := model.Actor{Kind: "user", ID: "alice"}
	rig := newRoleRig(t)
	rig.claim(t, silentRole, silentHolder, session.SessionEntry{SelfSubscribed: true})
	silent := rig.receiver(t, silentHolder, false)

	deps := rig.deps()
	deps.Listen = rig.database.Pool.Listener
	stop := runDeps(t, deps, 15*time.Second)
	defer stop()

	first := rig.routedEvent(t, "T-1", silentRole, alice)
	var forward contracts.Envelope
	select {
	case forward = <-silent.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the silent holder never received its forward")
	}
	if forward.SourceEventID != fmt.Sprint(first.ID) {
		t.Fatalf("the silent holder received event %s first, want event %d", forward.SourceEventID, first.ID)
	}

	// Committed on the same owner while the first event's receipt wait runs, and well inside
	// roleReceiptTimeout, so the first event has not been settled when the wait below ends.
	time.Sleep(200 * time.Millisecond)
	second := rig.laterEvent(t, "T-1", alice)
	select {
	case next := <-silent.arrived:
		t.Fatalf("the silent holder received event %s (its issue's second is %d) while event %d had not reached it",
			next.SourceEventID, second.ID, first.ID)
	case <-time.After(2 * time.Second):
	}
}
