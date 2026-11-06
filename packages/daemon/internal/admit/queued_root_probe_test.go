package admit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The reviewer's probe of round 4, driven through a real admission pass rather than through
// decideAwait alone: a root re-admitted and never promoted holds no slot, and the architect of
// the generation before it handed a question back in is still on the record until the linger's
// suspend runs, so the hand-back makes the mark non-zero again. Such a root must not enter
// awaiting_person — the wait is a slot going back, and this root has none to give — and it must
// not be taken out of the workflow either, since it is waiting in the line for its first slot.
//
// The architect's claim is seeded as the old generation leaves it — suspended, holding the task
// it took and confirmed — so that every later rule of decideAwait is satisfied: its architect
// owes it nothing (rule 5 reads owed, which a claim that has taken its task answers no for), its
// tree has no other member, and it could resume. Rule 0 is then the only rule standing between
// this root and the wait, which is what this test is for.
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
	// lifecycle and no gate, and a hand-back its old architect declared. The hand-back is
	// written through record.Store.Declare, which owns declared_seq and declared_at: PutIssue
	// writes neither, so a root seeded with them alone carries no hand-back at all and
	// ReconcileAwaiting never reads it.
	putIssue(t, pool, record.Issue{Key: root, Tree: root, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: root, Phase: phase.Admitted,
		Generation: 2, Status: "todo", DispatchStatus: "todo", Rank: "B", HandedOver: true})
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().Declare(context.Background(), tx, root, record.EventMark{Seq: 9, At: fixedNow.Add(-time.Hour)}); err != nil {
			t.Fatalf("declare the hand-back of %s: %v", root, err)
		}
	})
	seedMirror(t, pool, root, "todo")
	mirrorAsk(t, pool, record.RootAsk{Issue: root, Ask: "ask-1", Kind: record.AskQuestion, Open: true,
		Seq: 9, WaitingOn: "human", WaitingOnSeq: 9, ByTree: true})
	seedSuspendedArchitect(t, pool, root, 1)

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

// seedSuspendedArchitect records the architect claim of root's generation as the close of that
// generation leaves it: suspended by the daemon, holding the task it was given and confirmed,
// which is the one state in which its root owes it nothing (workflow's owed, decideAwait's rule
// 5). The claim is keyed by project, issue and role alone, so the generation names the task's
// run rather than the row.
func seedSuspendedArchitect(t *testing.T, pool *pgxpool.Pool, root string, generation uint64) {
	t.Helper()
	token := awaitingArchitect(t, root)
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, suspended_by, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $2, $3, $3, 'architect', $4, 'ses_architect', '/sessions/ses_architect.jsonl', 'suspended', 'daemon', 0, 0, 0, 0)`,
		string(token), testProject, root, int64(generation)); err != nil {
		t.Fatalf("record the suspended architect claim of %s: %v", root, err)
	}
	if _, err := pool.Exec(context.Background(), `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at,
		generation, phase, confirmed_at) values ($1, 'outbox:7', 'Decompose it.', $2, $3, 'admitted', $2)`,
		string(token), fixedNow, int64(generation)); err != nil {
		t.Fatalf("record the task the architect of %s took: %v", root, err)
	}
}
