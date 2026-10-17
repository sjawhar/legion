package daemon

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/rest"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// At the old root's linger expiry the new tree's hold on a carried child's role and the old tree's
// close come due together: the hold's deadline suspends the role, its session kept, and the old
// tree's cleanup, which no longer waits on a claim that runs nothing and whose issue has moved,
// reaches the runtime before the new tree's start does. The cleanup hands the child's Sandbox over
// to the child's own tree instead of deleting it: the same Sandbox and the volume it owns, relabelled
// and Suspended. The orphan sweep keeps it, and the new tree's start then resumes the role's session
// in that Sandbox, on that volume, expecting the volume to hold it. Which of the cleanup and the
// start comes first decides nothing.
func TestAnOldTreesCleanupBeforeTheNewTreesStartHandsTheCarriedChildsSandboxOver(t *testing.T) {
	f := newCarriedCleanup(t, true)
	ctx, st, api, logs, childKey, childSandbox, implementer := f.ctx, f.st, f.api, f.logs, f.childKey, f.childSandbox, f.implementer
	f.cleanupAndSweep()

	// Then the new tree's start of the implementer. The fixture runs no controller, so the launch
	// writes its pod template and Running patch into the Sandbox it found, then ends at its wait for
	// a pod, which suspends the Sandbox again; what it wrote is what the assertions read.
	clock := time.Now()
	runner := &outbox{pool: f.pool, records: f.records, project: "legion", dispatchProject: "LEGION", supervisor: f.sup, trees: st, tokens: outboxTokens{},
		stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets"), githubAPI: newBranchGitHub(t, nil, branchExists).url, defaultBranch: mainBranch,
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: childKey, Status: "todo"}}, notices: &outboxPublisher{},
		log: f.logger, now: func() time.Time { return clock },
	}
	enqueueOutbox(t, f.pool, f.records, mustOutboxRow(t, childKey, record.SuperviseRequest{Op: "start", Tree: childKey, Role: claim.RoleImplementer,
		Generation: 2, Phase: phase.Implementing, Task: "Issue: " + childKey + ". Phase: implementing. Resume the existing phase work.", ResumeTask: true}, clock))
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("the new tree's start: %v", err)
	}
	if slices.Contains(api.created, childSandbox) || api.uid(childSandbox) != "sandbox-uid-child" || !api.pvc(childSandbox) {
		t.Fatalf("the new tree's start created %v and left Sandbox uid %q (pvc %t); want sandbox-uid-child kept with its volume",
			api.created, api.uid(childSandbox), api.pvc(childSandbox))
	}
	if got := api.label(childSandbox, "legion.dev/tree"); got != childKey {
		t.Fatalf("the carried child's Sandbox after the new tree's start is under tree %q, want %s", got, childKey)
	}
	if env := api.initEnv(childSandbox); !slices.Contains(env, "LEGION_EXPECT_ISSUE_VOLUME") {
		t.Fatalf("the implementer's resume wrote no pod template into the kept Sandbox expecting its volume to hold the session (init env %v)", env)
	}
	if !strings.Contains(logs.String(), "wait for its new pod") {
		t.Fatalf("the implementer's resume did not reach its wait for a new pod of the kept Sandbox:\n%s", logs.String())
	}
	stored, err := st.Claims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range stored {
		if c.Token == implementer.Token && (c.Tree != childKey || c.SessionFile != implementer.SessionFile || c.WorkspaceLost) {
			t.Fatalf("the implementer after the new tree's start = %+v; want it in %s with its session file and no loss", c, childKey)
		}
	}
	if strings.Contains(logs.String(), "volume was lost") {
		t.Fatalf("the carried child's resume reported a lost volume:\n%s", logs.String())
	}
}

// A carried child the old tree's cleanup handed its Sandbox to before admission opened its tree
// keeps that Sandbox while it waits in admission's line: the orphan sweep, reading the real store,
// leaves it. Once the child leaves the line (a person sets it to backlog) it can no longer be
// admitted into that tree, so the Sandbox belongs nowhere and the sweep deletes it with its volume,
// fenced and logged, as every other orphan is.
func TestTheSweepDeletesACarriedChildsHandedOverSandboxOnceItLeavesTheLine(t *testing.T) {
	f := newCarriedCleanup(t, false)
	f.cleanupAndSweep()
	child := f.child
	child.Status, child.DispatchStatus = "backlog", "backlog"
	putOutboxIssue(t, f.pool, f.records, child)
	if err := f.newRuntime().ReconcileOrphans(f.ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, deleted := f.api.deleted(f.childSandbox); !deleted || f.api.pvc(f.childSandbox) {
		t.Fatalf("the carried child's Sandbox after it left the line: deleted %t, pvc %t; want it deleted with its volume", deleted, f.api.pvc(f.childSandbox))
	}
	if !strings.Contains(f.logs.String(), "deleted an orphaned sandbox") {
		t.Fatalf("the sweep logged no delete of the carried child's Sandbox:\n%s", f.logs.String())
	}
}

// carriedCleanup is the state a done root's linger expiry leaves a carried child in: the old root's
// architect retired by the close, the old tree's cleanup reserved, and the child's implementer
// suspended by the hold's deadline in its old tree, its session kept and not yet re-pointed, since
// the new tree's start has not run. The child waits in todo, handed over, at generation 2 of its own
// tree, which admission has opened or not (opened). Each Sandbox is on the real runtime against a
// fake API, with the real store. The fake API serves a watch its initial events alone, so a runtime
// sees a Sandbox as the API holds it only from its own first listing (newRuntime).
type carriedCleanup struct {
	t                         *testing.T
	ctx                       context.Context
	pool                      *pgxpool.Pool
	st                        *store.Store
	records                   record.Store
	api                       *issueSandboxAPI
	rt                        *sandbox.Runtime
	newRuntime                func() *sandbox.Runtime
	sup                       *supervisor
	logs                      *syncBuffer
	logger                    *slog.Logger
	lifecycle                 treelifecycle.Lifecycle
	rootKey, childKey         string
	rootSandbox, childSandbox string
	child                     record.Issue
	implementer               supervise.Claim
}

func newCarriedCleanup(t *testing.T, opened bool) *carriedCleanup {
	t.Helper()
	const rootKey, childKey = "LEGION-207", "LEGION-208"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	expired := time.Now().Add(-time.Minute)
	parent := rootKey
	root := record.Issue{Key: rootKey, Project: "LEGION", Tree: rootKey, Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A",
		LingerUntil: &expired, LastDispatchSeq: 1}
	child := record.Issue{Key: childKey, Project: "LEGION", Tree: childKey, Parent: &parent, Title: "child", Phase: phase.Implementing, Generation: 2,
		Status: "todo", DispatchStatus: "todo", HandedOver: true, Rank: "B", LastDispatchSeq: 2}
	for _, issue := range []record.Issue{root, child} {
		putOutboxIssue(t, pool, records, issue)
	}
	trees := []string{rootKey}
	if opened {
		trees = append(trees, childKey)
	}
	for _, tree := range trees {
		if _, err := st.OpenTreeLifecycle(ctx, "legion", tree, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", tree, err)
		}
	}
	lifecycle, reserved, err := st.ReserveWorkflowTreeCleanup(ctx, "legion", rootKey, 1)
	if err != nil || !reserved {
		t.Fatalf("reserve the old tree's cleanup = %+v, %t, %v; want it reserved", lifecycle, reserved, err)
	}
	// The child's Sandbox, left Running by its old tree, and the old root's own.
	rootSandbox, childSandbox := "legion-legion-legion-207", "legion-legion-legion-208"
	api := newIssueSandboxAPI(issueSandbox(rootSandbox, "sandbox-uid-root", rootKey, rootKey), issueSandbox(childSandbox, "sandbox-uid-child", rootKey, childKey))
	server := httptest.NewServer(api)
	t.Cleanup(func() { cancel(); server.Close() })
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	runtimeCtx, stopRuntime := context.WithCancel(ctx)
	t.Cleanup(stopRuntime)
	newRuntime := func() *sandbox.Runtime {
		t.Helper()
		rt, err := sandbox.New(runtimeCtx, &rest.Config{Host: server.URL}, sandbox.Options{
			Namespace: "legion", Project: "legion", Store: st, Image: "ghcr.io/example/worker@sha256:" + strings.Repeat("a", 64),
			StorageClass: "standard", IssueVolume: resource.MustParse("1Gi"), StreamURL: "tcp://127.0.0.1:13371", Resources: defaultReservations(t),
			Tools:       sandbox.Tools{GH: "/usr/bin/gh", Git: "/usr/bin/git", JJ: "/usr/bin/jj", Legion: "/opt/legion/bin/legion", AgentSecrets: "/opt/legion/bin/agent-secrets"},
			BootTimeout: 300 * time.Millisecond, BusyBackoff: time.Millisecond, TerminationGrace: 100 * time.Millisecond, ProbeInterval: time.Hour, AdoptTimeout: time.Second,
			Tokens: issueProvisionTokens{}, GitHubCredential: appauth.GitHubCredential(outboxTokens{}, legionOwner), Conns: fake.NewConns(), Log: logger,
		})
		if err != nil {
			t.Fatal(err)
		}
		return rt
	}
	rt := newRuntime()
	prompt := filepath.Join(t.TempDir(), "role.md")
	if err := os.WriteFile(prompt, []byte("You are the role.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sup := newSupervisor(runtimeCtx, st, "legion", t.TempDir(), logger)
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: fake.NewConns(), Store: st, Specs: issueLaunchSpecs{prompt: prompt}, Clock: stillClock{}, Log: logger,
		VolumeLost:     sup.volumeLost,
		Limits:         supervise.Limits{LaunchFailures: 1, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: 24,
	}
	t.Cleanup(sup.stop)
	architect := supervise.Claim{Token: "legion-legion-legion-207-architect", Project: "legion", Tree: rootKey, TreeEpoch: 1, Issue: rootKey,
		Role: claim.RoleArchitect, State: supervise.StateRetired, Generation: 1}
	implementer := supervise.Claim{Token: "legion-legion-legion-208-implementer", Project: "legion", Tree: rootKey, TreeEpoch: 1, Issue: childKey,
		Role: claim.RoleImplementer, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon, Generation: 1,
		Session: "saved-implementer", SessionFile: "/home/legion/.omp/profiles/legion/agent/sessions/--legion-workspaces-acme-widgets-legion-208--/implementer.jsonl"}
	for _, c := range []supervise.Claim{architect, implementer} {
		if err := st.PutClaim(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sup.restore(ctx, []supervise.Claim{implementer}); err != nil {
		t.Fatal(err)
	}
	return &carriedCleanup{t: t, ctx: ctx, pool: pool, st: st, records: records, api: api, rt: rt, newRuntime: newRuntime, sup: sup, logs: logs,
		logger: logger, lifecycle: lifecycle, rootKey: rootKey, childKey: childKey, rootSandbox: rootSandbox, childSandbox: childSandbox,
		child: child, implementer: implementer}
}

// cleanupAndSweep runs the old tree's cleanup, which reaches the runtime before the new tree's start
// does, then the orphan sweep on a runtime that lists the Sandboxes as the cleanup left them: the
// old root's Sandbox is deleted with its volume, and the carried child's is handed over to the
// child's own tree and kept by the sweep's rule, with its volume.
func (f *carriedCleanup) cleanupAndSweep() {
	t, api, childSandbox := f.t, f.api, f.childSandbox
	t.Helper()
	if err := f.st.CleanupReservedTree(f.ctx, "legion", f.rootKey, f.lifecycle.Epoch, f.rt); err != nil {
		t.Fatalf("the old tree's cleanup: %v", err)
	}
	if got := api.mode(f.rootSandbox); got != "deleted" || api.pvc(f.rootSandbox) {
		t.Fatalf("the old root's Sandbox after the cleanup = %s (pvc %t), want it deleted with its volume", got, api.pvc(f.rootSandbox))
	}
	if _, deleted := api.deleted(childSandbox); deleted || api.uid(childSandbox) != "sandbox-uid-child" || !api.pvc(childSandbox) ||
		api.label(childSandbox, "legion.dev/tree") != f.childKey || api.mode(childSandbox) != "Suspended" {
		t.Fatalf("the carried child's Sandbox after the old tree's cleanup = %s, uid %q, tree %q, pvc %t, deleted %t; want sandbox-uid-child handed over to %s, Suspended, with its volume",
			api.mode(childSandbox), api.uid(childSandbox), api.label(childSandbox, "legion.dev/tree"), api.pvc(childSandbox), deleted, f.childKey)
	}
	if live, err := f.st.TreeLive(f.ctx, "legion", f.rootKey); err != nil || live {
		t.Fatalf("the old tree after its cleanup is live = %t, %v; want its cleanup confirmed", live, err)
	}
	if !strings.Contains(f.logs.String(), "handed a carried issue's sandbox over to its new tree") {
		t.Fatalf("the cleanup logged no hand-over:\n%s", f.logs.String())
	}
	if err := f.newRuntime().ReconcileOrphans(f.ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, deleted := api.deleted(childSandbox); deleted || api.uid(childSandbox) != "sandbox-uid-child" || !api.pvc(childSandbox) {
		t.Fatal("the orphan sweep after the cleanup deleted the carried child's Sandbox")
	}
	if strings.Contains(f.logs.String(), "orphaned sandbox") || strings.Contains(f.logs.String(), "written since its delete was decided") {
		t.Fatalf("the orphan sweep decided to delete the handed-over Sandbox:\n%s", f.logs.String())
	}
}
