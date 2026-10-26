package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// applyControllerTick is one turn: it re-reads from Dispatch the issues anything is currently
// waiting on and applies the tick carrying what it saw. The read runs here, before the fact,
// because every handler decides inside the fact's transaction and performs no I/O; the mirror
// handler writes the rows there, ahead of the two handlers that judge from them. So a blocker
// whose closing event the daemon never consumed is caught within one wake interval instead of
// standing until the next boot's listing.
func (w *workflowRuntime) applyControllerTick(ctx context.Context, at time.Time) {
	eventID := fmt.Sprintf("controller-tick:%s:%s:%d", w.dispatchProject, w.bootID, at.UnixNano())
	fact := intake.ControllerTick{Observed: w.observeWaitedOn(ctx)}
	if _, err := intake.ApplyFact(ctx, w.pool, "controller", eventID, fact, w.handlers...); err != nil {
		w.log.Warn("apply the controller wake tick failed", "error", err)
	}
}

// blockerReadFanout is how many blocker keys one turn reads one at a time: up to this many are
// read singly, and above it the whole project is listed once instead. The listing is a single
// unpaged request (dispatch.HTTPClient.ListIssues) and Dispatch caps an issue at 20 blockers, so
// the fan-out a waiting set can ask for is bounded on both sides of the choice.
const blockerReadFanout = 25

// observeWaitedOn is what Dispatch says now about the issues this daemon is holding something
// back for. It is empty on almost every turn — nothing waiting means nothing read from Dispatch
// at all — and empty is the tick exactly as it has always been.
//
// A failed read never costs the turn: it is logged and the tick is applied with whatever was
// read, since the tick is also the controller's daily-report wake and the only wake a tree
// waiting on a refused root claim gets.
func (w *workflowRuntime) observeWaitedOn(ctx context.Context) []intake.IssueStatus {
	keys, err := w.waitedOn(ctx)
	if err != nil {
		w.log.Warn("the controller tick's read of what this project waits on failed", "error", err)
		return nil
	}
	if len(keys) == 0 {
		return nil
	}
	return w.readObserved(ctx, keys)
}

// waitedOn is the distinct keys of the unfinished blockers of everything this daemon holds back,
// in one short read-only transaction: the judgement the deciders themselves make, read rather
// than remade (blockers.Blocked), over every root on the waiting line (record.Waiting) and every
// recorded child a blocker holds at phase.Admitted. A candidate the rule exempts is read for
// nothing, as promotion admits it whatever its links say and nothing consults the answer
// (blockers.Exempt), and the keys come out in that judgement's own order, by the issue waiting
// on them.
func (w *workflowRuntime) waitedOn(ctx context.Context) ([]string, error) {
	tx, err := w.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	issues, err := w.records.Issues(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("list the issues of %s: %w", w.dispatchProject, err)
	}
	slots, err := w.records.Slots(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("list the admission slots: %w", err)
	}
	rows, err := blockers.Load(ctx, tx, w.records, w.dispatchProject)
	if err != nil {
		return nil, err
	}
	var keys []string
	seen := make(map[string]struct{})
	for _, blocked := range blockers.Blocked(rows, issues, record.Waiting(issues, slots)) {
		for _, blocker := range blocked.BlockedBy {
			if _, repeated := seen[blocker]; repeated {
				continue
			}
			seen[blocker] = struct{}{}
			keys = append(keys, blocker)
		}
	}
	return keys, nil
}

// readObserved is Dispatch's answer for keys: one GetIssue each while they number
// blockerReadFanout or fewer, and one whole-project listing above that, whose every summary is
// taken — it is one request for all of them, and a row it refreshes beyond the keys asked for
// costs nothing behind the mirror's own sequence fence.
//
// A key whose read fails is logged and left out, and so is one Dispatch no longer serves, which
// is no error at all: the rest of the turn goes on with what was read, and the row of a key left
// out keeps the last thing Dispatch said about it.
func (w *workflowRuntime) readObserved(ctx context.Context, keys []string) []intake.IssueStatus {
	if len(keys) > blockerReadFanout {
		summaries, err := w.dispatch.ListIssues(ctx, w.dispatchProject, nil)
		if err != nil {
			w.log.Warn("the controller tick's re-read of the project's issues failed", "blockers", len(keys), "error", err)
			return nil
		}
		observed := make([]intake.IssueStatus, 0, len(summaries))
		for _, summary := range summaries {
			observed = append(observed, blockers.Observed(summary))
		}
		return observed
	}
	observed := make([]intake.IssueStatus, 0, len(keys))
	for _, key := range keys {
		issue, err := w.dispatch.GetIssue(ctx, key)
		if err != nil {
			if dispatch.Missing(err) {
				w.log.Info("the controller tick re-read a blocker Dispatch no longer serves", "issue", key)
				continue
			}
			w.log.Warn("the controller tick's re-read of a blocker failed", "issue", key, "error", err)
			continue
		}
		observed = append(observed, intake.IssueStatus{
			Key: key, Status: issue.Status, Blockers: issue.BlockedBy, Seq: issue.LastSeq, ClaimedBy: issue.Claim.Holder(), ClaimKnown: true})
	}
	return observed
}
