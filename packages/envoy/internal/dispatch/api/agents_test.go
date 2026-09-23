package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func agentsHandler(t *testing.T, envoyURL string) http.Handler {
	t.Helper()
	return agentsHandlerWithStore(t, envoyURL, storetest.Open(t))
}

func agentsHandlerWithStore(t *testing.T, envoyURL string, database *store.Store) http.Handler {
	t.Helper()
	deps := Deps{
		Store: database,
		Identity: identity.HeaderIdentity{
			Header:        "X-Dispatch-User",
			AllowedLogins: map[string]struct{}{"alice": {}},
		},
		AgentToken: "agent-token",
		Envoy:      envoy.New(envoyURL),
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	return mux
}

func TestAgentsListsLiveSessionsNewestFirstForHumans(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"session_id":"older","title":"worker","last_seen":100,"port":1,"topics":["t"]},{"session_id":"b","title":"planner","last_seen":200},{"session_id":"a","title":"reviewer","last_seen":200}]`))
	}))
	defer listener.Close()

	response := dispatchRequest(t, agentsHandler(t, listener.URL), http.MethodGet, "/api/v1/agents", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	rows := decodeBody[[]map[string]any](t, response)
	if got := []any{rows[0]["session_id"], rows[1]["session_id"], rows[2]["session_id"]}; !reflect.DeepEqual(got, []any{"a", "b", "older"}) {
		t.Fatalf("order = %v", got)
	}
	if _, leaked := rows[2]["port"]; leaked {
		t.Fatal("port must not be exposed")
	}
	if _, leaked := rows[2]["topics"]; leaked {
		t.Fatal("topics must not be exposed")
	}
	if rows[0]["capabilities"] == nil || rows[0]["roles"] == nil {
		t.Fatalf("missing slices must encode as []: %v", rows[0])
	}
}

func TestAgentsIncludeGroupedOpenAskCountsAndLastActivity(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"session_id":"planner","title":"Planner","dir":"/w/planner","machine_id":"host-a","roles":["planner"],"capabilities":["btw"],"last_seen":200},
			{"session_id":"reviewer","title":"Reviewer","dir":"/w/reviewer","machine_id":"host-b","roles":[],"capabilities":[],"last_seen":100}
		]`))
	}))
	defer listener.Close()

	database := storetest.Open(t)
	ctx := context.Background()
	for _, query := range []string{
		`insert into projects (key, name) values ('TEST', 'Test')`,
		`insert into issues (key, project_key, number, title, status, created_by, rank) values ('TEST-1', 'TEST', 1, 'Test issue', 'todo', '{"kind":"user","id":"alice"}', 'U')`,
		`insert into issues (key, project_key, number, title, status, created_by, rank, closed_at) values ('TEST-2', 'TEST', 2, 'Closed issue', 'done', '{"kind":"user","id":"alice"}', 'V', '2026-09-13T11:00:00Z')`,
		`insert into asks (issue_key, author, question, state) values
			('TEST-1', '{"kind":"session","id":"planner"}', 'First open question', 'open'),
			('TEST-1', '{"kind":"session","id":"planner"}', 'Second open question', 'open'),
			('TEST-2', '{"kind":"session","id":"planner"}', 'Closed issue question', 'open')`,
		`insert into events (issue_key, seq, type, actor, payload, notify, created_at) values
			('TEST-1', 1, 'message.created', '{"kind":"session","id":"planner"}', '{}', false, '2026-09-13T12:00:00Z'),
			('TEST-1', 2, 'message.created', '{"kind":"session","id":"planner"}', '{}', false, '2026-09-13T13:00:00Z')`,
	} {
		if _, err := database.Pool.Exec(ctx, query); err != nil {
			t.Fatalf("seed agent enrichment data: %v", err)
		}
	}

	response := dispatchRequest(
		t,
		agentsHandlerWithStore(t, listener.URL, database),
		http.MethodGet,
		"/api/v1/agents",
		nil,
		"alice",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	rows := decodeBody[[]struct {
		SessionID    string  `json:"session_id"`
		OpenAsks     int     `json:"open_asks"`
		LastActivity *string `json:"last_activity"`
	}](t, response)
	if len(rows) != 2 {
		t.Fatalf("rows = %#v, want two live sessions", rows)
	}
	if rows[0].SessionID != "planner" || rows[0].OpenAsks != 2 {
		t.Fatalf("planner row = %#v, want two open asks from the open issue", rows[0])
	}
	if rows[0].LastActivity == nil || *rows[0].LastActivity != "2026-09-13T13:00:00Z" {
		t.Fatalf("planner activity = %#v, want newest session event", rows[0].LastActivity)
	}
	if rows[1].SessionID != "reviewer" || rows[1].OpenAsks != 0 || rows[1].LastActivity != nil {
		t.Fatalf("reviewer row = %#v, want no open asks or activity", rows[1])
	}
}

// A bearer session needs to see who advertises which delivery modes before it targets a
// message; the issue-less POST /agents/{session}/messages stays human-only.
func TestAgentsListsCapabilitiesForBearerCallers(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","last_seen":100,"capabilities":["aside","btw"]}]`))
	}))
	defer listener.Close()
	handler := agentsHandler(t, listener.URL)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	rows := decodeBody[[]map[string]any](t, response)
	if len(rows) != 1 || !reflect.DeepEqual(rows[0]["capabilities"], []any{"aside", "btw"}) {
		t.Fatalf("rows = %v", rows)
	}

	response = bearerRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "hi", "delivery": "btw", "actor": map[string]string{"kind": "session", "id": "s2"},
	})
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("issue-less targeted message by a bearer: status %d: %s", response.Code, response.Body.String())
	}
}

func TestAgentsReportsListenerUnavailable(t *testing.T) {
	for name, envoyURL := range map[string]string{"refused": "http://127.0.0.1:1", "unconfigured": ""} {
		t.Run(name, func(t *testing.T) {
			response := dispatchRequest(t, agentsHandler(t, envoyURL), http.MethodGet, "/api/v1/agents", nil, "alice")
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"ENVOY_UNAVAILABLE"`) {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
		})
	}
}
