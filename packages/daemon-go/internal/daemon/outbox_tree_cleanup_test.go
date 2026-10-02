package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// treeCleanupRuntime is the fake runtime with the durable whole-tree capability over the daemon's
// real store. Its cleanup censuses the tree's stored claims, as the Sandbox runtime's does, and
// confirms the reservation once every one has retired, after failing deleteFailures times the way
// a transient Sandbox delete does.
type treeCleanupRuntime struct {
	*fake.Runtime
	store          *store.Store
	deleteFailures int
}

func (r *treeCleanupRuntime) ReserveWorkflowTreeCleanup(ctx context.Context, project, tree string, generation uint64) (uint64, bool, error) {
	lifecycle, reserved, err := r.store.ReserveWorkflowTreeCleanup(ctx, project, tree, generation)
	return lifecycle.Epoch, reserved, err
}

func (r *treeCleanupRuntime) ReserveOperatorTreeCleanup(context.Context, string, string) (uint64, bool, error) {
	return 0, false, errors.New("not reached")
}

func (r *treeCleanupRuntime) CleanupTree(ctx context.Context, project, tree string, epoch uint64) error {
	if err := r.store.CheckTreeCleanupReservation(ctx, project, tree, epoch); err != nil {
		return err
	}
	pending, err := r.store.PendingTreeClaims(ctx, project, tree)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return fmt.Errorf("cleanup of tree %s waits for %s: %w", tree, strings.Join(pending, ", "), store.ErrIssueCleanupInProgress)
	}
	if r.deleteFailures > 0 {
		r.deleteFailures--
		return errors.New("transient Sandbox delete failure")
	}
	return r.store.ConfirmTreeCleanup(ctx, project, tree, epoch)
}

// A workflow close that reserved its tree's cleanup keeps driving it through the outbox after a
// re-admission ends the linger it closed. The reservation is taken, then its cleanup waits for a
// claim still running (or a delete fails transiently), then the root goes back to todo: the
// re-admission bumps the root's generation, clears its linger, and cannot open the tree's next
// epoch until the cleanup confirms. Every remaining or retried close row of the tree must still
// retire its claim and drive the cleanup to confirmation before the stale-close fences (the row's
// generation, the ended linger) apply; finished without acting, they would leave the claims
// running, the reservation unconfirmed, and the tree waiting for admission forever.
func TestAReservedTreeCloseFinishesItsCleanupAfterReadmission(t *testing.T) {
	for _, tc := range []struct {
		name           string
		withChild      bool
		deleteFailures int
	}{
		{name: "cleanup waits for a running claim", withChild: true},
		{name: "cleanup delete fails transiently", deleteFailures: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			st, err := store.Open(ctx, pool.Config().ConnString())
			if err != nil {
				t.Fatalf("open the store: %v", err)
			}
			t.Cleanup(st.Close)
			records := record.NewStore()
			root := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}
			putOutboxIssue(t, pool, records, root)
			parent := root.Key
			child := record.Issue{Key: "LEGION-209", Project: "LEGION", Tree: root.Key, Parent: &parent, Title: "child", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "V"}
			if tc.withChild {
				putOutboxIssue(t, pool, records, child)
			}
			if _, err := st.OpenTreeLifecycle(ctx, "legion", root.Key, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatalf("admit the tree: %v", err)
			}

			rt := &treeCleanupRuntime{Runtime: fake.NewRuntime(), store: st, deleteFailures: tc.deleteFailures}
			sup := newSupervisor(ctx, nil, "legion", t.TempDir(), quietLogger())
			sup.deps = supervise.Deps{
				Runtime: rt, Conns: fake.NewConns(), Store: st, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
				Limits:   supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
				Timeouts: supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second},
			}
			t.Cleanup(sup.stop)
			runner := &outbox{log: quietLogger(),
				dispatchProject: "LEGION",
				pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets"),
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
			}
			start := func(issue record.Issue, role claim.Role, task string) *supervise.Machine {
				t.Helper()
				row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role, Task: task, Generation: issue.Generation}, time.Now())
				if err := runner.execute(ctx, row); err != nil {
					t.Fatalf("start the %s of %s: %v", role, issue.Key, err)
				}
				token, err := claim.NewToken("legion", issue.Key, role)
				if err != nil {
					t.Fatal(err)
				}
				machine, found := sup.Machine(token)
				if !found {
					t.Fatalf("no claim for the %s of %s", role, issue.Key)
				}
				return machine
			}
			closeOf := func(issue record.Issue, role claim.Role) record.OutboxRow {
				return mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "tree_close", Tree: root.Key, Role: role, Generation: 1, Linger: 1}, time.Now())
			}
			architect := start(root, claim.RoleArchitect, "")
			var tester *supervise.Machine
			if tc.withChild {
				tester = start(child, claim.RoleTester, "Test it.")
			}

			// The tree lingers at generation 1 and its linger expires: the architect's close reserves
			// the cleanup and retires the architect, but the cleanup does not finish yet.
			until := time.Now().Add(-time.Minute)
			root.Phase, root.Status, root.LingerUntil = phase.Done, "done", &until
			putOutboxIssue(t, pool, records, root)
			if err := runner.execute(ctx, closeOf(root, claim.RoleArchitect)); err == nil {
				t.Fatal("the first close finished its cleanup; want it to wait for the running claim or fail its delete")
			}
			if got := architect.Claim().State; got != supervise.StateRetired {
				t.Fatalf("architect after the reserving close = %s, want retired", got)
			}

			// The root goes back to todo and is re-admitted: generation 2, no linger. The tree's next
			// epoch waits for the reserved cleanup.
			root.Generation, root.Phase, root.Status, root.LingerUntil = 2, phase.Planning, "in_progress", nil
			putOutboxIssue(t, pool, records, root)
			if opened, err := st.OpenTreeLifecycle(ctx, "legion", root.Key, treelifecycle.AuthorityWorkflow); !errors.Is(err, treelifecycle.ErrCleanupReserved) {
				t.Fatalf("re-admission before the cleanup confirmed = %+v, %v; want the reservation's wait", opened, err)
			}

			// The remaining close, and the retried one, still act under the reservation.
			if tc.withChild {
				if err := runner.execute(ctx, closeOf(child, claim.RoleTester)); err != nil {
					t.Fatalf("the tester's close after re-admission: %v", err)
				}
				if got := tester.Claim().State; got != supervise.StateRetired {
					t.Fatalf("tester after its close under the reservation = %s, want retired", got)
				}
			}
			if err := runner.execute(ctx, closeOf(root, claim.RoleArchitect)); err != nil {
				t.Fatalf("the architect's retried close after re-admission: %v", err)
			}
			opened, err := st.OpenTreeLifecycle(ctx, "legion", root.Key, treelifecycle.AuthorityWorkflow)
			if err != nil || opened.Epoch != 2 {
				t.Fatalf("re-admission after the closes = %+v, %v; want the tree's epoch 2 opened", opened, err)
			}
			if pending, err := st.PendingTreeClaims(ctx, "legion", root.Key); err != nil || len(pending) != 0 {
				t.Fatalf("claims of the closed tree still pending: %v (err %v)", pending, err)
			}
		})
	}
}
