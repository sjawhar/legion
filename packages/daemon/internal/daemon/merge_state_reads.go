package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// mergeStateLadderReads is how many times one ladder reads GitHub's merge state for a head GitHub
// answers `unknown` for, and mergeStateLadderWait how long it waits between those reads. GitHub
// computes the merge state lazily: the first read of a head it has not looked at yet asks it to,
// and answers `unknown` while it works, which is an answer about GitHub and not about the head. A
// few seconds is the usual wait, so five reads half a minute apart cover it with room over.
//
// After the fifth `unknown` the ladder stops, and nothing stands behind it: no row waits in the
// outbox, no timer is left armed, and no pass comes round again. `unknown` is no answer about the
// head, so nobody is told of it: the issue's next event, its next handoff and the daemon's next
// start each read again. Every other answer the ladder gets is recorded, and an approval left
// standing on a head GitHub answers with a state it would not merge from tells the tree's
// architect and the reviewer, once for that head and state, in the review-stuck notice the round
// writes (workflow's reviewRound, stuckMergeState).
const (
	mergeStateLadderReads = 5
	mergeStateLadderWait  = 30 * time.Second
)

// queueMergeState is what the workflow asks for when a fact leaves a review round waiting on
// GitHub's merge state for the pull request's head (workflow's askMergeState, the engine's
// Config.ReadMergeState): a ladder of reads for issue. It only records the issue and wakes the
// reader, so the fact's own transaction waits for no GitHub call, and a second fact of the same
// issue queued before the reader takes it is one ladder rather than two. A runtime with no reader
// - the API-only daemon, and a test that drives the ladder itself - queues nothing.
func (w *workflowRuntime) queueMergeState(issue string) {
	w.mergeStates.add(issue)
}

// readMergeStates runs a ladder for each issue queueMergeState queued, until ctx ends: each wake
// takes every issue queued since the last one, in the order of their keys, and runs its ladder on
// a goroutine of its own, so one issue's half-minute waits hold up no other's. An issue whose
// ladder is still running is left to it: that ladder reads GitHub again anyway, and a second one
// would only double the reads.
func (w *workflowRuntime) readMergeStates(ctx context.Context) {
	for {
		issues, ok := w.mergeStates.take(ctx)
		if !ok {
			return
		}
		w.mergeStateMu.Lock()
		if w.mergeStateRunning == nil {
			w.mergeStateRunning = map[string]bool{}
		}
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
//
// The fact names the head GitHub answered for, which the engine records the answer against only
// while the record still stands at it (workflow's mergeability): this ladder reads GitHub with no
// transaction of its own, and a push can land while it waits. The ladder reads on, since what it
// answers is GitHub's state for the head it read, and the push the record took is itself a fact
// that asks for a read of the head it left (workflow's askMergeState).
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
