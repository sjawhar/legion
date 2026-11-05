package admit

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The reviewer's probe of round 4, driven through a real admission pass rather than through
// decideAwait alone: a root re-admitted and never promoted holds no slot, and its architect of
// the generation before is live until the linger's suspend runs, so a hand-back of that
// architect's makes the mark non-zero again. Such a root must not enter awaiting_person — the
// wait is a slot going back, and this root has none to give — and it must not be taken out of
// the workflow either, since it is waiting in the line for its first slot.
//
// What the pass must leave: the root at admitted, with no wait recorded, its status untouched,
// no slot, and its place in the line.
func TestAReadmittedRootWithAHandBackNeitherWaitsOnAPersonNorLeavesTheWorkflow(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	const root, running = "LEGION-100", "LEGION-RUN"

	// The cap's one slot is taken, so the queued root is not promoted in this pass.
	seedSlotted(t, pool, running, "A")

	// The re-admitted root: a new generation at admitted, todo, with no slot row, no tree
	// lifecycle and no gate, and a hand-back its old architect declared.
	putIssue(t, pool, record.Issue{Key: root, Tree: root, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: root, Phase: phase.Admitted,
		Generation: 2, Status: "todo", DispatchStatus: "todo", Rank: "B", HandedOver: true,
		DeclaredSeq: 9, DeclaredAt: fixedNow.Add(-time.Hour)})
	seedMirror(t, pool, root, "todo")
	mirrorAsk(t, pool, record.RootAsk{Issue: root, Ask: "ask-1", Kind: record.AskQuestion, Open: true,
		Seq: 9, WaitingOn: "human", WaitingOnSeq: 9, ByTree: true})

	tick(t, pool, admission, "tick")

	got := issue(t, pool, root)
	if got.Phase != phase.Admitted {
		t.Fatalf("%s stands at %s after the pass, want admitted: a root that was never promoted holds no slot to hand back and leaves the workflow for nothing", root, got.Phase)
	}
	if got.Awaiting != nil {
		t.Fatalf("%s recorded a wait on a person: %+v. The wait is its slot going back, and it holds none", root, got.Awaiting)
	}
	if got.Status != "todo" {
		t.Fatalf("%s reads %q after the pass, want todo: nothing of this pass moves a root waiting in the line", root, got.Status)
	}
	for _, slot := range slots(t, pool) {
		if slot.Issue == root {
			t.Fatalf("%s holds a slot after the pass: the cap's one slot is %s's", root, running)
		}
	}
	assertWaiting(t, pool, []string{root})
}
