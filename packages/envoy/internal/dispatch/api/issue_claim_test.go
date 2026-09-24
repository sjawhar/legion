package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// claimHandler is a server whose Envoy listener lists exactly the sessions in live, which a
// test mutates to end a session.
func claimHandler(t *testing.T, live *[]string) http.Handler {
	t.Helper()
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" {
			http.NotFound(w, r)
			return
		}
		sessions := make([]map[string]any, 0, len(*live))
		for _, id := range *live {
			sessions = append(sessions, map[string]any{"session_id": id, "title": "worker " + id})
		}
		_ = json.NewEncoder(w).Encode(sessions)
	}))
	t.Cleanup(listener.Close)
	handler, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	return handler
}

// claimIssueKey seeds a project and one issue at the given status.
func claimIssueKey(t *testing.T, handler http.Handler, status string) string {
	t.Helper()
	project := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice")
	if project.Code != http.StatusCreated && project.Code != http.StatusConflict {
		t.Fatalf("create project: status=%d body=%s", project.Code, project.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Claimable work",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	key := decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
	moved := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]string{
		"status": status,
	}, "alice")
	if moved.Code != http.StatusOK {
		t.Fatalf("move issue to %s: status=%d body=%s", status, moved.Code, moved.Body.String())
	}
	return key
}

// claimedIssue is the part of an issue read a claim test asserts on.
type claimedIssue struct {
	Status string `json:"status"`
	Claim  *struct {
		Actor struct {
			Kind   string `json:"kind"`
			ID     string `json:"id"`
			Origin *struct {
				SessionTitle string `json:"session_title"`
			} `json:"origin"`
		} `json:"actor"`
		At string `json:"at"`
	} `json:"claim"`
}

func claimActorBody(id, title string) map[string]any {
	return map[string]any{
		"kind":   "session",
		"id":     id,
		"origin": map[string]any{"session_title": title},
	}
}

// claimEvents returns the issue's claim and release events, oldest first.
func claimEvents(t *testing.T, handler http.Handler, key string) []struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
} {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", response.Code, response.Body.String())
	}
	all := decodeBody[[]struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}](t, response)
	claims := all[:0]
	for _, event := range all {
		if event.Type == "issue.claimed" || event.Type == "issue.released" {
			claims = append(claims, event)
		}
	}
	return claims
}

func TestClaimRefusesALiveHolderAndPassesOnWhenThatSessionEnds(t *testing.T) {
	live := []string{"session-one", "session-two"}
	handler := claimHandler(t, &live)
	key := claimIssueKey(t, handler, "todo")

	first := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	})
	if first.Code != http.StatusOK {
		t.Fatalf("first claim: status=%d body=%s", first.Code, first.Body.String())
	}
	claimed := decodeBody[claimedIssue](t, first)
	if claimed.Claim == nil || claimed.Claim.Actor.ID != "session-one" {
		t.Fatalf("first claim holder: %+v", claimed)
	}

	contested := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-two", "Second agent"),
	})
	if contested.Code != http.StatusConflict {
		t.Fatalf("contested claim: status=%d body=%s", contested.Code, contested.Body.String())
	}
	conflict := decodeBody[struct {
		Code  string       `json:"code"`
		Error string       `json:"error"`
		Claim claimedIssue `json:"-"`
	}](t, contested)
	if conflict.Code != "ISSUE_CLAIMED" {
		t.Fatalf("contested claim code: %+v", conflict)
	}
	if !strings.Contains(conflict.Error, "session-one") || !strings.Contains(conflict.Error, "Implementer") {
		t.Fatalf("the refusal must name the live holder and its title: %q", conflict.Error)
	}

	// The holder's session ends: the listener no longer lists it, so any agent may take it.
	live = []string{"session-two"}
	takeover := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-two", "Second agent"),
	})
	if takeover.Code != http.StatusOK {
		t.Fatalf("takeover: status=%d body=%s", takeover.Code, takeover.Body.String())
	}
	took := decodeBody[claimedIssue](t, takeover)
	if took.Claim == nil || took.Claim.Actor.ID != "session-two" {
		t.Fatalf("takeover holder: %+v", took)
	}
	events := claimEvents(t, handler, key)
	if len(events) != 2 {
		t.Fatalf("claim events: %+v", events)
	}
	last := events[1]
	if last.Type != "issue.claimed" || last.Payload["reason"] != "takeover" {
		t.Fatalf("takeover event: %+v", last)
	}
	previous, ok := last.Payload["previous_claim"].(map[string]any)
	if !ok {
		t.Fatalf("takeover event must carry the previous claim: %+v", last)
	}
	actor, _ := previous["actor"].(map[string]any)
	if actor["id"] != "session-one" {
		t.Fatalf("previous claimant: %+v", previous)
	}
}

func TestClaimRecordsTheRequestingSessionAndNoOther(t *testing.T) {
	live := []string{"session-one"}
	handler := claimHandler(t, &live)
	key := claimIssueKey(t, handler, "todo")

	// A human's claim is the human's, whatever session the body names: a cookie caller's
	// identity comes from the cookie, never the body.
	human := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Someone else"),
	}, "alice")
	if human.Code != http.StatusOK {
		t.Fatalf("human claim: status=%d body=%s", human.Code, human.Body.String())
	}
	held := decodeBody[claimedIssue](t, human)
	if held.Claim == nil || held.Claim.Actor.Kind != "user" || held.Claim.Actor.ID != "alice" {
		t.Fatalf("a human claim must record the human: %+v", held.Claim)
	}

	// No argument claims for another session: the request shape has none, and an invented
	// one is refused rather than quietly ignored.
	onBehalf := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor":      claimActorBody("session-one", "Implementer"),
		"session_id": "session-two",
	})
	if onBehalf.Code != http.StatusBadRequest {
		t.Fatalf("claiming for another session: status=%d body=%s", onBehalf.Code, onBehalf.Body.String())
	}

	release := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/claim", nil, "alice")
	if release.Code != http.StatusOK {
		t.Fatalf("human release: status=%d body=%s", release.Code, release.Body.String())
	}
	session := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	})
	if session.Code != http.StatusOK {
		t.Fatalf("session claim: status=%d body=%s", session.Code, session.Body.String())
	}
	took := decodeBody[claimedIssue](t, session)
	if took.Claim == nil || took.Claim.Actor.Kind != "session" || took.Claim.Actor.ID != "session-one" {
		t.Fatalf("a bearer claim must record the requesting session: %+v", took.Claim)
	}
	if took.Claim.Actor.Origin == nil || took.Claim.Actor.Origin.SessionTitle != "Implementer" {
		t.Fatalf("the claim keeps the session's title for the surfaces that name it: %+v", took.Claim)
	}
}

// The claim and the status are separate records (Sami, 2026-09-24): one names the session
// implementing the issue, the other is how humans track where work has got to.
func TestClaimAndStatusMoveIndependently(t *testing.T) {
	live := []string{"session-one"}
	handler := claimHandler(t, &live)
	key := claimIssueKey(t, handler, "todo")

	claimed := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	})
	if claimed.Code != http.StatusOK {
		t.Fatalf("claim: status=%d body=%s", claimed.Code, claimed.Body.String())
	}
	if got := decodeBody[claimedIssue](t, claimed); got.Status != "todo" {
		t.Fatalf("claiming must not move the status: got %q, want todo", got.Status)
	}

	released := bearerRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	})
	if released.Code != http.StatusOK {
		t.Fatalf("release: status=%d body=%s", released.Code, released.Body.String())
	}
	moved := bearerRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
		"status": "in_progress",
		"actor":  claimActorBody("session-one", "Implementer"),
	})
	if moved.Code != http.StatusOK {
		t.Fatalf("move to in_progress: status=%d body=%s", moved.Code, moved.Body.String())
	}
	after := decodeBody[claimedIssue](t, moved)
	if after.Status != "in_progress" {
		t.Fatalf("status after the move: %q", after.Status)
	}
	if after.Claim != nil {
		t.Fatalf("a status move must not claim the issue: %+v", after.Claim)
	}
}

func TestClaimReleaseBelongsToItsHolderAHumanOrAnEndedSession(t *testing.T) {
	live := []string{"session-one", "session-two"}
	handler := claimHandler(t, &live)
	key := claimIssueKey(t, handler, "todo")
	claim := func(id string) *httptest.ResponseRecorder {
		return bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
			"actor": claimActorBody(id, "worker "+id),
		})
	}
	release := func(id string) *httptest.ResponseRecorder {
		return bearerRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/claim", map[string]any{
			"actor": claimActorBody(id, "worker "+id),
		})
	}

	if response := claim("session-one"); response.Code != http.StatusOK {
		t.Fatalf("claim: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := release("session-two"); response.Code != http.StatusConflict {
		t.Fatalf("another live session must not release the claim: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := release("session-one"); response.Code != http.StatusOK {
		t.Fatalf("the holder releases its own claim: status=%d body=%s", response.Code, response.Body.String())
	}

	if response := claim("session-one"); response.Code != http.StatusOK {
		t.Fatalf("re-claim: status=%d body=%s", response.Code, response.Body.String())
	}
	humanRelease := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/claim", nil, "alice")
	if humanRelease.Code != http.StatusOK {
		t.Fatalf("a human releases any claim: status=%d body=%s", humanRelease.Code, humanRelease.Body.String())
	}
	if got := decodeBody[claimedIssue](t, humanRelease); got.Claim != nil {
		t.Fatalf("claim after release: %+v", got.Claim)
	}

	if response := claim("session-one"); response.Code != http.StatusOK {
		t.Fatalf("claim before the session ends: status=%d body=%s", response.Code, response.Body.String())
	}
	live = []string{"session-two"}
	if response := release("session-two"); response.Code != http.StatusOK {
		t.Fatalf("an ended session's claim may be cleared by anyone: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestClosingAnIssueReleasesItsClaimAndAClosedIssueTakesNone(t *testing.T) {
	live := []string{"session-one"}
	handler := claimHandler(t, &live)
	key := claimIssueKey(t, handler, "todo")
	if response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	}); response.Code != http.StatusOK {
		t.Fatalf("claim: status=%d body=%s", response.Code, response.Body.String())
	}

	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]string{
		"status": "done",
	}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	if got := decodeBody[claimedIssue](t, closed); got.Claim != nil {
		t.Fatalf("closing an issue releases its claim: %+v", got.Claim)
	}
	events := claimEvents(t, handler, key)
	if len(events) != 2 || events[1].Type != "issue.released" || events[1].Payload["reason"] != "closed" {
		t.Fatalf("close must record the release: %+v", events)
	}
	previous, _ := events[1].Payload["previous_claim"].(map[string]any)
	actor, _ := previous["actor"].(map[string]any)
	if actor["id"] != "session-one" {
		t.Fatalf("the release names whose claim it was: %+v", events[1].Payload)
	}

	reclaim := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	})
	if reclaim.Code != http.StatusConflict {
		t.Fatalf("claiming a closed issue: status=%d body=%s", reclaim.Code, reclaim.Body.String())
	}
	if code := decodeBody[struct {
		Code string `json:"code"`
	}](t, reclaim).Code; code != "ISSUE_CLOSED" {
		t.Fatalf("claiming a closed issue code: %q", code)
	}
}

func TestClaimIsOnEveryIssueRead(t *testing.T) {
	live := []string{"session-one"}
	handler := claimHandler(t, &live)
	key := claimIssueKey(t, handler, "todo")
	if response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	}); response.Code != http.StatusOK {
		t.Fatalf("claim: status=%d body=%s", response.Code, response.Body.String())
	}

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("read issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	if got := decodeBody[claimedIssue](t, detail); got.Claim == nil || got.Claim.Actor.ID != "session-one" {
		t.Fatalf("the issue detail carries the claim: %+v", got.Claim)
	}

	list := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST", nil, "alice")
	if list.Code != http.StatusOK {
		t.Fatalf("list issues: status=%d body=%s", list.Code, list.Body.String())
	}
	rows := decodeBody[[]claimedIssue](t, list)
	if len(rows) != 1 || rows[0].Claim == nil || rows[0].Claim.Actor.ID != "session-one" {
		t.Fatalf("a list row carries the claim: %+v", rows)
	}
	if rows[0].Claim.Actor.Origin == nil || rows[0].Claim.Actor.Origin.SessionTitle != "Implementer" {
		t.Fatalf("a list row can label the holder: %+v", rows[0].Claim)
	}
}
