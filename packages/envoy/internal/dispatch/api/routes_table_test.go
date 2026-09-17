package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

type routeIndex struct {
	Routes []routeIndexEntry `json:"routes"`
	Docs   string            `json:"docs"`
}

func TestRouteIndexListsEveryRouteSortedByPathThenMethod(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, Deps{AgentToken: "agent-token"})

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

// The table is the registration: every row is mounted, every row is described, and the
// count only moves when a route is deliberately added or removed.
func TestRoutesTablePinsEveryRegisteredRoute(t *testing.T) {
	const registeredRoutes = 95
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
		case authPublic, authAny, authHuman, authBearer:
		default:
			t.Fatalf("route %q has auth %q", key, route.Auth)
		}
	}
	if got := len((&server{deps: Deps{TestHooksEnabled: true}}).routes()); got != registeredRoutes+1 {
		t.Fatalf("test hooks add one route: got %d, want %d", got, registeredRoutes+1)
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
		{"{owner}", "owner"},
		{"{repo}", "repo"},
		{"{slug}", "spec"},
		{"{number}", "1"},
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

// authRefusal reports whether a response is the server turning the caller away for who they
// are: 401, or a 403 whose code names the caller kind. Other 4xx/5xx are the handler
// proceeding past the gate on placeholder input.
func authRefusal(response *httptest.ResponseRecorder) (bool, string) {
	switch response.Code {
	case http.StatusUnauthorized:
		return true, "401"
	case http.StatusForbidden:
		var body struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		if body.Code == "HUMAN_ONLY" || body.Code == "REPLY_FORBIDDEN" {
			return true, "403 " + body.Code
		}
	}
	return false, ""
}

// Every row's auth cell is checked against what its handler does to a cookie caller, a bearer,
// and an anonymous request, so GET /api/v1 cannot describe a route the server refuses (or
// admits) differently.
func TestRoutesTableAuthCellsMatchHandlers(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	probe := func(route apiRoute, credential string) *httptest.ResponseRecorder {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		request := httptest.NewRequestWithContext(ctx, route.Method, routeProbePath(route.Pattern), bytes.NewReader([]byte(routeProbeBody(route))))
		request.Header.Set("Content-Type", "application/json")
		switch credential {
		case "cookie":
			request.Header.Set("X-Dispatch-User", "alice")
		case "bearer":
			request.Header.Set("Authorization", "Bearer agent-token")
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
	for _, route := range (&server{}).routes() {
		t.Run(route.Method+" "+route.Pattern, func(t *testing.T) {
			switch route.Auth {
			case authPublic:
				expectAdmitted(t, route, "anonymous")
			case authAny:
				expectRefused(t, route, "anonymous")
				expectAdmitted(t, route, "cookie")
				expectAdmitted(t, route, "bearer")
			case authHuman:
				expectRefused(t, route, "anonymous")
				expectAdmitted(t, route, "cookie")
				expectRefused(t, route, "bearer")
			case authBearer:
				expectRefused(t, route, "anonymous")
				expectRefused(t, route, "cookie")
				expectAdmitted(t, route, "bearer")
			}
		})
	}
}
