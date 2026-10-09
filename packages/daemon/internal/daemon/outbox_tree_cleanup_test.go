package daemon

import (
	"context"
	"errors"
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

// A workflow close that reserved its tree's cleanup keeps driving it through the outbox after a
// re-admission ends the linger it closed. The reservation is taken, then its cleanup waits for a
// claim still running (or a delete fails transiently, the fake runtime's FailCleanupTree), then
// the root goes back to todo: the re-admission bumps the root's generation, clears its linger,
// and cannot open the tree's next epoch until the cleanup confirms. Every remaining or retried
// close row of the tree must still retire its claim and drive the cleanup to confirmation before
// the stale-close fences (the row's generation, the ended linger) apply; finished without acting,
// they would leave the claims running, the reservation unconfirmed, and the tree waiting for
// admission forever.
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

			rt := fake.NewRuntime()
			if tc.deleteFailures > 0 {
				rt.FailCleanupTree(errors.New("transient Sandbox delete failure"))
			}
			sup := newSupervisor(ctx, nil, "legion", t.TempDir(), quietLogger())
			sup.deps = supervise.Deps{
				Runtime: rt, Conns: fake.NewConns(), Store: st, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
				Limits:   supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
				Timeouts: supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second},
			}
			t.Cleanup(sup.stop)
			runner := &outbox{log: quietLogger(),
				dispatchProject: "LEGION",
				pool:            pool, records: records, supervisor: sup, trees: st, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets"),
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

			// The remaining close, and the retried one, still act under the reservation. The delete
			// that failed transiently above succeeds on retry.
			rt.FailCleanupTree(nil)
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
			claims, err := st.Claims(ctx)
			if err != nil {
				t.Fatalf("read claims: %v", err)
			}
			for _, c := range claims {
				if c.Tree == root.Key && c.State != supervise.StateRetired {
					t.Fatalf("claim %s of the closed tree still %s, want retired", c.Token, c.State)
				}
			}
		})
	}
}

// A child of a lingering tree set back to todo is re-admitted as a root of its own (an orphan),
// while its roles' claims, suspended by the old tree's close, still name the old tree. The old
// tree's cleanup does not wait on those claims, which run nothing and are no longer its, and the
// orphan's start re-points its claim to the orphan's own tree before it starts it, so the claim
// binds the new tree's lifecycle rather than the old tree's confirmed one. Under tmux the kept
// session is on the host and resumes, and so does one kept in the Sandbox runtime's session
// database (session_store postgres), which every tree's pods read. Under a runtime that keeps
// sessions on the tree's volume the session stayed on the old tree's volume, which the new tree's
// pods never mount: the claim drops it and starts fresh, recreating its workspace, rather than
// resuming a session the launcher refuses until the launch budget runs out.
func TestAnOrphansClaimsLeaveTheirOldTreeAndStartInTheirOwn(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		inPod, volumeSessions bool
	}{{"tmux", false, false}, {"a runtime that keeps sessions on the tree volume", true, true}, {"a runtime that keeps sessions in a database", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			st := outboxTreeStore(t, pool, "LEGION-208")
			records := record.NewStore()
			until := time.Now().Add(-time.Minute)
			root := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until}
			putOutboxIssue(t, pool, records, root)
			parent := root.Key
			child := record.Issue{Key: "LEGION-209", Project: "LEGION", Tree: root.Key, Parent: &parent, Title: "child", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "V"}
			putOutboxIssue(t, pool, records, child)

			rt := fake.NewRuntime()
			rt.InPod, rt.VolumeSessions = tc.inPod, tc.volumeSessions
			sup := newSupervisor(ctx, st, "legion", t.TempDir(), quietLogger())
			sup.deps = supervise.Deps{
				Runtime: rt, Conns: fake.NewConns(), Store: st, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
				Limits:   supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
				Timeouts: supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second},
			}
			t.Cleanup(sup.stop)
			token, err := claim.NewToken("legion", child.Key, claim.RolePlanner)
			if err != nil {
				t.Fatal(err)
			}
			machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: root.Key, TreeEpoch: 1, Issue: child.Key, Role: claim.RolePlanner,
				Generation: 1, State: supervise.StateSuspended, Session: "ses-planner", SessionFile: "/legion/sessions/planner.jsonl"}, "")
			if err != nil {
				t.Fatal(err)
			}

			// The old tree's linger expired and reserved its cleanup; the child was re-admitted as a root.
			lifecycle, reserved, err := st.ReserveWorkflowTreeCleanup(ctx, "legion", root.Key, root.Generation)
			if err != nil || !reserved {
				t.Fatalf("reserve the old tree's cleanup = %v, %v", reserved, err)
			}
			child.Tree, child.Parent, child.Generation = child.Key, nil, 2
			putOutboxIssue(t, pool, records, child)
			if _, err := st.OpenTreeLifecycle(ctx, "legion", child.Key, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatalf("admit the orphan's tree: %v", err)
			}
			if err := st.CleanupReservedTree(ctx, "legion", root.Key, lifecycle.Epoch, rt); err != nil {
				t.Fatalf("the old tree's cleanup with the orphan's suspended claim = %v, want it confirmed", err)
			}

			runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: st, tokens: outboxTokens{},
				project: "legion", stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets"),
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-209"}, nil
				},
			}
			start := mustOutboxRow(t, child.Key, record.SuperviseRequest{Op: "start", Tree: child.Key, Role: claim.RolePlanner, Generation: 2, Phase: phase.Planning, Task: "Plan it."}, time.Now())
			if err := runner.execute(ctx, start); err != nil {
				t.Fatalf("the orphan's planner start: %v", err)
			}
			got := machine.Claim()
			if got.Tree != child.Key || got.TreeEpoch != 1 || got.State != supervise.StateLaunching {
				t.Fatalf("orphan's planner = tree %s epoch %d %s, want tree %s epoch 1 launching", got.Tree, got.TreeEpoch, got.State, child.Key)
			}
			resumes, spawns := rt.CallsOf("Resume"), rt.CallsOf("Spawn")
			if tc.volumeSessions {
				if len(resumes) != 0 || len(spawns) != 1 || spawns[0].Spec.Tree != child.Key || got.SessionFile != "" || !got.WorkspaceLost {
					t.Fatalf("resumes %+v, spawns %+v, claim %+v; want one fresh spawn in tree %s, the old volume's session dropped", resumes, spawns, got, child.Key)
				}
				return
			}
			if len(resumes) != 1 || resumes[0].Spec.Tree != child.Key || resumes[0].Spec.ResumeSessionFile != "/legion/sessions/planner.jsonl" {
				t.Fatalf("resumes = %+v, want the kept session resumed once in tree %s", resumes, child.Key)
			}
		})
	}
}
