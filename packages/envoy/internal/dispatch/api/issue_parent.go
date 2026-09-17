package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// parseIssueParent decodes the tri-state `parent` field of an issue PATCH. Absent →
// (nil, false): leave it alone. JSON null → (nil, true): clear it. A string → its trimmed
// issue key, which must be non-empty. Anything else → 400 PARENT_INPUT.
func parseIssueParent(raw json.RawMessage) (parent *string, provided bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var key string
	if err := json.Unmarshal(raw, &key); err != nil {
		return nil, true, errorf(http.StatusBadRequest, "PARENT_INPUT", "parent must be an issue key or null")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, true, errorf(http.StatusBadRequest, "PARENT_INPUT", "parent must not be blank")
	}
	return &key, true, nil
}

// parentDepthCap bounds every recursive issue walk — the ancestor walk, the children
// subtree walk, the components lateral's owner search, and the architecture tree's
// containment walks — like refs.Closure's depth cap: with `union` recursion the queries
// terminate even if a raced reparent ever commits a cycle. parentDepthCapSQL is the same
// number spliced into the walks built as SQL text.
const parentDepthCap = 32

var parentDepthCapSQL = strconv.Itoa(parentDepthCap)

// lockIssueAndParent locks the issue and its proposed parent `for update` in key order —
// serializing the pairwise A→B / B→A reparent race (deadlock detection breaks a crossed
// order) — then validates the reparent: the parent must exist (400 PARENT_INPUT), differ
// from the issue, share its project (400 PARENT_INPUT), and not be a descendant of the
// issue (409 PARENT_INPUT naming the cycle path). A concurrent reparent of an unlocked
// ancestor can still race the walk; the depth-capped reads keep terminating regardless.
func lockIssueAndParent(ctx context.Context, tx pgx.Tx, key, parent string) error {
	if parent == key {
		return errorf(http.StatusBadRequest, "PARENT_INPUT", "an issue cannot be its own parent")
	}
	first, second := key, parent
	if parent < key {
		first, second = parent, key
	}
	projects := map[string]string{}
	for _, lockKey := range []string{first, second} {
		var project string
		err := tx.QueryRow(ctx, `select project_key from issues where key = $1 for update`, lockKey).Scan(&project)
		if errors.Is(err, pgx.ErrNoRows) && lockKey == parent {
			return errorf(http.StatusBadRequest, "PARENT_INPUT", "parent issue %s not found", parent)
		}
		if err != nil {
			return err
		}
		projects[lockKey] = project
	}
	if projects[parent] != projects[key] {
		return errorf(http.StatusBadRequest, "PARENT_INPUT", "parent must be in the same project")
	}
	rows, err := tx.Query(ctx, `
		with recursive ancestors as (
			select key, parent_key, 1 as depth from issues where key = $1
			union
			select i.key, i.parent_key, a.depth + 1
			from issues i join ancestors a on i.key = a.parent_key
			where a.depth < $2
		)
		select key from ancestors order by depth
	`, parent, parentDepthCap)
	if err != nil {
		return err
	}
	defer rows.Close()
	chain := []string{}
	for rows.Next() {
		var ancestor string
		if err := rows.Scan(&ancestor); err != nil {
			return err
		}
		chain = append(chain, ancestor)
		if ancestor == key {
			path := key + " → " + strings.Join(chain, " → ")
			return errorf(http.StatusConflict, "PARENT_INPUT", "would create a cycle: %s", path)
		}
	}
	return rows.Err()
}

func stringPointersEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
