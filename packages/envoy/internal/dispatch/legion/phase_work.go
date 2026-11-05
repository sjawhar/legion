package legion

// phase_work.go holds the one read of the phase-work half of tree.go's rule: which issues carry
// Legion phase work. The Legion view (api/legion_phase_work.go, which keeps the answer and grows
// it from the event feed) and the metrics (legion/metrics, which reads it with the rest of its
// population) both ask it, so the two place an issue in the same tree.

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// PhaseWorkKeys is every issue that carries Legion phase work, over the whole log: the issues a
// handoff was recorded for, and the issues an event of a session titled `Legion <role> · <that
// issue's key>` was written on. It is read project-wide, which is what makes the answer a fact of
// the project rather than of a window (tree.go).
//
// The title rule is this package's own (RoleSessionWorks, session_title.go), and the titles are
// resolved the one way Dispatch resolves them, each session's newest
// (delivery.ResolveSessionTitles). Every reader of the fact comes through here, so a session
// renamed after it wrote, or an event that carried no title of its own, moves the board and the
// metrics together.
func PhaseWorkKeys(ctx context.Context, pool *store.Pool) (map[string]struct{}, error) {
	keys := map[string]struct{}{}
	if err := readHandoffIssues(ctx, pool, keys); err != nil {
		return nil, err
	}
	written, sessions, err := readSessionIssues(ctx, pool)
	if err != nil {
		return nil, err
	}
	titles, err := delivery.ResolveSessionTitles(ctx, pool, sessions)
	if err != nil {
		return nil, fmt.Errorf("resolve the session titles of the Legion phase work: %w", err)
	}
	for _, one := range written {
		if RoleSessionWorks(titles[one.session], one.issue) {
			keys[one.issue] = struct{}{}
		}
	}
	return keys, nil
}

// readHandoffIssues takes every issue a handoff was recorded for into keys.
func readHandoffIssues(ctx context.Context, pool *store.Pool, keys map[string]struct{}) error {
	rows, err := pool.Query(ctx, `select distinct issue_key from handoffs`)
	if err != nil {
		return fmt.Errorf("read the issues a handoff was recorded for: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return fmt.Errorf("scan an issue a handoff was recorded for: %w", err)
		}
		keys[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the issues a handoff was recorded for: %w", err)
	}
	return nil
}

// sessionIssue is one session that wrote an event on one issue, a candidate for phase work until
// the session's title is resolved.
type sessionIssue struct{ issue, session string }

// readSessionIssues is every (issue, session) pair an event names, once each, with the sessions
// as a sorted list for the title read. One row per pair is all the rule needs: whether a session
// the title rule makes a Legion role of wrote on the issue at all.
func readSessionIssues(ctx context.Context, pool *store.Pool) ([]sessionIssue, []string, error) {
	rows, err := pool.Query(ctx, `
		select distinct issue_key, actor->>'id'
		from events
		where issue_key is not null and actor->>'kind' = 'session' and actor->>'id' is not null
	`)
	if err != nil {
		return nil, nil, fmt.Errorf("read the sessions that wrote on the issues: %w", err)
	}
	defer rows.Close()
	var written []sessionIssue
	sessions := map[string]struct{}{}
	for rows.Next() {
		var one sessionIssue
		if err := rows.Scan(&one.issue, &one.session); err != nil {
			return nil, nil, fmt.Errorf("scan a session that wrote on an issue: %w", err)
		}
		written = append(written, one)
		sessions[one.session] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the sessions that wrote on the issues: %w", err)
	}
	return written, slices.Sorted(maps.Keys(sessions)), nil
}
