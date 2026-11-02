package githubrest

import (
	"context"
	"net/url"
	"strings"
	"time"
)

// PullRequest is one pull request as GitHub answers GET /pulls/{number} and lists it in GET
// /pulls: its number, state (open or closed), page, body, base branch, head branch, commit and the
// repository the head lives in (nil when GitHub no longer has it, a deleted fork), when it last
// changed, whether and when it merged and its merge commit, and its nullable, lazily computed
// `mergeable`, which the list leaves out, as it leaves out `merged` (merged_at says it there).
type PullRequest struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	Base    struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	UpdatedAt      time.Time  `json:"updated_at"`
	Mergeable      *bool      `json:"mergeable"`
	Merged         bool       `json:"merged"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
}

// IsMerged says whether GitHub shows the pull request merged: `merged` on GET /pulls/{number}, or
// a `merged_at`, which the list carries too.
func (p PullRequest) IsMerged() bool {
	return p.Merged || p.MergedAt != nil
}

// HeadIn says whether the pull request's head lives in repository (owner/name, in any case): a
// branch of the repository itself, not of a fork, whose branch may carry any name.
func (p PullRequest) HeadIn(repository string) bool {
	return p.Head.Repo != nil && strings.EqualFold(p.Head.Repo.FullName, repository)
}

// OpenPullRequests is every open pull request of the client's repository, as GET
// /pulls?state=open lists them, a hundred to a page (GetPages): one request per hundred open pull
// requests, whatever their branches.
func (c Client) OpenPullRequests(ctx context.Context) ([]PullRequest, error) {
	return GetPages[PullRequest](ctx, c, "/pulls?state=open")
}

// OpenPullRequestOn is the open pull request on branch of the client's repository, whose owner is
// owner (GET /pulls?head=<owner>:<branch>&state=open), and whether there is one. The list's filter
// is not taken on trust: a listed pull request counts only when its head.ref is branch, the same
// identity the webhook path reads (intake's decodePullRequest takes head_ref alone), whichever
// repository the head lives in. The workflow keeps one pull request per branch and assumes one is
// open at a time, so of two it takes the first GitHub lists.
func (c Client) OpenPullRequestOn(ctx context.Context, owner, branch string) (PullRequest, bool, error) {
	var pulls []PullRequest
	query := url.Values{"head": {owner + ":" + branch}, "state": {"open"}}
	if err := c.Get(ctx, "/pulls?"+query.Encode(), &pulls); err != nil {
		return PullRequest{}, false, err
	}
	for _, pull := range pulls {
		if pull.Head.Ref == branch {
			return pull, true, nil
		}
	}
	return PullRequest{}, false, nil
}
