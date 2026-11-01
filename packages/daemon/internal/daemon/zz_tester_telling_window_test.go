package daemon

// THROWAWAY tester tests (INFRA-251 round 15): the telling side's validity rule, with the issue
// handed over inside each window between composing what a teller names and telling it.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

type testerRowState struct {
	exists            bool
	claimedBy         int64
	heldBy, heldDeliv string
}

func testerRow(t *testing.T, r *heldTaskRig, ref string) testerRowState {
	t.Helper()
	var s testerRowState
	err := r.pool.QueryRow(r.ctx, `select coalesce(claimed_by, 0), coalesce(held_by_claim, ''), coalesce(held_by_delivery, '')
		from parked_decisions where ref = $1`, ref).Scan(&s.claimedBy, &s.heldBy, &s.heldDeliv)
	if err == pgx.ErrNoRows {
		return s
	}
	if err != nil {
		t.Fatalf("read the row of %s: %v", ref, err)
	}
	s.exists = true
	return s
}

// testerChildStarts is every architect start row of the child, with its task.
func testerChildStarts(t *testing.T, r *heldTaskRig) map[int64]string {
	t.Helper()
	rows, err := r.pool.Query(r.ctx, `select id, coalesce(payload->>'task', '') from outbox
		where kind = 'supervise' and issue = $1 and payload->>'op' = 'start' and payload->>'role' = 'architect' order by id`, heldTaskChild)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var task string
		if err := rows.Scan(&id, &task); err != nil {
			t.Fatal(err)
		}
		out[id] = task
	}
	return out
}

func testerNaming(m map[int64]string, ref string) (n int, id int64) {
	for k, v := range m {
		if strings.Contains(v, ref) {
			n++
			id = k
		}
	}
	return
}

// testerChildHears runs the child's architect start row with a connection registered for the
// child's architect, brings that agent up, starts its turn, and returns its prompts.
func testerChildHears(t *testing.T, r *heldTaskRig, id int64) []string {
	t.Helper()
	token := mustClaimToken(t, heldTaskChild, claim.RoleArchitect)
	conn := fake.NewConn()
	r.sup.deps.Conns.(*fake.Conns).Register(token, conn)
	r.runRow(id)
	machine, found := r.sup.Machine(token)
	if !found {
		t.Fatalf("the child's start row %d made no architect claim", id)
	}
	machine.Wait()
	launch := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: launch},
		supervise.RequestRegister{Claim: token, Generation: launch, Session: "ses-child", SessionFile: "/tmp/child.jsonl"},
		supervise.RequestReady{Claim: token, Generation: launch, Session: "ses-child"},
	} {
		if err := machine.Handle(r.ctx, ev); err != nil {
			t.Fatalf("handle %T for the child's architect: %v", ev, err)
		}
		machine.Wait()
	}
	if err := machine.Handle(r.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
		t.Fatalf("start the child's turn: %v", err)
	}
	machine.Wait()
	var out []string
	for _, p := range conn.Prompts() {
		out = append(out, p.Message)
	}
	return out
}

func testerCount(prompts []string, ref string) int {
	n := 0
	for _, p := range prompts {
		n += strings.Count(p, ref)
	}
	return n
}

// A catch-up of the root names a decision on the child; while it is publishing, the root's linger
// hands the child over. The new tree's architect start must name it, and the old catch-up's
// forget must neither delete the row nor wipe the new start's claim.
func TestTesterHandOverBetweenCatchUpComposeAndPublish(t *testing.T) {
	r := newHeldTaskRig(t)
	ref := "dispatch://" + heldTaskChild + "/ask/ask-c"
	keepDecision(t, r.pool, r.records, heldTaskChild, ref, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	machine, _ := r.sup.Machine(r.token)
	now := time.Now()
	keptCatchUp(t, r.pool, r.records, heldTaskRoot, machine.Claim().Generation, now)
	leased, payload := leaseCatchUp(t, r.pool, r.records, heldTaskRoot, now)
	if payload.CatchUp.Kept == nil || !strings.Contains(payload.CatchUp.Decided, ref) {
		t.Fatalf("the leased catch-up = %+v, want it to name %s", payload.CatchUp, ref)
	}
	t.Logf("composed: row %+v", testerRow(t, r, ref))
	publisher := &hookPublisher{hook: func() error {
		r.linger()
		r.handOver()
		t.Logf("inside the publish, after the hand-over: row %+v; child starts %v", testerRow(t, r, ref), testerChildStarts(t, r))
		return nil
	}}
	r.runner.notices = publisher
	if err := r.runner.notice(r.ctx, leased, payload); err != nil {
		t.Fatalf("run the publishing catch-up: %v", err)
	}
	publisher.ranHook(t)
	after := testerRow(t, r, ref)
	starts := testerChildStarts(t, r)
	n, id := testerNaming(starts, ref)
	t.Logf("after the publish settled: row %+v; child starts naming it: %d (row %d)", after, n, id)
	if !after.exists {
		t.Fatalf("the decision on the handed-over child was deleted by the old tree's catch-up")
	}
	if n != 1 {
		t.Fatalf("%d architect starts of the child name %s, want 1: %v", n, ref, starts)
	}
	if after.claimedBy != id {
		t.Errorf("the row is claimed by %d after the old catch-up settled, want the child's start row %d", after.claimedBy, id)
	}
	prompts := testerChildHears(t, r, id)
	final := testerRow(t, r, ref)
	t.Logf("child's architect prompts: %q; row after its turn started: %+v", prompts, final)
	if testerCount(prompts, ref) != 1 || final.exists {
		t.Fatalf("the child's architect was told %s %d times and the row exists=%v; want once and gone", ref, testerCount(prompts, ref), final.exists)
	}
}

// The root's start task names a decision on the child and is delivered to its running architect,
// unconfirmed; the root's linger hands the child over; then the turn starts. The child's start
// must name it, and the old confirmation must not delete it nor wipe the child's start's claim.
func TestTesterHandOverBetweenHeldTaskQueueAndConfirmation(t *testing.T) {
	r := newHeldTaskRig(t)
	ref := "dispatch://" + heldTaskChild + "/ask/ask-c"
	keepDecision(t, r.pool, r.records, heldTaskChild, ref, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	r.runRow(r.enqueueStart())
	pending := r.pending()
	if pending == nil || !strings.Contains(pending.Task, ref) {
		t.Fatalf("the architect's task = %#v, want it naming %s", pending, ref)
	}
	t.Logf("queued: prompts=%d row %+v", len(r.conn.Prompts()), testerRow(t, r, ref))
	r.linger()
	r.handOver()
	starts := testerChildStarts(t, r)
	n, id := testerNaming(starts, ref)
	t.Logf("after the hand-over: row %+v; child starts naming it: %d (row %d)", testerRow(t, r, ref), n, id)
	machine, _ := r.sup.Machine(r.token)
	if err := machine.Handle(r.ctx, supervise.StreamTurnStart{Claim: r.token}); err != nil {
		t.Fatalf("start the turn: %v", err)
	}
	machine.Wait()
	after := testerRow(t, r, ref)
	t.Logf("after the old confirmation: row %+v; old prompts %d", after, len(r.conn.Prompts()))
	if !after.exists {
		t.Fatalf("the decision on the handed-over child was deleted by the old tree's confirmation")
	}
	if n != 1 {
		t.Fatalf("%d architect starts of the child name %s, want 1: %v", n, ref, starts)
	}
	if after.claimedBy != id && after.heldBy == "" {
		t.Errorf("after the old confirmation the row is claimed by %d and held by nothing; want it still claimed by the child's start row %d", after.claimedBy, id)
	}
	prompts := testerChildHears(t, r, id)
	final := testerRow(t, r, ref)
	t.Logf("child's architect prompts: %q; row after its turn started: %+v", prompts, final)
	if testerCount(prompts, ref) != 1 || final.exists {
		t.Fatalf("the child's architect was told %s %d times and the row exists=%v; want once and gone", ref, testerCount(prompts, ref), final.exists)
	}
	_ = claim.RoleArchitect
	_ = record.RelayKept
	_ = context.Background
}
