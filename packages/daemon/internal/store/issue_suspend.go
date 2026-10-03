package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// IssueSuspension checks a workflow close against current issue/root generations, later starts,
// unfinished role stops and the complete stored role population. The caller holds the runtime's
// issue launch lock through this check and its Sandbox patch, never this database transaction.
// A superseded close is finished without acting; an unfinished stop remains a visible retry.
func (s *Store) IssueSuspension(ctx context.Context, project string, key runtime.IssueResourceKey) (IssueResources, bool, error) {
	if key.Issue == "" || key.Tree == "" || key.IssueGeneration == 0 || key.TreeGeneration == 0 || key.StopRow <= 0 {
		return IssueResources{}, false, errors.New("issue suspension requires issue, tree, both generations and its outbox row")
	}
	var resources IssueResources
	var act bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		var current bool
		if err := tx.QueryRow(ctx, `select exists (
			select 1 from issues i join issues root on root.key = i.tree
			where i.key = $1 and i.tree = $2 and i.generation = $3 and root.generation = $4
			and (root.linger_until is not null or (i.key <> i.tree and i.phase = 'done'
				and i.status in ('done', 'backlog', 'icebox', 'triage'))))`,
			key.Issue, key.Tree, key.IssueGeneration, key.TreeGeneration).Scan(&current); err != nil {
			return err
		}
		if !current {
			return nil
		}
		var restarted bool
		if err := tx.QueryRow(ctx, `select exists (
			select 1 from claims where project = $1 and issue = $2 and last_start_row > $3)
			or exists (select 1 from outbox where issue = $2 and id > $3 and kind = 'supervise' and payload->>'op' = 'start')`,
			project, key.Issue, key.StopRow).Scan(&restarted); err != nil {
			return err
		}
		if restarted {
			return nil
		}
		var stopping bool
		if err := tx.QueryRow(ctx, `select exists (
			select 1 from outbox where issue = $1 and id < $2 and kind = 'supervise'
			and payload->>'op' = 'suspend' and (payload->>'generation')::bigint = $3)`,
			key.Issue, key.StopRow, key.IssueGeneration).Scan(&stopping); err != nil {
			return err
		}
		if stopping {
			return fmt.Errorf("issue %s suspension waits for its role stop effects", key.Issue)
		}
		var pending string
		err := tx.QueryRow(ctx, `select token || ':' || state from claims where project = $1 and issue = $2
			and (state not in ('suspended', 'failed', 'retired') or locator is not null) order by token limit 1`, project, key.Issue).Scan(&pending)
		if err == nil {
			return fmt.Errorf("issue %s suspension waits for stored claim %s", key.Issue, pending)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		resources, err = scanIssueResources(tx.QueryRow(ctx, selectIssueResources+` where project = $1 and issue = $2`, project, key.Issue))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if resources.Tree != key.Tree {
			return fmt.Errorf("issue %s resources belong to tree %s, not %s", key.Issue, resources.Tree, key.Tree)
		}
		act = !resources.CleanupStarted
		return nil
	})
	if err != nil {
		return IssueResources{}, false, fmt.Errorf("authorize issue suspension: %w", err)
	}
	return resources, act, nil
}
