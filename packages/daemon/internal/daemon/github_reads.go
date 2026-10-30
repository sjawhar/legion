package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// githubPullRequest is one pull request as GitHub answers GET /pulls/{number} and lists it in
// GET /pulls: its number, state (open or closed), page, body, base branch, head branch and commit,
// when it last changed, its nullable, lazily computed `mergeable`, which the list leaves out, and
// `mergeable_state`, GitHub's own merge state for the head, which the list leaves out too.
type githubPullRequest struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	Base    struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	UpdatedAt      time.Time `json:"updated_at"`
	Mergeable      *bool     `json:"mergeable"`
	MergeableState string    `json:"mergeable_state"`
}

// mergeability is the pull request's `mergeable` as the mergeability fact reads it: null
// is record.MergeabilityUnknown, GitHub not having computed it yet; true is
// record.MergeabilityMergeable; false is record.MergeabilityConflicting, the state that sends the
// issue back to implementing from every phase after the implementer's (workflow's
// mergeability).
func (p githubPullRequest) mergeability() record.Mergeability {
	switch {
	case p.Mergeable == nil:
		return record.MergeabilityUnknown
	case *p.Mergeable:
		return record.MergeabilityMergeable
	default:
		return record.MergeabilityConflicting
	}
}

// mergeState is the pull request's `mergeable_state` as the record keeps it: GitHub's own merge
// state for the head, in GitHub's spelling, which the daemon records and decides nothing on yet.
// An answer with no `mergeable_state` at all - a list entry, which leaves it out - is
// record.MergeStateUnread, since the read found none, and a spelling outside the eight GitHub
// documents is record.MergeStateUnknown, the one state that already means "GitHub has not said
// this head can be merged": the record's values are then exactly the set the state view and the
// contract name, and an unrecognised state is never read as one the issue may advance on
// (record.MergeState.Mergeable).
func (p githubPullRequest) mergeState() record.MergeState {
	switch state := record.MergeState(p.MergeableState); state {
	case record.MergeStateUnread, record.MergeStateClean, record.MergeStateUnstable, record.MergeStateHasHooks,
		record.MergeStateBlocked, record.MergeStateDirty, record.MergeStateBehind, record.MergeStateDraft:
		return state
	default:
		return record.MergeStateUnknown
	}
}

// readPullRequest reads pr from one GET /pulls/{number}: the base the mergeability is recorded
// against, that mergeability itself, and the head and its clock followHead compares with the
// record, in the single read they share. It also returns the githubrest.Client its own token mint
// built, so the per-issue read's further reads of the same pull request reuse it rather than
// minting a second token.
func (w *workflowRuntime) readPullRequest(ctx context.Context, pr record.PullRequest) (githubrest.Client, githubPullRequest, error) {
	repository, err := ghrepo.Parse("the pull request's repository", pr.Repo)
	if err != nil {
		return githubrest.Client{}, githubPullRequest{}, err
	}
	github, err := w.implementClient(ctx, repository)
	if err != nil {
		return githubrest.Client{}, githubPullRequest{}, err
	}
	var pull githubPullRequest
	if err := github.Get(ctx, fmt.Sprintf("/pulls/%d", pr.Number), &pull); err != nil {
		return githubrest.Client{}, githubPullRequest{}, err
	}
	return github, pull, nil
}

// implementClient is a client for repository's REST API on the implement App's installation token.
func (w *workflowRuntime) implementClient(ctx context.Context, repository ghrepo.Repository) (githubrest.Client, error) {
	lease, err := w.tokens.Token(ctx, appauth.Implement, repository.Owner())
	if err != nil {
		return githubrest.Client{}, fmt.Errorf("mint the implement App token for %s: %w", repository.Owner(), err)
	}
	return githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(w.githubAPI, repository)}, nil
}
