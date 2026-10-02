package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"math"
	"time"

	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// IssueResources is the durable ownership record for one issue's shared Sandbox resources. A
// claim only owns its role process; the issue owns this record, its Sandbox, its six launcher
// Secrets and, when it is a root, the tree PVC. Cleanup is a capability transition, not an
// inference from whichever claims happen to be live in one runtime. Generation is the record's own
// admission epoch: 1 at the issue's first launch, one more at each re-admission after a confirmed
// cleanup. It is neither a claim's launch generation nor the workflow's issue generation.
type IssueResources struct {
	Project, Issue, Tree, Sandbox string
	Generation                    uint64
	CleanupStarted                bool
	CleanupGeneration             uint64
	CleanupConfirmedAt            time.Time
}

const issuePodLayout = "issue-pod-v1"

const selectIssueResources = `select project, issue, tree, sandbox_name, generation, cleanup_started,
	cleanup_generation, cleanup_confirmed_at from issue_resources`

// ErrIssueCleanupInProgress refuses a launch while its issue's resources, or its tree root's, are
// being deleted. Resource cleanup only ever runs under its tree's lifecycle reservation, so this is
// that reservation's wait: it wraps treelifecycle.ErrCleanupReserved, which the supervision machine
// matches without importing the store. The issue is admitted again once a fresh admission opens
// the tree's next epoch.
var ErrIssueCleanupInProgress = fmt.Errorf("issue resources are being cleaned up: %w", treelifecycle.ErrCleanupReserved)

// ErrTreeChildrenPending refuses to begin a tree root's cleanup while a child issue of the tree
// holds resources whose deletion is not confirmed: the root Sandbox owns the tree PVC, and
// deleting it lets owner-reference garbage collection remove the volume a child still mounts.
var ErrTreeChildrenPending = errors.New("a child issue of the tree still holds resources")

// EnsureIssueResources records an issue's resources before any role process of it starts. A
// record whose cleanup was confirmed is re-admitted at the next epoch; one being cleaned up refuses
// the launch (ErrIssueCleanupInProgress). A child issue is admitted under its tree root's record,
// locked for share, so a root's BeginIssueCleanup, which locks that record for update, either sees
// the child's record or is seen by it: once the root's cleanup has begun, a new child admission (no
// record, or a confirmed one) is refused.
func (s *Store) EnsureIssueResources(ctx context.Context, project, issue, tree, sandbox string, treeEpoch uint64) error {
	switch {
	case project == "":
		return errors.New("issue resources have no project")
	case issue == "":
		return errors.New("issue resources have no issue")
	case tree == "":
		return fmt.Errorf("issue resources %s have no tree", issue)
	case sandbox == "":
		return fmt.Errorf("issue resources %s have no Sandbox name", issue)
	case treeEpoch == 0:
		return fmt.Errorf("issue resources %s have no tree lifecycle epoch", issue)
	}
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := treelifecycle.CheckWork(ctx, tx, project, tree, treeEpoch); err != nil {
			return err
		}
		rootCleaning := false
		if issue != tree {
			err := tx.QueryRow(ctx, `select cleanup_started from issue_resources where project = $1 and issue = $2 for share`,
				project, tree).Scan(&rootCleaning)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("its tree root %s has no issue resources", tree)
			}
			if err != nil {
				return fmt.Errorf("read its tree root %s: %w", tree, err)
			}
		}
		own, err := scanIssueResources(tx.QueryRow(ctx, selectIssueResources+` where project = $1 and issue = $2 for update`, project, issue))
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		confirmed := exists && own.CleanupStarted && !own.CleanupConfirmedAt.IsZero()
		switch {
		case exists && own.CleanupStarted && !confirmed:
			return ErrIssueCleanupInProgress
		case exists && !confirmed && (own.Tree != tree || own.Sandbox != sandbox):
			return fmt.Errorf("recorded as tree %s and Sandbox %s, not tree %s and Sandbox %s", own.Tree, own.Sandbox, tree, sandbox)
		case exists && !confirmed:
			return nil
		case rootCleaning:
			return fmt.Errorf("its tree root %s: %w", tree, ErrIssueCleanupInProgress)
		case exists:
			_, err = tx.Exec(ctx, `update issue_resources set tree = $3, sandbox_name = $4, generation = generation + 1,
				cleanup_started = false, cleanup_generation = null, cleanup_confirmed_at = null, updated_at = now()
				where project = $1 and issue = $2`, project, issue, tree, sandbox)
		default:
			_, err = tx.Exec(ctx, `insert into issue_resources (project, issue, tree, sandbox_name, generation)
				values ($1, $2, $3, $4, 1) on conflict (project, issue) do nothing`, project, issue, tree, sandbox)
		}
		return err
	})
	if err != nil {
		if errors.Is(err, treelifecycle.ErrCleanupReserved) {
			return fmt.Errorf("ensure issue resources %s: %w", issue, errors.Join(ErrIssueCleanupInProgress, err))
		}
		return fmt.Errorf("ensure issue resources %s: %w", issue, err)
	}
	return nil
}

// BeginIssueCleanup begins one issue's physical cleanup only under a prior explicit reservation of
// its tree lifecycle epoch, checked in the same transaction. Workflow and operator authority
// reserve through different validated entry points; zero is never authority. A retry of an
// unconfirmed issue cleanup returns it. Like every lifecycle operation it takes the shared
// serializer before any lifecycle or resource row, so it cannot lock in the opposite order from an
// admission.
func (s *Store) BeginIssueCleanup(ctx context.Context, project, issue, tree string, treeEpoch uint64) (IssueResources, bool, error) {
	if tree == "" || treeEpoch == 0 {
		return IssueResources{}, false, fmt.Errorf("begin issue cleanup %s: missing tree lifecycle epoch", issue)
	}
	var resources IssueResources
	began := false
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := treelifecycle.CheckReservation(ctx, tx, project, tree, treeEpoch); err != nil {
			return err
		}
		own, err := scanIssueResources(tx.QueryRow(ctx, selectIssueResources+` where project = $1 and issue = $2 for update`, project, issue))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if own.Tree != tree {
			return fmt.Errorf("recorded under tree %s, not closing tree %s", own.Tree, tree)
		}
		if own.CleanupStarted {
			if own.CleanupConfirmedAt.IsZero() {
				resources, began = own, true
			}
			return nil
		}
		var live bool
		if err := tx.QueryRow(ctx, `select exists (
			select 1 from claims where project = $1 and issue = $2 and state <> 'retired')`, project, issue).Scan(&live); err != nil {
			return fmt.Errorf("census its claims: %w", err)
		}
		if live {
			return nil
		}
		if own.Tree == own.Issue {
			var pending bool
			if err := tx.QueryRow(ctx, `select exists (
				select 1 from issue_resources where project = $1 and tree = $2 and issue <> $2 and cleanup_confirmed_at is null)`,
				project, own.Tree).Scan(&pending); err != nil {
				return fmt.Errorf("census its tree's child issues: %w", err)
			}
			if pending {
				return ErrTreeChildrenPending
			}
		}
		if _, err := tx.Exec(ctx, `update issue_resources set cleanup_started = true, cleanup_generation = generation,
			updated_at = now() where project = $1 and issue = $2`, project, issue); err != nil {
			return err
		}
		own.CleanupStarted, own.CleanupGeneration = true, own.Generation
		resources, began = own, true
		return nil
	})
	if err != nil {
		return IssueResources{}, false, fmt.Errorf("begin issue cleanup %s: %w", issue, err)
	}
	return resources, began, nil
}

// ConfirmIssueCleanup records that every resource of the issue's cleanup epoch is gone through its
// API: its Sandbox, and for a root also the tree PVC. Until then the record refuses re-admission.
func (s *Store) ConfirmIssueCleanup(ctx context.Context, project, issue string, generation uint64) error {
	if generation == 0 || generation > math.MaxInt64 {
		return fmt.Errorf("confirm issue cleanup %s: invalid generation %d", issue, generation)
	}
	tag, err := s.pool.Exec(ctx, `update issue_resources set cleanup_confirmed_at = now(), updated_at = now()
		where project = $1 and issue = $2 and generation = $3 and cleanup_started = true and cleanup_generation = $3`,
		project, issue, int64(generation))
	if err != nil {
		return fmt.Errorf("confirm issue cleanup %s: %w", issue, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("confirm issue cleanup %s: its generation is not cleaning", issue)
	}
	return nil
}

// IssueResources reads an issue's durable resource capability.
func (s *Store) IssueResources(ctx context.Context, project, issue string) (IssueResources, bool, error) {
	resources, err := scanIssueResources(s.pool.QueryRow(ctx, selectIssueResources+` where project = $1 and issue = $2`, project, issue))
	if errors.Is(err, pgx.ErrNoRows) {
		return IssueResources{}, false, nil
	}
	if err != nil {
		return IssueResources{}, false, fmt.Errorf("read issue resources %s: %w", issue, err)
	}
	return resources, true, nil
}

// ConfirmRootCleanup atomically confirms the root resource and its tree reservation after
// foreground Sandbox deletion is API-confirmed. Neither confirmation may admit a new epoch alone.
// The shared serializer comes first, before the resource row, as in every lifecycle operation.
func (s *Store) ConfirmRootCleanup(ctx context.Context, project, issue, tree string, resourceGeneration, treeEpoch uint64) error {
	if resourceGeneration == 0 || treeEpoch == 0 {
		return fmt.Errorf("confirm root cleanup %s: missing resource or tree epoch", issue)
	}
	return s.Tx(ctx, func(tx pgx.Tx) error {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `update issue_resources set cleanup_confirmed_at = now(), updated_at = now()
			where project = $1 and issue = $2 and generation = $3 and cleanup_started = true and cleanup_generation = $3`,
			project, issue, int64(resourceGeneration))
		if err != nil {
			return fmt.Errorf("confirm root cleanup %s: %w", issue, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("confirm root cleanup %s: its generation is not cleaning", issue)
		}
		return treelifecycle.ConfirmCleanup(ctx, tx, project, tree, treeEpoch)
	})
}

// TreeIssueResources lists a tree's durable resources in the only deletion order that preserves
// its root-owned volume: every child issue before the root. It is the explicit operator-close
// capability for a tree no workflow `issues` row backs.
func (s *Store) TreeIssueResources(ctx context.Context, project, tree string) ([]IssueResources, error) {
	rows, err := s.pool.Query(ctx, selectIssueResources+` where project = $1 and tree = $2
		order by issue = $2, issue`, project, tree)
	if err != nil {
		return nil, fmt.Errorf("list issue resources of tree %s: %w", tree, err)
	}
	defer rows.Close()
	var resources []IssueResources
	for rows.Next() {
		resource, err := scanIssueResources(rows)
		if err != nil {
			return nil, fmt.Errorf("read issue resources of tree %s: %w", tree, err)
		}
		resources = append(resources, resource)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list issue resources of tree %s: %w", tree, err)
	}
	return resources, nil
}

// EnsureIssuePodLayout atomically installs the issue-pod marker once boot's legacy censuses (the
// claims before migration, the cluster's Sandboxes before the store opened) found no old layout. A
// different marker is a refusal: no runtime may guess which pod layout an old claim or Sandbox
// belongs to.
func (s *Store) EnsureIssuePodLayout(ctx context.Context, project string) error {
	var layout string
	err := s.pool.QueryRow(ctx, `select layout from runtime_layouts where project = $1`, project).Scan(&layout)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = s.pool.Exec(ctx, `insert into runtime_layouts (project, layout) values ($1, $2)`, project, issuePodLayout)
		if err != nil {
			return fmt.Errorf("record issue-pod layout for %s: %w", project, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("read runtime layout for %s: %w", project, err)
	case layout != issuePodLayout:
		return fmt.Errorf("project %s has runtime layout %q, want %q", project, layout, issuePodLayout)
	default:
		return nil
	}
}

// HasLegacySandboxClaims is a raw JSON census used before Claims unmarshals locators. A legacy
// per-claim Sandbox locator lacks podUid/container/generation, so the normal strict locator
// decoder would refuse first without naming the layout migration that is needed.
func (s *Store) HasLegacySandboxClaims(ctx context.Context, project string) (bool, error) {
	var legacy bool
	err := s.pool.QueryRow(ctx, `select exists (
		select 1 from claims where project = $1 and locator->>'runtime' = 'sandbox'
		and (locator->'sandbox'->>'podUid' is null or locator->'sandbox'->>'container' is null
			or locator->'sandbox'->>'generation' is null)
	)`, project).Scan(&legacy)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			// A brand-new database has no claims table until the first migration, and therefore
			// cannot contain a legacy locator. This preserves the no-schema-write-before-census
			// ordering for existing databases.
			return false, nil
		}
		return false, fmt.Errorf("census legacy Sandbox claims for %s: %w", project, err)
	}
	return legacy, nil
}

// UndecodableTreeCloses lists the outbox tree closes of the Dispatch project dispatchProject (the
// issue-key prefix the outbox leases by, record.Postgres.ClaimDue) whose linger, the root
// generation they expire, is not a whole number of at least 1. The outbox decodes such a row
// strictly and refuses it on every attempt (record.validateOutboxPayload). Migration 0016 gave the
// older shape, a close naming no linger, its root's generation or deleted it, and every writer
// since sets it, so after migration this census is empty unless a row was written outside the
// daemon.
func (s *Store) UndecodableTreeCloses(ctx context.Context, dispatchProject string) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `select id from outbox where kind = 'supervise' and payload->>'op' = 'tree_close'
		and split_part(issue, '-', 1) = $1
		and not coalesce(jsonb_typeof(payload->'linger') = 'number' and payload->>'linger' ~ '^[1-9][0-9]*$', false)
		order by id`, dispatchProject)
	if err != nil {
		return nil, fmt.Errorf("census undecodable tree closes: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("census undecodable tree closes: %w", err)
	}
	return ids, nil
}

type issueResourcesRow interface{ Scan(...any) error }

func scanIssueResources(row issueResourcesRow) (IssueResources, error) {
	var resources IssueResources
	var generation int64
	var cleanupGeneration *int64
	var confirmed *time.Time
	err := row.Scan(&resources.Project, &resources.Issue, &resources.Tree, &resources.Sandbox, &generation,
		&resources.CleanupStarted, &cleanupGeneration, &confirmed)
	if err != nil {
		return IssueResources{}, err
	}
	resources.Generation = uint64(generation)
	if cleanupGeneration != nil {
		resources.CleanupGeneration = uint64(*cleanupGeneration)
	}
	if confirmed != nil {
		resources.CleanupConfirmedAt = *confirmed
	}
	return resources, nil
}
