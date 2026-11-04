package githubapp

// commits.go holds the two reads a recorded check result's currency turns on inside this
// package: whether a commit sits on a branch, and a repository's default branch. Each answers
// "Dispatch cannot see this repository" through CheckRepositoryResponse, the one rule that says
// what unreadable means — delivery's own read of the pull requests GitHub names for a commit
// (github_prs.go, through readGitHubPage's bounded retry) classifies its answer by it too.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
)

// readRepositoryJSON performs one GET of path (an API path under the API origin) with an
// installation token, through Client.Read, and answers the body of a read of repoFull whose
// answer is JSON, classified by CheckRepositoryResponse: a status the caller names in handled
// comes back as its own status for the caller to read, any other failure as the error that rule
// names.
func (c *Client) readRepositoryJSON(ctx context.Context, token, path, repoFull string, handled ...int) ([]byte, int, error) {
	body, status, header, err := c.Read(ctx, token, path)
	if err != nil {
		return nil, status, err
	}
	if err := CheckRepositoryResponse(status, header, body, repoFull, c.base+path, handled...); err != nil {
		return nil, status, err
	}
	return body, status, nil
}

// CheckRepositoryResponse classifies one repository read's answer. A 200, or a status the caller
// names in handled and reads itself, is no error. A 403 or 404 is ErrRepositoryUnreadable — the
// installation may not read this resource, or cannot see the repository at all — which the
// currency read takes as "unknown", never as "older"; a rate limit behind either status is the
// *RateLimitError instead. Any other status is the error CheckResponse makes, named by the
// target. repoFull is the `owner/repo` the error names.
func CheckRepositoryResponse(status int, header http.Header, body []byte, repoFull, target string, handled ...int) error {
	if status == http.StatusOK || slices.Contains(handled, status) {
		return nil
	}
	if status == http.StatusForbidden || status == http.StatusNotFound {
		if limited := RateLimit(status, header, body); limited != nil {
			return limited
		}
		return fmt.Errorf("%w: %s", ErrRepositoryUnreadable, repoFull)
	}
	if err := CheckResponse(status, header, body); err != nil {
		return fmt.Errorf("GET %s: %w", target, err)
	}
	return nil
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
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/compare/" + url.PathEscape(branch) + "..." + url.PathEscape(sha) + "?per_page=1"
	body, _, err := c.readRepositoryJSON(ctx, token, path, owner+"/"+repo)
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
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	body, _, err := c.readRepositoryJSON(ctx, token, path, owner+"/"+repo)
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
