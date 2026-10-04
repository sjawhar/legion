package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// IssueClose is one workflow close of an issue, as its issue_suspend outbox row names it: the run
// it closed (the issue's generation and its tree root's) and the row, written after the close's
// per-role stops. A later start supersedes it even within the same generation.
type IssueClose struct {
	Issue, Tree                     string
	IssueGeneration, TreeGeneration uint64
	Row                             int64
}

// IssueSuspension decides whether a workflow close may suspend its issue's Sandbox now, in one
// short transaction under the shared serializer: false, finishing the close without acting, once
// the run it closed is over (the root no longer lingers at its generation, and the issue is no
// child that left the workflow done), a later start of the issue was written, or the tree's
// cleanup is reserved, which deletes what a suspension would keep; a wait while one of the close's
// role stops is unfinished or a stored claim of the issue may still hold a process. The caller
// holds the runtime's issue launch lock through this check and its Sandbox patch, never this
// transaction. outOfWorkflow is the workflow's rule for a status that takes an issue out of it
// (record.OutOfWorkflow), which the store does not restate.
func (s *Store) IssueSuspension(ctx context.Context, project string, close IssueClose, outOfWorkflow func(status string) bool) (bool, error) {
	if close.Issue == "" || close.Tree == "" || close.IssueGeneration == 0 || close.TreeGeneration == 0 || close.Row <= 0 {
		return false, errors.New("issue suspension requires issue, tree, both generations and its outbox row")
	}
	var act bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		var issueGeneration, rootGeneration int64
		var issuePhase, status string
		var lingering bool
		err := tx.QueryRow(ctx, `select i.generation, i.phase, i.status, root.generation, root.linger_until is not null
			from issues i join issues root on root.key = i.tree where i.key = $1 and i.tree = $2`,
			close.Issue, close.Tree).Scan(&issueGeneration, &issuePhase, &status, &rootGeneration, &lingering)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		leftDone := close.Issue != close.Tree && phase.Phase(issuePhase) == phase.Done && outOfWorkflow(status)
		if uint64(issueGeneration) != close.IssueGeneration || uint64(rootGeneration) != close.TreeGeneration || !(lingering || leftDone) {
			return nil
		}
		var restarted bool
		if err := tx.QueryRow(ctx, `select exists (
			select 1 from claims where project = $1 and issue = $2 and last_start_row > $3)
			or exists (select 1 from outbox where issue = $2 and id > $3 and kind = 'supervise' and payload->>'op' = 'start')`,
			project, close.Issue, close.Row).Scan(&restarted); err != nil {
			return err
		}
		if restarted {
			return nil
		}
		var reserved bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from tree_lifecycles
			where project = $1 and tree = $2 and cleanup_started)`, project, close.Tree).Scan(&reserved); err != nil {
			return err
		}
		if reserved {
			return nil
		}
		var stopping bool
		if err := tx.QueryRow(ctx, `select exists (
			select 1 from outbox where issue = $1 and id < $2 and kind = 'supervise'
			and payload->>'op' = 'suspend' and (payload->>'generation')::bigint = $3)`,
			close.Issue, close.Row, close.IssueGeneration).Scan(&stopping); err != nil {
			return err
		}
		if stopping {
			return wait.Errorf("issue %s suspension waits for its role stop effects", close.Issue)
		}
		var pending string
		err = tx.QueryRow(ctx, `select token || ':' || state from claims where project = $1 and issue = $2 and `+claimMayRun+`
			order by token limit 1`, project, close.Issue).Scan(&pending)
		if err == nil {
			return wait.Errorf("issue %s suspension waits for stored claim %s", close.Issue, pending)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		act = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("authorize issue suspension: %w", err)
	}
	return act, nil
}
