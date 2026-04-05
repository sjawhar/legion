package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

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
