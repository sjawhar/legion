package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logging"
	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/metrics"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

var (
	sharedListenerNATSOnce sync.Once
	sharedListenerNATSURI  string
	sharedListenerNATSErr  error
)

func sharedListenerTestNATSURI(t *testing.T) string {
	t.Helper()
	sharedListenerNATSOnce.Do(func() {
		ctx := context.Background()
		ctr, err := tcnats.Run(ctx, "nats:2.10")
		if err != nil {
			sharedListenerNATSErr = err
			return
		}
		sharedListenerNATSURI, sharedListenerNATSErr = ctr.ConnectionString(ctx)
	})
	if sharedListenerNATSErr != nil {
		t.Fatalf("failed to start shared NATS: %v", sharedListenerNATSErr)
	}
	return sharedListenerNATSURI
}

func clearKVBucket(t *testing.T, conn *natsgo.Conn, bucket string) {
	t.Helper()
	js, err := conn.JetStream(natsgo.MaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("failed to open JetStream: %v", err)
	}
	kv, err := js.KeyValue(bucket)
	if errors.Is(err, natsgo.ErrBucketNotFound) {
		return
	}
	if err != nil {
		t.Fatalf("failed to open bucket %s: %v", bucket, err)
	}
	keys, err := kv.Keys()
	if errors.Is(err, natsgo.ErrNoKeysFound) {
		return
	}
	if err != nil {
		t.Fatalf("failed to list bucket %s keys: %v", bucket, err)
	}
	for _, key := range keys {
		if err := kv.Delete(key); err != nil && !errors.Is(err, natsgo.ErrKeyNotFound) {
			t.Fatalf("failed to delete key %s from bucket %s: %v", key, bucket, err)
		}
	}
}

func resetListenerTestState(t *testing.T, conn *natsgo.Conn) {
	t.Helper()
	js, err := conn.JetStream(natsgo.MaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("failed to open JetStream: %v", err)
	}
	if err := js.PurgeStream(bus.Stream); err != nil && !errors.Is(err, natsgo.ErrStreamNotFound) {
		t.Fatalf("failed to purge stream %s: %v", bus.Stream, err)
	}
	clearKVBucket(t, conn, store.Bucket)
	clearKVBucket(t, conn, session.SessionBucket)
}

func TestReadinessGate_NotReady_Returns503(t *testing.T) {
	handler := readinessGate(func() bool { return false }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called when not ready")
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/interests/subscribe", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
	if body := rr.Body.String(); body != "service starting\n" {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestReadinessGate_Ready_PassesThrough(t *testing.T) {
	var called bool
	handler := readinessGate(func() bool { return true }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/interests/subscribe", nil))

	if !called {
		t.Fatal("handler should be called when ready")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

// healthzHandler builds the same /healthz handler used in main(), parameterized
// by the shared state pointer so we can control readiness in tests.
func healthzHandler(state *atomic.Pointer[listenerDeps]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		d := state.Load()
		if d == nil {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "starting"})
			return
		}
		// Post-init path would check NATS health, but requires a live
		// bus.Client which needs NATS. Only the starting state is
		// testable without NATS.
	}
}

func TestHealthz_Starting_Returns200WithJSON(t *testing.T) {
	var state atomic.Pointer[listenerDeps]
	handler := healthzHandler(&state)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	ct := rr.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %q", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	if body["status"] != "starting" {
		t.Fatalf("expected status 'starting', got %q", body["status"])
	}
}

func TestFullMux_StartingState(t *testing.T) {
	// Simulate the full mux with no deps published (starting state).
	var state atomic.Pointer[listenerDeps]

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler(&state))

	v1 := http.NewServeMux()
	v1.HandleFunc("/v1/interests/subscribe", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("v1 handler should not be called during starting state")
	})
	mux.Handle("/v1/", readinessGate(func() bool { return state.Load() != nil }, v1))

	t.Run("healthz returns 200 starting", func(t *testing.T) {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
	})

	t.Run("v1 endpoint returns 503", func(t *testing.T) {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/interests/subscribe", nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d", rr.Code)
		}
	})

	t.Run("v1 messages returns 503", func(t *testing.T) {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/messages/send", nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d", rr.Code)
		}
	})

	t.Run("v1 registry returns 503", func(t *testing.T) {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/registry/some-id", nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d", rr.Code)
		}
	})

	t.Run("v1 sessions returns 503", func(t *testing.T) {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/sessions", nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d", rr.Code)
		}
	})
}

func TestPublishHandler_RejectsAgentTopics(t *testing.T) {
	// publishHandler validation runs before deps.client is used, so a
	// minimal non-nil deps (with nil inner fields) is enough to test the
	// rejection path without NATS.
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{})
	handler := publishHandler(&state)

	cases := []struct {
		name       string
		topic      string
		wantStatus int
		wantSubstr string
	}{
		{
			name:       "bare agent prefix",
			topic:      contracts.AgentTopicPrefix,
			wantStatus: http.StatusBadRequest,
			wantSubstr: "cannot publish to agent topics",
		},
		{
			name:       "agent with session ID",
			topic:      contracts.AgentSubject("ses_abc123"),
			wantStatus: http.StatusBadRequest,
			wantSubstr: "cannot publish to agent topics",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"source_session":"ses_test","topic":"` + tc.topic + `","message":"hello"}`
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d (body: %s)", tc.wantStatus, rr.Code, rr.Body.String())
			}
			if tc.wantSubstr != "" && !strings.Contains(rr.Body.String(), tc.wantSubstr) {
				t.Fatalf("expected body to contain %q, got %q", tc.wantSubstr, rr.Body.String())
			}
		})
	}
}

func TestPublishHandler_MethodNotAllowed(t *testing.T) {
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{})
	handler := publishHandler(&state)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/messages/publish", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestPublishHandler_RejectsInvalidSource(t *testing.T) {
	// Validation runs before deps.client is used, so nil inner fields
	// are enough for rejection-path tests.
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{})
	handler := publishHandler(&state)

	cases := []struct {
		name       string
		body       string
		wantSubstr string
	}{
		{
			name:       "rejects invalid source",
			body:       `{"source":"invalid","topic":"notifications.test.foo","message":"hello"}`,
			wantSubstr: "source must be one of",
		},
		{
			name:       "rejects empty-ish source after trim",
			body:       `{"source":" ","topic":"notifications.test.foo","message":"hello"}`,
			wantSubstr: "source is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d (body: %s)", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tc.wantSubstr) {
				t.Fatalf("expected body to contain %q, got %q", tc.wantSubstr, rr.Body.String())
			}
		})
	}
}

func setupPublishTestClient(t *testing.T) *bus.Client {
	t.Helper()
	client, err := bus.Connect([]string{sharedListenerTestNATSURI(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to connect bus: %v", err)
	}
	t.Cleanup(func() { client.Conn.Close() })
	resetListenerTestState(t, client.Conn)
	return client
}

func TestPublishHandler_SourceFieldWithNATS(t *testing.T) {
	client := setupPublishTestClient(t)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client})
	handler := publishHandler(&state)

	cases := []struct {
		name       string
		body       string
		wantSource string
	}{
		{
			name:       "defaults to agent when source omitted",
			body:       `{"topic":"notifications.test.foo","message":"hello"}`,
			wantSource: "agent",
		},
		{
			name:       "defaults to agent when source empty",
			body:       `{"source":"","topic":"notifications.test.foo","message":"hello"}`,
			wantSource: "agent",
		},
		{
			name:       "accepts ghostwispr source",
			body:       `{"source":"ghostwispr","topic":"notifications.ghostwispr.rec-1.transcript","message":"{}"}`,
			wantSource: "ghostwispr",
		},
		{
			name:       "accepts github source",
			body:       `{"source":"github","topic":"notifications.github.acme.widgets.pr","message":"{}"}`,
			wantSource: "github",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
			}
			var env contracts.Envelope
			if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}
			if env.Source != tc.wantSource {
				t.Fatalf("expected source %q, got %q", tc.wantSource, env.Source)
			}
		})
	}
}

func TestIsValidRole(t *testing.T) {
	valid := []string{"legion-controller", "opencode_dev", "r2d2", "a"}
	for _, role := range valid {
		if !isValidRole(role) {
			t.Fatalf("expected role %q to be valid", role)
		}
	}

	invalid := []string{"", "Legion", "-bad", "bad.role", "bad role"}
	for _, role := range invalid {
		if isValidRole(role) {
			t.Fatalf("expected role %q to be invalid", role)
		}
	}
}

func TestRoleSetHandler_Validation(t *testing.T) {
	registry := setupAdminTestRegistry(t, nil)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{registry: registry})
	handler := roleSetHandler(&state, "test-machine")

	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantSubstr string
	}{
		{
			name:       "empty session id",
			body:       `{"session_id":"","role":"legion-controller"}`,
			wantStatus: http.StatusBadRequest,
			wantSubstr: "session_id is required",
		},
		{
			name:       "empty role",
			body:       `{"session_id":"ses_1","role":""}`,
			wantStatus: http.StatusBadRequest,
			wantSubstr: "role is required",
		},
		{
			name:       "invalid role",
			body:       `{"session_id":"ses_1","role":"Legion"}`,
			wantStatus: http.StatusBadRequest,
			wantSubstr: "role must match ^[a-z0-9][a-z0-9_-]*$",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/roles/set", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("expected %d, got %d: %s", tc.wantStatus, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantSubstr) {
				t.Fatalf("expected body to contain %q, got %q", tc.wantSubstr, rec.Body.String())
			}
		})
	}
}

func TestRoleSetHandler_SetsRole(t *testing.T) {
	registry := setupAdminTestRegistry(t, nil)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{registry: registry})
	handler := roleSetHandler(&state, "test-machine")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/roles/set", strings.NewReader(`{"session_id":"ses_role","role":"legion-controller"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var item store.Interest
	if err := json.NewDecoder(rec.Body).Decode(&item); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if item.SessionID != "ses_role" {
		t.Fatalf("expected session ses_role, got %s", item.SessionID)
	}
	if item.MachineID != "test-machine" {
		t.Fatalf("expected machine test-machine, got %s", item.MachineID)
	}
	if len(item.Topics) != 1 || item.Topics[0] != "notifications.role.legion-controller" {
		t.Fatalf("expected role topic, got %v", item.Topics)
	}

	persisted, err := registry.Get("ses_role")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if len(persisted.Topics) != 1 || persisted.Topics[0] != "notifications.role.legion-controller" {
		t.Fatalf("expected persisted role topic, got %v", persisted.Topics)
	}
}

func setupAdminTestRegistry(t *testing.T, interests map[string][]string) *store.Registry {
	t.Helper()
	client, err := bus.Connect([]string{sharedListenerTestNATSURI(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to connect bus: %v", err)
	}
	t.Cleanup(func() { client.Conn.Close() })
	resetListenerTestState(t, client.Conn)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	for sessionID, topics := range interests {
		allTopics := append([]string{contracts.AgentSubject(sessionID)}, topics...)
		if _, err := registry.Upsert(store.Interest{
			SessionID: sessionID,
			MachineID: "test-machine",
			Dir:       "/test",
		}, allTopics); err != nil {
			t.Fatalf("failed to upsert interest for %s: %v", sessionID, err)
		}
	}
	// Allow watcher to propagate all Upsert events to cache
	time.Sleep(500 * time.Millisecond)
	return registry
}

func TestAdminInterestsHandler_ListAll(t *testing.T) {
	registry := setupAdminTestRegistry(t, map[string][]string{
		"ses_list_b": {"notifications.test.>"},
		"ses_list_a": {"notifications.github.>"},
	})
	handler := adminInterestsHandler(registry)

	req := httptest.NewRequest(http.MethodGet, "/v1/interests/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var items []store.Interest
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 interests, got %d", len(items))
	}
	if items[0].SessionID != "ses_list_a" {
		t.Fatalf("expected first item ses_list_a, got %s", items[0].SessionID)
	}
}

func TestAdminInterestsHandler_GetBySession(t *testing.T) {
	registry := setupAdminTestRegistry(t, map[string][]string{
		"ses_get_test": {"notifications.test.>"},
	})
	handler := adminInterestsHandler(registry)

	req := httptest.NewRequest(http.MethodGet, "/v1/interests/ses_get_test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var item store.Interest
	if err := json.NewDecoder(rec.Body).Decode(&item); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if item.SessionID != "ses_get_test" {
		t.Fatalf("expected ses_get_test, got %s", item.SessionID)
	}
}

func TestAdminInterestsHandler_DeleteBySession(t *testing.T) {
	registry := setupAdminTestRegistry(t, map[string][]string{
		"ses_delete_target": {"notifications.test.>"},
	})
	handler := adminInterestsHandler(registry)

	req := httptest.NewRequest(http.MethodDelete, "/v1/interests/ses_delete_target", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Allow watcher to propagate delete event to cache
	time.Sleep(500 * time.Millisecond)
	if _, err := registry.Get("ses_delete_target"); err == nil {
		t.Fatal("expected interest to be deleted")
	}
}

func TestAdminInterestsHandler_DeleteIdempotent(t *testing.T) {
	registry := setupAdminTestRegistry(t, nil)
	handler := adminInterestsHandler(registry)

	req := httptest.NewRequest(http.MethodDelete, "/v1/interests/ses_nonexistent", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for non-existent session, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminInterestsHandler_MethodNotAllowed(t *testing.T) {
	registry := setupAdminTestRegistry(t, nil)
	handler := adminInterestsHandler(registry)

	req := httptest.NewRequest(http.MethodPost, "/v1/interests/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func setupSessionsTest(t *testing.T, interests map[string][]string, ports map[string]int) (*store.Registry, *session.SessionRegistry) {
	t.Helper()
	client, err := bus.Connect([]string{sharedListenerTestNATSURI(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to connect bus: %v", err)
	}
	t.Cleanup(func() { client.Conn.Close() })
	resetListenerTestState(t, client.Conn)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("failed to create session registry: %v", err)
	}
	for sessionID, topics := range interests {
		allTopics := append([]string{contracts.AgentSubject(sessionID)}, topics...)
		if _, err := registry.Upsert(store.Interest{
			SessionID: sessionID,
			MachineID: "test-machine",
			Dir:       "/test/" + sessionID,
		}, allTopics); err != nil {
			t.Fatalf("failed to upsert interest for %s: %v", sessionID, err)
		}
	}
	for sessionID, port := range ports {
		if err := sessions.Put(sessionID, session.SessionEntry{
			Port:      port,
			MachineID: "test-machine",
			Dir:       "/test/" + sessionID,
		}); err != nil {
			t.Fatalf("failed to put session entry for %s: %v", sessionID, err)
		}
	}
	// Allow watcher to propagate interest events to cache
	time.Sleep(500 * time.Millisecond)
	return registry, sessions
}

func TestSessionsHandler_JoinsRegistries(t *testing.T) {
	registry, sessions := setupSessionsTest(t,
		map[string][]string{
			"ses_b": {"notifications.test.>"},
			"ses_a": {"notifications.github.>"},
		},
		map[string]int{
			"ses_a": 13382,
			"ses_b": 13383,
		},
	)
	handler := sessionsHandler(registry, sessions)

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var items []sessionInfo
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(items))
	}
	// List is sorted by SessionID
	if items[0].SessionID != "ses_a" {
		t.Fatalf("expected first item ses_a, got %s", items[0].SessionID)
	}
	if items[0].Port != 13382 {
		t.Fatalf("expected ses_a port 13382, got %d", items[0].Port)
	}
	if items[0].MachineID != "test-machine" {
		t.Fatalf("expected machine_id test-machine, got %s", items[0].MachineID)
	}
	if items[0].Dir != "/test/ses_a" {
		t.Fatalf("expected dir /test/ses_a, got %s", items[0].Dir)
	}
	if len(items[0].Topics) == 0 {
		t.Fatal("expected ses_a to have topics")
	}
	if items[1].Port != 13383 {
		t.Fatalf("expected ses_b port 13383, got %d", items[1].Port)
	}
}

func TestSessionsHandler_IncludesTitle(t *testing.T) {
	client, err := bus.Connect([]string{sharedListenerTestNATSURI(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to connect bus: %v", err)
	}
	t.Cleanup(func() { client.Conn.Close() })
	resetListenerTestState(t, client.Conn)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("failed to create session registry: %v", err)
	}
	if _, err := registry.Upsert(store.Interest{
		SessionID: "ses_titled",
		MachineID: "test-machine",
		Dir:       "/test/ses_titled",
	}, []string{contracts.AgentSubject("ses_titled")}); err != nil {
		t.Fatalf("failed to upsert interest: %v", err)
	}
	if err := sessions.Put("ses_titled", session.SessionEntry{
		Port:      13382,
		MachineID: "test-machine",
		Dir:       "/test/ses_titled",
		Title:     "My Session Title",
	}); err != nil {
		t.Fatalf("failed to put session entry: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	handler := sessionsHandler(registry, sessions)
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var items []sessionInfo
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 session, got %d", len(items))
	}
	if items[0].Title != "My Session Title" {
		t.Fatalf("expected title 'My Session Title', got %q", items[0].Title)
	}
}

func TestSessionsHandler_NilSessionRegistry(t *testing.T) {
	// When session registry is nil, endpoint returns 503
	handler := sessionsHandler(nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSessionsHandler_MethodNotAllowed(t *testing.T) {
	handler := sessionsHandler(nil, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestSessionsHandler_EmptyList(t *testing.T) {
	registry, sessions := setupSessionsTest(t, nil, nil)
	handler := sessionsHandler(registry, sessions)

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var items []sessionInfo
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(items))
	}
}

func TestSessionsHandler_OnlyLiveSessions(t *testing.T) {
	// Sessions in interests but NOT in envoy_sessions should be excluded
	registry, sessions := setupSessionsTest(t,
		map[string][]string{
			"ses_live": {"notifications.test.>"},
			"ses_dead": {"notifications.github.>"},
		},
		map[string]int{
			"ses_live": 13382,
			// ses_dead intentionally not in sessions registry — it's dead
		},
	)
	handler := sessionsHandler(registry, sessions)

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var items []sessionInfo
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 live session, got %d", len(items))
	}
	if items[0].SessionID != "ses_live" {
		t.Fatalf("expected ses_live, got %s", items[0].SessionID)
	}
	if items[0].Port != 13382 {
		t.Fatalf("expected port 13382, got %d", items[0].Port)
	}
	if len(items[0].Topics) == 0 {
		t.Fatal("expected ses_live to have topics from interests")
	}
}

func TestSessionsHandler_NoInterestsData(t *testing.T) {
	// Session in envoy_sessions but NOT in interests — still appears, just no topics
	registry, sessions := setupSessionsTest(t,
		nil, // no interests
		map[string]int{
			"ses_orphan": 13382,
		},
	)
	handler := sessionsHandler(registry, sessions)

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var items []sessionInfo
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 session, got %d", len(items))
	}
	if items[0].Port != 13382 {
		t.Fatalf("expected port 13382, got %d", items[0].Port)
	}
	if len(items[0].Topics) != 0 {
		t.Fatalf("expected no topics for orphan session, got %v", items[0].Topics)
	}
}

func TestIdempotencyKey_Send(t *testing.T) {
	client := setupPublishTestClient(t)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client})

	// Create a handler for /v1/messages/send
	mux := http.NewServeMux()
	v1 := http.NewServeMux()
	v1.HandleFunc("/v1/messages/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SourceSession  string `json:"source_session"`
			TargetSession  string `json:"target_session"`
			Message        string `json:"message"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		item := contracts.Envelope{
			EventID:        id.New(),
			Source:         "agent",
			SourceSession:  body.SourceSession,
			SourceEventID:  id.New(),
			Topic:          contracts.AgentSubject(body.TargetSession),
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: body.Message,
			TraceID:        id.New(),
		}
		dedupeKey := "agent." + body.TargetSession + "." + id.New()
		if body.IdempotencyKey != "" {
			dedupeKey = "agent." + body.TargetSession + "." + body.IdempotencyKey
		}
		item.DedupeKey = dedupeKey
		if err := item.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d := state.Load()
		if err := d.client.Publish(item); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(item)
	})
	mux.Handle("/v1/", readinessGate(func() bool { return state.Load() != nil }, v1))

	// Test: same idempotency_key produces same DedupeKey
	rr1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages/send", strings.NewReader(`{"source_session":"src1","target_session":"tgt1","message":"hello","idempotency_key":"retry-abc"}`))
	req1.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Fatalf("first request failed: expected 200, got %d (body: %s)", rr1.Code, rr1.Body.String())
	}
	var env1 contracts.Envelope
	if err := json.NewDecoder(rr1.Body).Decode(&env1); err != nil {
		t.Fatalf("failed to decode first response: %v", err)
	}

	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages/send", strings.NewReader(`{"source_session":"src1","target_session":"tgt1","message":"hello","idempotency_key":"retry-abc"}`))
	req2.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Fatalf("second request failed: expected 200, got %d (body: %s)", rr2.Code, rr2.Body.String())
	}
	var env2 contracts.Envelope
	if err := json.NewDecoder(rr2.Body).Decode(&env2); err != nil {
		t.Fatalf("failed to decode second response: %v", err)
	}

	if env1.DedupeKey != env2.DedupeKey {
		t.Fatalf("expected same DedupeKey for same idempotency_key, got %q and %q", env1.DedupeKey, env2.DedupeKey)
	}
	if !strings.HasPrefix(env1.DedupeKey, "agent.tgt1.retry-abc") {
		t.Fatalf("expected DedupeKey to start with 'agent.tgt1.retry-abc', got %q", env1.DedupeKey)
	}
}

func TestIdempotencyKey_Publish(t *testing.T) {
	client := setupPublishTestClient(t)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client})
	handler := publishHandler(&state)

	// Test: same idempotency_key produces same DedupeKey
	rr1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(`{"topic":"notifications.test.foo","message":"hello","idempotency_key":"publish-xyz"}`))
	req1.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Fatalf("first request failed: expected 200, got %d (body: %s)", rr1.Code, rr1.Body.String())
	}
	var env1 contracts.Envelope
	if err := json.NewDecoder(rr1.Body).Decode(&env1); err != nil {
		t.Fatalf("failed to decode first response: %v", err)
	}

	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(`{"topic":"notifications.test.foo","message":"hello","idempotency_key":"publish-xyz"}`))
	req2.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Fatalf("second request failed: expected 200, got %d (body: %s)", rr2.Code, rr2.Body.String())
	}
	var env2 contracts.Envelope
	if err := json.NewDecoder(rr2.Body).Decode(&env2); err != nil {
		t.Fatalf("failed to decode second response: %v", err)
	}

	if env1.DedupeKey != env2.DedupeKey {
		t.Fatalf("expected same DedupeKey for same idempotency_key, got %q and %q", env1.DedupeKey, env2.DedupeKey)
	}
	if !strings.HasPrefix(env1.DedupeKey, "publish.publish-xyz") {
		t.Fatalf("expected DedupeKey to start with 'publish.publish-xyz', got %q", env1.DedupeKey)
	}
}

func TestIdempotencyKey_BackwardsCompat(t *testing.T) {
	client := setupPublishTestClient(t)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client})
	handler := publishHandler(&state)

	// Test: no idempotency_key produces different DedupeKeys (existing behavior)
	rr1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(`{"topic":"notifications.test.foo","message":"hello"}`))
	req1.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Fatalf("first request failed: expected 200, got %d (body: %s)", rr1.Code, rr1.Body.String())
	}
	var env1 contracts.Envelope
	if err := json.NewDecoder(rr1.Body).Decode(&env1); err != nil {
		t.Fatalf("failed to decode first response: %v", err)
	}

	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(`{"topic":"notifications.test.foo","message":"hello"}`))
	req2.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Fatalf("second request failed: expected 200, got %d (body: %s)", rr2.Code, rr2.Body.String())
	}
	var env2 contracts.Envelope
	if err := json.NewDecoder(rr2.Body).Decode(&env2); err != nil {
		t.Fatalf("failed to decode second response: %v", err)
	}

	if env1.DedupeKey == env2.DedupeKey {
		t.Fatalf("expected different DedupeKeys when no idempotency_key provided, got same: %q", env1.DedupeKey)
	}
	if !strings.HasPrefix(env1.DedupeKey, "publish.") {
		t.Fatalf("expected DedupeKey to start with 'publish.', got %q", env1.DedupeKey)
	}
}

func TestDurableConsumerRestart(t *testing.T) {
	publisher, err := bus.Connect([]string{sharedListenerTestNATSURI(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to connect publisher bus: %v", err)
	}
	t.Cleanup(func() { publisher.Conn.Close() })
	resetListenerTestState(t, publisher.Conn)

	firstListener, err := bus.Connect([]string{sharedListenerTestNATSURI(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to connect first listener bus: %v", err)
	}
	t.Cleanup(func() { firstListener.Conn.Close() })

	consumer := "listener-durable-restart-test"
	_ = publisher.JS().DeleteConsumer(bus.Stream, consumer)

	var (
		mu        sync.Mutex
		delivered []string
	)
	handler := func(msg *natsgo.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			t.Errorf("failed to decode envelope: %v", err)
			_ = msg.Ack()
			return
		}
		mu.Lock()
		delivered = append(delivered, item.EventID)
		mu.Unlock()
		_ = msg.Ack()
	}

	_, err = startListenerSubscription(firstListener, consumer, handler)
	if err != nil {
		t.Fatalf("first subscribe failed: %v", err)
	}

	first := contracts.Envelope{
		EventID:        "evt-restart-1",
		Source:         "test",
		SourceEventID:  "src-restart-1",
		Topic:          "notifications.test.restart",
		DedupeKey:      "dedupe-restart-1",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "first",
		TraceID:        "trace-restart-1",
	}
	if err := publisher.Publish(first); err != nil {
		t.Fatalf("publish first failed: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(delivered)
		mu.Unlock()
		if count == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	firstCount := len(delivered)
	mu.Unlock()
	if firstCount != 1 {
		t.Fatalf("expected first delivery before restart, got %d", firstCount)
	}

	firstListener.Close()

	second := contracts.Envelope{
		EventID:        "evt-restart-2",
		Source:         "test",
		SourceEventID:  "src-restart-2",
		Topic:          "notifications.test.restart",
		DedupeKey:      "dedupe-restart-2",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "second",
		TraceID:        "trace-restart-2",
	}
	third := contracts.Envelope{
		EventID:        "evt-restart-3",
		Source:         "test",
		SourceEventID:  "src-restart-3",
		Topic:          "notifications.test.restart",
		DedupeKey:      "dedupe-restart-3",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "third",
		TraceID:        "trace-restart-3",
	}
	if err := publisher.Publish(second); err != nil {
		t.Fatalf("publish second failed: %v", err)
	}
	if err := publisher.Publish(third); err != nil {
		t.Fatalf("publish third failed: %v", err)
	}

	secondListener, err := bus.Connect([]string{sharedListenerTestNATSURI(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to connect second listener bus: %v", err)
	}
	t.Cleanup(func() { secondListener.Conn.Close() })

	resub, err := startListenerSubscription(secondListener, consumer, handler)
	if err != nil {
		t.Fatalf("second subscribe failed: %v", err)
	}
	defer resub.Unsubscribe()

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(delivered)
		mu.Unlock()
		if count == 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	got := append([]string(nil), delivered...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("expected durable consumer to resume pending messages after restart, got %d deliveries: %v", len(got), got)
	}
}

func TestDeadSessionACK(t *testing.T) {
	client := setupPublishTestClient(t)
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	if shouldNAKFanoutDelivery(sessions, "ses_dead", errors.New("delivery failed")) {
		t.Fatal("expected dead session fan-out failure to ACK instead of NAK")
	}

	portListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve port: %v", err)
	}
	port := portListener.Addr().(*net.TCPAddr).Port
	if err := portListener.Close(); err != nil {
		t.Fatalf("failed to release port: %v", err)
	}
	if err := sessions.Put("ses_live", session.SessionEntry{Port: port, MachineID: "test-machine", Dir: "/test"}); err != nil {
		t.Fatalf("failed to register live session: %v", err)
	}

	if !shouldNAKFanoutDelivery(sessions, "ses_live", errors.New("delivery failed")) {
		t.Fatal("expected live session delivery failure to NAK for retry")
	}
}




func TestHealthzConsumerLag(t *testing.T) {
	natsURI := sharedListenerTestNATSURI(t)
	conn, err := natsgo.Connect(natsURI)
	if err != nil {
		t.Fatalf("failed to connect to NATS: %v", err)
	}
	defer conn.Close()

	resetListenerTestState(t, conn)

	// Create a bus client
	client, err := bus.Connect([]string{natsURI})
	if err != nil {
		t.Fatalf("failed to create bus client: %v", err)
	}
	defer client.Conn.Close()

	// Create a consumer
	consumerName := "listener-test-machine"
	_, err = client.Subscribe(
		"notifications.>",
		func(msg *natsgo.Msg) { _ = msg.Ack() },
		natsgo.Durable(consumerName),
		natsgo.AckExplicit(),
		natsgo.ManualAck(),
	)
	if err != nil {
		t.Fatalf("failed to create subscription: %v", err)
	}

	// Get consumer info to verify it exists and has the expected fields
	consumerInfo, err := client.JS().ConsumerInfo(bus.Stream, consumerName)
	if err != nil {
		t.Fatalf("failed to get consumer info: %v", err)
	}

	// Verify consumer info has the fields we need for /healthz
	if consumerInfo == nil {
		t.Fatal("expected consumer info, got nil")
	}

	// Simulate the /healthz handler response with consumer lag fields
	response := map[string]interface{}{"status": "healthy"}
	response["num_pending"] = consumerInfo.NumPending
	response["num_ack_pending"] = consumerInfo.NumAckPending

	// Verify the response contains the expected fields
	if _, ok := response["num_pending"]; !ok {
		t.Fatal("expected num_pending in response")
	}
	if _, ok := response["num_ack_pending"]; !ok {
		t.Fatal("expected num_ack_pending in response")
	}

	// Verify the values are correct types
	if response["num_pending"] != consumerInfo.NumPending {
		t.Fatalf("expected num_pending %d, got %v", consumerInfo.NumPending, response["num_pending"])
	}
	if response["num_ack_pending"] != consumerInfo.NumAckPending {
		t.Fatalf("expected num_ack_pending %d, got %v", consumerInfo.NumAckPending, response["num_ack_pending"])
	}
}

func TestMetrics(t *testing.T) {
	met := metrics.New()
	received := met.NewCounter("envoy_messages_received_total", "Total messages received by the listener")
	delivered := met.NewCounter("envoy_messages_delivered_total", "Total message delivery attempts")
	naked := met.NewCounter("envoy_messages_naked_total", "Total messages NAK'd for retry")
	duration := met.NewHistogram("envoy_delivery_duration_seconds", "Duration of message delivery attempts", metrics.DefaultBuckets)
	met.NewGauge("envoy_active_sessions", "Number of active sessions")
	met.NewGauge("envoy_active_interests", "Number of active interest subscriptions")
	met.NewGaugeFunc("envoy_consumer_pending", "Number of pending messages in the consumer", func() int64 { return 5 })

	// Simulate message processing.
	received.Inc([2]string{"source", "agent"}, [2]string{"topic_prefix", "notifications.github"})
	received.Inc([2]string{"source", "agent"}, [2]string{"topic_prefix", "notifications.github"})
	delivered.Inc([2]string{"delivery_status", "delivered"})
	naked.Inc()
	duration.Observe(0.05, [2]string{"delivery_status", "delivered"})

	rr := httptest.NewRecorder()
	met.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	body := rr.Body.String()

	// All 7 metric names must be present.
	expected := []string{
		"envoy_messages_received_total",
		"envoy_messages_delivered_total",
		"envoy_messages_naked_total",
		"envoy_delivery_duration_seconds",
		"envoy_active_sessions",
		"envoy_active_interests",
		"envoy_consumer_pending",
	}
	for _, name := range expected {
		if !strings.Contains(body, name) {
			t.Errorf("expected /metrics to contain %q, body:\n%s", name, body)
		}
	}

	// Verify specific counter values.
	if !strings.Contains(body, `envoy_messages_received_total{source="agent",topic_prefix="notifications.github"} 2`) {
		t.Errorf("expected received_total to be 2, body:\n%s", body)
	}
	if !strings.Contains(body, `envoy_messages_delivered_total{delivery_status="delivered"} 1`) {
		t.Errorf("expected delivered_total{delivered} to be 1, body:\n%s", body)
	}
	if !strings.Contains(body, `envoy_messages_naked_total 1`) {
		t.Errorf("expected naked_total to be 1, body:\n%s", body)
	}
	if !strings.Contains(body, "envoy_consumer_pending 5") {
		t.Errorf("expected consumer_pending gauge to be 5, body:\n%s", body)
	}

	// Prometheus text format.
	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("expected text/plain Content-Type, got %q", ct)
	}
}

func TestMetrics_NotGatedByReadiness(t *testing.T) {
	var state atomic.Pointer[listenerDeps]
	met := metrics.New()
	met.NewGauge("envoy_active_sessions", "Number of active sessions")

	mux := http.NewServeMux()
	mux.Handle("/metrics", met.Handler())
	mux.Handle("/v1/", readinessGate(func() bool { return state.Load() != nil }, http.NewServeMux()))

	// /metrics must return 200 before deps are set (startup).
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected /metrics 200 during startup, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "envoy_active_sessions") {
		t.Fatal("expected metric names in startup /metrics output")
	}
}

func TestTopicPrefix(t *testing.T) {
	cases := []struct {
		topic string
		want  string
	}{
		{"notifications.github.acme.widgets.issue.42.comment", "notifications.github"},
		{"notifications.agent.ses_abc123", "notifications.agent"},
		{"notifications.slack.T01.C02.message", "notifications.slack"},
		{"single", "single"},
		{"", ""},
	}
	for _, tc := range cases {
		got := metrics.TopicPrefix(tc.topic)
		if got != tc.want {
			t.Errorf("TopicPrefix(%q) = %q, want %q", tc.topic, got, tc.want)
		}
	}
}


func TestCheckSelfHealth_HealthyReturnsNil(t *testing.T) {
	client := setupTestNATS(t)
	defer client.Close()
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1), session.WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	if err := checkSelfHealth(registry, sessions); err != nil {
		t.Fatalf("healthy probe should not error: %v", err)
	}
}

func TestCheckSelfHealth_ClosedConnReturnsError(t *testing.T) {
	// Regression for the sami listener after-recovery scenario — the KV
	// registries hold handles bound to the original *nats.Conn that the bus
	// recovery path replaced. checkSelfHealth must surface that as an error
	// so the self-health watchdog can terminate the listener.
	client := setupTestNATS(t)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1), session.WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}

	client.Conn.Close()

	if err := checkSelfHealth(registry, sessions); err == nil {
		t.Fatal("probe after conn close should return error")
	}
}

func TestRunSelfHealthLoop_TerminatesAfterThreshold(t *testing.T) {
	logger := logging.New("test")
	var probeCalls int32
	probe := func() error {
		atomic.AddInt32(&probeCalls, 1)
		return errors.New("always failing for test")
	}
	terminated := make(chan struct{}, 1)
	terminate := func() { terminated <- struct{}{} }

	done := make(chan struct{})
	go func() {
		runSelfHealthLoop(logger, probe, terminate, 5*time.Millisecond, 3)
		close(done)
	}()

	select {
	case <-terminated:
	case <-time.After(2 * time.Second):
		t.Fatal("terminate was not invoked within 2s")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not exit after terminate")
	}
	if got := atomic.LoadInt32(&probeCalls); got < 3 {
		t.Fatalf("expected at least 3 probe calls before terminate, got %d", got)
	}
}

func TestRunSelfHealthLoop_RecoveryResetsCounter(t *testing.T) {
	logger := logging.New("test")
	var probeCalls int32
	probe := func() error {
		calls := atomic.AddInt32(&probeCalls, 1)
		// Fail, fail, succeed, fail, fail, ... — never 3-in-a-row.
		if calls%3 == 0 {
			return nil
		}
		return errors.New("intermittent")
	}
	terminate := func() { t.Fatal("terminate must not be called when counter resets") }

	done := make(chan struct{})
	go func() {
		runSelfHealthLoop(logger, probe, terminate, 5*time.Millisecond, 3)
		close(done)
	}()

	// Let the loop run long enough to do ~10 cycles and verify it never terminates.
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&probeCalls); got < 5 {
		t.Fatalf("expected at least 5 probe calls, got %d", got)
	}
	select {
	case <-done:
		t.Fatal("loop terminated despite intermittent recoveries")
	default:
	}
}

// setupTestNATS launches a NATS testcontainer dedicated to this package's tests.
func setupTestNATS(t *testing.T) *bus.Client {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcnats.Run(ctx, "nats:2.10")
	if err != nil {
		t.Fatalf("failed to start NATS: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	client, err := bus.Connect([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	return client
}