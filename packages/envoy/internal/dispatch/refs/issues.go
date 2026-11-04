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
// every recursive issue walk — the ancestor walk below, the children subtree walk, the components
// and repository laterals' owner searches, the architecture tree's containment walks, and the
// legion-label walk — like Closure's depth cap: the queries terminate even if a raced reparent
// ever commits a cycle. api's parentDepthCap is this number, and the walks it builds as SQL text
// splice it in.
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

// IssueRepoLateral resolves the repository of the issue aliased `i` in the enclosing query: the
// nearest issue on its parent chain (itself first) whose own `repo` is set, else the project's
// default mapping, else null. Joined `left join lateral (...) repo on true`, it yields one row
// per issue. The walk climbs one link per step, keeping only a depth counter, and stops at the
// first issue that names a repository or at IssueDepthCap links up, which writes never let a
// chain pass (api's refuseParentTooDeep): at most IssueDepthCap+1 steps per issue. A walk that
// reaches the cap with links still above it sets `too_deep`, and the read refuses it rather than
// answer with the default (api's repoScan.resolve, IssueRepository below). Such a chain is a
// loop, or one a direct database write made, or one an earlier release's API built: migration
// 0113 refuses to apply while one exists, but an earlier release left serving after it, against
// the rollout rule (cmd/dispatch/README.md), can build one again.
//
// This is the server's one repository-resolution rule. The issue page resolves many issues at
// once through it (api's issue reads), and IssueRepository resolves one through it, so the
// currency mark and the pull-request head attribution can never disagree with the issue page.
var IssueRepoLateral = `
	left join lateral (
		with recursive chain as (
			select i.parent_key, i.repo, 0 as depth
			union all
			select p.parent_key, p.repo, c.depth + 1
			from issues p join chain c on p.key = c.parent_key
			where c.repo is null and c.depth < ` + issueDepthCapSQL + `
		)
		select
			coalesce(
				(select c.repo from chain c where c.repo is not null order by c.depth limit 1),
				(select d.repo from repo_projects d where d.project = i.project_key and d.is_default)
			) as resolved,
			exists (
				select 1 from chain c
				where c.depth = ` + issueDepthCapSQL + ` and c.repo is null and c.parent_key is not null
			) as too_deep
	) repo on true`

// IssueRepository is the repository the issue key resolves to, read through q: its own, the
// nearest one above it on `child_of`, or its project's default mapping; nil when none resolves,
// and nil for an issue no row has and for the empty key. It is IssueRepoLateral for one issue,
// so it answers exactly what the issue page shows; a chain the walk could not finish is an error
// rather than the project's default, which would be a wrong answer. The results route and the
// pull-request head attribution both make this read.
func IssueRepository(ctx context.Context, q Queryer, key string) (*string, error) {
	if key == "" {
		return nil, nil
	}
	var resolved *string
	var tooDeep bool
	if err := q.QueryRow(ctx, `
		select repo.resolved, repo.too_deep
		from issues i`+IssueRepoLateral+`
		where i.key = $1
	`, key).Scan(&resolved, &tooDeep); err != nil {
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
