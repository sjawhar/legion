package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// issueRead bounds one GitHub read the per-issue read makes: the pull request itself, the list of
// the issue's branch when the record holds none, the reviews, and the compare of an approved head
// with the head. Each is a single answer that comes in well under a second, so a read that runs
// this long holds a request GitHub never answered.
const issueRead = 30 * time.Second

// readIssuePullRequest reads one issue's pull request from GitHub and applies what it shows, as the
// webhook would have: the head GitHub holds it at (followHead, and the handoff-only push that
// reached it), whether that head can be merged into its base and GitHub's own merge state
// (applyMergeability), a deciding review the daemon has not recorded (readReviews), and the push
// that carries an approval set aside below the head (readApprovalCarry), in that order. An issue
// whose record holds no open pull request is looked up by its branch instead (openPullRequestOn),
// and the opened its webhook would have carried is applied first, so the reads that follow judge
// the pull request that leaves.
//
// It is the backstop for a webhook that never arrived: GitHub can fail to deliver one, and
// Dispatch's redelivery sweep gives up after five attempts, which leaves the record holding a head,
// a review or a mergeability GitHub has moved past, and every later decision judged against it.
//
// It runs at two moments and at no other: as the workflow runtime starts (readIssuesAtStart), which
// covers the deliveries made while the daemon was down, and once an agent's handoff has been
// recorded (readHandedOff), the moment the daemon next decides on the pull request's state. It
// reads one issue, not every open pull request, so a handoff costs the reads of its own pull
// request alone.
//
// Every fact it applies goes through intake.ApplyFact under an event id of its own, and the
// workflow's own fences decide each against the record: a second read of a pull request nothing
// changed on applies nothing the workflow records. A failed read is returned to the caller, which
// logs it; the record then stands as it was, and the next read of the issue asks again.
func (w *workflowRuntime) readIssuePullRequest(ctx context.Context, issue string) error {
	recorded, err := w.issuePullRequest(ctx, issue)
	if err != nil || recorded == nil {
		return err
	}
	pr := *recorded
	read, cancel := context.WithTimeout(ctx, issueRead)
	github, pull, err := w.readPullRequest(read, pr)
	cancel()
	if err != nil {
		return fmt.Errorf("read pull request %s#%d and its mergeability: %w", pr.Repo, pr.Number, err)
	}
	if pr, err = w.followHead(ctx, github, pr, pull); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		// The reads below go on against the record as it stands when a comparison GitHub keeps
		// failing stops this one following the head: the next read of the issue follows it.
		w.log.Warn("follow the head GitHub shows for an issue's pull request; this read judges it at the recorded head",
			"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "recorded", pr.HeadSHA, "head", pull.Head.SHA, "error", err)
	}
	// The reads after the head start their bound only now: following a head waits on the fact lock,
	// which the GitHub consumer's backlog can hold for a while.
	prCtx, cancelReads := context.WithTimeout(ctx, issueRead)
	defer cancelReads()
	w.applyMergeability(ctx, pr, pull)
	if w.readReviews(ctx, prCtx, github, pr) {
		return nil
	}
	w.readApprovalCarry(ctx, prCtx, github, pr)
	return nil
}

// issuePullRequest is the open pull request the per-issue read judges for key: the record's, when
// it holds one open, and otherwise the one GitHub shows open on the issue's branch
// (openPullRequestOn), applied as the intake.PullRequestOpened its webhook would have carried -
// naming the repository as that webhook's fact would (heardSpelling), a reopen when the record
// holds the same pull request closed - and read back as the workflow recorded it. It is nil for an
// issue the daemon does not record, one with no repository to read, one whose branch GitHub holds
// no open pull request on - which an implementing issue's architect is told of (tellMissing) - and
// one whose opened the workflow declined, which leaves the record as it was and nothing for the
// reads that follow to judge.
func (w *workflowRuntime) issuePullRequest(ctx context.Context, key string) (*record.PullRequest, error) {
	issue, pr, err := w.issueRecord(ctx, key)
	if err != nil || issue == nil {
		return nil, err
	}
	if pr != nil && pr.State == record.PullRequestOpen {
		return pr, nil
	}
	if issue.GitHubRepo == "" {
		return nil, nil
	}
	branch := claim.IssueBranch(key)
	read, cancel := context.WithTimeout(ctx, issueRead)
	pull, found, err := w.openPullRequestOn(read, issue.GitHubRepo, branch)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("list the open pull requests of %s on %s: %w", issue.GitHubRepo, branch, err)
	}
	if !found {
		w.tellMissing(ctx, *issue, pr)
		return nil, nil
	}
	repo := w.heardSpelling(issue.GitHubRepo)
	w.log.Warn("workflow: GitHub holds an open pull request for an issue the daemon records none for; applying the opened its webhook would",
		"repo", repo, "pullRequest", pull.Number, "issue", key, "head", pull.Head.SHA, "updated_at", pull.UpdatedAt)
	fact := intake.PullRequestOpened{Repo: repo, Number: pull.Number, Branch: branch, HeadSHA: pull.Head.SHA, Body: pull.Body,
		URL: pull.HTMLURL, UpdatedAt: pull.UpdatedAt, Reopened: pr != nil && pr.Number == pull.Number}
	eventID := fmt.Sprintf("opened-read:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, repo, pull.Number, time.Now().UnixNano())
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
		return nil, fmt.Errorf("apply the open pull request %s#%d read from GitHub: %w", repo, pull.Number, err)
	}
	_, after, err := w.issueRecord(ctx, key)
	if err != nil {
		return nil, err
	}
	if after == nil || after.State != record.PullRequestOpen || after.Number != pull.Number {
		w.log.Info("workflow: the workflow did not record the open pull request this read found; the next read of the issue applies it again",
			"repo", repo, "pullRequest", pull.Number, "issue", key)
		return nil, nil
	}
	return after, nil
}

// tellMissing tells issue's architect that its implementer completed and GitHub lists no open pull
// request on the issue's branch, with none open or merged recorded either
// (intake.PullRequestMissing, which the workflow decides and tells: workflow's pullRequestMissing).
// The per-issue read reaches it where the pass once did: an implementer's completion queues the
// read of its own issue (handoffCompleted), and the daemon's start reads every issue whose
// implementer completed with no pull request recorded (readIssuesAtStart). An issue in any other
// phase, or one whose record lets the implementation move, is told nothing. The event id names the
// issue's generation and the handoff commit, so the architect is told once per handoff commit,
// whichever read finds it, and again only for a completion at another commit. A failed read or
// apply is logged, and the next read of the issue tells it.
func (w *workflowRuntime) tellMissing(ctx context.Context, issue record.Issue, pr *record.PullRequest) {
	if issue.Phase != phase.Implementing || record.LetsImplementationMove(pr) {
		return
	}
	var rows []record.PhaseRow
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		rows, err = w.records.Phases(ctx, tx, issue.Key)
		return err
	}); err != nil {
		if ctx.Err() == nil {
			w.log.Warn("read the phases of an issue GitHub lists no open pull request for", "issue", issue.Key, "error", err)
		}
		return
	}
	commit := ""
	for _, row := range rows {
		if row.Role == claim.RoleImplementer {
			commit = row.HandoffCommit
		}
	}
	if commit == "" {
		return
	}
	branch := claim.IssueBranch(issue.Key)
	eventID := fmt.Sprintf("pull-request-missing:%s:%s:%d:%s", w.dispatchProject, issue.Key, issue.Generation, commit)
	w.log.Warn("workflow: GitHub lists no open pull request on the branch of an issue whose implementer completed; telling its architect",
		"repo", issue.GitHubRepo, "issue", issue.Key, "branch", branch, "handoff", commit)
	fact := intake.PullRequestMissing{Issue: issue.Key, Repo: issue.GitHubRepo, Branch: branch, HandoffCommit: commit}
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
		w.log.Warn("apply a missing pull request read from GitHub", "repo", issue.GitHubRepo, "issue", issue.Key, "error", err)
	}
}

// issueRecord is key's issue record and its pull request record, both nil for what is not
// recorded, read in one transaction.
func (w *workflowRuntime) issueRecord(ctx context.Context, key string) (*record.Issue, *record.PullRequest, error) {
	var issue *record.Issue
	var pr *record.PullRequest
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		if issue, err = w.records.Issue(ctx, tx, key); err != nil || issue == nil {
			return err
		}
		pr, err = w.records.PullRequest(ctx, tx, key)
		return err
	}); err != nil {
		return nil, nil, fmt.Errorf("read the record of %s: %w", key, err)
	}
	return issue, pr, nil
}

// readIssuesAtStart reads the pull request of each issue in the workflow once as the runtime starts
// (readIssuePullRequest): every issue with an open pull request recorded, and every issue whose
// implementer completed with none open or merged recorded, which is the pull request this read
// looks up by the branch. It is what covers a daemon that was down while GitHub made the
// deliveries nothing redelivers. A read that fails is logged and the boot goes on, since the
// issue's next handoff reads the same pull request; GitHub's rate limit ends the start read there,
// because every read after it on the same installation meets the same limit, and the panes share
// it.
func (w *workflowRuntime) readIssuesAtStart(ctx context.Context) {
	issues, err := w.issuesInWorkflow(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("list the issues to read the pull request of as the daemon starts", "error", err)
		}
		return
	}
	for _, issue := range issues {
		if ctx.Err() != nil {
			return
		}
		err := w.readIssuePullRequest(ctx, issue)
		if err == nil {
			continue
		}
		var answer *githubrest.Answer
		if errors.As(err, &answer) && answer.RateLimited {
			w.log.Error("read an issue's pull request as the daemon starts: GitHub's rate limit; the start reads no more issues, and each issue's next handoff reads it",
				"issue", issue, "error", err)
			return
		}
		w.log.Warn("read an issue's pull request as the daemon starts; the issue's next handoff reads it", "issue", issue, "error", err)
	}
}

// issuesInWorkflow is every issue of the project the per-issue read has something to read for, by
// key: those with an open pull request recorded, and those whose implementer completed with none
// open or merged recorded (record.Store.HandedOffWithoutOpenOrMergedPullRequest), whose pull
// request GitHub may hold open with its opened lost. An issue with neither - one still being
// planned - has no pull request to read, and costs no GitHub call.
func (w *workflowRuntime) issuesInWorkflow(ctx context.Context) ([]string, error) {
	var open []record.PullRequest
	var waiting []record.HandedOff
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		if open, err = w.records.OpenPullRequests(ctx, tx, w.dispatchProject); err != nil {
			return err
		}
		waiting, err = w.records.HandedOffWithoutOpenOrMergedPullRequest(ctx, tx, w.dispatchProject)
		return err
	}); err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, pr := range open {
		keys[pr.Issue] = true
	}
	for _, handedOff := range waiting {
		keys[handedOff.Issue.Key] = true
	}
	return slices.Sorted(maps.Keys(keys)), nil
}

// handoffCompleted queues the read of issue's pull request for readHandedOff, once the API has
// recorded a worker's completion of its phase (the handoff route's own hook): the daemon decides
// the issue's next step on the pull request's state, and this is when it is read. It only records
// the issue and wakes the read, so the route answers the agent without waiting for GitHub, and the
// read runs on the runtime's own goroutine, outside the completion's transaction.
func (w *workflowRuntime) handoffCompleted(issue string) {
	w.handedOff.add(issue)
}

// readHandedOff reads the pull request of each issue handoffCompleted queued, until ctx ends: each
// wake takes every issue queued since the last one, in the order of their keys, and reads each once
// (readIssuePullRequest). A read that fails is logged and nothing is queued again: the issue's next
// handoff, and the daemon's next start, read it again.
func (w *workflowRuntime) readHandedOff(ctx context.Context) {
	for {
		issues, ok := w.handedOff.take(ctx)
		if !ok {
			return
		}
		for _, issue := range issues {
			if ctx.Err() != nil {
				return
			}
			if err := w.readIssuePullRequest(ctx, issue); err != nil {
				w.log.Warn("read the pull request of an issue whose agent handed off; the record stands as it was until the next read",
					"issue", issue, "error", err)
			}
		}
	}
}
