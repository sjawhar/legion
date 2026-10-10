package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// reviewBody answers intake's ConsumerSpec.ReviewBody: review's body, read fresh from GitHub when
// Envoy's normalizer capped it (intake.PullRequestReview.BodyTruncated) and restored unchanged
// otherwise. Only the review App's own reviews can carry the Legion footer the workflow reads
// (byReviewer), so no other author's review is ever looked up: its capped body, whatever it says,
// decides nothing more than its full one would.
//
// The read goes through GitHub's REST review (`GET /pulls/{number}/reviews/{id}`), with the review
// App's installation token (readPermission's own mint, reused rather than duplicated): the same
// transient mint failure and rate limit are returned as an intake.RetryLater, so intake waits
// GitHub out before the delivery is tried again rather than applying a review whose footer it
// could not restore. Any other failure is returned too, and intake retries the delivery after its
// nak delay.
func (w *workflowRuntime) reviewBody(ctx context.Context, review intake.PullRequestReview) (string, error) {
	if review.Author != w.reviewAppLogin {
		return review.Body, nil
	}
	repository, err := ghrepo.Parse("the review's repository", review.Repo)
	if err != nil {
		return "", err
	}
	lease, err := w.tokens.Token(ctx, appauth.Review, repository.Owner())
	if err != nil {
		var transient *appauth.TransientError
		if errors.As(err, &transient) {
			return "", &intake.RetryLater{After: time.Minute,
				Err: fmt.Errorf("mint the review App token for %s: a transient failure minting it, retry in %s: %w", repository.Owner(), time.Minute, err)}
		}
		return "", fmt.Errorf("mint the review App token for %s: %w", repository.Owner(), err)
	}
	client := githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(w.githubAPI, repository)}
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
