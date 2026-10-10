package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// reviewBody answers intake's ConsumerSpec.ReviewBody: review's body, read fresh from GitHub when
// Envoy's normalizer capped it (intake.PullRequestReview.BodyTruncated) and restored unchanged
// otherwise. Only the review App's own reviews can carry the Legion footer the workflow reads
// (byReviewer), so no other author's review is ever looked up: its capped body, whatever it says,
// decides nothing more than its full one would. Nor is a review on a pull request the daemon does
// not record looked up, which the workflow drops unread.
//
// The read goes through GitHub's REST review (`GET /pulls/{number}/reviews/{id}`), with the review
// App's installation token (reviewAppClient, reused rather than duplicated): the same transient
// mint failure and rate limit are returned as an intake.RetryLater, so intake waits GitHub out
// before the delivery is tried again rather than applying a review whose footer it could not
// restore. While GitHub's rate limit stands, the read is answered with the wait left and no call
// is made, as reviewerCanWrite's permission read is; the limit is held the same way when the read
// itself meets it. Any other failure is returned too, and intake retries the delivery after its
// nak delay.
func (w *workflowRuntime) reviewBody(ctx context.Context, review intake.PullRequestReview) (string, error) {
	if review.Author != w.reviewAppLogin {
		return review.Body, nil
	}
	var pr *record.PullRequest
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		pr, err = w.records.PullRequestByNumber(ctx, tx, review.Repo, review.Number)
		return err
	}); err != nil {
		return "", fmt.Errorf("read the record of pull request %s#%d: %w", review.Repo, review.Number, err)
	}
	if pr == nil {
		return review.Body, nil
	}
	repository, err := ghrepo.Parse("the review's repository", review.Repo)
	if err != nil {
		return "", err
	}
	if left := w.rateLimitLeft(); left > 0 {
		return "", &intake.RetryLater{After: left,
			Err: fmt.Errorf("read review %d's body on %s#%d: GitHub's rate limit stands for another %s, so this read was not made", review.ID, repository.String(), review.Number, left)}
	}
	client, _, err := w.reviewAppClient(ctx, repository)
	if err != nil {
		var later *intake.RetryLater
		if errors.As(err, &later) {
			w.holdRateLimit(later.After)
		}
		return "", err
	}
	var answer struct {
		Body string `json:"body"`
	}
	if err := client.Get(ctx, "/pulls/"+strconv.Itoa(review.Number)+"/reviews/"+strconv.FormatInt(review.ID, 10), &answer); err != nil {
		var refused *githubrest.Answer
		if errors.As(err, &refused) && refused.RateLimited {
			return "", &intake.RetryLater{After: refused.RetryAfter,
				Err: fmt.Errorf("read review %d's body on %s#%d: GitHub's rate limit, retry in %s: %w", review.ID, repository.String(), review.Number, refused.RetryAfter, err)}
		}
		return "", fmt.Errorf("read review %d's body on %s#%d: %w", review.ID, repository.String(), review.Number, err)
	}
	return answer.Body, nil
}
