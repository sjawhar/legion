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

// reviewPermissionTTL is how long an account GitHub gave no write access stands so for its
// repository and login before GitHub is asked again. Any account can review a public repository's
// pull request, and every deciding review would otherwise cost a call on the review App's
// installation, whose hourly limit the reviewer's panes share: within the window an account costs
// one call, however many reviews it submits. Its cost is an account just given write access, whose
// review was read without it, deciding nothing until the window passes. Write access is never
// kept: a writer reviews rarely, so keeping it would save almost nothing, and it would let a writer
// whose access was just removed decide, and skip the check that GitHub's answer names the review's
// author.
const reviewPermissionTTL = 5 * time.Minute

// permissionKey is one kept answer's repository and login, both lowercased, since GitHub matches
// either in any case.
type permissionKey struct{ repository, login string }

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
// the workflow drops; both are answered false. An account answered no write access stands so for
// reviewPermissionTTL; write access is never kept (reviewPermissionTTL's comment).
//
// An account GitHub does not know on the repository is answered `404`: no write access, logged,
// and the review decides nothing. A `403` that is not GitHub's rate limit is no write access too,
// but it says the review App's installation may not read collaborators at all, so no author but
// the review App decides until that is fixed: it is logged at error, naming the installation. A
// rate limit is returned as an intake.RetryLater naming the wait GitHub asks for, which intake
// waits out before the review's delivery is tried again, and is held: while it stands, every
// account with no answer kept is answered with the wait left and no call is made, since GitHub
// warns that calling on through a limit can get an integration banned. Any other failure - a mint
// that failed, a network error, a 5xx, the pull request's record unread - is returned, and intake
// retries the delivery after its nak delay rather than applying it with a permission nobody read.
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
	if w.noWriteAccess(key) {
		return false, nil
	}
	if left := w.rateLimitLeft(); left > 0 {
		return false, &intake.RetryLater{After: left,
			Err: fmt.Errorf("read %s's permission on %s: GitHub's rate limit stands for another %s, so this read was not made", review.Author, repository.String(), left)}
	}
	canWrite, err := w.readPermission(ctx, repository, review.Author)
	if err != nil {
		var later *intake.RetryLater
		if errors.As(err, &later) {
			w.holdRateLimit(later.After)
		}
		return false, err
	}
	if !canWrite {
		w.keepNoWriteAccess(key)
	}
	return canWrite, nil
}

// readPermission reads login's permission on repository from GitHub, as reviewerCanWrite says. A
// mint that fails as GitHub's trouble rather than an answer (appauth.TransientError) is returned as
// an intake.RetryLater naming a minute's wait, the same hold a rate-limited permission read sets
// (reviewerCanWrite's errors.As), since minting on through the App's own rate limit risks the same
// ban GitHub warns a repeated call does. A mint that fails for any other reason is returned as is.
func (w *workflowRuntime) readPermission(ctx context.Context, repository ghrepo.Repository, login string) (bool, error) {
	lease, err := w.tokens.Token(ctx, appauth.Review, repository.Owner())
	if err != nil {
		var transient *appauth.TransientError
		if errors.As(err, &transient) {
			return false, &intake.RetryLater{After: time.Minute,
				Err: fmt.Errorf("mint the review App token for %s: a transient failure minting it, retry in %s: %w", repository.Owner(), time.Minute, err)}
		}
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

// noWriteAccess says whether key is an account this repository gave no write access within the
// window that still stands.
func (w *workflowRuntime) noWriteAccess(key permissionKey) bool {
	w.permissionsMu.Lock()
	defer w.permissionsMu.Unlock()
	return time.Now().Before(w.permissions[key])
}

// keepNoWriteAccess keeps key as an account with no write access for the permission TTL, and drops
// every answer that no longer stands, so no more is held than the accounts that reviewed within one
// window.
func (w *workflowRuntime) keepNoWriteAccess(key permissionKey) {
	ttl := w.permissionTTL
	if ttl == 0 {
		ttl = reviewPermissionTTL
	}
	now := time.Now()
	w.permissionsMu.Lock()
	defer w.permissionsMu.Unlock()
	if w.permissions == nil {
		w.permissions = map[permissionKey]time.Time{}
	}
	for cached, until := range w.permissions {
		if !now.Before(until) {
			delete(w.permissions, cached)
		}
	}
	w.permissions[key] = now.Add(ttl)
}

// rateLimitLeft is how long GitHub's last rate limit still asks to be left alone, zero when none
// stands. GitHub's documentation warns that calling on while a limit stands can get an integration
// banned, so no read is made within it.
func (w *workflowRuntime) rateLimitLeft() time.Duration {
	w.permissionsMu.Lock()
	defer w.permissionsMu.Unlock()
	return time.Until(w.limitedUntil)
}

// holdRateLimit records that GitHub's rate limit stands for another after, keeping the later of
// this limit and one already held.
func (w *workflowRuntime) holdRateLimit(after time.Duration) {
	until := time.Now().Add(after)
	w.permissionsMu.Lock()
	defer w.permissionsMu.Unlock()
	if until.After(w.limitedUntil) {
		w.limitedUntil = until
	}
}
