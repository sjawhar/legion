package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
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

// issueSandboxAPI is the Kubernetes API as the daemon's issue closes see it: the project's
// Sandboxes by name, each with the volume claim it owns, and no pod — there is no controller, so a
// Sandbox's operatingMode is the daemon's request to the real Sandbox controller, not a claim about
// that controller's pods. Every Sandbox delete is recorded with its options and, with foreground
// propagation, takes the PVC the Sandbox owns as garbage collection would; a create gives the new
// Sandbox a uid and is recorded; a patch applies the daemon's JSON patch; role Secrets are accepted
// and forgotten; and every PVC request is counted, since the restricted daemon identity has none.
type issueSandboxAPI struct {
	mu      sync.Mutex
	objects map[string]map[string]any
	// pvcs are the volume claims the Sandboxes own, by Sandbox name, while they exist.
	pvcs        map[string]bool
	deletes     map[string]metav1.DeleteOptions
	created     []string
	pvcRequests int
	uids        int
}

// issueSandbox is a Running issue Sandbox of tree and issue as the API holds it, owning its volume.
func issueSandbox(name, uid, tree, issue string) map[string]any {
	return map[string]any{
		"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox",
		"metadata": map[string]any{"name": name, "namespace": "legion", "uid": uid, "resourceVersion": "1", "generation": float64(1),
			"labels": map[string]any{"legion.dev/project": "legion", "legion.dev/tree": tree, "legion.dev/issue": issue}},
		"spec": map[string]any{"operatingMode": "Running", "volumeClaimTemplates": []any{map[string]any{"metadata": map[string]any{"name": "issue"}}}},
	}
}

func newIssueSandboxAPI(sandboxes ...map[string]any) *issueSandboxAPI {
	a := &issueSandboxAPI{objects: map[string]map[string]any{}, pvcs: map[string]bool{}, deletes: map[string]metav1.DeleteOptions{}}
	for _, s := range sandboxes {
		name := s["metadata"].(map[string]any)["name"].(string)
		a.objects[name], a.pvcs[name] = s, true
	}
	return a
}

func (a *issueSandboxAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "true" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.URL.Query().Get("sendInitialEvents") == "true" {
			a.mu.Lock()
			version, kind := "v1", "Pod"
			if strings.HasSuffix(r.URL.Path, "/sandboxes") {
				version, kind = "agents.x-k8s.io/v1beta1", "Sandbox"
				for _, object := range a.objects {
					_ = json.NewEncoder(w).Encode(map[string]any{"type": "ADDED", "object": object})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "BOOKMARK", "object": map[string]any{
				"apiVersion": version, "kind": kind, "metadata": map[string]any{"resourceVersion": "1",
					"annotations": map[string]any{"k8s.io/initial-events-end": "true"}},
			}})
			a.mu.Unlock()
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	status := func(code int32, reason metav1.StatusReason) {
		w.WriteHeader(int(code))
		_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: reason, Code: code})
	}
	missing := func() { status(http.StatusNotFound, metav1.StatusReasonNotFound) }
	path := r.URL.Path
	switch {
	case strings.Contains(path, "/persistentvolumeclaims"):
		a.pvcRequests++
		status(http.StatusForbidden, metav1.StatusReasonForbidden)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/pods"):
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "PodList", "metadata": map[string]any{"resourceVersion": "1"}, "items": []any{}})
	case strings.Contains(path, "/pods/"):
		missing()
	case strings.Contains(path, "/secrets/") && r.Method == http.MethodGet:
		missing()
	case strings.Contains(path, "/secrets"):
		// A role Secret written for a new pod: accepted as sent, and kept by nothing here.
		w.WriteHeader(http.StatusCreated)
		_, _ = io.Copy(w, r.Body)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/sandboxes"):
		items := []any{}
		for _, object := range a.objects {
			items = append(items, object)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "SandboxList", "metadata": map[string]any{"resourceVersion": "1"}, "items": items})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/sandboxes"):
		var object map[string]any
		if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		meta := object["metadata"].(map[string]any)
		name := meta["name"].(string)
		if _, taken := a.objects[name]; taken {
			status(http.StatusConflict, metav1.StatusReasonAlreadyExists)
			return
		}
		a.uids++
		meta["uid"], meta["resourceVersion"], meta["generation"] = fmt.Sprintf("sandbox-uid-created-%d", a.uids), "1", float64(1)
		a.objects[name], a.pvcs[name] = object, true
		a.created = append(a.created, name)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(object)
	case strings.Contains(path, "/sandboxes/"):
		name := path[strings.LastIndex(path, "/")+1:]
		object, ok := a.objects[name]
		if !ok {
			missing()
			return
		}
		meta, spec := object["metadata"].(map[string]any), object["spec"].(map[string]any)
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(object)
		case http.MethodPatch:
			var ops []struct {
				Op    string `json:"op"`
				Path  string `json:"path"`
				Value any    `json:"value"`
			}
			if err := json.NewDecoder(r.Body).Decode(&ops); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			for _, op := range ops {
				switch {
				case op.Op == "test" && op.Path == "/metadata/uid" && op.Value == meta["uid"]:
				case op.Op == "add" && strings.HasPrefix(op.Path, "/spec/"):
					spec[strings.TrimPrefix(op.Path, "/spec/")] = op.Value
					meta["generation"] = meta["generation"].(float64) + 1
				case op.Op == "add" && strings.HasPrefix(op.Path, "/metadata/labels/"):
					meta["labels"].(map[string]any)[strings.NewReplacer("~1", "/", "~0", "~").Replace(strings.TrimPrefix(op.Path, "/metadata/labels/"))] = op.Value
				default:
					http.Error(w, "unexpected or stale patch", 409)
					return
				}
			}
			meta["resourceVersion"] = fmt.Sprintf("%d", int(meta["generation"].(float64))+1)
			_ = json.NewEncoder(w).Encode(object)
		case http.MethodDelete:
			var options metav1.DeleteOptions
			if err := json.NewDecoder(r.Body).Decode(&options); err != nil && err != io.EOF {
				http.Error(w, err.Error(), 400)
				return
			}
			if p := options.Preconditions; p != nil && (p.UID != nil && string(*p.UID) != meta["uid"] || p.ResourceVersion != nil && *p.ResourceVersion != meta["resourceVersion"]) {
				status(http.StatusConflict, metav1.StatusReasonConflict)
				return
			}
			a.deletes[name] = options
			delete(a.objects, name)
			if options.PropagationPolicy != nil && *options.PropagationPolicy == metav1.DeletePropagationForeground {
				delete(a.pvcs, name)
			}
			_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Success"})
		default:
			http.Error(w, "unexpected method", 405)
		}
	default:
		http.Error(w, "unexpected resource", 404)
	}
}

// mode is the Sandbox's operatingMode, or "deleted" once the API no longer has it.
func (a *issueSandboxAPI) mode(name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	object, ok := a.objects[name]
	if !ok {
		return "deleted"
	}
	return object["spec"].(map[string]any)["operatingMode"].(string)
}

// uid is the Sandbox's uid, "" once the API no longer has it.
func (a *issueSandboxAPI) uid(name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	object, ok := a.objects[name]
	if !ok {
		return ""
	}
	return object["metadata"].(map[string]any)["uid"].(string)
}

// pvc is whether the volume claim the Sandbox owns still exists.
func (a *issueSandboxAPI) pvc(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pvcs[name]
}

// deleted is the options of the Sandbox's delete, and whether one was requested.
func (a *issueSandboxAPI) deleted(name string) (metav1.DeleteOptions, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	options, ok := a.deletes[name]
	return options, ok
}

// initEnv is the environment variable names of the Sandbox's workspace-init container, from the
// pod template the daemon's Running patch wrote; nil when the Sandbox has no template.
func (a *issueSandboxAPI) initEnv(name string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	object, ok := a.objects[name]
	if !ok {
		return nil
	}
	template, _ := object["spec"].(map[string]any)["podTemplate"].(map[string]any)
	if template == nil {
		return nil
	}
	containers, _ := template["spec"].(map[string]any)["initContainers"].([]any)
	var names []string
	for _, c := range containers {
		container := c.(map[string]any)
		if container["name"] != "workspace-init" {
			continue
		}
		env, _ := container["env"].([]any)
		for _, e := range env {
			names = append(names, e.(map[string]any)["name"].(string))
		}
	}
	return names
}

type issueProvisionTokens struct{}

func (issueProvisionTokens) Token(context.Context, string) (string, error) {
	return "fixture-provision-token", nil
}

func TestIssueCloseSuspendsSandboxAfterRestartUntilLingerCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
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
		rt, err := sandbox.New(runtimeCtx, &rest.Config{Host: server.URL}, sandbox.Options{
			Namespace: "legion", Project: "legion", Store: reopened, Image: "ghcr.io/example/worker@sha256:" + strings.Repeat("a", 64),
			StorageClass: "standard", IssueVolume: resource.MustParse("1Gi"), StreamURL: "tcp://127.0.0.1:13371", Resources: defaultReservations(t),
			Tools:       sandbox.Tools{GH: "/usr/bin/gh", Git: "/usr/bin/git", JJ: "/usr/bin/jj", Legion: "/opt/legion/bin/legion", AgentSecrets: "/opt/legion/bin/agent-secrets"},
			BootTimeout: time.Second, TerminationGrace: time.Second, ProbeInterval: time.Hour, AdoptTimeout: time.Second,
			Tokens: issueProvisionTokens{}, GitHubCredential: gitHubCredential(outboxTokens{}, "legion"), Conns: fake.NewConns(), Log: quietLogger(),
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
	engine := workflow.New(records, workflow.Config{Project: "legion", Linger: time.Hour}, quietLogger())
	runner := &outbox{pool: pool, records: records, project: "legion", dispatchProject: "LEGION", supervisor: sup, trees: st,
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: issue.Key, Status: "done"}}, notices: &outboxPublisher{},
		handlers: []intake.Handler{engine}, log: quietLogger(), now: func() time.Time { return clock },
	}
	token := claim.Token("legion-legion-legion-208-planner")
	stored := supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, TreeEpoch: 1, Issue: issue.Key, Role: claim.RolePlanner,
		State: supervise.StateLaunching, Generation: 1, Session: "saved-planner", SessionFile: "/legion/sessions/planner.jsonl"}
	if err := st.PutClaim(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "close", intake.DispatchIssue{Key: issue.Key, Seq: 2, Type: "issue.closed", Status: "done", Title: issue.Title, Rank: "U"}, engine); err != nil {
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
	recovered := *runner
	recovered.supervisor = restarted
	runner = &recovered
	// The stored role's orderly stop completes independently of the runtime watch population.
	stored.State = supervise.StateSuspended
	if err := st.PutClaim(ctx, stored); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := api.mode(name); got != "Suspended" {
		t.Fatalf("Sandbox after issue close = %s, want Suspended", got)
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
		t.Fatalf("Sandbox after linger cleanup = %s, want deleted", got)
	}
}

// issueLaunchSpecs builds a launch the Sandbox runtime accepts for any workflow claim: a role prompt
// on disk and the issue's repository; the machine fills in the claim, its generation, its boot token
// and the session it resumes.
type issueLaunchSpecs struct{ prompt string }

func (s issueLaunchSpecs) SpawnSpec(context.Context, supervise.Claim) (runtime.SpawnSpec, error) {
	return runtime.SpawnSpec{
		Env:        map[string]string{"JJ_USER": "legion-implement[bot]"},
		Prompt:     runtime.PromptParts{RolePromptPaths: []string{s.prompt}},
		Repository: ghrepo.MustParse("acme/widgets"),
	}, nil
}

// A child closed as done gives its volume back at once while its parent keeps running: the close's
// issue_close rows retire the child's claims with their sessions dropped, and its issue_suspend row,
// a releasing one, foreground-deletes the child's Sandbox — taking the PVC it owns with it, with no
// PVC request from the restricted daemon — once every claim has retired; the root's Sandbox stays
// Running on its own volume. A child parked in backlog keeps today's path: its claims suspended with
// their sessions, its Sandbox Suspended and kept. The done child set back to todo launches fresh:
// a new Sandbox is created, and since the issue records no session, workspace-init is not told to
// expect a volume, so nothing reports the released volume as lost.
func TestAChildClosedDoneReleasesItsVolumeWhileItsParentRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	root := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	parent := root.Key
	done := record.Issue{Key: "LEGION-209", Project: "LEGION", Tree: root.Key, Parent: &parent, Title: "done child", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "V", LastDispatchSeq: 1}
	parked := record.Issue{Key: "LEGION-210", Project: "LEGION", Tree: root.Key, Parent: &parent, Title: "parked child", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "W", LastDispatchSeq: 1}
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
	rt, err := sandbox.New(runtimeCtx, &rest.Config{Host: server.URL}, sandbox.Options{
		Namespace: "legion", Project: "legion", Store: st, Image: "ghcr.io/example/worker@sha256:" + strings.Repeat("a", 64),
		StorageClass: "standard", IssueVolume: resource.MustParse("1Gi"), StreamURL: "tcp://127.0.0.1:13371", Resources: defaultReservations(t),
		Tools:       sandbox.Tools{GH: "/usr/bin/gh", Git: "/usr/bin/git", JJ: "/usr/bin/jj", Legion: "/opt/legion/bin/legion", AgentSecrets: "/opt/legion/bin/agent-secrets"},
		BootTimeout: 300 * time.Millisecond, TerminationGrace: 100 * time.Millisecond, ProbeInterval: time.Hour, AdoptTimeout: time.Second,
		Tokens: issueProvisionTokens{}, GitHubCredential: gitHubCredential(outboxTokens{}, "legion"), Conns: fake.NewConns(), Log: logger,
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
		VolumeLost: sup.volumeLost,
		Limits:     supervise.Limits{LaunchFailures: 1, PromptFailures: 2, PromptRetires: 2},
		Timeouts:   supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second},
	}
	t.Cleanup(sup.stop)
	// The root's architect works on, with its session; each child has one role stopped with its
	// session retained, as a phase's end leaves it, supervised by this daemon.
	architect := supervise.Claim{Token: "legion-legion-legion-208-architect", Project: "legion", Tree: root.Key, TreeEpoch: 1, Issue: root.Key, Role: claim.RoleArchitect,
		State: supervise.StateWorking, Generation: 1, Session: "saved-architect", SessionFile: "/legion/sessions/architect.jsonl"}
	planner := supervise.Claim{Token: "legion-legion-legion-209-planner", Project: "legion", Tree: root.Key, TreeEpoch: 1, Issue: done.Key, Role: claim.RolePlanner,
		State: supervise.StateSuspended, Generation: 1, Session: "saved-planner", SessionFile: "/legion/sessions/planner.jsonl"}
	tester := supervise.Claim{Token: "legion-legion-legion-210-tester", Project: "legion", Tree: root.Key, TreeEpoch: 1, Issue: parked.Key, Role: claim.RoleTester,
		State: supervise.StateSuspended, Generation: 1, Session: "saved-tester", SessionFile: "/legion/sessions/tester.jsonl"}
	for _, c := range []supervise.Claim{architect, planner, tester} {
		if err := st.PutClaim(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sup.restore(ctx, []supervise.Claim{planner, tester}); err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	engine := workflow.New(records, workflow.Config{Project: "legion", Linger: time.Hour}, logger)
	admission := admit.New(records, engine, 2, "LEGION", logger)
	runner := &outbox{pool: pool, records: records, project: "legion", dispatchProject: "LEGION", supervisor: sup, trees: st, tokens: outboxTokens{},
		stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets"), githubAPI: newBranchGitHub(t, nil, branchExists).url,
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

	// (a) The done child: its claims closed, its Sandbox and volume gone, its parent untouched.
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "child-done", intake.DispatchIssue{Key: done.Key, Seq: 2, Type: "issue.closed", Status: "done", Title: done.Title, Parent: root.Key, Rank: "V"}, engine, admission); err != nil {
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
	if api.pvc(doneSandbox) {
		t.Fatal("the done child's volume claim survived its Sandbox's foreground delete")
	}
	if got := api.mode(rootSandbox); got != "Running" || !api.pvc(rootSandbox) || api.uid(rootSandbox) != "sandbox-uid-root" {
		t.Fatalf("root Sandbox after the child's close = %s (pvc %t, uid %s), want the same Sandbox Running with its volume", got, api.pvc(rootSandbox), api.uid(rootSandbox))
	}
	if _, touched := api.deleted(rootSandbox); touched || api.pvcRequests != 0 {
		t.Fatalf("the child's close deleted the root's Sandbox (%t) or sent %d PVC requests, want neither", touched, api.pvcRequests)
	}
	if got := storedClaim(planner.Token); got.State != supervise.StateRetired || got.Session != "" || got.SessionFile != "" || got.WorkspaceLost || got.Locator != nil {
		t.Fatalf("done child's planner after the close = %+v, want retired with no session, no loss and no locator", got)
	}
	if sessions, err := st.IssueHasSessions(ctx, "legion", done.Key); err != nil || sessions {
		t.Fatalf("IssueHasSessions(%s) = %t, %v after the close, want false", done.Key, sessions, err)
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
	if !strings.Contains(logs.String(), "released the closed issue's Sandbox") {
		t.Fatal("the release logged no line naming the issue's Sandbox")
	}

	// (c) The parked child: today's path, its Sandbox Suspended and kept, its sessions retained.
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "child-backlog", intake.DispatchIssue{Key: parked.Key, Seq: 2, Type: "issue.updated", Status: "backlog", Title: parked.Title, Parent: root.Key, Rank: "W"}, engine, admission); err != nil {
		t.Fatal(err)
	}
	run("the parked child's move to backlog")
	if got := api.mode(parkedSandbox); got != "Suspended" || !api.pvc(parkedSandbox) || api.uid(parkedSandbox) != "sandbox-uid-parked" {
		t.Fatalf("parked child's Sandbox = %s (pvc %t, uid %s), want the same Sandbox Suspended with its volume", got, api.pvc(parkedSandbox), api.uid(parkedSandbox))
	}
	if _, touched := api.deleted(parkedSandbox); touched {
		t.Fatal("the parked child's move deleted its Sandbox")
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
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "child-todo", intake.DispatchIssue{Key: done.Key, Seq: 3, Type: "issue.updated", Status: "todo", Title: done.Title, Parent: root.Key, Rank: "V"}, engine, admission); err != nil {
		t.Fatal(err)
	}
	run("the re-entered child's branch and status")
	run("the re-entered child's planner start")
	if !slices.Contains(api.created, doneSandbox) {
		t.Fatalf("the re-entered child's start created %v, want a new Sandbox %s", api.created, doneSandbox)
	}
	if uid := api.uid(doneSandbox); uid == "" || uid == "sandbox-uid-done" {
		t.Fatalf("re-entered child's Sandbox uid = %q, want a Sandbox other than the released one", uid)
	}
	env := api.initEnv(doneSandbox)
	if !slices.Contains(env, "PATH") {
		t.Fatalf("the re-entered child's workspace-init environment %v names no PATH; the Running patch wrote no pod template", env)
	}
	if slices.Contains(env, "LEGION_EXPECT_ISSUE_VOLUME") {
		t.Fatalf("the re-entered child's workspace-init environment %v expects a volume that was released", env)
	}
	if got := storedClaim(planner.Token); got.SessionFile != "" || got.WorkspaceLost {
		t.Fatalf("re-entered child's planner = %+v, want a fresh launch with no session to resume and no loss", got)
	}
	if logged := logs.String(); strings.Contains(logged, "volume was lost") {
		t.Fatalf("the re-entered child's launch reported a lost volume:\n%s", logged)
	}
}
