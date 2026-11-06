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

// A person's decision on a child, kept while the old root lingered with nobody in its tree to hear
// it, goes with that child when the linger hands it over into a tree of its own: the row stands
// under the child's own key, so its new architect's start task names it with nothing moved, and
// the old root keeps what was decided on the rest of its tree. A decision made on a member the
// hand-over leaves behind — the grandchild here, which the record still shows in the old tree —
// is that tree's to answer until it leaves too, each member of a closing tree being handed over
// under a fence of its own (workflow's unfinishedHandOvers).
func TestAHandedOverChildTakesTheDecisionsKeptOnIt(t *testing.T) {
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
	if !strings.Contains(task, childAsk) {
		t.Fatalf("the start task of %s = %q, want it to name the answer on %s, which a person made on it",
			child, task, childAsk)
	}
	for _, elsewhere := range []string{rootAsk, grandAsk} {
		if strings.Contains(task, elsewhere) {
			t.Fatalf("the start task of %s = %q, want nothing of %s, which a person decided on an issue of the tree it left",
				child, task, elsewhere)
		}
	}
	if left := keptRefs(t, pool, old); len(left) != 2 || left[0] != rootAsk || left[1] != grandAsk {
		t.Fatalf("%s keeps %q after the hand-over, want %s and %s: what was decided on the child went with it, and what was "+
			"decided on the members it left behind did not", old, left, rootAsk, grandAsk)
	}
}

// A decision the old root's architect was already given — a start named it and the claim took the
// task — but has never been sent, because its root has had no architect to hear it since
// (supervise's Deps.RootHears), is owed to the child the linger hands over. The row stands where
// a person's decision always stands, under the issue it was made on, held by that task's delivery;
// the hold counts only while the holding claim's tree is the row's issue's tree, so it counts for
// nothing the moment the child becomes a tree of its own, and the child's start names it. If the
// old architect is handed that task after all, it is told a decision of a tree it no longer holds,
// which is the lesser fault; what the task names of the tree it still holds stays held.
func TestAHandedOverChildTakesWhatTheOldArchitectsHeldTaskHolds(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	const old, child = "LEGION-208", "LEGION-209"
	seedLingeringHandovers(t, pool, old, child)
	childAsk, rootAsk := "dispatch://LEGION-209/ask/ask-1", "dispatch://LEGION-208/ask/ask-3"
	// The old root's architect holds a start task that names both decisions, unsent and
	// unconfirmed, and the rows it names are held by that delivery.
	seedHeldArchitectTask(t, pool, old, record.KeptDecisions{Decisions: []record.DecisionMark{
		{Ref: childAsk, At: fixedNow, Issue: child, Decision: record.DecisionAskAnswered, Actor: "user:alice"},
		{Ref: rootAsk, At: fixedNow, Issue: old, Decision: record.DecisionAskResolved, Actor: "user:bob"},
	}})

	applyMirrored(t, pool, admission, "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets",
		Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: old, Ancestry: []string{old}, Rank: "B",
		HandedOver: handed, DaemonActor: true}, admission.engine)

	task := architectStartTask(t, pool, child)
	if !strings.Contains(task, childAsk) {
		t.Fatalf("the start task of %s = %q, want it to name %s, which the old architect's held task still holds",
			child, task, childAsk)
	}
	if strings.Contains(task, rootAsk) {
		t.Fatalf("the start task of %s = %q, want nothing of %s, which a person decided on the tree it left", child, task, rootAsk)
	}
	if left := keptRefs(t, pool, old); len(left) != 1 || left[0] != rootAsk {
		t.Fatalf("%s keeps %q after the hand-over, want %s alone: its architect's held task still holds what was decided on it",
			old, left, rootAsk)
	}
}

// seedHeldArchitectTask records root's architect ready with a start task it has been given and
// nobody has sent: unconfirmed, naming kept, as a task held while its root has no architect to hear
// it stands (supervise's sendPending), with each row it names held by that delivery, as the write
// that queued it left them (the claim store's putDelivery).
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
	for _, named := range kept.Decisions {
		if _, err := pool.Exec(context.Background(), `insert into parked_decisions (issue, ref, decision, actor, at,
			held_by_claim, held_by_delivery) values ($1, $2, $3, $4, $5, $6, 'outbox:7')`,
			named.Issue, named.Ref, string(named.Decision), named.Actor, named.At, string(token)); err != nil {
			t.Fatalf("hold the decision %s on the architect's task of %s: %v", named.Ref, root, err)
		}
	}
}
