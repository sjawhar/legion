package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// mergeStateLadderReads is how many times one ladder reads GitHub's merge state for a head GitHub
// answers `unknown` for, and mergeStateLadderWait how long it waits between those reads. GitHub
// computes the merge state lazily: the first read of a head it has not looked at yet asks it to,
// and answers `unknown` while it works, which is an answer about GitHub and not about the head. A
// few seconds is the usual wait, so five reads half a minute apart cover it with room over.
//
// After the fifth `unknown` the ladder stops, and nothing stands behind it: no row waits in the
// outbox, no timer is left armed, and no pass comes round again. The record then shows an
// approval standing on a head GitHub has not reported mergeable, which is the state the issue's
// architect reads and acts on.
const (
	mergeStateLadderReads = 5
	mergeStateLadderWait  = 30 * time.Second
)

// mergeStateTrigger is the intake handler that decides, for each fact the workflow has just
// applied, whether the daemon reads GitHub's merge state for the pull request's head
// (readMergeState). It runs after the engine, so what it reads is the record the fact left.
//
// The merge state is read on each of the pull request's own GitHub events - an opened, a head
// that moved, a push, a review - on each CI settlement, and on each push to the base the pull
// request sits on, and only while an approval stands on the head: those are the moments the state
// can have changed under an issue that is waiting on it, and the reviewing round is the one place
// a recorded merge state moves an issue (workflow's reviewRound). An issue in any other phase, or
// one whose reviewer has approved another head than the one the pull request stands at, is read
// by nothing here: the per-issue read at each handoff and at the daemon's start covers it.
type mergeStateTrigger struct{ runtime *workflowRuntime }

var _ intake.Handler = mergeStateTrigger{}

func (t mergeStateTrigger) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	records := t.runtime.records
	// Every fact but the base push names one pull request, and the arms differ only in how the
	// record is looked up: by the branch the fact names, or by its number.
	var (
		named *record.PullRequest
		pulls []record.PullRequest
		err   error
	)
	switch fact := fact.(type) {
	case intake.PullRequestOpened:
		named, err = records.PullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
	case intake.PullRequestSynchronized:
		named, err = records.PullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
	case intake.Push:
		named, err = records.PullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
	case intake.PullRequestReview:
		named, err = records.PullRequestByNumber(ctx, tx, fact.Repo, fact.Number)
	case intake.PullRequestChecks:
		named, err = records.PullRequestByNumber(ctx, tx, fact.Repo, fact.Number)
	case intake.BasePush:
		// One push to a base serves every pull request sitting on it, which is how the engine
		// marked them (workflow's basePush).
		pulls, err = records.OpenPullRequestsOnBase(ctx, tx, fact.Repo, fact.Branch)
	default:
		return intake.Result{}, nil
	}
	if err != nil {
		return intake.Result{}, err
	}
	// A fact whose pull request the record does not hold is read for nothing: there is no head to
	// ask GitHub about.
	if named != nil {
		pulls = []record.PullRequest{*named}
	}
	for _, pr := range pulls {
		waits, err := t.runtime.approvalWaitsOnTheMergeState(ctx, tx, pr)
		if err != nil {
			return intake.Result{}, err
		}
		if !waits {
			continue
		}
		issue := pr.Issue
		intake.OnCommit(ctx, func() { t.runtime.queueMergeState(issue) })
	}
	return intake.Result{}, nil
}

// approvalWaitsOnTheMergeState says whether pr's issue is one GitHub's merge state would move: it
// stands in reviewing, and its reviewer's recorded approval approves the code the head carries
// (classify.ApprovalStands), so the only thing left between it and accepting is GitHub reporting
// that head one the issue may advance on (record.MergeState.Mergeable).
func (w *workflowRuntime) approvalWaitsOnTheMergeState(ctx context.Context, tx pgx.Tx, pr record.PullRequest) (bool, error) {
	if pr.State != record.PullRequestOpen {
		return false, nil
	}
	issue, err := w.records.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil || issue.Phase != phase.Reviewing {
		return false, err
	}
	rows, err := w.records.Phases(ctx, tx, pr.Issue)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.Role != claim.RoleReviewer {
			continue
		}
		return row.Decision != nil && row.Decision.State == "approved" && classify.ApprovalStands(pr, row.Decision.Head), nil
	}
	return false, nil
}

// queueMergeState asks readMergeStates for a ladder on issue. It only records the issue and wakes
// the reader, so the fact's own transaction waits for no GitHub call, and a second fact of the
// same issue queued before the reader takes it is one ladder rather than two. A runtime with no
// reader - the API-only daemon, and a test that drives the ladder itself - queues nothing.
func (w *workflowRuntime) queueMergeState(issue string) {
	if issue == "" {
		return
	}
	w.mergeStateMu.Lock()
	if w.mergeStateWanted == nil {
		w.mergeStateWanted = map[string]bool{}
	}
	w.mergeStateWanted[issue] = true
	w.mergeStateMu.Unlock()
	select {
	case w.mergeStateWake <- struct{}{}:
	default:
	}
}

// readMergeStates runs a ladder for each issue queueMergeState queued, until ctx ends: each wake
// takes every issue queued since the last one, in the order of their keys, and runs its ladder on
// a goroutine of its own, so one issue's half-minute waits hold up no other's. An issue whose
// ladder is still running is left to it: that ladder reads GitHub again anyway, and a second one
// would only double the reads.
func (w *workflowRuntime) readMergeStates(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.mergeStateWake:
		}
		w.mergeStateMu.Lock()
		if w.mergeStateRunning == nil {
			w.mergeStateRunning = map[string]bool{}
		}
		issues := slices.Sorted(maps.Keys(w.mergeStateWanted))
		clear(w.mergeStateWanted)
		started := make([]string, 0, len(issues))
		for _, issue := range issues {
			if w.mergeStateRunning[issue] {
				continue
			}
			w.mergeStateRunning[issue] = true
			started = append(started, issue)
		}
		w.mergeStateMu.Unlock()
		for _, issue := range started {
			go w.mergeStateLadder(ctx, issue)
		}
	}
}

// mergeStateLadder reads GitHub's merge state for issue's pull request until GitHub answers with
// one, at most mergeStateLadderReads times, mergeStateLadderWait apart. GitHub's `unknown` says it
// is still computing the state, so the ladder asks again; every other answer, the read finding no
// open pull request among them, is an answer, and the ladder ends on it. After the last `unknown`
// it ends too, leaving nothing queued or armed behind it. A failed read ends the ladder: the
// issue's next event, its next handoff and the daemon's next start all read again.
func (w *workflowRuntime) mergeStateLadder(ctx context.Context, issue string) {
	defer func() {
		w.mergeStateMu.Lock()
		delete(w.mergeStateRunning, issue)
		w.mergeStateMu.Unlock()
	}()
	for read := 1; ; read++ {
		state, err := w.readMergeState(ctx, issue)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("read GitHub's merge state for an issue whose approval waits on it; the record stands as it was until the next read",
					"issue", issue, "read", read, "error", err)
			}
			return
		}
		if state != record.MergeStateUnknown || read >= mergeStateLadderReads {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.mergeStateWait()):
		}
	}
}

// mergeStateWait is how long the ladder waits between reads of a head GitHub is still computing
// the merge state of: mergeStateLadderWait, which a test shortens through the runtime's own field.
func (w *workflowRuntime) mergeStateWait() time.Duration {
	if w.mergeStateRetry > 0 {
		return w.mergeStateRetry
	}
	return mergeStateLadderWait
}

// readMergeState reads issue's pull request from GitHub once (one GET /pulls/{number}) and applies
// what it says about merging that head into its base (applyMergeability, the same fact the
// per-issue read applies), answering GitHub's merge state as that read found it. An issue whose
// record holds no open pull request is read from nothing and answers record.MergeStateUnread: the
// state belongs to a pull request, and there is none.
func (w *workflowRuntime) readMergeState(ctx context.Context, issue string) (record.MergeState, error) {
	_, recorded, err := w.issueRecord(ctx, issue)
	if err != nil {
		return record.MergeStateUnread, err
	}
	if recorded == nil || recorded.State != record.PullRequestOpen {
		return record.MergeStateUnread, nil
	}
	read, cancel := context.WithTimeout(ctx, issueRead)
	_, pull, err := w.readPullRequest(read, *recorded)
	cancel()
	if err != nil {
		return record.MergeStateUnread, fmt.Errorf("read pull request %s#%d and its merge state: %w", recorded.Repo, recorded.Number, err)
	}
	w.applyMergeability(ctx, *recorded, pull)
	return pull.mergeState(), nil
}
