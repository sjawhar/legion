package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
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

// lockIssueAndParent locks the issue and its proposed parent `for no key update` in key order —
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
		err := tx.QueryRow(ctx, `select project_key from issues where key = $1 for no key update`, lockKey).Scan(&project)
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

// waitNode is one half of an issue in the dependency graph. An issue starts after its blocked_by
// targets are done and after its parent starts; it is done after it starts and after its children
// are done. Splitting the two halves lets sibling dependencies stand while still refusing a child
// that waits on its parent, or a parent that waits on its descendant.
type waitNode struct {
	key  string
	done bool
}

// assertParentLeavesNoDependencyCycle checks a parent link already written in tx. The link adds
// two waits: the parent's done waits on the child's done, and the child's start waits on the
// parent's start.
func assertParentLeavesNoDependencyCycle(ctx context.Context, tx pgx.Tx, child, parent string) error {
	if err := assertNoWaitPath(ctx, tx, []waitNode{{key: child, done: true}}, waitNode{key: parent, done: true}); err != nil {
		return err
	}
	return assertNoWaitPath(ctx, tx, []waitNode{{key: parent}}, waitNode{key: child})
}

// writeBlockedBy replaces issue's dependency targets after locking each target in key order.
// Every target must stay in the issue's project; the dependency walk rejects a target through
// which issue would wait on itself before the replacement reaches the graph.
func writeBlockedBy(ctx context.Context, tx pgx.Tx, issue, project string, targets []string) error {
	slices.Sort(targets)
	targets = slices.Compact(targets)
	for _, target := range targets {
		var targetProject string
		err := tx.QueryRow(ctx, `select project_key from issues where key = $1 for no key update`, target).Scan(&targetProject)
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(http.StatusBadRequest, "BLOCKED_BY_INPUT", "blocked_by issue %s not found", target)
		}
		if err != nil {
			return err
		}
		if targetProject != project {
			return errorf(http.StatusBadRequest, "BLOCKED_BY_OUTSIDE_PROJECT", "blocked_by issue %s must be in the same project", target)
		}
	}
	from := make([]waitNode, len(targets))
	for index, target := range targets {
		from[index] = waitNode{key: target, done: true}
	}
	if err := assertNoWaitPath(ctx, tx, from, waitNode{key: issue}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from issue_links where issue_key = $1 and kind = 'blocked_by'`, issue); err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		insert into issue_links (issue_key, kind, target_key)
		select $1, 'blocked_by', target_key from unnest($2::text[]) as target_key
	`, issue, targets)
	return err
}

// assertNoWaitPath refuses a new wait whose head reaches its tail. It locks each frontier's issue
// rows for no key update in key order before reading their links, as lockIssueAndParent does, and
// stops at parentDepthCap without taking a project-wide lock.
func assertNoWaitPath(ctx context.Context, tx pgx.Tx, from []waitNode, goal waitNode) error {
	seen := map[waitNode]bool{}
	frontier := from
	for depth := 0; len(frontier) > 0; depth++ {
		if depth > parentDepthCap {
			return errorf(http.StatusConflict, "DEPENDENCY_CYCLE", "the dependency graph is deeper than %d links", parentDepthCap)
		}
		var keys, starts, dones []string
		for _, node := range frontier {
			halves := []waitNode{node, {key: node.key}}
			if !node.done {
				halves = halves[:1]
			}
			for _, half := range halves {
				if half == goal {
					return errorf(http.StatusConflict, "DEPENDENCY_CYCLE", "%s would wait on itself through blocked_by and parent links", goal.key)
				}
				if seen[half] {
					continue
				}
				seen[half] = true
				keys = append(keys, half.key)
				if half.done {
					dones = append(dones, half.key)
				} else {
					starts = append(starts, half.key)
				}
			}
		}
		slices.Sort(keys)
		keys = slices.Compact(keys)
		if _, err := tx.Exec(ctx, `select key from issues where key = any($1) order by key for no key update`, keys); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			select parent_key, false from issues where key = any($1) and parent_key is not null
			union
			select target_key, true from issue_links where issue_key = any($1) and kind = 'blocked_by'
			union
			select key, true from issues where parent_key = any($2)
		`, starts, dones)
		if err != nil {
			return err
		}
		frontier, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (waitNode, error) {
			var node waitNode
			return node, row.Scan(&node.key, &node.done)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func stringPointersEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
