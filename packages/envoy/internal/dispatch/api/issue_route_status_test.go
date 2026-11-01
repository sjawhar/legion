package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// fakeRoster is the live-session list a route-status test's listener answers: session id to
// the roles that session holds. A test edits it between reads to start a session, have one
// claim a role, or end one, and sets down or refilling to have the listener refuse the list.
type fakeRoster struct {
	mu        sync.Mutex
	sessions  map[string][]string
	lookups   atomic.Int32
	down      atomic.Bool
	refilling atomic.Bool
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
// counting every call, answers a plain 503 while roster.down is set, and the listener's
// refilling answer with Retry-After 7 while roster.refilling is set. Its roster cache keeps no
// answer (both TTLs zero), so every read asks the listener and a test that edits roster between
// two reads sees the edit at the second; cachedRouteStatusServer is the server a deployment runs.
func routeStatusServer(t *testing.T, roster *fakeRoster) (http.Handler, *store.Store) {
	t.Helper()
	handler, database, served := cachedRouteStatusServer(t, roster)
	served.rosters.ttl, served.rosters.silenceTTL = 0, 0
	return handler, database
}

// cachedRouteStatusServer is routeStatusServer keeping the TTLs register sets, and also returns
// the server, so a test can shorten them before its first read.
func cachedRouteStatusServer(t *testing.T, roster *fakeRoster) (http.Handler, *store.Store, *server) {
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
		if roster.refilling.Load() {
			answerListRefilling(w, "7")
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
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	return handler, database, served
}

// routedIssue is the part of an issue read (detail or list row) a route-status test asserts on.
type routedIssue struct {
	Key               string  `json:"key"`
	Route             *string `json:"route"`
	RouteStatus       *string `json:"route_status"`
	RouteHolder       *string `json:"route_holder"`
	RouteStatusReason *string `json:"route_status_reason"`
}

// reachText is a route's reach as the tests compare it, "<route_status> <route_holder>", each
// "null" when absent.
func reachText(status, holder *string) string {
	text := func(value *string) string {
		if value == nil {
			return "null"
		}
		return *value
	}
	return text(status) + " " + text(holder)
}

// routedIssueKey creates an issue in project TEST, routed when route is not empty, and pins it
// for alice so requireReach can read it from her pinned list too.
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
	if pinned := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+key+"/state", map[string]bool{"pinned": true}, "alice"); pinned.Code != http.StatusOK {
		t.Fatalf("pin %s: status=%d body=%s", key, pinned.Code, pinned.Body.String())
	}
	return key
}

// requireReach reads one issue every way Dispatch serves it - the detail, its row in the
// project list and its row in alice's pinned list - and requires each to show the route in the
// given state (status "" is null) with the given holder and reason ("" is null).
func requireReach(t *testing.T, handler http.Handler, key, status, holder, reason string) {
	t.Helper()
	orNull := func(value string) string {
		if value == "" {
			return "null"
		}
		return value
	}
	text := func(value *string) string {
		if value == nil {
			return "null"
		}
		return *value
	}
	want := "route_status=" + orNull(status) + " route_holder=" + orNull(holder) + " route_status_reason=" + orNull(reason)
	reads := func(issue routedIssue) string {
		return "route_status=" + text(issue.RouteStatus) + " route_holder=" + text(issue.RouteHolder) +
			" route_status_reason=" + text(issue.RouteStatusReason)
	}
	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", key, detail.Code, detail.Body.String())
	}
	if got := reads(decodeBody[routedIssue](t, detail)); got != want {
		t.Fatalf("GET /issues/%s reads %s, want %s", key, got, want)
	}
	for _, listing := range []string{"project=TEST", "pinned=true"} {
		list := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?"+listing, nil, "alice")
		if list.Code != http.StatusOK {
			t.Fatalf("list issues ?%s: status=%d body=%s", listing, list.Code, list.Body.String())
		}
		rows := decodeBody[[]routedIssue](t, list)
		row := slices.IndexFunc(rows, func(issue routedIssue) bool { return issue.Key == key })
		if row < 0 {
			t.Fatalf("?%s has no row for %s", listing, key)
		}
		if got := reads(rows[row]); got != want {
			t.Fatalf("the ?%s row for %s reads %s, want %s", listing, key, got, want)
		}
	}
}

func TestRoleRouteReadsNoHolderUntilASessionClaimsTheRole(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-other", "reviewer")
	handler, _ := routeStatusServer(t, roster)
	key := routedIssueKey(t, handler, "role:sre")

	requireReach(t, handler, key, "no_holder", "", "")

	roster.set("ses-sre", "sre")
	requireReach(t, handler, key, "live", "ses-sre", "")

	// The holder's session ends: the listener no longer lists it, so nobody holds the role.
	roster.end("ses-sre")
	requireReach(t, handler, key, "no_holder", "", "")
}

func TestSessionRouteReadsLiveOnlyWhileTheSessionIsListed(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-worker")
	handler, _ := routeStatusServer(t, roster)
	key := routedIssueKey(t, handler, "session:ses-worker")

	requireReach(t, handler, key, "live", "ses-worker", "")

	roster.end("ses-worker")
	requireReach(t, handler, key, "no_holder", "", "")
}

func TestRouteStatusIsUnknownWhenTheListenerDoesNotAnswer(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-sre", "sre")
	handler, _ := routeStatusServer(t, roster)
	key := routedIssueKey(t, handler, "role:sre")
	requireReach(t, handler, key, "live", "ses-sre", "")

	roster.down.Store(true)
	requireReach(t, handler, key, "unknown", "", "unreachable")
}

// While the listener refuses its list as refilling, the role's holder is still running: the
// route reads unknown with the reason, never no_holder, and reads live again once the list is
// back.
func TestRouteStatusIsUnknownWhileTheListenerRefills(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-sre", "sre")
	handler, _ := routeStatusServer(t, roster)
	key := routedIssueKey(t, handler, "role:sre")
	requireReach(t, handler, key, "live", "ses-sre", "")

	roster.refilling.Store(true)
	requireReach(t, handler, key, "unknown", "", "refilling")

	roster.refilling.Store(false)
	requireReach(t, handler, key, "live", "ses-sre", "")
}

func TestRouteStatusIsUnknownWithoutAConfiguredListener(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	key := routedIssueKey(t, handler, "role:sre")
	requireReach(t, handler, key, "unknown", "", "unreachable")
}

func TestUnroutedIssueHasNoRouteStatusAndCostsNoListenerCall(t *testing.T) {
	roster := &fakeRoster{}
	handler, _ := routeStatusServer(t, roster)
	key := routedIssueKey(t, handler, "")
	before := roster.lookups.Load()

	requireReach(t, handler, key, "", "", "")
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
		counts[*row.Route+" "+reachText(row.RouteStatus, row.RouteHolder)]++
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

	type filtered struct {
		keys    []string
		reasons map[string]int
	}
	list := func(filter string) filtered {
		t.Helper()
		before := roster.lookups.Load()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST&route_status="+filter, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("filter %s: status=%d body=%s", filter, response.Code, response.Body.String())
		}
		if calls := roster.lookups.Load() - before; calls != 1 {
			t.Fatalf("filter %s made %d listener calls, want 1", filter, calls)
		}
		got := filtered{reasons: map[string]int{}}
		for _, row := range decodeBody[[]routedIssue](t, response) {
			if row.RouteStatus == nil || *row.RouteStatus != filter {
				t.Fatalf("filter %s returned %s", filter, row.Key)
			}
			got.keys = append(got.keys, row.Key)
			reason := "null"
			if row.RouteStatusReason != nil {
				reason = *row.RouteStatusReason
			}
			got.reasons[reason]++
		}
		sort.Strings(got.keys)
		return got
	}
	keys := func(filter string) []string {
		t.Helper()
		return list(filter).keys
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
	if blind.Code != http.StatusServiceUnavailable || !strings.Contains(blind.Body.String(), `"code":"ENVOY_UNAVAILABLE"`) {
		t.Fatalf("route_status=no_holder with the listener down: status=%d body=%s, want 503 ENVOY_UNAVAILABLE", blind.Code, blind.Body.String())
	}
	if got := list("unknown"); len(got.keys) != 3 || got.reasons["unreachable"] != 3 {
		t.Fatalf("route_status=unknown with the listener down returned %v, want the three open routed issues, each unreachable", got)
	}

	// A refilling listener refuses the same filters, with its own code and its Retry-After.
	roster.down.Store(false)
	roster.refilling.Store(true)
	for _, filter := range []string{"no_holder", "live"} {
		refused := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST&route_status="+filter, nil, "alice")
		requireEnvoyRefilling(t, refused, "7")
	}
	if got := list("unknown"); len(got.keys) != 3 || got.reasons["refilling"] != 3 {
		t.Fatalf("route_status=unknown while the listener refills returned %v, want the three open routed issues, each refilling", got)
	}
}

// stateRosterServer is a Dispatch server whose listener answers GET /v1/sessions from rows, and
// answers the live rows alone to a read that did not ask for the unresponsive ones - which is
// what the listener does. A roster built from the live-only answer is the bug
// dispatch://INFRA-392 is about: every stalled session reads as ended.
func stateRosterServer(t *testing.T, rows []map[string]any) *server {
	t.Helper()
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" {
			http.NotFound(w, r)
			return
		}
		answer := rows
		if r.URL.Query().Get("include") != "unresponsive" {
			answer = nil
			for _, row := range rows {
				if row["state"] == "live" {
					answer = append(answer, row)
				}
			}
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(listener.Close)
	return &server{deps: Deps{Envoy: envoy.New(listener.URL)}, lifetime: context.Background()}
}

// A session past its live window still holds its role and still receives: what is sent to it is
// stored and waits. So a route to it reads holder_unresponsive, naming that holder, and neither
// live (which would promise a session that answers now) nor no_holder (which the owner audit
// reads as "nobody is working this", and which would have a person hand the work to someone
// else). Both kinds of route answer the same way, since both reach one session.
func TestRouteReachNamesAnUnresponsiveHolderRatherThanDroppingIt(t *testing.T) {
	served := stateRosterServer(t, []map[string]any{
		{"session_id": "ses-live", "state": "live", "last_seen": 1700000000000, "roles": []string{"legion-merger"}},
		{"session_id": "ses-stalled", "state": "unresponsive", "last_seen": 1699999000000, "roles": []string{"legion-planner"}},
	})
	roster := served.listRouteRoster(context.Background())
	if !roster.answered {
		t.Fatalf("the roster did not answer: %v", roster.err)
	}
	for _, tc := range []struct {
		route  string
		status string
		holder string
	}{
		{route: "session:ses-stalled", status: "holder_unresponsive", holder: "ses-stalled"},
		{route: "role:legion-planner", status: "holder_unresponsive", holder: "ses-stalled"},
		{route: "session:ses-live", status: "live", holder: "ses-live"},
		{route: "role:legion-merger", status: "live", holder: "ses-live"},
		{route: "session:ses-ended", status: "no_holder", holder: "null"},
		{route: "role:legion-nobody", status: "no_holder", holder: "null"},
	} {
		reach := roster.reach(&tc.route)
		if got, want := reachText(reach.RouteStatus, reach.RouteHolder), tc.status+" "+tc.holder; got != want {
			t.Errorf("%s reaches %s, want %s", tc.route, got, want)
		}
	}
}

// The route_status filter takes the new state by name, so a reader can list exactly the issues
// whose holder has stopped answering - and no_holder, which is the filter an audit uses to find
// unworked issues, keeps excluding them.
func TestRouteStatusFilterTakesHolderUnresponsive(t *testing.T) {
	for _, value := range []string{"live", "holder_unresponsive", "no_holder", "unknown", ""} {
		if got, err := parseRouteStatusFilter(value); err != nil || got != value {
			t.Errorf("route_status=%q: %q, %v; want it accepted", value, got, err)
		}
	}
	_, err := parseRouteStatusFilter("unresponsive")
	if err == nil || !strings.Contains(err.Error(), "holder_unresponsive") {
		t.Fatalf("route_status=unresponsive: %v, want a refusal naming holder_unresponsive", err)
	}
}

// What an issue read says about the session holding its claim. The read's source is this
// roster, so it answers for a holder the roster lists (live or unresponsive) and reads one it
// does not list as gone; takeable_at is the instant another agent may take the claim without a
// person, which is only ever answered for a holder that will come free.
func TestClaimLivenessDatesWhenAnUnresponsiveHoldersClaimComesFree(t *testing.T) {
	const lastSeen = 1700000000000
	served := stateRosterServer(t, []map[string]any{
		{"session_id": "ses-live", "state": "live", "last_seen": lastSeen},
		{"session_id": "ses-stalled", "state": "unresponsive", "last_seen": lastSeen},
	})
	roster := served.listRouteRoster(context.Background())
	now := time.UnixMilli(lastSeen).Add(7 * time.Minute).UTC()
	claim := func(kind, id string) *model.IssueClaim {
		return &model.IssueClaim{Actor: model.Actor{Kind: kind, ID: id}, At: now}
	}
	text := func(liveness model.IssueClaimLiveness) string {
		part := func(value *time.Time) string {
			if value == nil {
				return "null"
			}
			return value.UTC().Format(time.RFC3339)
		}
		state := "null"
		if liveness.HolderState != nil {
			state = *liveness.HolderState
		}
		return state + " " + part(liveness.LastSeen) + " " + part(liveness.TakeableAt)
	}
	seen := time.UnixMilli(lastSeen).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name  string
		claim *model.IssueClaim
		want  string
	}{
		// A live holder's claim is not takeable by waiting, so there is no instant to name.
		{name: "a live holder", claim: claim("session", "ses-live"), want: "live " + seen + " null"},
		// Twenty minutes past the heartbeat it was last seen at, and not before.
		{name: "an unresponsive holder", claim: claim("session", "ses-stalled"),
			want: "unresponsive " + seen + " " + time.UnixMilli(lastSeen).Add(20*time.Minute).UTC().Format(time.RFC3339)},
		// No record at all: gone, takeable as of this read.
		{name: "a holder the registry has no record of", claim: claim("session", "ses-ended"),
			want: "gone null " + now.Format(time.RFC3339)},
		// A person holds it until a person releases it; there is no session state to read.
		{name: "a human holder", claim: claim("user", "alice@example.com"), want: "null null null"},
		{name: "nobody", claim: nil, want: "null null null"},
	} {
		if got := text(roster.claimLiveness(tc.claim, now)); got != tc.want {
			t.Errorf("%s: holder_state/last_seen/takeable_at = %s, want %s", tc.name, got, tc.want)
		}
	}
	// A listener that gave no list cannot judge the holder, and an issue read still answers:
	// unknown, never gone, which would read as a free claim.
	silent := routeRoster{reason: model.RouteReasonUnreachable}
	if got := text(silent.claimLiveness(claim("session", "ses-live"), now)); got != "unknown null null" {
		t.Fatalf("with no list to judge by: %s, want unknown null null", got)
	}
}

// A route is judged against the stored listener answer, the route_roster row. When the database
// cannot read that row, the failure is the database's: a filter for a judged state answers it as
// one, never as the listener's silence (503 ENVOY_UNAVAILABLE, whose remedy - route_status=unknown
// - points an operator at the listener). A read that only shows a route still serves the issue,
// with its route unknown.
func TestARouteStatusFilterAnswersAFailedRosterReadAsTheDatabases(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-sre", "sre")
	handler, database := routeStatusServer(t, roster)
	key := routedIssueKey(t, handler, "role:sre")
	requireReach(t, handler, key, "live", "ses-sre", "")

	if _, err := database.Pool.Exec(context.Background(), `alter table route_roster rename to route_roster_unreadable`); err != nil {
		t.Fatalf("make the route_roster row unreadable: %v", err)
	}
	for _, filter := range []string{"live", "no_holder"} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST&route_status="+filter, nil, "alice")
		code := decodeBody[struct {
			Code string `json:"code"`
		}](t, response).Code
		if response.Code != http.StatusInternalServerError || code == "ENVOY_UNAVAILABLE" {
			t.Fatalf("route_status=%s with the roster unreadable: status=%d body=%s, want 500 naming no listener failure",
				filter, response.Code, response.Body.String())
		}
	}
	requireReach(t, handler, key, "unknown", "", "unreachable")
}

// secondRouteStatusServer is a second server process beside served: the same database and the
// same listener, but a pool of its own (storetest.SecondProcess), a roster cache of its own with
// served's TTLs and fetch bound, and its own single-flight fetch, so the two share only what
// Postgres holds, as two envoy-dispatch processes do.
func secondRouteStatusServer(t *testing.T, served *server) (http.Handler, *server) {
	t.Helper()
	deps := served.deps
	deps.Store = storetest.SecondProcess(t, served.deps.Store)
	mux := http.NewServeMux()
	other := register(mux, served.lifetime, deps)
	t.Cleanup(func() { waitBackgroundOf(t, other) })
	other.rosters.ttl, other.rosters.silenceTTL = served.rosters.ttl, served.rosters.silenceTTL
	other.rosters.timeout = served.rosters.timeout
	return mux, other
}

// Two server processes judge a route against one listener answer: the one the first stored
// serves the second for its TTL, so a route the first reads live is never read as reaching nobody
// on the second because the second asked the listener a moment later, and the second makes no
// call of its own. Both TTLs are a minute, so a slow machine's reads stay inside them.
func TestTwoServersJudgeARouteAgainstTheOneStoredListenerAnswer(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-sre", "sre")
	handler, _, served := cachedRouteStatusServer(t, roster)
	served.rosters.ttl = time.Minute
	other, _ := secondRouteStatusServer(t, served)
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	if got := detailReach(t, handler, key); got != "live ses-sre" {
		t.Fatalf("the first server reads %s, want live ses-sre", got)
	}
	roster.end("ses-sre")
	if got := detailReach(t, other, key); got != "live ses-sre" {
		t.Fatalf("the second server reads %s inside the TTL of the answer the first stored, want live ses-sre", got)
	}
	if got := detailReach(t, handler, key); got != "live ses-sre" {
		t.Fatalf("the first server reads %s on its second read, want live ses-sre", got)
	}
	if calls := roster.lookups.Load() - before; calls != 1 {
		t.Fatalf("three reads on two servers inside the TTL made %d listener calls, want 1", calls)
	}
}

// A listener failure one server meets is every server's: the stored silence has the second read
// unknown too, without a call of its own, even though the listener answers again by then. The
// silence is a minute, so a slow machine's reads stay inside it.
func TestAListenerFailureOneServerMetReadsUnknownOnTheOther(t *testing.T) {
	roster := &fakeRoster{}
	roster.down.Store(true)
	handler, _, served := cachedRouteStatusServer(t, roster)
	served.rosters.ttl, served.rosters.silenceTTL = time.Minute, time.Minute
	other, _ := secondRouteStatusServer(t, served)
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	if got := detailReach(t, handler, key); got != "unknown null" {
		t.Fatalf("the first server reads %s with the listener down, want unknown null", got)
	}
	roster.down.Store(false)
	roster.set("ses-sre", "sre")
	if got := detailReach(t, other, key); got != "unknown null" {
		t.Fatalf("the second server reads %s inside the silence the first stored, want unknown null", got)
	}
	blind := dispatchRequest(t, other, http.MethodGet, "/api/v1/issues?project=TEST&route_status=no_holder", nil, "alice")
	if blind.Code != http.StatusServiceUnavailable {
		t.Fatalf("route_status=no_holder on the second server inside the silence: status=%d body=%s, want 503", blind.Code, blind.Body.String())
	}
	if calls := roster.lookups.Load() - before; calls != 1 {
		t.Fatalf("three reads on two servers inside the silence made %d listener calls, want 1", calls)
	}
}

// A listener answer is stored only over an older one, by when its call was sent: a fetch that was
// sent first and answers last does not replace the answer a later fetch on the other server
// stored, and the reads waiting on it are served that newer answer. The listener holds its first
// call and answers it with nobody; every later call lists ses-sre holding sre.
func TestAnOlderListenerAnswerNeverReplacesANewerOne(t *testing.T) {
	var calls atomic.Int32
	gate := make(chan struct{})
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) == 1 {
			<-gate
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[{"session_id":"ses-sre","title":"worker","roles":["sre"]}]`))
	}))
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	// Release before the close, which waits for every request in flight to finish.
	t.Cleanup(listener.Close)
	t.Cleanup(release)
	_, _, _, first := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL, envoyTimeout: time.Minute})
	first.rosters.ttl, first.rosters.timeout = time.Minute, time.Minute
	_, second := secondRouteStatusServer(t, first)

	older := make(chan routeRoster, 1)
	go func() { older <- readRoster(t, first, context.Background()) }()
	awaitCalls(t, &calls, 1)
	if got := sreReach(readRoster(t, second, context.Background())); got != "live ses-sre" {
		t.Fatalf("the second server's own fetch reads %s, want live ses-sre", got)
	}
	release()
	select {
	case roster := <-older:
		if got := sreReach(roster); got != "live ses-sre" {
			t.Fatalf("the read waiting on the older fetch reads %s, want the newer stored live ses-sre", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the read waiting on the older fetch never returned")
	}
	if got := sreReach(readRoster(t, first, context.Background())); got != "live ses-sre" {
		t.Fatalf("the first server's next read reads %s, want the newer stored live ses-sre", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("the listener saw %d calls, want 2: one fetch per server", got)
	}
}
