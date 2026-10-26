package daemon

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// conversationOf is the daemon's recorded conversation of session, read from its store.
func conversationOf(t *testing.T, dsn, session string) (store.AgentConversation, bool) {
	t.Helper()
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	row, found, err := st.AgentConversation(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	return row, found
}

// A daemon with no `answers` still records every registration's conversation: the history is the
// daemon's own record, and costs a row.
func TestADaemonRecordsEveryRegisteredConversationWithoutAnswers(t *testing.T) {
	cfg := testConfig(t)
	rt := fake.NewRuntime()
	var record built
	d := startDaemon(t, cfg, fakeRuntime(rt, &record))
	token := d.spawn(architect())
	readyClaim(t, d, rt, token)

	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	row, found := conversationOf(t, cfg.PostgresDSN, "ses_"+string(token))
	if !found || row.Project != project || row.Issue != "LEGION-1" || row.Role != claim.RoleArchitect || row.Token != token ||
		row.Harness != runtime.BackendOmp || row.Location != "/sessions/"+string(token)+".jsonl" || row.Source != store.ConversationRegistered {
		t.Errorf("the recorded conversation = %+v (found %t), want the architect's registration", row, found)
	}
}

// registryWrites is every write a stand-in agent registry took, by path, with its bearer.
type registryWrites struct {
	mu     sync.Mutex
	bodies map[string][]string
	bearer []string
}

func (w *registryWrites) of(path string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bodies[path]
}

// paths is every path the stand-in took a write on.
func (w *registryWrites) paths() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Sorted(maps.Keys(w.bodies))
}

// registryListener points cfg at a stand-in Envoy listener that takes every request with 200 and
// records its body by path, and gives the daemon the listener credential listener-bearer.
func registryListener(t *testing.T, cfg *config.Config) *registryWrites {
	t.Helper()
	writes := &registryWrites{bodies: map[string][]string{}}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		writes.mu.Lock()
		writes.bodies[r.URL.Path] = append(writes.bodies[r.URL.Path], string(body))
		writes.bearer = append(writes.bearer, r.Header.Get("Authorization"))
		writes.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(listener.Close)
	cfg.EnvoyURL = listener.URL
	cfg.EnvoyTokenFile = filepath.Join(t.TempDir(), "envoy-token")
	if err := os.WriteFile(cfg.EnvoyTokenFile, []byte("listener-bearer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return writes
}

// bootHeartbeat waits for the daemon's first write of its project's answering key and returns it,
// failing unless it carries its time.
func bootHeartbeat(t *testing.T, writes *registryWrites, cfg config.Config) (answering bool) {
	t.Helper()
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/legion/projects/" + project
	testwait.Eventually(t, "the boot's heartbeat", func() bool { return len(writes.of(path)) > 0 })
	var beat struct {
		Answering *bool  `json:"answering"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal([]byte(writes.of(path)[0]), &beat); err != nil || beat.Answering == nil || beat.UpdatedAt == "" {
		t.Fatalf("the heartbeat = %s, %v; want answering and its time", writes.of(path)[0], err)
	}
	return *beat.Answering
}

// answersEnvironment is a daemon environment for `answers`: a gh of the test's own, and a home of
// the test's own, so the boot's backfill reads no session of the host's.
func answersEnvironment(t *testing.T) []string {
	t.Helper()
	gh := binary(t, t.TempDir(), "gh")
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "LEGION_GH_PATH=" + gh}
}

// A daemon with `answers` writes each registration to its own Envoy listener's agent registry with
// its own listener credential, the entry's legion_project the Legion project token and not the
// Dispatch project the issue key carries. With no workflow its answer loop does not run, so its
// heartbeat says at boot that it does not answer.
func TestADaemonWithAnswersWritesItsRegistrationsToTheRegistry(t *testing.T) {
	cfg := testConfig(t)
	writes := registryListener(t, &cfg)
	cfg.Answers = answersConfig(filepath.Join(t.TempDir(), "ran"), "")
	rt := fake.NewRuntime()
	var record built
	o := fakeRuntime(rt, &record)
	o.environ = answersEnvironment(t)
	d := startDaemon(t, cfg, o)

	if bootHeartbeat(t, writes, cfg) {
		t.Errorf("a daemon with answers and no workflow says it answers")
	}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := d.spawn(architect())
	readyClaim(t, d, rt, token)
	session := "ses_" + string(token)
	path := "/v1/legion/agents/sessions/" + session
	testwait.Eventually(t, "the registration's write", func() bool { return len(writes.of(path)) > 0 })
	var entry map[string]string
	if err := json.Unmarshal([]byte(writes.of(path)[0]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["session_id"] != session || entry["legion_project"] != project || entry["issue"] != "LEGION-1" || entry["role"] != "architect" ||
		entry["role_token"] != string(token) || entry["title"] != "Legion architect · LEGION-1" || entry["harness"] != "omp" || entry["started_at"] == "" {
		t.Errorf("the registration's entry = %v, want the architect's under the Legion project token %s", entry, project)
	}
	if strings.EqualFold(entry["legion_project"], "LEGION") {
		t.Errorf("legion_project %q is the Dispatch project", entry["legion_project"])
	}
	writes.mu.Lock()
	defer writes.mu.Unlock()
	for _, bearer := range writes.bearer {
		if bearer != "Bearer listener-bearer" {
			t.Errorf("a registry write carried %q, want the daemon's listener credential", bearer)
		}
	}
}

// A daemon without `answers` that holds an Envoy credential says at boot that it does not answer,
// with that credential, so a listener still holding the answering key an earlier boot with `answers`
// wrote stops handing the project's messages off at once. It writes no registration to the registry.
func TestADaemonWithoutAnswersSaysAtBootItDoesNotAnswer(t *testing.T) {
	cfg := testConfig(t)
	writes := registryListener(t, &cfg)
	rt := fake.NewRuntime()
	var record built
	d := startDaemon(t, cfg, fakeRuntime(rt, &record))

	if bootHeartbeat(t, writes, cfg) {
		t.Errorf("a daemon without answers says it answers")
	}
	token := d.spawn(architect())
	readyClaim(t, d, rt, token)
	if _, found := conversationOf(t, cfg.PostgresDSN, "ses_"+string(token)); !found {
		t.Fatalf("the architect's registration is not recorded")
	}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	if paths := writes.paths(); !slices.Equal(paths, []string{"/v1/legion/projects/" + project}) {
		t.Errorf("the listener took writes on %v, want the heartbeat's alone", paths)
	}
	writes.mu.Lock()
	defer writes.mu.Unlock()
	for _, bearer := range writes.bearer {
		if bearer != "Bearer listener-bearer" {
			t.Errorf("a heartbeat carried %q, want the daemon's listener credential", bearer)
		}
	}
}

// A daemon with `answers` and the workflow open runs its answer loop, and its heartbeat says at boot
// that it answers.
func TestADaemonWhoseAnswerLoopRunsSaysItAnswers(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	writes := registryListener(t, &cfg)
	cfg.Answers = answersConfig(filepath.Join(t.TempDir(), "ran"), "")
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.environ = answersEnvironment(t)
	startDaemon(t, cfg, o)

	if !bootHeartbeat(t, writes, cfg) {
		t.Errorf("a daemon whose answer loop runs says it does not answer")
	}
}
