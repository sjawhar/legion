package delivery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A pull request's issue is the first of five sources that names an issue Dispatch has, each tried
// only when every earlier one names none (the prototype's collector/attribution.py):
//
//  1. a key in its title or body, title first;
//  2. the issue whose external link is the pull request's own URL;
//  3. the issue whose external link is a GitHub issue the body cites, a full
//     github.com/<owner>/<repo>/issues/<n> URL or a bare #N in the pull request's own repository,
//     the first such citation in body order;
//  4. a key in its head branch name, whatever its case;
//  5. a key in its commit messages, in commit order.
//
// Sources 1, 3, 4 and 5 read what GitHub says about the pull request, which does not change once it
// has merged, so those inputs (AttributionInputs) are read from GitHub once and stored on its row.
// Sources 2 and 3 also read Dispatch's external links, which change whenever someone links an issue,
// and every source reads whether the issue it names exists, so a stored row's issue is resolved
// again from the database alone (AttributePullRequests), a batch of rows a pass, the rows checked
// longest ago first: an issue that links a pull request tomorrow credits it within one walk of the
// table, with no GitHub call. Nothing in the schema says which rows a change could affect (a link
// carries no time, and adding or removing one leaves its issue's updated_at alone), so the walk
// visits them all in turn.

// attributionSource names the source a pull request's issue came from.
type attributionSource string

const (
	sourceTitleBody       attributionSource = "title_body"
	sourceExternalLink    attributionSource = "external_link"
	sourceGitHubIssueLink attributionSource = "github_issue_link"
	sourceBranch          attributionSource = "branch"
	sourceCommitMessage   attributionSource = "commit_message"
	sourceNone            attributionSource = "none"
)

// AttributionInputs is what the attribution reads from GitHub, as stored on the pull request's
// row: the issue keys its title and body name, the GitHub issue URLs its body cites, the keys its
// head branch names and the keys its commit messages name, each deduplicated in first-seen order.
// Nil on a DeliveryPullRequest means they have not been read.
type AttributionInputs struct {
	TitleKeys   []string
	CitedIssues []string
	BranchKeys  []string
	CommitKeys  []string
}

// attributionFacts is what GitHub says about one pull request that the attribution reads.
type attributionFacts struct {
	Repo           string
	URL            string
	Title          string
	Body           string
	HeadRef        string
	CommitMessages []string
}

// githubIssueURL matches a full GitHub issue URL; githubBareReference a bare #N of two to six
// digits (the prototype's reference shape), which GitHub reads as an issue or pull request of the
// same repository. A # after a word character, `&` (an HTML character reference), `/` (a URL
// fragment) or another # (a heading) is not a reference.
var (
	githubIssueURL      = regexp.MustCompile(`github\.com/([\w.-]+)/([\w.-]+)/issues/([1-9][0-9]*)\b`)
	githubBareReference = regexp.MustCompile(`(?:^|[^\w&/#])#([1-9][0-9]{1,5})\b`)
)

func attributionInputsFrom(facts attributionFacts) AttributionInputs {
	return AttributionInputs{
		TitleKeys:   uniqueKeys(facts.Title + "\n" + facts.Body),
		CitedIssues: citedIssues(facts.Repo, facts.Body),
		BranchKeys:  uniqueKeys(strings.ToUpper(facts.HeadRef)),
		CommitKeys:  uniqueKeys(strings.Join(facts.CommitMessages, "\n")),
	}
}

func uniqueKeys(text string) []string {
	return appendUnique([]string{}, issueKeyCandidate.FindAllString(text, -1)...)
}

func appendUnique(list []string, values ...string) []string {
	for _, value := range values {
		if !slices.Contains(list, value) {
			list = append(list, value)
		}
	}
	return list
}

// citedIssues is every GitHub issue body cites, as https://github.com/<owner>/<repo>/issues/<n>,
// in the order the citations appear; a bare #N names an issue of repo.
func citedIssues(repo, body string) []string {
	type citation struct {
		at  int
		url string
	}
	citations := []citation{}
	for _, match := range githubIssueURL.FindAllStringSubmatchIndex(body, -1) {
		citations = append(citations, citation{match[0], "https://github.com/" + body[match[2]:match[3]] + "/" + body[match[4]:match[5]] + "/issues/" + body[match[6]:match[7]]})
	}
	for _, match := range githubBareReference.FindAllStringSubmatchIndex(body, -1) {
		citations = append(citations, citation{match[2], "https://github.com/" + repo + "/issues/" + body[match[2]:match[3]]})
	}
	slices.SortStableFunc(citations, func(a, b citation) int { return cmp.Compare(a.at, b.at) })
	urls := []string{}
	for _, c := range citations {
		urls = appendUnique(urls, c.url)
	}
	return urls
}

// attributionSelect is the one statement that resolves an issue from stored inputs: the issue
// and source of the first source naming an issue Dispatch has, or no row. Its arguments are the
// SQL expressions for the pull request's URL and its four stored inputs, so a single pull request
// (resolveStoredIssueKey, bound parameters) and every stored row (AttributePullRequests, the row's
// own columns) resolve through the same text.
func attributionSelect(url, titleKeys, citedIssues, branchKeys, commitKeys string) string {
	return `select s.key, s.source from (
		select t.key, '` + string(sourceTitleBody) + `' as source, 0 as rank, t.n
			from unnest(` + titleKeys + `) with ordinality t(key, n) join issues i on i.key = t.key
		union all
		select l.issue_key, '` + string(sourceExternalLink) + `', 1, 0
			from issue_external_links l where l.url = ` + url + `
		union all
		select l.issue_key, '` + string(sourceGitHubIssueLink) + `', 2, c.n
			from unnest(` + citedIssues + `) with ordinality c(url, n) join issue_external_links l on l.url = c.url
		union all
		select b.key, '` + string(sourceBranch) + `', 3, b.n
			from unnest(` + branchKeys + `) with ordinality b(key, n) join issues i on i.key = b.key
		union all
		select m.key, '` + string(sourceCommitMessage) + `', 4, m.n
			from unnest(` + commitKeys + `) with ordinality m(key, n) join issues i on i.key = m.key
	) s order by s.rank, s.n limit 1`
}

// resolveIssueKey is the issue facts names now, and the source it came from.
func resolveIssueKey(ctx context.Context, pool *store.Pool, facts attributionFacts) (*string, attributionSource, error) {
	return resolveStoredIssueKey(ctx, pool, facts.URL, attributionInputsFrom(facts))
}

func resolveStoredIssueKey(ctx context.Context, pool *store.Pool, url string, inputs AttributionInputs) (*string, attributionSource, error) {
	var key string
	var source attributionSource
	err := pool.QueryRow(ctx,
		attributionSelect("$1", "$2::text[]", "$3::text[]", "$4::text[]", "$5::text[]"),
		url, inputs.TitleKeys, inputs.CitedIssues, inputs.BranchKeys, inputs.CommitKeys,
	).Scan(&key, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, sourceNone, nil
	}
	if err != nil {
		return nil, sourceNone, fmt.Errorf("resolve the issue of %s: %w", url, err)
	}
	return &key, source, nil
}

// AttributionResolveBatch is how many stored rows one reconcile pass resolves again
// (AttributePullRequests): a 28-day population is about 4,000 rows, so a pass costs a bounded
// few milliseconds however large the table grows, and the walk comes round to every row in a
// handful of passes at today's size.
const AttributionResolveBatch = 1000

// AttributePullRequests resolves again, from the database alone, the issue of up to limit stored
// pull requests whose inputs have been read, those checked longest ago first, and writes each
// one that changed: an issue that links a pull request, or cites one of its GitHub issues,
// credits it here; one that no longer does uncredits it. Every row it reads is marked checked
// now, so the next pass takes the rows after it. Answers how many rows it changed.
func AttributePullRequests(ctx context.Context, pool *store.Pool, limit int) (int64, error) {
	var changed int64
	err := pool.QueryRow(ctx, `
		with batch as (
			select repo, number, url, issue_key, attribution_title_keys, attribution_cited_issues,
				attribution_branch_keys, attribution_commit_keys
			from delivery_pull_requests
			where attribution_title_keys is not null
			order by attribution_checked_at nulls first, repo, number
			limit $1
		), resolved as (
			select src.repo, src.number, r.key, src.issue_key is distinct from r.key as changed
			from batch src
			left join lateral (`+attributionSelect(
		"src.url", "src.attribution_title_keys", "src.attribution_cited_issues",
		"src.attribution_branch_keys", "src.attribution_commit_keys")+`) r on true
		), written as (
			update delivery_pull_requests pr
			set issue_key = resolved.key,
				updated_at = case when resolved.changed then now() else pr.updated_at end,
				attribution_checked_at = now()
			from resolved
			where pr.repo = resolved.repo and pr.number = resolved.number
			returning resolved.changed
		)
		select count(*) filter (where changed) from written
	`, limit).Scan(&changed)
	if err != nil {
		return 0, fmt.Errorf("attribute stored pull requests: %w", err)
	}
	return changed, nil
}
