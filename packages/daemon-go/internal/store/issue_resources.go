package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// IssueResources is the durable ownership record for one issue's shared Sandbox resources. A
// claim only owns its role process; the issue owns this record, its Sandbox, its six launcher
// Secrets and, when it is a root, the tree PVC. Cleanup is a capability transition, not an
// inference from whichever claims happen to be live in one runtime.
type IssueResources struct {
	Project, Issue, Tree, Sandbox string
	Generation                    uint64
	CleanupStarted                bool
	CleanupGeneration             uint64
	CleanupConfirmedAt            time.Time
}

const issuePodLayout = "issue-pod-v1"

// EnsureIssueResources records an issue's resources before its first role process starts. A newer
// issue generation re-admits the record only after its previous cleanup was fenced and started;
// a stale caller cannot clear a cleanup that belongs to a later generation.
func (s *Store) EnsureIssueResources(ctx context.Context, resources IssueResources) error {
	if err := validResources(resources); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `insert into issue_resources (project, issue, tree, sandbox_name, generation)
		values ($1, $2, $3, $4, $5)
		on conflict (project, issue) do update set tree = excluded.tree, sandbox_name = excluded.sandbox_name,
		generation = excluded.generation, cleanup_started = false, cleanup_generation = null,
		cleanup_confirmed_at = null, updated_at = now()
		where issue_resources.generation < excluded.generation`,
		resources.Project, resources.Issue, resources.Tree, resources.Sandbox, int64(resources.Generation))
	if err != nil {
		return fmt.Errorf("ensure issue resources %s: %w", resources.Issue, err)
	}
	return nil
}

// BeginIssueCleanup claims cleanup exactly once for the closing generation. The caller already
// proved every stored sibling claim of this issue is retired in the same cleanup effect; a start
// at another generation makes this a no-op rather than deleting re-admitted resources.
func (s *Store) BeginIssueCleanup(ctx context.Context, project, issue string, generation uint64) (IssueResources, bool, error) {
	if generation == 0 || generation > math.MaxInt64 {
		return IssueResources{}, false, fmt.Errorf("begin issue cleanup %s: invalid generation %d", issue, generation)
	}
	row := s.pool.QueryRow(ctx, `update issue_resources set cleanup_started = true, cleanup_generation = $3,
		updated_at = now() where project = $1 and issue = $2 and generation = $3 and cleanup_started = false
		returning project, issue, tree, sandbox_name, generation, cleanup_started, cleanup_generation, cleanup_confirmed_at`, project, issue, int64(generation))
	resources, err := scanIssueResources(row)
	if err == nil {
		return resources, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return IssueResources{}, false, fmt.Errorf("begin issue cleanup %s: %w", issue, err)
	}
	// A previous attempt began the durable effect but failed before confirmation. The outbox row
	// retries with the same close generation; it must resume this cleanup rather than silently
	// leave cleanup_started true forever. Rows of another generation or a confirmed cleanup remain
	// no-ops.
	resources, err = scanIssueResources(s.pool.QueryRow(ctx, `select project, issue, tree, sandbox_name,
		generation, cleanup_started, cleanup_generation, cleanup_confirmed_at from issue_resources
		where project = $1 and issue = $2 and generation = $3 and cleanup_started = true
			and cleanup_generation = $3 and cleanup_confirmed_at is null`, project, issue, int64(generation)))
	if errors.Is(err, pgx.ErrNoRows) {
		return IssueResources{}, false, nil
	}
	if err != nil {
		return IssueResources{}, false, fmt.Errorf("resume issue cleanup %s: %w", issue, err)
	}
	return resources, true, nil
}

// ConfirmIssueCleanup records the API-confirmed Sandbox deletion. The root PVC deletion effect may
// run only after this transition, fenced to the same generation.
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
	resources, err := scanIssueResources(s.pool.QueryRow(ctx, `select project, issue, tree, sandbox_name,
		generation, cleanup_started, cleanup_generation, cleanup_confirmed_at from issue_resources where project = $1 and issue = $2`, project, issue))
	if errors.Is(err, pgx.ErrNoRows) {
		return IssueResources{}, false, nil
	}
	if err != nil {
		return IssueResources{}, false, fmt.Errorf("read issue resources %s: %w", issue, err)
	}
	return resources, true, nil
}

// EnsureIssuePodLayout atomically installs the issue-pod marker after a caller's legacy-object
// census succeeded. A different marker is a pre-migration refusal: no runtime may guess which pod
// layout an old claim or Sandbox belongs to.
func (s *Store) EnsureIssuePodLayout(ctx context.Context, project string, legacyClaims bool) error {
	if legacyClaims {
		return fmt.Errorf("project %s still has legacy per-claim Sandbox locators; migrate or remove them before enabling issue pods", project)
	}
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
		return false, fmt.Errorf("census legacy Sandbox claims for %s: %w", project, err)
	}
	return legacy, nil
}

func validResources(resources IssueResources) error {
	switch {
	case resources.Project == "":
		return errors.New("issue resources have no project")
	case resources.Issue == "":
		return errors.New("issue resources have no issue")
	case resources.Tree == "":
		return fmt.Errorf("issue resources %s have no tree", resources.Issue)
	case resources.Sandbox == "":
		return fmt.Errorf("issue resources %s have no Sandbox name", resources.Issue)
	case resources.Generation == 0 || resources.Generation > math.MaxInt64:
		return fmt.Errorf("issue resources %s have invalid generation %d", resources.Issue, resources.Generation)
	}
	return nil
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
