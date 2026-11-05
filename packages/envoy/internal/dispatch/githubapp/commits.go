package githubapp

// commits.go holds the reads a recorded check result's currency turns on inside this package:
// whether a commit sits on a branch, a repository's default branch, and the conditional reads of
// an open pull request's head a background pass fills Envoy's listener records in from (its check
// runs, its check suites, the repository's open pull requests and the repository's own identity).
// Each answers "Dispatch cannot see this repository" through CheckRepositoryResponse, the one
// rule that says what unreadable means — delivery's own read of the pull requests GitHub names
// for a commit (github_prs.go, through readGitHubPage's bounded retry) classifies its answer by
// it too.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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

// pollResponseLimit bounds the body of the conditional reads below, in place of the generic
// responseLimit. A commit's check runs read with filter=all carries each run's output.text, which
// GitHub caps at 65,535 characters: a hundred runs at that cap is about 6.4 MB, so responseLimit
// (1 MiB) would fail such a head's read at every pass, for ever. Eight MiB is that bound with
// room over it, derived from GitHub's own caps and a full page — not measured against any
// repository. The other three reads are far smaller (a page of 100 pull requests is well under
// it) and share it, since one bound is one thing to reason about.
const pollResponseLimit = 8 << 20

// objectsPerPage is the page size the polled reads ask GitHub for, its own maximum: a head with
// few checks is then one request, and a page holding exactly this many says another page may
// follow.
const objectsPerPage = 100

// readConditionalRepositoryJSON is readRepositoryJSON made conditional: it sends the ETag the
// caller remembered from this path's last answer and returns the answer's own ETag beside the
// body, with 304 Not Modified handled, since the caller reads that status itself. It reads
// within pollResponseLimit rather than responseLimit.
func (c *Client) readConditionalRepositoryJSON(ctx context.Context, token, path, repoFull, etag string) ([]byte, int, string, error) {
	body, status, header, err := c.ReadConditionalWithin(ctx, token, path, etag, pollResponseLimit)
	if err != nil {
		return nil, status, "", err
	}
	if err := CheckRepositoryResponse(status, header, body, repoFull, c.base+path, http.StatusNotModified); err != nil {
		return nil, status, "", err
	}
	// GitHub repeats the ETag on a 304; when it does not, the empty value travels to the caller,
	// which decides whether to keep the one it had.
	return body, status, header.Get("ETag"), nil
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

// repoPath is the API path of owner/repo, each part escaped as CommitOnBranch escapes its own.
func repoPath(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

// pageNumber is the page a caller asked for, counting from one: a caller that counted from zero,
// or below, asks GitHub for the first page rather than for a page GitHub does not name.
func pageNumber(page int) int {
	if page < 1 {
		return 1
	}
	return page
}

// ObjectPage is one page of a conditional read of GitHub check objects: the objects as
// contracts.GithubCIObservation takes them, the answer's ETag, whether GitHub answered 304 Not
// Modified (Objects is then empty), and whether the page was full, so a caller knows another
// page may follow.
type ObjectPage struct {
	Objects     []map[string]any
	ETag        string
	NotModified bool
	Full        bool
}

// CommitCheckRuns reads page page (1-based) of GET
// /repos/{owner}/{repo}/commits/{sha}/check-runs?filter=all&per_page=100, conditional on etag.
// filter=all, not GitHub's default latest, because a run Envoy's listener never recorded may be
// an earlier attempt of a check that has since run again.
func (c *Client) CommitCheckRuns(ctx context.Context, token, owner, repo, sha string, page int, etag string) (ObjectPage, error) {
	if c == nil {
		return ObjectPage{}, ErrNoAppKey
	}
	path := repoPath(owner, repo) + "/commits/" + url.PathEscape(sha) +
		"/check-runs?filter=all&per_page=" + strconv.Itoa(objectsPerPage) + "&page=" + strconv.Itoa(pageNumber(page))
	return c.readObjectPage(ctx, token, path, owner+"/"+repo, "check_runs", etag)
}

// CommitCheckSuites reads page page (1-based) of GET
// /repos/{owner}/{repo}/commits/{sha}/check-suites?per_page=100, conditional on etag.
func (c *Client) CommitCheckSuites(ctx context.Context, token, owner, repo, sha string, page int, etag string) (ObjectPage, error) {
	if c == nil {
		return ObjectPage{}, ErrNoAppKey
	}
	path := repoPath(owner, repo) + "/commits/" + url.PathEscape(sha) +
		"/check-suites?per_page=" + strconv.Itoa(objectsPerPage) + "&page=" + strconv.Itoa(pageNumber(page))
	return c.readObjectPage(ctx, token, path, owner+"/"+repo, "check_suites", etag)
}

// readObjectPage reads one page of a commit's check objects: path is the page's whole API path
// and field the key GitHub's answer carries the objects under, both answers being
// {"total_count":n,"<field>":[…]}.
func (c *Client) readObjectPage(ctx context.Context, token, path, repoFull, field, etag string) (ObjectPage, error) {
	body, status, answerETag, err := c.readConditionalRepositoryJSON(ctx, token, path, repoFull, etag)
	if err != nil {
		return ObjectPage{}, err
	}
	if status == http.StatusNotModified {
		return ObjectPage{ETag: answerETag, NotModified: true}, nil
	}
	// The objects alone are decoded into maps, as the webhook's own bodies reach
	// contracts.GithubCIObservation: total_count is GitHub's count of the whole list, which the
	// page size and Full already say what a caller needs of.
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(body, &answer); err != nil {
		return ObjectPage{}, fmt.Errorf("decode the %s of %s: %w", field, repoFull, err)
	}
	var objects []map[string]any
	if raw, ok := answer[field]; ok {
		if err := json.Unmarshal(raw, &objects); err != nil {
			return ObjectPage{}, fmt.Errorf("decode the %s of %s: %w", field, repoFull, err)
		}
	}
	return ObjectPage{Objects: objects, ETag: answerETag, Full: len(objects) == objectsPerPage}, nil
}

// PullRequestHead is one open pull request of a repository: its number and its head commit.
type PullRequestHead struct {
	Number  int64
	HeadSHA string
}

// PullRequestPage is one page of GET /repos/{owner}/{repo}/pulls?state=open&per_page=100, read
// conditionally as ObjectPage is.
type PullRequestPage struct {
	PullRequests []PullRequestHead
	ETag         string
	NotModified  bool
	Full         bool
}

// OpenPullRequests reads page page (1-based) of the repository's open pull requests, conditional
// on etag: the heads a pass reads the check runs and suites of. GitHub's answer is a bare array,
// each item carrying number and head.sha.
func (c *Client) OpenPullRequests(ctx context.Context, token, owner, repo string, page int, etag string) (PullRequestPage, error) {
	if c == nil {
		return PullRequestPage{}, ErrNoAppKey
	}
	path := repoPath(owner, repo) + "/pulls?state=open&per_page=" + strconv.Itoa(objectsPerPage) +
		"&page=" + strconv.Itoa(pageNumber(page))
	body, status, answerETag, err := c.readConditionalRepositoryJSON(ctx, token, path, owner+"/"+repo, etag)
	if err != nil {
		return PullRequestPage{}, err
	}
	if status == http.StatusNotModified {
		return PullRequestPage{ETag: answerETag, NotModified: true}, nil
	}
	var answer []struct {
		Number int64 `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return PullRequestPage{}, fmt.Errorf("decode the open pull requests of %s/%s: %w", owner, repo, err)
	}
	pulls := make([]PullRequestHead, len(answer))
	for i, pull := range answer {
		pulls[i] = PullRequestHead{Number: pull.Number, HeadSHA: pull.Head.SHA}
	}
	return PullRequestPage{PullRequests: pulls, ETag: answerETag, Full: len(answer) == objectsPerPage}, nil
}

// RepositoryAnswer is GET /repos/{owner}/{repo} read conditionally for the two fields a poller
// keys its records by: the name GitHub itself gives the repository, and its id.
type RepositoryAnswer struct {
	FullName    string
	ID          int64
	ETag        string
	NotModified bool
}

// RepositoryIdentity reads owner/repo itself, conditional on etag, for the name and id a poller
// keys its records by. A repository the App cannot see is ErrRepositoryUnreadable, as every read
// here is.
func (c *Client) RepositoryIdentity(ctx context.Context, token, owner, repo, etag string) (RepositoryAnswer, error) {
	if c == nil {
		return RepositoryAnswer{}, ErrNoAppKey
	}
	body, status, answerETag, err := c.readConditionalRepositoryJSON(ctx, token, repoPath(owner, repo), owner+"/"+repo, etag)
	if err != nil {
		return RepositoryAnswer{}, err
	}
	if status == http.StatusNotModified {
		return RepositoryAnswer{ETag: answerETag, NotModified: true}, nil
	}
	var repository struct {
		FullName string `json:"full_name"`
		ID       int64  `json:"id"`
	}
	if err := json.Unmarshal(body, &repository); err != nil {
		return RepositoryAnswer{}, fmt.Errorf("decode %s/%s: %w", owner, repo, err)
	}
	return RepositoryAnswer{FullName: repository.FullName, ID: repository.ID, ETag: answerETag}, nil
}
