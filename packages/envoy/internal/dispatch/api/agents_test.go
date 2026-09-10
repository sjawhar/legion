package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

func agentsHandler(t *testing.T, envoyURL string) http.Handler {
	t.Helper()
	deps := Deps{
		Identity: identity.HeaderIdentity{
			Header:        "X-Dispatch-User",
			AllowedLogins: map[string]struct{}{"alice": {}},
		},
		AgentToken: "agent-token",
	}
	if envoyURL != "" {
		deps.Envoy = envoy.New(envoyURL)
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

func TestAgentsRejectsBearerCallers(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()

	agentsHandler(t, "http://127.0.0.1:1").ServeHTTP(response, request)

	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
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
