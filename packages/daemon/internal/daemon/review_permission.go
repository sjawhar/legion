package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// reviewPermissionRead bounds one permission read: a single GitHub answer, which comes in well
// under a second.
const reviewPermissionRead = 15 * time.Second

// reviewerCanWrite answers intake's ConsumerSpec.ReviewPermission: whether login has write access
// or higher to repository, which is what lets a review decide a review round. It reads GitHub's
// collaborator permission for the user
// (`GET /repos/{owner}/{repo}/collaborators/{username}/permission`), which every App's Metadata
// read permission reaches, with the review App's installation token. The answer's `permission` is
// GitHub's legacy summary of the role: `admin` or `write` is write access or higher - a maintainer
// reads as `write` there - and `read`, `none` or anything else is not.
//
// The review App's own reviews decide by its login alone (workflow's decidesRound), so its login is
// answered without a GitHub call. An account GitHub does not know on the repository is answered
// `404`, and one whose permission this installation may not read `403`: both are no write access,
// logged, and decide nothing. Any other failure - a mint that failed, a network error, a rate
// limit, a 5xx - is returned, and intake retries the review's whole delivery after its nak delay
// rather than applying it with a permission nobody read.
func (w *workflowRuntime) reviewerCanWrite(ctx context.Context, repository ghrepo.Repository, login string) (bool, error) {
	if login == "" || login == w.reviewAppLogin {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, reviewPermissionRead)
	defer cancel()
	lease, err := w.tokens.Token(ctx, appauth.Review, repository.Owner())
	if err != nil {
		return false, fmt.Errorf("mint the review App token for %s: %w", repository.Owner(), err)
	}
	client := githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(w.githubAPI, repository)}
	var answer struct {
		Permission string `json:"permission"`
	}
	if err := client.Get(ctx, "/collaborators/"+url.PathEscape(login)+"/permission", &answer); err != nil {
		var refused *githubrest.Answer
		if errors.As(err, &refused) && (refused.Status == http.StatusNotFound || refused.Status == http.StatusForbidden) {
			w.log.Info("workflow: GitHub gives the review's author no write access to the repository",
				"repo", repository.String(), "login", login, "status", refused.Status)
			return false, nil
		}
		return false, fmt.Errorf("read %s's permission on %s: %w", login, repository.String(), err)
	}
	if answer.Permission != "admin" && answer.Permission != "write" {
		w.log.Info("workflow: GitHub gives the review's author no write access to the repository",
			"repo", repository.String(), "login", login, "permission", answer.Permission)
		return false, nil
	}
	return true, nil
}
