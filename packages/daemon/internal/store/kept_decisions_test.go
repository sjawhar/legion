package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// keptRig is a store with the records a kept decision's claim and hold read: issues and their
// trees, outbox rows that claim, and root-architect claims whose deliveries hold.
type keptRig struct {
	t     *testing.T
	ctx   context.Context
	store *Store
	at    time.Time
}

func newKeptRig(t *testing.T) *keptRig {
	t.Helper()
	return &keptRig{t: t, ctx: context.Background(), store: migratedStore(t), at: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)}
}

func (r *keptRig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.store.Pool().Exec(r.ctx, sql, args...); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
}

// issue records key standing in tree.
func (r *keptRig) issue(key, tree string) {
	r.t.Helper()
	r.exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ($1, $2, 'LEGION', $1, 'admitted', 1, 'in_progress', 'A', 1)
		on conflict (key) do update set tree = excluded.tree`, key, tree)
}

// startRow enqueues a root-architect start of tree and returns its id.
func (r *keptRig) startRow(tree string) int64 {
	r.t.Helper()
	row, err := record.NewOutboxRow(tree, record.SuperviseRequest{Op: "start", Tree: tree, Role: claim.RoleArchitect, Generation: 1}, r.at)
	if err != nil {
		r.t.Fatal(err)
	}
	var id int64
	if err := pgx.BeginFunc(r.ctx, r.store.Pool(), func(tx pgx.Tx) error {
		var err error
		id, err = record.NewStore().EnqueueID(r.ctx, tx, row)
		return err
	}); err != nil {
		r.t.Fatalf("enqueue the start of %s: %v", tree, err)
	}
	return id
}

// keep keeps a person's decision ref on issue, claimed by row when row is not zero.
func (r *keptRig) keep(issue, ref string, row int64) {
	r.t.Helper()
	r.exec(`insert into parked_decisions (issue, ref, decision, actor, at) values ($1, $2, 'ask-answered', 'user:alice', $3)`, issue, ref, r.at)
	if row != 0 {
		r.exec(`update parked_decisions set claimed_by = $2 where ref = $1`, ref, row)
	}
}

// architect is the root architect claim of tree, recording rows as the start rows it queued a task from.
func (r *keptRig) architect(tree string, rows ...int64) supervise.Claim {
	r.t.Helper()
	token := claim.Token("legion-legion-" + tree + "-architect")
	c := supervise.Claim{Token: token, Project: "legion", Tree: tree, Issue: tree, Role: claim.RoleArchitect, Generation: 1,
		State: supervise.StateReady, DeliveredRows: rows}
	if err := r.store.PutClaim(r.ctx, c); err != nil {
		r.t.Fatalf("put the architect claim of %s: %v", tree, err)
	}
	return c
}

// task is the delivery of a start task naming ref, a decision on issue.
func (r *keptRig) task(id, issue, ref string) supervise.Delivery {
	r.t.Helper()
	kept, err := json.Marshal(record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: ref, At: r.at, Issue: issue,
		Decision: record.DecisionAskAnswered, Actor: "user:alice"}}})
	if err != nil {
		r.t.Fatal(err)
	}
	return supervise.Delivery{ID: id, Task: "a person decided: " + ref, Generation: 1, QueuedAt: r.at, Kept: kept}
}

// row reads ref's kept row: whether it stands, who claims it and who holds it.
func (r *keptRig) row(ref string) (stands bool, claimedBy int64, heldBy, heldDelivery string) {
	r.t.Helper()
	err := r.store.Pool().QueryRow(r.ctx, `select coalesce(claimed_by, 0), coalesce(held_by_claim, ''), coalesce(held_by_delivery, '')
		from parked_decisions where ref = $1`, ref).Scan(&claimedBy, &heldBy, &heldDelivery)
	if err == pgx.ErrNoRows {
		return false, 0, "", ""
	}
	if err != nil {
		r.t.Fatalf("read the kept row %s: %v", ref, err)
	}
	return true, claimedBy, heldBy, heldDelivery
}

// A confirmation the machine takes back (a prompt refused busy after a turn of the agent's own
// confirmed it, a late refusal) writes the delivery back unconfirmed. The confirmation's write
// told the decision the delivery held and deleted it; the architect never read it, so the write
// back puts it back, held by the delivery again, and its resend names it. A second telling is the
// worst case, never a loss.
func TestADeliveryWrittenBackUnconfirmedHoldsWhatItsConfirmationTold(t *testing.T) {
	r := newKeptRig(t)
	const tree, ref = "LEGION-300", "dispatch://LEGION-300/ask/ask-1"
	r.issue(tree, tree)
	start := r.startRow(tree)
	r.keep(tree, ref, start)
	architect := r.architect(tree, start)
	d := r.task("outbox:1", tree, ref)
	if err := r.store.PutClaimAndDelivery(r.ctx, architect, d); err != nil {
		t.Fatalf("queue the task: %v", err)
	}
	confirmed := d
	confirmed.ConfirmedAt = r.at.Add(time.Minute)
	if err := r.store.PutClaimAndDelivery(r.ctx, architect, confirmed); err != nil {
		t.Fatalf("confirm the task: %v", err)
	}
	if stands, _, _, _ := r.row(ref); stands {
		t.Fatalf("%s stands after its delivery's confirmation, want it told", ref)
	}
	if err := r.store.PutDelivery(r.ctx, architect.Token, d); err != nil {
		t.Fatalf("write the delivery back unconfirmed: %v", err)
	}
	stands, _, heldBy, heldDelivery := r.row(ref)
	if !stands || heldBy != string(architect.Token) || heldDelivery != d.ID {
		t.Fatalf("%s after the confirmation was taken back: stands %t, held by %q/%q; want it standing again, held by %s/%s, "+
			"so the resend names it rather than losing it", ref, stands, heldBy, heldDelivery, architect.Token, d.ID)
	}
}

// A delivery's write takes only the rows it owns: those claimed by a start row whose task this
// claim queued, or already held by this claim. A decision whose issue moved to another tree, whose
// start the other tree's architect now holds it under, is that delivery's: a rewrite of the old
// tree's delivery (a take-back after a death) must not steal it back.
func TestADeliveryRewriteDoesNotTakeARowAnotherTreeHolds(t *testing.T) {
	r := newKeptRig(t)
	const old, moved, ref = "LEGION-310", "LEGION-311", "dispatch://LEGION-311/ask/ask-1"
	r.issue(old, old)
	r.issue(moved, old)
	oldStart := r.startRow(old)
	r.keep(moved, ref, oldStart)
	oldArchitect := r.architect(old, oldStart)
	oldTask := r.task("outbox:1", moved, ref)
	if err := r.store.PutClaimAndDelivery(r.ctx, oldArchitect, oldTask); err != nil {
		t.Fatalf("queue the old tree's task: %v", err)
	}

	// The issue leaves for a tree of its own, whose start names the decision (the old hold is void
	// there) and whose architect holds it.
	r.issue(moved, moved)
	newStart := r.startRow(moved)
	if err := pgx.BeginFunc(r.ctx, r.store.Pool(), func(tx pgx.Tx) error {
		return record.NewStore().ClaimKeptDecisions(r.ctx, tx, moved, record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: ref, At: r.at}}}, newStart)
	}); err != nil {
		t.Fatalf("claim the decision for the new tree's start: %v", err)
	}
	newArchitect := r.architect(moved, newStart)
	newTask := r.task("outbox:2", moved, ref)
	if err := r.store.PutClaimAndDelivery(r.ctx, newArchitect, newTask); err != nil {
		t.Fatalf("queue the new tree's task: %v", err)
	}

	// The old tree's task is taken back under a new id after its process died.
	takenBack := oldTask
	takenBack.ID = "outbox:1-interrupted"
	if err := r.store.PutDelivery(r.ctx, oldArchitect.Token, takenBack); err != nil {
		t.Fatalf("take the old task back: %v", err)
	}
	if _, _, heldBy, heldDelivery := r.row(ref); heldBy != string(newArchitect.Token) || heldDelivery != newTask.ID {
		t.Fatalf("%s is held by %q/%q after the old tree's task was rewritten, want it still held by %s/%s: "+
			"the old delivery owns it no longer", ref, heldBy, heldDelivery, newArchitect.Token, newTask.ID)
	}
}

// A row is claimed or held, never both. A catch-up of the tree a decision's issue moved into
// claims it over the old tree's void hold and clears that hold; the old delivery's retirement then
// releases nothing it does not own, so the new claim still counts and a later hand-over finds it.
func TestAHoldReleaseLeavesAClaimThatCounts(t *testing.T) {
	r := newKeptRig(t)
	const old, moved, ref = "LEGION-320", "LEGION-321", "dispatch://LEGION-321/ask/ask-1"
	r.issue(old, old)
	r.issue(moved, old)
	oldStart := r.startRow(old)
	r.keep(moved, ref, oldStart)
	oldArchitect := r.architect(old, oldStart)
	oldTask := r.task("outbox:1", moved, ref)
	if err := r.store.PutClaimAndDelivery(r.ctx, oldArchitect, oldTask); err != nil {
		t.Fatalf("queue the old tree's task: %v", err)
	}

	r.issue(moved, moved)
	catchUp := r.startRow(moved)
	if err := pgx.BeginFunc(r.ctx, r.store.Pool(), func(tx pgx.Tx) error {
		return record.NewStore().ClaimKeptDecisions(r.ctx, tx, moved, record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: ref, At: r.at}}}, catchUp)
	}); err != nil {
		t.Fatalf("claim the decision for the new tree: %v", err)
	}
	if _, claimedBy, heldBy, _ := r.row(ref); claimedBy != catchUp || heldBy != "" {
		t.Fatalf("%s after the new tree's claim: claimed by %d, held by %q; want claimed by %d and no hold: a row is claimed or held, never both",
			ref, claimedBy, heldBy, catchUp)
	}
	if err := r.store.RetireDelivery(r.ctx, oldArchitect, oldTask.ID); err != nil {
		t.Fatalf("retire the old tree's task: %v", err)
	}
	if _, claimedBy, _, _ := r.row(ref); claimedBy != catchUp {
		t.Fatalf("%s is claimed by %d after the old task's retirement, want %d: a hold release must not clear a claim that counts",
			ref, claimedBy, catchUp)
	}
}
