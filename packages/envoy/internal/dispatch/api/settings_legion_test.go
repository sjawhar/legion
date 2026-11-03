package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// legionSettingResponse is one row of GET /api/v1/settings/legion.
type legionSettingResponse struct {
	Project   string `json:"project"`
	StateURL  string `json:"state_url"`
	CreatedBy struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"created_by"`
	UpdatedAt string `json:"updated_at"`
}

// seedLegionProjects creates each project a Legion settings test writes.
func seedLegionProjects(t *testing.T, handler http.Handler, keys ...string) {
	t.Helper()
	for _, key := range keys {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": key, "name": key}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", key, created.Code, created.Body.String())
		}
	}
}

// TestALegionSettingRoundTripsAndIsRemoved: a PUT names a project's daemon, the GET lists it with
// the actor that set it, a second PUT moves the URL and keeps that actor, and the DELETE removes
// it.
func TestALegionSettingRoundTripsAndIsRemoved(t *testing.T) {
	handler := newTestHandler(t)
	seedLegionProjects(t, handler, "INFRA", "CORE")

	if listed := decodeBody[[]legionSettingResponse](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/legion", nil, "alice")); len(listed) != 0 {
		t.Fatalf("a deployment with no setting listed %v, want []", listed)
	}

	written := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/legion/INFRA",
		map[string]any{"state_url": "http://legion-infra.internal:8080/"}, "alice")
	if written.Code != http.StatusOK {
		t.Fatalf("set INFRA's daemon: status=%d body=%s", written.Code, written.Body.String())
	}
	setting := decodeBody[legionSettingResponse](t, written)
	if setting.StateURL != "http://legion-infra.internal:8080" {
		t.Errorf("state_url = %q, want the trailing slash trimmed", setting.StateURL)
	}
	if setting.CreatedBy.Kind != "user" || setting.CreatedBy.ID != "alice" {
		t.Errorf("created_by = %+v, want the person who set it", setting.CreatedBy)
	}

	if moved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/legion/INFRA",
		map[string]any{"state_url": "https://legion-infra.internal"}, "bob"); moved.Code != http.StatusOK {
		t.Fatalf("move INFRA's daemon: status=%d body=%s", moved.Code, moved.Body.String())
	}
	if again := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/legion/CORE",
		map[string]any{"state_url": "https://legion-core.internal"}, "alice"); again.Code != http.StatusOK {
		t.Fatalf("set CORE's daemon: status=%d body=%s", again.Code, again.Body.String())
	}

	listed := decodeBody[[]legionSettingResponse](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/legion", nil, "alice"))
	if len(listed) != 2 || listed[0].Project != "CORE" || listed[1].Project != "INFRA" {
		t.Fatalf("listed %+v, want both settings by project key", listed)
	}
	if listed[1].StateURL != "https://legion-infra.internal" {
		t.Errorf("INFRA's state_url = %q, want the moved one", listed[1].StateURL)
	}
	if listed[1].CreatedBy.ID != "alice" {
		t.Errorf("INFRA's created_by = %q, want the actor that first set it", listed[1].CreatedBy.ID)
	}

	removed := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/settings/legion/INFRA", nil, "alice")
	if removed.Code != http.StatusNoContent {
		t.Fatalf("remove INFRA's daemon: status=%d body=%s", removed.Code, removed.Body.String())
	}
	if again := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/settings/legion/INFRA", nil, "alice"); again.Code != http.StatusNotFound {
		t.Errorf("removing it twice: status=%d body=%s, want 404", again.Code, again.Body.String())
	}
	if listed := decodeBody[[]legionSettingResponse](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/legion", nil, "alice")); len(listed) != 1 {
		t.Fatalf("listed %+v after the delete, want CORE alone", listed)
	}
}

// TestAnUnknownProjectAndABadStateURLAreRefused: a setting names a project that exists, and a URL
// every call can be built from.
func TestAnUnknownProjectAndABadStateURLAreRefused(t *testing.T) {
	handler := newTestHandler(t)
	seedLegionProjects(t, handler, "INFRA")

	unknown := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/legion/NOPE",
		map[string]any{"state_url": "https://legion.internal"}, "alice")
	if unknown.Code != http.StatusNotFound {
		t.Errorf("a PUT naming an unknown project: status=%d body=%s, want 404", unknown.Code, unknown.Body.String())
	}
	if removed := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/settings/legion/NOPE", nil, "alice"); removed.Code != http.StatusNotFound {
		t.Errorf("a DELETE naming an unknown project: status=%d body=%s, want 404", removed.Code, removed.Body.String())
	}

	for name, value := range map[string]string{
		"nothing":      "",
		"no scheme":    "legion.internal:8080",
		"a path":       "https://legion.internal/legion",
		"a query":      "https://legion.internal?token=secret",
		"a credential": "https://user:pass@legion.internal",
	} {
		refused := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/legion/INFRA",
			map[string]any{"state_url": value}, "alice")
		if refused.Code != http.StatusBadRequest {
			t.Errorf("a state_url with %s: status=%d body=%s, want 400", name, refused.Code, refused.Body.String())
		}
	}
}

// TestALegionSettingIsRefusedToAProjectScopedToken: pointing Dispatch at a host, and relaying a
// change to what a daemon runs on, belong to the deployment, so a credential that may write one
// project is refused all three writes by name — including the one whose path names its own
// project.
func TestALegionSettingIsRefusedToAProjectScopedToken(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-that-may-not-point-dispatch-at-a-daemon"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")

	for _, write := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPut, "/api/v1/settings/legion/" + brokerTestProject, map[string]any{"state_url": "https://legion.internal"}},
		{http.MethodDelete, "/api/v1/settings/legion/" + brokerTestProject, nil},
		{http.MethodPut, "/api/v1/legion/" + brokerTestProject + "/settings", map[string]any{"changes": map[string]any{"concurrency": 4}}},
	} {
		refused := rig.serve(caller.presenting(t, write.method, write.path, token, write.body))
		if refused.Code != http.StatusForbidden {
			t.Fatalf("%s %s under a project-scoped token: status=%d body=%s, want 403",
				write.method, write.path, refused.Code, refused.Body.String())
		}
		refusal := decodeBody[refusalAnswer](t, refused)
		if refusal.Code != codeProjectScopeDeployment {
			t.Errorf("%s %s refusal = %+v, want %s", write.method, write.path, refusal, codeProjectScopeDeployment)
		}
		if !strings.Contains(refusal.Error, write.method+" ") {
			t.Errorf("%s %s refusal %q does not name the route", write.method, write.path, refusal.Error)
		}
	}
}

// fakeLegionDaemon answers PUT /legion/v1/operator/settings, recording the body and the bearer.
type fakeLegionDaemon struct {
	*httptest.Server
	authorization string
	body          []byte
}

func newFakeLegionDaemon(t *testing.T, status int, answer string) *fakeLegionDaemon {
	t.Helper()
	daemon := &fakeLegionDaemon{}
	daemon.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != legion.OperatorSettingsPath {
			http.NotFound(w, r)
			return
		}
		daemon.authorization = r.Header.Get("Authorization")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the relayed body: %v", err)
		}
		daemon.body = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(daemon.Close)
	return daemon
}

// TestTheLegionSettingsRelayAnswersTheDaemonsResult: the relay sends the operator's changes and
// the caller's login to the project's daemon with the bearer the token file holds, and answers
// the daemon's per-key result as it came back.
func TestTheLegionSettingsRelayAnswersTheDaemonsResult(t *testing.T) {
	const answer = `{"concurrency":{"applied":"at-once"},"runtime.tmux.window":{"refused":"no such setting"}}`
	daemon := newFakeLegionDaemon(t, http.StatusOK, answer)
	tokenFile := filepath.Join(t.TempDir(), "daemon-token")
	if err := os.WriteFile(tokenFile, []byte("a-projected-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler, _, _ := newTestServer(t, testServerOptions{legionDaemonTokenFile: tokenFile})
	seedLegionProjects(t, handler, "INFRA")
	if set := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/legion/INFRA",
		map[string]any{"state_url": daemon.URL}, "alice"); set.Code != http.StatusOK {
		t.Fatalf("set INFRA's daemon: status=%d body=%s", set.Code, set.Body.String())
	}

	relayed := dispatchRequest(t, handler, http.MethodPut, "/api/v1/legion/INFRA/settings", map[string]any{
		"changes": map[string]any{"concurrency": 4, "runtime.tmux.window": "legion"},
	}, "alice")
	if relayed.Code != http.StatusOK {
		t.Fatalf("relay a change: status=%d body=%s", relayed.Code, relayed.Body.String())
	}
	if strings.TrimSpace(relayed.Body.String()) != answer {
		t.Errorf("the relay answered %s, want the daemon's own body unchanged", relayed.Body.String())
	}
	if daemon.authorization != "Bearer a-projected-token" {
		t.Errorf("the daemon saw %q, want the token file's contents as a bearer", daemon.authorization)
	}
	var sent struct {
		Changes map[string]json.RawMessage `json:"changes"`
		By      string                     `json:"by"`
	}
	if err := json.Unmarshal(daemon.body, &sent); err != nil {
		t.Fatalf("decode the relayed body %s: %v", daemon.body, err)
	}
	if sent.By != "alice" {
		t.Errorf("by = %q, want the person who asked", sent.By)
	}
	if string(sent.Changes["concurrency"]) != "4" || string(sent.Changes["runtime.tmux.window"]) != `"legion"` {
		t.Errorf("changes = %v, want the operator's own", sent.Changes)
	}
}

// TestTheLegionSettingsRelayPassesARefusalThroughAndSendsNoBearerWithoutAFile: with no token file
// the relay presents nothing, and whatever the daemon answers — status and body — is what the
// caller sees.
func TestTheLegionSettingsRelayPassesARefusalThroughAndSendsNoBearerWithoutAFile(t *testing.T) {
	const refusal = `{"concurrency":{"refused":"a running issue holds a slot"}}`
	daemon := newFakeLegionDaemon(t, http.StatusConflict, refusal)
	handler := newTestHandler(t)
	seedLegionProjects(t, handler, "INFRA")
	if set := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/legion/INFRA",
		map[string]any{"state_url": daemon.URL}, "alice"); set.Code != http.StatusOK {
		t.Fatalf("set INFRA's daemon: status=%d body=%s", set.Code, set.Body.String())
	}

	relayed := dispatchRequest(t, handler, http.MethodPut, "/api/v1/legion/INFRA/settings", map[string]any{
		"changes": map[string]any{"concurrency": 1},
	}, "alice")
	if relayed.Code != http.StatusConflict || strings.TrimSpace(relayed.Body.String()) != refusal {
		t.Errorf("the relay answered %d %s, want the daemon's own refusal", relayed.Code, relayed.Body.String())
	}
	if daemon.authorization != "" {
		t.Errorf("the daemon saw %q, want no bearer: no token file is configured", daemon.authorization)
	}
}

// TestTheLegionSettingsRelayRefusesWhatItCannotSend: a project with no daemon, an empty change
// set, and a daemon that cannot be reached are each the relay's own refusal rather than a call
// nobody can read.
func TestTheLegionSettingsRelayRefusesWhatItCannotSend(t *testing.T) {
	handler := newTestHandler(t)
	seedLegionProjects(t, handler, "INFRA", "CORE")

	unconfigured := dispatchRequest(t, handler, http.MethodPut, "/api/v1/legion/CORE/settings",
		map[string]any{"changes": map[string]any{"concurrency": 4}}, "alice")
	if unconfigured.Code != http.StatusNotFound {
		t.Errorf("a project with no daemon: status=%d body=%s, want 404", unconfigured.Code, unconfigured.Body.String())
	}
	if code := decodeBody[refusalAnswer](t, unconfigured).Code; code != "LEGION_NOT_CONFIGURED" {
		t.Errorf("code = %q, want LEGION_NOT_CONFIGURED", code)
	}

	// A daemon whose address nothing answers: the listener is closed before the relay runs.
	gone := newFakeLegionDaemon(t, http.StatusOK, `{}`)
	goneURL := gone.URL
	gone.Close()
	if set := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/legion/INFRA",
		map[string]any{"state_url": goneURL}, "alice"); set.Code != http.StatusOK {
		t.Fatalf("set INFRA's daemon: status=%d body=%s", set.Code, set.Body.String())
	}
	if empty := dispatchRequest(t, handler, http.MethodPut, "/api/v1/legion/INFRA/settings",
		map[string]any{"changes": map[string]any{}}, "alice"); empty.Code != http.StatusBadRequest {
		t.Errorf("an empty change set: status=%d body=%s, want 400", empty.Code, empty.Body.String())
	}
	unreachable := dispatchRequest(t, handler, http.MethodPut, "/api/v1/legion/INFRA/settings",
		map[string]any{"changes": map[string]any{"concurrency": 4}}, "alice")
	if unreachable.Code != http.StatusServiceUnavailable {
		t.Errorf("an unreachable daemon: status=%d body=%s, want 503", unreachable.Code, unreachable.Body.String())
	}
	if code := decodeBody[refusalAnswer](t, unreachable).Code; code != "LEGION_DAEMON_UNAVAILABLE" {
		t.Errorf("code = %q, want LEGION_DAEMON_UNAVAILABLE", code)
	}
}
