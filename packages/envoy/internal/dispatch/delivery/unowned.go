package delivery

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// UnownedIssue is one open P0 issue with neither a claim nor a route.
type UnownedIssue struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// ListUnownedP0 is the "P0 issues with no owner" KPI (CONTRACT.md "KPI targets"): every issue with
// priority 0, status other than done, claimed_by null and route null, across every project, in
// project then number order (the prototype's collector/src/collector/dispatch.py:157-170
// unowned_p0, without its fetch of three named projects). It follows neither the measures' window
// nor their facets.
func ListUnownedP0(ctx context.Context, pool *store.Pool) ([]UnownedIssue, error) {
	rows, err := pool.Query(ctx, `
		select key, title from issues
		where priority = 0 and status <> 'done' and claimed_by is null and route is null
		order by project_key, number
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	issues := []UnownedIssue{}
	for rows.Next() {
		var issue UnownedIssue
		if err := rows.Scan(&issue.Key, &issue.Title); err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}
