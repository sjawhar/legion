package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The three routes that read a session's interest record - GET /v1/sessions, GET
// /v1/registry/<session_id> and GET /v1/interests/<session_id> - read a record the interest cache
// misses through to the bucket, and on a bus that went silent without closing the connection that
// read would wait out the JetStream MaxWait (10 s), as long as this listener's HTTP write timeout.
// Each must answer such a read as the named 503 with Retry-After: 1 inside the caller's 5 s
// attempt, and never as a session with no topics.
//
// The rig is the relay TestASendAndAPublishAnswerABusThatStoppedAnsweringInsideTheBudget freezes.
// One live session has subscribed to nothing, so the interest cache holds no record for it and
// every read of it is a miss read through to the bucket; the other's record is cached, so a cache
// hit is shown to need no bus. The negative control is every request asked again before the
// freeze on the same rig: each answers 200, or the 404 the bucket's word that the key is absent
// earns.
func TestTheInterestReadRoutesAnswerABusThatStoppedAnsweringInsideTheBudget(t *testing.T) {
	const (
		missed = "ses_interest_routes_subscribed_to_nothing"
		cached = "ses_interest_routes_cached_interest"
		topic  = "notifications.role.reviewer"
		within = 4 * time.Second
	)
	relay := startListenerRelay(t, testnats.URL(t))
	client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's bus through the relay: %v", err)
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
	for _, id := range []string{missed, cached} {
		if err := sessions.Put(id, session.SessionEntry{Port: 1, MachineID: "test-machine", Dir: "/test/" + id, Title: id}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		if _, err := sessions.Get(id); err != nil {
			t.Fatalf("the listener's session cache does not hold %s (%v): the session read could be what fails", id, err)
		}
	}
	if _, err := registry.Upsert(store.Interest{SessionID: cached, MachineID: "test-machine", Dir: "/test/" + cached},
		[]string{contracts.AgentSubject(cached), topic}); err != nil {
		t.Fatalf("write %s's interest record: %v", cached, err)
	}
	cachedIDs := func() []string {
		var ids []string
		for _, item := range registry.List() {
			ids = append(ids, item.SessionID)
		}
		return ids
	}
	if ids := cachedIDs(); !slices.Contains(ids, cached) || slices.Contains(ids, missed) {
		t.Fatalf("the interest cache holds %v, want %s and not %s", ids, cached, missed)
	}

	state := &listenerDeps{client: client, registry: registry, sessions: sessions}
	get := func(handler http.HandlerFunc, path string) (*httptest.ResponseRecorder, time.Duration) {
		recorder := httptest.NewRecorder()
		started := time.Now()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder, time.Since(started)
	}
	list := func() (*httptest.ResponseRecorder, time.Duration) {
		return get(sessionsHandler(registry, sessions), "/v1/sessions")
	}
	entry := func(id string) (*httptest.ResponseRecorder, time.Duration) {
		return get(sessionEntryHandler(state), "/v1/registry/"+id)
	}
	interests := func(id string) (*httptest.ResponseRecorder, time.Duration) {
		return get(adminInterestsHandler(registry), "/v1/interests/"+id)
	}
	wantTopics := func(t *testing.T, info sessionInfo, want []string) {
		t.Helper()
		if !slices.Equal(info.Topics, want) {
			t.Fatalf("%s's topics = %v, want %v", info.SessionID, info.Topics, want)
		}
	}
	cachedTopics := []string{contracts.AgentSubject(cached), topic}

	// The control: with the bus answering, every request is served.
	recorder, _ := list()
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /v1/sessions with the bus answering: status %d, body %s; want 200", recorder.Code, recorder.Body.String())
	}
	var rows []sessionInfo
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode the session list %s: %v", recorder.Body.String(), err)
	}
	if len(rows) != 2 {
		t.Fatalf("GET /v1/sessions listed %d rows, want 2: %s", len(rows), recorder.Body.String())
	}
	for _, row := range rows {
		if row.SessionID == cached {
			wantTopics(t, row, cachedTopics)
		} else {
			wantTopics(t, row, nil)
		}
	}
	for id, want := range map[string][]string{missed: nil, cached: cachedTopics} {
		recorder, _ := entry(id)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /v1/registry/%s with the bus answering: status %d, body %s; want 200", id, recorder.Code, recorder.Body.String())
		}
		wantTopics(t, decodeSessionInfo(t, recorder), want)
	}
	if recorder, _ := interests(missed); recorder.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/interests/%s with the bus answering: status %d, body %s; want 404", missed, recorder.Code, recorder.Body.String())
	}
	if recorder, _ := interests(cached); recorder.Code != http.StatusOK {
		t.Fatalf("GET /v1/interests/%s with the bus answering: status %d, body %s; want 200", cached, recorder.Code, recorder.Body.String())
	}

	// The next protocol line the client writes arms the swallow; every byte after it is discarded
	// and the connection stays open, so the client still reports itself connected.
	relay.arm("\r\n", 1)

	wantBusUnanswered := func(t *testing.T, route string, recorder *httptest.ResponseRecorder, elapsed time.Duration) {
		t.Helper()
		if elapsed >= within {
			t.Fatalf("%s answered after %s (status %d, body %s), want inside %s - a bus that stopped answering must be refused inside the caller's 5 s attempt, not left to its timeout",
				route, elapsed, recorder.Code, recorder.Body.String(), within)
		}
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s with the bus silent: status %d, body %s; want 503", route, recorder.Code, recorder.Body.String())
		}
		var refusal apiError
		if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
			t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
		}
		if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) {
			t.Fatalf("the refusal reads %q, want it to begin %q", refusal.Error, busUnansweredPrefix)
		}
		if got := recorder.Header().Get("Retry-After"); got != "1" {
			t.Fatalf("Retry-After = %q, want 1", got)
		}
		t.Logf("%s with the bus silent: 503 in %s: %s", route, elapsed, refusal.Error)
	}
	t.Run("sessions", func(t *testing.T) {
		recorder, elapsed := list()
		wantBusUnanswered(t, "GET /v1/sessions", recorder, elapsed)
	})
	t.Run("registry", func(t *testing.T) {
		recorder, elapsed := entry(missed)
		wantBusUnanswered(t, "GET /v1/registry/"+missed, recorder, elapsed)
		// A cache hit asks nothing of the bus, so it is still served with its topics.
		hit, _ := entry(cached)
		if hit.Code != http.StatusOK {
			t.Fatalf("GET /v1/registry/%s, whose interest is cached, with the bus silent: status %d, body %s; want 200", cached, hit.Code, hit.Body.String())
		}
		wantTopics(t, decodeSessionInfo(t, hit), cachedTopics)
	})
	t.Run("interests", func(t *testing.T) {
		recorder, elapsed := interests(missed)
		wantBusUnanswered(t, "GET /v1/interests/"+missed, recorder, elapsed)
		if hit, _ := interests(cached); hit.Code != http.StatusOK {
			t.Fatalf("GET /v1/interests/%s, whose interest is cached, with the bus silent: status %d, body %s; want 200", cached, hit.Code, hit.Body.String())
		}
	})
}
