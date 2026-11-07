package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The repair compares the status the record holds for Dispatch against the status of the issue's
// phase, and the record can be stale: Dispatch already shows the phase's status, and the event
// that would have told the record so never reached the daemon. The repair queues a row on every
// turn then, and the outbox writes none of them, because Dispatch already shows what the row would
// write. That is the cost the design accepts for reading one field of the record rather than the
// board, and this test is what records it.
func TestTheStatusRepairKeepsQueuingARowTheOutboxWillNotWrite(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	putOutboxIssue(t, pool, records, staleStatusIssue(phase.Implementing, "in_progress", "todo"))
	putStatusRepairSlot(t, pool, records, "LEGION-208")
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, quietLogger())
	client := &outboxDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "in_progress"}}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, now: time.Now, log: quietLogger()}

	for _, turn := range []string{"tick-1", "tick-2"} {
		if _, err := intake.ApplyFact(ctx, pool, "controller", turn, intake.ControllerTick{}, engine); err != nil {
			t.Fatalf("apply %s: %v", turn, err)
		}
		if got := outboxRows(t, pool); got != 1 {
			t.Fatalf("%s left %d outbox rows, want the one status row the repair queued", turn, got)
		}
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("drain the outbox after %s: %v", turn, err)
		}
		if got := outboxRows(t, pool); got != 0 {
			t.Fatalf("%s left %d outbox rows after the drain, want none", turn, got)
		}
	}

	if len(client.statuses) != 0 {
		t.Fatalf("Dispatch was written %v, want nothing: it already shows the phase's status", client.statuses)
	}
	if got := readIssue(t, pool, "LEGION-208"); got == nil || got.Status != "in_progress" || got.DispatchStatus != "todo" {
		t.Fatalf("the record = %+v, want status in_progress and the stale todo it held for Dispatch", got)
	}
}

// A person moved the issue to testing and the daemon never consumed that event, so the record
// holds the todo it saw before. The repair queues the in_progress of the issue's phase against
// that todo, and the outbox writes nothing: Dispatch shows neither the row's status nor the status
// the row was queued against, which is the guard that keeps a stale row off a person's move. The
// record and the board both stay where they were.
func TestTheStatusRepairWritesNothingOverAStatusTheDaemonNeverSaw(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	putOutboxIssue(t, pool, records, staleStatusIssue(phase.Implementing, "in_progress", "todo"))
	putStatusRepairSlot(t, pool, records, "LEGION-208")
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, quietLogger())
	client := &outboxDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "testing"}}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, now: time.Now, log: quietLogger()}

	if _, err := intake.ApplyFact(ctx, pool, "controller", "tick", intake.ControllerTick{}, engine); err != nil {
		t.Fatalf("apply the tick: %v", err)
	}
	if got := outboxRows(t, pool); got != 1 {
		t.Fatalf("the tick left %d outbox rows, want the one status row the repair queued", got)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the outbox: %v", err)
	}

	if len(client.statuses) != 0 {
		t.Fatalf("Dispatch was written %v, want nothing over the move the daemon never saw", client.statuses)
	}
	if client.issue.Status != "testing" {
		t.Fatalf("Dispatch shows %q, want the testing a person moved the issue to", client.issue.Status)
	}
	if got := readIssue(t, pool, "LEGION-208"); got == nil || got.Status != "in_progress" || got.DispatchStatus != "todo" {
		t.Fatalf("the record = %+v, want status in_progress and the todo it held for Dispatch", got)
	}
}

// The outbox finishes a status write it did send without touching the status the record holds for
// Dispatch. That field moves only when Dispatch tells the daemon what it shows — the write's own
// event, or a boot listing — so the record still reads todo here, and the next repair run queues
// the write again until that event arrives.
func TestNoOutboxFinishWritesTheRecordsDispatchStatus(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	putOutboxIssue(t, pool, records, staleStatusIssue(phase.Implementing, "todo", "todo"))
	putStatusRepairSlot(t, pool, records, "LEGION-208")
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, quietLogger())
	client := &outboxDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "todo"}}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, now: time.Now, log: quietLogger()}

	if _, err := intake.ApplyFact(ctx, pool, "controller", "tick", intake.ControllerTick{}, engine); err != nil {
		t.Fatalf("apply the tick: %v", err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the outbox: %v", err)
	}

	if want := []string{"in_progress"}; len(client.statuses) != 1 || client.statuses[0] != want[0] {
		t.Fatalf("Dispatch was written %v, want %v", client.statuses, want)
	}
	if got := outboxRows(t, pool); got != 0 {
		t.Fatalf("outbox rows after the write = %d, want the row finished", got)
	}
	if got := readIssue(t, pool, "LEGION-208"); got == nil || got.Status != "in_progress" || got.DispatchStatus != "todo" {
		t.Fatalf("the record = %+v, want status in_progress and the todo Dispatch last told the daemon", got)
	}
}

// staleStatusIssue is the one issue of these tests, a root of its own tree, standing in at, with
// the record's own status and the status the record holds for Dispatch.
func staleStatusIssue(at phase.Phase, status, shown string) record.Issue {
	return record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "the issue whose status write was lost", Phase: at,
		Generation: 1, Status: status, DispatchStatus: shown, Rank: "U", HandedOver: true, LastDispatchSeq: 1}
}

// putStatusRepairSlot grants the issue's slot and opens its tree's lifecycle under the workflow's
// authority, as admission's own promotion does in one transaction. The lifecycle, not the slot
// row, is what says a tree runs (record.TreeRuns, which the repair reads), so a fixture that
// granted a slot and nothing else would describe a tree the daemon reads as never opened.
func putStatusRepairSlot(t *testing.T, pool *pgxpool.Pool, records record.Store, issue string) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: issue, Index: 0, AdmittedAt: time.Now().UTC()}); err != nil {
			return err
		}
		project, err := claim.ProjectToken("LEGION")
		if err != nil {
			return err
		}
		_, err = records.OpenTreeLifecycle(context.Background(), tx, project, issue, treelifecycle.AuthorityWorkflow)
		return err
	}); err != nil {
		t.Fatalf("seed the slot of %s: %v", issue, err)
	}
}
