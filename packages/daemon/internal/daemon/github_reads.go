package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// logReadFailure logs err, a GitHub read of the per-issue read that failed, at error level with
// attributes, then err and GitHub's HTTP status when err carries one: as limited when the answer
// was GitHub's rate limit, and as failed otherwise. It returns true for the rate limit: every
// further read on the same installation meets the same limit, and the panes share it, so the
// caller must stop the whole read there.
func (w *workflowRuntime) logReadFailure(err error, limited, failed string, attributes ...any) bool {
	attributes = append(attributes, "error", err)
	var answer *githubrest.Answer
	if errors.As(err, &answer) {
		attributes = append(attributes, "status", answer.Status)
	}
	if answer != nil && answer.RateLimited {
		w.log.Error(limited, attributes...)
		return true
	}
	w.log.Error(failed, attributes...)
	return false
}

// readEventID is the event id, under source "github", of a fact a read of GitHub builds about
// pull request number of repo: kind, the Dispatch project, the pull request, and observed, what
// the read saw - a review's id, GitHub's submitted_at, the two heads of a compare. It holds no
// read time, no boot and nothing of who made the read, so one observation is one fact however
// many reads, boots, daemons or outbox rows see it, and unapplied finds it applied before any
// transaction. The per-issue read and the outbox row that make the same read of GitHub share
// this id, which is what keeps them from applying one observation twice (approvalCarryEvent).
func readEventID(kind, project, repo string, number int, observed ...string) string {
	return fmt.Sprintf("%s:%s:%s#%d:%s", kind, project, repo, number, strings.Join(observed, ":"))
}

// clock is GitHub's time t as an event id spells it.
func clock(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// unapplied says whether processed_events holds no eventID under source "github"
// (intake.Applied), read with no transaction and no fact lock: a fact an earlier read, another
// boot or a webhook applied already costs that one read and no wait behind the GitHub consumer's
// facts. A failed read is logged and says false, leaving the fact to a later read, as a failed
// apply would.
func (w *workflowRuntime) unapplied(ctx context.Context, eventID string) bool {
	applied, err := intake.Applied(ctx, w.pool, "github", []string{eventID})
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("read whether a fact read from GitHub is applied; a later read applies it", "event", eventID, "error", err)
		}
		return false
	}
	return !applied[eventID]
}

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
// state for the head, in GitHub's spelling, which is what an approval standing on that head
// waits for (workflow's reviewRound).
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
	github, err := implementClient(ctx, w.tokens, w.githubAPI, repository)
	if err != nil {
		return githubrest.Client{}, githubPullRequest{}, fmt.Errorf("mint the implement App token for %s: %w", repository.Owner(), err)
	}
	var pull githubPullRequest
	if err := github.Get(ctx, fmt.Sprintf("/pulls/%d", pr.Number), &pull); err != nil {
		return githubrest.Client{}, githubPullRequest{}, err
	}
	return github, pull, nil
}

// implementClient is the client for repository's REST API on the implement App's installation
// token: the App every read of an issue's own repository is made as, the per-issue reads' and
// the outbox rows' alike. The mint failure is returned bare, so each caller says in its own
// words what it was minting the token for.
func implementClient(ctx context.Context, tokens appauth.Tokens, githubAPI string, repository ghrepo.Repository) (githubrest.Client, error) {
	lease, err := tokens.Token(ctx, appauth.Implement, repository.Owner())
	if err != nil {
		return githubrest.Client{}, err
	}
	return githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(githubAPI, repository)}, nil
}
