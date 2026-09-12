package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func TestMatchingTopics(t *testing.T) {
	base := "notifications.dispatch.issue.T-1"
	cases := []struct {
		name   string
		topics []string
		want   []string
	}{
		{
			name:   "bare topic alone never matches a typed event",
			topics: []string{base},
			want:   []string{},
		},
		{
			name:   "the paired wildcard matches",
			topics: []string{base, base + ".>"},
			want:   []string{base + ".>"},
		},
		{
			name:   "a broader dispatch-wide wildcard matches",
			topics: []string{"notifications.dispatch.>"},
			want:   []string{"notifications.dispatch.>"},
		},
		{
			name:   "an unrelated issue's topic does not match",
			topics: []string{"notifications.dispatch.issue.T-2.>"},
			want:   []string{},
		},
		{
			name:   "the session's own inbox topic does not match",
			topics: []string{"notifications.agent.ses_1"},
			want:   []string{},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := matchingTopics(testCase.topics, base); !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("matchingTopics(%v, %q) = %v, want %v", testCase.topics, base, got, testCase.want)
			}
		})
	}
}

// fakeListener is a minimal stand-in for the Envoy listener's interests and
// sessions surface (cmd/listener/api.go), stateful enough to prove the
// unsubscribe flow round-trips through it.
type fakeListener struct {
	mu              sync.Mutex
	interests       map[string][]string
	sessions        []map[string]any
	calls           []unsubscribeCall
	failUnsubscribe bool
}

type unsubscribeCall struct {
	sessionID string
	topics    []string
}

func newFakeListener(t *testing.T, interests map[string][]string, sessions []map[string]any) (string, *fakeListener) {
	t.Helper()
	if interests == nil {
		interests = map[string][]string{}
	}
	state := &fakeListener{interests: interests, sessions: sessions}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/interests/unsubscribe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SessionID string   `json:"session_id"`
			Topics    []string `json:"topics"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		state.mu.Lock()
		if state.failUnsubscribe {
			state.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		cut := map[string]bool{}
		for _, topic := range body.Topics {
			cut[topic] = true
		}
		next := make([]string, 0, len(state.interests[body.SessionID]))
		for _, topic := range state.interests[body.SessionID] {
			if !cut[topic] {
				next = append(next, topic)
			}
		}
		state.interests[body.SessionID] = next
		state.calls = append(state.calls, unsubscribeCall{sessionID: body.SessionID, topics: body.Topics})
		state.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string][]string{"removed": body.Topics})
	})
	mux.HandleFunc("/v1/interests/", func(w http.ResponseWriter, r *http.Request) {
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/interests/")
		state.mu.Lock()
		defer state.mu.Unlock()
		if sessionID == "" {
			rows := make([]map[string]any, 0, len(state.interests))
			for id, topics := range state.interests {
				rows = append(rows, map[string]any{"session_id": id, "topics": topics, "updated_at": 1700000000000})
			}
			writeJSON(w, http.StatusOK, rows)
			return
		}
		topics, ok := state.interests[sessionID]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"session_id": sessionID, "topics": topics, "updated_at": 1700000000000})
	})
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		writeJSON(w, http.StatusOK, state.sessions)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL, state
}

func (f *fakeListener) unsubscribeCalls() []unsubscribeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]unsubscribeCall(nil), f.calls...)
}

// newSubscribersHandler registers a fresh API mux against database, optionally
// pointed at a fake Envoy listener. Called twice against the same database —
// once with no listener to mint an issue/document key or slug, again with a
// fake listener seeded using that now-known key — since a request needs
// deps.Envoy fixed at Register time but the issue key isn't known until after
// creating it.
func newSubscribersHandler(t *testing.T, database *store.Store, envoyURL string) http.Handler {
	t.Helper()
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{
		Store: database, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	deps, err := NewDeps(DepsInput{
		Store: database,
		Identity: identity.HeaderIdentity{
			Header:        "X-Dispatch-User",
			AllowedLogins: map[string]struct{}{"alice": {}},
		},
		AgentToken: "agent-token",
		ServerURL:  "https://dispatch.example",
		Docs:       documentService,
		Events:     broker,
		EnvoyURL:   envoyURL,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	return mux
}

func createTestIssue(t *testing.T, handler http.Handler, project, title string) string {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": project, "name": project,
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": project, "title": title,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
}

func TestListIssueSubscribersFiltersEnrichesAndSortsByLastSeen(t *testing.T) {
	database := openEmptyTestStore(t)
	key := createTestIssue(t, newSubscribersHandler(t, database, ""), "TEST", "Issue")
	envoyURL, _ := newFakeListener(t, map[string][]string{
		"live":      {"notifications.dispatch.issue." + key, "notifications.dispatch.issue." + key + ".>"},
		"gone":      {"notifications.dispatch.issue." + key + ".>"},
		"elsewhere": {"notifications.dispatch.issue.OTHER-1.>"},
	}, []map[string]any{
		{"session_id": "live", "title": "planner", "last_seen": 1800000000000},
	})
	handler := newSubscribersHandler(t, database, envoyURL)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/subscribers", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	rows := decodeBody[[]map[string]any](t, response)
	if len(rows) != 2 {
		t.Fatalf("subscribers = %#v, want 2 rows (elsewhere excluded)", rows)
	}
	if rows[0]["session_id"] != "live" || rows[0]["live"] != true || rows[0]["title"] != "planner" {
		t.Fatalf("live row = %#v", rows[0])
	}
	if rows[1]["session_id"] != "gone" || rows[1]["live"] != false {
		t.Fatalf("dead row = %#v", rows[1])
	}
	liveTopics, _ := rows[0]["topics"].([]any)
	if len(liveTopics) != 1 || liveTopics[0] != "notifications.dispatch.issue."+key+".>" {
		t.Fatalf("live topics = %#v, want only the wildcard form", liveTopics)
	}
}

func TestListIssueSubscribersRejectsBearerCallers(t *testing.T) {
	database := openEmptyTestStore(t)
	key := createTestIssue(t, newSubscribersHandler(t, database, ""), "TEST", "Issue")
	envoyURL, _ := newFakeListener(t, nil, nil)
	handler := newSubscribersHandler(t, database, envoyURL)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/issues/"+key+"/subscribers", nil)
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestListIssueSubscribersReportsEnvoyUnavailable(t *testing.T) {
	database := openEmptyTestStore(t)
	handler := newSubscribersHandler(t, database, "")
	key := createTestIssue(t, handler, "TEST", "Issue")

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/subscribers", nil, "alice")
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"ENVOY_UNAVAILABLE"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestListArtifactSubscribersRejectsIssueLinkedArtifacts(t *testing.T) {
	database := openEmptyTestStore(t)
	bootstrap := newSubscribersHandler(t, database, "")
	key := createTestIssue(t, bootstrap, "TEST", "Issue")
	uploaded := dispatchRequest(t, bootstrap, http.MethodPost, "/api/v1/issues/"+key+"/artifacts", map[string]string{
		"name": "notes.md", "content": "# Notes\n",
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload artifact: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	artifactID := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, uploaded).Artifact.ID
	envoyURL, _ := newFakeListener(t, nil, nil)
	handler := newSubscribersHandler(t, database, envoyURL)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/subscribers", nil, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"ARTIFACT_LINKED"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestListArtifactSubscribersForUnlinkedDocument(t *testing.T) {
	database := openEmptyTestStore(t)
	bootstrap := newSubscribersHandler(t, database, "")
	if response := dispatchRequest(t, bootstrap, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	document := createProjectDocument(t, bootstrap, "CORE", "Design notes", "# Design notes\n")
	envoyURL, _ := newFakeListener(t, map[string][]string{
		"writer": {"notifications.dispatch.document." + document.Project + "." + document.Slug + ".>"},
	}, nil)
	handler := newSubscribersHandler(t, database, envoyURL)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+document.ID+"/subscribers", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	rows := decodeBody[[]map[string]any](t, response)
	if len(rows) != 1 || rows[0]["session_id"] != "writer" {
		t.Fatalf("subscribers = %#v", rows)
	}
}

func TestUnsubscribeIssueSessionRemovesTopicsAppendsEventAndNotifiesTheSession(t *testing.T) {
	database := openEmptyTestStore(t)
	bootstrap := newSubscribersHandler(t, database, "")
	key := createTestIssue(t, bootstrap, "TEST", "Issue")
	envoyURL, listener := newFakeListener(t, map[string][]string{
		"planner": {"notifications.dispatch.issue." + key, "notifications.dispatch.issue." + key + ".>"},
	}, []map[string]any{{"session_id": "planner", "title": "planner", "last_seen": 200}})
	handler := newSubscribersHandler(t, database, envoyURL)

	response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/subscribers/planner", nil, "alice")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	if calls := listener.unsubscribeCalls(); len(calls) != 1 ||
		calls[0].sessionID != "planner" ||
		!reflect.DeepEqual(calls[0].topics, []string{"notifications.dispatch.issue." + key + ".>"}) {
		t.Fatalf("listener unsubscribe calls = %#v", calls)
	}

	log := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, "alice")
	events := decodeBody[[]struct {
		Type    string `json:"type"`
		Notify  bool   `json:"notify"`
		Payload struct {
			SessionID string      `json:"session_id"`
			By        model.Actor `json:"by"`
			Topics    []string    `json:"topics"`
		} `json:"payload"`
	}](t, log)
	removed := events[len(events)-1]
	if removed.Type != "subscription.removed" || !removed.Notify {
		t.Fatalf("last event = %#v, want a notifying subscription.removed", removed)
	}
	if removed.Payload.SessionID != "planner" || removed.Payload.By != (model.Actor{Kind: "user", ID: "alice"}) ||
		!reflect.DeepEqual(removed.Payload.Topics, []string{"notifications.dispatch.issue." + key + ".>"}) {
		t.Fatalf("subscription.removed payload = %#v", removed.Payload)
	}

	remaining := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/subscribers", nil, "alice")
	if rows := decodeBody[[]map[string]any](t, remaining); len(rows) != 0 {
		t.Fatalf("subscribers after unsubscribe = %#v, want none", rows)
	}
}

func TestUnsubscribeRefusesABroadWildcardSubscriptionAndLeavesItIntact(t *testing.T) {
	database := openEmptyTestStore(t)
	bootstrap := newSubscribersHandler(t, database, "")
	key1 := createTestIssue(t, bootstrap, "TEST", "Issue one")
	created2 := dispatchRequest(t, bootstrap, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue two",
	}, "alice")
	if created2.Code != http.StatusCreated {
		t.Fatalf("create second issue: status=%d body=%s", created2.Code, created2.Body.String())
	}
	key2 := decodeBody[struct {
		Key string `json:"key"`
	}](t, created2).Key
	// "wide" is subscribed only via a wildcard broader than either issue: removing
	// it from key1 must not also unsubscribe it from key2 or anything else.
	envoyURL, listener := newFakeListener(t, map[string][]string{
		"wide": {"notifications.dispatch.>"},
	}, nil)
	handler := newSubscribersHandler(t, database, envoyURL)

	before := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key1+"/subscribers", nil, "alice")
	rows := decodeBody[[]map[string]any](t, before)
	if len(rows) != 1 || rows[0]["session_id"] != "wide" || rows[0]["removable"] != false ||
		rows[0]["via"] != "notifications.dispatch.>" {
		t.Fatalf("subscribers before = %#v, want the wide subscriber marked non-removable", rows)
	}

	response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key1+"/subscribers/wide", nil, "alice")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"NOT_REMOVABLE"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if calls := listener.unsubscribeCalls(); len(calls) != 0 {
		t.Fatalf("listener unsubscribe calls = %#v, want none — the wildcard must never be removed", calls)
	}

	after := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key2+"/subscribers", nil, "alice")
	if rows := decodeBody[[]map[string]any](t, after); len(rows) != 1 || rows[0]["session_id"] != "wide" {
		t.Fatalf("subscribers of the other issue after the refused unsubscribe = %#v, want \"wide\" still present", rows)
	}
}

func TestUnsubscribeCommitsItsAuditEventBeforeAListenerFailureAndRetriesIdempotently(t *testing.T) {
	database := openEmptyTestStore(t)
	bootstrap := newSubscribersHandler(t, database, "")
	key := createTestIssue(t, bootstrap, "TEST", "Issue")
	envoyURL, listener := newFakeListener(t, map[string][]string{
		"planner": {"notifications.dispatch.issue." + key, "notifications.dispatch.issue." + key + ".>"},
	}, nil)
	listener.failUnsubscribe = true
	handler := newSubscribersHandler(t, database, envoyURL)

	response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/subscribers/planner", nil, "alice")
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), `"code":"ENVOY_UNSUBSCRIBE_FAILED"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	log := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, "alice")
	if !strings.Contains(log.Body.String(), `"type":"subscription.remove_requested"`) {
		t.Fatalf("event log did not record the durable unsubscribe command: %s", log.Body.String())
	}
	remaining := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/subscribers", nil, "alice")
	if rows := decodeBody[[]map[string]any](t, remaining); len(rows) != 1 || rows[0]["session_id"] != "planner" {
		t.Fatalf("subscribers after the failed unsubscribe = %#v, want \"planner\" still intact", rows)
	}

	listener.failUnsubscribe = false
	retry := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/subscribers/planner", nil, "alice")
	if retry.Code != http.StatusNoContent {
		t.Fatalf("retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	log = dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, "alice")
	if strings.Count(log.Body.String(), `"type":"subscription.removed"`) != 1 {
		t.Fatalf("retry duplicated the durable unsubscribe command: %s", log.Body.String())
	}
}

func TestUnsubscribeAfterResubscriptionAppendsAnotherCompletion(t *testing.T) {
	database := openEmptyTestStore(t)
	bootstrap := newSubscribersHandler(t, database, "")
	key := createTestIssue(t, bootstrap, "TEST", "Issue")
	topic := "notifications.dispatch.issue." + key + ".>"
	envoyURL, listener := newFakeListener(t, map[string][]string{
		"planner": {topic},
	}, nil)
	handler := newSubscribersHandler(t, database, envoyURL)

	first := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/subscribers/planner", nil, "alice")
	if first.Code != http.StatusNoContent {
		t.Fatalf("first unsubscribe: status=%d body=%s", first.Code, first.Body.String())
	}
	listener.mu.Lock()
	listener.interests["planner"] = []string{topic, topic}
	listener.mu.Unlock()

	second := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/subscribers/planner", nil, "alice")
	if second.Code != http.StatusNoContent {
		t.Fatalf("second unsubscribe: status=%d body=%s", second.Code, second.Body.String())
	}
	log := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, "alice")
	events := decodeBody[[]struct {
		ID      int64  `json:"id"`
		Type    string `json:"type"`
		Payload struct {
			RequestEventID int64 `json:"request_event_id"`
		} `json:"payload"`
	}](t, log)
	var requested, completed []int64
	for _, event := range events {
		switch event.Type {
		case "subscription.remove_requested":
			requested = append(requested, event.ID)
		case "subscription.removed":
			completed = append(completed, event.Payload.RequestEventID)
		}
	}
	if len(requested) != 2 || !reflect.DeepEqual(completed, requested) {
		t.Fatalf("resubscribed removal command IDs: requested=%v completed=%v", requested, completed)
	}
}

func TestUnsubscribeIssueSessionReportsNotFoundWhenNotSubscribed(t *testing.T) {
	database := openEmptyTestStore(t)
	bootstrap := newSubscribersHandler(t, database, "")
	key := createTestIssue(t, bootstrap, "TEST", "Issue")
	envoyURL, _ := newFakeListener(t, map[string][]string{
		"planner": {"notifications.dispatch.issue.OTHER-1.>"},
	}, nil)
	handler := newSubscribersHandler(t, database, envoyURL)

	response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/subscribers/planner", nil, "alice")
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"SUBSCRIBER_NOT_FOUND"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestUnsubscribeIssueSessionReportsNotFoundForAnUnknownSession(t *testing.T) {
	database := openEmptyTestStore(t)
	bootstrap := newSubscribersHandler(t, database, "")
	key := createTestIssue(t, bootstrap, "TEST", "Issue")
	envoyURL, _ := newFakeListener(t, map[string][]string{}, nil)
	handler := newSubscribersHandler(t, database, envoyURL)

	response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/subscribers/ghost", nil, "alice")
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"SUBSCRIBER_NOT_FOUND"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestUnsubscribeIssueSessionRejectsBearerCallers(t *testing.T) {
	database := openEmptyTestStore(t)
	bootstrap := newSubscribersHandler(t, database, "")
	key := createTestIssue(t, bootstrap, "TEST", "Issue")
	envoyURL, _ := newFakeListener(t, map[string][]string{}, nil)
	handler := newSubscribersHandler(t, database, envoyURL)

	request := httptest.NewRequest(http.MethodDelete, "/api/v1/issues/"+key+"/subscribers/planner", nil)
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestUnsubscribeArtifactSessionRejectsIssueLinkedArtifacts(t *testing.T) {
	database := openEmptyTestStore(t)
	bootstrap := newSubscribersHandler(t, database, "")
	key := createTestIssue(t, bootstrap, "TEST", "Issue")
	uploaded := dispatchRequest(t, bootstrap, http.MethodPost, "/api/v1/issues/"+key+"/artifacts", map[string]string{
		"name": "notes.md", "content": "# Notes\n",
	}, "alice")
	artifactID := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, uploaded).Artifact.ID
	envoyURL, _ := newFakeListener(t, nil, nil)
	handler := newSubscribersHandler(t, database, envoyURL)

	response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/artifacts/"+artifactID+"/subscribers/planner", nil, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"ARTIFACT_LINKED"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
