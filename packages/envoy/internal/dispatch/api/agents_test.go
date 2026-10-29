package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func agentsHandler(t *testing.T, envoyURL string) http.Handler {
	t.Helper()
	return agentsHandlerWithStore(t, envoyURL, storetest.Open(t))
}

func agentsHandlerWithStore(t *testing.T, envoyURL string, database *store.Store) http.Handler {
	t.Helper()
	deps := Deps{
		Store:       database,
		Identity:    headerIdentity(database),
		AgentTokens: sharedAgentTokens(t, "agent-token"),
		Envoy:       envoy.New(envoyURL),
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
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

// GET /api/v1/agents carries each session's real machine (host) wherever the listener reported
// it, and keeps machine_id (the listener that owns the session's delivery) untouched beside it.
func TestAgentsReportsHostFromListener(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"session_id":"a","title":"planner","machine_id":"example-listener","host":"example-host-a","last_seen":100}]`))
	}))
	defer listener.Close()

	response := dispatchRequest(t, agentsHandler(t, listener.URL), http.MethodGet, "/api/v1/agents", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	rows := decodeBody[[]map[string]any](t, response)
	if rows[0]["machine_id"] != "example-listener" {
		t.Fatalf("machine_id = %v, want the routing listener's own machine unchanged", rows[0]["machine_id"])
	}
	if rows[0]["host"] != "example-host-a" {
		t.Fatalf("host = %v, want the listener-reported host", rows[0]["host"])
	}
}

// A listener that predates the "host" field answers with none; Dispatch must still show a
// machine rather than an empty one during a partial rollout.
func TestAgentsFallsBackToMachineIDWhenListenerReportsNoHost(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"session_id":"a","title":"planner","machine_id":"example-listener","last_seen":100}]`))
	}))
	defer listener.Close()

	response := dispatchRequest(t, agentsHandler(t, listener.URL), http.MethodGet, "/api/v1/agents", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	rows := decodeBody[[]map[string]any](t, response)
	if rows[0]["host"] != "example-listener" {
		t.Fatalf("host = %v, want the machine_id fallback", rows[0]["host"])
	}
}

func TestAgentsIncludeGroupedOpenAskCountsAndLastActivity(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"session_id":"planner","title":"Planner","dir":"/w/planner","machine_id":"host-a","roles":["planner"],"capabilities":["btw"],"last_seen":200},
			{"session_id":"reviewer","title":"Reviewer","dir":"/w/reviewer","machine_id":"host-b","roles":[],"capabilities":[],"last_seen":100}
		]`))
	}))
	defer listener.Close()

	database := storetest.Open(t)
	ctx := context.Background()
	// TEST-2's open ask on a closed issue is what a close by a release older than the withdrawal
	// leaves during a roll, until the repair withdraws it: the counts still leave it out.
	for _, query := range []string{
		`insert into projects (key, name) values ('TEST', 'Test')`,
		`insert into issues (key, project_key, number, title, status, created_by, rank) values ('TEST-1', 'TEST', 1, 'Test issue', 'todo', '{"kind":"user","id":"alice"}', 'U')`,
		`insert into issues (key, project_key, number, title, status, created_by, rank, closed_at) values ('TEST-2', 'TEST', 2, 'Closed issue', 'done', '{"kind":"user","id":"alice"}', 'V', '2026-09-13T11:00:00Z')`,
		`insert into asks (issue_key, author, question, state) values
			('TEST-1', '{"kind":"session","id":"planner"}', 'First open question', 'open'),
			('TEST-1', '{"kind":"session","id":"planner"}', 'Second open question', 'open'),
			('TEST-2', '{"kind":"session","id":"planner"}', 'Closed issue question', 'open')`,
		`insert into events (issue_key, seq, type, actor, payload, notify, created_at) values
			('TEST-1', 1, 'message.created', '{"kind":"session","id":"planner"}', '{}', false, '2026-09-13T12:00:00Z'),
			('TEST-1', 2, 'message.created', '{"kind":"session","id":"planner"}', '{}', false, '2026-09-13T13:00:00Z')`,
	} {
		if _, err := database.Pool.Exec(ctx, query); err != nil {
			t.Fatalf("seed agent enrichment data: %v", err)
		}
	}

	response := dispatchRequest(
		t,
		agentsHandlerWithStore(t, listener.URL, database),
		http.MethodGet,
		"/api/v1/agents",
		nil,
		"alice",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	rows := decodeBody[[]struct {
		SessionID    string  `json:"session_id"`
		OpenAsks     int     `json:"open_asks"`
		LastActivity *string `json:"last_activity"`
	}](t, response)
	if len(rows) != 2 {
		t.Fatalf("rows = %#v, want two live sessions", rows)
	}
	if rows[0].SessionID != "planner" || rows[0].OpenAsks != 2 {
		t.Fatalf("planner row = %#v, want two open asks from the open issue", rows[0])
	}
	if rows[0].LastActivity == nil || *rows[0].LastActivity != "2026-09-13T13:00:00Z" {
		t.Fatalf("planner activity = %#v, want newest session event", rows[0].LastActivity)
	}
	if rows[1].SessionID != "reviewer" || rows[1].OpenAsks != 0 || rows[1].LastActivity != nil {
		t.Fatalf("reviewer row = %#v, want no open asks or activity", rows[1])
	}
}

// A bearer session needs to see who advertises which delivery modes before it targets a
// message; the issue-less POST /agents/{session}/messages stays human-only.
func TestAgentsListsCapabilitiesForBearerCallers(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","last_seen":100,"capabilities":["aside","btw"]}]`))
	}))
	defer listener.Close()
	handler := agentsHandler(t, listener.URL)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	rows := decodeBody[[]map[string]any](t, response)
	if len(rows) != 1 || !reflect.DeepEqual(rows[0]["capabilities"], []any{"aside", "btw"}) {
		t.Fatalf("rows = %v", rows)
	}

	response = bearerRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "hi", "delivery": "btw",
		"actor": map[string]string{"kind": "session", "id": "s2"},
	})
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("issue-less targeted message by a bearer: status %d: %s", response.Code, response.Body.String())
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

// agentIDs reads GET /api/v1/agents and answers the session ids it lists, in its order.
func agentIDs(t *testing.T, handler http.Handler) []string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list agents: status %d: %s", response.Code, response.Body.String())
	}
	rows := decodeBody[[]struct {
		SessionID string `json:"session_id"`
	}](t, response)
	ids := make([]string, len(rows))
	for index, row := range rows {
		ids[index] = row.SessionID
	}
	return ids
}

// sessionsListener is a listener whose GET /v1/sessions answers the JSON list answer. calls counts
// the requests that reached it.
func sessionsListener(t *testing.T, answer string) (url string, calls *atomic.Int32) {
	t.Helper()
	calls = &atomic.Int32{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(listener.Close)
	return listener.URL, calls
}

// The agents list and issue reads share the process's one copy of the listener's sessions: an
// agents list, an issue read and a second agents list inside the TTL make one listener call, and
// the second list is the first answer even though the listener's own answer has changed since.
func TestAgentsListsAndIssueReadsWithinTheTTLShareOneListenerCall(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-sre", "sre")
	handler, _, served := cachedRouteStatusServer(t, roster)
	// A minute, not the deployment's five seconds, so a slow machine's reads stay inside it.
	served.rosters.ttl = time.Minute
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	if got := agentIDs(t, handler); !reflect.DeepEqual(got, []string{"ses-sre"}) {
		t.Fatalf("first agents list = %v, want [ses-sre]", got)
	}
	if got := detailReach(t, handler, key); got != "live ses-sre" {
		t.Fatalf("issue read after the agents list: %s, want live ses-sre", got)
	}
	roster.end("ses-sre")
	roster.set("ses-new", "sre")
	if got := agentIDs(t, handler); !reflect.DeepEqual(got, []string{"ses-sre"}) {
		t.Fatalf("second agents list inside the TTL = %v, want the cached [ses-sre]", got)
	}
	if calls := roster.lookups.Load() - before; calls != 1 {
		t.Fatalf("two agents lists and an issue read inside the TTL made %d listener calls, want 1", calls)
	}
}

// A listener that fails answers every agents list 503 ENVOY_UNAVAILABLE with the fetch's own
// error, and is asked once within the silence window: the second list answers the kept failure
// without asking again. A list after the window asking the listener again is the cache's own
// rule (issue_route_status_cache_test.go). The window is a minute, so a loaded machine's second
// list still lands inside it.
func TestAgentsAnswer503ThroughTheSilenceWindow(t *testing.T) {
	roster := &fakeRoster{}
	roster.down.Store(true)
	handler, _, served := cachedRouteStatusServer(t, roster)
	served.rosters.ttl, served.rosters.silenceTTL = time.Minute, time.Minute
	before := roster.lookups.Load()

	for read := 1; read <= 2; read++ {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents", nil, "alice")
		body := response.Body.String()
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"ENVOY_UNAVAILABLE"`) ||
			!strings.Contains(body, "GET /v1/sessions returned 503") {
			t.Fatalf("agents list %d with the listener down: status %d: %s, want 503 ENVOY_UNAVAILABLE carrying the fetch's error", read, response.Code, body)
		}
	}
	if calls := roster.lookups.Load() - before; calls != 1 {
		t.Fatalf("two agents lists inside the silence window made %d listener calls, want 1", calls)
	}
}

// Every read the cache serves shares one session list, so agents lists and issue reads running
// together leave it as the listener sent it: no list sorts it or fills in a host in place (the
// race detector catches the write itself), and each list still comes out newest first with the
// machine_id standing in for a missing host. The listener sends the sessions oldest first, so the
// sorted order is the reverse of the cached one.
func TestConcurrentAgentsListsAndIssueReadsLeaveTheSharedListAsTheListenerSentIt(t *testing.T) {
	listenerURL, calls := sessionsListener(t, `[
		{"session_id":"a","title":"oldest","machine_id":"box-a","roles":["sre"],"last_seen":100},
		{"session_id":"b","title":"middle","machine_id":"box-b","host":"host-b","last_seen":200},
		{"session_id":"c","title":"newest","machine_id":"box-c","last_seen":300}
	]`)
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listenerURL})
	served.rosters.ttl = time.Minute
	key := routedIssueKey(t, handler, "role:sre")

	const readers = 8
	agents := make([]*httptest.ResponseRecorder, readers)
	issues := make([]*httptest.ResponseRecorder, readers)
	start := make(chan struct{})
	var done sync.WaitGroup
	for index := range readers {
		done.Add(2)
		go func() {
			defer done.Done()
			<-start
			agents[index] = dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents", nil, "alice")
		}()
		go func() {
			defer done.Done()
			<-start
			issues[index] = dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
		}()
	}
	close(start)
	done.Wait()

	type agentRow struct {
		SessionID string `json:"session_id"`
		Host      string `json:"host"`
	}
	want := []agentRow{{"c", "box-c"}, {"b", "host-b"}, {"a", "box-a"}}
	for index, response := range agents {
		if response.Code != http.StatusOK {
			t.Fatalf("agents list %d: status %d: %s", index, response.Code, response.Body.String())
		}
		if got := decodeBody[[]agentRow](t, response); !reflect.DeepEqual(got, want) {
			t.Fatalf("agents list %d = %v, want %v", index, got, want)
		}
	}
	for index, response := range issues {
		if response.Code != http.StatusOK {
			t.Fatalf("issue read %d: status %d: %s", index, response.Code, response.Body.String())
		}
		issue := decodeBody[routedIssue](t, response)
		if got := reachText(issue.RouteStatus, issue.RouteHolder); got != "live a" {
			t.Fatalf("issue read %d: %s, want live a", index, got)
		}
	}

	served.rosters.mu.Lock()
	cached := served.rosters.roster.sessions
	served.rosters.mu.Unlock()
	sent := make([]agentRow, len(cached))
	for index, session := range cached {
		sent[index] = agentRow{session.SessionID, session.Host}
	}
	if wantSent := []agentRow{{"a", ""}, {"b", "host-b"}, {"c", ""}}; !reflect.DeepEqual(sent, wantSent) {
		t.Fatalf("the cached session list is %v after the reads, want the listener's own %v", sent, wantSent)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("%d agents lists and %d issue reads inside the TTL made %d listener calls, want 1", readers, readers, got)
	}
}

// An agents list whose request ends while it waits on the shared fetch answers 503
// ENVOY_UNAVAILABLE at once, saying why; the fetch runs on, and the next list is its answer.
func TestAnAgentsListWhoseRequestEndsWhileItWaitsOnTheFetchAnswers503(t *testing.T) {
	database := storetest.Open(t)
	mux := http.NewServeMux()
	_, calls, release := heldRosterMux(t, mux, Deps{Store: database, Identity: headerIdentity(database)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil).WithContext(ctx)
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		mux.ServeHTTP(response, request)
	}()
	awaitCalls(t, calls, 1)
	cancel()
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("an agents list did not answer within 5 s of its request ending")
	}
	body := response.Body.String()
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"ENVOY_UNAVAILABLE"`) ||
		!strings.Contains(body, "context canceled") {
		t.Fatalf("an agents list whose request ended: status %d: %s, want 503 ENVOY_UNAVAILABLE saying the request ended", response.Code, body)
	}

	release()
	if got := agentIDs(t, mux); !reflect.DeepEqual(got, []string{"ses-sre"}) {
		t.Fatalf("the next agents list = %v, want the fetch's answer [ses-sre]", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("two agents lists made %d listener calls, want 1", got)
	}
}

// While the process drains after a shutdown, the shared fetch, which runs on the process's
// lifetime, cannot answer; an agents list the drain still serves asks a healthy listener on its
// own request and lists its sessions, whether the cache was cold or already holds the shutdown's
// failure.
func TestAnAgentsListDuringAShutdownDrainListsAHealthyListenersSessions(t *testing.T) {
	listenerURL, _ := sessionsListener(t, `[{"session_id":"ses-live","title":"worker","last_seen":100}]`)
	database := storetest.Open(t)
	mux := http.NewServeMux()
	// The process's lifetime has already ended: the server is draining after a shutdown.
	lifetime, shutdown := context.WithCancel(context.Background())
	shutdown()
	register(mux, lifetime, Deps{Store: database, Identity: headerIdentity(database), Envoy: envoy.New(listenerURL)})

	for read := 1; read <= 2; read++ {
		if got := agentIDs(t, mux); !reflect.DeepEqual(got, []string{"ses-live"}) {
			t.Fatalf("agents list %d during the drain = %v, want the listener's [ses-live]", read, got)
		}
	}
}
