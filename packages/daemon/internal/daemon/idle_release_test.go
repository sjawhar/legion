package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
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
	// beforeAuthorize runs inside SuspendIssue, before it asks the store: the pod lock is held
	// and the teardown's own read has already answered, which is where production's second
	// answer can differ from its first. A test sets it before the drain.
	beforeAuthorize func()
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
	r.mu.Lock()
	hook := r.beforeAuthorize
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
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

// refusals is how many times the runtime asked the store under its pod lock and was refused.
func (r *releaseRuntime) refusals() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refused
}

// idleRig is an issue in the workflow whose pod has been quiet long enough to release: one live
// implementer in its own session, idle with the phase's task still pending, on the daemon's
// plugin contract and the configured image, with nothing queued for any role.
type idleRig struct {
	pool    *pgxpool.Pool
	st      *store.Store
	issues  record.Store
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
	// The teardown records the release through the supervisor's store, as the daemon's does.
	sup.store = st

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
	// The idle rule releases nothing unless the Envoy listener has reported itself ready inside
	// the freshness window (R7, workflowRuntime.listenerLive). A rig stands for a deployment
	// whose listener is reporting.
	if _, err := pool.Exec(ctx, `insert into listener_reports (project, ready_at) values ($1, $2)
		on conflict (project) do update set ready_at = excluded.ready_at`, "legion", time.Now().UTC()); err != nil {
		t.Fatalf("seed the listener's report: %v", err)
	}

	// The drain runs a minute after whatever the release queued, so every row of it is due.
	runner := &outbox{
		pool: pool, records: records, project: "legion", dispatchProject: "LEGION", supervisor: sup, trees: st,
		notices: &outboxPublisher{}, log: quietLogger(),
		now: func() time.Time { return time.Now().Add(time.Minute) },
	}
	w := &workflowRuntime{pool: pool, projectID: "legion", log: quietLogger(),
		idleStore: st, idleImages: rt, idlePeriod: 10 * time.Minute}
	return &idleRig{pool: pool, st: st, issues: records, runner: runner, sup: sup, rt: rt, w: w,
		issue: issue, token: token, machine: machine}
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
	_, held, err := r.st.WorkspaceBundleSize(context.Background(), r.issue.Key)
	if err != nil {
		t.Fatalf("read the saved bundle: %v", err)
	}
	return held
}

// releasedArchitect leaves the pod an architect claim an earlier release suspended and no wake
// has brought back: suspended with the source `released`, as quiet as the rest of the pod, and
// restored into the supervisor the way the daemon's boot restores it.
func (r *idleRig) releasedArchitect(t *testing.T) *supervise.Machine {
	t.Helper()
	ctx := context.Background()
	token := mustClaimToken(t, r.issue.Key, claim.RoleArchitect)
	stored := supervise.Claim{Token: token, Project: "legion", Tree: r.issue.Tree, Issue: r.issue.Key,
		Role: claim.RoleArchitect, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased,
		Session: "ses-architect", SessionFile: "/tmp/architect.jsonl", PluginContract: api.DaemonAPIVersion}
	if err := r.st.PutClaim(ctx, stored); err != nil {
		t.Fatalf("seed the architect an earlier release left released: %v", err)
	}
	if _, err := r.sup.restore(ctx, []supervise.Claim{stored}); err != nil {
		t.Fatalf("restore the released architect: %v", err)
	}
	if _, err := r.pool.Exec(ctx, `update claims set updated_at = now() - interval '30 minutes'
		where token = $1`, string(token)); err != nil {
		t.Fatalf("backdate the released architect: %v", err)
	}
	machine, ok := r.sup.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	if got := machine.Claim(); !got.Released() {
		t.Fatalf("the restored architect = %s by %s, want it released", got.State, got.SuspendedBy)
	}
	return machine
}

// The teardown acts on the answer the runtime took under its pod lock, not on the one this row
// read before it. The two differ: the release's rows run on the issue's lifecycle lane, and a
// notice or a phase-ended message runs on a lane of its own, so the wake one of those makes can
// resume a role in between. A teardown that took the first answer and ignored the second
// recorded a release of a pod that still stands, armed a backstop over it and forgot its wake
// marks, and left the other roles released in a running pod with nothing to bring them back.
func TestATeardownTheRuntimeFindsUnauthorizedCancelsTheRelease(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	// Under the pod lock, before the store is asked: a re-admission moved the issue's run on,
	// which is a close's business and not a release's.
	rig.rt.beforeAuthorize = func() {
		if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
			t.Errorf("move the issue's generation on: %v", err)
		}
	}

	rig.drain(t)

	if refused := rig.rt.refusals(); refused != 1 {
		t.Fatalf("authorizations refused under the pod lock = %d, want the one the teardown asked for", refused)
	}
	if _, suspended := rig.rt.reads(); suspended != 0 {
		t.Fatalf("Sandbox teardowns = %d, want none: the store refused the pod", suspended)
	}
	records := rig.records(t)
	if len(records.Releases) != 0 {
		t.Fatalf("releases recorded = %+v, want none: the pod still stands", records.Releases)
	}
	if len(records.Holds) != 1 || records.Holds[0].Reason != workflow.IdleHoldUnauthorized.String() {
		t.Fatalf("holds = %+v, want the one %s", records.Holds, workflow.IdleHoldUnauthorized)
	}
	var backstops int
	if err := rig.pool.QueryRow(ctx, `select count(*) from pod_wake_backstops where issue = $1`, rig.issue.Key).Scan(&backstops); err != nil {
		t.Fatal(err)
	}
	if backstops != 0 {
		t.Fatalf("backstops armed = %d, want none: a backstop belongs to a pod that is gone", backstops)
	}
	if rig.marked(t) {
		t.Fatal("the cancelled release left its mark behind")
	}
	if rig.bundled(t) {
		t.Fatal("the cancelled release left its bundle behind")
	}
	if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
		t.Fatalf("the claim = %s by %s, want the cancelled release to have resumed it into the pod it kept",
			got.State, got.SuspendedBy)
	}
}

// A cancel resumes the claims its own release suspended and no others, and the resume is a row
// of the issue's lane rather than a relaunch held in memory: a release that did not finish is
// undone by one transaction that queues those resumes and deletes the mark together.
//
// The pod here holds an architect an earlier release left released, which no wake has brought
// back. Resuming it would put a role into a pod that has no work for it, with no wake and no
// task saying why it is there.
func TestACancelResumesOnlyTheRolesItsOwnReleaseSuspended(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	architect := rig.releasedArchitect(t)

	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	if got := rig.rows(t); len(got) != 3 {
		t.Fatalf("queued rows = %v, want the save, the implementer's stop and the teardown: the architect is already released", got)
	}
	rig.rt.beforeAuthorize = func() {
		if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
			t.Errorf("move the issue's generation on: %v", err)
		}
	}

	rig.drain(t)

	if got := architect.Claim(); !got.Released() {
		t.Fatalf("the architect = %s by %s, want the release that never touched it to have left it released",
			got.State, got.SuspendedBy)
	}
	if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
		t.Fatalf("the implementer = %s by %s, want the claim this release suspended resumed", got.State, got.SuspendedBy)
	}
	if rig.marked(t) {
		t.Fatal("the cancelled release left its mark behind")
	}
}

// The boundary's read belongs to the release and not to its claim: it is the one row that
// answers a task, notice, message or start the pod was given around the mark, so it runs before
// the row returns for a claim with nothing to stop. A boundary that returned first left that
// delivery unanswered, the release ran on and took the pod, and the delivery then cost a wake
// and a pod start to answer what the standing pod would have answered.
func TestABoundaryWhoseClaimRunsNothingStillAnswersADeliveryAroundTheMark(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	boundary := rig.boundary(t)
	// A start for another role of the pod whose transaction was already in flight when the mark
	// was written: below the release's own rows, and not due, so it is still queued when the
	// boundary reads it.
	if _, err := rig.pool.Exec(ctx, `insert into outbox (id, kind, issue, payload, attempts, next_at, last_error)
		values ($1, 'supervise', $2, '{"op":"start","tree":"LEGION-208","role":"tester","generation":1}', 0, now() + interval '1 hour', '')`,
		boundary-2, rig.issue.Key); err != nil {
		t.Fatal(err)
	}
	// The role whose stop is the boundary is retired before that stop runs: it has no process
	// left to stop, which is what used to take the row out before it read the mark.
	if err := rig.machine.Handle(ctx, supervise.RequestStop{Claim: rig.token}); err != nil {
		t.Fatalf("retire the claim the boundary stops: %v", err)
	}

	rig.drain(t)

	if _, suspended := rig.rt.reads(); suspended != 0 {
		t.Fatalf("Sandbox teardowns = %d, want none: the pod is the one that answers the queued start", suspended)
	}
	if rig.bundled(t) {
		t.Fatal("the cancelled release left its bundle behind")
	}
	if rig.marked(t) {
		t.Fatal("the cancelled release left its mark behind")
	}
	records := rig.records(t)
	if len(records.Releases) != 0 {
		t.Fatalf("releases recorded = %+v, want none: the release was cancelled at its boundary", records.Releases)
	}
	if len(records.Holds) != 1 || records.Holds[0].Reason != workflow.IdleHoldOvertaken.String() {
		t.Fatalf("holds = %+v, want the one %s", records.Holds, workflow.IdleHoldOvertaken)
	}
}

// A pod that has had no turn in progress and no turn due for the idle period is released while
// its issue is still in the workflow: the decision writes the save of the workspace's unpushed
// work, one `release` row per live role behind it, and the teardown last. Each claim goes to
// suspended with the source `released` keeping its phase and the task it holds, the bundle is
// saved before any role process is stopped — only a shim can build it — and the Sandbox teardown
// runs last.
func TestAnIdleIssuesPodIsReleasedWithItsClaimsKeepingTheirPhaseAndTask(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()

	rig.w.releaseIdlePods(ctx)
	if got := rig.rows(t); len(got) != 3 || got[0] != "issue_bundle" || got[1] != "supervise:release" || got[2] != "issue_release" {
		t.Fatalf("queued rows = %v, want the save, then the role stop, then the teardown", got)
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

// R4's boundary, through the whole daemon path. The boundary is the stop of the first role
// process of the pod, and until it has run a delivery for a role of the pod cancels the
// release: one queued below the release's own rows was in flight when the mark was written,
// one above them arrived while every role still ran. Either way the roles go on in the same
// pod, nothing is suspended, no Sandbox is torn down and the bundle the save stored goes with
// the release, since the pod keeps that work in its own workspace.
func TestADeliveryThePodIsGivenAroundTheMarkCancelsTheRelease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before bool
	}{
		{name: "queued below the release's rows", before: true},
		{name: "queued above them", before: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			ctx := context.Background()
			rig.w.releaseIdlePods(ctx)
			boundary := rig.boundary(t)

			// Below the boundary means below the release's own first row, the save, which sits
			// one id under the boundary: the delivery's transaction took its id before the
			// release's and committed after. Above it, anywhere after its last row. It is not
			// due yet, so the drain below runs the release's own rows and this row stays
			// queued, which is what the boundary reads.
			id := boundary - 2
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
			if got.State == supervise.StateSuspended {
				t.Fatalf("the claim = %s by %s, want the cancelled release to have left it running", got.State, got.SuspendedBy)
			}
			if _, suspended := rig.rt.reads(); suspended != 0 {
				t.Fatalf("Sandbox teardowns = %d, want none: the pod goes on", suspended)
			}
			if rig.bundled(t) {
				t.Fatal("the cancelled release left its bundle behind")
			}
			if rig.marked(t) {
				t.Fatal("the release's mark outlived it")
			}
			// The pod it kept is recorded under the hold's own word, so a deployment reading
			// the records sees a release that stopped short and why.
			holds := rig.records(t).Holds
			if len(holds) != 1 || holds[0].Reason != workflow.IdleHoldOvertaken.String() {
				t.Fatalf("the holds are %+v, want one %s", holds, workflow.IdleHoldOvertaken)
			}
			// The delivery itself is untouched: it is still queued for its role.
			var left int
			if err := rig.pool.QueryRow(ctx, `select count(*) from outbox where issue = $1
				and payload->>'op' = 'start' and payload->>'role' = 'tester'`, rig.issue.Key).Scan(&left); err != nil {
				t.Fatal(err)
			}
			if left != 1 {
				t.Fatalf("the queued start = %d rows, want it left for its own turn", left)
			}
		})
	}
}

// A turn that began after the release was marked — a direct message, a notice or a person's
// answer reaching a live session — leaves no outbox row at all: what says it happened is the
// claim, working rather than idle. The save stored the workspace from before that turn's edits,
// so the boundary cancels the release and the pod keeps the work in its own workspace.
func TestATurnStartedAfterTheMarkCancelsTheRelease(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePods(ctx)
	if _, err := rig.pool.Exec(ctx, `update claims set state = 'working' where issue = $1`, rig.issue.Key); err != nil {
		t.Fatal(err)
	}

	rig.drain(t)

	if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
		t.Fatalf("the claim = %s by %s, want the cancelled release to have left it in its turn", got.State, got.SuspendedBy)
	}
	if _, suspended := rig.rt.reads(); suspended != 0 {
		t.Fatalf("Sandbox teardowns = %d, want none: the pod is in a turn", suspended)
	}
	if rig.bundled(t) {
		t.Fatal("the cancelled release left behind a bundle taken before the turn's edits")
	}
	if rig.marked(t) {
		t.Fatal("the release's mark outlived it")
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

// A release cancelled because its issue left the workflow queues no resume, and the issue's
// close then deletes its Sandbox, which is what takes the pod's role Secrets — its launcher
// tokens and its `gh` credentials — with it.
//
// The sequence is the one the lane runs. The release's save and role stop have run, so the
// implementer is suspended as `released`; the root is parked under the teardown's pod lock, so
// the linger's own rows — the role suspends and the issue_suspend that deletes the Sandbox —
// are queued above the release's; the teardown then reads the issue as no longer the workflow's
// and cancels. A resume queued for the claim the release suspended would land above the close's
// rows and supersede them: store.IssueSuspension would read a start newer than its own row,
// finish the close without acting, and leave the roles of a parked issue running in a Sandbox
// nothing deletes.
func TestACancelOfAnIssueThatLeftTheWorkflowLetsItsCloseDeleteTheSandbox(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	engine := workflow.New(rig.issues, workflow.Config{Project: "LEGION", Repositories: testRepositories,
		Linger: time.Hour}, quietLogger())
	// The cancel names the roles it queued a resume for, which is the one place that set is
	// read after the fact: a resume the lane has already run leaves no row behind.
	logs := &syncBuffer{}
	rig.runner.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	rig.runner.handlers = []intake.Handler{engine}
	rig.w.releaseIdlePod(ctx, rig.issue.Key)

	// Under the teardown's pod lock, before the store is asked: a person parks the root, which
	// lingers its tree and queues the close's own rows. The close's SuspendIssue runs through
	// the same hook, and parks nothing a second time.
	var parked sync.Once
	rig.rt.beforeAuthorize = func() {
		parked.Do(func() {
			if _, err := intake.ApplyFact(ctx, rig.pool, "dispatch", "park", intake.DispatchIssue{
				Key: rig.issue.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
				Status: "backlog", Title: rig.issue.Title, Rank: rig.issue.Rank}, engine); err != nil {
				t.Errorf("park the root: %v", err)
			}
		})
	}

	rig.drain(t)

	if !strings.Contains(logs.String(), "resumed=[]") {
		t.Fatalf("the cancel queued a resume; want none, since the claims of a parked issue are its close's and a start above the close's rows supersedes them: %s",
			logs.String())
	}
	if _, suspended := rig.rt.reads(); suspended != 1 {
		t.Fatalf("Sandbox teardowns = %d, want the close's one: its role Secrets go with the Sandbox", suspended)
	}
	if rig.marked(t) {
		t.Fatal("the cancelled release left its mark behind")
	}
	if rig.bundled(t) {
		t.Fatal("the cancelled release left its bundle behind")
	}
	var left int
	if err := rig.pool.QueryRow(ctx, `select count(*) from outbox where issue = $1
		and kind in ('supervise', 'issue_suspend', 'issue_release')`, rig.issue.Key).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d stop rows of the pod remain, want the release's and the close's every one finished", left)
	}
	if got := rig.machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByDaemon {
		t.Fatalf("the implementer = %s by %s, want it suspended by the close that took its pod",
			got.State, got.SuspendedBy)
	}
}
