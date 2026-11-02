package refs

import (
	"context"
	"fmt"
)

// IssueDepthCap is the most links an issue may sit below its root: a create or reparent that
// would put an issue deeper is refused (the api package's refuseParentTooDeep), so it bounds
// every recursive issue walk — the ancestor walk below, the children subtree walk, the components
// and repository laterals' owner searches, the architecture tree's containment walks, and the
// legion-label walk — like Closure's depth cap: the queries terminate even if a raced reparent
// ever commits a cycle. api's parentDepthCap is this number, and the walks it builds as SQL text
// splice it in.
const IssueDepthCap = 32

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
