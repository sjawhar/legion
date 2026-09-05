package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-github/v66/github"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A request with no bearer never reaches the MCP handler, even when it is an
// otherwise valid initialize that the handler would happily accept.
func TestHandlerRejectsRequestsWithoutBearer(t *testing.T) {
	server := newServer(func(context.Context, string) *github.Client {
		t.Error("GitHub client built for a request without a bearer")
		return nil
	})
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
	))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	if recorder.Header().Get("Mcp-Session-Id") != "" {
		t.Fatal("a session was opened for a request without a bearer")
	}
}

// rotatingBearer sends whatever token is current at the time of each request:
// every plugin call mints a fresh GitHub token.
type rotatingBearer struct {
	token string
}

func (r *rotatingBearer) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+r.token)
	return http.DefaultTransport.RoundTrip(clone)
}

// recordingServer returns a dispatch server whose GitHub clients point at a
// failing stub, plus the list of bearers those clients were built with.
func recordingServer(t *testing.T) (*Server, *[]string) {
	t.Helper()
	githubStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "stub", http.StatusInternalServerError)
	}))
	t.Cleanup(githubStub.Close)
	stubURL, err := url.Parse(githubStub.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	tokensUsed := &[]string{}
	server := newServer(func(_ context.Context, token string) *github.Client {
		*tokensUsed = append(*tokensUsed, token)
		client := github.NewClient(nil)
		client.BaseURL = stubURL
		return client
	})
	return server, tokensUsed
}

// connect opens a real Streamable HTTP session against the server. The
// httptest server is closed by t.Cleanup registered here, before the session's
// own cleanup, so the session (and any standalone SSE stream a stateful
// transport would hold open) closes first and a failing test fails instead of
// hanging in Close.
func connect(t *testing.T, server *Server, bearer *rotatingBearer) *mcpsdk.ClientSession {
	t.Helper()
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	transport := &mcpsdk.StreamableClientTransport{
		Endpoint:   httpServer.URL,
		HTTPClient: &http.Client{Transport: bearer},
	}
	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil).Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

var dispatchArguments = map[string]any{
	"repo":     "acme/example-repo",
	"subject":  "s",
	"context":  "c",
	"question": "q",
}

// Installation tokens expire within an hour. The server must authenticate each
// tools/call with that call's bearer, never one seen earlier on the same
// connection.
func TestDispatchUsesTheBearerOfEachCall(t *testing.T) {
	server, tokensUsed := recordingServer(t)
	bearer := &rotatingBearer{token: "token-at-initialize"}
	session := connect(t, server, bearer)

	bearer.token = "token-at-call"
	if _, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "dispatch", Arguments: dispatchArguments}); err != nil {
		t.Fatal(err)
	}

	if want := []string{"token-at-call"}; !slices.Equal(*tokensUsed, want) {
		t.Fatalf("GitHub client built with %v, want %v", *tokensUsed, want)
	}
}

// Independent of how the transport builds sessions: the handler's bearer is
// the call's own Authorization header, not anything carried on ctx. (Under a
// stateless transport a ctx-derived bearer would happen to be per-call too,
// so the session test above cannot tell the two apart on its own.)
func TestDispatchHandlerReadsTheBearerFromTheCallsOwnHeaders(t *testing.T) {
	server, tokensUsed := recordingServer(t)
	header := http.Header{}
	header.Set("Authorization", "Bearer from-this-call")
	request := &mcpsdk.CallToolRequest{Extra: &mcpsdk.RequestExtra{Header: header}}

	server.dispatchHandler(context.Background(), request, dispatchInput{Repo: "acme/example-repo", Subject: "s", Context: "c", Question: "q"})

	if want := []string{"from-this-call"}; !slices.Equal(*tokensUsed, want) {
		t.Fatalf("GitHub client built with %v, want %v", *tokensUsed, want)
	}
}

// Plugins send no Mcp-Session-Id, but a stray one (a client that did
// initialize, or a header replayed by a proxy) must not make the server answer
// 404: the endpoint is stateless and serves every tools/call on its own.
func TestDispatchServesSessionsFromBeforeARestart(t *testing.T) {
	server, tokensUsed := recordingServer(t)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]any{"name": "dispatch", "arguments": dispatchArguments},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Mcp-Session-Id", "minted-by-the-previous-server-process")
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if want := []string{"token"}; !slices.Equal(*tokensUsed, want) {
		t.Fatalf("GitHub client built with %v, want %v", *tokensUsed, want)
	}
}

// The service-facing schema requires only the prose: `subject` opens a
// thread and `thread` continues one, so neither can be required.
func TestDispatchToolSchemaRequiresOnlyContextAndQuestion(t *testing.T) {
	server, _ := recordingServer(t)
	session := connect(t, server, &rotatingBearer{token: "token"})
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "dispatch" {
		t.Fatalf("tools: %+v", tools.Tools)
	}
	// The client holds the server's schema as its plain JSON marshaling.
	raw, err := json.Marshal(tools.Tools[0].InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("input schema %s: %v", raw, err)
	}
	if want := []string{"context", "question"}; !slices.Equal(schema.Required, want) {
		t.Errorf("required %v, want %v", schema.Required, want)
	}
	for _, name := range []string{"subject", "thread", "ask", "urgency", "repo", "parent", "origin"} {
		if _, ok := schema.Properties[name]; !ok {
			t.Errorf("schema lacks %q", name)
		}
	}
}

// A call that mixes the two modes fails validation before any GitHub call.
func TestDispatchHandlerRejectsMixedModeBeforeGitHub(t *testing.T) {
	server, tokensUsed := recordingServer(t)
	header := http.Header{}
	header.Set("Authorization", "Bearer t")
	request := &mcpsdk.CallToolRequest{Extra: &mcpsdk.RequestExtra{Header: header}}

	_, _, err := server.dispatchHandler(context.Background(), request, dispatchInput{Repo: "acme/example-repo", Thread: "42", Subject: "s", Context: "c", Question: "q"})

	if err == nil || err.Error() != "thread cannot be combined with subject, urgency, or parent" {
		t.Fatalf("err %v", err)
	}
	if len(*tokensUsed) != 1 {
		t.Errorf("the GitHub client is built from this call's bearer exactly once: %v", *tokensUsed)
	}
}
