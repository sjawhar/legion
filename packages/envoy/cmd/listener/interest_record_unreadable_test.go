package main

import (
	"encoding/json"
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

// An interest record the bucket holds and this build cannot decode is not a bus that did not
// answer: the bus answered, with a record no retry will change. The three routes that read a
// session's interest record - GET /v1/sessions, GET /v1/registry/<session_id> and GET
// /v1/interests/<session_id> - must not answer it with the bus-unavailable 503, its
// busUnansweredPrefix body or its Retry-After: 1. That label sends every caller into a retry that
// can never succeed and points an operator's filter at an outage that never happened; on GET
// /v1/sessions it also takes down the whole listing, which every Dispatch mention and broadcast
// reads, for as long as the one record stays. It is the same distinction the role routes draw for
// an unreadable claim record (store.ErrMalformedRoleClaim, writeRoleClaimUnreadable). How the
// route answers instead - an error naming the unreadable record, or the session without its
// topics - is the route's to choose; this test pins only that it is not the bus's answer.
//
// The record is written into the interest bucket directly, as another build or a hand edit could
// leave it, so the listener's interest cache never holds it and every read of it reaches the
// bucket. The negative control is a second session with a readable record on the same rig,
// served 200 with its topics.
func TestAnUnreadableInterestRecordIsNotAnsweredAsTheBusNotAnswering(t *testing.T) {
	const (
		readable   = "ses_interest_record_readable"
		unreadable = "ses_interest_record_unreadable"
		topic      = "notifications.role.reviewer"
	)
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the bus: %v", err)
	}
	t.Cleanup(client.Close)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
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
	if _, err := registry.Upsert(store.Interest{SessionID: readable, MachineID: "test-machine", Dir: "/test/" + readable},
		[]string{contracts.AgentSubject(readable), topic}); err != nil {
		t.Fatalf("write %s's interest record: %v", readable, err)
	}
	interestBucket, err := client.JS().KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open the interest bucket: %v", err)
	}
	if _, err := interestBucket.Put(unreadable, []byte("not a json interest record")); err != nil {
		t.Fatalf("write %s's unreadable interest record: %v", unreadable, err)
	}
	for _, item := range registry.List() {
		if item.SessionID == unreadable {
			t.Fatalf("the interest cache holds %s: the routes would not read the unreadable record from the bucket", unreadable)
		}
	}

	state := &listenerDeps{client: client, registry: registry, sessions: sessions}
	get := func(handler http.HandlerFunc, path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder
	}

	// The control: the readable session is served with its topics on the same rig.
	control := get(sessionEntryHandler(state), "/v1/registry/"+readable)
	if control.Code != http.StatusOK {
		t.Fatalf("GET /v1/registry/%s: status %d, body %s; want 200", readable, control.Code, control.Body.String())
	}
	if info := decodeSessionInfo(t, control); !slices.Contains(info.Topics, topic) {
		t.Fatalf("GET /v1/registry/%s answered topics %v, want them to include %s", readable, info.Topics, topic)
	}

	for _, route := range []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/v1/sessions", sessionsHandler(registry, sessions)},
		{"/v1/registry/" + unreadable, sessionEntryHandler(state)},
		{"/v1/interests/" + unreadable, adminInterestsHandler(registry)},
	} {
		t.Run(route.path, func(t *testing.T) {
			recorder := get(route.handler, route.path)
			var refusal apiError
			_ = json.Unmarshal(recorder.Body.Bytes(), &refusal)
			if recorder.Code == http.StatusServiceUnavailable || strings.HasPrefix(refusal.Error, busUnansweredPrefix) {
				t.Fatalf("GET %s with an unreadable interest record: status %d, body %s; want anything but the bus-unavailable 503 - the bus answered, and the record it answered with cannot be read on any retry",
					route.path, recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Retry-After"); got != "" {
				t.Fatalf("GET %s with an unreadable interest record carries Retry-After %q, want none: no retry reads the record differently", route.path, got)
			}
		})
	}
}
