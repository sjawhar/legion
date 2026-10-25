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
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
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
// reaches the runtime before the new tree's start does. The cleanup deletes the child's Sandbox
// with the rest of the tree's — there is no disk to carry, since an issue pod's disk went with
// that pod — and the new tree's start then creates a Sandbox of the child's own and resumes the
// role's session, which the store holds, in a pod of its own.
func TestAnOldTreesCleanupBeforeTheNewTreesStartDeletesTheCarriedChildsSandbox(t *testing.T) {
	f := newCarriedCleanup(t, true)
	ctx, st, api, logs, childKey, childSandbox, implementer := f.ctx, f.st, f.api, f.logs, f.childKey, f.childSandbox, f.implementer
	f.cleanupAndSweep()

	// Then the new tree's start of the implementer. The fixture runs no controller, so the launch
	// creates the Sandbox, writes its pod template and Running patch into it, then ends at its wait
	// for a pod; what it wrote is what the assertions read.
	clock := time.Now()
	runner := &outbox{pool: f.pool, records: f.records, project: "legion", dispatchProject: "LEGION", supervisor: f.sup, trees: st, tokens: outboxTokens{},
		stateDir: t.TempDir(), githubAPI: newBranchGitHub(t, nil, branchExists).url,
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: childKey, Status: "todo"}}, notices: &outboxPublisher{},
		log: f.logger, now: func() time.Time { return clock },
	}
	enqueueOutbox(t, f.pool, f.records, mustOutboxRow(t, childKey, record.SuperviseRequest{Op: "start", Tree: childKey, Role: claim.RoleImplementer,
		Generation: 2, Phase: phase.Implementing, Task: "Issue: " + childKey + ". Phase: implementing. Resume the existing phase work.", ResumeTask: true}, clock))
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("the new tree's start: %v", err)
	}
	if !slices.Contains(api.created, childSandbox) || api.uid(childSandbox) == "" || api.uid(childSandbox) == "sandbox-uid-child" {
		t.Fatalf("the new tree's start created %v and left Sandbox uid %q; want a Sandbox of its own in place of the deleted one",
			api.created, api.uid(childSandbox))
	}
	if got := api.label(childSandbox, "legion.dev/tree"); got != childKey {
		t.Fatalf("the carried child's Sandbox after the new tree's start is under tree %q, want %s", got, childKey)
	}
	if env := api.initEnv(childSandbox); !slices.Contains(env, "PATH") {
		t.Fatalf("the implementer's resume wrote no pod template into the Sandbox it created (init env %v)", env)
	}
	if !strings.Contains(logs.String(), "wait for its new pod") {
		t.Fatalf("the implementer's resume did not reach its wait for a new pod:\n%s", logs.String())
	}
	stored, err := st.Claims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range stored {
		if c.Token == implementer.Token && (c.Tree != childKey || c.SessionFile != implementer.SessionFile) {
			t.Fatalf("the implementer after the new tree's start = %+v; want it in %s with its session file", c, childKey)
		}
	}
}

// A carried child whose own tree's launch has created a Sandbox for it keeps that Sandbox while
// it waits in admission's line, though no lifecycle of its tree is open yet: the orphan sweep,
// reading the real store, leaves it (Store.IssueSandboxHome), since a pod of the child's may
// already be running in it. Once the child leaves the line (a person sets it to backlog) it can
// no longer be admitted into that tree, so the Sandbox belongs nowhere and the sweep deletes it,
// fenced and logged, as every other orphan is.
func TestTheSweepDeletesACarriedChildsOwnSandboxOnceItLeavesTheLine(t *testing.T) {
	f := newCarriedCleanup(t, false)
	f.cleanupAndSweep()
	f.launchedChildSandbox("sandbox-uid-child-own")
	if err := f.newRuntime().ReconcileOrphans(f.ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	if got := f.api.uid(f.childSandbox); got != "sandbox-uid-child-own" {
		t.Fatalf("the Sandbox of the child waiting in the line = %q, want the one its own tree's launch created", got)
	}
	child := f.child
	child.Status, child.DispatchStatus = "backlog", "backlog"
	putOutboxIssue(t, f.pool, f.records, child)
	if err := f.newRuntime().ReconcileOrphans(f.ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	if got := f.api.mode(f.childSandbox); got != "deleted" {
		t.Fatalf("the carried child's Sandbox after it left the line = %s, want it deleted", got)
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
	root := record.Issue{Key: rootKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: rootKey, Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A",
		LingerUntil: &expired, LastDispatchSeq: 1}
	child := record.Issue{Key: childKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: childKey, Parent: &parent, Title: "child", Phase: phase.Implementing, Generation: 2,
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
			SessionStore: ompsessions.Store{Kind: ompsessions.StorePostgres, DSNSecret: "SESSION_DSN"},
			Tools:        sandbox.Tools{GH: "/usr/bin/gh", Git: "/usr/bin/git", JJ: "/usr/bin/jj", Legion: "/opt/legion/bin/legion", AgentSecrets: "/opt/legion/bin/agent-secrets"},
			BootTimeout:  300 * time.Millisecond, RegistrationIntervals: 3, BusyBackoff: time.Millisecond, TerminationGrace: 100 * time.Millisecond, ProbeInterval: time.Hour, AdoptTimeout: time.Second,
			Tokens: issueProvisionTokens{}, GitHubCredential: appauth.GitHubCredential(outboxTokens{}, legionOwner), Conns: fake.NewConns(), Log: logger,
			RepositoryCache: defaultRepositoryCache(t), Mapped: noRepositories,
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

// cleanupAndSweep runs the old tree's cleanup, which reaches the runtime before the new tree's
// start does, then the orphan sweep on a runtime that lists the Sandboxes as the cleanup left
// them: the old root's Sandbox and the carried child's are both deleted, with everything each
// owned, and the sweep finds nothing of the tree left to judge.
func (f *carriedCleanup) cleanupAndSweep() {
	t, api := f.t, f.api
	t.Helper()
	if err := f.st.CleanupReservedTree(f.ctx, "legion", f.rootKey, f.lifecycle.Epoch, f.rt); err != nil {
		t.Fatalf("the old tree's cleanup: %v", err)
	}
	for _, name := range []string{f.rootSandbox, f.childSandbox} {
		if got := api.mode(name); got != "deleted" || api.pvc(name) {
			t.Fatalf("Sandbox %s after the cleanup = %s (pvc %t), want it deleted with everything it owned", name, got, api.pvc(name))
		}
	}
	if live, err := f.st.TreeLive(f.ctx, "legion", f.rootKey); err != nil || live {
		t.Fatalf("the old tree after its cleanup is live = %t, %v; want its cleanup confirmed", live, err)
	}
	if err := f.newRuntime().ReconcileOrphans(f.ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.logs.String(), "orphaned sandbox") {
		t.Fatalf("the orphan sweep found something of the cleaned-up tree to act on:\n%s", f.logs.String())
	}
}

// launchedChildSandbox puts the Sandbox the child's own tree's launch creates into the API, under
// the child's tree and owning no volume claim template, as this runtime now makes one for an
// issue: the launch itself needs a controller to finish, which the fixture has none of, and what
// the sweep judges is the Sandbox the API holds.
func (f *carriedCleanup) launchedChildSandbox(uid string) {
	f.t.Helper()
	s := issueSandbox(f.childSandbox, uid, f.childKey, f.childKey)
	delete(s["spec"].(map[string]any), "volumeClaimTemplates")
	f.api.mu.Lock()
	defer f.api.mu.Unlock()
	f.api.objects[f.childSandbox] = s
}
