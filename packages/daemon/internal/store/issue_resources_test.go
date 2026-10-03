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

const (
	rootIssue, childIssue = "LEGION-208", "LEGION-209"
	rootSandbox           = "legion-legion-legion-208"
	childSandbox          = "legion-legion-legion-209"
)

func ensure(t *testing.T, store *Store, issue, sandbox string) {
	t.Helper()
	if issue == rootIssue {
		if _, err := store.OpenTreeLifecycle(context.Background(), "legion", rootIssue, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open lifecycle of %s: %v", issue, err)
		}
	}
	if err := store.EnsureIssueResources(context.Background(), "legion", issue, rootIssue, sandbox, 1); err != nil {
		t.Fatalf("admit %s: %v", issue, err)
	}
	if issue == rootIssue {
		seedLingeringRoot(t, store, 1)
	}
}

// seedLingeringRoot is the tree-close state an old cleanup must bind to. A re-admission updates
// this row and commits its architect start in the same transaction before the outbox has created a
// claim, precisely the interval BeginIssueCleanup must fence.
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

func beginWorkflowCleanup(t *testing.T, store *Store, ctx context.Context, issue string) (IssueResources, bool, error) {
	t.Helper()
	lifecycle, reserved, err := store.ReserveWorkflowTreeCleanup(ctx, "legion", rootIssue, 1)
	if err != nil || !reserved {
		return IssueResources{}, false, err
	}
	return store.BeginIssueCleanup(ctx, "legion", issue, rootIssue, lifecycle.Epoch)
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
	begun, began, err := beginWorkflowCleanup(t, store, ctx, rootIssue)
	if err != nil || !began || begun.CleanupGeneration != 1 {
		t.Fatalf("first cleanup = %+v, began %t, err %v", begun, began, err)
	}
	if retry, began, err := beginWorkflowCleanup(t, store, ctx, rootIssue); err != nil || !began || retry.CleanupGeneration != 1 {
		t.Fatalf("cleanup retry = %+v, began %t, err %v", retry, began, err)
	}
	if err := store.EnsureIssueResources(ctx, "legion", rootIssue, rootIssue, rootSandbox, 1); !errors.Is(err, ErrIssueCleanupInProgress) {
		t.Fatalf("admission during cleanup = %v, want ErrIssueCleanupInProgress", err)
	}
	if err := store.ConfirmIssueCleanup(ctx, "legion", rootIssue, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmTreeCleanup(ctx, "legion", rootIssue, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenTreeLifecycle(ctx, "legion", rootIssue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureIssueResources(ctx, "legion", rootIssue, rootIssue, rootSandbox, 2); err != nil {
		t.Fatal(err)
	}
	if got := resourcesOf(t, store, rootIssue); got.Generation != 2 || got.CleanupStarted || !got.CleanupConfirmedAt.IsZero() {
		t.Fatalf("re-admitted resources = %+v", got)
	}
	if err := store.ConfirmIssueCleanup(ctx, "legion", rootIssue, 1); err == nil {
		t.Fatal("a stale epoch's confirmation was accepted for the re-admitted issue")
	}
}

// A re-admission commits a new root generation and its architect start before that start has
// created a claim. The old tree-close cleanup must wait for that transaction, then finish without
// taking the resources; censusing only claims lets it delete the re-admitted tree in this window.
func TestCommittedReadmissionStartFencesAnOldCleanup(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	ensure(t, store, rootIssue, rootSandbox)

	reopening, err := store.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopening.Rollback(ctx) }()
	if _, err := reopening.Exec(ctx, `update issues set generation = 2, phase = 'admitted', status = 'todo',
		linger_until = null where key = $1`, rootIssue); err != nil {
		t.Fatal(err)
	}
	if _, err := reopening.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('supervise', $1, '{"op":"start","tree":"LEGION-208","role":"architect","generation":2}', 0, now(), '')`, rootIssue); err != nil {
		t.Fatal(err)
	}

	type result struct {
		resources IssueResources
		began     bool
		err       error
	}
	cleaning := make(chan result, 1)
	go func() {
		resources, began, err := beginWorkflowCleanup(t, store, ctx, rootIssue)
		cleaning <- result{resources, began, err}
	}()
	select {
	case got := <-cleaning:
		t.Fatalf("old cleanup did not wait for committed re-admission: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
	if err := reopening.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-cleaning
	if got.err != nil || got.began {
		t.Fatalf("old cleanup after re-admission = %+v, want no cleanup", got)
	}
	if resources := resourcesOf(t, store, rootIssue); resources.CleanupStarted {
		t.Fatalf("old cleanup took re-admitted root resources: %+v", resources)
	}
}

// A cleanup marker survives a transient API-delete failure. If the tree is re-admitted while the
// retry backs off, the old cleanup must resume and confirm before the new start can record
// resources: treating the changed root generation as a stale retry clears no marker and leaves the
// new start held forever.
func TestAnUnconfirmedCleanupResumesAcrossReadmissionBeforeTheNewStart(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	ensure(t, store, rootIssue, rootSandbox)
	first, began, err := beginWorkflowCleanup(t, store, ctx, rootIssue)
	if err != nil || !began {
		t.Fatalf("begin first cleanup: %+v, began %t, err %v", first, began, err)
	}
	if _, err := store.Pool().Exec(ctx, `update issues set generation = 2, phase = 'admitted', status = 'todo',
		linger_until = null where key = $1`, rootIssue); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureIssueResources(ctx, "legion", rootIssue, rootIssue, rootSandbox, 1); !errors.Is(err, ErrIssueCleanupInProgress) {
		t.Fatalf("new start before cleanup confirmation = %v, want ErrIssueCleanupInProgress", err)
	}
	retry, began, err := beginWorkflowCleanup(t, store, ctx, rootIssue)
	if err != nil || !began || retry.CleanupGeneration != first.CleanupGeneration {
		t.Fatalf("retry after re-admission = %+v, began %t, err %v; want the begun cleanup", retry, began, err)
	}
	if err := store.ConfirmIssueCleanup(ctx, "legion", rootIssue, retry.CleanupGeneration); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmTreeCleanup(ctx, "legion", rootIssue, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenTreeLifecycle(ctx, "legion", rootIssue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureIssueResources(ctx, "legion", rootIssue, rootIssue, rootSandbox, 2); err != nil {
		t.Fatalf("new start after cleanup confirmation: %v", err)
	}
	if got := resourcesOf(t, store, rootIssue); got.Generation != 2 || got.CleanupStarted {
		t.Fatalf("new start resources = %+v, want admitted epoch 2 after confirmation", got)
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
	if _, began, err := beginWorkflowCleanup(t, store, ctx, childIssue); err != nil || began {
		t.Fatalf("cleanup with a launching claim: began %t, err %v", began, err)
	}
	if got := resourcesOf(t, store, childIssue); got.CleanupStarted {
		t.Fatalf("cleanup with a launching claim took the record: %+v", got)
	}
	live.State = supervise.StateRetired
	if err := store.PutClaim(ctx, live); err != nil {
		t.Fatal(err)
	}
	if _, began, err := beginWorkflowCleanup(t, store, ctx, childIssue); err != nil || !began {
		t.Fatalf("cleanup once the claim retired: began %t, err %v", began, err)
	}
}

// A tree root is cleaned last: while a child record is unconfirmed its cleanup takes nothing, so a
// re-admitted root still launches; once begun it refuses every new child admission, and a re-admitted
// root admits children again.
func TestATreeRootIsCleanedLastAndFencesNewChildren(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	if _, err := store.OpenTreeLifecycle(ctx, "legion", rootIssue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureIssueResources(ctx, "legion", childIssue, rootIssue, childSandbox, 1); err == nil || !strings.Contains(err.Error(), "has no issue resources") {
		t.Fatalf("child admission without its root = %v", err)
	}
	ensure(t, store, rootIssue, rootSandbox)
	ensure(t, store, childIssue, childSandbox)
	if _, began, err := beginWorkflowCleanup(t, store, ctx, rootIssue); !errors.Is(err, ErrTreeChildrenPending) || began {
		t.Fatalf("root cleanup with an unconfirmed child: began %t, err %v; want ErrTreeChildrenPending", began, err)
	}
	if got := resourcesOf(t, store, rootIssue); got.CleanupStarted {
		t.Fatalf("a refused root cleanup took the record: %+v", got)
	}
	child, began, err := beginWorkflowCleanup(t, store, ctx, childIssue)
	if err != nil || !began {
		t.Fatalf("child cleanup: began %t, err %v", began, err)
	}
	if _, _, err := beginWorkflowCleanup(t, store, ctx, rootIssue); !errors.Is(err, ErrTreeChildrenPending) {
		t.Fatalf("root cleanup while the child's cleanup is unconfirmed = %v, want ErrTreeChildrenPending", err)
	}
	if err := store.ConfirmIssueCleanup(ctx, "legion", childIssue, child.CleanupGeneration); err != nil {
		t.Fatal(err)
	}
	if _, began, err := beginWorkflowCleanup(t, store, ctx, rootIssue); err != nil || !began {
		t.Fatalf("root cleanup after every child confirmed: began %t, err %v", began, err)
	}
	for _, admit := range []struct{ issue, sandbox string }{{"LEGION-210", "legion-legion-legion-210"}, {childIssue, childSandbox}} {
		if err := store.EnsureIssueResources(ctx, "legion", admit.issue, rootIssue, admit.sandbox, 1); !errors.Is(err, ErrIssueCleanupInProgress) {
			t.Fatalf("child %s admitted during its root's cleanup: %v", admit.issue, err)
		}
	}
	if err := store.ConfirmIssueCleanup(ctx, "legion", rootIssue, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmTreeCleanup(ctx, "legion", rootIssue, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenTreeLifecycle(ctx, "legion", rootIssue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureIssueResources(ctx, "legion", rootIssue, rootIssue, rootSandbox, 2); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureIssueResources(ctx, "legion", childIssue, rootIssue, childSandbox, 2); err != nil {
		t.Fatal(err)
	}
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
		admitted <- store.EnsureIssueResources(ctx, "legion", childIssue, rootIssue, childSandbox, 1)
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

// A root cleanup reservation fences a child before its first resource admission. The supervisor
// persists StateLaunching before the sandbox runtime calls EnsureIssueResources, so counting only
// resource rows lets this child escape the root's deletion census.
func TestReservedTreeRefusesAPreResourceChildClaim(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	ensure(t, store, rootIssue, rootSandbox)
	if _, began, err := beginWorkflowCleanup(t, store, ctx, rootIssue); err != nil || !began {
		t.Fatalf("reserve root cleanup: began %t, err %v", began, err)
	}
	child := tmuxClaim(claim.Token("legion-legion-legion-209-implementer"))
	child.State = supervise.StateLaunching
	if _, err := store.AdmitClaim(ctx, child); !errors.Is(err, ErrIssueCleanupInProgress) {
		t.Fatalf("pre-resource child claim during root cleanup = %v, want ErrIssueCleanupInProgress", err)
	}
}
