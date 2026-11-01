package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// An interest record this build cannot decode (store.ErrMalformedInterest) is answered as what it
// is. The two single-session routes, which read the record itself, answer a 500 with no
// Retry-After whose body names the session and that its interest record is unreadable, as an
// unreadable role claim is answered, and the registry logs the decode failure at WARN once per
// read, naming the session. GET /v1/sessions does not refuse the listing for it: it lists that
// session with no topics and roles beside the readable one with its topics, and logs nothing,
// because it joins the interest copy (store.Registry.Cached) and reads no key of the bucket. The
// copy never holds a record it could not decode (interestCache.Apply evicts it), so the row the
// listing builds from the copy is the same row the read-through routes report: the session, with
// no topics.
func TestAnUnreadableInterestRecordIsAnsweredAsUnreadable(t *testing.T) {
	const (
		readable   = "ses_interest_answer_readable"
		unreadable = "ses_interest_answer_unreadable"
		topic      = "notifications.role.reviewer"
	)
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the bus: %v", err)
	}
	t.Cleanup(client.Close)
	logs := &lockedBuffer{}
	registry, err := store.Open(client.Conn, store.WithReplicas(1),
		store.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	for _, id := range []string{readable, unreadable} {
		if err := sessions.Put(id, session.SessionEntry{Port: 1, MachineID: "test-machine", Dir: "/test/" + id, Title: id}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	// The listing answers the refilling 503 until the session copy has completed a read of its
	// bucket, which says nothing about the record this test writes.
	waitForSessions(t, sessions)
	if _, err := registry.Upsert(store.Interest{SessionID: readable, MachineID: "test-machine", Dir: "/test/" + readable},
		[]string{contracts.AgentSubject(readable), topic}); err != nil {
		t.Fatalf("write %s's interest record: %v", readable, err)
	}
	if _, err := registry.SetRole(readable, "test-machine", "reviewer", false); err != nil {
		t.Fatalf("claim reviewer for %s: %v", readable, err)
	}
	waitForRegistry(t, registry)
	interestBucket, err := client.JS().KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open the interest bucket: %v", err)
	}
	if _, err := interestBucket.Put(unreadable, []byte("not a json interest record")); err != nil {
		t.Fatalf("write %s's unreadable interest record: %v", unreadable, err)
	}

	state := &listenerDeps{client: client, registry: registry, sessions: sessions}
	get := func(handler http.HandlerFunc, path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder
	}

	for _, route := range []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/v1/registry/" + unreadable, sessionEntryHandler(state)},
		{"/v1/interests/" + unreadable, adminInterestsHandler(registry, sessions, nil)},
	} {
		t.Run(route.path, func(t *testing.T) {
			before := len(logs.String())
			recorder := get(route.handler, route.path)
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status %d, body %s; want 500", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Retry-After"); got != "" {
				t.Fatalf("Retry-After = %q, want none: a retry reads the same record", got)
			}
			var refusal apiError
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
			}
			head := "envoy: the interest record of session " + unreadable + " is unreadable: malformed interest record: invalid character "
			if !strings.HasPrefix(refusal.Error, head) {
				t.Fatalf("the refusal reads %q, want it to begin %q", refusal.Error, head)
			}
			warnings := warnLinesNaming(logs.String()[before:], unreadable)
			if len(warnings) != 1 {
				t.Fatalf("the route logged %d WARN lines naming %s, want 1: %v", len(warnings), unreadable, warnings)
			}
			if message, _ := warnings[0]["error"].(string); !strings.HasPrefix(message, "invalid character ") {
				t.Fatalf("the WARN line %v does not carry the decode failure", warnings[0])
			}
		})
	}

	before := len(logs.String())
	recorder := get(sessionsHandler(registry, sessions), "/v1/sessions")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /v1/sessions: status %d, body %s; want 200", recorder.Code, recorder.Body.String())
	}
	var rows []sessionInfo
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode GET /v1/sessions %s: %v", recorder.Body.String(), err)
	}
	byID := map[string]sessionInfo{}
	for _, row := range rows {
		byID[row.SessionID] = row
	}
	if row, ok := byID[unreadable]; !ok || len(row.Topics) != 0 || len(row.Roles) != 0 || row.Title != unreadable {
		t.Fatalf("GET /v1/sessions row for %s = %+v (listed %v), want it listed by title with no topics and roles", unreadable, row, ok)
	}
	if row := byID[readable]; !slices.Contains(row.Topics, topic) || !slices.Contains(row.Roles, "reviewer") {
		t.Fatalf("GET /v1/sessions row for %s = %+v, want its topics to include %s and its roles reviewer", readable, row, topic)
	}

	if warnings := warnLinesNaming(logs.String()[before:], unreadable); len(warnings) != 0 {
		t.Fatalf("GET /v1/sessions logged %d WARN lines naming %s, want none: the listing reads the interest copy, never the record: %v", len(warnings), unreadable, warnings)
	}
}

// warnLinesNaming is every WARN record in logs whose session_id is sessionID.
func warnLinesNaming(logs string, sessionID string) []map[string]any {
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["level"] == "WARN" && record["session_id"] == sessionID {
			found = append(found, record)
		}
	}
	return found
}
