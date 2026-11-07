package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// routeIndexGolden is the committed index of the table: one route per line, sorted by path then
// method, as `METHOD path auth`. It is what catches a route added or removed by accident, and
// what a hand-kept count could not do — two branches that each add a route are each wrong about
// a count, where this file merges into the union of the lines they add. Write it again with
// `go test ./internal/dispatch/api -run TestRouteIndexLists -update`.
const routeIndexGolden = "testdata/routes.txt"

// routeIndexLines renders the table as routeIndexGolden holds it: the rows sorted by path, then
// method, and rendered after that, so two methods of one path with different auth classes are
// still ordered by their methods. Descriptions are left out: they are prose that changes with
// every wording pass, and the test below already holds every row to having one.
func routeIndexLines(routes []apiRoute) string {
	sorted := slices.Clone(routes)
	slices.SortFunc(sorted, func(left, right apiRoute) int {
		return cmp.Or(strings.Compare(left.Pattern, right.Pattern), strings.Compare(left.Method, right.Method))
	})
	lines := make([]string, 0, len(sorted))
	for _, route := range sorted {
		lines = append(lines, route.Method+" "+route.Pattern+" "+string(route.Auth))
	}
	return strings.Join(lines, "\n") + "\n"
}

// The rendered index's own order, which the committed file can prove only for the pairs the
// table happens to hold: two methods of one path whose auth classes sort the other way round are
// ordered by their methods, not by their auth.
func TestRouteIndexLinesOrderTwoMethodsOfOnePathByMethod(t *testing.T) {
	got := routeIndexLines([]apiRoute{
		{routeRow: routeRow{Method: http.MethodGet, Pattern: "/api/v1/things/{id}", Auth: authAny}},
		{routeRow: routeRow{Method: http.MethodDelete, Pattern: "/api/v1/things/{id}", Auth: authPublic}},
	})
	want := "DELETE /api/v1/things/{id} public\nGET /api/v1/things/{id} any\n"
	if got != want {
		t.Fatalf("routeIndexLines = %q, want %q", got, want)
	}
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

	// The table against the committed index. A route added, removed, or given another auth class
	// reds this test until testdata/routes.txt is written again, deliberately, with -update.
	got := routeIndexLines((&server{}).routes())
	if *updateFixtures {
		if err := os.WriteFile(routeIndexGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", routeIndexGolden)
	}
	want, err := os.ReadFile(routeIndexGolden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		committed := strings.Split(strings.TrimSuffix(string(want), "\n"), "\n")
		table := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
		var added, removed []string
		for _, line := range table {
			if !slices.Contains(committed, line) {
				added = append(added, line)
			}
		}
		for _, line := range committed {
			if !slices.Contains(table, line) {
				removed = append(removed, line)
			}
		}
		t.Errorf("the table and %s differ.\nthe table has, and the file does not: %v\nthe file has, and the table does not: %v\nRun the test again with -update when the change is deliberate.",
			routeIndexGolden, added, removed)
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

// The table is the registration: every row is mounted, every row is described, and no row is
// listed twice.
//
// No count is pinned. A count is one number two branches cannot both be right about: a branch
// that adds a route updates it, and the base that added one first then makes that branch's
// merge red for a reason that is not the branch's change
// (docs/solutions/github/a-base-gate-arrives-in-the-merge-and-the-job-your-branch-alone-adds-is-the-line-it-refuses-pr2517.md:63-67
// records that failure: "routes() has 147 rows, want 146: the base had added a route").
// What catches a route added or removed by accident is the committed index
// TestRouteIndexListsEveryRouteSortedByPathThenMethod compares the table against
// (testdata/routes.txt), which two branches that each add a route merge into the union of their
// two lines.
func TestRoutesTablePinsEveryRegisteredRoute(t *testing.T) {
	routes := (&server{}).routes()
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
	if got := len((&server{deps: Deps{TestHooksEnabled: true}}).routes()); got != len(routes)+12 {
		t.Fatalf("test hooks add twelve routes: got %d, want %d", got, len(routes)+12)
	}
}

// TestEveryIDRouteIsNamed: every route whose pattern carries {id} or {block_id} says what that
// id names — a kind whose prefix is resolved into it and the 404 that route answers for an id
// it does not hold, or a reason it resolves none — and every name in the table is a route the
// server serves. The sub-tests are the negative control, as TestEveryMutatingRouteIsClassed's
// are: the same walk over a fixture holding a route the table does not name, one that resolves
// nothing and says why not at once, and one that resolves an id without naming that 404, fails
// on each.
func TestEveryIDRouteIsNamed(t *testing.T) {
	routes := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	if undecided := undecidedIDRoutes(routes); len(undecided) > 0 {
		t.Fatalf("%d route(s) whose id the prefix layer cannot decide:\n%s", len(undecided), strings.Join(undecided, "\n"))
	}
	served := make(map[string]bool, len(routes))
	for _, route := range routes {
		served[route.Method+" "+route.Pattern] = true
	}
	for name := range idRoutes {
		if !served[name] {
			t.Errorf("idRoutes names %q, which the table does not serve", name)
		}
		if !hasIDWildcard(name) {
			t.Errorf("idRoutes names %q, whose pattern carries no id", name)
		}
	}

	t.Run("a route the table does not name fails this walk", func(t *testing.T) {
		undecided := undecidedIDRoutes([]apiRoute{
			{routeRow: routeRow{Method: http.MethodGet, Pattern: "/api/v1/widgets", Auth: authAny}},
			{routeRow: routeRow{Method: http.MethodGet, Pattern: "/api/v1/widgets/{id}", Auth: authAny}},
		})
		if len(undecided) != 1 || !strings.HasPrefix(undecided[0], "GET /api/v1/widgets/{id}:") {
			t.Fatalf("the walk answered %q, want the unnamed id route named and the other left alone", undecided)
		}
	})

	t.Run("a row that resolves nothing and gives no reason fails it too", func(t *testing.T) {
		withIDRoute(t, "GET /api/v1/widgets/{block_id}", idRoute{})
		undecided := undecidedIDRoutes([]apiRoute{
			{routeRow: routeRow{Method: http.MethodGet, Pattern: "/api/v1/widgets/{block_id}", Auth: authAny}},
		})
		if len(undecided) != 1 || !strings.Contains(undecided[0], "gives no reason") {
			t.Fatalf("the walk answered %q, want the reasonless row named", undecided)
		}
	})

	t.Run("a row that resolves a kind and gives a reason fails it too", func(t *testing.T) {
		withIDRoute(t, "GET /api/v1/widgets/{id}", idRoute{Kind: "ask", NotFound: "NOT_FOUND", Reason: "and also not"})
		undecided := undecidedIDRoutes([]apiRoute{
			{routeRow: routeRow{Method: http.MethodGet, Pattern: "/api/v1/widgets/{id}", Auth: authAny}},
		})
		if len(undecided) != 1 || !strings.Contains(undecided[0], "a reason not to") {
			t.Fatalf("the walk answered %q, want the contradictory row named", undecided)
		}
	})

	t.Run("a row that resolves an id and names no 404 of its own fails it too", func(t *testing.T) {
		withIDRoute(t, "GET /api/v1/widgets/{id}", idRoute{Kind: "ask"})
		undecided := undecidedIDRoutes([]apiRoute{
			{routeRow: routeRow{Method: http.MethodGet, Pattern: "/api/v1/widgets/{id}", Auth: authAny}},
		})
		if len(undecided) != 1 || !strings.Contains(undecided[0], "what 404 the route answers") {
			t.Fatalf("the walk answered %q, want the row with no not-found answer named", undecided)
		}
	})

	t.Run("a row that resolves a block and names no 404 for it fails it too", func(t *testing.T) {
		withIDRoute(t, "GET /api/v1/widgets/{id}/blocks/{block_id}",
			idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND", Blocks: true})
		undecided := undecidedIDRoutes([]apiRoute{
			{routeRow: routeRow{Method: http.MethodGet, Pattern: "/api/v1/widgets/{id}/blocks/{block_id}", Auth: authAny}},
		})
		if len(undecided) != 1 || !strings.Contains(undecided[0], "the document does not hold") {
			t.Fatalf("the walk answered %q, want the row with no block not-found answer named", undecided)
		}
	})

	t.Run("every row names a kind the prefix resolver reads and a 404 that is one", func(t *testing.T) {
		for name, entry := range idRoutes {
			switch entry.Kind {
			case "", "ask", "comment", "message", "artifact":
			default:
				t.Errorf("idRoutes gives %q the kind %q, which refs.ResolvePrefix does not read", name, entry.Kind)
			}
			if entry.WithinIssue && !strings.Contains(name, "{key}") {
				t.Errorf("idRoutes looks within the issue of %q, whose pattern names none", name)
			}
			if entry.Blocks && !strings.Contains(name, "{block_id}") {
				t.Errorf("idRoutes reads a block of %q, whose pattern carries no {block_id}", name)
			}
			if strings.Contains(name, "{block_id}") && !entry.Blocks {
				t.Errorf("idRoutes leaves the {block_id} of %q unresolved", name)
			}
			for column, code := range map[string]string{"NotFound": entry.NotFound, "BlockNotFound": entry.BlockNotFound} {
				if code != "" && code != notFoundNone && !strings.HasSuffix(code, "NOT_FOUND") {
					t.Errorf("idRoutes gives %q the %s code %q, which is no not-found code", name, column, code)
				}
			}
		}
	})
}

// withIDRoute names one route in idRoutes for the length of a test, so a negative control can
// walk a row the server does not serve without leaving it behind.
func withIDRoute(t *testing.T, name string, entry idRoute) {
	t.Helper()
	idRoutes[name] = entry
	t.Cleanup(func() { delete(idRoutes, name) })
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

// personalTokenHolding mints a personal agent token of alice's on the test store holding exactly
// the scopes named, and answers its plaintext: the credential a scoped row admits.
func personalTokenHolding(t *testing.T, database *store.Store, scopes ...string) string {
	t.Helper()
	return personalTokenOwnedBy(t, database, "alice", scopes...)
}

// personalTokenOwnedBy mints one person's personal agent token holding exactly the scopes named.
// The row is written straight to the table rather than minted through POST
// /api/v1/me/agent-tokens, which is itself one of the rows under test and needs a credential of
// this kind to call. The owner is a parameter because two owners' tokens are what tells a route
// comparing whole identities from one comparing kind and id alone: every personal token resolves
// with an empty id (agent_tokens.go), so only the owner tells them apart.
func personalTokenOwnedBy(t *testing.T, database *store.Store, owner string, scopes ...string) string {
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
	`, owner, "probe", hash[:], plaintext[len("dsp_"):len("dsp_")+agentTokenPrefixLength], scopes); err != nil {
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
		// The Inbox's Mark all read is no older row either: it writes the person's own read
		// marks, so it is theirs as the two rows above it are.
		"POST /api/v1/me/inbox/read",
		// The Dismiss beside it writes the person's own dismissal marks, so it is theirs too.
		"POST /api/v1/me/inbox/dismiss",
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
