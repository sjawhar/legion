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
	"sync/atomic"
	"testing"
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
