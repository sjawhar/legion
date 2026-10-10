package admit

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A root that finishes while a child of its tree is still mid-run hands that child to Legion as a
// root of its own: one Dispatch write sets the child todo and puts the legion label on it, and that
// write's event admits the child as an orphan with its own architect started, as the label and the
// todo do when a person sets them. Before, the child stayed suspended in its root's linger,
// unlabelled and with no architect, and nothing ran it again (LEGION-624). A child that finished,
// one a person parked, and one that never started its first phase stay with the tree.
func TestAFinishedRootHandsItsUnfinishedChildToLegion(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, DesignGate: config.DesignGateRootIssues, ReviewRoundCap: 3, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, running, finished, parked, unstarted = "LEGION-1", "LEGION-2", "LEGION-3", "LEGION-4", "LEGION-5"
	seedSlotted(t, pool, root, "A")
	parent := root
	child := func(key string, at phase.Phase, status string) record.Issue {
		return record.Issue{Key: key, Tree: root, Project: testProject, Title: key, Parent: &parent, Phase: at, Generation: 1, Status: status, DispatchStatus: status, Rank: "B", LastDispatchSeq: 3}
	}
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		rootIssue, err := records.Issue(ctx, tx, root)
		if err != nil {
			t.Fatalf("read root: %v", err)
		}
		rootIssue.Phase = phase.ProductionCheck
		for _, put := range []error{
			records.PutIssue(ctx, tx, *rootIssue),
			records.PutPhase(ctx, tx, record.PhaseRow{Issue: root, Role: claim.RoleImplementer, Claim: "root-implementer", HandoffCommit: "production-check", LastHandoff: "production-check"}),
			records.PutIssue(ctx, tx, child(running, phase.Implementing, "in_progress")),
			records.PutIssue(ctx, tx, child(finished, phase.Done, "done")),
			records.PutIssue(ctx, tx, child(parked, phase.Done, "backlog")),
			records.PutIssue(ctx, tx, child(unstarted, phase.Admitted, "todo")),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
	})

	if result, err := intake.ApplyFact(context.Background(), pool, "api", "signoff", intake.SignOff{Issue: root}, engine, admission); err != nil || result.Refusal != nil {
		t.Fatalf("sign off %s = %+v, %v", root, result.Refusal, err)
	}
	rows, err := pool.Query(context.Background(), `select issue, payload->>'status', payload->>'observedStatus', coalesce(payload->>'handOver', 'false')
		from outbox where kind = 'dispatch_status' and issue <> $1 order by id`, root)
	if err != nil {
		t.Fatalf("read the children's status writes: %v", err)
	}
	var writes [][4]string
	for rows.Next() {
		var write [4]string
		if err := rows.Scan(&write[0], &write[1], &write[2], &write[3]); err != nil {
			t.Fatalf("scan a status write: %v", err)
		}
		writes = append(writes, write)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the children's status writes: %v", err)
	}
	if want := [][4]string{{running, "todo", "in_progress", "true"}}; !reflect.DeepEqual(writes, want) {
		t.Fatalf("the children's Dispatch status writes after the root's sign-off = %v, want %v: the running child handed to Legion, todo with the label", writes, want)
	}

	// The write's event carries the todo and the label.
	apply(t, pool, admission, "running-handed-over", intake.DispatchIssue{Key: running, Seq: 4, Type: "issue.updated", Status: "todo", Title: running, Parent: root, Rank: "B", HandedOver: true}, engine)
	got := issue(t, pool, running)
	architectStarts := 0
	for _, effect := range effects(t, pool) {
		if request, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == running && request.Op == "start" && request.Role == claim.RoleArchitect {
			architectStarts++
		}
	}
	if got.Tree != running || got.Phase != phase.Admitted || got.Generation != 2 || got.Status != "in_progress" || architectStarts != 1 {
		t.Fatalf("the handed-over child = %#v with %d architect starts, want its own admitted root, generation 2, with its architect started once", got, architectStarts)
	}
	assertSlots(t, pool, []record.Slot{{Issue: running, Index: 0, AdmittedAt: fixedNow}})
	for _, key := range []string{finished, parked, unstarted} {
		if got := issue(t, pool, key); got.Tree != root || got.Generation != 1 {
			t.Fatalf("%s = %#v, want it left in %s's tree", key, got, root)
		}
	}
}
