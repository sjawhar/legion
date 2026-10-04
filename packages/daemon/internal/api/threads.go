package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewthreads"
)

// ThreadsResolveRequest is the reviewer's `legion threads resolve`: its grant, and the pull request
// it names, which must be the one recorded for the issue the grant is for.
type ThreadsResolveRequest struct {
	GrantID string `json:"grantId"`
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
}

// ThreadsResolveResponse is each unresolved review thread's outcome, in GitHub's order. When GitHub
// refused to resolve a thread the rule closes, Refused names it and why, and Threads holds the
// outcomes before it, the threads already resolved among them: the run stopped there.
type ThreadsResolveResponse struct {
	Threads []reviewthreads.Outcome `json:"threads"`
	Refused *ThreadRefusal          `json:"refused,omitempty"`
}

// ThreadRefusal is the thread GitHub refused to resolve, and GitHub's message.
type ThreadRefusal struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

// resolveThreads resolves, for the reviewer, each review thread on its issue's pull request that a
// bot outside Legion's role Apps opened and whose newest submitted comment is the Legion review
// App's Accepted: (reviewthreads.ReviewersAcceptanceOfABot), as the implement App: GitHub lets only
// the pull request's author's App resolve its threads, and the implementer opens every Legion pull
// request, so the reviewer, who acts as the review App, cannot. The reviewer is the party that
// adjudicates such a bot's finding, and a required review workflow that fails while its threads
// stand passes on a re-run only once they are resolved. Every other thread is left open, its
// outcome saying why: the daemon resolves nothing a Legion App opened (the implementer's and the
// merger's own `legion threads resolve` close those on their opener's Accepted:), and nothing whose
// newest comment is anything but that acceptance. Only the reviewer's grant may call it, for its
// issue's pull request alone, and the implement App's token never leaves the daemon. Each
// resolution is logged with the thread and whose acceptance closed it. A thread GitHub refuses to
// resolve stops the run, and the answer names it beside the outcomes before it, so the reviewer
// sees the threads already resolved; a read that fails before any write answers 502.
func (s *server) resolveThreads(w http.ResponseWriter, r *http.Request) {
	var req ThreadsResolveRequest
	if !readBody(w, r, &req) || !requireFailureFields(w, field{"grantId", req.GrantID}, field{"repo", req.Repo}) {
		return
	}
	repository, err := ghrepo.Parse("repo", req.Repo)
	if err != nil || req.Number <= 0 {
		writeFailure(w, http.StatusBadRequest, "INVALID_PULL_REQUEST", "repo must be owner/name and number a positive pull request number")
		return
	}
	grant, ok := s.redeem(w, req.GrantID)
	if !ok {
		return
	}
	if grant.Controller || grant.Role != claim.RoleReviewer {
		writeFailure(w, http.StatusForbidden, "REVIEWER_REQUIRED", "only the reviewer's grant has the daemon resolve review threads; every other role runs legion threads resolve as its own App")
		return
	}
	pr, err := s.issuePullRequest(r.Context(), grant.Issue)
	if err != nil {
		s.log.Error("api: read the pull request to resolve threads on", "issue", grant.Issue, "error", err)
		writeFailure(w, http.StatusInternalServerError, "RECORD_READ_FAILED", "could not read the issue's pull request")
		return
	}
	if pr == nil {
		writeFailure(w, http.StatusConflict, "NO_PULL_REQUEST", fmt.Sprintf("%s has no pull request recorded", grant.Issue))
		return
	}
	if !strings.EqualFold(pr.Repo, repository.String()) || pr.Number != req.Number {
		writeFailure(w, http.StatusForbidden, "PULL_REQUEST_NOT_THE_ISSUES", fmt.Sprintf("the grant is for %s, whose pull request is %s#%d, not %s#%d", grant.Issue, pr.Repo, pr.Number, repository, req.Number))
		return
	}
	apps, err := reviewthreads.AppsFrom(s.legionAppLogins(r.Context()))
	if err != nil || apps == nil {
		writeFailure(w, http.StatusBadGateway, "LEGION_APP_LOGINS_UNAVAILABLE", "could not read the logins of Legion's role Apps, so no thread can be told a bot's")
		return
	}
	lease, ok := s.leaseForGrant(w, r, grant, appauth.Implement)
	if !ok {
		return
	}
	outcomes, err := reviewthreads.Resolve(r.Context(), reviewthreads.TokenGraphQL(s.githubGraphQL, lease.Token), repository, req.Number,
		func(thread reviewthreads.Thread) (reviewthreads.Acceptance, string) {
			acceptance, reason := reviewthreads.Resolution(thread, apps)
			if acceptance == "" || acceptance == reviewthreads.ReviewersAcceptanceOfABot {
				return acceptance, reason
			}
			return "", string(acceptance) + ", which the implementer's or the merger's legion threads resolve closes: the daemon resolves only a bot's thread the Legion reviewer accepted"
		})
	// The daemon reads as the implement App, which GitHub shows the implementer's own drafts in a
	// pending review; the reviewer is told nothing of a thread whose newest comment is one.
	shown := outcomes[:0]
	for _, outcome := range outcomes {
		if outcome.LeftOpen == reviewthreads.PendingDraft {
			continue
		}
		if outcome.Resolved != "" {
			s.log.Info("api: resolved a review thread for the reviewer", "issue", grant.Issue, "pull_request", fmt.Sprintf("%s#%d", repository, req.Number),
				"thread", outcome.URL, "by", string(outcome.Resolved))
		}
		shown = append(shown, outcome)
	}
	outcomes = shown
	var refused *reviewthreads.Refused
	if errors.As(err, &refused) {
		writeJSON(w, http.StatusOK, ThreadsResolveResponse{Threads: outcomes, Refused: &ThreadRefusal{URL: refused.URL, Error: refused.Err.Error()}})
		return
	}
	if err != nil {
		writeFailure(w, http.StatusBadGateway, "THREADS_READ_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ThreadsResolveResponse{Threads: outcomes})
}

// issuePullRequest is the pull request recorded for issue, nil when it has none.
func (s *server) issuePullRequest(ctx context.Context, issue string) (*record.PullRequest, error) {
	if s.pool == nil || s.records == nil {
		return nil, errors.New("record dependencies are unavailable")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return s.records.PullRequest(ctx, tx, issue)
}
