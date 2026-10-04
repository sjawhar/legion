package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// reviewPermissionTTL is how long a permission GitHub answered stands for its repository and login
// before GitHub is asked again. Any account can review a public repository's pull request, and
// every deciding review would otherwise cost a call on the review App's installation, whose hourly
// limit the reviewer's panes share: within it an account costs one call, however many reviews it
// submits. Its cost is a permission changed within it, read as it was: a writer whose access was
// just removed can still decide a round, and an account just given write access, whose review was
// read without it, decides nothing until it passes.
const reviewPermissionTTL = 5 * time.Minute

// permissionKey is one cached answer's repository and login, both lowercased, since GitHub matches
// either in any case.
type permissionKey struct{ repository, login string }

// permissionAnswer is one cached answer, standing until until.
type permissionAnswer struct {
	canWrite bool
	until    time.Time
}

// reviewerCanWrite answers intake's ConsumerSpec.ReviewPermission: whether review's author has
// write access or higher to its repository, which is what lets a review decide a review round. It
// reads GitHub's collaborator permission for the user
// (`GET /repos/{owner}/{repo}/collaborators/{username}/permission`), which every App's Metadata
// read permission reaches, with the review App's installation token. The answer's `permission` is
// GitHub's legacy summary of the role: `admin` or `write` is write access or higher - a maintainer
// reads as `write` there - and `read`, `none` or anything else is not. An answer for another
// account than the author (GitHub resolves some logins to another user) is not either, and logged.
//
// GitHub is not asked about the review App's own reviews, which decide by its login alone
// (workflow's decidesRound), nor about a review on a pull request the daemon does not record, which
// the workflow drops; both are answered false. An answer stands for reviewPermissionTTL.
//
// An account GitHub does not know on the repository is answered `404`: no write access, logged,
// and the review decides nothing. A `403` that is not GitHub's rate limit is no write access too,
// but it says the review App's installation may not read collaborators at all, so no author but
// the review App decides until that is fixed: it is logged at error, naming the installation. A
// rate limit is returned as an intake.RetryLater naming the wait GitHub asks for, which intake
// waits out before the review's delivery is tried again. Any other failure - a mint that failed, a
// network error, a 5xx, the pull request's record unread - is returned, and intake retries the
// delivery after its nak delay rather than applying it with a permission nobody read.
func (w *workflowRuntime) reviewerCanWrite(ctx context.Context, review intake.PullRequestReview) (bool, error) {
	if review.Author == w.reviewAppLogin {
		return false, nil
	}
	var pr *record.PullRequest
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		pr, err = w.records.PullRequestByNumber(ctx, tx, review.Repo, review.Number)
		return err
	}); err != nil {
		return false, fmt.Errorf("read the record of pull request %s#%d: %w", review.Repo, review.Number, err)
	}
	if pr == nil {
		return false, nil
	}
	repository, err := ghrepo.Parse("the review's repository", review.Repo)
	if err != nil {
		return false, err
	}
	key := permissionKey{repository: strings.ToLower(repository.String()), login: strings.ToLower(review.Author)}
	if canWrite, ok := w.cachedPermission(key); ok {
		return canWrite, nil
	}
	canWrite, err := w.readPermission(ctx, repository, review.Author)
	if err != nil {
		return false, err
	}
	w.cachePermission(key, canWrite)
	return canWrite, nil
}

// readPermission reads login's permission on repository from GitHub, as reviewerCanWrite says.
func (w *workflowRuntime) readPermission(ctx context.Context, repository ghrepo.Repository, login string) (bool, error) {
	lease, err := w.tokens.Token(ctx, appauth.Review, repository.Owner())
	if err != nil {
		return false, fmt.Errorf("mint the review App token for %s: %w", repository.Owner(), err)
	}
	client := githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(w.githubAPI, repository)}
	var answer struct {
		Permission string `json:"permission"`
		User       struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := client.Get(ctx, "/collaborators/"+url.PathEscape(login)+"/permission", &answer); err != nil {
		var refused *githubrest.Answer
		if !errors.As(err, &refused) {
			return false, fmt.Errorf("read %s's permission on %s: %w", login, repository.String(), err)
		}
		switch {
		case refused.RateLimited:
			return false, &intake.RetryLater{After: refused.RetryAfter,
				Err: fmt.Errorf("read %s's permission on %s: GitHub's rate limit, retry in %s: %w", login, repository.String(), refused.RetryAfter, err)}
		case refused.Status == http.StatusNotFound:
			w.log.Info("workflow: GitHub gives the review's author no write access to the repository",
				"repo", repository.String(), "login", login, "status", refused.Status)
			return false, nil
		case refused.Status == http.StatusForbidden:
			w.log.Error("workflow: GitHub refuses the review App's installation a review author's repository permission, so the review decides nothing; no review but the review App's decides a round until the installation may read the repository's collaborators",
				"app", appauth.Review, "bot", lease.Identity.Name, "installation_owner", repository.Owner(), "repo", repository.String(), "login", login, "status", refused.Status, "body", refused.Body)
			return false, nil
		}
		return false, fmt.Errorf("read %s's permission on %s: %w", login, repository.String(), err)
	}
	if !strings.EqualFold(answer.User.Login, login) {
		w.log.Warn("workflow: GitHub answered the repository permission of another account than the review's author, so the review decides nothing",
			"repo", repository.String(), "login", login, "answered_login", answer.User.Login, "permission", answer.Permission)
		return false, nil
	}
	if answer.Permission != "admin" && answer.Permission != "write" {
		w.log.Info("workflow: GitHub gives the review's author no write access to the repository",
			"repo", repository.String(), "login", login, "permission", answer.Permission)
		return false, nil
	}
	return true, nil
}

// cachedPermission is the answer cached for key, if one still stands.
func (w *workflowRuntime) cachedPermission(key permissionKey) (bool, bool) {
	w.permissionsMu.Lock()
	defer w.permissionsMu.Unlock()
	answer, ok := w.permissions[key]
	if !ok || !time.Now().Before(answer.until) {
		return false, false
	}
	return answer.canWrite, true
}

// cachePermission keeps canWrite for key for the permission TTL, and drops every answer that no
// longer stands, so the cache holds no more than the accounts that reviewed within one TTL.
func (w *workflowRuntime) cachePermission(key permissionKey, canWrite bool) {
	ttl := w.permissionTTL
	if ttl == 0 {
		ttl = reviewPermissionTTL
	}
	now := time.Now()
	w.permissionsMu.Lock()
	defer w.permissionsMu.Unlock()
	if w.permissions == nil {
		w.permissions = map[permissionKey]permissionAnswer{}
	}
	for cached, answer := range w.permissions {
		if !now.Before(answer.until) {
			delete(w.permissions, cached)
		}
	}
	w.permissions[key] = permissionAnswer{canWrite: canWrite, until: now.Add(ttl)}
}
