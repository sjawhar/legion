package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// releaseRuntime is the fake runtime with the two things an idle release asks of a Kubernetes
// one: the bundle of the issue workspace's unpushed work, taken from a launcher of the pod, and
// the issue-level teardown that deletes the pod, its disk and its Sandbox. It also answers
// whether the pod runs the configured image, which the daemon reads outside the transaction.
type releaseRuntime struct {
	*fake.Runtime
	mu        sync.Mutex
	onImage   bool
	bundle    runtime.WorkspaceBundle
	bundleErr error
	bundled   []claim.Role
	suspended int
	refused   int
}

func (r *releaseRuntime) IssuePodRunsConfiguredImage(string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.onImage
}

func (r *releaseRuntime) BundleWorkspace(_ context.Context, _ string, role claim.Role) (runtime.WorkspaceBundle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bundled = append(r.bundled, role)
	return r.bundle, r.bundleErr
}

func (r *releaseRuntime) SuspendIssue(ctx context.Context, _, _ string, authorize func(context.Context) (bool, error)) error {
	act, err := authorize(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if act {
		r.suspended++
	} else {
		r.refused++
	}
	return nil
}

func (r *releaseRuntime) reads() (bundled []claim.Role, suspended int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]claim.Role(nil), r.bundled...), r.suspended
}

// idleRig is an issue in the workflow whose pod has been quiet long enough to release: one live
// implementer in its own session, idle with the phase's task still pending, on the daemon's
// plugin contract and the configured image, with nothing queued for any role.
type idleRig struct {
	pool    *pgxpool.Pool
	st      *store.Store
	runner  *outbox
	sup     *supervisor
	rt      *releaseRuntime
	w       *workflowRuntime
	issue   record.Issue
	token   claim.Token
	machine *supervise.Machine
}

func newIdleRig(t *testing.T) *idleRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	putOutboxIssue(t, pool, records, issue)
	st := outboxTreeStore(t, pool, issue.Key)

	rt := &releaseRuntime{Runtime: fake.NewRuntime(), onImage: true,
		bundle: runtime.WorkspaceBundle{Bundle: []byte("PACK"), Head: "abc", Tips: []string{"abc"}, At: time.Now().UTC()}}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Runtime, sup.deps.Store, sup.deps.PluginContract = rt, st, api.DaemonAPIVersion

	token := mustClaimToken(t, issue.Key, claim.RoleImplementer)
	locator, err := rt.Spawn(ctx, runtime.SpawnSpec{Claim: token, Role: claim.RoleImplementer})
	if err != nil {
		t.Fatalf("mint a locator: %v", err)
	}
	pending := supervise.Delivery{ID: "outbox:7", Task: "implement it", Phase: phase.Implementing,
		Generation: 1, QueuedAt: time.Now().UTC().Add(-time.Hour)}
	stored := supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
		Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
		State: supervise.StateIdle, Locator: &locator, Session: "ses-implementer",
		SessionFile: "/tmp/implementer.jsonl", PluginContract: api.DaemonAPIVersion, Pending: &pending}
	if err := st.PutClaimAndDelivery(ctx, stored, pending); err != nil {
		t.Fatalf("seed the idle implementer: %v", err)
	}
	if _, err := sup.restore(ctx, []supervise.Claim{stored}); err != nil {
		t.Fatalf("restore the idle implementer: %v", err)
	}
	machine, ok := sup.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	if got := machine.Claim(); got.State != supervise.StateIdle || got.Pending == nil {
		t.Fatalf("the restored claim = %+v, want it idle holding its phase's task", got)
	}
	// The pod last did something half an hour ago, and its session reported five minutes later
	// that it runs no background work with no reminder armed.
	if _, err := pool.Exec(ctx, `update claims set updated_at = now() - interval '30 minutes',
		activity_at = now() - interval '25 minutes', activity_background = false,
		activity_kinds = '', activity_next_reminder_at = null where issue = $1`, issue.Key); err != nil {
		t.Fatalf("backdate the pod: %v", err)
	}

	// The drain runs a minute after whatever the release queued, so every row of it is due.
	runner := &outbox{
		pool: pool, records: records, project: "legion", dispatchProject: "LEGION", supervisor: sup, trees: st,
		notices: &outboxPublisher{}, log: quietLogger(),
		now: func() time.Time { return time.Now().Add(time.Minute) },
	}
	w := &workflowRuntime{pool: pool, projectID: "legion", log: quietLogger(),
		idleStore: st, idleImages: rt, idlePeriod: 10 * time.Minute}
	return &idleRig{pool: pool, st: st, runner: runner, sup: sup, rt: rt, w: w, issue: issue, token: token, machine: machine}
}

// drain runs the issue's lifecycle lane until it has nothing left to do.
func (r *idleRig) drain(t *testing.T) {
	t.Helper()
	for range 6 {
		if err := r.runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("run the outbox: %v", err)
		}
	}
}

// rows is every outbox row of the issue, by kind and the supervise op where it has one.
func (r *idleRig) rows(t *testing.T) []string {
	t.Helper()
	found, err := r.pool.Query(context.Background(), `select kind, coalesce(payload->>'op', '') from outbox
		where issue = $1 order by id`, r.issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer found.Close()
	var kinds []string
	for found.Next() {
		var kind, op string
		if err := found.Scan(&kind, &op); err != nil {
			t.Fatal(err)
		}
		if op != "" {
			kind += ":" + op
		}
		kinds = append(kinds, kind)
	}
	return kinds
}

func (r *idleRig) boundary(t *testing.T) int64 {
	t.Helper()
	var boundary int64
	if err := r.pool.QueryRow(context.Background(), `select boundary_row from issue_releases where issue = $1`,
		r.issue.Key).Scan(&boundary); err != nil {
		t.Fatalf("read the release's boundary: %v", err)
	}
	return boundary
}

func (r *idleRig) marked(t *testing.T) bool {
	t.Helper()
	var marked bool
	if err := r.pool.QueryRow(context.Background(), `select exists (select 1 from issue_releases where issue = $1)`,
		r.issue.Key).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	return marked
}

func (r *idleRig) bundled(t *testing.T) bool {
	t.Helper()
	_, held, err := r.st.WorkspaceBundle(context.Background(), r.issue.Key)
	if err != nil {
		t.Fatalf("read the saved bundle: %v", err)
	}
	return held
}

// A pod that has had no turn in progress and no turn due for the idle period is released while
// its issue is still in the workflow: the decision writes one `release` row per live role and the
// teardown behind them, each claim goes to suspended with the source `released` keeping its phase
// and the task it holds, the workspace's unpushed work is saved as a bundle before anything is
// deleted, and the Sandbox teardown runs last.
func TestAnIdleIssuesPodIsReleasedWithItsClaimsKeepingTheirPhaseAndTask(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()

	rig.w.releaseIdlePods(ctx)
	if got := rig.rows(t); len(got) != 2 || got[0] != "supervise:release" || got[1] != "issue_release" {
		t.Fatalf("queued rows = %v, want the role stop then the teardown", got)
	}
	if !rig.marked(t) {
		t.Fatal("the release left no mark")
	}

	rig.drain(t)

	got := rig.machine.Claim()
	if got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByReleased {
		t.Fatalf("the released claim = %s by %s, want suspended by released", got.State, got.SuspendedBy)
	}
	if got.Pending == nil || got.Pending.Phase != phase.Implementing || got.Pending.ID != "outbox:7" {
		t.Fatalf("the released claim's task = %+v, want its phase's task kept", got.Pending)
	}
	if got.Generation != 1 || got.ServingGeneration != 1 {
		t.Fatalf("the released claim = %+v, want its place in the phase kept", got)
	}
	var state, by string
	if err := rig.pool.QueryRow(ctx, `select state, suspended_by from claims where token = $1`, string(rig.token)).Scan(&state, &by); err != nil {
		t.Fatal(err)
	}
	if state != "suspended" || by != "released" {
		t.Fatalf("the stored claim = %s by %s, want suspended by released", state, by)
	}
	var tasks int
	if err := rig.pool.QueryRow(ctx, `select count(*) from pending_task_deliveries where claim_token = $1`, string(rig.token)).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 {
		t.Fatalf("stored pending tasks = %d, want the phase's task kept through the release", tasks)
	}

	bundled, suspended := rig.rt.reads()
	if len(bundled) != 1 || bundled[0] != claim.RoleImplementer {
		t.Fatalf("bundle asked of %v, want the pod's one live role", bundled)
	}
	if suspended != 1 {
		t.Fatalf("Sandbox teardowns = %d, want one", suspended)
	}
	if !rig.bundled(t) {
		t.Fatal("the release deleted the pod with no bundle saved")
	}
	if rig.marked(t) {
		t.Fatal("the finished release left its mark behind")
	}
	if got := rig.rows(t); len(got) != 0 {
		t.Fatalf("rows left after the release = %v, want none", got)
	}
}

// R4's two halves. The boundary is the stop of the first role process of the pod. A delivery
// queued below it was queued while every role still ran, so it cancels the release: the roles go
// on in the same pod, nothing is suspended, and the bundle, had one been stored, goes with it.
// One queued above it waits behind the release on the issue's lifecycle lane, so the release
// finishes and the pod comes back for it.
func TestADeliveryBeforeTheBoundaryCancelsTheReleaseAndOneAfterItWaits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		before   bool
		released bool
	}{
		{name: "queued before the boundary", before: true},
		{name: "queued after the boundary", released: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			ctx := context.Background()
			rig.w.releaseIdlePods(ctx)
			boundary := rig.boundary(t)

			// The delivery's transaction took its id before the release's and committed after.
			// It is not due yet, so the drain below runs the release's own rows and this row
			// stays queued, which is what the boundary reads.
			id := boundary - 1
			if !tc.before {
				id = boundary + 10
			}
			if _, err := rig.pool.Exec(ctx, `insert into outbox (id, kind, issue, payload, attempts, next_at, last_error)
				values ($1, 'supervise', $2, '{"op":"start","tree":"LEGION-208","role":"tester","generation":1}', 0, now() + interval '1 hour', '')`,
				id, rig.issue.Key); err != nil {
				t.Fatal(err)
			}

			rig.drain(t)

			got := rig.machine.Claim()
			_, suspended := rig.rt.reads()
			if tc.released {
				if got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByReleased {
					t.Fatalf("the claim = %s by %s, want the release to have finished", got.State, got.SuspendedBy)
				}
				if suspended != 1 || !rig.bundled(t) {
					t.Fatalf("teardowns = %d, bundle saved = %t; want the release complete", suspended, rig.bundled(t))
				}
			} else {
				if got.State == supervise.StateSuspended {
					t.Fatalf("the claim = %s by %s, want the cancelled release to have left it running", got.State, got.SuspendedBy)
				}
				if suspended != 0 {
					t.Fatalf("Sandbox teardowns = %d, want none: the pod goes on", suspended)
				}
				if rig.bundled(t) {
					t.Fatal("the cancelled release left its bundle behind")
				}
			}
			if rig.marked(t) {
				t.Fatal("the release's mark outlived it")
			}
			// The delivery itself is untouched either way: it is still queued for its role.
			var left int
			if err := rig.pool.QueryRow(ctx, `select count(*) from outbox where issue = $1 and id = $2`, rig.issue.Key, id).Scan(&left); err != nil {
				t.Fatal(err)
			}
			if left != 1 {
				t.Fatalf("the queued start = %d rows, want it left for its own turn", left)
			}
		})
	}
}

// A bundle the release cannot save stops it: the pod stays, every claim the release suspended is
// resumed into it, and no Sandbox is torn down. The work a role has not pushed is kept at the
// cost of a pod, never dropped to save one.
func TestABundleTheReleaseCannotSaveStopsItAndResumesTheClaims(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bundle runtime.WorkspaceBundle
		err    error
	}{
		{name: "the bundle could not be built", err: errors.New("the launcher answered nothing")},
		{name: "the bundle is over the bound", bundle: runtime.WorkspaceBundle{Bundle: make([]byte, runtime.MaxWorkspaceBundleBytes+1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			ctx := context.Background()
			rig.rt.bundle, rig.rt.bundleErr = tc.bundle, tc.err

			rig.w.releaseIdlePods(ctx)
			rig.drain(t)

			got := rig.machine.Claim()
			if got.State == supervise.StateSuspended {
				t.Fatalf("the claim = %s by %s, want it resumed into the pod the release kept", got.State, got.SuspendedBy)
			}
			if _, suspended := rig.rt.reads(); suspended != 0 {
				t.Fatalf("Sandbox teardowns = %d, want none", suspended)
			}
			if rig.bundled(t) {
				t.Fatal("a release that could not save its bundle left one behind")
			}
			if rig.marked(t) {
				t.Fatal("the stopped release left its mark behind")
			}
		})
	}
}

// Release is off unless the deployment asks for it: `idle_minutes: 0`, a runtime that is not
// Kubernetes, and a runtime that cannot read a pod's image each leave the tick with nothing to
// run, so it returns rather than ticking over a pod it must not decide.
func TestTheIdleTickDoesNothingWhenReleaseIsOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    *workflowRuntime
	}{
		{name: "idle_minutes is 0", w: &workflowRuntime{idleStore: &store.Store{}, idleImages: &releaseRuntime{}}},
		{name: "no store", w: &workflowRuntime{idlePeriod: time.Minute, idleImages: &releaseRuntime{}}},
		{name: "a runtime with no pod image", w: &workflowRuntime{idlePeriod: time.Minute, idleStore: &store.Store{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			go func() { defer close(done); tc.w.tickIdle(context.Background()) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("tickIdle kept running with release off")
			}
		})
	}
}
