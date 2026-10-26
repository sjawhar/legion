package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// readApprovalCarry is the required-checks pass's read for a review round whose approval is set
// aside: the reviewer approved a head other than the recorded one, no recorded push carries the
// approval up to it (classify.ApprovalStands), and no push that may change code is on its way
// (classify.CodeOnItsWay). A recorded push carries an approval only from the head it replaced, so
// an approval is set aside when the webhook of the push above it was lost, still waits behind the
// GitHub consumer's backlog, or came from a listener that could not say what it changed, and also
// when the approved head lies between two recorded heads, as when followHead brought the record
// from an older head past it. The read asks GitHub to compare the approved head with the recorded
// one (handoffPush) and, when only the issue's own handoffs changed between them, applies the
// intake.Push that says so, which the workflow decides as it decides the webhook's (workflow's
// push): the approval carries to the recorded head and the round ends. Anything else leaves the
// approval set aside, as before. GitHub's answer for two heads never changes, so one that does not
// carry the approval is logged once and not asked for again by this process (uncarried). It runs
// after followHead and readReviews, so it compares with the head GitHub shows and judges the
// decision a review just read recorded.
//
// ctx bounds the apply, and read the reads before it. It says whether the pass must stop: ctx
// ended, or GitHub answered the compare with its rate limit. Any other failure is logged and
// applies nothing, and the next pass reads again.
func (w *workflowRuntime) readApprovalCarry(ctx, read context.Context, github githubrest.Client, listed record.PullRequest) bool {
	approved, pr, err := w.approvalSetAside(read, listed)
	if err != nil {
		if ctx.Err() != nil {
			return true
		}
		w.log.Warn("read whether a pull request's approval is set aside", "repo", listed.Repo, "pullRequest", listed.Number, "issue", listed.Issue, "error", err)
		return false
	}
	if pr == nil {
		return false
	}
	compared := fmt.Sprintf("%s#%d:%s...%s", pr.Repo, pr.Number, approved, pr.HeadSHA)
	if w.uncarried[compared] {
		return false
	}
	push, aside, err := handoffPush(read, github, *pr, approved, pr.HeadSHA)
	if err != nil {
		if ctx.Err() != nil {
			return true
		}
		attributes := []any{"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "approved", approved, "head", pr.HeadSHA, "error", err}
		var answer *githubrest.Answer
		if errors.As(err, &answer) {
			attributes = append(attributes, "status", answer.Status)
		}
		if answer != nil && answer.RateLimited {
			w.log.Error("compare a pull request's approved head with its head: GitHub's rate limit; this pass reads no more pull requests", attributes...)
			return true
		}
		w.log.Error("compare a pull request's approved head with its head; the approval stays set aside until a later pass or the push's webhook carries it", attributes...)
		return false
	}
	if aside != "" {
		w.log.Info("workflow: the reviewer's approval stays set aside below the head; only an APPROVE of the head or a REQUEST_CHANGES ends the round", "reason", aside,
			"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "approved", approved, "head", pr.HeadSHA)
		if w.uncarried == nil {
			w.uncarried = map[string]bool{}
		}
		w.uncarried[compared] = true
		return false
	}
	w.log.Warn("workflow: only the issue's handoffs changed above the approved head, and no recorded push says so; applying the push GitHub's compare shows",
		"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "approved", approved, "head", pr.HeadSHA, "paths", *push.ChangedPaths)
	eventID := fmt.Sprintf("approval-carry:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, pr.Repo, pr.Number, time.Now().UnixNano())
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, push, w.handlers...); err != nil {
		w.log.Warn("apply a handoff-only push read from GitHub's compare", "repo", pr.Repo, "pullRequest", pr.Number, "error", err)
	}
	return false
}

// approvalSetAside is the head the reviewer of listed's issue approved, and the pull request as
// recorded now, when that approval is set aside at the recorded head with no push that may change
// code on its way: the issue is in reviewing, listed is still its open pull request, the reviewer's
// recorded decision is an approval naming a head, and the approval does not stand. A nil pull
// request says it is not. The record is read afresh, since the pass's earlier reads of listed (a
// head it followed, a review it applied) can have changed it.
func (w *workflowRuntime) approvalSetAside(ctx context.Context, listed record.PullRequest) (string, *record.PullRequest, error) {
	var approved string
	var pr *record.PullRequest
	err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		issue, err := w.records.Issue(ctx, tx, listed.Issue)
		if err != nil || issue == nil || issue.Phase != phase.Reviewing {
			return err
		}
		current, err := w.records.PullRequest(ctx, tx, listed.Issue)
		if err != nil || current == nil || current.Number != listed.Number || current.State != record.PullRequestOpen {
			return err
		}
		rows, err := w.records.Phases(ctx, tx, listed.Issue)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Role != claim.RoleReviewer || row.Decision == nil || row.Decision.State != "approved" || row.Decision.Head == "" {
				continue
			}
			if !classify.CodeOnItsWay(*current) && !classify.ApprovalStands(*current, row.Decision.Head) {
				approved, pr = row.Decision.Head, current
			}
		}
		return nil
	})
	return approved, pr, err
}
