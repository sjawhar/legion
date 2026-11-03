package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// issueRepoLateral and issueRepoColumns are refs' repository-resolution rule, the one every read
// that resolves an issue's repository is built on: the issue reads here, the currency mark and
// the pull-request head attribution (refs.IssueRepository). They are named here, as
// parentDepthCap names refs.IssueDepthCap, so this package's queries read as its own text.
var issueRepoLateral = refs.IssueRepoLateral

const issueRepoColumns = refs.IssueRepoColumns

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
