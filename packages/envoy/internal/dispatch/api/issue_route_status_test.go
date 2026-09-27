package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// fakeRoster is the live-session list a route-status test's listener answers: session id to
// the roles that session holds. A test edits it between reads to start a session, have one
// claim a role, or end one.
type fakeRoster struct {
	mu       sync.Mutex
	sessions map[string][]string
	lookups  atomic.Int32
	down     atomic.Bool
}

func (r *fakeRoster) set(sessionID string, roles ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions == nil {
		r.sessions = map[string][]string{}
	}
	r.sessions[sessionID] = append([]string{}, roles...)
}

func (r *fakeRoster) end(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, sessionID)
}

// routeStatusServer is a Dispatch server whose listener answers GET /v1/sessions from roster,
// counting every call, and answers 503 while roster.down is set.
func routeStatusServer(t *testing.T, roster *fakeRoster) (http.Handler, *store.Store) {
	t.Helper()
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" {
			http.NotFound(w, r)
			return
		}
		roster.lookups.Add(1)
		if roster.down.Load() {
			http.Error(w, "listener restarting", http.StatusServiceUnavailable)
			return
		}
		roster.mu.Lock()
		rows := make([]map[string]any, 0, len(roster.sessions))
		for id, roles := range roster.sessions {
			rows = append(rows, map[string]any{"session_id": id, "title": "worker " + id, "roles": roles})
		}
		roster.mu.Unlock()
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(listener.Close)
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	return handler, database
}

// routedIssue is the part of an issue read (detail or list row) a route-status test asserts on.
type routedIssue struct {
	Key         string  `json:"key"`
	Route       *string `json:"route"`
	RouteStatus *string `json:"route_status"`
	RouteHolder *string `json:"route_holder"`
}

func routedIssueKey(t *testing.T, handler http.Handler, route string) string {
	t.Helper()
	project := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice")
	if project.Code != http.StatusCreated && project.Code != http.StatusConflict {
		t.Fatalf("create project: status=%d body=%s", project.Code, project.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "TEST", "title": "Routed work", "force": true,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	key := decodeBody[routedIssue](t, created).Key
	if route != "" {
		routed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]string{"route": route}, "alice")
		if routed.Code != http.StatusOK {
			t.Fatalf("route %s to %s: status=%d body=%s", key, route, routed.Code, routed.Body.String())
		}
	}
	return key
}

// requireReach reads one issue both ways Dispatch serves it - the detail and its row in the
// project list - and requires each to show the route in the given state (status "" is null) with
// the given holder ("" is null).
func requireReach(t *testing.T, handler http.Handler, key, status, holder string) {
	t.Helper()
	text := func(value *string) string {
		if value == nil {
			return "null"
		}
		return *value
	}
	orNull := func(value string) string {
		if value == "" {
			return "null"
		}
		return value
	}
	want := "route_status=" + orNull(status) + " route_holder=" + orNull(holder)
	reads := func(issue routedIssue) string {
		return "route_status=" + text(issue.RouteStatus) + " route_holder=" + text(issue.RouteHolder)
	}
	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", key, detail.Code, detail.Body.String())
	}
	if got := reads(decodeBody[routedIssue](t, detail)); got != want {
		t.Fatalf("GET /issues/%s reads %s, want %s", key, got, want)
	}
	list := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST", nil, "alice")
	if list.Code != http.StatusOK {
		t.Fatalf("list issues: status=%d body=%s", list.Code, list.Body.String())
	}
	for _, row := range decodeBody[[]routedIssue](t, list) {
		if row.Key != key {
			continue
		}
		if got := reads(row); got != want {
			t.Fatalf("the list row for %s reads %s, want %s", key, got, want)
		}
		return
	}
	t.Fatalf("list has no row for %s", key)
}

func TestRoleRouteReadsNoHolderUntilASessionClaimsTheRole(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-other", "reviewer")
	handler, _ := routeStatusServer(t, roster)
	key := routedIssueKey(t, handler, "role:sre")

	requireReach(t, handler, key, "no_holder", "")

	roster.set("ses-sre", "sre")
	requireReach(t, handler, key, "live", "ses-sre")

	// The holder's session ends: the listener no longer lists it, so nobody holds the role.
	roster.end("ses-sre")
	requireReach(t, handler, key, "no_holder", "")
}

func TestSessionRouteReadsLiveOnlyWhileTheSessionIsListed(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-worker")
	handler, _ := routeStatusServer(t, roster)
	key := routedIssueKey(t, handler, "session:ses-worker")

	requireReach(t, handler, key, "live", "ses-worker")

	roster.end("ses-worker")
	requireReach(t, handler, key, "no_holder", "")
}

func TestRouteStatusIsUnknownWhenTheListenerDoesNotAnswer(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-sre", "sre")
	handler, _ := routeStatusServer(t, roster)
	key := routedIssueKey(t, handler, "role:sre")
	requireReach(t, handler, key, "live", "ses-sre")

	roster.down.Store(true)
	requireReach(t, handler, key, "unknown", "")
}

func TestRouteStatusIsUnknownWithoutAConfiguredListener(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	key := routedIssueKey(t, handler, "role:sre")
	requireReach(t, handler, key, "unknown", "")
}

func TestUnroutedIssueHasNoRouteStatusAndCostsNoListenerCall(t *testing.T) {
	roster := &fakeRoster{}
	handler, _ := routeStatusServer(t, roster)
	key := routedIssueKey(t, handler, "")
	before := roster.lookups.Load()

	requireReach(t, handler, key, "", "")
	if calls := roster.lookups.Load() - before; calls != 0 {
		t.Fatalf("reading an unrouted issue made %d listener calls, want 0", calls)
	}
}

// seedRoutedIssues inserts count open issues into project TEST, alternating a held role, an
// unheld role and a gone session, so a list read resolves every route kind at scale.
func seedRoutedIssues(t *testing.T, handler http.Handler, database *store.Store, count int) {
	t.Helper()
	routedIssueKey(t, handler, "")
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, status, created_by, rank, route)
		select 'TEST-' || (n + 100), 'TEST', n + 100, 'Seeded ' || n, 'todo', '{"kind":"user","id":"alice"}',
		       lpad(n::text, 6, '0'),
		       case n % 3 when 0 then 'role:sre' when 1 then 'role:platform-po' else 'session:ses-gone' end
		from generate_series(1, $1) as n
	`, count); err != nil {
		t.Fatalf("seed %d routed issues: %v", count, err)
	}
}

func TestIssueListResolvesEveryRouteWithOneListenerCall(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-sre", "sre")
	handler, database := routeStatusServer(t, roster)
	seedRoutedIssues(t, handler, database, 200)
	before := roster.lookups.Load()

	list := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST", nil, "alice")
	if list.Code != http.StatusOK {
		t.Fatalf("list issues: status=%d body=%s", list.Code, list.Body.String())
	}
	if calls := roster.lookups.Load() - before; calls != 1 {
		t.Fatalf("listing 200 routed issues made %d listener calls, want exactly 1", calls)
	}
	counts := map[string]int{}
	for _, row := range decodeBody[[]routedIssue](t, list) {
		if row.Route == nil {
			continue
		}
		holder := "null"
		if row.RouteHolder != nil {
			holder = *row.RouteHolder
		}
		counts[*row.Route+" "+*row.RouteStatus+" "+holder]++
	}
	want := map[string]int{
		"role:sre live ses-sre":           66,
		"role:platform-po no_holder null": 67,
		"session:ses-gone no_holder null": 67,
	}
	if len(counts) != len(want) {
		t.Fatalf("route reach counts = %v, want %v", counts, want)
	}
	for shape, count := range want {
		if counts[shape] != count {
			t.Fatalf("route reach counts = %v, want %v", counts, want)
		}
	}
}

func TestRouteStatusFilterListsOnlyOpenIssuesInThatState(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-sre", "sre")
	handler, _ := routeStatusServer(t, roster)
	unheld := routedIssueKey(t, handler, "role:platform-po")
	held := routedIssueKey(t, handler, "role:sre")
	gone := routedIssueKey(t, handler, "session:ses-gone")
	routedIssueKey(t, handler, "")
	closed := routedIssueKey(t, handler, "role:platform-po")
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+closed, map[string]string{"status": "done"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("close %s: status=%d body=%s", closed, response.Code, response.Body.String())
	}

	keys := func(filter string) []string {
		t.Helper()
		before := roster.lookups.Load()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST&route_status="+filter, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("filter %s: status=%d body=%s", filter, response.Code, response.Body.String())
		}
		if calls := roster.lookups.Load() - before; calls != 1 {
			t.Fatalf("filter %s made %d listener calls, want 1", filter, calls)
		}
		var got []string
		for _, row := range decodeBody[[]routedIssue](t, response) {
			if row.RouteStatus == nil || *row.RouteStatus != filter {
				t.Fatalf("filter %s returned %s", filter, row.Key)
			}
			got = append(got, row.Key)
		}
		sort.Strings(got)
		return got
	}
	want := []string{unheld, gone}
	sort.Strings(want)
	if got := keys("no_holder"); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("route_status=no_holder returned %v, want %v", got, want)
	}
	if got := keys("live"); len(got) != 1 || got[0] != held {
		t.Fatalf("route_status=live returned %v, want [%s]", got, held)
	}

	invalid := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST&route_status=held", nil, "alice")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("route_status=held: status=%d body=%s, want 400", invalid.Code, invalid.Body.String())
	}

	// With the listener down no route can be judged, so a filter for a judged state refuses
	// rather than answering an empty list that reads as "every route reaches someone".
	roster.down.Store(true)
	blind := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST&route_status=no_holder", nil, "alice")
	if blind.Code != http.StatusServiceUnavailable {
		t.Fatalf("route_status=no_holder with the listener down: status=%d body=%s, want 503", blind.Code, blind.Body.String())
	}
	if got := keys("unknown"); len(got) != 3 {
		t.Fatalf("route_status=unknown with the listener down returned %v, want the three open routed issues", got)
	}
}
