package daemon

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// approvalCarryEvent is the one event id the carry of a reviewer's approval across a handoff-only
// push is applied under, whichever of the two makes it: the per-issue read (readApprovalCarry) or
// the outbox row the push queued (outbox.approvalCarry). Both ask GitHub the same question about
// the same two commits and apply the same fact, so both must apply it under the same id and the
// same source, and whichever gets there first is the one apply (readEventID, source "github").
func approvalCarryEvent(project string, pr record.PullRequest, approved string) string {
	return readEventID("approval-carry", project, pr.Repo, pr.Number, approved+"..."+pr.HeadSHA)
}

// carryApproval is the compare that carries a reviewer's approval across a handoff-only push, and
// the only place either caller makes it: GitHub is asked to compare the head the reviewer
// approved with the head the pull request stands at (handoffPush) and, when only the issue's own
// handoffs changed between them, the intake.Push that says so is applied, which the workflow
// decides as it decides the webhook's (workflow's push) - the approval carries to the head and
// the round ends.
//
// It answers the push GitHub's compare showed, the reason the approval stays set aside ("" when
// the compare carried it), whether the carry got as far as applying the push, and the failure.
// carried is what tells the two failures apart: false with an error is GitHub refusing the
// compare, which a caller reads for a rate limit, and true with one is the apply, which is the
// daemon's own and says nothing about GitHub.
//
// The fact is applied under approvalCarryEvent, so the read and the row carry one approval once
// however many of them run, and a retried row applies nothing twice. Nothing here logs: the two
// callers log in their own words, one as a read of an issue and one as a row.
func carryApproval(ctx context.Context, pool *pgxpool.Pool, project string, github githubrest.Client, pr record.PullRequest, approved string, handlers []intake.Handler) (push intake.Push, aside string, carried bool, err error) {
	push, aside, err = handoffPush(ctx, github, pr, approved, pr.HeadSHA)
	if err != nil || aside != "" {
		return push, aside, false, err
	}
	_, err = intake.ApplyFact(ctx, pool, "github", approvalCarryEvent(project, pr, approved), push, handlers...)
	return push, "", true, err
}

// readApprovalCarry is the per-issue read's carry of an approval left set aside below the pull
// request's head (classify.ApprovalSetAside): the reviewer approved a head other than the
// recorded one, no recorded push carries the approval up to it, and no push that may change code
// is on its way. The approved head can also lie between two recorded heads, as when followHead
// brought the record from an older head past it. The read makes the compare (carryApproval), and
// anything the compare does not carry leaves the approval set aside, as before. GitHub's answer
// for two fixed commits never changes, so one that does not carry the approval is logged once and
// not asked for again by this process (uncarried). It runs after followHead and readReviews, so
// it compares with the head GitHub shows and judges the decision a review just read recorded.
//
// ctx bounds the apply, and read the reads before it. It says whether the read must stop: ctx
// ended, or GitHub answered the compare with its rate limit. Any other failure is logged and
// applies nothing, and the next read of the issue asks again.
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
	if w.carryRefused(compared) || !w.unapplied(ctx, approvalCarryEvent(w.dispatchProject, *pr, approved)) {
		return false
	}
	// The compare's reads take read's bound and its apply ctx's, as every read of the issue does,
	// so a read that is being wound down applies nothing new.
	push, aside, carried, err := carryApproval(ctx, w.pool, w.dispatchProject, github, *pr, approved, w.handlers)
	switch {
	case err != nil && carried:
		w.log.Warn("apply a handoff-only push read from GitHub's compare", "repo", pr.Repo, "pullRequest", pr.Number, "error", err)
		return false
	case err != nil:
		if ctx.Err() != nil {
			return true
		}
		return w.logReadFailure(err,
			"compare a pull request's approved head with its head: GitHub's rate limit; this read asks GitHub no more",
			"compare a pull request's approved head with its head; the approval stays set aside until a later read of the issue or the push's webhook carries it",
			"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "approved", approved, "head", pr.HeadSHA)
	}
	if aside != "" {
		w.log.Info("workflow: the reviewer's approval stays set aside below the head; only an APPROVE of the head or a REQUEST_CHANGES ends the round", "reason", aside,
			"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "approved", approved, "head", pr.HeadSHA)
		w.refuseCarry(compared)
		return false
	}
	w.log.Warn("workflow: only the issue's handoffs changed above the approved head, and no recorded push says so; the push GitHub's compare shows is applied",
		"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "approved", approved, "head", pr.HeadSHA, "paths", *push.ChangedPaths)
	return false
}

// carryRefused is whether GitHub has already answered the comparison compared - a repository, a
// pull request and the two heads - with something that does not carry the approval, and
// refuseCarry records one it has: the compare of two fixed commits never changes its answer, so
// it is asked for once. Only the per-issue read keeps it, since only the read comes round to the
// same pull request again on its own; the outbox row that makes the same compare runs once for
// its two heads and is not retried after an answer (outbox.approvalCarry). The read can run on
// more than one goroutine, so the set is kept under readMemoMu.
func (w *workflowRuntime) carryRefused(compared string) bool {
	w.readMemoMu.Lock()
	defer w.readMemoMu.Unlock()
	return w.uncarried[compared]
}

func (w *workflowRuntime) refuseCarry(compared string) {
	w.readMemoMu.Lock()
	defer w.readMemoMu.Unlock()
	if w.uncarried == nil {
		w.uncarried = map[string]bool{}
	}
	w.uncarried[compared] = true
}

// approvalSetAside is the head the reviewer of listed's issue approved, and the pull request as
// recorded now, when that approval is set aside at the recorded head with no push that may change
// code on its way: the issue is in reviewing, listed is still its open pull request, the reviewer's
// recorded decision is an approval naming a head, and the approval does not stand. A nil pull
// request says it is not. The record is read afresh, since the read's earlier reads of listed (a
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
			if classify.ApprovalSetAside(*current, row.Decision.Head) {
				approved, pr = row.Decision.Head, current
			}
		}
		return nil
	})
	return approved, pr, err
}
