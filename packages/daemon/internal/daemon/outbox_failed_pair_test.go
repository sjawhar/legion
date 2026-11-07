package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The pair a transition writes, run on a failed claim in the order the cap imposes: the relaunch
// row at once, the task row only when the issue's slot comes (workflow.StartOrOwe).
//
// A failed claim keeps the task it was working. A retry revives it and sends that task again, so
// a relaunch row that retried a failed claim put the worker back to work with no slot — the one
// thing the cap counts — and the task row that followed found a claim no longer failed, so its
// own guard no longer held, and delivered a second prompt for the same phase. The retry belongs
// to the row that carries the task: it comes with the slot, and the kept task is delivered once.
func TestTheRelaunchRowLeavesAFailedClaimForTheRowThatCarriesItsTask(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
	token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree,
		Issue: issue.Key, Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create implementer claim: %v", err)
	}
	runtime.ScriptSpawn(fake.SpawnResult{Err: errors.New("pane launch failed")}, fake.SpawnResult{Err: errors.New("pane launch failed")})
	_ = machine.Handle(ctx, supervise.RequestSpawn{Claim: token})
	if got := machine.Claim().State; got != supervise.StateFailed {
		t.Fatalf("claim state = %s, want failed: the fixture's own premise", got)
	}

	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir()}
	const task = "Continue. Reason: retry held phase."
	request := record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer,
		Generation: issue.Generation, Phase: issue.Phase}

	// The relaunch row: the transition's request with no task and the mark that says its task
	// travels behind it (workflow.StartOrOwe), claimable at once, while the issue holds no slot.
	relaunchRequest := request
	relaunchRequest.Relaunch = true
	relaunch := mustOutboxRow(t, issue.Key, relaunchRequest, time.Now())
	relaunch.ID = 91
	if err := runner.execute(ctx, relaunch); err != nil {
		t.Fatalf("run the relaunch row of a failed claim: %v", err)
	}
	if got := machine.Claim(); got.State != supervise.StateFailed || got.Pending != nil {
		t.Fatalf("the claim after the relaunch row = %+v, want it still failed with nothing pending: a retry here revives it and sends the task it kept, while the issue holds no slot", got)
	}

	// The task row, when the slot comes: the same request with the task, which retries the
	// claim and delivers the one task it kept — not a second prompt of its own.
	withTask := request
	withTask.Task, withTask.NeedsSlot = task, true
	row := mustOutboxRow(t, issue.Key, withTask, time.Now())
	row.ID = 92
	if err := runner.execute(ctx, row); err != nil {
		t.Fatalf("run the task row of a failed claim: %v", err)
	}
	got := machine.Claim()
	if got.State != supervise.StateLaunching || got.Budgets != (supervise.Budgets{}) {
		t.Fatalf("the claim after the task row = %+v, want it launching on fresh budgets: the row that carries the task is the one that retries it", got)
	}
	if got.Pending == nil || got.Pending.ID != "outbox:92" || got.Pending.Task != task {
		t.Fatalf("the pending delivery = %+v, want the task row's own task, delivered once: the relaunch before it delivered nothing", got.Pending)
	}
}
