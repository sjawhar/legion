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

// ThreadsResolveRequest is the reviewer's resolve_threads: its grant and the GraphQL node ids of
// the review threads to resolve, at least one. The pull request is the one recorded for the issue
// the grant is for; the request names none.
type ThreadsResolveRequest struct {
	GrantID string   `json:"grantId"`
	Threads []string `json:"threads"`
}

// ThreadsResolveResponse is each named thread's outcome, in the order the request named them.
// When GitHub refused to resolve one, Refused names it and why, and Threads holds the ones before
// it, resolved: the run stopped there, and the ids after it were not written.
type ThreadsResolveResponse struct {
	Threads []reviewthreads.Outcome `json:"threads"`
	Refused *ThreadRefusal          `json:"refused,omitempty"`
}

// ThreadRefusal is the thread GitHub refused to resolve, by node id, and GitHub's message.
type ThreadRefusal struct {
	Thread string `json:"thread"`
	Error  string `json:"error"`
}

// resolveThreads resolves, for the reviewer, the review threads it names by node id on its issue's
// recorded pull request, as the implement App: GitHub lets only the pull request's author's App
// resolve its threads, and the implementer opens every Legion pull request, so the reviewer, who
// acts as the review App, cannot. The daemon reads no comment and judges nothing: the reviewer
// chose the threads, one by one, having answered each. Only the reviewer's grant may call it, on
// its issue's pull request alone, which the request does not name, since the daemon records it
// (an issue with none recorded is refused NO_PULL_REQUEST), and the implement App's token never
// leaves the daemon. The pull request's threads are listed once before any write: an id that is no
// thread of it refuses the whole request, naming every such id, and a thread GitHub already holds
// resolved is answered as resolved with reviewthreads.AlreadyResolved, so a retry after a partial
// run is idempotent. Each resolution is logged with its thread. A thread GitHub refuses to resolve
// stops the run, and the answer names it beside the outcomes before it, so the reviewer sees the
// threads already resolved; a read that fails before any write answers 502.
func (s *server) resolveThreads(w http.ResponseWriter, r *http.Request) {
	var req ThreadsResolveRequest
	if !readBody(w, r, &req) || !requireFailureFields(w, field{"grantId", req.GrantID}) {
		return
	}
	if len(req.Threads) == 0 {
		writeFailure(w, http.StatusBadRequest, "MISSING_FIELD", "threads is required: the node id of each review thread to resolve")
		return
	}
	for _, thread := range req.Threads {
		if strings.TrimSpace(thread) == "" {
			writeFailure(w, http.StatusBadRequest, "INVALID_THREAD", "every thread must be a review thread's node id, none empty")
			return
		}
	}
	grant, ok := s.redeem(w, req.GrantID)
	if !ok {
		return
	}
	if grant.Controller || grant.Role != claim.RoleReviewer {
		writeFailure(w, http.StatusForbidden, "REVIEWER_REQUIRED", "only the reviewer's grant has the daemon resolve review threads; the pull request's author resolves its own with gh")
		return
	}
	pr, err := s.issuePullRequest(r.Context(), grant.Issue)
	if err != nil {
		s.log.Error("api: read the pull request to resolve threads on", "issue", grant.Issue, "error", err)
		writeFailure(w, http.StatusInternalServerError, "RECORD_READ_FAILED", "could not read the issue's pull request")
		return
	}
	if pr == nil {
		writeFailure(w, http.StatusConflict, "NO_PULL_REQUEST", fmt.Sprintf("%s has no pull request recorded; nothing to resolve threads on", grant.Issue))
		return
	}
	repository, err := ghrepo.Parse("the recorded pull request's repository", pr.Repo)
	if err != nil {
		s.log.Error("api: the recorded pull request to resolve threads on is malformed", "issue", grant.Issue, "error", err)
		writeFailure(w, http.StatusInternalServerError, "RECORD_READ_FAILED", "could not read the issue's pull request")
		return
	}
	lease, ok := s.leaseForGrant(w, r, grant, appauth.Implement)
	if !ok {
		return
	}
	pullRequest := fmt.Sprintf("%s#%d", repository, pr.Number)
	outcomes, err := reviewthreads.Resolve(r.Context(), reviewthreads.TokenGraphQL(s.githubGraphQL, lease.Token), repository, pr.Number, req.Threads)
	for _, outcome := range outcomes {
		if outcome.Reason == "" {
			s.log.Info("api: resolved a review thread for the reviewer", "issue", grant.Issue, "pull_request", pullRequest, "thread", outcome.Thread)
		}
	}
	answer := ThreadsResolveResponse{Threads: outcomes}
	var foreign *reviewthreads.NotOnPullRequest
	var refused *reviewthreads.Refused
	switch {
	case errors.As(err, &foreign):
		writeFailure(w, http.StatusBadRequest, "THREAD_NOT_ON_PULL_REQUEST", fmt.Sprintf("%s: not review threads of %s, so nothing was resolved", strings.Join(foreign.Threads, ", "), pullRequest))
		return
	case errors.As(err, &refused):
		answer.Refused = &ThreadRefusal{Thread: refused.Thread, Error: refused.Err.Error()}
	case err != nil:
		writeFailure(w, http.StatusBadGateway, "THREADS_READ_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, answer)
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
