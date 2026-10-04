package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

const rootIssue, childIssue = "LEGION-208", "LEGION-209"

// seedLingeringRoot is the tree-close state a workflow cleanup reservation binds to: the root's
// issue row lingering at generation after its close.
func seedLingeringRoot(t *testing.T, store *Store, generation int) {
	t.Helper()
	_, err := store.Pool().Exec(context.Background(), `insert into issues
		(key, tree, project, title, phase, generation, status, rank, linger_until, last_dispatch_seq)
		values ($1, $1, 'LEGION', 'root', 'done', $2, 'done', 'A', now() + interval '1 hour', 1)
		on conflict (key) do update set generation = excluded.generation, status = excluded.status,
			phase = excluded.phase, linger_until = excluded.linger_until`, rootIssue, generation)
	if err != nil {
		t.Fatalf("seed lingering root: %v", err)
	}
}

// openLingeringTree is a workflow tree admitted at epoch one whose root now lingers at generation
// one after its close.
func openLingeringTree(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.OpenTreeLifecycle(context.Background(), "legion", rootIssue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open lifecycle of %s: %v", rootIssue, err)
	}
	seedLingeringRoot(t, store, 1)
}

// countingReleaser is a runtime that holds nothing per tree and counts the releases it was asked
// for.
type countingReleaser struct{ calls int }

func (r *countingReleaser) CleanupTree(context.Context, string) error {
	r.calls++
	return nil
}

// A close reserves the tree barrier before any process of the tree exists. The delayed root claim
// cannot persist; confirming the empty cleanup lets only a fresh explicit workflow admission open
// epoch two and write it.
func TestCloseFencesADelayedRootClaimUntilAFreshAdmission(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	openLingeringTree(t, store)
	lifecycle, reserved, err := store.ReserveWorkflowTreeCleanup(ctx, "legion", rootIssue, 1)
	if err != nil || !reserved || lifecycle.Epoch != 1 {
		t.Fatalf("reserve before any claim = %+v, reserved %t, err %v", lifecycle, reserved, err)
	}
	root := supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion", Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, State: supervise.StateQueued}
	if _, err := store.AdmitClaim(ctx, root); !errors.Is(err, treelifecycle.ErrCleanupReserved) || !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("delayed root claim after reservation = %v, want the reservation's wait", err)
	}
	if err := store.CleanupReservedTree(ctx, "legion", rootIssue, lifecycle.Epoch, &countingReleaser{}); err != nil {
		t.Fatal(err)
	}
	opened, err := store.OpenTreeLifecycle(ctx, "legion", rootIssue, treelifecycle.AuthorityWorkflow)
	if err != nil || opened.Epoch != 2 {
		t.Fatalf("fresh workflow admission = %+v, err %v, want epoch 2", opened, err)
	}
	bound, err := store.AdmitClaim(ctx, root)
	if err != nil || bound.TreeEpoch != 2 {
		t.Fatalf("root claim after confirmed cleanup = %+v, err %v", bound, err)
	}
}

// A workflow close that names a generation its root never lingered at, or none, reserves nothing
// and is no error: the outbox's ordinary fences finish its row, so no boot census has to refuse
// it, and the tree stays open for its claims.
func TestACloseOfAGenerationTheRootNeverLingeredAtReservesNothing(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	openLingeringTree(t, store)
	for _, generation := range []uint64{0, 2, 1 << 63} {
		if lifecycle, reserved, err := store.ReserveWorkflowTreeCleanup(ctx, "legion", rootIssue, generation); err != nil || reserved {
			t.Fatalf("reserve at generation %d = %+v, reserved %t, err %v; want nothing reserved and no error", generation, lifecycle, reserved, err)
		}
	}
	c := supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion", Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, State: supervise.StateQueued}
	if bound, err := store.AdmitClaim(ctx, c); err != nil || bound.TreeEpoch != 1 {
		t.Fatalf("claim after the unrunnable closes = %+v, err %v; want the open epoch 1", bound, err)
	}
}

// A workflow close cannot select the explicit operator authority by passing a malformed lifecycle
// input. The authority is stored when the authenticated root producer opened the tree, not inferred
// from a zero/sentinel generation at cleanup.
func TestMalformedWorkflowCloseCannotSelectOperatorAuthority(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	if _, err := store.OpenTreeLifecycle(ctx, "legion", rootIssue, treelifecycle.AuthorityOperator); err != nil {
		t.Fatal(err)
	}
	seedLingeringRoot(t, store, 1)
	if _, reserved, err := store.ReserveWorkflowTreeCleanup(ctx, "legion", rootIssue, 1); err == nil || reserved || !strings.Contains(err.Error(), "authority") {
		t.Fatalf("workflow reserve against operator tree: reserved %t, err %v", reserved, err)
	}
	// A zero generation is not an authority tag: the workflow entry point reserves nothing.
	if _, reserved, err := store.ReserveWorkflowTreeCleanup(ctx, "legion", rootIssue, 0); err != nil || reserved {
		t.Fatalf("workflow reserve with generation 0: reserved %t, err %v", reserved, err)
	}
	// Nor can a physical cleanup run without an explicit reserved epoch.
	releaser := &countingReleaser{}
	if err := store.CleanupReservedTree(ctx, "legion", rootIssue, 0, releaser); err == nil || releaser.calls != 0 {
		t.Fatalf("cleanup with epoch 0 = %v after %d releases; want a refusal before any release", err, releaser.calls)
	}
	if lifecycle, err := store.OpenTreeLifecycle(ctx, "legion", rootIssue, treelifecycle.AuthorityWorkflow); err == nil {
		t.Fatalf("workflow open of an operator tree = %+v, want an authority refusal", lifecycle)
	}
}

// A reservation whose transaction commits while a start waits on the shared serializer wins: the
// start is refused with the named cleanup wait once the reservation commits, and writes nothing.
func TestAReservationThatCommitsFirstRefusesAWaitingStart(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	if _, err := store.OpenTreeLifecycle(ctx, "legion", rootIssue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	tx, err := store.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := treelifecycle.ReserveCleanup(ctx, tx, "legion", rootIssue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	child := supervise.Claim{Token: claim.Token("legion-legion-legion-209-implementer"), Project: "legion", Tree: rootIssue, Issue: childIssue, Role: claim.RoleImplementer, State: supervise.StateQueued}
	admitted := make(chan error, 1)
	go func() {
		_, err := store.AdmitClaim(ctx, child)
		admitted <- err
	}()
	select {
	case err := <-admitted:
		t.Fatalf("the start did not wait for the reservation's serializer: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-admitted; !errors.Is(err, treelifecycle.ErrCleanupReserved) || !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("start after the reservation committed = %v, want the reservation's wait", err)
	}
	var count int
	if err := store.Pool().QueryRow(ctx, `select count(*) from claims where token = $1`, string(child.Token)).Scan(&count); err != nil || count != 0 {
		t.Fatalf("the refused start left %d claim rows (err %v)", count, err)
	}
}

// A restart reads the same unconfirmed reservation from Postgres. It still rejects delayed claim
// persistence, and its retry returns the original epoch rather than losing the cleanup marker in a
// runtime-local cache.
func TestRestartRetainsTreeCleanupReservation(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	openLingeringTree(t, store)
	lifecycle, reserved, err := store.ReserveWorkflowTreeCleanup(ctx, "legion", rootIssue, 1)
	if err != nil || !reserved {
		t.Fatalf("reserve = %+v, reserved %t, err %v", lifecycle, reserved, err)
	}
	restarted, err := Open(ctx, store.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	claim := supervise.Claim{Token: claim.Token("legion-legion-legion-209-implementer"), Project: "legion", Tree: rootIssue, Issue: childIssue, Role: claim.RoleImplementer, State: supervise.StateQueued}
	if _, err := restarted.AdmitClaim(ctx, claim); !errors.Is(err, treelifecycle.ErrCleanupReserved) {
		t.Fatalf("delayed child claim after restart = %v, want the reservation's wait", err)
	}
	retry, resumed, err := restarted.ReserveWorkflowTreeCleanup(ctx, "legion", rootIssue, 1)
	if err != nil || !resumed || retry.Epoch != lifecycle.Epoch {
		t.Fatalf("reservation retry after restart = %+v, resumed %t, err %v", retry, resumed, err)
	}
}

// The reserved cleanup waits, releasing nothing, while any stored claim of the tree has not
// retired; once every claim retired it releases the tree and confirms. A retained claim token is
// then no authority to launch a later tree lifetime: re-admission writes the same token only after
// that confirmation, and its persisted TreeEpoch changes with the newly opened lifecycle.
func TestReservedCleanupWaitsForEveryClaimAndTheNextEpochRebindsIt(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	openLingeringTree(t, store)
	c := supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion", Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, State: supervise.StateQueued}
	first, err := store.AdmitClaim(ctx, c)
	if err != nil || first.TreeEpoch != 1 {
		t.Fatalf("first claim = %+v, err %v", first, err)
	}
	lifecycle, reserved, err := store.ReserveWorkflowTreeCleanup(ctx, "legion", rootIssue, 1)
	if err != nil || !reserved {
		t.Fatalf("reserve = reserved %t, err %v", reserved, err)
	}
	releaser := &countingReleaser{}
	err = store.CleanupReservedTree(ctx, "legion", rootIssue, lifecycle.Epoch, releaser)
	if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), string(c.Token)+":queued") || releaser.calls != 0 {
		t.Fatalf("cleanup with a queued claim = %v after %d releases; want a wait naming it and no release", err, releaser.calls)
	}
	first.State = supervise.StateRetired
	if err := store.PutClaim(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupReservedTree(ctx, "legion", rootIssue, lifecycle.Epoch, releaser); err != nil || releaser.calls != 1 {
		t.Fatalf("cleanup once every claim retired = %v after %d releases; want one release and a confirmation", err, releaser.calls)
	}
	if err := store.CleanupReservedTree(ctx, "legion", rootIssue, lifecycle.Epoch, releaser); err == nil || releaser.calls != 1 {
		t.Fatalf("cleanup of a confirmed reservation = %v after %d releases; want a refusal and no second release", err, releaser.calls)
	}
	if _, err := store.OpenTreeLifecycle(ctx, "legion", rootIssue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	second, err := store.AdmitClaim(ctx, c)
	if err != nil || second.TreeEpoch != 2 {
		t.Fatalf("reactivated claim = %+v, err %v", second, err)
	}
}

// Every lifecycle operation takes the shared apply_fact serializer before any lifecycle row. A
// fact transaction holds that serializer and then locks those rows (admission binding work), so a
// cleanup step that locked a row first and then waited for the serializer would deadlock against
// it and Postgres would abort one of the two.
func TestCleanupStepsTakeTheSerializerBeforeTheirRows(t *testing.T) {
	const lifecycleRow = `select 1 from tree_lifecycles where project = 'legion' and tree = $1 for update`
	t.Run("reservation", func(t *testing.T) {
		store := migratedStore(t)
		ctx := context.Background()
		openLingeringTree(t, store)
		holdSerializerThenLock(t, store, lifecycleRow, func() error {
			_, reserved, err := store.ReserveWorkflowTreeCleanup(ctx, "legion", rootIssue, 1)
			if err == nil && !reserved {
				return errors.New("reserved nothing")
			}
			return err
		})
	})
	t.Run("cleanup", func(t *testing.T) {
		store := migratedStore(t)
		ctx := context.Background()
		openLingeringTree(t, store)
		lifecycle, reserved, err := store.ReserveWorkflowTreeCleanup(ctx, "legion", rootIssue, 1)
		if err != nil || !reserved {
			t.Fatalf("reserve = reserved %t, err %v", reserved, err)
		}
		holdSerializerThenLock(t, store, lifecycleRow, func() error {
			return store.CleanupReservedTree(ctx, "legion", rootIssue, lifecycle.Epoch, &countingReleaser{})
		})
	})
}

// holdSerializerThenLock is the fact side of the lock order: it takes the serializer, waits until
// step is blocked behind it, then locks the row lock names for rootIssue, and only then commits.
// The row lock must not wait on step, and step must succeed once the fact commits.
func holdSerializerThenLock(t *testing.T, store *Store, lock string, step func() error) {
	t.Helper()
	ctx := context.Background()
	fact, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fact.Rollback(ctx) }()
	if err := treelifecycle.Serialize(ctx, fact); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- step() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := store.Pool().QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and wait_event = 'advisory'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cleanup step never waited for the serializer (it returned %v)", <-done)
		}
		time.Sleep(20 * time.Millisecond)
	}
	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := fact.Exec(lockCtx, lock, rootIssue); err != nil {
		t.Fatalf("the fact transaction could not lock the row behind the waiting cleanup step: %v", err)
	}
	if err := fact.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("cleanup step after the fact committed: %v", err)
	}
}
