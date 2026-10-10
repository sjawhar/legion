package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// githubReview is one review as GitHub lists a pull request's reviews (GET /pulls/{number}/reviews):
// its id, its author, its body, its state (APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED or
// PENDING), the commit it was written on, and when it was submitted, which a pending review has not
// been.
type githubReview struct {
	ID   int64 `json:"id"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	CommitID    string    `json:"commit_id"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// readReviews is the required-checks pass's read of pr's reviews while its issue's review round is
// open (workflow.ReviewRoundOpen). The daemon otherwise learns a decision only from its
// pull_request_review webhook, and a delivery that never reaches the notification stream leaves the
// round open with the decision on GitHub; a restart loses nothing, since the daemon's durable
// consumer replays what the stream holds, so the read is for the delivery the stream never got. It
// reads every page of the pull request's reviews with github, the implement App's client the pass
// read the pull request with, and applies the review its round may still take (reviewToApply) as
// the intake.PullRequestReview fact the webhook would have carried, which the workflow decides as it
// decides the webhook's. A round open for a pass costs one read per hundred of its reviews, about
// thirty an hour, on the installation the rest of the pass reads with; a round that is not open
// costs none.
//
// Nothing beyond the workflow's own order keeps the read and the webhook from deciding twice. Both
// carry GitHub's review id and submission time, so whichever copy is applied first records the
// review as the pull request's newest deciding one (record.PullRequest.ReviewSeen), and the other is
// not after it and records nothing; facts apply one at a time (intake.ApplyFact), so the two copies
// never both pass that order. A review of a round that already ended is no newer than the mark
// either, so it neither reopens that round nor decides a later one.
//
// ctx bounds the apply, and read the reads before it. It says whether the pass must stop: ctx
// ended, or GitHub answered the reviews read with its rate limit, which every later read of the
// pass on the same installation meets. Any other failure is logged and leaves the round as it stood
// for the next pass to read again.
func (w *workflowRuntime) readReviews(ctx, read context.Context, github githubrest.Client, pr record.PullRequest) bool {
	issue, err := w.recordedIssue(read, pr.Issue)
	if err != nil {
		if ctx.Err() != nil {
			return true
		}
		w.log.Error("read the issue of a pull request to read its reviews; a decision its webhook never delivered stays unread until a later pass reads it",
			"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "error", err)
		return false
	}
	if issue == nil || !workflow.ReviewRoundOpen(*issue) {
		return false
	}
	reviews, err := githubrest.GetPages[githubReview](read, github, fmt.Sprintf("/pulls/%d/reviews", pr.Number))
	if err != nil {
		if ctx.Err() != nil {
			return true
		}
		attributes := []any{"repo", pr.Repo, "pullRequest", pr.Number, "error", err}
		var answer *githubrest.Answer
		if errors.As(err, &answer) {
			attributes = append(attributes, "status", answer.Status)
		}
		if answer != nil && answer.RateLimited {
			w.log.Error("read a pull request's reviews: GitHub's rate limit; this pass reads no more pull requests", attributes...)
			return true
		}
		w.log.Error("read a pull request's reviews; a decision its webhook never delivered stays unread until a later pass reads it", attributes...)
		return false
	}
	review, ok, err := w.reviewToApply(read, pr, reviews)
	if err != nil {
		if ctx.Err() != nil {
			return true
		}
		w.log.Warn("read whether a review's author may decide its round; a later pass reads it again",
			"repo", pr.Repo, "pullRequest", pr.Number, "review", review.ID, "author", review.Author, "error", err)
		return false
	}
	if !ok {
		return false
	}
	// A deciding review the pull request's record had not seen when the pass listed it: its webhook
	// was lost, or is still on its way, and whichever copy comes second records nothing.
	w.log.Warn("workflow: GitHub holds a deciding review the daemon has not recorded; applying it as its webhook would",
		"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "review", review.ID, "author", review.Author, "state", review.State, "submitted_at", review.SubmittedAt)
	eventID := fmt.Sprintf("review-read:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, pr.Repo, pr.Number, time.Now().UnixNano())
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, review, w.handlers...); err != nil {
		w.log.Warn("apply a review read from GitHub", "repo", pr.Repo, "pullRequest", pr.Number, "review", review.ID, "error", err)
	}
	return false
}

// reviewToApply is the review of reviews, GitHub's list of pr's, that the workflow would hold as its
// round's decision had every webhook arrived. It is the newest by submission time, then id
// (record.ReviewOrder), of the reviews that decide - an approval or a request for changes; a
// comment, a dismissed review and a pending draft decide nothing - that are newer than the pull
// request's newest deciding review as the pass listed it (record.PullRequest.ReviewSeen), and whose
// author is the review App or, read with the webhook's own resolver (reviewerCanWrite), an account
// with write access to the repository. GitHub lists reviews in the order they were created, and a
// draft keeps its place when it is submitted later, so the list's order decides nothing. Only the
// newest is applied: once the reviewer has completed, the first decision applied ends the round,
// where webhooks delivered in full leave the newest deciding it. Its body is the review's whole
// body, where the webhook's envelope carries its first 2,048 characters. It says false when no
// review qualifies, and returns the resolver's error, which ends the search.
func (w *workflowRuntime) reviewToApply(ctx context.Context, pr record.PullRequest, reviews []githubReview) (intake.PullRequestReview, bool, error) {
	var candidates []intake.PullRequestReview
	for _, listed := range reviews {
		review := intake.PullRequestReview{Repo: pr.Repo, Number: pr.Number, ID: listed.ID, SubmittedAt: listed.SubmittedAt,
			State: strings.ToLower(listed.State), CommitID: listed.CommitID, Author: listed.User.Login, Body: listed.Body}
		order := record.ReviewOrder{SubmittedAt: review.SubmittedAt, ID: review.ID}
		if review.ID == 0 || review.SubmittedAt.IsZero() || review.Author == "" || !review.Decides() || !order.After(pr.ReviewSeen) {
			continue
		}
		candidates = append(candidates, review)
	}
	slices.SortFunc(candidates, func(a, b intake.PullRequestReview) int {
		return cmp.Or(b.SubmittedAt.Compare(a.SubmittedAt), cmp.Compare(b.ID, a.ID))
	})
	for _, review := range candidates {
		if w.reviewAppLogin != "" && review.Author == w.reviewAppLogin {
			return review, true, nil
		}
		canWrite, err := w.reviewerCanWrite(ctx, review)
		if err != nil {
			return review, false, err
		}
		if canWrite {
			review.AuthorCanWrite = true
			return review, true, nil
		}
	}
	return intake.PullRequestReview{}, false, nil
}
