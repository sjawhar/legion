package daemon

import (
	"context"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	legionstore "github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// isolatedRemovableSupervisor is isolatedOutboxPool plus a supervisor wired to a real store-backed
// ClaimStore, so sup.Claims (removableWorkspaces' own dependency, read with no machine lock taken:
// the point of the fix this test file covers) returns every claimOn plants — unlike
// newOutboxSupervisor's fake in-memory outboxClaimStore, which supervisor.Claims never reads at
// all, since its own supervisor.store stays nil there.
func isolatedRemovableSupervisor(t *testing.T) (*pgxpool.Pool, *supervisor) {
	t.Helper()
	base, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(context.Background(), adminURL.String())
	if err != nil {
		t.Fatalf("connect test admin database: %v", err)
	}
	t.Cleanup(admin.Close)
	name := "legion_removable_test_" + outboxSuffix(t)
	if _, err := admin.Exec(context.Background(), "create database "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	databaseURL := *base
	databaseURL.Path = "/" + name
	st, err := legionstore.Open(context.Background(), databaseURL.String())
	if err != nil {
		t.Fatalf("open isolated store: %v", err)
	}
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate isolated store: %v", err)
	}
	t.Cleanup(st.Close)
	// Every claim claimOn plants is of tree LEGION-1, which admission opens before any claim of it
	// exists (store.AdmitClaim refuses a claim of a tree whose lifecycle is absent).
	if _, err := st.OpenTreeLifecycle(context.Background(), "legion", "LEGION-1", treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("admit tree LEGION-1: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), databaseURL.String())
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(pool.Close)
	sup := newSupervisor(context.Background(), st, "legion", t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: fake.NewRuntime(), Conns: fake.NewConns(), Store: st, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		Limits:   supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts: supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second},
	}
	t.Cleanup(sup.stop)
	return pool, sup
}

// putRemovableIssue plants issue so TreeIssues reads it back; a helper distinct from
// putOutboxIssue only in intent (dispatch://LEGION-583's removable_test.go).
func putRemovableIssue(t *testing.T, pool *pgxpool.Pool, records record.Store, issue record.Issue) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutIssue(context.Background(), tx, issue)
	}); err != nil {
		t.Fatalf("put issue %s: %v", issue.Key, err)
	}
}

// putMergedPullRequest records issue's pull request as merged at head, what removableWorkspaces
// pairs a done or parked candidate with so workspace-init's push-safety check can tell a squash
// merge's now-branchless head from a commit that was never pushed at all.
func putMergedPullRequest(t *testing.T, pool *pgxpool.Pool, records record.Store, issue, head string) {
	t.Helper()
	pr := record.PullRequest{
		Issue: issue, Repo: "acme/widgets", Number: 1, Branch: "legion/" + issue,
		HeadSHA: head, HeadUpdatedAt: time.Now().UTC(), State: record.PullRequestMerged,
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutPullRequest(context.Background(), tx, pr)
	}); err != nil {
		t.Fatalf("put pull request of %s: %v", issue, err)
	}
}

// claimOn creates issue's role claim in state, the architectClaimOn of notice_exceptions_test.go
// generalized to any role: removableWorkspaces asks after every role's claim, not the architect's
// alone.
func claimOn(t *testing.T, sup *supervisor, issue string, role claim.Role, state supervise.ClaimState) {
	t.Helper()
	if _, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: mustClaimToken(t, issue, role), Project: "legion", Tree: "LEGION-1", Issue: issue, Role: role, State: state, Session: "ses_" + issue + "_" + string(role),
	}, ""); err != nil {
		t.Fatalf("create the %s claim of %s: %v", role, issue, err)
	}
}

func callRemovable(t *testing.T, pool *pgxpool.Pool, records record.Store, sup *supervisor, tree, exclude string) []runtime.RemovableWorkspace {
	t.Helper()
	got, err := removableWorkspaces(pool, records, sup)(context.Background(), tree, exclude)
	if err != nil {
		t.Fatalf("removableWorkspaces: %v", err)
	}
	return got
}

// A done child is a candidate, paired with its merged pull request's head: workspace-init's
// push-safety check needs the head once GitHub deletes the squash merge's branch.
func TestRemovableWorkspacesIncludesADoneChildWithItsMergedHead(t *testing.T) {
	pool, sup := isolatedRemovableSupervisor(t)
	records := record.NewStore()
	putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-1", Project: "LEGION", Tree: "LEGION-1", Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-2", Project: "LEGION", Tree: "LEGION-1", Title: "Done child", Phase: phase.Done, Generation: 1, Status: "done"})
	putMergedPullRequest(t, pool, records, "LEGION-2", "deadbeef")

	got := callRemovable(t, pool, records, sup, "LEGION-1", "")
	if len(got) != 1 || got[0].Issue != "LEGION-2" || got[0].MergedHead != "deadbeef" {
		t.Fatalf("candidates = %+v, want one: LEGION-2 with merged head deadbeef", got)
	}
}

// A done child whose every role's claim is suspended, failed, retired, or never made is a
// candidate, whatever Dispatch status brought it there (done, backlog, icebox, and triage all
// force phase done on the same leave, workflow/linger.go): a human shelved it, or its work is
// genuinely finished, and nothing of its role is coming back on its own.
func TestRemovableWorkspacesIncludesADoneChildWithEveryClaimParkedOrAbsent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		states map[claim.Role]supervise.ClaimState
	}{
		{"backlog, a suspended claim", "backlog", map[claim.Role]supervise.ClaimState{
			claim.RoleImplementer: supervise.StateSuspended,
		}},
		{"icebox, a failed and a retired claim", "icebox", map[claim.Role]supervise.ClaimState{
			claim.RoleImplementer: supervise.StateFailed,
			claim.RoleTester:      supervise.StateRetired,
		}},
		{"backlog, no claim ever made", "backlog", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, sup := isolatedRemovableSupervisor(t)
			records := record.NewStore()
			putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-1", Project: "LEGION", Tree: "LEGION-1", Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
			putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-2", Project: "LEGION", Tree: "LEGION-1", Title: "Done child", Phase: phase.Done, Generation: 1, Status: tc.status})
			for role, state := range tc.states {
				claimOn(t, sup, "LEGION-2", role, state)
			}

			got := callRemovable(t, pool, records, sup, "LEGION-1", "")
			if len(got) != 1 || got[0].Issue != "LEGION-2" {
				t.Fatalf("candidates = %+v, want one: LEGION-2", got)
			}
		})
	}
}

// A child merely between phases — awaiting_merge, a review round the architect has not yet
// decided, or plain todo waiting on the workflow — never reaches phase done, so it is never a
// candidate whatever its claims or Dispatch status: removing its workspace here would force its
// next phase into a multi-minute checkout, the shape this correction exists to stop.
func TestRemovableWorkspacesExcludesAChildBetweenPhases(t *testing.T) {
	for _, tc := range []struct {
		name   string
		phase  phase.Phase
		status string
	}{
		{"awaiting_merge", phase.AwaitingMerge, "in_progress"},
		{"a review round still in_progress", phase.Reviewing, "in_progress"},
		{"todo, waiting on the workflow", phase.Admitted, "todo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, sup := isolatedRemovableSupervisor(t)
			records := record.NewStore()
			putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-1", Project: "LEGION", Tree: "LEGION-1", Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
			putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-2", Project: "LEGION", Tree: "LEGION-1", Title: "Mid-phase child", Phase: tc.phase, Generation: 1, Status: tc.status})
			// Every claim suspended: the predicate excludes this issue by its phase alone, never
			// reaching the claim check, so a suspended claim here proves nothing about claims and
			// everything about phase.
			claimOn(t, sup, "LEGION-2", claim.RoleImplementer, supervise.StateSuspended)

			got := callRemovable(t, pool, records, sup, "LEGION-1", "")
			if len(got) != 0 {
				t.Fatalf("candidates = %+v, want none", got)
			}
		})
	}
}

// A live claim excludes its issue whatever its phase: an ordinary mid-phase child with a working
// claim, and the race leave (workflow/linger.go) opens — its phase-done write and its claim
// suspends are not one transaction, so a probe between them can see phase done on an issue whose
// implementer claim the outbox has not yet suspended. Without the claim check on the done branch
// this second case was a candidate on phase alone; both must be excluded.
func TestRemovableWorkspacesExcludesALiveChild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase phase.Phase
	}{
		{"mid-phase, a working claim", phase.Implementing},
		{"phase done, a claim the outbox has not yet suspended", phase.Done},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, sup := isolatedRemovableSupervisor(t)
			records := record.NewStore()
			putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-1", Project: "LEGION", Tree: "LEGION-1", Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
			putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-2", Project: "LEGION", Tree: "LEGION-1", Title: "Live child", Phase: tc.phase, Generation: 1, Status: "backlog"})
			claimOn(t, sup, "LEGION-2", claim.RoleImplementer, supervise.StateWorking)

			got := callRemovable(t, pool, records, sup, "LEGION-1", "")
			if len(got) != 0 {
				t.Fatalf("candidates = %+v, want none", got)
			}
		})
	}
}

// The tree's own root and the issue excluded for this launch are never candidates, whatever their
// phase or status.
func TestRemovableWorkspacesExcludesTheRootAndTheExcludedIssue(t *testing.T) {
	pool, sup := isolatedRemovableSupervisor(t)
	records := record.NewStore()
	putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-1", Project: "LEGION", Tree: "LEGION-1", Title: "Root", Phase: phase.Done, Generation: 1, Status: "done"})
	putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-2", Project: "LEGION", Tree: "LEGION-1", Title: "Done, but this launch's own issue", Phase: phase.Done, Generation: 1, Status: "done"})

	got := callRemovable(t, pool, records, sup, "LEGION-1", "LEGION-2")
	if len(got) != 0 {
		t.Fatalf("candidates = %+v, want none", got)
	}
}

// barrieredTreeIssues is records.Store with TreeIssues forced to rendezvous with n callers before
// any of them returns, so two goroutines racing removableWorkspaces for the same tree are
// guaranteed to overlap deep inside it (past the point the launching claim's own machine lock is
// already held) rather than happening to run one after the other, which would hide a lock-order
// bug behind a lucky interleaving.
type barrieredTreeIssues struct {
	record.Store
	barrier *sync.WaitGroup
}

func (b barrieredTreeIssues) TreeIssues(ctx context.Context, tx pgx.Tx, tree string) ([]record.Issue, error) {
	b.barrier.Done()
	b.barrier.Wait()
	return b.Store.TreeIssues(ctx, tx, tree)
}

// removableOnlyRuntime is runtime.Runtime whose Spawn calls only removable before delegating:
// specs.SpawnSpec no longer calls removable (dispatch://LEGION-583's last-moment fix moved that
// call into the sandbox runtime's own relaunch, after the tree's launch turn is held); this
// wraps the fake runtime's Spawn instead, the same call Machine.launch→start reaches under the
// same lock, so the deadlock this test guards against is exercised at its real call site.
type removableOnlyRuntime struct {
	*fake.Runtime
	removable func(ctx context.Context, tree, exclude string) ([]runtime.RemovableWorkspace, error)
}

func (r removableOnlyRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if _, err := r.removable(ctx, spec.Tree, spec.Issue); err != nil {
		return runtime.Locator{}, err
	}
	return r.Runtime.Spawn(ctx, spec)
}

// Two done siblings of the same tree launching at once each compute the tree's removable
// workspaces from inside Machine.Handle, which holds the launching claim's own machine lock for
// the whole transition (supervisor.go:188-189's rule: code reached this way reads no other
// machine). A version of issueHasALiveClaim that took another machine's lock to read its claim
// state deadlocked here: each Handle call waits on the other's lock from inside its own, an AB-BA
// cycle neither side ever breaks. Both must complete.
func TestRemovableWorkspacesTwoDoneSiblingsLaunchAtOnceAndBothComplete(t *testing.T) {
	pool, sup := isolatedRemovableSupervisor(t)
	records := record.NewStore()
	putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-1", Project: "LEGION", Tree: "LEGION-1", Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-2", Project: "LEGION", Tree: "LEGION-1", Title: "Done sibling", Phase: phase.Done, Generation: 1, Status: "done"})
	putRemovableIssue(t, pool, records, record.Issue{Key: "LEGION-3", Project: "LEGION", Tree: "LEGION-1", Title: "Done sibling", Phase: phase.Done, Generation: 1, Status: "done"})

	var barrier sync.WaitGroup
	barrier.Add(2)
	barriered := barrieredTreeIssues{Store: records, barrier: &barrier}
	removable := removableWorkspaces(pool, barriered, sup)
	fakeRuntime, ok := sup.deps.Runtime.(*fake.Runtime)
	if !ok {
		t.Fatalf("sup.deps.Runtime is %T, want *fake.Runtime", sup.deps.Runtime)
	}
	sup.deps.Runtime = removableOnlyRuntime{Runtime: fakeRuntime, removable: removable}

	launch := func(issue string) (*supervise.Machine, claim.Token) {
		t.Helper()
		token := mustClaimToken(t, issue, claim.RoleImplementer)
		machine, created, err := sup.Create(context.Background(), supervise.Claim{
			Token: token, Project: "legion", Tree: "LEGION-1", Issue: issue, Role: claim.RoleImplementer,
			State: supervise.StateQueued, Generation: 1,
		}, "")
		if err != nil {
			t.Fatalf("create the implementer claim of %s: %v", issue, err)
		}
		if !created {
			t.Fatalf("the implementer claim of %s already existed", issue)
		}
		return machine, token
	}
	machine2, token2 := launch("LEGION-2")
	machine3, token3 := launch("LEGION-3")

	results := make(chan error, 2)
	for machine, token := range map[*supervise.Machine]claim.Token{machine2: token2, machine3: token3} {
		go func(machine *supervise.Machine, token claim.Token) {
			results <- machine.Handle(context.Background(), supervise.RequestSpawn{Claim: token})
		}(machine, token)
	}
	deadline := time.After(10 * time.Second)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("launch a done sibling: %v", err)
			}
		case <-deadline:
			t.Fatal("two done siblings launching at once did not both complete within 10s: likely the AB-BA deadlock this test guards against")
		}
	}
	if got := machine2.Claim().State; got != supervise.StateLaunching {
		t.Errorf("LEGION-2's implementer claim = %s, want launching", got)
	}
	if got := machine3.Claim().State; got != supervise.StateLaunching {
		t.Errorf("LEGION-3's implementer claim = %s, want launching", got)
	}
}
