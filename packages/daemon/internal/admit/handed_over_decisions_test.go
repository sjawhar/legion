package admit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// keepParkedDecision keeps one person's decision on an ask of issue, under the root of its tree,
// by the one rule both paths that find such a decision follow (record.RelayParkedDecision).
func keepParkedDecision(t *testing.T, pool *pgxpool.Pool, root, issue, ref string) {
	t.Helper()
	ctx := context.Background()
	records := record.NewStore()
	inTx(t, pool, func(tx pgx.Tx) {
		stored, err := records.Issue(ctx, tx, root)
		if err != nil || stored == nil {
			t.Fatalf("read the root %s: %v", root, err)
		}
		outcome, err := record.RelayParkedDecision(ctx, tx, records, issue, *stored, true,
			record.ParkedDecision{Ref: ref, Decision: record.DecisionAskAnswered, Actor: "user:alice", At: fixedNow}, 0, "")
		if err != nil {
			t.Fatalf("keep the decision %s: %v", ref, err)
		}
		if outcome != record.RelaySent && outcome != record.RelayKept {
			t.Fatalf("the decision %s on %s was not kept: outcome %d", ref, issue, outcome)
		}
	})
}

// architectStartTask is the task of the one architect start queued for key.
func architectStartTask(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	var tasks []string
	for _, queued := range effects(t, pool) {
		request, ok := queued.payload.(record.SuperviseRequest)
		if ok && queued.issue == key && request.Op == "start" && request.Role == claim.RoleArchitect {
			tasks = append(tasks, request.Task)
		}
	}
	if len(tasks) != 1 {
		t.Fatalf("%d architect starts queued for %s, want the one its admission writes", len(tasks), key)
	}
	return tasks[0]
}

// keptRefs is every decision reference root keeps, by reference.
func keptRefs(t *testing.T, pool *pgxpool.Pool, root string) []string {
	t.Helper()
	ctx := context.Background()
	var refs []string
	inTx(t, pool, func(tx pgx.Tx) {
		kept, err := record.NewStore().ParkedDecisions(ctx, tx, root)
		if err != nil {
			t.Fatalf("read the decisions kept for %s: %v", root, err)
		}
		for _, decision := range kept {
			refs = append(refs, decision.Ref)
		}
	})
	return refs
}

// A person's decisions on a child's own subtree, kept while the old root lingered with nobody in
// its tree to hear them, go with that child when the linger hands it over into a tree of its own:
// its new architect's start task names them, and the old root keeps only what was decided on the
// rest of its tree. Left behind, they would wait under a root nobody runs again and the child's
// architect would never hear them.
func TestAHandedOverChildTakesTheDecisionsKeptOnItsSubtree(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	const old, child, grandchild = "LEGION-208", "LEGION-209", "LEGION-210"
	seedLingeringHandovers(t, pool, old, child)
	parent := child
	putIssue(t, pool, record.Issue{Key: grandchild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: old, Project: testProject, Title: grandchild, Parent: &parent, Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", Rank: "C", LastDispatchSeq: 1})
	childAsk, grandAsk, rootAsk := "dispatch://LEGION-209/ask/ask-1", "dispatch://LEGION-210/ask/ask-2", "dispatch://LEGION-208/ask/ask-3"
	keepParkedDecision(t, pool, old, child, childAsk)
	keepParkedDecision(t, pool, old, grandchild, grandAsk)
	keepParkedDecision(t, pool, old, old, rootAsk)

	// The linger's end hands the child over, which admission re-admits as an orphan carrying its run
	// and promotes into a tree of its own, writing its architect's start.
	applyMirrored(t, pool, admission, "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets",
		Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: old, Ancestry: []string{old}, Rank: "B",
		HandedOver: handed, DaemonActor: true}, admission.engine)
	if got := issue(t, pool, child); got.Tree != child {
		t.Fatalf("%s = %#v, want a tree of its own", child, got)
	}

	task := architectStartTask(t, pool, child)
	if !strings.Contains(task, childAsk) || !strings.Contains(task, grandAsk) {
		t.Fatalf("the start task of %s = %q, want it to name the answers on %s and %s, which a person made on its own subtree",
			child, task, childAsk, grandAsk)
	}
	if strings.Contains(task, rootAsk) {
		t.Fatalf("the start task of %s = %q, want nothing of %s, which a person decided on the tree it left",
			child, task, rootAsk)
	}
	if left := keptRefs(t, pool, old); len(left) != 1 || left[0] != rootAsk {
		t.Fatalf("%s keeps %q after the hand-over, want %s alone: what was decided on the child's subtree went with it",
			old, left, rootAsk)
	}
}

// A decision the old root's architect was already given — a start named it, the claim took the task
// and the rows were forgotten then — but has never been sent, because its root has had no architect
// to hear it since (supervise's Deps.RootHears), is kept again for the child the linger hands over:
// the task holds the only record of it (supervise.Delivery.Kept), and nothing of that task reaches
// the child's own architect. The child's start names it; if the old architect is handed that task
// after all, it is told a decision of a tree it no longer holds, which is the lesser fault.
func TestAHandedOverChildTakesWhatTheOldArchitectsHeldTaskNames(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	const old, child = "LEGION-208", "LEGION-209"
	seedLingeringHandovers(t, pool, old, child)
	childAsk, rootAsk := "dispatch://LEGION-209/ask/ask-1", "dispatch://LEGION-208/ask/ask-3"
	// The old root's architect holds a start task that names both decisions, unsent and
	// unconfirmed: the rows themselves are gone, forgotten when the claim took the task.
	seedHeldArchitectTask(t, pool, old, record.KeptDecisions{Decisions: []record.DecisionMark{
		{Ref: childAsk, At: fixedNow, Issue: child, Decision: record.DecisionAskAnswered, Actor: "user:alice"},
		{Ref: rootAsk, At: fixedNow, Issue: old, Decision: record.DecisionAskResolved, Actor: "user:bob"},
	}})

	applyMirrored(t, pool, admission, "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets",
		Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: old, Ancestry: []string{old}, Rank: "B",
		HandedOver: handed, DaemonActor: true}, admission.engine)

	task := architectStartTask(t, pool, child)
	if !strings.Contains(task, childAsk) {
		t.Fatalf("the start task of %s = %q, want it to name %s, which the old architect's held task is the only record of",
			child, task, childAsk)
	}
	if strings.Contains(task, rootAsk) {
		t.Fatalf("the start task of %s = %q, want nothing of %s, which a person decided on the tree it left", child, task, rootAsk)
	}
	if left := keptRefs(t, pool, old); len(left) != 0 {
		t.Fatalf("%s keeps %q after the hand-over, want none: the held task is still the old architect's own record", old, left)
	}
}

// seedHeldArchitectTask records root's architect ready with a start task it has been given and
// nobody has sent: unconfirmed, naming kept, as a task held while its root has no architect to hear
// it stands (supervise's sendPending).
func seedHeldArchitectTask(t *testing.T, pool *pgxpool.Pool, root string, kept record.KeptDecisions) {
	t.Helper()
	token := awaitingArchitect(t, root)
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, delivered_rows)
		values ($1, $2, $3, $3, 'architect', 1, 'ses_architect', '', 'ready', 0, 0, 0, 0, '{7}')`,
		string(token), testProject, root); err != nil {
		t.Fatalf("record the architect claim of %s: %v", root, err)
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		t.Fatalf("encode the decisions the task names: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `insert into pending_task_deliveries (claim_token, delivery_id, task,
		queued_at, generation, phase, kept_decisions)
		values ($1, 'outbox:7', 'While nobody in the tree could hear it, a person decided on the tree.', now(), 1, '', $2::jsonb)`,
		string(token), encoded); err != nil {
		t.Fatalf("record the architect's held task of %s: %v", root, err)
	}
}
