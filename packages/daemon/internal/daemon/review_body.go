package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
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
// is made, as reviewerCanWrite's permission read is; the limit is held the same way (holdIfLimited)
// whichever of the mint or the read itself meets it.
func (w *workflowRuntime) reviewBody(ctx context.Context, review intake.PullRequestReview) (string, error) {
	if review.Author != w.reviewAppLogin {
		return review.Body, nil
	}
	repository, recorded, err := w.reviewedRepository(ctx, review)
	if err != nil {
		return "", err
	}
	if !recorded {
		return review.Body, nil
	}
	if left := w.rateLimitLeft(); left > 0 {
		return "", rateLimitRefusal(left, fmt.Sprintf("read review %d's body on %s#%d", review.ID, repository.String(), review.Number))
	}
	client, _, err := w.reviewAppClient(ctx, repository)
	if err != nil {
		return "", w.holdIfLimited(err)
	}
	var answer struct {
		Body string `json:"body"`
	}
	if err := client.Get(ctx, "/pulls/"+strconv.Itoa(review.Number)+"/reviews/"+strconv.FormatInt(review.ID, 10), &answer); err != nil {
		var refused *githubrest.Answer
		if errors.As(err, &refused) && refused.RateLimited {
			return "", w.holdIfLimited(&intake.RetryLater{After: refused.RetryAfter,
				Err: fmt.Errorf("read review %d's body on %s#%d: GitHub's rate limit, retry in %s: %w", review.ID, repository.String(), review.Number, refused.RetryAfter, err)})
		}
		return "", fmt.Errorf("read review %d's body on %s#%d: %w", review.ID, repository.String(), review.Number, err)
	}
	return answer.Body, nil
}
