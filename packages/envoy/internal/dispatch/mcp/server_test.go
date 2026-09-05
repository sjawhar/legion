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

// rotatingBearer sends whatever token is current at the time of each request,
// mirroring the dispatch shim: one MCP session for the life of the process,
// a freshly minted GitHub token on every call.
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

// Installation tokens expire an hour after the shim initializes its session.
// The server must authenticate each tools/call with that call's bearer, never
// the one the session was initialized with.
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

// The shim never re-initializes, so a session id minted before a server
// restart keeps arriving for the life of the shim process. A server that has
// never seen the id must serve the call rather than answer 404 until the shim
// is restarted.
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
