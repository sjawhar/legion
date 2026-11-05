package refs

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// IssueDepthCap is the most links an issue may sit below its root: a create or reparent that
// would put an issue deeper is refused (the api package's refuseParentTooDeep), so it bounds
// every recursive issue walk — the ancestor walk below, the children subtree walk, the
// components and repository walks that climb a whole read's chains together, the architecture
// tree's containment walks, and the legion-label walk — like Closure's depth cap: the queries
// terminate even if a raced reparent ever commits a cycle. api's parentDepthCap is this number,
// and the walks it builds as SQL text splice it in.
const IssueDepthCap = 32

// issueDepthCapSQL is IssueDepthCap spliced into the walks built as SQL text.
var issueDepthCapSQL = strconv.Itoa(IssueDepthCap)

// IssueAncestors is the parent chain of the issue key, parent first up to the root, read through
// q: the issues above it on `child_of`, which is issues.parent_key (the graph_edges view's
// child_of arm). A root issue, an issue no row has, and the empty key answer []. This is the
// server's one recursive ancestor walk: an issue event's ancestors (api's issueAncestors), a
// reparent's cycle refusal (api's validateIssueParent, which reads it from the proposed parent)
// and a block pointer's ancestor rule (docs' refuseBlockPointers) are all this query.
func IssueAncestors(ctx context.Context, q Queryer, key string) ([]string, error) {
	ancestors := []string{}
	if key == "" {
		return ancestors, nil
	}
	if err := q.QueryRow(ctx, `
		with recursive ancestors as (
			select p.key, p.parent_key, 1 as depth
			from issues i join issues p on p.key = i.parent_key where i.key = $1
			union
			select i.key, i.parent_key, a.depth + 1
			from issues i join ancestors a on i.key = a.parent_key
			where a.depth < $2
		)
		select coalesce(array_agg(key order by depth), '{}') from ancestors
	`, key, IssueDepthCap).Scan(&ancestors); err != nil {
		return nil, fmt.Errorf("read the ancestors of %s: %w", key, err)
	}
	return ancestors, nil
}

// IssueDescendants is every issue below the issue key on `child_of`, at any status, read through
// q: its children, their children, and so on, bounded by IssueDepthCap like the ancestor walk
// above, so the query terminates even if a raced reparent ever commits a cycle. The key itself is
// not in the answer. An issue with no child, an issue no row has, and the empty key answer [].
// This is the server's one recursive descendant walk over whole subtrees: api's coverage read is
// this query. api's loadChildren is a different read — one row per direct child, with a rollup
// over that child's own subtree — and stays its own.
func IssueDescendants(ctx context.Context, q Queryer, key string) ([]string, error) {
	descendants := []string{}
	if key == "" {
		return descendants, nil
	}
	if err := q.QueryRow(ctx, `
		with recursive descendants as (
			select c.key, 1 as depth from issues c where c.parent_key = $1
			union
			select c.key, d.depth + 1
			from issues c join descendants d on c.parent_key = d.key
			where d.depth < $2
		)
		select coalesce(array_agg(key order by key), '{}') from descendants
	`, key, IssueDepthCap).Scan(&descendants); err != nil {
		return nil, fmt.Errorf("read the descendants of %s: %w", key, err)
	}
	return descendants, nil
}

// IssueRepoTerms resolve the repository of every issue of `subjects`, the one-column term of
// issue keys each statement that splices these defines before them (api's issueComponentsTerms
// reads the same one): the nearest issue on its parent chain (itself first) whose own `repo` is
// set, else the project's default mapping, else null. One walk climbs every subject's chain
// together and one join reads the defaults of every project at once, so a read of many issues
// pays one set of joins rather than a walk and a default lookup per row. A chain stops climbing
// at the first issue that names a repository or at IssueDepthCap links up, which writes never
// let a chain pass (api's refuseParentTooDeep): at most IssueDepthCap+1 rows per subject, at
// most one of them naming a repository, which is why `max` over a chain is the nearest
// repository and needs no order. A walk that reaches the cap with links still above it sets
// `too_deep`, and the reader of these columns refuses it rather than answer with the default
// (api's repoScan.resolve, IssueRepository below). Such a chain is a loop, or one a direct
// database write made, or one an earlier release's API built: migration 0113 refuses to apply
// while one exists, but an earlier release left serving after it, against the rollout rule
// (cmd/dispatch/README.md), can build one again. A paged listing resolves only the issues of
// its page, so such a chain fails the page it is on and no other (api/issue_queries.go,
// issueSummaryQuery).
//
// This is the server's one repository-resolution rule. The issue page resolves a page of issues
// through these terms (api's issue reads), IssueRepository resolves one key through them, and
// delivery's attribution splices them over the candidate keys of a pull request to hold its
// issue to the repository it is in, so the currency mark and the pull-request head attribution
// can never disagree with the issue page.
var IssueRepoTerms = `repo_chain as (
		select s.key as start, i.parent_key, i.repo, 0 as depth
		from subjects s join issues i on i.key = s.key
		union all
		select c.start, p.parent_key, p.repo, c.depth + 1
		from repo_chain c join issues p on p.key = c.parent_key
		where c.repo is null and c.depth < ` + issueDepthCapSQL + `
	), repo as (
		select c.start as key,
		       max(c.repo) as owned,
		       bool_or(c.depth = ` + issueDepthCapSQL + ` and c.repo is null and c.parent_key is not null) as too_deep
		from repo_chain c
		group by c.start
	)`

// IssueRepoJoin attaches IssueRepoTerms' answer to the issue aliased `i` in the enclosing query,
// with the default mapping of every project those issues belong to, read once. One row per
// issue: a key that is no issue has no chain and no row, which the columns below read as the
// default alone.
const IssueRepoJoin = `left join repo on repo.key = i.key
	left join repo_projects repo_default on repo_default.project = i.project_key and repo_default.is_default`

// IssueRepoResolved is the repository an issue resolves to, over IssueRepoJoin: the nearest one
// its chain names, else its project's default mapping, else null. A statement that holds an
// issue to a repository rather than reading one — delivery's attributionHeld, which keeps a
// pull request's issue to the repository the pull request is in — compares against this
// expression, so it can never be a second copy of the rule drifting from this one.
const IssueRepoResolved = `coalesce(repo.owned, repo_default.repo)`

// IssueRepoTooDeep reports whether the walk ran out of depth with links still above it, which
// every reader of the resolved repository refuses rather than answer with the project's default
// (api's repoScan.resolve, IssueRepository below, delivery's attributionHeld).
const IssueRepoTooDeep = `coalesce(repo.too_deep, false)`

// IssueRepoColumns are the repository columns every issue read scans, in the order api's
// repoScan and IssueRepository below take them: the issue's own, the nearest one its chain names
// else its project's default, and whether the walk ran out of depth.
const IssueRepoColumns = `i.repo, ` + IssueRepoResolved + `, ` + IssueRepoTooDeep

// issueRepositoryQuery is IssueRepoTerms over a subjects term of one key, the shape api's
// issueWithProgressQuery gives the issue page's single-key read. The text is built once, at
// start, rather than on every read: the concatenation is work a request would repeat for a
// statement that never differs, and pgx caches a prepared plan per statement text.
var issueRepositoryQuery = `
	with recursive subjects as (select $1::text as key), ` + IssueRepoTerms + `
	select ` + IssueRepoColumns + `
	from issues i ` + IssueRepoJoin + `
	where i.key = $1`

// IssueRepository is the repository the issue key resolves to, read through q: its own, the
// nearest one above it on `child_of`, or its project's default mapping; nil when none resolves,
// and nil for an issue no row has and for the empty key. It runs IssueRepoTerms for one key, the
// terms the issue reads run for a whole page, so it answers exactly what the issue page shows; a
// chain the walk could not finish is an error rather than the project's default, which would be
// a wrong answer. The results route makes this read for the issue of a check.
func IssueRepository(ctx context.Context, q Queryer, key string) (*string, error) {
	if key == "" {
		return nil, nil
	}
	// own is the issue's own repository, which this read scans and does not answer: the columns
	// stay the issue reads' own set so the two cannot drift apart.
	var own, resolved *string
	var tooDeep bool
	if err := q.QueryRow(ctx, issueRepositoryQuery, key).Scan(&own, &resolved, &tooDeep); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolve the repository of %s: %w", key, err)
	}
	if tooDeep {
		return nil, fmt.Errorf("resolve the repository of %s: its parent chain is deeper than %d links or loops", key, IssueDepthCap)
	}
	return resolved, nil
}
