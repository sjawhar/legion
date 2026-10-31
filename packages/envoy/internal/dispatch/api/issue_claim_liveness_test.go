package api

// The claim scenarios here drive the listener's per-session read (GET /v1/registry/{session_id})
// that judges whether a claim's holder is still working (INFRA-249): a holder the live list has
// dropped but whose record still answers, and a read that does not answer at all. Each stands up
// a listener of its own that answers that read and the live list differently, which is the point
// of the scenario, so they live apart from issue_claim_test.go, whose claimListener answers both
// from one roster.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// INFRA-249. The live list is the listener's in-memory cache of the registry: a session that
// is working while that cache refills is absent from it. A claim asks about the holder's own
// record instead, so a holder the list has dropped keeps its issue - judged by the list, this
// takeover was granted, and two sessions then worked the same issue.
func TestClaimRefusesAHolderTheLiveListDroppedButTheStoreStillHolds(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/registry/session-one":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"session_id": "session-one", "title": "worker session-one",
			})
		case "/v1/sessions":
			// The cache has not refilled the holder yet; it lists the contender alone.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"session_id": "session-two", "title": "worker session-two"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(listener.Close)
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	key := claimIssueKey(t, handler, "todo")

	if response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	}); response.Code != http.StatusOK {
		t.Fatalf("the holder's claim: status=%d body=%s", response.Code, response.Body.String())
	}

	contested := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-two", "Second agent"),
	})
	if contested.Code != http.StatusConflict {
		t.Fatalf("a takeover from a holder the list dropped: status=%d body=%s, want 409",
			contested.Code, contested.Body.String())
	}
	body := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, contested)
	if body.Code != "ISSUE_CLAIMED" {
		t.Fatalf("code = %q, want ISSUE_CLAIMED: %s", body.Code, body.Error)
	}
	if !strings.Contains(body.Error, "worker session-one") {
		t.Fatalf("the refusal must name the live holder by the title its record carries: %q", body.Error)
	}
	if holder := currentHolder(t, handler, key); holder != "session-one" {
		t.Fatalf("the working session keeps its claim: holder = %q", holder)
	}
}

// INFRA-249. A read of the holder's record that did not answer says nothing about whether that
// session is working, so the claim is refused as unavailable. "Unknown, therefore free" would
// hand a working session's issue to another on a transport error, which no later read undoes.
func TestClaimRefusesWhenTheHoldersSessionReadDoesNotAnswer(t *testing.T) {
	var refusing atomic.Bool
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/registry/") {
			http.NotFound(w, r)
			return
		}
		if refusing.Load() {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"envoy: the message bus did not answer: no responders"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": strings.TrimPrefix(r.URL.Path, "/v1/registry/"), "title": "worker",
		})
	}))
	t.Cleanup(listener.Close)
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	key := claimIssueKey(t, handler, "todo")

	// The first claim is of a free issue, which is judged against nobody and takes no read.
	if response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	}); response.Code != http.StatusOK {
		t.Fatalf("the holder's claim: status=%d body=%s", response.Code, response.Body.String())
	}

	refusing.Store(true)
	contested := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-two", "Second agent"),
	})
	if contested.Code != http.StatusServiceUnavailable {
		t.Fatalf("a takeover while the holder's record cannot be read: status=%d body=%s, want 503",
			contested.Code, contested.Body.String())
	}
	if code := decodeBody[struct {
		Code string `json:"code"`
	}](t, contested).Code; code != "ENVOY_UNAVAILABLE" {
		t.Fatalf("code = %q, want ENVOY_UNAVAILABLE: %s", code, contested.Body.String())
	}
	if holder := currentHolder(t, handler, key); holder != "session-one" {
		t.Fatalf("an unanswered read leaves the claim alone: holder = %q", holder)
	}
}

// unresponsiveHolder is the judgement a claim write reaches when the listener answers that the
// holder is unresponsive - past its live window, inside its gone horizon - last seen at
// lastSeen.
func unresponsiveHolder(holder *model.IssueClaim, state string, lastSeen time.Time) judgement {
	return judgement{holder: holder, live: holderLiveness{fetched: true, holder: &envoy.Session{
		SessionID: holder.Actor.ID, Title: "worker " + holder.Actor.ID,
		State: state, LastSeen: lastSeen.UnixMilli(),
	}}}
}

// dispatch://INFRA-392. A session that misses its heartbeats under box load is unresponsive,
// not gone: it is still working the issue it claimed, so neither another agent's claim nor
// another agent's release takes it away, and the refusal says what state the holder is in and
// since when, because the caller's real question is whether to wait. Once the holder has not
// been seen for longer than the grace, the claim is free to take and to clear.
func TestAClaimSurvivesItsHoldersSilenceForTheGrace(t *testing.T) {
	lastSeen := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	holder := &model.IssueClaim{
		Actor: model.Actor{Kind: "session", ID: "ses-stalled"},
		At:    lastSeen.Add(-2 * time.Hour),
	}
	contender := model.Actor{Kind: "session", ID: "ses-two"}
	grace := contracts.IssueClaimUnresponsiveGrace
	for _, rule := range []struct {
		name string
		humanRule
	}{
		{name: "a claim", humanRule: humanRule{forcible: true}},
		{name: "a release", humanRule: humanRule{override: true}},
	} {
		for _, tc := range []struct {
			name    string
			state   string
			now     time.Time
			blocked bool
		}{
			{name: "while the holder is live", state: envoy.StateLive, now: lastSeen.Add(time.Minute), blocked: true},
			{name: "while it is unresponsive", state: envoy.StateUnresponsive, now: lastSeen.Add(10 * time.Minute), blocked: true},
			// The grace is the age the silence may reach, as the registry's own windows are.
			{name: "at the last moment of the grace", state: envoy.StateUnresponsive, now: lastSeen.Add(grace), blocked: true},
			{name: "once the silence outlasts the grace", state: envoy.StateUnresponsive, now: lastSeen.Add(grace + time.Second)},
			{name: "once the holder is gone", state: envoy.StateGone, now: lastSeen.Add(2 * time.Hour)},
		} {
			judged := unresponsiveHolder(holder, tc.state, lastSeen)
			blocked := claimBlockedBy(holder, contender, rule.humanRule, judged, tc.now)
			if (blocked != nil) != tc.blocked {
				t.Fatalf("%s %s: %v, want blocked=%v", rule.name, tc.name, blocked, tc.blocked)
			}
			if !tc.blocked || tc.state != envoy.StateUnresponsive {
				continue
			}
			// The refusal names the state, the heartbeat it was last seen at, and the moment
			// the claim comes free, so the refused caller knows whether to wait or to ask a
			// person. A bare "is still running" would be a claim nothing established.
			text := blocked.Error()
			for _, want := range []string{
				"session ses-stalled (worker ses-stalled)",
				"is unresponsive since " + lastSeen.Format(time.RFC3339),
				"keeps the claim until " + lastSeen.Add(grace).Format(time.RFC3339),
			} {
				if !strings.Contains(text, want) {
					t.Errorf("%s %s: refusal %q, want it to carry %q", rule.name, tc.name, text, want)
				}
			}
			if strings.Contains(text, "is still running") {
				t.Errorf("%s %s: refusal %q claims the holder is running, which no read established",
					rule.name, tc.name, text)
			}
		}
	}
	// A person still takes it, with force on the claim route and by being a person on the
	// release route; the grace holds agents apart, never a human off the issue.
	human := model.Actor{Kind: "user", ID: "alice@example.com"}
	judged := unresponsiveHolder(holder, envoy.StateUnresponsive, lastSeen)
	for _, rule := range []humanRule{{override: true, forcible: true}, {override: true}} {
		if blocked := claimBlockedBy(holder, human, rule, judged, lastSeen.Add(time.Minute)); blocked != nil {
			t.Fatalf("a person over an unresponsive holder: %v, want it allowed", blocked)
		}
	}
}

// A holder the listener calls unresponsive while naming no heartbeat dates nothing, and the
// claim stays with it: the direction that errs toward one session per issue is to wait, and the
// refusal then says only what the live-holder refusal says.
func TestAnUndatedSilenceKeepsTheClaim(t *testing.T) {
	holder := &model.IssueClaim{Actor: model.Actor{Kind: "session", ID: "ses-stalled"}, At: time.Now().UTC()}
	judged := judgement{holder: holder, live: holderLiveness{fetched: true, holder: &envoy.Session{
		SessionID: "ses-stalled", State: envoy.StateUnresponsive,
	}}}
	blocked := claimBlockedBy(holder, model.Actor{Kind: "session", ID: "ses-two"}, humanRule{forcible: true}, judged, time.Now())
	if blocked == nil {
		t.Fatal("an unresponsive holder with no last_seen lost its claim; nothing established when it was last seen")
	}
	if !strings.Contains(blocked.Error(), "is unresponsive, not gone") {
		t.Fatalf("refusal %q does not name the state the listener did give", blocked.Error())
	}
	if strings.Contains(blocked.Error(), "since") || strings.Contains(blocked.Error(), "0001-01-01") {
		t.Fatalf("refusal %q dates a silence the listener did not date", blocked.Error())
	}
}

// stalledListener is a listener whose one session, session-one, answers every read with the
// state and heartbeat the test sets, on the per-session route a claim write reads and in the
// list an issue read's roster takes. It serves the live rows alone to a read that did not ask
// for the unresponsive ones, as the listener does, so a Dispatch that forgot the selector sees
// a session that has ended.
type stalledListener struct {
	mu       sync.Mutex
	state    string
	lastSeen time.Time
}

func (l *stalledListener) set(state string, lastSeen time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state, l.lastSeen = state, lastSeen
}

func (l *stalledListener) row() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return map[string]any{
		"session_id": "session-one", "title": "worker session-one",
		"state": l.state, "last_seen": l.lastSeen.UnixMilli(),
	}
}

// serveStalledSession stands up that listener and the Dispatch server reading it, with the
// session starting out live and seen at lastSeen.
func serveStalledSession(t *testing.T, lastSeen time.Time) (http.Handler, *stalledListener) {
	t.Helper()
	stalled := &stalledListener{state: envoy.StateLive, lastSeen: lastSeen}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		row := stalled.row()
		asked := r.URL.Query().Get("include") == "unresponsive"
		live := row["state"] == envoy.StateLive
		switch {
		case r.URL.Path == "/v1/sessions":
			rows := []map[string]any{}
			if live || asked {
				rows = append(rows, row)
			}
			_ = json.NewEncoder(w).Encode(rows)
		case r.URL.Path == "/v1/registry/session-one" && (live || asked):
			_ = json.NewEncoder(w).Encode(row)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no live session"}`))
		}
	}))
	t.Cleanup(listener.Close)
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	// Every read asks the listener, so a test that moves the session sees the move at its next
	// read rather than waiting out the roster's five seconds.
	served.rosters.ttl, served.rosters.silenceTTL = 0, 0
	return handler, stalled
}

// issueClaimLiveness is what an issue read carries beside the claim.
type issueClaimLiveness struct {
	Claim *struct {
		Actor struct {
			ID string `json:"id"`
		} `json:"actor"`
	} `json:"claim"`
	HolderState *string `json:"holder_state"`
	LastSeen    *string `json:"last_seen"`
	TakeableAt  *string `json:"takeable_at"`
}

func readIssueClaimLiveness(t *testing.T, handler http.Handler, key string) issueClaimLiveness {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read issue %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
	return decodeBody[issueClaimLiveness](t, response)
}

// dispatch://INFRA-392, end to end. A session under box load misses its heartbeats; the
// registry calls it unresponsive and keeps its record. Another agent may neither take its
// issue nor release it until the silence outlasts the grace, and the refusal names the state
// and the heartbeat it was last seen at, so the refused caller knows to wait.
func TestAnotherAgentCannotTakeOrReleaseAnUnresponsiveHoldersIssue(t *testing.T) {
	handler, stalled := serveStalledSession(t, time.Now())
	key := claimIssueKey(t, handler, "todo")
	if response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	}); response.Code != http.StatusOK {
		t.Fatalf("the holder's claim: status=%d body=%s", response.Code, response.Body.String())
	}

	lastSeen := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Millisecond)
	stalled.set(envoy.StateUnresponsive, lastSeen)
	for _, attempt := range []struct {
		name   string
		method string
	}{
		{name: "a claim", method: http.MethodPost},
		{name: "a release", method: http.MethodDelete},
	} {
		refused := bearerRequest(t, handler, attempt.method, "/api/v1/issues/"+key+"/claim", map[string]any{
			"actor": claimActorBody("session-two", "Second agent"),
		})
		if refused.Code != http.StatusConflict {
			t.Fatalf("%s over an unresponsive holder: status=%d body=%s, want 409",
				attempt.name, refused.Code, refused.Body.String())
		}
		body := decodeBody[struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}](t, refused)
		if body.Code != "ISSUE_CLAIMED" {
			t.Fatalf("%s: code = %q, want ISSUE_CLAIMED: %s", attempt.name, body.Code, body.Error)
		}
		for _, want := range []string{
			"is unresponsive since " + lastSeen.Format(time.RFC3339),
			"keeps the claim until " + lastSeen.Add(contracts.IssueClaimUnresponsiveGrace).Format(time.RFC3339),
		} {
			if !strings.Contains(body.Error, want) {
				t.Fatalf("%s: refusal %q, want it to carry %q", attempt.name, body.Error, want)
			}
		}
		if holder := currentHolder(t, handler, key); holder != "session-one" {
			t.Fatalf("%s: holder = %q, want the stalled session to keep its issue", attempt.name, holder)
		}
	}

	// Past the grace the session has had its chance to come back, and the issue is free.
	stalled.set(envoy.StateUnresponsive, time.Now().Add(-(contracts.IssueClaimUnresponsiveGrace + time.Minute)))
	taken := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-two", "Second agent"),
	})
	if taken.Code != http.StatusOK {
		t.Fatalf("a claim once the silence outlasted the grace: status=%d body=%s", taken.Code, taken.Body.String())
	}
	if holder := currentHolder(t, handler, key); holder != "session-two" {
		t.Fatalf("holder = %q, want session-two", holder)
	}
}

// An issue read says what state the session working it is in, when it was last seen, and from
// when its claim may be taken: the three a person or an agent needs to decide whether to wait
// for it. They sit beside the claim, never inside it, because model.IssueClaim is the payload
// of issue.claimed and issue.released and a read-time state must not persist into one.
func TestIssueReadCarriesTheClaimHoldersStateBesideTheClaim(t *testing.T) {
	seen := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	handler, stalled := serveStalledSession(t, seen)
	key := claimIssueKey(t, handler, "todo")
	if response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	}); response.Code != http.StatusOK {
		t.Fatalf("the holder's claim: status=%d body=%s", response.Code, response.Body.String())
	}

	live := readIssueClaimLiveness(t, handler, key)
	if live.Claim == nil || live.Claim.Actor.ID != "session-one" {
		t.Fatalf("claim = %#v, want session-one", live.Claim)
	}
	if live.HolderState == nil || *live.HolderState != "live" {
		t.Fatalf("holder_state = %v, want live", live.HolderState)
	}
	if live.LastSeen == nil || *live.LastSeen != seen.Format(time.RFC3339Nano) {
		t.Fatalf("last_seen = %v, want the holder's heartbeat %s", live.LastSeen, seen.Format(time.RFC3339Nano))
	}
	// Nothing frees a live holder's claim by waiting, so there is no instant to name.
	if live.TakeableAt != nil {
		t.Fatalf("takeable_at = %v for a live holder, want null", *live.TakeableAt)
	}

	stalled.set(envoy.StateUnresponsive, seen)
	stale := readIssueClaimLiveness(t, handler, key)
	if stale.HolderState == nil || *stale.HolderState != "unresponsive" {
		t.Fatalf("holder_state = %v, want unresponsive", stale.HolderState)
	}
	takeable := seen.Add(contracts.IssueClaimUnresponsiveGrace).Format(time.RFC3339Nano)
	if stale.TakeableAt == nil || *stale.TakeableAt != takeable {
		t.Fatalf("takeable_at = %v, want %s: the heartbeat plus the grace", stale.TakeableAt, takeable)
	}
}

// A listener that cannot be read says nothing about the holder. An issue read must still
// render - a page that fails because a session's heartbeats are late is worse than one that
// says it cannot tell - so it answers holder_state unknown, never gone, which would read as a
// free claim. A write is the opposite: it refuses, because handing one session's work to
// another on a transport error is not a mistake the next read undoes.
func TestIssueReadSaysUnknownWhileAClaimWriteRefusesWithNoListener(t *testing.T) {
	var refilling atomic.Bool
	var reachable atomic.Bool
	reachable.Store(true)
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case refilling.Load():
			answerListRefilling(w, "7")
		case !reachable.Load():
			http.Error(w, "listener restarting", http.StatusServiceUnavailable)
		case r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"session-one","title":"worker","state":"live","last_seen":1}]`))
		default:
			_, _ = w.Write([]byte(`{"session_id":"session-one","title":"worker","state":"live","last_seen":1}`))
		}
	}))
	t.Cleanup(listener.Close)
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	served.rosters.ttl, served.rosters.silenceTTL = 0, 0
	key := claimIssueKey(t, handler, "todo")
	if response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	}); response.Code != http.StatusOK {
		t.Fatalf("the holder's claim: status=%d body=%s", response.Code, response.Body.String())
	}

	for _, tc := range []struct {
		name  string
		apply func()
	}{
		{name: "the listener is unreachable", apply: func() { reachable.Store(false) }},
		{name: "the listener is refilling its list", apply: func() { refilling.Store(true) }},
	} {
		refilling.Store(false)
		reachable.Store(true)
		tc.apply()
		read := readIssueClaimLiveness(t, handler, key)
		if read.HolderState == nil || *read.HolderState != "unknown" {
			t.Fatalf("%s: holder_state = %v, want unknown", tc.name, read.HolderState)
		}
		if read.TakeableAt != nil || read.LastSeen != nil {
			t.Fatalf("%s: last_seen=%v takeable_at=%v, want both null when nothing was established",
				tc.name, read.LastSeen, read.TakeableAt)
		}
		refused := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
			"actor": claimActorBody("session-two", "Second agent"),
		})
		if refused.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: a claim write answered status=%d body=%s, want 503",
				tc.name, refused.Code, refused.Body.String())
		}
		if code := decodeBody[struct {
			Code string `json:"code"`
		}](t, refused).Code; code != "ENVOY_UNAVAILABLE" {
			t.Fatalf("%s: code = %q, want ENVOY_UNAVAILABLE: %s", tc.name, code, refused.Body.String())
		}
		if holder := currentHolder(t, handler, key); holder != "session-one" {
			t.Fatalf("%s: holder = %q, want the claim left alone", tc.name, holder)
		}
	}
}
