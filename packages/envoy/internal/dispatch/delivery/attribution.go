package delivery

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A pull request's issue is the first issue its five ranked sources name that resolves to the
// repository the pull request is in: a candidate ranked above it that resolves to another
// repository, or to no repository at all, is passed over, and a pull request no candidate of
// which resolves here is attributed to nobody, with the source "none". The sources rank as the
// prototype's collector/attribution.py tried them, every candidate of one before any of the
// next (attributionCandidates):
//
//  1. a key in its title or body, title first;
//  2. the issue whose external link is the pull request's own URL;
//  3. the issue whose external link is a GitHub issue the body cites, a full
//     github.com/<owner>/<repo>/issues/<n> URL or a bare #N in the pull request's own repository,
//     the first such citation in body order;
//  4. a key in its head branch name, whatever its case;
//  5. a key in its commit messages, in commit order.
//
// The holding to the repository is the server's one repository-resolution rule, spliced into the
// same statement as the candidates (refs.IssueRepoTerms, attributionHeld), so what an
// attribution believes an issue's repository is can never disagree with the issue page.
//
// Sources 1, 3, 4 and 5 read what GitHub says about the pull request, which does not change once it
// has merged, so those inputs (AttributionInputs) are read from GitHub once and stored on its row.
// Sources 2 and 3 also read Dispatch's external links, which change whenever someone links an issue,
// every source reads whether the issue it names exists, and the holding reads where that issue
// sits — a reparent, or a project's repository mapping, moves it — so a stored row's issue is
// resolved again from the database alone (AttributePullRequests), a batch of rows a pass, the rows
// checked longest ago first: an issue that links a pull request tomorrow credits it within one walk
// of the table, with no GitHub call. Nothing in the schema says which rows a change could affect (a
// link carries no time, and adding or removing one leaves its issue's updated_at alone), so the
// walk visits them all in turn.

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

// attributionColumns are the SQL expressions attributionCandidates reads a pull request's own
// repository, its URL and its four stored inputs from: bound parameters for one pull request
// (resolveAttributionCandidates), or a stored row's own columns for every row
// (AttributePullRequests). Named, so the two callers cannot hand the repository and the four
// text[] inputs over in another order.
type attributionColumns struct {
	Repo, URL, TitleKeys, CitedIssues, BranchKeys, CommitKeys string
}

// attributionCandidateBound is how many ranked candidates one attribution carries out of the
// union. Every candidate costs a repository resolution now — a walk up its parent chain and its
// project's default mapping (refs.IssueRepoTerms) — and a title and a body together can name
// many keys: a Legion pull request names one or two, while a body listing a tree's issues can
// name a dozen, so 25 is room for the shapes that occur and a bound on the work a pathological
// body can ask for. The bound drops only candidates ranked below it, which under the rule
// before this change — the first ranked candidate outright — could never have won either.
const attributionCandidateBound = 25

// attributionCandidates is the issues a pull request's five sources name, best first: the
// ranking is the prototype's, every candidate of a source before any of the next, and within a
// source the order the keys were read in. The two link sources compare URLs lowercased on both
// sides: GitHub's owner and repository names are case-insensitive, its answers spell them as
// their owners do (`Acme/Widgets`), and Dispatch stores an issue's GitHub link lowercased
// (canonicalRepo) or as a person typed it (issue_external_links_url_lower serves the
// comparison).
func attributionCandidates(c attributionColumns) string {
	return `select s.key, s.source, s.rank, s.n from (
		select t.key, '` + string(sourceTitleBody) + `' as source, 0 as rank, t.n
			from unnest(` + c.TitleKeys + `) with ordinality t(key, n) join issues i on i.key = t.key
		union all
		select l.issue_key, '` + string(sourceExternalLink) + `', 1, 0
			from issue_external_links l where lower(l.url) = lower(` + c.URL + `)
		union all
		select l.issue_key, '` + string(sourceGitHubIssueLink) + `', 2, c.n
			from unnest(` + c.CitedIssues + `) with ordinality c(url, n) join issue_external_links l on lower(l.url) = lower(c.url)
		union all
		select b.key, '` + string(sourceBranch) + `', 3, b.n
			from unnest(` + c.BranchKeys + `) with ordinality b(key, n) join issues i on i.key = b.key
		union all
		select m.key, '` + string(sourceCommitMessage) + `', 4, m.n
			from unnest(` + c.CommitKeys + `) with ordinality m(key, n) join issues i on i.key = m.key
	) s order by s.rank, s.n limit ` + strconv.Itoa(attributionCandidateBound)
}

// attributionHeld is the condition one candidate passes to be the pull request's issue: it
// resolves, through the server's one repository-resolution rule (refs.IssueRepoTerms, joined by
// refs.IssueRepoJoin in the enclosing statement), to the repository the expression repo reads.
// It is never null — a candidate whose issue row is missing, and one whose parent chain ran out
// of depth, is simply not held — so a caller may read it as a plain boolean and a `where` over
// it drops exactly the candidates that are not held. The comparison is lowercased on both sides
// because GitHub spells an owner and a repository as their owners do (`Acme/Widgets`), which is
// why the code before this change used strings.EqualFold.
func attributionHeld(repo string) string {
	return `coalesce(not ` + refs.IssueRepoTooDeep + ` and lower(` + refs.IssueRepoResolved + `) = lower(` + repo + `), false)`
}

// attributionOneColumns are the bound parameters one pull request's candidates are read from:
// its URL, its four inputs and, last, the repository it is in.
var attributionOneColumns = attributionColumns{
	Repo: "$6", URL: "$1", TitleKeys: "$2::text[]", CitedIssues: "$3::text[]",
	BranchKeys: "$4::text[]", CommitKeys: "$5::text[]",
}

// attributionCandidatesQuery is the one statement that resolves a pull request's candidates from
// stored inputs: every candidate the five sources name, best first, each marked whether it is
// held to the pull request's own repository. The repository rule is spliced in over the
// candidate keys the way refs.IssueRepository splices it over one key, so one walk resolves them
// all together. `left join issues`, not `join`: a candidate whose issue row has gone stays in
// the answer, unheld, so a caller can name the candidates it passed over. The text is built
// once, at start, rather than on every read: the concatenation is work a read would repeat for
// a statement that never differs, and pgx caches a prepared plan per statement text.
var attributionCandidatesQuery = `with recursive candidates as (
	` + attributionCandidates(attributionOneColumns) + `
), subjects as (
	select distinct key from candidates
), ` + refs.IssueRepoTerms + `
select c.key, c.source, ` + attributionHeld(attributionOneColumns.Repo) + ` as held
from candidates c
	left join issues i on i.key = c.key
	` + refs.IssueRepoJoin + `
order by c.rank, c.n`

// attributionCandidate is one issue a pull request's sources name: the key, the source that
// named it, and whether it resolves to the repository the pull request is in. Only a held
// candidate may be the pull request's issue; the unheld ones above it are what a caller names
// when it reports what it passed over (headAttribution).
type attributionCandidate struct {
	key    string
	source attributionSource
	held   bool
}

// resolveAttributionCandidates is every issue the stored inputs of the pull request at url, in
// repo, name, best first, each marked held or not: the whole ranked list, so a caller that must
// skip a candidate can take the next rather than make a second read.
func resolveAttributionCandidates(ctx context.Context, pool *store.Pool, repo, url string, inputs AttributionInputs) ([]attributionCandidate, error) {
	rows, err := pool.Query(ctx, attributionCandidatesQuery,
		url, inputs.TitleKeys, inputs.CitedIssues, inputs.BranchKeys, inputs.CommitKeys, repo)
	if err != nil {
		return nil, fmt.Errorf("resolve the issue of %s: %w", url, err)
	}
	defer rows.Close()
	var candidates []attributionCandidate
	for rows.Next() {
		var candidate attributionCandidate
		if err := rows.Scan(&candidate.key, &candidate.source, &candidate.held); err != nil {
			return nil, fmt.Errorf("resolve the issue of %s: %w", url, err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve the issue of %s: %w", url, err)
	}
	return candidates, nil
}

// resolveStoredIssueKey is the issue a pull request's stored inputs name: the first candidate
// held to repo, the repository the pull request is in, and nobody with the source "none" when
// no candidate is. Its callers hand over the repository they already hold — completePullRequest
// its repoFull, readAttributionInputs its row's own repo.
func resolveStoredIssueKey(ctx context.Context, pool *store.Pool, repo, url string, inputs AttributionInputs) (*string, attributionSource, error) {
	candidates, err := resolveAttributionCandidates(ctx, pool, repo, url, inputs)
	if err != nil {
		return nil, sourceNone, err
	}
	for _, candidate := range candidates {
		if candidate.held {
			key := candidate.key
			return &key, candidate.source, nil
		}
	}
	return nil, sourceNone, nil
}

// AttributionResolveBatch is how many stored rows one reconcile pass resolves again
// (AttributePullRequests): a 28-day population is about 4,000 rows, so a pass costs a bounded
// few milliseconds however large the table grows, and the walk comes round to every row in a
// handful of passes at today's size.
const AttributionResolveBatch = 1000

// AttributePullRequests resolves again, from the database alone, the issue of up to limit stored
// pull requests whose inputs have been read, those checked longest ago first, and writes each
// one that changed: an issue that links a pull request, or cites one of its GitHub issues,
// credits it here; one that no longer does, and one that has moved to another repository,
// uncredits it. Every row it reads is marked checked now, so the next pass takes the rows after
// it. Answers how many rows it changed.
//
// One statement and one set-based update, whatever the batch's size: the candidates of every row
// are collected first, then one repository walk resolves all of their issues together
// (refs.IssueRepoTerms over the batch's whole candidate list), then each row takes its own best
// held candidate. Resolving inside the lateral instead would pay for a recursive walk per row —
// AttributionResolveBatch of them a pass.
func AttributePullRequests(ctx context.Context, pool *store.Pool, limit int) (int64, error) {
	var changed int64
	err := pool.QueryRow(ctx, `
		with recursive batch as (
			select repo, number, url, issue_key, attribution_title_keys, attribution_cited_issues,
				attribution_branch_keys, attribution_commit_keys
			from delivery_pull_requests
			where attribution_title_keys is not null
			order by attribution_checked_at nulls first, repo, number
			limit $1
		), candidates as (
			select src.repo as pr_repo, src.number as pr_number, f.key, f.rank, f.n
			from batch src
			left join lateral (`+attributionCandidates(attributionColumns{
		Repo: "src.repo", URL: "src.url", TitleKeys: "src.attribution_title_keys",
		CitedIssues: "src.attribution_cited_issues", BranchKeys: "src.attribution_branch_keys",
		CommitKeys: "src.attribution_commit_keys",
	})+`) f on true
		), subjects as (
			select distinct key from candidates where key is not null
		), `+refs.IssueRepoTerms+`, held as (
			select c.pr_repo, c.pr_number, c.key, c.rank, c.n
			from candidates c
				left join issues i on i.key = c.key
				`+refs.IssueRepoJoin+`
			where `+attributionHeld("c.pr_repo")+`
		), picked as (
			select distinct on (pr_repo, pr_number) pr_repo, pr_number, key
			from held order by pr_repo, pr_number, rank, n
		), resolved as (
			select src.repo, src.number, p.key, src.issue_key is distinct from p.key as changed
			from batch src
			left join picked p on p.pr_repo = src.repo and p.pr_number = src.number
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
