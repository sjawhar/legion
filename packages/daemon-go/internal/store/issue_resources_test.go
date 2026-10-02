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
)

const (
	rootIssue, childIssue = "LEGION-208", "LEGION-209"
	rootSandbox           = "legion-legion-legion-208"
	childSandbox          = "legion-legion-legion-209"
)

func ensure(t *testing.T, store *Store, issue, sandbox string) {
	t.Helper()
	if err := store.EnsureIssueResources(context.Background(), "legion", issue, rootIssue, sandbox); err != nil {
		t.Fatalf("admit %s: %v", issue, err)
	}
}

func resourcesOf(t *testing.T, store *Store, issue string) IssueResources {
	t.Helper()
	got, found, err := store.IssueResources(context.Background(), "legion", issue)
	if err != nil || !found {
		t.Fatalf("resources of %s: found %t, err %v", issue, found, err)
	}
	return got
}

// An issue's resources are cleaned once per admission epoch: a retried cleanup resumes the one it
// began, a launch while it runs is refused, and only a confirmed cleanup re-admits the issue.
func TestIssueResourcesReadmitOnlyAfterAConfirmedCleanup(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	ensure(t, store, rootIssue, rootSandbox)
	if got := resourcesOf(t, store, rootIssue); got.Generation != 1 || got.CleanupStarted {
		t.Fatalf("first admission = %+v", got)
	}
	begun, began, err := store.BeginIssueCleanup(ctx, "legion", rootIssue)
	if err != nil || !began || begun.CleanupGeneration != 1 {
		t.Fatalf("first cleanup = %+v, began %t, err %v", begun, began, err)
	}
	if retry, began, err := store.BeginIssueCleanup(ctx, "legion", rootIssue); err != nil || !began || retry.CleanupGeneration != 1 {
		t.Fatalf("cleanup retry = %+v, began %t, err %v", retry, began, err)
	}
	if err := store.EnsureIssueResources(ctx, "legion", rootIssue, rootIssue, rootSandbox); !errors.Is(err, ErrIssueCleanupInProgress) {
		t.Fatalf("admission during cleanup = %v, want ErrIssueCleanupInProgress", err)
	}
	if err := store.ConfirmIssueCleanup(ctx, "legion", rootIssue, 1); err != nil {
		t.Fatal(err)
	}
	if _, began, err := store.BeginIssueCleanup(ctx, "legion", rootIssue); err != nil || began {
		t.Fatalf("cleanup after confirmation: began %t, err %v", began, err)
	}
	ensure(t, store, rootIssue, rootSandbox)
	if got := resourcesOf(t, store, rootIssue); got.Generation != 2 || got.CleanupStarted || !got.CleanupConfirmedAt.IsZero() {
		t.Fatalf("re-admitted resources = %+v", got)
	}
	if err := store.ConfirmIssueCleanup(ctx, "legion", rootIssue, 1); err == nil {
		t.Fatal("a stale epoch's confirmation was accepted for the re-admitted issue")
	}
}

// A claim of the issue that is anything but retired means the issue was re-admitted (its launch
// persisted the claim before it recorded resources): cleanup begins nothing until it retires.
func TestCleanupBeginsNothingWhileAClaimOfTheIssueIsLive(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	ensure(t, store, rootIssue, rootSandbox)
	ensure(t, store, childIssue, childSandbox)
	live := tmuxClaim(claim.Token("legion-legion-legion-209-implementer"))
	live.State = supervise.StateLaunching
	if err := store.PutClaim(ctx, live); err != nil {
		t.Fatal(err)
	}
	if _, began, err := store.BeginIssueCleanup(ctx, "legion", childIssue); err != nil || began {
		t.Fatalf("cleanup with a launching claim: began %t, err %v", began, err)
	}
	if got := resourcesOf(t, store, childIssue); got.CleanupStarted {
		t.Fatalf("cleanup with a launching claim took the record: %+v", got)
	}
	live.State = supervise.StateRetired
	if err := store.PutClaim(ctx, live); err != nil {
		t.Fatal(err)
	}
	if _, began, err := store.BeginIssueCleanup(ctx, "legion", childIssue); err != nil || !began {
		t.Fatalf("cleanup once the claim retired: began %t, err %v", began, err)
	}
}

// A tree root is cleaned last: while a child record is unconfirmed its cleanup takes nothing, so a
// re-admitted root still launches; once begun it refuses every new child admission, and a re-admitted
// root admits children again.
func TestATreeRootIsCleanedLastAndFencesNewChildren(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	if err := store.EnsureIssueResources(ctx, "legion", childIssue, rootIssue, childSandbox); err == nil || !strings.Contains(err.Error(), "has no issue resources") {
		t.Fatalf("child admission without its root = %v", err)
	}
	ensure(t, store, rootIssue, rootSandbox)
	ensure(t, store, childIssue, childSandbox)
	if _, began, err := store.BeginIssueCleanup(ctx, "legion", rootIssue); !errors.Is(err, ErrTreeChildrenPending) || began {
		t.Fatalf("root cleanup with an unconfirmed child: began %t, err %v; want ErrTreeChildrenPending", began, err)
	}
	if got := resourcesOf(t, store, rootIssue); got.CleanupStarted {
		t.Fatalf("a refused root cleanup took the record: %+v", got)
	}
	child, began, err := store.BeginIssueCleanup(ctx, "legion", childIssue)
	if err != nil || !began {
		t.Fatalf("child cleanup: began %t, err %v", began, err)
	}
	if _, _, err := store.BeginIssueCleanup(ctx, "legion", rootIssue); !errors.Is(err, ErrTreeChildrenPending) {
		t.Fatalf("root cleanup while the child's cleanup is unconfirmed = %v, want ErrTreeChildrenPending", err)
	}
	if err := store.ConfirmIssueCleanup(ctx, "legion", childIssue, child.CleanupGeneration); err != nil {
		t.Fatal(err)
	}
	if _, began, err := store.BeginIssueCleanup(ctx, "legion", rootIssue); err != nil || !began {
		t.Fatalf("root cleanup after every child confirmed: began %t, err %v", began, err)
	}
	for _, admit := range []struct{ issue, sandbox string }{{"LEGION-210", "legion-legion-legion-210"}, {childIssue, childSandbox}} {
		if err := store.EnsureIssueResources(ctx, "legion", admit.issue, rootIssue, admit.sandbox); !errors.Is(err, ErrIssueCleanupInProgress) {
			t.Fatalf("child %s admitted during its root's cleanup: %v", admit.issue, err)
		}
	}
	if err := store.ConfirmIssueCleanup(ctx, "legion", rootIssue, 1); err != nil {
		t.Fatal(err)
	}
	ensure(t, store, rootIssue, rootSandbox)
	ensure(t, store, childIssue, childSandbox)
	if got := resourcesOf(t, store, childIssue); got.Generation != 2 || got.CleanupStarted {
		t.Fatalf("child under a re-admitted root = %+v", got)
	}
}

// The child admission reads its root under a lock a root cleanup's update waits on, and so sees a
// cleanup that committed while it waited; a plain read would admit the child into a tree whose root
// cleanup has already counted its children.
func TestAChildAdmissionWaitsOutAConcurrentRootCleanup(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	ensure(t, store, rootIssue, rootSandbox)
	tx, err := store.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The root cleanup's transaction, between its locking read and its write.
	if _, err := tx.Exec(ctx, `select 1 from issue_resources where project = 'legion' and issue = $1 for update`, rootIssue); err != nil {
		t.Fatal(err)
	}
	admitted := make(chan error, 1)
	go func() {
		admitted <- store.EnsureIssueResources(ctx, "legion", childIssue, rootIssue, childSandbox)
	}()
	select {
	case err := <-admitted:
		t.Fatalf("the child admission did not wait for the root's lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := tx.Exec(ctx, `update issue_resources set cleanup_started = true, cleanup_generation = generation
		where project = 'legion' and issue = $1`, rootIssue); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-admitted; !errors.Is(err, ErrIssueCleanupInProgress) {
		t.Fatalf("child admission after the root cleanup committed = %v, want ErrIssueCleanupInProgress", err)
	}
	if _, found, err := store.IssueResources(ctx, "legion", childIssue); err != nil || found {
		t.Fatalf("the refused child left a record: found %t, err %v", found, err)
	}
}

func TestIssuePodLayoutRefusesOtherLayouts(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	if err := store.EnsureIssuePodLayout(ctx, "legion"); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureIssuePodLayout(ctx, "legion"); err != nil {
		t.Fatalf("same layout: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `update runtime_layouts set layout = 'legacy-v0' where project = 'legion'`); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureIssuePodLayout(ctx, "legion"); err == nil || !strings.Contains(err.Error(), "legacy-v0") {
		t.Fatalf("wrong layout = %v", err)
	}
}
