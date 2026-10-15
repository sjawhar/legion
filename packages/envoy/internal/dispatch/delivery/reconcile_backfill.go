package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// backfillWindowsPerPass bounds how many one-hour windows one pass's backfill walks. runOnce has
// no pass deadline, and walking every remaining hour of 28 days in one pass would hold the regular
// steps' five-minute catch-up for tens of minutes; at 24 windows a pass a backfill finishes in
// about 28 passes.
const backfillWindowsPerPass = 24

// backfillWindow is the backfill's unit of work: fixed, not a listing page, because GitHub lists
// a created=A..B window newest first, so progress taken from a page would sit near the window's
// newest edge and skip its older runs after a rate limit. An hour holds about 30 PR-check runs.
const backfillWindow = time.Hour

// backfillStep names one workflow kind's head_branch and event backfill progress.
func backfillStep(kind DeliveryRunKind) string {
	return "backfill/runs/" + string(kind)
}

// createdAtBackfillStep names the deploy runs' created_at backfill progress: a step of its own, so
// a database whose backfill/runs/deploy finished before created_at was stored walks the window
// again. Only deploy runs need it, since containment is the one reader of a run's creation.
const createdAtBackfillStep = "backfill/created_at/" + string(DeliveryRunKindDeploy)

// backfillRuns re-lists the last BackfillWindow of kind's runs once under step, so every stored
// run carries what only the listing carries: GitHub's head_branch and event (backfillStep; a row
// stored before those columns has neither, and counts in no measure that filters on them), and
// created_at (createdAtBackfillStep; a row stored before that column reads its started_at in its
// place, DeliveryRun.Created). Its progress row (step) starts at now - BackfillWindow with
// began_at now and walks [through, began_at] in one-hour windows, oldest first, at most
// backfillWindowsPerPass a pass; a window's runs are all stored before
// through moves to its end, so a window a rate limit stops is listed again in full on the next
// pass. It lists jobs only for a concluded run that has none stored and no jobs-unfetchable mark:
// the regular step already lists every concluded run's jobs, and the backfill exists for the
// columns, which the listing alone carries. Once through reaches began_at the step is finished
// and lists nothing again.
//
// Every failure is logged and the pass resumes next time, never returned: runOnce's health state
// (last_reconcile_at, last_error) describes the regular steps, which a backfill rate limit must
// not make look failed.
func (r *Reconcile) backfillRuns(ctx context.Context, step, owner, repo, repoFull, workflowPath string, kind DeliveryRunKind, production Production, now time.Time) {
	scope := runsProgressScope(repoFull, workflowPath)
	warn := func(message string, err error, args ...any) {
		slog.Warn("dispatch delivery: backfill runs: "+message, append([]any{"step", step, "error", err}, args...)...)
	}
	progress, err := readBackfillProgress(ctx, r.pool, step, scope)
	if err != nil {
		warn("read progress", err)
		return
	}
	if !progress.found {
		if err := StartBackfillProgress(ctx, r.pool, step, scope, now.Add(-BackfillWindow).Truncate(time.Second), now); err != nil {
			warn("start", err)
			return
		}
		// Read back rather than assume: another reconcile may have begun the same backfill first.
		if progress, err = readBackfillProgress(ctx, r.pool, step, scope); err != nil || !progress.found {
			warn("read progress after starting", err)
			return
		}
	}
	for range backfillWindowsPerPass {
		if !progress.through.Before(progress.beganAt) {
			return
		}
		since := progress.through
		until := since.Add(backfillWindow)
		if progress.beganAt.Before(until) {
			until = progress.beganAt
		}
		if err := r.backfillWindow(ctx, owner, repo, repoFull, workflowPath, kind, production, since, until); err != nil {
			warn("window", err, "window_start", formatWindowBound(since), "window_end", formatWindowBound(until))
			return
		}
		if err := AdvanceBackfillProgress(ctx, r.pool, step, scope, until); err != nil {
			warn("record progress", err, "through", formatWindowBound(until))
			return
		}
		progress.through = until
	}
}

// backfillWindow lists every run of kind created in [since, until) and only then stores them: a
// listing that fails part way writes nothing, and a store that fails part way leaves the window's
// progress unrecorded, so the next pass lists it again in full. GitHub's created qualifier is
// inclusive on both ends, so the window is asked for as since..until-1s, and the next window
// starts at until.
func (r *Reconcile) backfillWindow(ctx context.Context, owner, repo, repoFull, workflowPath string, kind DeliveryRunKind, production Production, since, until time.Time) error {
	last := until.Add(-time.Second)
	if last.Before(since) {
		return nil
	}
	var runs []FetchedRun
	collect := func(_ time.Time, window []FetchedRun) error {
		runs = append(runs, window...)
		return nil
	}
	if err := ListWorkflowRuns(ctx, r.github, owner, repo, workflowPath, since, last, githubResultCap, collect); err != nil {
		return fmt.Errorf("list %s workflow runs: %w", kind, err)
	}
	skipJobs, err := ListRunIDsSkippingJobs(ctx, r.pool, repoFull, completedRunIDs(runs))
	if err != nil {
		return fmt.Errorf("list %s runs whose jobs the backfill skips: %w", kind, err)
	}
	for _, run := range runs {
		if err := r.reconcileRun(ctx, owner, repo, repoFull, kind, production, run, skipJobs); err != nil {
			return fmt.Errorf("run %d: %w", run.RunID, err)
		}
	}
	return nil
}
