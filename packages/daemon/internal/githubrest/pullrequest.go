package githubrest

import (
	"context"
	"net/url"
	"time"
)

// PullRequest is one pull request as GitHub answers GET /pulls/{number} and lists it in GET
// /pulls: its number, state (open or closed), page, body, base branch, head branch and commit,
// when it last changed, and its nullable, lazily computed `mergeable`, which the list leaves out.
type PullRequest struct {
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
	UpdatedAt time.Time `json:"updated_at"`
	Mergeable *bool     `json:"mergeable"`
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
