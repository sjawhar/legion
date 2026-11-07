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

	"github.com/jackc/pgx/v5"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

type issueProvisionTokens struct{}

func (issueProvisionTokens) Token(context.Context, ghrepo.Repository) (string, error) {
	return "fixture-provision-token", nil
}

// A close whose role stop is still pending acts on nothing, and the same close, resumed after the
// daemon and its runtime restart, suspends the issue's Sandbox and deletes it: neither the runtime
// nor the outbox keeps the pending effect in memory, and the database owns it. The stopped role's
// session is the store's and outlives the Sandbox, which owned no part of it. The tree stays live
// until its linger closes, and its cleanup then finds the issue's Sandbox already gone.
func TestIssueCloseDeletesTheSandboxAfterARestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	putOutboxIssue(t, pool, records, issue)
	if _, err := st.OpenTreeLifecycle(ctx, "legion", issue.Tree, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	name := "legion-legion-legion-208"
	api := newIssueSandboxAPI(issueSandbox(name, "sandbox-uid", issue.Tree, issue.Key))
	server := httptest.NewServer(api)
	t.Cleanup(func() { cancel(); server.Close() })
	newSupervisorRuntime := func() (*supervisor, context.CancelFunc) {
		t.Helper()
		runtimeCtx, stopRuntime := context.WithCancel(ctx)
		t.Cleanup(stopRuntime)
		reopened, err := store.Open(ctx, pool.Config().ConnString())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(reopened.Close)
		rt, err := sandbox.New(runtimeCtx, sandboxRESTConfig(server.URL), sandbox.Options{
			Namespace: "legion", Project: "legion", Store: reopened, Image: "ghcr.io/example/worker@sha256:" + strings.Repeat("a", 64),
			StorageClass: "standard", IssueVolume: resource.MustParse("1Gi"), StreamURL: "tcp://127.0.0.1:13371", Resources: defaultReservations(t),
			SessionStore: ompsessions.Store{Kind: ompsessions.StorePostgres, DSNSecret: "SESSION_DSN"},
			Tools:        sandbox.Tools{GH: "/usr/bin/gh", Git: "/usr/bin/git", JJ: "/usr/bin/jj", Legion: "/opt/legion/bin/legion", AgentSecrets: "/opt/legion/bin/agent-secrets"},
			BootTimeout:  time.Second, RegistrationIntervals: 3, BusyBackoff: time.Millisecond, TerminationGrace: time.Second, ProbeInterval: time.Hour, AdoptTimeout: time.Second,
			Tokens: issueProvisionTokens{}, GitHubCredential: appauth.GitHubCredential(outboxTokens{}, legionRepos, ghrepo.Renames{}, nil, quietLogger()),
			ControllerCredential: appauth.ControllerCredential(outboxTokens{}, legionRepos, ghrepo.Renames{}, nil, quietLogger()),
			GitHubDirs:           legionDirs, Conns: fake.NewConns(), Log: quietLogger(),
			RepositoryCache: defaultRepositoryCache(t), Mapped: noRepositories,
		})
		if err != nil {
			t.Fatal(err)
		}
		sup := newSupervisor(runtimeCtx, nil, "legion", t.TempDir(), quietLogger())
		sup.deps.Runtime = rt
		t.Cleanup(sup.stop)
		return sup, stopRuntime
	}
	sup, stopRuntime := newSupervisorRuntime()
	clock := time.Now()
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Linger: time.Hour}, quietLogger())
	// newRunner is the outbox over the given supervisor: the restart below builds a second one
	// over the restarted runtime, since the outbox carries locks a copy must not.
	newRunner := func(sup *supervisor) *outbox {
		return &outbox{pool: pool, records: records, project: "legion", dispatchProject: "LEGION", supervisor: sup, trees: st,
			dispatch: &outboxDispatch{issue: dispatch.Issue{Key: issue.Key, Status: "done"}}, notices: &outboxPublisher{},
			handlers: []intake.Handler{engine}, log: quietLogger(), now: func() time.Time { return clock },
		}
	}
	runner := newRunner(sup)
	token := claim.Token("legion-legion-legion-208-planner")
	stored := supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, TreeEpoch: 1, Issue: issue.Key, Role: claim.RolePlanner,
		State: supervise.StateLaunching, Generation: 1, Session: "saved-planner", SessionFile: "/legion/sessions/planner.jsonl"}
	if err := st.PutClaim(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "close", intake.DispatchIssue{Key: issue.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.closed", Status: "done", Title: issue.Title, Rank: "U"}, engine); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := api.mode(name); got != "Running" {
		t.Fatalf("close during launch changed Sandbox to %s", got)
	}
	var pendingStop bool
	if err := pool.QueryRow(ctx, `select exists (select 1 from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'suspend' and payload->>'role' = 'planner')`, issue.Key).Scan(&pendingStop); err != nil || !pendingStop {
		t.Fatalf("close lost the stored launching role's stop before its machine appeared: pending=%t, err=%v", pendingStop, err)
	}
	// Restart the runtime and outbox while the close still waits. Neither has retained watch
	// membership or an in-memory suspension request; the same database owns the pending effect.
	stopRuntime()
	restarted, _ := newSupervisorRuntime()
	runner = newRunner(restarted)
	// The stored role's orderly stop completes independently of the runtime watch population.
	stored.State = supervise.StateSuspended
	if err := st.PutClaim(ctx, stored); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := api.mode(name); got != "deleted" {
		t.Fatalf("Sandbox after issue close = %s, want it suspended and then deleted", got)
	}
	if options, deleted := api.deleted(name); !deleted || options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground {
		t.Fatalf("Sandbox delete = %+v (requested %t), want foreground propagation", options, deleted)
	}
	claims, err := st.Claims(ctx)
	if err != nil || len(claims) != 1 || claims[0].Session != "saved-planner" || claims[0].SessionFile != "/legion/sessions/planner.jsonl" {
		t.Fatalf("close lost retained session: %+v, %v", claims, err)
	}
	if live, err := st.TreeLive(ctx, "legion", issue.Tree); err != nil || !live {
		t.Fatalf("close started tree cleanup: live=%t, err=%v", live, err)
	}
	stored.State = supervise.StateRetired
	if err := st.PutClaim(ctx, stored); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Hour)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := api.mode(name); got != "deleted" {
		t.Fatalf("Sandbox after linger cleanup = %s, want it still gone", got)
	}
	if live, err := st.TreeLive(ctx, "legion", issue.Tree); err != nil || live {
		t.Fatalf("tree after its linger cleanup is live = %t, %v; want its cleanup confirmed", live, err)
	}
}

// issueLaunchSpecs builds a launch the Sandbox runtime accepts for any workflow claim: a role prompt
// on disk and the issue's repository; the machine fills in the claim, its generation, its boot token
// and the session it resumes.
type issueLaunchSpecs struct{ prompt string }

func (s issueLaunchSpecs) SpawnSpec(context.Context, supervise.Claim) (runtime.SpawnSpec, error) {
	return runtime.SpawnSpec{
		Env:           map[string]string{"JJ_USER": "legion-implement[bot]"},
		Prompt:        runtime.PromptParts{RolePromptPaths: []string{s.prompt}},
		Repository:    ghrepo.MustParse("acme/widgets"),
		DefaultBranch: "main",
	}, nil
}

// A child closed as done gives its Sandbox back at once while its parent keeps running: the
// close's issue_close rows retire the child's claims with their sessions dropped, and its
// issue_suspend row foreground-deletes the child's Sandbox — taking what it owns with it, with no
// PVC request from the restricted daemon; the root's Sandbox stays Running. A child parked in
// backlog keeps its claims suspended with their sessions, and its Sandbox goes the same way,
// since the pod it ran in, and the disk with it, go either way. The done child set back to todo
// launches fresh because its resume path was cleared, while its session id stays as reaping
// evidence until the volume is gone.
func TestAChildClosedDoneDeletesItsSandboxWhileItsParentRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	root := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	parent := root.Key
	done := record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root.Key, Parent: &parent, Title: "done child", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "V", LastDispatchSeq: 1}
	parked := record.Issue{Key: "LEGION-210", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root.Key, Parent: &parent, Title: "parked child", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "W", LastDispatchSeq: 1}
	for _, issue := range []record.Issue{root, done, parked} {
		putOutboxIssue(t, pool, records, issue)
	}
	approved := 1
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: root.Key, Index: 0, AdmittedAt: time.Now()}); err != nil {
			return err
		}
		return records.PutGate(ctx, tx, record.DesignGate{Issue: root.Key, ArtifactID: "artifact-208", LatestVersion: 1, ApprovedVersion: &approved})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OpenTreeLifecycle(ctx, "legion", root.Key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	rootSandbox, doneSandbox, parkedSandbox := "legion-legion-legion-208", "legion-legion-legion-209", "legion-legion-legion-210"
	api := newIssueSandboxAPI(
		issueSandbox(rootSandbox, "sandbox-uid-root", root.Key, root.Key),
		issueSandbox(doneSandbox, "sandbox-uid-done", root.Key, done.Key),
		issueSandbox(parkedSandbox, "sandbox-uid-parked", root.Key, parked.Key),
	)
	server := httptest.NewServer(api)
	t.Cleanup(func() { cancel(); server.Close() })
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	runtimeCtx, stopRuntime := context.WithCancel(ctx)
	t.Cleanup(stopRuntime)
	rt, err := sandbox.New(runtimeCtx, sandboxRESTConfig(server.URL), sandbox.Options{
		Namespace: "legion", Project: "legion", Store: st, Image: "ghcr.io/example/worker@sha256:" + strings.Repeat("a", 64),
		StorageClass: "standard", IssueVolume: resource.MustParse("1Gi"), StreamURL: "tcp://127.0.0.1:13371", Resources: defaultReservations(t),
		SessionStore: ompsessions.Store{Kind: ompsessions.StorePostgres, DSNSecret: "SESSION_DSN"},
		Tools:        sandbox.Tools{GH: "/usr/bin/gh", Git: "/usr/bin/git", JJ: "/usr/bin/jj", Legion: "/opt/legion/bin/legion", AgentSecrets: "/opt/legion/bin/agent-secrets"},
		BootTimeout:  300 * time.Millisecond, RegistrationIntervals: 3, BusyBackoff: time.Millisecond, TerminationGrace: 100 * time.Millisecond, ProbeInterval: time.Hour, AdoptTimeout: time.Second,
		Tokens: issueProvisionTokens{}, GitHubCredential: appauth.GitHubCredential(outboxTokens{}, legionRepos, ghrepo.Renames{}, nil, logger),
		ControllerCredential: appauth.ControllerCredential(outboxTokens{}, legionRepos, ghrepo.Renames{}, nil, logger),
		GitHubDirs:           legionDirs, Conns: fake.NewConns(), Log: logger,
		RepositoryCache: defaultRepositoryCache(t), Mapped: noRepositories,
	})
	if err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(t.TempDir(), "role.md")
	if err := os.WriteFile(prompt, []byte("You are the role.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sup := newSupervisor(runtimeCtx, st, "legion", t.TempDir(), logger)
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: fake.NewConns(), Store: st, Specs: issueLaunchSpecs{prompt: prompt}, Clock: stillClock{}, Log: logger,
		Settings: supervise.FixedSettings(
			supervise.Limits{LaunchFailures: 1, PromptFailures: 2, PromptRetires: 2},
			supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond}),
		PluginContract: 24,
	}
	t.Cleanup(sup.stop)
	// The root's architect works on, with its session, and its implementer works the implementing
	// phase the root stands in: an issue that holds an admission slot has a worker with the task
	// of its phase, and admission's pass starts one again for a slot whose worker has none
	// (admit's resumeSlottedWorkers). Each child has one role stopped with its session retained,
	// as a phase's end leaves it, supervised by this daemon.
	architect := supervise.Claim{Token: "legion-legion-legion-208-architect", Project: "legion", Tree: root.Key, TreeEpoch: 1, Issue: root.Key, Role: claim.RoleArchitect,
		State: supervise.StateWorking, Generation: 1, Session: "saved-architect", SessionFile: "/legion/sessions/architect.jsonl"}
	implementer := supervise.Claim{Token: "legion-legion-legion-208-implementer", Project: "legion", Tree: root.Key, TreeEpoch: 1, Issue: root.Key, Role: claim.RoleImplementer,
		State: supervise.StateWorking, Generation: 1, Session: "saved-implementer", SessionFile: "/legion/sessions/implementer.jsonl",
		Pending: &supervise.Delivery{ID: "outbox:1", Task: "Continue root. Issue: LEGION-208. Phase: implementing.", Generation: 1,
			Phase: phase.Implementing, QueuedAt: time.Now(), DeliveredAt: time.Now(), ConfirmedAt: time.Now()}}
	planner := supervise.Claim{Token: "legion-legion-legion-209-planner", Project: "legion", Tree: root.Key, TreeEpoch: 1, Issue: done.Key, Role: claim.RolePlanner,
		State: supervise.StateSuspended, Generation: 1, Session: "saved-planner", SessionFile: "/legion/sessions/planner.jsonl"}
	tester := supervise.Claim{Token: "legion-legion-legion-210-tester", Project: "legion", Tree: root.Key, TreeEpoch: 1, Issue: parked.Key, Role: claim.RoleTester,
		State: supervise.StateSuspended, Generation: 1, Session: "saved-tester", SessionFile: "/legion/sessions/tester.jsonl"}
	for _, c := range []supervise.Claim{architect, implementer, planner, tester} {
		if err := st.PutClaim(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sup.restore(ctx, []supervise.Claim{planner, tester}); err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Linger: time.Hour}, logger)
	admission := admit.New(records, engine, 2, "LEGION", logger)
	runner := &outbox{pool: pool, records: records, project: "legion", dispatchProject: "LEGION", supervisor: sup, trees: st, tokens: outboxTokens{},
		stateDir: t.TempDir(), githubAPI: newBranchGitHub(t, nil, branchExists).url,
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: done.Key, Status: "todo"}}, notices: &outboxPublisher{},
		handlers: []intake.Handler{engine, admission}, log: logger, now: func() time.Time { return clock },
	}
	run := func(what string) {
		t.Helper()
		clock = clock.Add(2 * time.Second)
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	storedClaim := func(token claim.Token) supervise.Claim {
		t.Helper()
		claims, err := st.Claims(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range claims {
			if c.Token == token {
				return c
			}
		}
		t.Fatalf("no stored claim %s", token)
		return supervise.Claim{}
	}
	effectsLeft := func(issue string) int {
		t.Helper()
		var left int
		if err := pool.QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind in ('supervise', 'issue_suspend')`, issue).Scan(&left); err != nil {
			t.Fatal(err)
		}
		return left
	}

	// (a) The done child: its claims closed, its Sandbox and its pod's disk gone, its parent untouched.
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "child-done", intake.DispatchIssue{Key: done.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.closed", Status: "done", Title: done.Title, Parent: root.Key, Rank: "V"}, engine, admission); err != nil {
		t.Fatal(err)
	}
	run("the done child's close")
	if got := api.mode(doneSandbox); got != "deleted" {
		t.Fatalf("done child's Sandbox after its close = %s, want deleted", got)
	}
	options, deleted := api.deleted(doneSandbox)
	if !deleted || options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground {
		t.Fatalf("done child's Sandbox delete = %+v (requested %t), want foreground propagation", options, deleted)
	}
	if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "sandbox-uid-done" {
		t.Fatalf("done child's Sandbox delete preconditions = %+v, want fenced to uid sandbox-uid-done", options.Preconditions)
	}
	if api.disk(doneSandbox) {
		t.Fatal("the done child's pod's disk survived its Sandbox's foreground delete")
	}
	if got := api.mode(rootSandbox); got != "Running" || !api.disk(rootSandbox) || api.uid(rootSandbox) != "sandbox-uid-root" {
		t.Fatalf("root Sandbox after the child's close = %s (disk %t, uid %s), want the same Sandbox Running with its pod and that pod's disk", got, api.disk(rootSandbox), api.uid(rootSandbox))
	}
	if _, touched := api.deleted(rootSandbox); touched || api.pvcRequests != 0 {
		t.Fatalf("the child's close deleted the root's Sandbox (%t) or sent %d PVC requests, want neither", touched, api.pvcRequests)
	}
	if got := storedClaim(planner.Token); got.State != supervise.StateRetired || got.Session != planner.Session || got.SessionFile != "" || got.Locator != nil {
		t.Fatalf("done child's planner after the close = %+v, want retired with session evidence, no resume path, and no locator", got)
	}
	if got := storedClaim(architect.Token); got.State != supervise.StateWorking || got.Session != "saved-architect" {
		t.Fatalf("root architect after the child's close = %+v, want it working with its session", got)
	}
	if left := effectsLeft(done.Key); left != 0 {
		t.Fatalf("%d supervise/issue_suspend rows of the done child remain, want every one finished", left)
	}
	if live, err := st.TreeLive(ctx, "legion", root.Key); err != nil || !live {
		t.Fatalf("tree live after the child's close = %t, %v; want the tree still live", live, err)
	}
	if !strings.Contains(logs.String(), "deleted the closed issue's Sandbox") {
		t.Fatal("the close logged no line naming the issue's Sandbox")
	}

	// (c) The parked child: its claims suspended with their sessions, and its Sandbox deleted as
	// the done child's was, since its pod and the disk under it go either way.
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "child-backlog", intake.DispatchIssue{Key: parked.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "backlog", Title: parked.Title, Parent: root.Key, Rank: "W"}, engine, admission); err != nil {
		t.Fatal(err)
	}
	run("the parked child's move to backlog")
	if got := api.mode(parkedSandbox); got != "deleted" {
		t.Fatalf("parked child's Sandbox = %s, want it suspended and then deleted", got)
	}
	if options, deleted := api.deleted(parkedSandbox); !deleted || options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground {
		t.Fatalf("parked child's Sandbox delete = %+v (requested %t), want foreground propagation", options, deleted)
	}
	if got := storedClaim(tester.Token); got.State != supervise.StateSuspended || got.Session != "saved-tester" || got.SessionFile != "/legion/sessions/tester.jsonl" {
		t.Fatalf("parked child's tester = %+v, want it suspended with its session kept", got)
	}
	if left := effectsLeft(parked.Key); left != 0 {
		t.Fatalf("%d supervise/issue_suspend rows of the parked child remain, want every one finished", left)
	}

	// (b) The done child set back to todo re-enters under its live tree and its planner starts
	// again: a fresh spawn into a new Sandbox whose workspace-init expects no volume. The fixture
	// runs no controller, so the launch ends at its wait for a pod; what it asked of the API is
	// what the assertion reads.
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "child-todo", intake.DispatchIssue{Key: done.Key, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "todo", Title: done.Title, Parent: root.Key, Rank: "V"}, engine, admission); err != nil {
		t.Fatal(err)
	}
	run("the re-entered child's branch and status")
	run("the re-entered child's planner start")
	if !slices.Contains(api.created, doneSandbox) {
		t.Fatalf("the re-entered child's start created %v, want a new Sandbox %s", api.created, doneSandbox)
	}
	if uid := api.uid(doneSandbox); uid == "" || uid == "sandbox-uid-done" {
		t.Fatalf("re-entered child's Sandbox uid = %q, want one other than the Sandbox its close deleted", uid)
	}
	env := api.initEnv(doneSandbox)
	if !slices.Contains(env, "PATH") {
		t.Fatalf("the re-entered child's workspace-init environment %v names no PATH; the Running patch wrote no pod template", env)
	}
	if got := storedClaim(planner.Token); got.SessionFile != "" {
		t.Fatalf("re-entered child's planner = %+v, want a fresh launch with no session to resume", got)
	}
}
