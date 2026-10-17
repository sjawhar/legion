package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// parseIssueRepo decodes the tri-state `repo` field of an issue write. Absent → (nil, false): leave
// it alone. JSON null → (nil, true): clear it, so the issue resolves through its parent chain and the
// project's default again. A string → its canonical `owner/name` (externalRef's canonicalisation,
// which lowercases and drops a `.git` suffix); anything else → 400 INVALID_ISSUE.
func parseIssueRepo(raw json.RawMessage) (repo *string, provided bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, true, errorf(http.StatusBadRequest, "INVALID_ISSUE", `repo must be "owner/name" or null`)
	}
	canonical, _, err := externalRef(strings.TrimSpace(value) + "#1")
	if err != nil {
		return nil, true, errorf(http.StatusBadRequest, "INVALID_ISSUE", `repo must be "owner/name" (got %q)`, value)
	}
	return &canonical, true, nil
}

// checkIssueRepoMapped refuses, by name, a repository that is not a mapping of the issue's project,
// before the write reaches the foreign key that would refuse it without one. It holds the mapping's
// row in key share until the write commits, so the mapping cannot be deleted or moved to another
// project in between (deleteRepoProject and putRepoProject lock it for update).
func checkIssueRepoMapped(ctx context.Context, tx pgx.Tx, repo, project string) error {
	err := tx.QueryRow(ctx, `
		select 1 from repo_projects where repo = $1 and project = $2 for key share
	`, repo, project).Scan(new(int))
	if errors.Is(err, pgx.ErrNoRows) {
		return errorf(http.StatusBadRequest, "REPO_NOT_MAPPED", "%s is not mapped to project %s (Settings → Repositories → Projects)", repo, project)
	}
	return err
}

// issueRepoLateral resolves the repository of the issue aliased `i` in the enclosing query: the
// nearest issue on its parent chain (itself first) whose own `repo` is set, else the project's
// default mapping, else null. Joined `left join lateral (...) repo on true`, it yields one row per
// issue. The walk climbs one link per step, keeping only a depth counter, and stops at the first
// issue that names a repository or at parentDepthCap links up, which writes never let a chain pass
// (refuseParentTooDeep): at most parentDepthCap+1 steps per issue. A walk that reaches the cap with
// links still above it sets `too_deep`, and repoScan.resolve refuses it rather than answer with the
// default. Such a chain is a loop, or one a direct database write made, or one an earlier release's
// API built: migration 0113 refuses to apply while one exists, but an earlier release left serving
// after it, against the rollout rule (cmd/dispatch/README.md), can build one again.
var issueRepoLateral = `
	left join lateral (
		with recursive chain as (
			select i.parent_key, i.repo, 0 as depth
			union all
			select p.parent_key, p.repo, c.depth + 1
			from issues p join chain c on p.key = c.parent_key
			where c.repo is null and c.depth < ` + parentDepthCapSQL + `
		)
		select
			coalesce(
				(select c.repo from chain c where c.repo is not null order by c.depth limit 1),
				(select d.repo from repo_projects d where d.project = i.project_key and d.is_default)
			) as resolved,
			exists (
				select 1 from chain c
				where c.depth = ` + parentDepthCapSQL + ` and c.repo is null and c.parent_key is not null
			) as too_deep
	) repo on true`

// issueRepoColumns are the repository columns every issue read scans, in repoScan's order.
const issueRepoColumns = `i.repo, repo.resolved, repo.too_deep`

// repoScan receives issueRepoColumns for one issue.
type repoScan struct {
	own, resolved *string
	tooDeep       bool
}

func (r *repoScan) targets() []any { return []any{&r.own, &r.resolved, &r.tooDeep} }

// resolve answers the issue's own and resolved repository, refusing a parent chain the walk could
// not finish within parentDepthCap links: such a chain names no answer, and the project's default
// would be a wrong one.
func (r *repoScan) resolve(key string) (own, resolved *string, err error) {
	if r.tooDeep {
		return nil, nil, fmt.Errorf("resolve the repository of %s: its parent chain is deeper than %d links or loops", key, parentDepthCap)
	}
	return r.own, r.resolved, nil
}
