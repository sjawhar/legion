package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// ErrPullRequestNotFound is a 404 or 410 from GitHub fetching a specific pull request: the pull
// request or its repository no longer exists, or no longer reaches this token. A permanent
// condition, unlike every other FetchPullRequest failure (a transient 5xx, a rate limit) --
// reconcile.completePartialPullRequest marks the row unfetchable rather than retrying it forever.
var ErrPullRequestNotFound = errors.New("pull request not found or gone")

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
	Body           string // for issue attribution
	HeadRef        string // the head branch's name, for issue attribution; empty on a search result
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
	Head           struct {
		Ref string `json:"ref"`
	} `json:"head"`
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
	payload, err := fetchPullRequestPayload(ctx, client, token, owner, repo, number)
	if err != nil {
		return FetchedPullRequest{}, err
	}
	commitsPath := fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=1", url.PathEscape(owner), url.PathEscape(repo), number)
	commitsBody, commitsStatus, commitsHeader, err := readGitHubPage(ctx, client, token, commitsPath)
	if err != nil {
		return FetchedPullRequest{}, fmt.Errorf("fetch first commit of %s/%s PR #%d: %w", owner, repo, number, err)
	}
	if err := githubapp.CheckResponse(commitsStatus, commitsHeader, commitsBody); err != nil {
		return FetchedPullRequest{}, fmt.Errorf("fetch first commit of %s/%s PR #%d: %w", owner, repo, number, err)
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
		HeadRef:        payload.Head.Ref,
		FirstCommitAt:  &firstCommitAt,
	}, nil
}

// fetchPullRequestPayload is GET /repos/{owner}/{repo}/pulls/{number} under token, through
// readGitHubPage's bounded retry: a 404 or 410 is ErrPullRequestNotFound, any other failure is
// wrapped naming the pull request.
func fetchPullRequestPayload(ctx context.Context, client *githubapp.Client, token, owner, repo string, number int) (pullRequestPayload, error) {
	pullPath := fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repo), number)
	body, status, header, err := readGitHubPage(ctx, client, token, pullPath)
	if err != nil {
		return pullRequestPayload{}, fmt.Errorf("fetch %s/%s PR #%d: %w", owner, repo, number, err)
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return pullRequestPayload{}, fmt.Errorf("fetch %s/%s PR #%d: %w (status %d)", owner, repo, number, ErrPullRequestNotFound, status)
	}
	if err := githubapp.CheckResponse(status, header, body); err != nil {
		return pullRequestPayload{}, fmt.Errorf("fetch %s/%s PR #%d: %w", owner, repo, number, err)
	}
	var payload pullRequestPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return pullRequestPayload{}, fmt.Errorf("decode %s/%s PR #%d: %w", owner, repo, number, err)
	}
	return payload, nil
}

// fetchAttributionFacts reads what the issue attribution needs of a pull request already stored
// complete, and nothing else: the pull request (its title, body and head branch) and then its
// commits' messages, one page of 100 at a time. A complete row already holds every other fact
// FetchPullRequest reads, its first commit's time included, so the backfill makes one call plus
// one per commits page rather than FetchPullRequest's two plus fetchCommitMessages' pages. It
// answers how many calls it made, a failed one included (retries inside readGitHubPage aside),
// for the backfill's call budget.
func fetchAttributionFacts(ctx context.Context, client *githubapp.Client, owner, repo, repoFull string, number int) (attributionFacts, int, error) {
	tokenSource := func() (string, error) { return client.RepositoryToken(ctx, owner, repo) }
	token, err := tokenSource()
	if err != nil {
		return attributionFacts{}, 0, fmt.Errorf("mint installation token for %s PR #%d: %w", repoFull, number, err)
	}
	payload, err := fetchPullRequestPayload(ctx, client, token, owner, repo, number)
	if err != nil {
		return attributionFacts{}, 1, err
	}
	messages, pages, err := fetchCommitMessagesWithToken(ctx, client, tokenSource, owner, repo, number)
	if err != nil {
		return attributionFacts{}, 1 + pages, err
	}
	return attributionFacts{
		Repo: repoFull, URL: payload.HTMLURL, Title: payload.Title, Body: payload.Body,
		HeadRef: payload.Head.Ref, CommitMessages: messages,
	}, 1 + pages, nil
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
//
// Author.Typename is GraphQL's own discriminator ("User", "Bot", "Organization", ...) for the
// `Actor` interface `author` resolves to. GitHub's GraphQL login for a bot-authored pull request
// is the bare account name ("sjawhar-agent"); its REST `user.login` for the identical account
// carries the "[bot]" suffix ("sjawhar-agent[bot]") REST adds for every App-created identity.
// IsPopulationPR (population.go) and delivery_settings.population_authors are both written and
// compared in REST's form -- Dispatch's own intake and every other fetcher in this package read
// REST -- so a GraphQL result normalizes to that same form here, at decode time, rather than
// teaching every consumer to recognize two spellings of one account.
type searchPullRequestNode struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Author *struct {
		Login    string `json:"login"`
		Typename string `json:"__typename"`
	} `json:"author"`
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
		if node.Author.Typename == "Bot" {
			pr.Author += "[bot]"
		}
	}
	if node.Labels != nil {
		pr.Labels = labelNames(node.Labels.Nodes)
	}
	return pr
}

// SearchMergedPullRequests finds every pull request merged in [since, until) authored by any of
// authors, in owner/repo, via GitHub's GraphQL search. GitHub's search caps every query at 1,000
// results: walkWindowed (windowed.go) halves the window and recurses when a query's total
// exceeds that cap, shared with ListWorkflowRuns's identical logic. The prototype's own measured
// rule is that a single day can hold on the order of 700-1,000 merges for the busiest repository
// it watched, so a caller backfilling several weeks should expect this function to recurse into
// day-sized or finer windows on its own, not pass one already that fine. Each result item carries
// enough to build a FetchedPullRequest-shaped result except MergeCommitSHA, Additions, Deletions
// and FirstCommitAt (search results carry none of them) -- those fields are left nil, and a
// caller that needs them calls FetchPullRequest per pull request afterward (reconcile's backfill
// path does this; a caller that only needs population-membership facts does not).
func SearchMergedPullRequests(ctx context.Context, client *githubapp.Client, owner, repo string, authors []string, since, until time.Time) ([]FetchedPullRequest, error) {
	token := func() (string, error) { return client.RepositoryToken(ctx, owner, repo) }
	var found []FetchedPullRequest
	err := searchMergedPullRequests(ctx, client, token, []string{owner + "/" + repo}, authors, since, until,
		func(_ time.Time, prs []FetchedPullRequest) error {
			found = append(found, prs...)
			return nil
		})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// installationSearchConcurrency bounds how many installations SearchMergedPullRequestsAcrossInstallation
// searches at once -- the same reasoning as reconcile.go's reconcilePartialConcurrency: enough to
// meaningfully parallelize an App installed on several accounts without treating one reconcile
// pass as free to hammer GitHub as hard as it can.
const installationSearchConcurrency = 8

// SearchMergedPullRequestsAcrossInstallation finds every pull request merged in [since, until)
// authored by any of authors, across every repository every installation of the App can see.
// LEGION-294's population rule (an author allowlist: sjawhar, sjawhar-agent, legion-implementer)
// names no installation or org boundary -- a merge under a second installation is still
// population, and Rev measured roughly a quarter of the real population living there -- so this
// does not stop at one configured deploy repository's installation; it lists every installation
// (ListInstallations) and searches each one's own repositories
// (ListInstallationRepositoriesByID), since an App-authenticated search query with no repo:/org:
// qualifier is NOT scoped to any installation's repositories for public-repository content --
// live-verified against a real installation token, it returns results from unrelated public
// repositories no installation of this App covers.
//
// since is asked for each installation's own window start, and an installation whose window
// start cannot be read is skipped with that error rather than searched from the backfill floor:
// a failed read is not evidence that nothing was imported, and searching 28 days on the strength
// of it spends thousands of requests re-importing what the step already has. visit is called
// once per completed search window with the installation it came from, so a caller records each
// installation's progress separately and one installation's failure never makes another redo
// what it already imported. A duplicate is nothing to guard against: GitHub does not let one
// repository belong to two installations of the same App, and every write behind visit is an
// upsert.
//
// listed, when not nil, is called once with every installation the App has, after the listing
// succeeds and before any installation is searched, so a caller can act on which installations
// exist -- reconcile prunes the progress of the ones that no longer do. It is not called when the
// listing fails or names no installation, so a caller never takes an empty or failed listing as
// "none exist".
//
// Installations are searched concurrently through boundedFanOut (reconcile.go) via
// searchOneInstallation -- see boundedFanOut's own doc comment for the shared
// fan-out/cancellation mechanics, and note that visit is therefore called from several
// goroutines at once. One installation's own failure never stops another: its error is collected
// by name (which installation, by id and account) rather than aborting the whole search, and
// everything every other installation visited still stands -- the pass is reported unhealthy,
// but nothing already imported is thrown away. A *githubapp.RateLimitError is the one
// exception: it stops every further installation from starting (the ones already in flight
// still finish), since a rate limit is a global condition on this installation token budget,
// not one installation's own problem.
func SearchMergedPullRequestsAcrossInstallation(
	ctx context.Context,
	client *githubapp.Client,
	authors []string,
	since func(installationID int64) (time.Time, error),
	until time.Time,
	listed func(installations []githubapp.Installation),
	visit func(installation githubapp.Installation, windowUntil time.Time, prs []FetchedPullRequest) error,
) error {
	installations, err := client.ListInstallations(ctx)
	if err != nil {
		return fmt.Errorf("list installations for merged-PR search: %w", err)
	}
	if len(installations) == 0 {
		return errors.New("merged-PR search: the App has no installations")
	}
	if listed != nil {
		listed(installations)
	}

	var mu sync.Mutex
	var failures []string
	fanErr := boundedFanOut(ctx, installationSearchConcurrency, installations, func(ctx context.Context, installation githubapp.Installation) error {
		windowStart, err := since(installation.ID)
		if err != nil {
			mu.Lock()
			failures = append(failures, fmt.Sprintf("installation %d (%s): %s", installation.ID, installation.AccountLogin, err))
			mu.Unlock()
			return nil
		}
		err = searchOneInstallation(ctx, client, installation, authors, windowStart, until,
			func(windowUntil time.Time, prs []FetchedPullRequest) error {
				return visit(installation, windowUntil, prs)
			})
		if err != nil {
			if limited, ok := githubapp.AsRateLimit(err); ok {
				return limited
			}
			mu.Lock()
			failures = append(failures, fmt.Sprintf("installation %d (%s): %s", installation.ID, installation.AccountLogin, err))
			mu.Unlock()
		}
		return nil
	})
	if fanErr != nil {
		return fmt.Errorf("rate-limited searching installations: %w", fanErr)
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d installations failed: %s", len(failures), len(installations), strings.Join(failures, "; "))
	}
	return nil
}

// searchOneInstallation is SearchMergedPullRequestsAcrossInstallation's per-installation body.
func searchOneInstallation(ctx context.Context, client *githubapp.Client, installation githubapp.Installation, authors []string, since, until time.Time, visit func(time.Time, []FetchedPullRequest) error) error {
	repos, err := client.ListInstallationRepositoriesByID(ctx, installation.ID)
	if err != nil {
		return fmt.Errorf("list repositories: %w", err)
	}
	if len(repos) == 0 {
		return nil
	}
	token := func() (string, error) { return client.Token(ctx, installation.ID) }
	return searchMergedPullRequests(ctx, client, token, repos, authors, since, until, visit)
}

// searchMergedPullRequestsWindow bounds how many results one window of the merged-PR search
// hands to its visit callback, and so how much a failed pass redoes: GitHub's own 1,000-result
// cap, which at 100 results a page is ten pages. Unlike a run listing, nothing here costs a
// GitHub call per result -- each one is classified and upserted against Postgres alone -- so a
// window is its pages and no more.
const searchMergedPullRequestsWindow = githubResultCap

// searchMergedPullRequests walks a GraphQL search query over [since, until] through walkWindowed
// (windowed.go), handing each completed window to visit. newFetcher builds a fresh query string
// and a fresh (nil) cursor for every window walkWindowed asks for -- the original call and each
// recursive half -- so GitHub's own cursor, not a REST page number, is this fetcher's only
// pagination state. Each page asks token for its own installation token, by readGitHubPage's
// rule; walkWindowed's error names the scope and the window, and each page's names its cursor.
func searchMergedPullRequests(ctx context.Context, client *githubapp.Client, token func() (string, error), repos, authors []string, since, until time.Time, visit func(time.Time, []FetchedPullRequest) error) error {
	scope := searchScopeLabel(repos)
	newFetcher := func(since, until time.Time) func() ([]FetchedPullRequest, int, error) {
		query := mergedPullRequestQuery(repos, authors, since, until)
		var cursor string
		return func() ([]FetchedPullRequest, int, error) {
			current, err := token()
			if err != nil {
				return nil, 0, fmt.Errorf("page after %q: mint installation token: %w", cursor, err)
			}
			result, err := fetchSearchPage(ctx, client, current, query, cursor)
			if err != nil {
				return nil, 0, err
			}
			cursor = result.endCursor
			return result.items, result.issueCount, nil
		}
	}
	return walkWindowed(since, until, "search merged PRs for "+scope, searchMergedPullRequestsWindow, newFetcher, visit)
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
        author { login __typename }
        createdAt
        mergedAt
        labels(first: 20) { nodes { name } }
      }
    }
  }
}`

type searchPage struct {
	items      []FetchedPullRequest
	endCursor  string
	issueCount int
}

// fetchSearchPage runs one page of query, after cursor ("" for the first page), returning the
// page's pull requests, its own pagination cursor, and the connection's total issueCount (the
// 1,000-result cap walkWindowed checks).
func fetchSearchPage(ctx context.Context, client *githubapp.Client, token, query, after string) (searchPage, error) {
	var variables map[string]any
	if after == "" {
		variables = map[string]any{"q": query, "after": nil}
	} else {
		variables = map[string]any{"q": query, "after": after}
	}
	body, status, header, err := queryGitHubPage(ctx, client, token, searchPullRequestsQuery, variables)
	if err != nil {
		return searchPage{}, fmt.Errorf("page after %q (%s): %w", after, pageAttemptsNote(), err)
	}
	if err := githubapp.CheckResponse(status, header, body); err != nil {
		return searchPage{}, fmt.Errorf("page after %q (%s): %w", after, pageAttemptsNote(), err)
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
		items:      items,
		endCursor:  response.Data.Search.PageInfo.EndCursor,
		issueCount: response.Data.Search.IssueCount,
	}, nil
}
