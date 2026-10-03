package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The API fixture holds the Sandbox after its role processes have stopped. There is no pod:
// its operatingMode is the daemon's request to the real Sandbox controller, not a claim about
// that controller's deletion or persistent-volume garbage collection.
type issueSandboxAPI struct {
	mu     sync.Mutex
	object map[string]any
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
				if a.object != nil {
					_ = json.NewEncoder(w).Encode(map[string]any{"type": "ADDED", "object": a.object})
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
	missing := func() {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: 404})
	}
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pods"):
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "PodList", "metadata": map[string]any{"resourceVersion": "1"}, "items": []any{}})
	case strings.Contains(r.URL.Path, "/pods/"):
		missing()
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sandboxes"):
		items := []any{}
		if a.object != nil {
			items = append(items, a.object)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "SandboxList", "metadata": map[string]any{"resourceVersion": "1"}, "items": items})
	case strings.Contains(r.URL.Path, "/sandboxes/"):
		if a.object == nil {
			missing()
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(a.object)
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
				case op.Op == "test" && op.Path == "/metadata/uid" && op.Value == "sandbox-uid":
				case op.Op == "add" && op.Path == "/spec/operatingMode":
					a.object["spec"].(map[string]any)["operatingMode"] = op.Value
				default:
					http.Error(w, "unexpected or stale patch", 409)
					return
				}
			}
			_ = json.NewEncoder(w).Encode(a.object)
		case http.MethodDelete:
			a.object = nil
			_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Success"})
		default:
			http.Error(w, "unexpected method", 405)
		}
	default:
		http.Error(w, "unexpected resource", 404)
	}
}

func (a *issueSandboxAPI) mode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.object == nil {
		return "deleted"
	}
	return a.object["spec"].(map[string]any)["operatingMode"].(string)
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
	if err := st.EnsureIssueResources(ctx, "legion", issue.Key, issue.Tree, name, 1); err != nil {
		t.Fatal(err)
	}
	api := &issueSandboxAPI{object: map[string]any{
		"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox",
		"metadata": map[string]any{"name": name, "namespace": "legion", "uid": "sandbox-uid", "resourceVersion": "1", "labels": map[string]any{"legion.dev/project": "legion", "legion.dev/tree": issue.Tree, "legion.dev/issue": issue.Key}},
		"spec":     map[string]any{"operatingMode": "Running"},
	}}
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
			Namespace: "legion", Project: "legion", Image: "ghcr.io/example/worker@sha256:" + strings.Repeat("a", 64),
			StorageClass: "standard", TreeVolume: resource.MustParse("1Gi"), StreamURL: "tcp://127.0.0.1:13371",
			Tools:       sandbox.Tools{GH: "/usr/bin/gh", Git: "/usr/bin/git", JJ: "/usr/bin/jj", Legion: "/opt/legion/bin/legion", AgentSecrets: "/opt/legion/bin/agent-secrets"},
			BootTimeout: time.Second, BootIntervals: 2, TerminationGrace: time.Second, ProbeInterval: time.Hour, AdoptTimeout: time.Second,
			Tokens: issueProvisionTokens{}, Conns: fake.NewConns(), Log: quietLogger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		rt.SetIssueResourceStore(reopened)
		sup := newSupervisor(runtimeCtx, nil, "legion", t.TempDir(), quietLogger())
		sup.deps.Runtime = rt
		t.Cleanup(sup.stop)
		return sup, stopRuntime
	}
	sup, stopRuntime := newSupervisorRuntime()
	clock := time.Now()
	engine := workflow.New(records, workflow.Config{Project: "legion", Linger: time.Hour}, quietLogger())
	runner := &outbox{pool: pool, records: records, project: "legion", dispatchProject: "LEGION", supervisor: sup,
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
	if got := api.mode(); got != "Running" {
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
	if got := api.mode(); got != "Suspended" {
		t.Fatalf("Sandbox after issue close = %s, want Suspended", got)
	}
	claims, err := st.Claims(ctx)
	if err != nil || len(claims) != 1 || claims[0].Session != "saved-planner" || claims[0].SessionFile != "/legion/sessions/planner.jsonl" {
		t.Fatalf("close lost retained session: %+v, %v", claims, err)
	}
	resources, found, err := st.IssueResources(ctx, "legion", issue.Key)
	if err != nil || !found || resources.CleanupStarted {
		t.Fatalf("close started deletion: %+v, %t, %v", resources, found, err)
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
	if got := api.mode(); got != "deleted" {
		t.Fatalf("Sandbox after linger cleanup = %s, want deleted", got)
	}
}
