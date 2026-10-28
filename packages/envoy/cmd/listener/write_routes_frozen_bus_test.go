package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// writeRoutesWithin is how long a write route may take to answer a bus that stopped answering: its
// sendRequestBudget (3.5 s) and a margin, inside the HTTP client's 5 s attempt.
const writeRoutesWithin = 4 * time.Second

// writeRouteRig is a listener's stores and /v1 routes on a bus reached through the relay
// TestASendAndAPublishAnswerABusThatStoppedAnsweringInsideTheBudget freezes.
type writeRouteRig struct {
	relay    *listenerRelay
	registry *store.Registry
	sessions *session.SessionRegistry
	mux      *http.ServeMux
}

func newWriteRouteRig(t *testing.T) *writeRouteRig {
	t.Helper()
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
	mux := http.NewServeMux()
	registerV1Routes(mux, &listenerDeps{client: client, registry: registry, sessions: sessions}, "test-machine", logging.New("test"))
	return &writeRouteRig{relay: relay, registry: registry, sessions: sessions, mux: mux}
}

func (rig *writeRouteRig) register(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := rig.sessions.Put(id, session.SessionEntry{Port: 1, MachineID: "test-machine", Dir: "/test/" + id, Title: id}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		if _, err := rig.sessions.Get(id); err != nil {
			t.Fatalf("the listener's session cache does not hold %s (%v)", id, err)
		}
	}
}

func (rig *writeRouteRig) serve(method, path, body string) (*httptest.ResponseRecorder, time.Duration) {
	recorder := httptest.NewRecorder()
	started := time.Now()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
	return recorder, time.Since(started)
}

// wantWriteBusUnanswered asserts the named 503 with Retry-After: 1, inside writeRoutesWithin.
func wantWriteBusUnanswered(t *testing.T, route string, recorder *httptest.ResponseRecorder, elapsed time.Duration) {
	t.Helper()
	if elapsed >= writeRoutesWithin {
		t.Fatalf("%s answered after %s (status %d, body %s), want inside %s - a bus that stopped answering must be refused inside the caller's 5 s attempt, not left to its timeout",
			route, elapsed, recorder.Code, recorder.Body.String(), writeRoutesWithin)
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

// Every /v1 route that writes - subscribe, unsubscribe, DELETE /v1/interests/<session_id>, POST
// /v1/roles/set and DELETE /v1/sessions/<session_id> - does its store work within
// sendRequestBudget of the request's own context. nats.go's KV reads and writes take no context
// and wait out the JetStream MaxWait (10 s), as long as this listener's HTTP write timeout, so on a
// bus that went silent without closing the connection each route must still answer the named 503
// with Retry-After: 1 inside the caller's 5 s attempt.
//
// The record each frozen request needs is not in the interest cache, so the read it makes first
// goes to the bucket; for subscribe and unsubscribe a second request names a session whose record
// is cached, so the read is answered from memory and the write itself is what meets the silent bus.
// POST /v1/roles/set's first bucket call is the claimant's registration write, and DELETE
// /v1/sessions/<session_id>'s the session bucket's read-then-delete. The negative control is each
// route asked before the freeze, on the same rig, for a session of its own: each is served.
func TestTheWriteRoutesAnswerABusThatStoppedAnsweringInsideTheBudget(t *testing.T) {
	const (
		topic         = "notifications.team.write-routes"
		otherTopic    = "notifications.team.write-routes-other"
		control       = "ses_write_routes_control"
		controlLeaver = "ses_write_routes_control_leaving"
		subscriber    = "ses_write_routes_subscriber_with_no_record"
		unsubscriber  = "ses_write_routes_unsubscriber_with_no_record"
		removed       = "ses_write_routes_removed_with_no_record"
		cachedSub     = "ses_write_routes_cached_subscriber"
		cachedUnsub   = "ses_write_routes_cached_unsubscriber"
		claimant      = "ses_write_routes_claimant"
		leaver        = "ses_write_routes_leaving"
	)
	rig := newWriteRouteRig(t)
	rig.register(t, control, controlLeaver, claimant, leaver)
	for _, id := range []string{cachedSub, cachedUnsub} {
		if _, err := rig.registry.Upsert(store.Interest{SessionID: id, MachineID: "test-machine", Dir: "/test/" + id},
			[]string{contracts.AgentSubject(id), topic, otherTopic}); err != nil {
			t.Fatalf("write %s's interest record: %v", id, err)
		}
	}
	subscribe := func(id, topic string) string {
		return `{"session_id":"` + id + `","topics":["` + topic + `"],"self_subscribed":true}`
	}
	unsubscribe := func(id, topic string) string {
		return `{"session_id":"` + id + `","topics":["` + topic + `"]}`
	}

	// The control: with the bus answering, every route is served.
	for _, request := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/v1/interests/subscribe", subscribe(control, topic), http.StatusOK},
		{http.MethodPost, "/v1/interests/unsubscribe", unsubscribe(control, topic), http.StatusOK},
		{http.MethodDelete, "/v1/interests/" + control, "", http.StatusNoContent},
		{http.MethodPost, "/v1/roles/set", `{"session_id":"` + control + `","role":"write-routes-control"}`, http.StatusOK},
		{http.MethodDelete, "/v1/sessions/" + controlLeaver, "", http.StatusOK},
	} {
		if recorder, _ := rig.serve(request.method, request.path, request.body); recorder.Code != request.want {
			t.Fatalf("%s %s with the bus answering: status %d, body %s; want %d", request.method, request.path, recorder.Code, recorder.Body.String(), request.want)
		}
	}
	var cachedIDs []string
	for _, item := range rig.registry.List() {
		cachedIDs = append(cachedIDs, item.SessionID)
	}
	for _, id := range []string{subscriber, unsubscriber, removed} {
		if slices.Contains(cachedIDs, id) {
			t.Fatalf("the interest cache holds %s (%v), want it read from the bucket", id, cachedIDs)
		}
	}
	for _, id := range []string{cachedSub, cachedUnsub} {
		if !slices.Contains(cachedIDs, id) {
			t.Fatalf("the interest cache does not hold %s (%v): its write would not be what meets the silent bus", id, cachedIDs)
		}
	}

	// The next protocol line the client writes arms the swallow; every byte after it is discarded
	// and the connection stays open, so the client still reports itself connected.
	rig.relay.arm("\r\n", 1)

	t.Run("frozen", func(t *testing.T) {
		for _, request := range []struct {
			name, method, path, body string
		}{
			{"subscribe", http.MethodPost, "/v1/interests/subscribe", subscribe(subscriber, topic)},
			{"subscribe a cached session", http.MethodPost, "/v1/interests/subscribe", subscribe(cachedSub, "notifications.team.write-routes-new")},
			{"unsubscribe", http.MethodPost, "/v1/interests/unsubscribe", unsubscribe(unsubscriber, topic)},
			{"unsubscribe a cached session", http.MethodPost, "/v1/interests/unsubscribe", unsubscribe(cachedUnsub, otherTopic)},
			{"delete interests", http.MethodDelete, "/v1/interests/" + removed, ""},
			{"claim a role", http.MethodPost, "/v1/roles/set", `{"session_id":"` + claimant + `","role":"write-routes"}`},
			{"delete a session", http.MethodDelete, "/v1/sessions/" + leaver, ""},
		} {
			t.Run(request.name, func(t *testing.T) {
				t.Parallel()
				recorder, elapsed := rig.serve(request.method, request.path, request.body)
				wantWriteBusUnanswered(t, request.method+" "+request.path, recorder, elapsed)
			})
		}
	})
}

// POST /v1/roles/set's claim (store.Registry.SetRoleWithPrevious) and a soft claim's read of the
// role's holder are role-bucket calls the claimant's registration write does not reach, so each
// is frozen on its own here: the relay passes everything up to the first protocol line naming the
// role bucket and swallows that line and every one after it. For the claim's read that is the
// direct get of the claim; for the claim's write it is the conditional create, which nats.go
// publishes with headers (HPUB) after the read and the claimant's interest write have answered,
// so the write itself is what meets the silent bus, and the rollback that follows a write that did
// not answer must not take the request past its budget either. The negative control is a hard claim
// of another role (role-store-control) by the same claimant on the same rig before the freeze: it
// is served.
func TestAClaimWhoseRoleStoreStopsAnsweringIsAnsweredInsideTheBudget(t *testing.T) {
	const claimant = "ses_role_store_claimant"
	for _, freeze := range []struct {
		name, trigger, body string
	}{
		{"the claim's read", "KV_" + bus.RoleClaimBucket, `{"session_id":"` + claimant + `","role":"role-store-read"}`},
		{"a soft claim's read", "KV_" + bus.RoleClaimBucket, `{"session_id":"` + claimant + `","role":"role-store-soft","soft":true}`},
		{"the claim's write", "HPUB $KV." + bus.RoleClaimBucket + ".", `{"session_id":"` + claimant + `","role":"role-store-write"}`},
	} {
		t.Run(freeze.name, func(t *testing.T) {
			t.Parallel()
			rig := newWriteRouteRig(t)
			rig.register(t, claimant)
			if recorder, _ := rig.serve(http.MethodPost, "/v1/roles/set", `{"session_id":"`+claimant+`","role":"role-store-control"}`); recorder.Code != http.StatusOK {
				t.Fatalf("POST /v1/roles/set with the bus answering: status %d, body %s; want 200", recorder.Code, recorder.Body.String())
			}
			rig.relay.arm(freeze.trigger, 1)
			recorder, elapsed := rig.serve(http.MethodPost, "/v1/roles/set", freeze.body)
			wantWriteBusUnanswered(t, "POST /v1/roles/set", recorder, elapsed)
		})
	}
}

// store.Registry.SetRoleWithPrevious joins a claim that lost its conditional write
// (nats.ErrKeyExists) with its holder read and its rollback (errors.Join). errors.Is finds a store
// answer in any part of a join, so a lost claim joined with a call the bus did not answer was
// answered as the store's own, a 500 with no Retry-After. A join is a store answer only when every
// part of it is; anything else is the bus-unavailable 503 with Retry-After: 1. The controls: a join
// of store answers, a store answer wrapped alone, and a store answer wrapped with its detail by one
// fmt.Errorf (an unreadable record and its decode error) are each still the store's answer.
func TestAJoinedStoreErrorIsTheStoresAnswerOnlyWhenEveryPartIs(t *testing.T) {
	dropped := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	for _, c := range []struct {
		name     string
		err      error
		answered bool
	}{
		{"a lost claim joined with a dropped connection", errors.Join(nats.ErrKeyExists, dropped), false},
		{"a lost claim joined with no responders", errors.Join(nats.ErrKeyExists, nats.ErrNoResponders), false},
		{"a lost claim joined with a join that holds no responders", errors.Join(nats.ErrKeyExists, errors.Join(nats.ErrKeyNotFound, nats.ErrNoResponders)), false},
		{"a wrapped join of a lost claim and a dropped connection", fmt.Errorf("claim: %w", errors.Join(nats.ErrKeyExists, dropped)), false},
		{"a dropped connection alone", dropped, false},
		{"a lost claim joined with no key", errors.Join(nats.ErrKeyExists, nats.ErrKeyNotFound), true},
		{"a lost claim alone", fmt.Errorf("write role r's claim: %w", nats.ErrKeyExists), true},
		{"an unreadable record and its decode error", fmt.Errorf("%w: %w", store.ErrMalformedInterest, errors.New("unexpected end of JSON input")), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := storeAnswered(context.Background(), c.err); got != c.answered {
				t.Fatalf("storeAnswered(%q) = %t, want %t", c.err, got, c.answered)
			}
			recorder := httptest.NewRecorder()
			writeStoreError(context.Background(), recorder, c.err, http.StatusInternalServerError)
			code, retry := recorder.Code, recorder.Header().Get("Retry-After")
			if c.answered && (code == http.StatusServiceUnavailable || retry != "") {
				t.Fatalf("the store's answer %q was answered %d with Retry-After %q, want the route's own status", c.err, code, retry)
			}
			if !c.answered && (code != http.StatusServiceUnavailable || retry != "1") {
				t.Fatalf("%q was answered %d with Retry-After %q (%s), want 503 with Retry-After: 1", c.err, code, retry, recorder.Body.String())
			}
		})
	}
}
