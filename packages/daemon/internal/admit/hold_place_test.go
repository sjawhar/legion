package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// heldLine is the waiting line both hold cases stand in, under a cap of one slot so that the two
// candidates in it compete for the same slot and the line's order alone decides which gets it:
//
//   - LEGION-9, a root that never ran, ranked at the front of the board;
//   - LEGION-2, a released child of the running tree slotTree seeds, whose own first phase can
//     start and whose tree root ranks behind LEGION-9.
//
// The two belong to different trees on purpose: the pass breaks at a held candidate for every
// candidate behind it, not only for the rest of that candidate's own tree.
func heldLine(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	slotTree(t, pool, true)
	parent := "LEGION-1"
	putIssue(t, pool, record.Issue{Key: "LEGION-2", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: "LEGION-2", Tree: "LEGION-1", Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "B"})
	seedWaiting(t, pool, "LEGION-9", "0")
	assertWaiting(t, pool, []string{"LEGION-9", "LEGION-2"})
}

// heldSummary is the listing snapshot Reconcile defers for LEGION-9 when the Dispatch consumer
// is behind: what release applies for it before anything else reads the line.
func heldSummary() intake.DispatchIssue {
	return intake.DispatchIssue{Key: "LEGION-9", ResolvedRepo: "acme/widgets", Title: "LEGION-9", Status: "todo", Rank: "0", HandedOver: true, Seq: 20}
}

// reachConsumer applies the Dispatch consumer's position at the target Reconcile captured, which
// is what releases every key it deferred (Admission.release).
func reachConsumer(t *testing.T, pool *pgxpool.Pool, admission *Admission, eventID string) {
	t.Helper()
	position := intake.DispatchConsumerPosition{AckFloorStream: admission.Target(), Idle: true}
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", eventID, position, admission); err != nil {
		t.Fatalf("apply the consumer position %s: %v", eventID, err)
	}
}

// dispatch://INFRA-1067's rule, read where only the issue-level cap can put it: the candidate
// behind the held one is a child of another tree. TestAHeldRootKeepsItsPlaceAndNothingBehindIt
// TakesTheSlot proves the rule over two roots, which is the case that existed before a child
// could hold a slot; this one proves that the stop is positional over the whole line and not
// bounded by the held candidate's own tree.
//
// A hold says the pass does not yet know enough to decide the candidate it stops at, so the pass
// breaks there and nothing behind that candidate takes a slot — whatever tree the candidate
// belongs to, and whatever tree the candidates behind it belong to. The held candidate keeps its
// place in the line, and its own release is what admits it.
//
// That is what separates a hold from a decided skip. A skip — an answered root that may not
// resume, a root a Dispatch link holds, a tree whose cleanup is reserved, a tree another
// authority holds — is a judgement the pass has made, so the line moves on and the next pass
// makes it again. A hold is the absence of a judgement: passing the candidate over would hand
// its slot to an issue ranked below it, and a slot is never taken back, so the inversion would
// be permanent. Both holds are read through the one predicate promoteHolds builds, so neither
// can drift from the other.
//
// The two releases are not the same release, and the third case is why the difference matters:
// release's own promotion pass runs with respectHolds false, which makes the predicate the boot
// catch-up's hold alone. So the Dispatch consumer catching up bypasses its own hold and never
// the catch-up's, and a candidate under both is still held after the first release.
func TestAHeldCandidateStopsTheLineForAChildOfAnotherTree(t *testing.T) {
	for _, tc := range []struct {
		name string
		// hold puts LEGION-9 under the case's hold, before the first pass.
		hold func(t *testing.T, admission *Admission)
		// release ends it. In the both-holds case it ends the first of the two and asserts the
		// state between the two releases, then ends the second.
		release func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
	}{
		{
			name: "the Dispatch consumer's hold",
			hold: func(_ *testing.T, admission *Admission) {
				admission.pending = map[string]intake.DispatchIssue{"LEGION-9": heldSummary()}
				admission.target = 100
			},
			release: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				reachConsumer(t, pool, admission, "position-1")
			},
		},
		{
			name: "the boot catch-up's hold",
			hold: func(_ *testing.T, admission *Admission) {
				admission.HoldForCatchUp("LEGION-9")
			},
			release: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				admission.ReleaseCatchUp("LEGION-9")
				tick(t, pool, admission, "tick-released")
			},
		},
		{
			name: "both holds",
			hold: func(_ *testing.T, admission *Admission) {
				admission.pending = map[string]intake.DispatchIssue{"LEGION-9": heldSummary()}
				admission.target = 100
				admission.HoldForCatchUp("LEGION-9")
			},
			release: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				reachConsumer(t, pool, admission, "position-1")
				// Between the two releases: the consumer has caught up, so release applied
				// LEGION-9's deferred summary and promoted with respectHolds false — and the
				// catch-up's hold, the one that survives that call, still stops the pass where
				// it stands.
				if holders := slots(t, pool); len(holders) != 0 {
					t.Fatalf("slots between the two releases = %#v, want none: the catch-up's hold is the one release never bypasses", holders)
				}
				assertWaiting(t, pool, []string{"LEGION-9", "LEGION-2"})
				admission.ReleaseCatchUp("LEGION-9")
				tick(t, pool, admission, "tick-released")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			heldLine(t, pool)
			admission := newAdmission(t, 1, nil)
			tc.hold(t, admission)

			tick(t, pool, admission, "tick-held")
			if holders := slots(t, pool); len(holders) != 0 {
				t.Fatalf("slots under the hold = %#v, want none: a hold stops the pass, so the child behind the held root takes nothing", holders)
			}
			assertWaiting(t, pool, []string{"LEGION-9", "LEGION-2"})

			tc.release(t, pool, admission)
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-9", Index: 0, AdmittedAt: fixedNow}})
			assertWaiting(t, pool, []string{"LEGION-2"})
		})
	}
}
