package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The pair a start writes (workflow.StartOrOwe), run with the cap full on a claim that is neither
// running nor failed and still holds its phase's task: the relaunch row at once, the task row
// only when the issue's slot comes.
//
// A member's start is delivered while its Sandbox launches, and the task is still unconfirmed
// when the tree stops: a person parks it, and the claim is suspended holding that generation and
// phase's task, or the linger closes the tree and retires the claim, which keeps what it held
// (supervise's retire). The person reopens the root — inside the linger, or after the close — and
// re-admission starts the member again (admit's startMidPhaseMembers). The relaunch row brought
// such a claim back by itself, with a rule-resume or a retry, and the task the claim kept goes
// out as soon as its agent is ready (supervise's sendPending): the member worked its phase with
// no slot — the one thing the cap counts — and the task row behind it would then find a claim no
// longer suspended or retired, so its own guard would not hold and it would prompt the same phase
// a second time. Every such claim is left for the row that carries its task, whatever state it is
// in, so the worker comes back exactly when the cap says it may.
//
// Which of the two tasks the worker then gets is the machine's own rule, and either way it is one
// task and it arrives with the slot: a daemon suspension ends the claim's phase, so the task
// queued for that phase goes with it (supervise's settle) and the task row's own is delivered,
// while a retirement keeps the task the claim was working and the retry sends that one, which is
// why the task row delivers none of its own to a retired claim.
func TestTheRelaunchRowLeavesAClaimThatHoldsItsTaskForTheRowThatCarriesIt(t *testing.T) {
	const kept, owedTask = "Continue. Reason: the start the stop interrupted.", "Continue. Reason: the tree came back."
	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
		// works is the one task the worker is left with once the slot has come.
		works string
	}{
		{"suspended by the linger", supervise.StateSuspended, owedTask},
		{"retired by the tree's close", supervise.StateRetired, kept},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
			putOutboxIssue(t, pool, records, issue)
			openTree(t, pool, "LEGION", issue.Tree)
			sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
			token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
			if err != nil {
				t.Fatalf("claim token: %v", err)
			}
			// The claim as the daemon holds it when the tree stopped and the re-admission comes:
			// stopped where it stood, keeping its session and the unconfirmed task of the phase it
			// was working.
			machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
				Role: claim.RoleImplementer, State: tc.state, Generation: 1, Session: "ses-implementer", SessionFile: "/tmp/implementer.jsonl",
				SuspendedBy: supervise.SuspendedByDaemon,
				Pending:     &supervise.Delivery{ID: "outbox:17", Task: kept, Phase: issue.Phase, Generation: issue.Generation},
			}, "")
			if err != nil {
				t.Fatalf("create the implementer claim: %v", err)
			}

			clock := time.Now()
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
				tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: func() time.Time { return clock },
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
				remove: func(context.Context, workspace.Workspace, sync.Locker) error { return nil },
			}
			request := record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer,
				Generation: issue.Generation, Phase: issue.Phase}

			// Both rows of the pair, as the re-admission writes them, with no slot for the issue:
			// the relaunch row is claimable at once and the task row waits for the slot
			// (record.Store's outboxInOrder).
			relaunch := request
			relaunch.Relaunch = true
			enqueueOutbox(t, pool, records, mustOutboxRow(t, issue.Key, relaunch, clock))
			owed := request
			owed.Task, owed.ResumeTask, owed.NeedsSlot = owedTask, true, true
			enqueueOutbox(t, pool, records, mustOutboxRow(t, issue.Key, owed, clock))

			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the relaunch row: %v", err)
			}
			machine.Wait()
			if got := machine.Claim(); got.State != tc.state || got.Pending == nil || got.Pending.ID != "outbox:17" {
				t.Fatalf("the claim after the relaunch row = %+v, want it still %s holding the task it kept: bringing it back here sends that task as soon as its agent is ready, while the issue holds no slot",
					got, tc.state)
			}
			if started := runtime.Calls(); len(started) != 0 {
				t.Fatalf("the relaunch row made %d runtime calls, want none: the row that carries the task is the one that brings the claim back", len(started))
			}
			if left := outboxRows(t, pool); left != 1 {
				t.Fatalf("outbox rows after the relaunch row = %d, want one: the task row waits for the issue's slot", left)
			}

			// The slot comes: the task row brings the claim back, and exactly one task reaches the
			// worker — never the kept one and a second prompt of its own.
			holdSlot(t, pool, issue.Key)
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the task row: %v", err)
			}
			machine.Wait()
			got := machine.Claim()
			if got.State == tc.state {
				t.Fatalf("the claim after the task row = %+v, want it brought back: the row that carries the task is the one that does it", got)
			}
			if len(resumesOf(runtime, token)) != 1 {
				t.Fatalf("the pair resumed the kept session %d times, want exactly once", len(resumesOf(runtime, token)))
			}
			if got.Pending == nil || got.Pending.Task != tc.works {
				t.Fatalf("the pending delivery = %+v, want the one task %q, delivered once: the pair must not prompt the same phase twice", got.Pending, tc.works)
			}
			if left := outboxRows(t, pool); left != 0 {
				t.Fatalf("outbox rows after the pair = %d, want none left", left)
			}
		})
	}
}
