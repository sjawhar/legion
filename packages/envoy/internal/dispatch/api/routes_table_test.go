package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type routeIndex struct {
	Routes []routeIndexEntry `json:"routes"`
	Docs   string            `json:"docs"`
}

func TestRouteIndexListsEveryRouteSortedByPathThenMethod(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, t.Context(), Deps{AgentTokens: sharedAgentTokens(t, "agent-token")})

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	index := decodeBody[routeIndex](t, response)
	if index.Docs != "skills/dispatch/SKILL.md" {
		t.Fatalf("docs = %q", index.Docs)
	}
	if !sort.SliceIsSorted(index.Routes, func(left, right int) bool {
		if index.Routes[left].Path != index.Routes[right].Path {
			return index.Routes[left].Path < index.Routes[right].Path
		}
		return index.Routes[left].Method < index.Routes[right].Method
	}) {
		t.Fatalf("routes are not sorted by path then method: %+v", index.Routes)
	}
	found := false
	for _, entry := range index.Routes {
		if entry.Method == http.MethodGet && entry.Path == "/api/v1/issues/{key}" {
			found = true
			if entry.Auth != "any" {
				t.Fatalf("GET /api/v1/issues/{key} auth = %q, want any", entry.Auth)
			}
			if entry.Description == "" {
				t.Fatal("GET /api/v1/issues/{key} has no description")
			}
		}
	}
	if !found {
		t.Fatalf("GET /api/v1/issues/{key} missing from the index: %+v", index.Routes)
	}
	if len(index.Routes) != len((&server{}).routes()) {
		t.Fatalf("index lists %d routes, table has %d", len(index.Routes), len((&server{}).routes()))
	}
}

// WriteRouteIndex is what `envoy-dispatch routes` prints and the docs site's API reference is
// generated from: every row of the table, as GET /api/v1 serves it, and no test hook.
func TestWriteRouteIndexCarriesEveryRouteTheTableHas(t *testing.T) {
	var output bytes.Buffer
	if err := WriteRouteIndex(&output); err != nil {
		t.Fatal(err)
	}
	var printed routeIndex
	if err := json.Unmarshal(output.Bytes(), &printed); err != nil {
		t.Fatalf("decode %q: %v", output.String(), err)
	}
	table := (&server{}).routes()
	if len(printed.Routes) != len(table) {
		t.Fatalf("printed %d routes, table has %d", len(printed.Routes), len(table))
	}
	byKey := make(map[string]routeIndexEntry, len(printed.Routes))
	for _, entry := range printed.Routes {
		byKey[entry.Method+" "+entry.Path] = entry
	}
	for _, route := range table {
		entry, ok := byKey[route.Method+" "+route.Pattern]
		if !ok {
			t.Errorf("%s %s missing from the printed index", route.Method, route.Pattern)
			continue
		}
		if entry.Auth != route.Auth || entry.Description != route.Description {
			t.Errorf("%s %s printed as auth %q, description %q; the table says %q, %q", route.Method, route.Pattern, entry.Auth, entry.Description, route.Auth, route.Description)
		}
	}
	for _, entry := range printed.Routes {
		if strings.Contains(entry.Path, "/_test/") {
			t.Errorf("printed index carries test hook %s %s", entry.Method, entry.Path)
		}
	}

	mux := http.NewServeMux()
	Register(mux, t.Context(), Deps{AgentTokens: sharedAgentTokens(t, "agent-token")})
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1", nil))
	if served := response.Body.String(); served != output.String() {
		t.Fatalf("printed index differs from GET /api/v1:\nprinted %s\nserved  %s", output.String(), served)
	}
}

// The table is the registration: every row is mounted, every row is described, and the
// count only moves when a route is deliberately added or removed.
func TestRoutesTablePinsEveryRegisteredRoute(t *testing.T) {
	const registeredRoutes = 164
	routes := (&server{}).routes()
	if len(routes) != registeredRoutes {
		t.Fatalf("routes() has %d rows, want %d (update the pin when adding a route)", len(routes), registeredRoutes)
	}
	seen := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		key := route.Method + " " + route.Pattern
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("route %q listed twice", key)
		}
		seen[key] = struct{}{}
		if route.Description == "" {
			t.Fatalf("route %q has no description", key)
		}
		if route.Handler == nil {
			t.Fatalf("route %q has no handler", key)
		}
		switch route.Auth {
		case authPublic, authAny, authBearer:
		default:
			t.Fatalf("route %q has auth %q", key, route.Auth)
		}
	}
	if got := len((&server{deps: Deps{TestHooksEnabled: true}}).routes()); got != registeredRoutes+6 {
		t.Fatalf("test hooks add six routes: got %d, want %d", got, registeredRoutes+6)
	}
}

// routeProbePath fills a mux pattern's wildcards with well-formed placeholders so the request
// reaches the handler's auth gate rather than a pattern mismatch.
func routeProbePath(pattern string) string {
	replacements := []struct{ from, to string }{
		{"/projects/{key}", "/projects/TEST"},
		{"/issues/{key}", "/issues/TEST-1"},
		{"/me/issues/{key}", "/me/issues/TEST-1"},
		{"{id}", "00000000-0000-0000-0000-000000000001"},
		{"{session_id}", "s1"},
		{"{block_id}", "b1"},
		{"{owner}", "owner"},
		{"{repo}", "repo"},
		{"{slug}", "spec"},
		{"{number}", "1"},
		{"{attempt}", "1"},
		{"{phase}", "plan"},
	}
	path := pattern
	for _, replacement := range replacements {
		path = strings.ReplaceAll(path, replacement.from, replacement.to)
	}
	return path
}

// routeProbeBody is the JSON body a probe sends. Every mutation that decodes before it
// authenticates accepts `actor` (a bearer's self-identification). Two routes need more: the
// review route's input has no actor field and would answer 400 to an unknown key, and the reply
// route answers REPLY_FORBIDDEN to a malformed reply as well as to the wrong caller, so its probe
// must be a complete reply to reach the caller check.
func routeProbeBody(route apiRoute) string {
	switch route.Method + " " + route.Pattern {
	case "POST /api/v1/artifacts/{id}/reviews":
		return `{"state":"approved"}`
	case "POST /api/v1/messages/{id}/reply", "POST /api/v1/comments/{id}/reply":
		return `{"actor":{"kind":"session","id":"s1"},"attempt":1,"body":"probe"}`
	}
	return `{"actor":{"kind":"session","id":"s1"}}`
}

// authRefusal reports whether a response is the server turning the caller away for what its
// credential is: 401, or a 403 whose code names the credential (OAUTH_SCOPE_REQUIRED is a
// credential that holds none of the row's scopes; HANDOFF_FORBIDDEN is a cookie caller on the
// bearer-only handoff write). Other 4xx/5xx are the handler proceeding past the gate on
// placeholder input.
func authRefusal(response *httptest.ResponseRecorder) (bool, string) {
	switch response.Code {
	case http.StatusUnauthorized:
		return true, "401"
	case http.StatusForbidden:
		var body struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		switch body.Code {
		case codeScopeRequired, "REPLY_FORBIDDEN", "ACCEPT_FORBIDDEN", "HANDOFF_FORBIDDEN":
			return true, "403 " + body.Code
		}
	}
	return false, ""
}

// personalTokenHolding mints a personal agent token on the test store holding exactly the scopes
// named, and answers its plaintext: the credential a scoped row admits. The row is written
// straight to the table rather than minted through POST /api/v1/me/agent-tokens, which is itself
// one of the rows under test and needs a credential of this kind to call.
func personalTokenHolding(t *testing.T, database *store.Store, scopes ...string) string {
	t.Helper()
	plaintext, hash, err := newAgentToken()
	if err != nil {
		t.Fatalf("mint a personal token: %v", err)
	}
	if scopes == nil {
		// A caller that names no scope wants a token holding none, and the column is not null:
		// an empty set, as the shared token's credential holds, rather than a null one.
		scopes = []string{}
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into agent_tokens (owner, name, token_hash, prefix, scopes)
		values ($1, $2, $3, $4, $5)
	`, "alice", "probe", hash[:], plaintext[len("dsp_"):len("dsp_")+agentTokenPrefixLength], scopes); err != nil {
		t.Fatalf("insert a personal token: %v", err)
	}
	return plaintext
}

// Every row's auth cell is checked against what its handler does to a cookie caller, a bearer,
// and an anonymous request, so GET /api/v1 cannot describe a route the server refuses (or
// admits) differently. A row that names OAuth scopes is checked against three bearers: the
// deployment's shared token, which holds no scope and is refused OAUTH_SCOPE_REQUIRED; a
// person's own token holding exactly the scopes the row declares, which is admitted; and, on a
// row that names `approve`, a token of that person holding `person` alone, which is refused.
// That last probe is what holds the gate to the table: without it a row the table declares
// `approve` whose gate asked only for `person` would pass every test here.
func TestRoutesTableAuthCellsMatchHandlers(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	personal := personalTokenHolding(t, database, model.ScopePerson, model.ScopeApprove)
	personOnly := personalTokenHolding(t, database, model.ScopePerson)
	probe := func(route apiRoute, credential string) *httptest.ResponseRecorder {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		request := httptest.NewRequestWithContext(ctx, route.Method, routeProbePath(route.Pattern), bytes.NewReader([]byte(routeProbeBody(route))))
		request.Header.Set("Content-Type", "application/json")
		switch credential {
		case "cookie":
			request.Header.Set("X-Dispatch-User", "alice")
		case "anonymous":
		case "bearer":
			request.Header.Set("Authorization", "Bearer agent-token")
		default:
			// A personal token's plaintext, presented as the person's own credential.
			request.Header.Set("Authorization", "Bearer "+credential)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	expectAdmitted := func(t *testing.T, route apiRoute, credential string) {
		t.Helper()
		response := probe(route, credential)
		if refused, why := authRefusal(response); refused {
			t.Fatalf("%s %s is %q but refuses a %s caller with %s: %s", route.Method, route.Pattern, route.Auth, credential, why, response.Body.String())
		}
	}
	expectRefused := func(t *testing.T, route apiRoute, credential string) {
		t.Helper()
		response := probe(route, credential)
		if refused, _ := authRefusal(response); !refused {
			t.Fatalf("%s %s is %q but admits a %s caller: %d %s", route.Method, route.Pattern, route.Auth, credential, response.Code, response.Body.String())
		}
	}
	expectScopeRefusal := func(t *testing.T, route apiRoute, credential string) {
		t.Helper()
		response := probe(route, credential)
		refused, why := authRefusal(response)
		if !refused || why != "403 "+codeScopeRequired {
			t.Fatalf("%s %s needs %v but answers a %s caller %d %s", route.Method, route.Pattern, route.OAuthScopes, credential, response.Code, response.Body.String())
		}
	}
	for _, route := range (&server{}).routes() {
		t.Run(route.Method+" "+route.Pattern, func(t *testing.T) {
			switch route.Auth {
			case authPublic:
				expectAdmitted(t, route, "anonymous")
			case authAny:
				expectRefused(t, route, "anonymous")
				expectAdmitted(t, route, "cookie")
				if len(route.OAuthScopes) > 0 {
					expectScopeRefusal(t, route, "bearer")
					if slices.Contains(route.OAuthScopes, model.ScopeApprove) {
						expectScopeRefusal(t, route, personOnly)
					}
					expectAdmitted(t, route, personal)
					return
				}
				expectAdmitted(t, route, "bearer")
			case authBearer:
				expectRefused(t, route, "anonymous")
				expectRefused(t, route, "cookie")
				expectAdmitted(t, route, "bearer")
			}
		})
	}
}

// Every route Dispatch refused a bearer token on before INFRA-1017 names the `person` scope now,
// and the scope map names no route the table does not mount: the two halves register's panic
// protects (scopedRoutesNotMounted), proved from the table alone with nothing mounted.
func TestEveryRouteThatWasABrowsersAloneNamesThePersonScope(t *testing.T) {
	browserOnly := []string{
		"GET /api/v1/settings/project-default-assignees",
		"PUT /api/v1/projects/{key}/default-assignee",
		"GET /api/v1/me/agent-tokens",
		"POST /api/v1/me/agent-tokens",
		"DELETE /api/v1/me/agent-tokens/{id}",
		"GET /api/v1/users",
		"GET /api/v1/issues/{key}/subscribers",
		"DELETE /api/v1/issues/{key}/subscribers/{session_id}",
		"GET /api/v1/inbox",
		"GET /api/v1/me/answers",
		"GET /api/v1/me/replies",
		"PUT /api/v1/me/threads/{id}/read",
		"GET /api/v1/agents/{session_id}/messages",
		"POST /api/v1/agents/{session_id}/messages",
		"POST /api/v1/broadcasts",
		"GET /api/v1/broadcasts",
		"GET /api/v1/broadcasts/{id}",
		"POST /api/v1/asks/{id}/answer",
		"POST /api/v1/comments/{id}/reopen",
		"POST /api/v1/comments/{id}/accept",
		"PATCH /api/v1/comments/{id}",
		"POST /api/v1/comments/{id}/reject",
		"GET /api/v1/artifacts/{id}/subscribers",
		"DELETE /api/v1/artifacts/{id}/subscribers/{session_id}",
		"POST /api/v1/artifacts/{id}/rebuild",
		"POST /api/v1/artifacts/{id}/reviews",
		"GET /api/v1/me/state",
		"PUT /api/v1/me/issues/{key}/state",
		"GET /api/v1/me/agents/state",
		"PUT /api/v1/me/agents/{session_id}/state",
		"GET /api/v1/me/documents/{id}/read",
		"PUT /api/v1/me/documents/{id}/read",
		"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep",
		"PUT /api/v1/me/asks/{id}/snooze",
		"DELETE /api/v1/me/asks/{id}/snooze",
		"GET /api/v1/credential-requests",
		"POST /api/v1/credential-requests/{id}/approve",
		"POST /api/v1/credential-requests/{id}/deny",
		"POST /api/v1/credential-requests/machine-lookup",
		"GET /api/v1/credential-grants",
		"POST /api/v1/credential-grants/{id}/revoke",
		"GET /api/v1/machine-logins",
		"POST /api/v1/machine-logins/{id}/revoke",
		// The sign-in rows and the capabilities read are this change's own, and are the
		// browser's alone for the reason the credential rows above them are: each names the
		// calling person, whom Dispatch takes from the credential and never from the request.
		"GET /api/v1/signins",
		"POST /api/v1/signins/{id}/revoke",
		"GET /api/v1/credentials/capabilities",
		"POST /api/v1/agents/{session_id}/stream/_test/publish",
		// The scope edit is the one route that was no row at all before INFRA-1017: it is the
		// person's own, as the mint and the revoke beside it are.
		"PATCH /api/v1/me/agent-tokens/{id}",
	}
	for _, name := range browserOnly {
		scopes, named := routeScopes[name]
		if !named || !slices.Contains(scopes, model.ScopePerson) {
			t.Errorf("%s names %v, want the %s scope among them", name, scopes, model.ScopePerson)
		}
	}
	if len(routeScopes) != len(browserOnly) {
		t.Errorf("routeScopes names %d routes, want exactly the %d above", len(routeScopes), len(browserOnly))
	}
	if unmounted := scopedRoutesNotMounted(); len(unmounted) > 0 {
		t.Errorf("routeScopes names %s, which the table does not mount", strings.Join(unmounted, ", "))
	}
}
