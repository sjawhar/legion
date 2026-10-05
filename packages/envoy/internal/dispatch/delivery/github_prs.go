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

// searchPullRequestsResponse is the GraphQL search query's answer, limited to the fields
// searchMergedPullRequests reads. GitHub's REST `/search/issues` endpoint refuses some App
// installation tokens on a private repository ("cannot be searched... do not have permission")
// even though the same token reads that repository's pulls/commits/actions endpoints directly;
// GraphQL's `search` connection does not share that restriction (confirmed against a real
// installation token during this slice's acceptance verification), so this is GraphQL, not REST,
// despite otherwise mirroring REST search's query string syntax (`repo:`/`is:pr`/`is:merged`/
// `merged:`/`author:`, built by mergedPullRequestQuery below) and its 1,000-result cap.
type searchPullRequestsResponse struct {
	Data struct {
		Search struct {
			IssueCount int                     `json:"issueCount"`
			PageInfo   searchPullRequestsPage  `json:"pageInfo"`
			Nodes      []searchPullRequestNode `json:"nodes"`
		} `json:"search"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type searchPullRequestsPage struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// searchPullRequestNode is one `... on PullRequest` search result, limited to the fields
// searchMergedPullRequests reads. A search result that is not a pull request (the query's `is:pr`
// qualifier should prevent this, but GraphQL's inline fragment simply omits every field when the
// node is some other type) decodes as its zero value and is skipped by requiring a non-zero Number.
// Carries no `body`: the field is large enough on its own (a long PR description, times up to 100
// results per page) to push a page over the 1 MiB response limit on its own, and nothing in this
// path reads it -- reconcile's search-found PRs are stored Partial until a later per-PR
// FetchPullRequest completes them, which is also where their issue-reference body text is read.
type searchPullRequestNode struct {
	Number int         `json:"number"`
	Title  string      `json:"title"`
	URL    string      `json:"url"`
	Author *githubUser `json:"author"`
	Labels *struct {
		Nodes []githubLabel `json:"nodes"`
	} `json:"labels"`
	CreatedAt time.Time  `json:"createdAt"`
	MergedAt  *time.Time `json:"mergedAt"`
}

func fetchedPullRequestFromSearchNode(node searchPullRequestNode) FetchedPullRequest {
	pr := FetchedPullRequest{
		Number: node.Number, Title: node.Title, URL: node.URL,
		CreatedAt: node.CreatedAt, MergedAt: node.MergedAt,
		// Body, MergeCommitSHA, Additions, Deletions and FirstCommitAt stay nil/empty: this
		// search response never carries them.
	}
	if node.Author != nil {
		pr.Author = node.Author.Login
	}
	if node.Labels != nil {
		pr.Labels = labelNames(node.Labels.Nodes)
	}
	return pr
}

// SearchMergedPullRequests finds every pull request merged in [since, until) authored by any of
// authors, in owner/repo, via GitHub's GraphQL search. GitHub's search caps every query at 1,000
// results: fetchWindowed (windowed.go) halves the window and recurses when a query's total
// exceeds that cap, shared with ListWorkflowRuns's identical logic. The prototype's own measured
// rule is that a single day can hold on the order of 700-1,000 merges for the busiest repository
// it watched, so a caller backfilling several weeks should expect this function to recurse into
// day-sized or finer windows on its own, not pass one already that fine. Each result item carries
// enough to build a FetchedPullRequest-shaped result except MergeCommitSHA, Additions, Deletions
// and FirstCommitAt (search results carry none of them) -- those fields are left nil, and a
// caller that needs them calls FetchPullRequest per pull request afterward (reconcile's backfill
// path does this; a caller that only needs population-membership facts does not).
func SearchMergedPullRequests(ctx context.Context, client *githubapp.Client, owner, repo string, authors []string, since, until time.Time) ([]FetchedPullRequest, error) {
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("mint installation token for %s/%s merged-PR search: %w", owner, repo, err)
	}
	return searchMergedPullRequests(ctx, client, token, []string{owner + "/" + repo}, authors, since, until)
}

// SearchMergedPullRequestsAcrossInstallation finds every pull request merged in [since, until)
// authored by any of authors, across every repository the GitHub App installation covering
// tokenOwner/tokenRepo can see -- LEGION-294's population rule spans "any repository", not just
// the one configured deploy repository. An App-authenticated search query with no repo:/org:
// qualifier is NOT scoped to the installation's repositories for public-repository content --
// live-verified against a real installation token, it returns results from unrelated public
// repositories the installation does not cover -- so this lists the installation's own
// repositories (GET /installation/repositories) and builds one repo: qualifier per repository
// instead of relying on the token alone. tokenOwner/tokenRepo (typically the configured deploy
// repository) is used only to resolve which installation to ask.
func SearchMergedPullRequestsAcrossInstallation(ctx context.Context, client *githubapp.Client, tokenOwner, tokenRepo string, authors []string, since, until time.Time) ([]FetchedPullRequest, error) {
	token, err := client.RepositoryToken(ctx, tokenOwner, tokenRepo)
	if err != nil {
		return nil, fmt.Errorf("mint installation token for %s/%s merged-PR search: %w", tokenOwner, tokenRepo, err)
	}
	repos, err := client.ListInstallationRepositories(ctx, tokenOwner, tokenRepo)
	if err != nil {
		return nil, fmt.Errorf("list installation repositories for %s/%s merged-PR search: %w", tokenOwner, tokenRepo, err)
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("list installation repositories for %s/%s merged-PR search: installation covers no repositories", tokenOwner, tokenRepo)
	}
	return searchMergedPullRequests(ctx, client, token, repos, authors, since, until)
}

// searchMergedPullRequests pages a GraphQL search query over [since, until) through fetchWindowed
// (windowed.go), rebuilding the query (and resetting the GraphQL cursor) whenever fetchWindowed
// calls fetchPage with a different [since, until) than the previous call -- which happens exactly
// once per recursive half, never within one half's own pagination loop.
func searchMergedPullRequests(ctx context.Context, client *githubapp.Client, token string, repos, authors []string, since, until time.Time) ([]FetchedPullRequest, error) {
	scope := searchScopeLabel(repos)
	var cursor string
	var windowSince, windowUntil time.Time
	fetchPage := func(page int, since, until time.Time) ([]FetchedPullRequest, int, error) {
		if !since.Equal(windowSince) || !until.Equal(windowUntil) {
			cursor = ""
			windowSince, windowUntil = since, until
		}
		query := mergedPullRequestQuery(repos, authors, since, until)
		result, err := fetchSearchPage(ctx, client, token, query, cursor)
		if err != nil {
			return nil, 0, err
		}
		cursor = result.endCursor
		return result.items, result.issueCount, nil
	}
	return fetchWindowed(since, until, "search merged PRs for "+scope, fetchPage)
}

// searchScopeLabel is what an error message calls the search scope.
func searchScopeLabel(repos []string) string {
	if len(repos) == 1 {
		return repos[0]
	}
	return fmt.Sprintf("%d installation repositories", len(repos))
}

// mergedPullRequestQuery builds one repo: qualifier per entry in repos. The query string syntax
// is GitHub's search syntax (GitHub ORs repeated repo:/author: qualifiers of the same kind).
func mergedPullRequestQuery(repos, authors []string, since, until time.Time) string {
	var b strings.Builder
	for _, repo := range repos {
		fmt.Fprintf(&b, "repo:%s ", repo)
	}
	fmt.Fprintf(&b, "is:pr is:merged merged:%s..%s", since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339))
	for _, author := range authors {
		fmt.Fprintf(&b, " author:%s", author)
	}
	return b.String()
}

const searchPullRequestsQuery = `query($q: String!, $after: String) {
  search(query: $q, type: ISSUE, first: 100, after: $after) {
    issueCount
    pageInfo { hasNextPage endCursor }
    nodes {
      ... on PullRequest {
        number
        title
        url
        author { login }
        createdAt
        mergedAt
        labels(first: 20) { nodes { name } }
      }
    }
  }
}`

type searchPage struct {
	items       []FetchedPullRequest
	hasNextPage bool
	endCursor   string
	issueCount  int
}

// fetchSearchPage runs one page of query, after cursor ("" for the first page), returning the
// page's pull requests, its own pagination cursor, and the connection's total issueCount (the
// 1,000-result cap fetchWindowed checks).
func fetchSearchPage(ctx context.Context, client *githubapp.Client, token, query, after string) (searchPage, error) {
	var variables map[string]any
	if after == "" {
		variables = map[string]any{"q": query, "after": nil}
	} else {
		variables = map[string]any{"q": query, "after": after}
	}
	body, status, header, err := client.GraphQL(ctx, token, searchPullRequestsQuery, variables)
	if err != nil {
		return searchPage{}, fmt.Errorf("page after %q: %w", after, err)
	}
	if limited := githubapp.RateLimit(status, header, body); limited != nil {
		return searchPage{}, fmt.Errorf("page after %q: %w", after, limited)
	}
	if status != http.StatusOK {
		return searchPage{}, fmt.Errorf("page after %q: status %d: %s", after, status, body)
	}
	var response searchPullRequestsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return searchPage{}, fmt.Errorf("decode page after %q: %w", after, err)
	}
	if len(response.Errors) > 0 {
		return searchPage{}, fmt.Errorf("page after %q: %s", after, response.Errors[0].Message)
	}
	items := make([]FetchedPullRequest, 0, len(response.Data.Search.Nodes))
	for _, node := range response.Data.Search.Nodes {
		if node.Number == 0 {
			// A search result GitHub's own is:pr qualifier should have excluded (not a pull
			// request): the inline fragment decodes every field to its zero value instead of
			// erroring, so this is the one way to detect and skip it.
			continue
		}
		items = append(items, fetchedPullRequestFromSearchNode(node))
	}
	return searchPage{
		items:       items,
		hasNextPage: response.Data.Search.PageInfo.HasNextPage,
		endCursor:   response.Data.Search.PageInfo.EndCursor,
		issueCount:  response.Data.Search.IssueCount,
	}, nil
}
