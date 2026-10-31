package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A start's task names a decision a person made while nobody in the root's tree could hear it, and
// the root's architect takes it: its turn on that task starts, so the architect has been told. The
// write recording that confirmation is the one that tells the decision (store.tellKeptDecisions),
// and it can fail — a database error, a lost connection — while the turn goes on. When the turn
// then ends and the delivery is retired, the architect has still been told: the decision must not
// stand KEPT for the root's next start or catch-up to name it a second time.
func TestADecisionToldInATurnWhoseConfirmationFailedIsNotToldAgain(t *testing.T) {
	r := newHeldTaskRig(t)
	ref := "dispatch://" + heldTaskRoot + "/ask/ask-told"
	keepDecision(t, r.pool, r.records, heldTaskRoot, ref, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	r.runRow(r.enqueueStart())
	prompts := r.conn.Prompts()
	if len(prompts) != 1 || !strings.Contains(prompts[0].Message, ref) {
		t.Fatalf("the architect was prompted %#v, want the start's task naming %s", prompts, ref)
	}

	// The confirmation's write fails for as long as the turn lasts: the database refuses the
	// telling of the row the delivery holds.
	for _, statement := range []string{
		`create function refuse_the_tell() returns trigger language plpgsql as $$
			begin raise exception 'the confirmation''s write did not commit'; end $$`,
		`create trigger refuse_the_tell before delete on parked_decisions for each row execute function refuse_the_tell()`,
	} {
		if _, err := r.pool.Exec(r.ctx, statement); err != nil {
			t.Fatalf("make the confirmation's write fail: %v", err)
		}
	}
	machine, _ := r.sup.Machine(r.token)
	for _, ev := range []supervise.Event{
		supervise.StreamTurnStart{Claim: r.token},
		supervise.StreamTurnEnd{Claim: r.token},
	} {
		_ = machine.Handle(r.ctx, ev)
		machine.Wait()
	}
	for _, statement := range []string{
		"drop trigger refuse_the_tell on parked_decisions",
		"drop function refuse_the_tell()",
	} {
		if _, err := r.pool.Exec(r.ctx, statement); err != nil {
			t.Fatalf("let the store take writes again: %v", err)
		}
	}

	// The root's next generation: its start task names what the root still keeps.
	r.readmit()
	var task string
	err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		task, _, err = r.engine.ParkedDecisionTask(r.ctx, tx, r.root)
		return err
	})
	if err != nil {
		t.Fatalf("compose the next start's task: %v", err)
	}
	if strings.Contains(task, ref) {
		t.Fatalf("the root's next start task %q names %s again; the architect was told it in the turn that started on "+
			"the first start's task, whose confirmation's write failed (kept now: %s)",
			task, ref, keptRefs(keptDecisions(t, r.pool, r.records, heldTaskRoot)))
	}
}
