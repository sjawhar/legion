package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
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

// A daemon with `answers` writes its project's answering key at boot and each registration to its
// own Envoy listener's agent registry with its own listener credential, the entry's legion_project
// the Legion project token and not the Dispatch project the issue key carries.
func TestADaemonWithAnswersWritesItsRegistrationsAndHeartbeatToTheRegistry(t *testing.T) {
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

	cfg := testConfig(t)
	cfg.EnvoyURL = listener.URL
	cfg.EnvoyTokenFile = filepath.Join(t.TempDir(), "envoy-token")
	if err := os.WriteFile(cfg.EnvoyTokenFile, []byte("listener-bearer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Answers = answersConfig(filepath.Join(t.TempDir(), "ran"), "")
	gh := binary(t, t.TempDir(), "gh")
	rt := fake.NewRuntime()
	var record built
	o := fakeRuntime(rt, &record)
	// A home of the test's own, so the boot's backfill reads no session of the host's.
	o.environ = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "LEGION_GH_PATH=" + gh}
	d := startDaemon(t, cfg, o)

	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	testwait.Eventually(t, "the boot's heartbeat", func() bool { return len(writes.of("/v1/legion/projects/"+project)) > 0 })
	var beat struct {
		Answering bool   `json:"answering"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal([]byte(writes.of("/v1/legion/projects/" + project)[0]), &beat); err != nil || !beat.Answering || beat.UpdatedAt == "" {
		t.Errorf("the heartbeat = %+v, %v; want answering with its time", beat, err)
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
