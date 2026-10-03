package store

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// OpenTreeLifecycle is the explicit authority entry point. Workflow admission calls it inside its
// fact transaction; the authenticated operator route calls it before its root claim is persisted.
func (s *Store) OpenTreeLifecycle(ctx context.Context, project, tree string, authority treelifecycle.Authority) (treelifecycle.Lifecycle, error) {
	var lifecycle treelifecycle.Lifecycle
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		opened, err := treelifecycle.Open(ctx, tx, project, tree, authority)
		if err != nil {
			return err
		}
		lifecycle = opened
		return nil
	})
	if err != nil {
		return treelifecycle.Lifecycle{}, err
	}
	return lifecycle, nil
}

// AdmitClaim binds a new or reactivated claim to an open lifecycle and writes both in one short
// globally serialized transaction. The caller receives the epoch it must carry through every later
// launch; runtime work happens after this transaction commits.
func (s *Store) AdmitClaim(ctx context.Context, c supervise.Claim) (supervise.Claim, error) {
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		epoch, err := treelifecycle.BindWork(ctx, tx, c.Project, c.Tree)
		if err != nil {
			return err
		}
		c.TreeEpoch = epoch
		return putClaim(ctx, tx, c)
	})
	if err != nil {
		if errors.Is(err, treelifecycle.ErrCleanupReserved) {
			return supervise.Claim{}, fmt.Errorf("admit claim %s: %w", c.Token, errors.Join(ErrIssueCleanupInProgress, err))
		}
		return supervise.Claim{}, fmt.Errorf("admit claim %s: %w", c.Token, err)
	}
	return c, nil
}

// CheckLaunch rejects a delayed claim before it can persist StateLaunching or call a runtime after
// the tree lifecycle reserved cleanup. The machine returns this named error directly, so outbox
// retry preserves its claim and launch budget.
func (s *Store) CheckLaunch(ctx context.Context, c supervise.Claim) error {
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := treelifecycle.CheckWork(ctx, tx, c.Project, c.Tree, c.TreeEpoch); err != nil {
			return fmt.Errorf("launch claim %s: %w", c.Token, err)
		}
		return nil
	})
	if errors.Is(err, treelifecycle.ErrCleanupReserved) {
		return errors.Join(ErrIssueCleanupInProgress, err)
	}
	return err
}

// ReserveWorkflowTreeCleanup records the workflow close before any resource snapshot. A retry of an
// unconfirmed reservation resumes regardless of a subsequent re-admission; a retry after the
// cleanup confirmed reserves nothing; a fresh reservation first proves the root is still lingering
// at the close's generation.
func (s *Store) ReserveWorkflowTreeCleanup(ctx context.Context, project, tree string, rootGeneration uint64) (treelifecycle.Lifecycle, bool, error) {
	if rootGeneration == 0 || rootGeneration > math.MaxInt64 {
		return treelifecycle.Lifecycle{}, false, fmt.Errorf("reserve workflow cleanup %s: invalid root generation %d", tree, rootGeneration)
	}
	var lifecycle treelifecycle.Lifecycle
	var reserved bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		var started, confirmed bool
		err := tx.QueryRow(ctx, `select cleanup_started, cleanup_confirmed_at is not null from tree_lifecycles
			where project = $1 and tree = $2 for update`, project, tree).Scan(&started, &confirmed)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && started && !confirmed {
			lifecycle, err = treelifecycle.ReserveCleanup(ctx, tx, project, tree, treelifecycle.AuthorityWorkflow)
			reserved = true
			return err
		}
		if err == nil && started && confirmed {
			return nil
		}
		var generation int64
		var lingering bool
		// Issue keys are unique across projects; the issue row carries the Dispatch project key while
		// project here is the normalized runtime project, so the key alone names the root.
		if err := tx.QueryRow(ctx, `select generation, linger_until is not null from issues
			where key = $1 for update`, tree).Scan(&generation, &lingering); err != nil {
			return fmt.Errorf("read workflow tree %s: %w", tree, err)
		}
		if uint64(generation) != rootGeneration || !lingering {
			return nil
		}
		lifecycle, err = treelifecycle.ReserveCleanup(ctx, tx, project, tree, treelifecycle.AuthorityWorkflow)
		reserved = err == nil
		return err
	})
	if err != nil {
		return treelifecycle.Lifecycle{}, false, err
	}
	return lifecycle, reserved, nil
}

// ReserveOperatorTreeCleanup is called only after api.closeTree authenticated the root operator
// close. It is not selected through a zero workflow generation. A close asked again after its
// cleanup confirmed reserves nothing, so the operator's retry answers as the finished close it is.
func (s *Store) ReserveOperatorTreeCleanup(ctx context.Context, project, tree string) (treelifecycle.Lifecycle, bool, error) {
	var lifecycle treelifecycle.Lifecycle
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		reserved, err := treelifecycle.ReserveCleanup(ctx, tx, project, tree, treelifecycle.AuthorityOperator)
		if err != nil {
			return err
		}
		lifecycle = reserved
		return nil
	})
	if errors.Is(err, treelifecycle.ErrLifecycleClosed) {
		return treelifecycle.Lifecycle{}, false, nil
	}
	if err != nil {
		return treelifecycle.Lifecycle{}, false, err
	}
	return lifecycle, true, nil
}

// PendingTreeClaims names every stored claim of the tree that has not retired, as token:state.
// Read after the tree's cleanup reservation committed, it is the tree's complete runnable
// population: no later claim can bind to the reserved epoch, and a claim that won before the
// reservation is already stored, whether or not its resources were admitted yet.
func (s *Store) PendingTreeClaims(ctx context.Context, project, tree string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `select token, state from claims where project = $1 and tree = $2 and state <> 'retired'
		order by token`, project, tree)
	if err != nil {
		return nil, fmt.Errorf("census claims of tree %s: %w", tree, err)
	}
	defer rows.Close()
	var pending []string
	for rows.Next() {
		var token, state string
		if err := rows.Scan(&token, &state); err != nil {
			return nil, fmt.Errorf("census claims of tree %s: %w", tree, err)
		}
		pending = append(pending, token+":"+state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("census claims of tree %s: %w", tree, err)
	}
	return pending, nil
}

// CheckTreeCleanupReservation proves the physical caller owns the current explicitly reserved
// epoch, taking the shared serializer before the lifecycle row (treelifecycle.CheckReservation).
func (s *Store) CheckTreeCleanupReservation(ctx context.Context, project, tree string, epoch uint64) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		return treelifecycle.CheckReservation(ctx, tx, project, tree, epoch)
	})
}

// ConfirmTreeCleanup records complete API-confirmed release. The next explicit root admission, not
// a delayed claim or old outbox row, opens the following epoch.
func (s *Store) ConfirmTreeCleanup(ctx context.Context, project, tree string, epoch uint64) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		return treelifecycle.ConfirmCleanup(ctx, tx, project, tree, epoch)
	})
}
