package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// FetchedPullRequest is one pull request's GitHub facts: everything RawPullRequest needs for the
// population/task-label decision, plus everything model.DeliveryPullRequest needs once a PR is
// known to belong to the population.
//
// FetchPullRequest fills every field. SearchMergedPullRequests, which reads GitHub's search
// results rather than the pull request itself, cannot supply MergeCommitSHA, Additions,
// Deletions, or FirstCommitAt -- GitHub's search-issues response carries none of them -- so those
// stay nil on a search result; a caller that needs them calls FetchPullRequest afterward (this is
// reconcile's backfill path; a caller that only needs population-membership facts does not).
type FetchedPullRequest struct {
	Number         int
	Title          string
	URL            string
	Author         string
	Labels         []string
	CreatedAt      time.Time
	MergedAt       *time.Time // nil if the pull request has not actually merged
	MergeCommitSHA *string
	Additions      *int
	Deletions      *int
	Body           string // for later issue-reference resolution
	FirstCommitAt  *time.Time
}

type githubLabel struct {
	Name string `json:"name"`
}

type githubUser struct {
	Login string `json:"login"`
}

// pullRequestPayload is GET /repos/{owner}/{repo}/pulls/{number}'s answer, limited to the fields
// FetchPullRequest reads.
type pullRequestPayload struct {
	Number         int           `json:"number"`
	Title          string        `json:"title"`
	HTMLURL        string        `json:"html_url"`
	User           githubUser    `json:"user"`
	Labels         []githubLabel `json:"labels"`
	Body           string        `json:"body"`
	CreatedAt      time.Time     `json:"created_at"`
	MergedAt       *time.Time    `json:"merged_at"`
	MergeCommitSHA *string       `json:"merge_commit_sha"`
	Additions      int           `json:"additions"`
	Deletions      int           `json:"deletions"`
}

// commitPayload is one element of GET /repos/{owner}/{repo}/pulls/{number}/commits, limited to
// the fields FetchPullRequest's first-commit lookup reads.
type commitPayload struct {
	SHA    string `json:"sha"`
	Commit struct {
		Author *struct {
			Date time.Time `json:"date"`
		} `json:"author"`
		Committer *struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

// FetchPullRequest fetches one pull request's complete facts: GET
// /repos/{owner}/{repo}/pulls/{number} for everything but the first commit, then GET
// /repos/{owner}/{repo}/pulls/{number}/commits?per_page=1 for FirstCommitAt (the first page's
// first element's commit.author.date, or commit.committer.date when GitHub reports no author for
// that commit). Mints its own token via client.RepositoryToken(ctx, owner, repo). A merged pull
// request cannot have zero commits, but an empty or malformed commits answer is reported as an
// error rather than indexed into, and every GitHub call error is wrapped with which call failed
// and for which pull request.
func FetchPullRequest(ctx context.Context, client *githubapp.Client, owner, repo string, number int) (FetchedPullRequest, error) {
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return FetchedPullRequest{}, fmt.Errorf("mint installation token for %s/%s PR #%d: %w", owner, repo, number, err)
	}

	pullPath := fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repo), number)
	body, status, _, err := client.Read(ctx, token, pullPath)
	if err != nil {
		return FetchedPullRequest{}, fmt.Errorf("fetch %s/%s PR #%d: %w", owner, repo, number, err)
	}
	if status != http.StatusOK {
		return FetchedPullRequest{}, fmt.Errorf("fetch %s/%s PR #%d: status %d: %s", owner, repo, number, status, body)
	}
	var payload pullRequestPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return FetchedPullRequest{}, fmt.Errorf("decode %s/%s PR #%d: %w", owner, repo, number, err)
	}

	commitsPath := fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=1", url.PathEscape(owner), url.PathEscape(repo), number)
	commitsBody, commitsStatus, _, err := client.Read(ctx, token, commitsPath)
	if err != nil {
		return FetchedPullRequest{}, fmt.Errorf("fetch first commit of %s/%s PR #%d: %w", owner, repo, number, err)
	}
	if commitsStatus != http.StatusOK {
		return FetchedPullRequest{}, fmt.Errorf("fetch first commit of %s/%s PR #%d: status %d: %s", owner, repo, number, commitsStatus, commitsBody)
	}
	var commits []commitPayload
	if err := json.Unmarshal(commitsBody, &commits); err != nil {
		return FetchedPullRequest{}, fmt.Errorf("decode first commit of %s/%s PR #%d: %w", owner, repo, number, err)
	}
	if len(commits) == 0 {
		return FetchedPullRequest{}, fmt.Errorf("fetch first commit of %s/%s PR #%d: GitHub answered no commits for a pull request", owner, repo, number)
	}
	firstCommitAt, err := commitDate(commits[0])
	if err != nil {
		return FetchedPullRequest{}, fmt.Errorf("fetch first commit of %s/%s PR #%d: %w", owner, repo, number, err)
	}

	additions := payload.Additions
	deletions := payload.Deletions
	return FetchedPullRequest{
		Number:         payload.Number,
		Title:          payload.Title,
		URL:            payload.HTMLURL,
		Author:         payload.User.Login,
		Labels:         labelNames(payload.Labels),
		CreatedAt:      payload.CreatedAt,
		MergedAt:       payload.MergedAt,
		MergeCommitSHA: payload.MergeCommitSHA,
		Additions:      &additions,
		Deletions:      &deletions,
		Body:           payload.Body,
		FirstCommitAt:  &firstCommitAt,
	}, nil
}

// commitDate reads one commit's authored time: commit.author.date, or commit.committer.date when
// GitHub reports no author for the commit (an unlinked email still carries a committer date).
// Neither present is a malformed answer.
func commitDate(c commitPayload) (time.Time, error) {
	if c.Commit.Author != nil {
		return c.Commit.Author.Date, nil
	}
	if c.Commit.Committer != nil {
		return c.Commit.Committer.Date, nil
	}
	return time.Time{}, fmt.Errorf("commit %s carries neither an author nor a committer date", c.SHA)
}

func labelNames(labels []githubLabel) []string {
	names := make([]string, len(labels))
	for i, label := range labels {
		names[i] = label.Name
	}
	return names
}

// searchIssuesPayload is GET /search/issues's answer, limited to the fields
// SearchMergedPullRequests reads.
type searchIssuesPayload struct {
	TotalCount int               `json:"total_count"`
	Items      []searchIssueItem `json:"items"`
}

type searchIssueItem struct {
	Number      int           `json:"number"`
	Title       string        `json:"title"`
	HTMLURL     string        `json:"html_url"`
	User        githubUser    `json:"user"`
	Labels      []githubLabel `json:"labels"`
	Body        string        `json:"body"`
	CreatedAt   time.Time     `json:"created_at"`
	PullRequest struct {
		MergedAt *time.Time `json:"merged_at"`
	} `json:"pull_request"`
}

func fetchedPullRequestFromSearchItem(item searchIssueItem) FetchedPullRequest {
	return FetchedPullRequest{
		Number:    item.Number,
		Title:     item.Title,
		URL:       item.HTMLURL,
		Author:    item.User.Login,
		Labels:    labelNames(item.Labels),
		CreatedAt: item.CreatedAt,
		MergedAt:  item.PullRequest.MergedAt,
		Body:      item.Body,
		// MergeCommitSHA, Additions, Deletions and FirstCommitAt stay nil: GitHub's search-issues
		// response never carries them.
	}
}

// SearchMergedPullRequests finds every pull request merged in [since, until) authored by any of
// authors, in owner/repo, via GitHub's REST search (GET
// /search/issues?q=repo:owner/repo+is:pr+is:merged+merged:ISO..ISO+author:A+author:A2...).
// GitHub's search caps every query at 1,000 results: when a query's total_count exceeds 1,000,
// the window is split into two halves at its midpoint and each half is searched recursively,
// whose results are concatenated -- the halves are a half-open partition of the original window,
// so nothing merged exactly at the midpoint is counted twice and nothing is skipped. The
// prototype's own measured rule is that a single day can hold on the order of 700-1,000 merges
// for the busiest repository it watched, so a caller backfilling several weeks should expect this
// function to recurse into day-sized or finer windows on its own, not pass one already that fine;
// the halving handles whatever slice the caller hands in. Each query is paginated fully (GitHub
// serves up to 100 results per page; every page up to the 1,000-result cap is read). Each
// result item carries enough to build a FetchedPullRequest-shaped result except
// MergeCommitSHA, Additions, Deletions and FirstCommitAt (search results carry none of them) --
// those fields are left nil, and a caller that needs them calls FetchPullRequest per pull request
// afterward (reconcile's backfill path does this; a caller that only needs population-membership
// facts does not).
func SearchMergedPullRequests(ctx context.Context, client *githubapp.Client, owner, repo string, authors []string, since, until time.Time) ([]FetchedPullRequest, error) {
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("mint installation token for %s/%s merged-PR search: %w", owner, repo, err)
	}
	return searchMergedPullRequests(ctx, client, token, owner, repo, authors, since, until)
}

// SearchMergedPullRequestsAcrossInstallation finds every pull request merged in [since, until)
// authored by any of authors, across every repository the GitHub App installation covering
// tokenOwner/tokenRepo can see -- LEGION-294's population rule spans "any repository", not just
// the one configured deploy repository, and GitHub's search API scopes an App-authenticated
// query with no `repo:`/`org:` qualifier to exactly the repositories that installation's token
// can read. tokenOwner/tokenRepo (typically the configured deploy repository) is used only to
// resolve which installation's token to mint; the search itself is not scoped to that repository.
func SearchMergedPullRequestsAcrossInstallation(ctx context.Context, client *githubapp.Client, tokenOwner, tokenRepo string, authors []string, since, until time.Time) ([]FetchedPullRequest, error) {
	token, err := client.RepositoryToken(ctx, tokenOwner, tokenRepo)
	if err != nil {
		return nil, fmt.Errorf("mint installation token for %s/%s merged-PR search: %w", tokenOwner, tokenRepo, err)
	}
	return searchMergedPullRequests(ctx, client, token, "", "", authors, since, until)
}

// minimumSearchWindow bounds the halving recursion: a query answering more than 1,000 results in
// a window this narrow cannot be split further, and is reported as an error rather than recursing
// forever.
const minimumSearchWindow = time.Second

func searchMergedPullRequests(ctx context.Context, client *githubapp.Client, token, owner, repo string, authors []string, since, until time.Time) ([]FetchedPullRequest, error) {
	query := mergedPullRequestQuery(owner, repo, authors, since, until)
	scope := searchScopeLabel(owner, repo)

	first, totalCount, err := fetchSearchPage(ctx, client, token, query, 1)
	if err != nil {
		return nil, fmt.Errorf("search merged PRs for %s in [%s, %s): %w", scope, since, until, err)
	}

	if totalCount > 1000 {
		if until.Sub(since) <= minimumSearchWindow {
			return nil, fmt.Errorf("search merged PRs for %s: %d results in the window [%s, %s), which cannot be narrowed further", scope, totalCount, since, until)
		}
		mid := since.Add(until.Sub(since) / 2)
		before, err := searchMergedPullRequests(ctx, client, token, owner, repo, authors, since, mid)
		if err != nil {
			return nil, err
		}
		after, err := searchMergedPullRequests(ctx, client, token, owner, repo, authors, mid, until)
		if err != nil {
			return nil, err
		}
		return append(before, after...), nil
	}

	results := first
	for page := 2; len(results) < totalCount; page++ {
		items, _, err := fetchSearchPage(ctx, client, token, query, page)
		if err != nil {
			return nil, fmt.Errorf("search merged PRs for %s in [%s, %s): %w", scope, since, until, err)
		}
		if len(items) == 0 {
			break
		}
		results = append(results, items...)
	}
	return results, nil
}

// searchScopeLabel is what an error message calls the search scope: the repository, or "the
// installation" when repo is empty (SearchMergedPullRequestsAcrossInstallation's no-repo-qualifier
// search).
func searchScopeLabel(owner, repo string) string {
	if repo == "" {
		return "the installation"
	}
	return owner + "/" + repo
}

// mergedPullRequestQuery omits the repo: qualifier when repo is empty, scoping the search to
// every repository the authenticating installation token can see instead of one repository.
func mergedPullRequestQuery(owner, repo string, authors []string, since, until time.Time) string {
	var b strings.Builder
	if repo != "" {
		fmt.Fprintf(&b, "repo:%s/%s ", owner, repo)
	}
	fmt.Fprintf(&b, "is:pr is:merged merged:%s..%s", since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339))
	for _, author := range authors {
		fmt.Fprintf(&b, " author:%s", author)
	}
	return b.String()
}

func fetchSearchPage(ctx context.Context, client *githubapp.Client, token, query string, page int) ([]FetchedPullRequest, int, error) {
	path := fmt.Sprintf("/search/issues?q=%s&per_page=100&page=%d", url.QueryEscape(query), page)
	body, status, _, err := client.Read(ctx, token, path)
	if err != nil {
		return nil, 0, fmt.Errorf("page %d: %w", page, err)
	}
	if status != http.StatusOK {
		return nil, 0, fmt.Errorf("page %d: status %d: %s", page, status, body)
	}
	var payload searchIssuesPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, 0, fmt.Errorf("decode page %d: %w", page, err)
	}
	items := make([]FetchedPullRequest, len(payload.Items))
	for i, item := range payload.Items {
		items[i] = fetchedPullRequestFromSearchItem(item)
	}
	return items, payload.TotalCount, nil
}
