package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// A Clear on the Agents page is a per-viewer cutoff the server keeps, so it follows the human
// across devices: written per session, read back as one map, invisible to other users and to
// sessions.
func TestPerUserAgentStateRoundTripsAndIsIsolated(t *testing.T) {
	handler := newTestHandler(t)
	cutoff := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	saved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/planner-session/state", map[string]any{
		"cleared_before": cutoff.Format(time.RFC3339Nano),
	}, "alice")
	if saved.Code != http.StatusOK {
		t.Fatalf("save Alice agent state: status=%d body=%s", saved.Code, saved.Body.String())
	}
	if got := decodeBody[userAgentState](t, saved); !parseTimestamp(t, got.ClearedBefore).Equal(cutoff) {
		t.Fatalf("saved cleared_before = %q, want %s", got.ClearedBefore, cutoff.Format(time.RFC3339Nano))
	}

	alice := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agents/state", nil, "alice")
	if alice.Code != http.StatusOK {
		t.Fatalf("Alice agent state: status=%d body=%s", alice.Code, alice.Body.String())
	}
	state := decodeBody[map[string]userAgentState](t, alice)
	if entry, ok := state["planner-session"]; !ok || !parseTimestamp(t, entry.ClearedBefore).Equal(cutoff) {
		t.Fatalf("Alice agent state = %#v, want planner-session cleared before %s", state, cutoff.Format(time.RFC3339Nano))
	}

	// A second Clear moves the cutoff forward for the same session; it does not add a row.
	later := cutoff.Add(30 * time.Second)
	moved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/planner-session/state", map[string]any{
		"cleared_before": later.Format(time.RFC3339Nano),
	}, "alice")
	if moved.Code != http.StatusOK {
		t.Fatalf("move Alice cutoff: status=%d body=%s", moved.Code, moved.Body.String())
	}
	state = decodeBody[map[string]userAgentState](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agents/state", nil, "alice"))
	if len(state) != 1 || !parseTimestamp(t, state["planner-session"].ClearedBefore).Equal(later) {
		t.Fatalf("Alice agent state after second clear = %#v, want one entry at %s", state, later.Format(time.RFC3339Nano))
	}

	bob := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agents/state", nil, "bob")
	if bob.Code != http.StatusOK || strings.Contains(bob.Body.String(), "planner-session") {
		t.Fatalf("Bob agent state leaked Alice's clear: status=%d body=%s", bob.Code, bob.Body.String())
	}
	if bob.Body.String() != "{}\n" && bob.Body.String() != "{}" {
		t.Fatalf("Bob agent state = %q, want an empty object (not null)", bob.Body.String())
	}

	for _, probe := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/me/agents/state", nil},
		{http.MethodPut, "/api/v1/me/agents/planner-session/state", map[string]any{
			"actor": map[string]string{"kind": "session", "id": "planner-session"}, "cleared_before": cutoff.Format(time.RFC3339Nano),
		}},
	} {
		session := agentRequest(t, handler, probe.method, probe.path, probe.body, "agent-token")
		if session.Code != http.StatusForbidden || !strings.Contains(session.Body.String(), `"code":"HUMAN_ONLY"`) {
			t.Fatalf("%s %s as a session: status=%d body=%s", probe.method, probe.path, session.Code, session.Body.String())
		}
	}
}

func TestPerUserAgentStateRejectsMalformedAndFutureCutoffs(t *testing.T) {
	handler := newTestHandler(t)
	for name, body := range map[string]map[string]any{
		"missing":    {},
		"not a time": {"cleared_before": "yesterday"},
		"future":     {"cleared_before": time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)},
	} {
		response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/planner-session/state", body, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_STATE"`) {
			t.Fatalf("%s cutoff: status=%d body=%s, want 400 INVALID_STATE", name, response.Code, response.Body.String())
		}
	}
	state := decodeBody[map[string]userAgentState](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agents/state", nil, "alice"))
	if len(state) != 0 {
		t.Fatalf("a rejected cutoff was stored: %#v", state)
	}
	// A cutoff that is "now" by a client clock a few seconds ahead of the server's still lands.
	ahead := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/planner-session/state", map[string]any{
		"cleared_before": time.Now().UTC().Add(5 * time.Second).Format(time.RFC3339Nano),
	}, "alice")
	if ahead.Code != http.StatusOK {
		t.Fatalf("cutoff seconds ahead of the server clock: status=%d body=%s", ahead.Code, ahead.Body.String())
	}
}

func parseTimestamp(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("parse timestamp %q: %v", value, err)
	}
	return parsed
}
