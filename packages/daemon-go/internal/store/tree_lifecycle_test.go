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
)

// Before the first root Sandbox/resource row exists, a close still reserves the independent tree
// barrier. The delayed root claim cannot persist; confirming the empty cleanup lets only a fresh
// explicit workflow admission open epoch two and write it.
func TestCloseBeforeFirstRootResourceFencesDelayedRootClaim(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	if _, err := store.OpenTreeLifecycle(ctx, "legion", rootIssue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	seedLingeringRoot(t, store, 1)
	lifecycle, reserved, err := store.ReserveWorkflowTreeCleanup(ctx, "legion", rootIssue, 1)
	if err != nil || !reserved || lifecycle.Epoch != 1 {
		t.Fatalf("reserve before first resource = %+v, reserved %t, err %v", lifecycle, reserved, err)
	}
	root := supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion", Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, State: supervise.StateQueued}
	if _, err := store.AdmitClaim(ctx, root); !errors.Is(err, ErrIssueCleanupInProgress) {
		t.Fatalf("delayed root claim after reservation = %v, want ErrIssueCleanupInProgress", err)
	}
	if err := store.ConfirmTreeCleanup(ctx, "legion", rootIssue, lifecycle.Epoch); err != nil {
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
	// A zero generation is not an authority tag: the workflow entry point refuses it outright.
	if _, reserved, err := store.ReserveWorkflowTreeCleanup(ctx, "legion", rootIssue, 0); err == nil || reserved {
		t.Fatalf("workflow reserve with generation 0: reserved %t, err %v", reserved, err)
	}
	// Nor can a physical cleanup run without an explicit reserved epoch.
	if _, began, err := store.BeginIssueCleanup(ctx, "legion", rootIssue, rootIssue, 0); err == nil || began {
		t.Fatalf("issue cleanup with epoch 0: began %t, err %v", began, err)
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
	if err := <-admitted; !errors.Is(err, ErrIssueCleanupInProgress) {
		t.Fatalf("start after the reservation committed = %v, want ErrIssueCleanupInProgress", err)
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
	if _, err := store.OpenTreeLifecycle(ctx, "legion", rootIssue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	seedLingeringRoot(t, store, 1)
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
	if _, err := restarted.AdmitClaim(ctx, claim); !errors.Is(err, ErrIssueCleanupInProgress) {
		t.Fatalf("delayed child claim after restart = %v, want ErrIssueCleanupInProgress", err)
	}
	retry, resumed, err := restarted.ReserveWorkflowTreeCleanup(ctx, "legion", rootIssue, 1)
	if err != nil || !resumed || retry.Epoch != lifecycle.Epoch {
		t.Fatalf("reservation retry after restart = %+v, resumed %t, err %v", retry, resumed, err)
	}
}

// A retained claim token is not authority to launch a later tree lifetime. Re-admission writes the
// same token only after explicit cleanup confirmation, and its persisted TreeEpoch changes with
// the newly opened lifecycle.
func TestReactivatedClaimBindsOnlyTheNextOpenedEpoch(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	if _, err := store.OpenTreeLifecycle(ctx, "legion", rootIssue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	seedLingeringRoot(t, store, 1)
	c := supervise.Claim{Token: claim.Token("legion-legion-legion-208-architect"), Project: "legion", Tree: rootIssue, Issue: rootIssue, Role: claim.RoleArchitect, State: supervise.StateQueued}
	first, err := store.AdmitClaim(ctx, c)
	if err != nil || first.TreeEpoch != 1 {
		t.Fatalf("first claim = %+v, err %v", first, err)
	}
	if _, reserved, err := store.ReserveWorkflowTreeCleanup(ctx, "legion", rootIssue, 1); err != nil || !reserved {
		t.Fatalf("reserve = reserved %t, err %v", reserved, err)
	}
	if err := store.ConfirmTreeCleanup(ctx, "legion", rootIssue, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenTreeLifecycle(ctx, "legion", rootIssue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	second, err := store.AdmitClaim(ctx, c)
	if err != nil || second.TreeEpoch != 2 {
		t.Fatalf("reactivated claim = %+v, err %v", second, err)
	}
}
