package githubapp

// commits.go holds the three reads a recorded check result's currency turns on: the pull
// requests GitHub names for a commit, whether a commit sits on a branch, and a repository's
// default branch. Each answers "Dispatch cannot see this repository" the same way
// (readRepositoryJSON), so what unreadable means is written once.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"
)

// readRepositoryJSON performs one GET under an installation token and answers its body, for a
// read of owner/repo whose answer is JSON. A 403 or 404 is ErrRepositoryUnreadable — the
// installation may not read this resource, or cannot see the repository at all — which the
// currency read takes as "unknown", never as "older"; a rate limit behind either status comes
// back as the *RateLimitError instead. Any other status is the error CheckResponse makes, named
// by the target, save one the caller names in handled, which comes back as its own status for
// the caller to read.
func (c *Client) readRepositoryJSON(ctx context.Context, token, target, owner, repo string, handled ...int) ([]byte, int, error) {
	body, status, header, err := c.do(ctx, c.tokenScope(token), http.MethodGet, target, "Bearer "+token)
	if err != nil {
		return nil, status, err
	}
	if status == http.StatusOK || slices.Contains(handled, status) {
		return body, status, nil
	}
	if status == http.StatusForbidden || status == http.StatusNotFound {
		if limited := RateLimit(status, header, body); limited != nil {
			return nil, status, limited
		}
		return nil, status, fmt.Errorf("%w: %s/%s", ErrRepositoryUnreadable, owner, repo)
	}
	if err := CheckResponse(status, header, body); err != nil {
		return nil, status, fmt.Errorf("GET %s: %w", target, err)
	}
	return body, status, nil
}

// CommitPull is one pull request GitHub names for a commit: what pull_request_heads stores
// about it, as GitHub's pull-request object carries it.
type CommitPull struct {
	Number    int
	Title     string
	Body      string
	URL       string
	State     string
	Merged    bool
	HeadSHA   string
	HeadRef   string
	BaseRef   string
	UpdatedAt *time.Time
}

// CommitPulls is every pull request GitHub associates with one commit
// (`GET /repos/{owner}/{repo}/commits/{sha}/pulls`), read under an installation token. Dispatch
// reads it when a result names a commit that is the head of no pull request it has an event
// for, so a pull request opened while Dispatch was not listening is known at once. A commit the
// repository does not have answers an empty list, as GitHub's own 422 for it does; a 403 or 404
// is ErrRepositoryUnreadable, which the currency read takes as "unknown", never as "older".
func (c *Client) CommitPulls(ctx context.Context, token, owner, repo, sha string) ([]CommitPull, error) {
	if c == nil {
		return nil, ErrNoAppKey
	}
	target := c.base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/commits/" + url.PathEscape(sha) + "/pulls"
	body, status, err := c.readRepositoryJSON(ctx, token, target, owner, repo, http.StatusUnprocessableEntity)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnprocessableEntity {
		return nil, nil
	}
	var payload []struct {
		Number    int        `json:"number"`
		Title     string     `json:"title"`
		Body      string     `json:"body"`
		HTMLURL   string     `json:"html_url"`
		State     string     `json:"state"`
		Merged    bool       `json:"merged"`
		MergedAt  *time.Time `json:"merged_at"`
		UpdatedAt *time.Time `json:"updated_at"`
		Head      struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode the pull requests of %s/%s@%s: %w", owner, repo, sha, err)
	}
	pulls := make([]CommitPull, len(payload))
	for index, pull := range payload {
		// The list form of a pull request carries merged_at but not merged, so a merge is read
		// from whichever of the two GitHub sent.
		pulls[index] = CommitPull{
			Number: pull.Number, Title: pull.Title, Body: pull.Body, URL: pull.HTMLURL,
			State: pull.State, Merged: pull.Merged || pull.MergedAt != nil,
			HeadSHA: pull.Head.SHA, HeadRef: pull.Head.Ref, BaseRef: pull.Base.Ref,
			UpdatedAt: pull.UpdatedAt,
		}
	}
	return pulls, nil
}

// CommitOnBranch answers whether sha is an ancestor of branch's head, or is that head
// (`GET /repos/{owner}/{repo}/compare/{branch}...{sha}`, whose `status` is `behind` when sha is
// behind the branch — that is, the branch holds it — and `identical` when it is the branch's
// head; `ahead` and `diverged` both mean the branch does not hold it). Only `status` is decoded:
// the comparison's commit list, which an unpaginated answer carries up to 250 of, is asked for
// one page of one so the body stays small, and `status` is a field of every page. A 403 or 404
// is ErrRepositoryUnreadable, which the currency read takes as "unknown"; a 404 is also GitHub's
// answer for a commit the repository does not have, which is likewise no proof that the branch
// lacks it.
func (c *Client) CommitOnBranch(ctx context.Context, token, owner, repo, branch, sha string) (bool, error) {
	if c == nil {
		return false, ErrNoAppKey
	}
	// The three dots are the comparison's own syntax and travel unescaped; each side is escaped
	// on its own so a `/` in a branch name travels as %2F.
	target := c.base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/compare/" + url.PathEscape(branch) + "..." + url.PathEscape(sha) + "?per_page=1"
	body, _, err := c.readRepositoryJSON(ctx, token, target, owner, repo)
	if err != nil {
		return false, err
	}
	var comparison struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &comparison); err != nil {
		return false, fmt.Errorf("decode the comparison of %s/%s %s...%s: %w", owner, repo, branch, sha, err)
	}
	return comparison.Status == "behind" || comparison.Status == "identical", nil
}

// DefaultBranch is the name of the repository's default branch (`GET /repos/{owner}/{repo}`,
// `default_branch` alone), the branch an after-merge or default-branch result's commit must be
// on. A 403 or 404 is ErrRepositoryUnreadable.
func (c *Client) DefaultBranch(ctx context.Context, token, owner, repo string) (string, error) {
	if c == nil {
		return "", ErrNoAppKey
	}
	target := c.base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	body, _, err := c.readRepositoryJSON(ctx, token, target, owner, repo)
	if err != nil {
		return "", err
	}
	var repository struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(body, &repository); err != nil {
		return "", fmt.Errorf("decode %s/%s: %w", owner, repo, err)
	}
	if repository.DefaultBranch == "" {
		return "", fmt.Errorf("%s/%s: GitHub named no default branch", owner, repo)
	}
	return repository.DefaultBranch, nil
}
