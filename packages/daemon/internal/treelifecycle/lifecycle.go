package treelifecycle

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/wait"
)

// Authority names the only producers that may open runnable work for a tree. A workflow root is
// opened by its admission fact; an operator tree is opened only by the authenticated operator route.
// The empty value is never authority.
type Authority string

const (
	AuthorityWorkflow Authority = "workflow"
	AuthorityOperator Authority = "operator"
)

// Lifecycle is the independent durable admission barrier for a project/tree. Its epoch binds claims
// to one open lifetime. Cleanup reserves that epoch until API-confirmed release; only an explicit
// fresh authority opens the next epoch.
type Lifecycle struct {
	Project, Tree      string
	Epoch              uint64
	Authority          Authority
	CleanupStarted     bool
	CleanupConfirmedAt time.Time
}

var (
	// ErrCleanupReserved is a wait: whatever it refuses runs once the cleanup confirmed and a fresh
	// admission opened the tree's next epoch.
	ErrCleanupReserved = wait.New("tree cleanup is reserved")
	ErrLifecycleAbsent = errors.New("tree lifecycle is absent")
	ErrLifecycleClosed = errors.New("tree cleanup is confirmed")
)

// Serialize is the same global fact serializer intake.ApplyFact uses. Facts promote other trees
// under one transaction, so a per-tree lock would introduce a cross-tree lock order. Every non-fact
// lifecycle operation takes this lock too; pg_advisory_xact_lock is re-entrant in the fact
// transaction and is released with it.
func Serialize(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtext('legion.apply_fact'))"); err != nil {
		return fmt.Errorf("serialize tree lifecycle: %w", err)
	}
	return nil
}

func scan(row pgx.Row) (Lifecycle, error) {
	var lifecycle Lifecycle
	var epoch int64
	var authority string
	var confirmed *time.Time
	if err := row.Scan(&lifecycle.Project, &lifecycle.Tree, &epoch, &authority, &lifecycle.CleanupStarted, &confirmed); err != nil {
		return Lifecycle{}, err
	}
	if confirmed != nil {
		lifecycle.CleanupConfirmedAt = *confirmed
	}
	if epoch <= 0 {
		return Lifecycle{}, fmt.Errorf("tree lifecycle %s/%s has invalid epoch %d", lifecycle.Project, lifecycle.Tree, epoch)
	}
	lifecycle.Epoch, lifecycle.Authority = uint64(epoch), Authority(authority)
	return lifecycle, nil
}

func read(ctx context.Context, tx pgx.Tx, project, tree string) (Lifecycle, error) {
	return scan(tx.QueryRow(ctx, `select project, tree, epoch, authority, cleanup_started,
		cleanup_confirmed_at from tree_lifecycles where project = $1 and tree = $2 for update`, project, tree))
}

// Open starts a fresh authorized lifetime or returns its current open epoch. It is called in the
// admission fact transaction or before an explicit operator root is created; a cleanup reservation
// is a named wait, never an implicit zero authority.
func Open(ctx context.Context, tx pgx.Tx, project, tree string, authority Authority) (Lifecycle, error) {
	if authority != AuthorityWorkflow && authority != AuthorityOperator {
		return Lifecycle{}, fmt.Errorf("open tree lifecycle %s: invalid authority %q", tree, authority)
	}
	if err := Serialize(ctx, tx); err != nil {
		return Lifecycle{}, err
	}
	lifecycle, err := read(ctx, tx, project, tree)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err := tx.Exec(ctx, `insert into tree_lifecycles (project, tree, epoch, authority) values ($1, $2, 1, $3)`, project, tree, string(authority))
		if err != nil {
			return Lifecycle{}, fmt.Errorf("create tree lifecycle %s: %w", tree, err)
		}
		return Lifecycle{Project: project, Tree: tree, Epoch: 1, Authority: authority}, nil
	}
	if err != nil {
		return Lifecycle{}, fmt.Errorf("read tree lifecycle %s: %w", tree, err)
	}
	if lifecycle.CleanupStarted && lifecycle.CleanupConfirmedAt.IsZero() {
		return Lifecycle{}, fmt.Errorf("open tree lifecycle %s: %w", tree, ErrCleanupReserved)
	}
	if lifecycle.CleanupStarted {
		if lifecycle.Epoch == math.MaxInt64 {
			return Lifecycle{}, fmt.Errorf("open tree lifecycle %s: epoch overflows", tree)
		}
		lifecycle.Epoch++
		lifecycle.Authority, lifecycle.CleanupStarted, lifecycle.CleanupConfirmedAt = authority, false, time.Time{}
		_, err = tx.Exec(ctx, `update tree_lifecycles set epoch = $3, authority = $4, cleanup_started = false,
			cleanup_confirmed_at = null, updated_at = now() where project = $1 and tree = $2`, project, tree, int64(lifecycle.Epoch), string(authority))
		if err != nil {
			return Lifecycle{}, fmt.Errorf("open next tree lifecycle %s: %w", tree, err)
		}
		return lifecycle, nil
	}
	if lifecycle.Authority != authority {
		return Lifecycle{}, fmt.Errorf("open tree lifecycle %s as %s: current authority is %s", tree, authority, lifecycle.Authority)
	}
	return lifecycle, nil
}

// BindWork binds a claim before its first durable write. Authority was established by the opener;
// this shared path rejects absent, reserved and confirmed lifecycles before a runnable row commits.
func BindWork(ctx context.Context, tx pgx.Tx, project, tree string) (uint64, error) {
	if err := Serialize(ctx, tx); err != nil {
		return 0, err
	}
	lifecycle, err := read(ctx, tx, project, tree)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("bind work of tree %s: %w", tree, ErrLifecycleAbsent)
	}
	if err != nil {
		return 0, fmt.Errorf("bind work of tree %s: %w", tree, err)
	}
	if lifecycle.CleanupStarted {
		if lifecycle.CleanupConfirmedAt.IsZero() {
			return 0, fmt.Errorf("bind work of tree %s: %w", tree, ErrCleanupReserved)
		}
		return 0, fmt.Errorf("bind work of tree %s: %w", tree, ErrLifecycleClosed)
	}
	return lifecycle.Epoch, nil
}

// CheckWork confirms a bound claim may start or resume. It is deliberately a short transaction
// separate from runtime work: a cleanup reservation makes the outbox retry the start without
// consuming a launch-failure budget.
func CheckWork(ctx context.Context, tx pgx.Tx, project, tree string, epoch uint64) error {
	if epoch == 0 {
		return fmt.Errorf("check work of tree %s: unbound lifecycle epoch", tree)
	}
	bound, err := BindWork(ctx, tx, project, tree)
	if err != nil {
		return err
	}
	if bound != epoch {
		return fmt.Errorf("check work of tree %s: bound epoch %d is stale; current is %d", tree, epoch, bound)
	}
	return nil
}

// ReserveCleanup is the irreversible durable reservation. A retry returns the existing
// unconfirmed reservation; a confirmed lifecycle cannot be selected by a delayed close.
func ReserveCleanup(ctx context.Context, tx pgx.Tx, project, tree string, authority Authority) (Lifecycle, error) {
	if err := Serialize(ctx, tx); err != nil {
		return Lifecycle{}, err
	}
	lifecycle, err := read(ctx, tx, project, tree)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lifecycle{}, fmt.Errorf("reserve tree cleanup %s: %w", tree, ErrLifecycleAbsent)
	}
	if err != nil {
		return Lifecycle{}, fmt.Errorf("reserve tree cleanup %s: %w", tree, err)
	}
	if lifecycle.Authority != authority {
		return Lifecycle{}, fmt.Errorf("reserve tree cleanup %s as %s: current authority is %s", tree, authority, lifecycle.Authority)
	}
	if lifecycle.CleanupStarted {
		if lifecycle.CleanupConfirmedAt.IsZero() {
			return lifecycle, nil
		}
		return Lifecycle{}, fmt.Errorf("reserve tree cleanup %s: %w", tree, ErrLifecycleClosed)
	}
	_, err = tx.Exec(ctx, `update tree_lifecycles set cleanup_started = true, updated_at = now() where project = $1 and tree = $2`, project, tree)
	if err != nil {
		return Lifecycle{}, fmt.Errorf("reserve tree cleanup %s: %w", tree, err)
	}
	lifecycle.CleanupStarted = true
	return lifecycle, nil
}

// CheckReservation proves the physical cleanup caller holds the current reservation epoch. It takes
// the shared serializer before the lifecycle row, as every lifecycle operation does. The authority
// was checked when the reservation was taken through its own entry point; the epoch names it.
func CheckReservation(ctx context.Context, tx pgx.Tx, project, tree string, epoch uint64) error {
	if epoch == 0 {
		return fmt.Errorf("check cleanup reservation of tree %s: unbound lifecycle epoch", tree)
	}
	if err := Serialize(ctx, tx); err != nil {
		return err
	}
	lifecycle, err := read(ctx, tx, project, tree)
	if err != nil {
		return fmt.Errorf("check cleanup reservation of tree %s: %w", tree, err)
	}
	if lifecycle.Epoch != epoch || !lifecycle.CleanupStarted || !lifecycle.CleanupConfirmedAt.IsZero() {
		return fmt.Errorf("check cleanup reservation of tree %s: reservation is not current", tree)
	}
	return nil
}

// ConfirmCleanup records complete API-confirmed release. The next explicit root admission, not a
// delayed claim or old outbox row, opens the following epoch.
func ConfirmCleanup(ctx context.Context, tx pgx.Tx, project, tree string, epoch uint64) error {
	if err := Serialize(ctx, tx); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `update tree_lifecycles set cleanup_confirmed_at = now(), updated_at = now()
		where project = $1 and tree = $2 and epoch = $3 and cleanup_started = true and cleanup_confirmed_at is null`, project, tree, int64(epoch))
	if err != nil {
		return fmt.Errorf("confirm tree cleanup %s: %w", tree, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("confirm tree cleanup %s: reservation is not current", tree)
	}
	return nil
}
