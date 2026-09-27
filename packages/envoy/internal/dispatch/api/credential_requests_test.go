// credential_requests_test.go is the unit layer: a fake broker (httptest.Server) proves the
// proxy's own logic — FEATURE_OFF, approver=me resolution and its refusal, ceremony path
// validation, and status/body forwarding both ways — without a real broker. contract_test.go is
// what proves a real broker's handlers round-trip through these same routes.
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const credentialTestUIToken = "dispatch-ui-token"

// newCredentialTestHandler mounts the real routes with AgentSecrets pointed at brokerURL ("" to
// leave the feature off). It never shares newTestServer's testServerOptions on purpose: every
// other API test leaves the feature off, and this file's tests are the only ones that need it on.
func newCredentialTestHandler(t *testing.T, brokerURL string) http.Handler {
	t.Helper()
	database := storetest.Open(t)
	documentService := docs.New(docs.Deps{
		Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond,
	})
	t.Cleanup(func() { _ = documentService.Shutdown(t.Context()) })
	allowed := map[string]struct{}{"alice": {}, "bob": {}}
	depsInput := DepsInput{
		Store:         database,
		Identity:      identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: allowed},
		AllowedLogins: allowed,
		AgentToken:    "agent-token",
		ServerURL:     "https://dispatch.example",
		Docs:          documentService,
		Events:        events.NewBroker(),
	}
	if brokerURL != "" {
		depsInput.AgentSecretsURL = brokerURL
		depsInput.AgentSecretsToken = credentialTestUIToken
	}
	deps, err := NewDeps(depsInput)
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	return mux
}

// credentialRoutes is every row this task adds, for table-driven coverage.
var credentialRoutes = []struct {
	method, path string
}{
	{http.MethodGet, "/api/v1/credential-requests"},
	{http.MethodGet, "/api/v1/credential-requests/rec1"},
	{http.MethodPost, "/api/v1/credential-requests/rec1/approve"},
	{http.MethodPost, "/api/v1/credential-requests/rec1/deny"},
	{http.MethodPost, "/api/v1/credential-requests/machine-lookup"},
	{http.MethodGet, "/api/v1/credential-keys/alice"},
	{http.MethodPost, "/api/v1/credential-keys/alice/register/begin"},
	{http.MethodGet, "/api/v1/credential-grants"},
	{http.MethodPost, "/api/v1/credential-grants/grant1/revoke"},
}

func TestCredentialRoutesAnswerFeatureOffWithNilClient(t *testing.T) {
	handler := newCredentialTestHandler(t, "")
	for _, route := range credentialRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			response := dispatchRequest(t, handler, route.method, route.path, map[string]any{}, "alice")
			if response.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404: %s", response.Code, response.Body.String())
			}
			body := decodeBody[struct {
				Code string `json:"code"`
			}](t, response)
			if body.Code != "FEATURE_OFF" {
				t.Fatalf("code = %q, want FEATURE_OFF", body.Code)
			}
		})
	}
}

// fakeBrokerRig is a fake broker recording the last request it served, answering canned
// status/body pairs keyed by "METHOD path".
type fakeBrokerRig struct {
	server        *httptest.Server
	lastAuth      string
	lastQuery     string
	lastBody      string
	calls         int
	statusByRoute map[string]int
	bodyByRoute   map[string]string
}

func newFakeBrokerRig(t *testing.T) *fakeBrokerRig {
	t.Helper()
	rig := &fakeBrokerRig{statusByRoute: map[string]int{}, bodyByRoute: map[string]string{}}
	rig.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.calls++
		rig.lastAuth = r.Header.Get("Authorization")
		rig.lastQuery = r.URL.RawQuery
		body, _ := io.ReadAll(r.Body)
		rig.lastBody = string(body)
		key := r.Method + " " + r.URL.Path
		status, ok := rig.statusByRoute[key]
		if !ok {
			status = http.StatusOK
		}
		responseBody, ok := rig.bodyByRoute[key]
		if !ok {
			responseBody = `{"ok":true}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(responseBody))
	}))
	t.Cleanup(rig.server.Close)
	return rig
}

func TestCredentialRouteForwardsBrokerSuccessBodyAndAuth(t *testing.T) {
	rig := newFakeBrokerRig(t)
	rig.bodyByRoute["GET /v1/credential-requests/rec1"] = `{"record_id":"rec1","state":"pending"}`
	handler := newCredentialTestHandler(t, rig.server.URL)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/credential-requests/rec1", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if response.Body.String() != `{"record_id":"rec1","state":"pending"}` {
		t.Fatalf("body = %s, want the broker's body verbatim", response.Body.String())
	}
	if rig.lastAuth != "Bearer "+credentialTestUIToken {
		t.Fatalf("broker saw Authorization = %q, want the configured UI token", rig.lastAuth)
	}
}

func TestCredentialRouteForwardsBrokerErrorStatusAndBodyVerbatim(t *testing.T) {
	rig := newFakeBrokerRig(t)
	rig.statusByRoute["POST /v1/credential-requests/rec1/approve"] = http.StatusConflict
	rig.bodyByRoute["POST /v1/credential-requests/rec1/approve"] = `{"code":"RECORD_TERMINAL","error":"already decided"}`
	handler := newCredentialTestHandler(t, rig.server.URL)

	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/credential-requests/rec1/approve",
		map[string]any{"assertion": json.RawMessage(`{"id":"a"}`)}, "alice")
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if response.Body.String() != `{"code":"RECORD_TERMINAL","error":"already decided"}` {
		t.Fatalf("body = %s, want the broker's error body verbatim", response.Body.String())
	}
}

func TestCredentialRouteRelaysRequestBodyVerbatim(t *testing.T) {
	rig := newFakeBrokerRig(t)
	handler := newCredentialTestHandler(t, rig.server.URL)

	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/credential-requests/rec1/deny",
		map[string]any{"assertion": map[string]string{"id": "abc"}}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if rig.lastBody != `{"assertion":{"id":"abc"}}` {
		t.Fatalf("broker saw body %q, want the request body byte-identical", rig.lastBody)
	}
}

func TestCredentialRouteTransportFailureIsAgentSecretsUnavailable(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // nothing listens here now: every call is a transport failure.
	handler := newCredentialTestHandler(t, deadURL)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/credential-requests/rec1", nil, "alice")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", response.Code, response.Body.String())
	}
	body := decodeBody[struct {
		Code string `json:"code"`
	}](t, response)
	if body.Code != "AGENT_SECRETS_UNAVAILABLE" {
		t.Fatalf("code = %q, want AGENT_SECRETS_UNAVAILABLE", body.Code)
	}
}

func TestListCredentialPendingResolvesApproverMeToCallerCanonicalLogin(t *testing.T) {
	rig := newFakeBrokerRig(t)
	handler := newCredentialTestHandler(t, rig.server.URL)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/credential-requests?approver=me", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if rig.lastQuery != "approver=alice" {
		t.Fatalf("broker saw query %q, want approver=alice", rig.lastQuery)
	}
}

func TestListCredentialGrantsResolvesApproverMeToCallerCanonicalLogin(t *testing.T) {
	rig := newFakeBrokerRig(t)
	handler := newCredentialTestHandler(t, rig.server.URL)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/credential-grants?approver=me", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if rig.lastQuery != "approver=alice" {
		t.Fatalf("broker saw query %q, want approver=alice", rig.lastQuery)
	}
}

func TestApproverLiteralOtherValueIsRejectedBeforeReachingTheBroker(t *testing.T) {
	rig := newFakeBrokerRig(t)
	handler := newCredentialTestHandler(t, rig.server.URL)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/credential-requests?approver=bob", nil, "alice")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
	}
	body := decodeBody[struct {
		Code string `json:"code"`
	}](t, response)
	if body.Code != "APPROVER_ME_ONLY" {
		t.Fatalf("code = %q, want APPROVER_ME_ONLY", body.Code)
	}
	if rig.calls != 0 {
		t.Fatalf("broker was called %d times, want 0: a literal approver must never reach it", rig.calls)
	}
}

func TestCredentialKeyCeremonyRejectsAnyCombinationOutsideRegisterEndorseBeginFinish(t *testing.T) {
	rig := newFakeBrokerRig(t)
	handler := newCredentialTestHandler(t, rig.server.URL)

	for _, path := range []string{
		"/api/v1/credential-keys/alice/rotate/begin",
		"/api/v1/credential-keys/alice/register/middle",
	} {
		t.Run(path, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPost, path, map[string]any{}, "alice")
			if response.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404: %s", response.Code, response.Body.String())
			}
			body := decodeBody[struct {
				Code string `json:"code"`
			}](t, response)
			if body.Code != "NOT_FOUND" {
				t.Fatalf("code = %q, want NOT_FOUND", body.Code)
			}
		})
	}
	if rig.calls != 0 {
		t.Fatalf("broker was called %d times, want 0: an invalid kind/step must never reach it", rig.calls)
	}
}

func TestCredentialKeyCeremonyAcceptsEveryValidCombination(t *testing.T) {
	rig := newFakeBrokerRig(t)
	handler := newCredentialTestHandler(t, rig.server.URL)

	for _, combo := range []string{"register/begin", "register/finish", "endorse/begin", "endorse/finish"} {
		t.Run(combo, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/credential-keys/alice/"+combo, map[string]any{}, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
			}
		})
	}
}
