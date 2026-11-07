package workflow

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// correctStatuses queues the Dispatch status write for every issue of the project whose recorded
// Dispatch status is not the status of the phase it stands in (PhaseStatus). A status write the
// daemon lost — a row dropped, a crash between the phase change and the write, a write Dispatch
// refused for good — leaves the board showing one thing while the issue works another, and nothing
// else puts it back: the next write comes with the next phase change, which can be days away. The
// boot runs this, and so does the controller tick (statusRepairTick), so a lost write is put back
// without a restart.
//
// It skips an issue by these rules, in this order:
//
//  1. it belongs to another project;
//  2. the daemon no longer works it (InWorkflow): its status is one the workflow does not run in,
//     or its phase is done. This clause alone keeps the repair off an issue parked at done, since
//     PhaseStatus(phase.Done) is "done" and the phase rule below would pass an issue standing
//     there with any other status. It is the whole protection for an issue a person reopened
//     under a record the daemon closed, which stands at phase done with a live status, so nothing
//     else may be relied on to hold it;
//  3. its phase has no status of its own (an empty PhaseStatus): the issue keeps what it shows;
//  4. its recorded Dispatch status is empty or out of the workflow (record.OutOfWorkflow). An
//     empty status would otherwise pass this walk, and the row it queued could never be written:
//     the outbox writes a status only where Dispatch shows the row's observed or target status
//     (daemon's (*outbox).status), which an empty observed status never matches;
//  5. the status recorded and the phase's status agree, so there is nothing to correct;
//  6. its tree's root is missing from the records this run reads, or that tree is not running by
//     the one rule every reader of the question asks (record.TreeRuns, read here through
//     record.RunningRoots): its lifecycle is not open under the workflow, the root lingers after
//     its close, or the root is itself still waiting in the line for the tree to be opened. Its
//     key goes in the run line's passed-over list;
//  7. it already has an unfinished dispatch_status row of any kind (PendingStatusWrites), so a
//     write is on its way. Its key goes in the run line's pending list.
//
// Rules 6 and 7 come last, and in that order, because the run line names them as the issues the
// run would otherwise have repaired, which holds only once every other rule has been applied.
//
// Each repair writes its own log line, and every run writes one run line, including a run that
// queued nothing. Those two lines are all a reader has: the daemon's state carries
// record.Issue.Status, not record.Issue.DispatchStatus, and this repair adds no field to it. So an
// issue whose board status still differs after a run is one of four things, and the log says which:
// it is in the run line's passed-over list, it is in its pending list, it has a repair line of its
// own, or it has none of the three — which by the rules above means its record holds a Dispatch
// status equal to its phase's, so what Dispatch shows and what the record says have come apart and
// the record is what to look at.
//
// The repair tells nobody. The record cannot tell a person's move on the board from a write the
// daemon lost, so any notice would name both, and most of them would be the person's own move
// arriving a moment before its event.
func (e *Engine) correctStatuses(ctx context.Context, tx pgx.Tx, issues []record.Issue,
	running map[string]struct{}, trigger string) error {
	pendingWrites, err := e.store.PendingStatusWrites(ctx, tx, e.cfg.Project)
	if err != nil {
		return err
	}
	hasPendingWrite := make(map[string]bool, len(pendingWrites))
	for _, row := range pendingWrites {
		hasPendingWrite[row.Issue] = true
	}
	passedOver, pending := []string{}, []string{}
	queued := 0
	for _, issue := range issues {
		if issue.Project != e.cfg.Project || !record.InWorkflow(issue) {
			continue
		}
		want := PhaseStatus(issue.Phase)
		if want == "" || issue.DispatchStatus == "" || record.OutOfWorkflow(issue.DispatchStatus) || issue.DispatchStatus == want {
			continue
		}
		if _, runs := running[issue.Tree]; !runs {
			passedOver = append(passedOver, issue.Key)
			continue
		}
		if hasPendingWrite[issue.Key] {
			pending = append(pending, issue.Key)
			continue
		}
		// The row is enqueued here rather than through Engine.status, which returns without
		// enqueueing anything when the record's own Status already holds the status asked for. An
		// issue whose record says one thing while Dispatch shows another is exactly what this
		// repair is for, so that guard would drop the write it exists to make.
		if err := e.enqueue(ctx, tx, issue.Key, record.StatusWrite{Status: want, ObservedStatus: issue.DispatchStatus}); err != nil {
			return err
		}
		recorded := issue.DispatchStatus
		// Only Status moves. DispatchStatus keeps what Dispatch last showed, because its writers
		// stay the Dispatch event (Engine.keepStatus and the create) and
		// admit.(*Admission).recordObservation, which is what puts the record level again once the
		// daemon's own write echoes back from Dispatch. Writing it here would say the board shows
		// the new status before anything wrote it there, and the next run would then find nothing
		// to repair for a write that never left the outbox.
		if issue.Status != want {
			issue.Status = want
			if err := e.store.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		queued++
		e.logOnCommit(ctx, "workflow: the status repair queues the status of the issue's phase", "issue", issue.Key,
			"phase", issue.Phase, "recorded", recorded, "wrote", want)
	}
	// Both lists are sorted so that the same state writes the same line, whatever order the store
	// returned the issues in.
	slices.Sort(passedOver)
	slices.Sort(pending)
	e.logOnCommit(ctx, "workflow: the status repair ran", "trigger", trigger, "queued", queued,
		"passedOver", passedOver, "pending", pending)
	return nil
}

// statusRepairTick runs the status repair on the controller's periodic tick, which is what puts a
// lost status write back without a daemon restart. Its period is controller_wake_interval_seconds,
// an hour by default, with the first tick a minute after the runtime starts. It returns the zero
// intake.Result: the workflow owns no other part of the tick.
//
// It reads the project's live records (LiveIssues), not every record the project keeps, because a
// fact's cost must not grow with the done records a project holds for good
// (admit's TestAFactReadsAsManyRowsWhateverNumberOfFinishedRecordsTheProjectKeeps, which the tick
// is one of the facts of). The repair selects the same issues from either list: every issue it can
// act on is short of phase done, which LiveIssues holds, and a root LiveIssues leaves out is one
// whose own record is done and holds no slot, whose tree has closed and lingers — so the issue
// under it is passed over either way, by the missing root rather than by the tree-runs rule. The
// boot reads the whole table once for its resume and hands the repair that list (booted).
func (e *Engine) statusRepairTick(ctx context.Context, tx pgx.Tx) (intake.Result, error) {
	issues, err := e.store.LiveIssues(ctx, tx, e.cfg.Project)
	if err != nil {
		return intake.Result{}, err
	}
	running, err := record.ReadRunningRoots(ctx, tx, e.store, e.cfg.Project, issues)
	if err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.correctStatuses(ctx, tx, issues, running, "tick")
}
