package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestSharedAgentTokenIsJudgedAlikeByTheAPIAndTheDocumentWebsocket presents each bearer to the
// HTTP API and to the document websocket, both served by the one mux Register mounts and both
// given the same shared token, as cmd/dispatch gives them. The two surfaces admit or refuse every
// bearer alike, because both compare it with auth.MatchesSharedAgentToken; an empty configured
// token refuses every bearer on both.
func TestSharedAgentTokenIsJudgedAlikeByTheAPIAndTheDocumentWebsocket(t *testing.T) {
	bearers := []struct {
		name          string
		authorization string
		// admitted is the verdict under the configured token "agent-token".
		admitted bool
	}{
		{name: "the shared token", authorization: "Bearer agent-token", admitted: true},
		{name: "the shared token in whitespace", authorization: " Bearer agent-token\t", admitted: true},
		{name: "a truncated token", authorization: "Bearer agent-toke"},
		{name: "a longer token", authorization: "Bearer agent-tokenx"},
		{name: "one byte different", authorization: "Bearer agent-tokem"},
		{name: "an empty token", authorization: "Bearer "},
		{name: "a lowercase scheme", authorization: "bearer agent-token"},
		{name: "no scheme", authorization: "agent-token"},
		{name: "no space after the scheme", authorization: "Beareragent-token"},
		{name: "two spaces after the scheme", authorization: "Bearer  agent-token"},
	}
	for _, surfaces := range []struct {
		name       string
		configured string
	}{
		{name: "configured", configured: "agent-token"},
		{name: "unconfigured", configured: ""},
	} {
		t.Run(surfaces.name, func(t *testing.T) {
			server, artifactID := newSharedAgentTokenServer(t, surfaces.configured)
			for _, bearer := range bearers {
				want := bearer.admitted && surfaces.configured != ""
				t.Run(bearer.name, func(t *testing.T) {
					if got := apiAdmitsBearer(t, server.URL, bearer.authorization); got != want {
						t.Errorf("HTTP API admitted %q = %t, want %t", bearer.authorization, got, want)
					}
					if got := documentWebsocketAdmitsBearer(t, server.URL, artifactID, bearer.authorization); got != want {
						t.Errorf("document websocket admitted %q = %t, want %t", bearer.authorization, got, want)
					}
				})
			}
		})
	}
}

// newSharedAgentTokenServer serves the API and the document websocket on one mux, wired as
// cmd/dispatch wires them, and returns the server and a document a websocket can open.
func newSharedAgentTokenServer(t *testing.T, configured string) (*httptest.Server, string) {
	t.Helper()
	database := storetest.Open(t)
	broker := events.NewBroker()
	allowed := map[string]struct{}{"alice": {}}
	requestIdentity := identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: allowed}
	documentService := docs.New(docs.Deps{
		Store: database, Events: broker, Identity: requestIdentity, AgentToken: configured,
		ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: requestIdentity, AllowedLogins: allowed, AgentToken: configured,
		RepoProjectsRaw: "owner/repo=TEST", ServerURL: "https://dispatch.example",
		Docs: documentService, Events: broker,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	issue := createInteractionIssue(t, mux, "TEST", "Shared token", "A spec")
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, issue.PrimaryArtifactID
}

// apiAdmitsBearer reports whether GET /api/v1/whoami took the bearer as the shared token's
// agent; any answer but that or a 401 fails the test.
func apiAdmitsBearer(t *testing.T, serverURL, authorization string) bool {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, serverURL+"/api/v1/whoami", nil)
	if err != nil {
		t.Fatalf("build whoami request: %v", err)
	}
	request.Header.Set("Authorization", authorization)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("whoami with %q: %v", authorization, err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		var caller struct {
			Kind  string  `json:"kind"`
			Owner *string `json:"owner"`
		}
		if err := json.NewDecoder(response.Body).Decode(&caller); err != nil {
			t.Fatalf("decode whoami with %q: %v", authorization, err)
		}
		if caller.Kind != "agent" || caller.Owner != nil {
			t.Fatalf("whoami with %q = %+v, want the shared token's agent", authorization, caller)
		}
		return true
	case http.StatusUnauthorized:
		return false
	default:
		t.Fatalf("whoami with %q: status %d, want 200 or 401", authorization, response.StatusCode)
		return false
	}
}

// documentWebsocketAdmitsBearer reports whether the document websocket opened for the bearer and
// a session actor; any refusal but a 401 handshake fails the test.
func documentWebsocketAdmitsBearer(t *testing.T, serverURL, artifactID, authorization string) bool {
	t.Helper()
	headers := http.Header{
		"Authorization":    []string{authorization},
		"X-Dispatch-Actor": []string{`{"kind":"session","id":"session-0123456789abcdef"}`},
	}
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err == nil {
		_ = connection.Close()
		return true
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("document websocket with %q: response=%#v err=%v, want an open socket or a 401", authorization, response, err)
	}
	return false
}
