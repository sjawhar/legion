package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type askFollowersRead struct {
	Followers []model.AskFollower `json:"followers"`
}

func followerSessions(followers []model.AskFollower) []string {
	ids := make([]string, 0, len(followers))
	for _, follower := range followers {
		ids = append(ids, follower.SessionID)
	}
	return ids
}

func readAskFollowers(t *testing.T, handler http.Handler, askID string) []string {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("get ask: status=%d body=%s", response.Code, response.Body.String())
	}
	return followerSessions(decodeBody[askFollowersRead](t, response).Followers)
}

func openAskAs(t *testing.T, handler http.Handler, issueKey, sessionID, question string) string {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": question,
		"options":  []map[string]string{{"label": "Yes"}, {"label": "No"}},
		"actor":    map[string]any{"kind": "session", "id": sessionID},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		ID string `json:"id"`
	}](t, response).ID
}

func replyToAskAs(t *testing.T, handler http.Handler, issueKey, askID, sessionID, body string) {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", map[string]any{
		"body":   body,
		"ask_id": askID,
		"actor":  map[string]any{"kind": "session", "id": sessionID},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("reply to ask: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEveryoneWhoWritesToAnAskFollowsIt(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Followers", "A spec")
	askID := openAskAs(t, handler, issue.Key, "s1", "Ship it?")
	if got := readAskFollowers(t, handler, askID); strings.Join(got, ",") != "s1" {
		t.Fatalf("followers after open = %v, want [s1]", got)
	}

	replyToAskAs(t, handler, issue.Key, askID, "s2", "I would hold.")
	if got := readAskFollowers(t, handler, askID); strings.Join(got, ",") != "s1,s2" {
		t.Fatalf("followers after s2 reply = %v, want [s1 s2]", got)
	}

	human := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Hold for now.", "ask_id": askID,
	}, "alice")
	if human.Code != http.StatusCreated {
		t.Fatalf("human reply: status=%d body=%s", human.Code, human.Body.String())
	}
	replyToAskAs(t, handler, issue.Key, askID, "s2", "Agreed.")
	if got := readAskFollowers(t, handler, askID); strings.Join(got, ",") != "s1,s2" {
		t.Fatalf("followers after human and repeat replies = %v, want [s1 s2]", got)
	}

	listed := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID+"/followers", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list followers: status=%d body=%s", listed.Code, listed.Body.String())
	}
	if got := followerSessions(decodeBody[askFollowersRead](t, listed).Followers); strings.Join(got, ",") != "s1,s2" {
		t.Fatalf("listed followers = %v, want [s1 s2]", got)
	}
}

func TestApprovalAskFollowsItsRequestingSession(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Approval followers", "A spec")
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s-architect"},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", response.Code, response.Body.String())
	}
	askID := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, response).Ask.ID
	if got := readAskFollowers(t, handler, askID); strings.Join(got, ",") != "s-architect" {
		t.Fatalf("approval ask followers = %v, want [s-architect]", got)
	}
}

func TestAskBlockWrittenBySessionFollowsThatSession(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Ask block followers", "Context\n")
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s-writer"},
		"ops": []map[string]string{{
			"after":    "end",
			"markdown": ":::ask{#block-ask urgency=\"high\" multiple=\"false\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait\n:::\n",
			"op":       "insert",
		}},
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("write ask block: status=%d body=%s", edited.Code, edited.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
		if inbox.Code != http.StatusOK {
			t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
		}
		for _, ask := range decodeBody[[]model.Ask](t, inbox) {
			if ask.BlockID == nil || *ask.BlockID != "block-ask" {
				continue
			}
			if got := readAskFollowers(t, handler, ask.ID); strings.Join(got, ",") != "s-writer" {
				t.Fatalf("ask block followers = %v, want [s-writer]", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("typed ask block did not settle into the inbox")
}

func followerRequest(t *testing.T, handler http.Handler, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		request := httptest.NewRequest(method, target, nil)
		request.Header.Set("Authorization", "Bearer agent-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	return sessionRequest(t, handler, method, target, body)
}

func TestASessionFollowsAndUnfollowsOnlyItself(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Self follow", "A spec")
	askID := openAskAs(t, handler, issue.Key, "s1", "Ship it?")
	replyToAskAs(t, handler, issue.Key, askID, "s2", "Hold.")

	forbidden := followerRequest(t, handler, http.MethodDelete, "/api/v1/asks/"+askID+"/followers/s2", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if forbidden.Code != http.StatusForbidden || !strings.Contains(forbidden.Body.String(), `"code":"FOLLOWER_FORBIDDEN"`) {
		t.Fatalf("s1 removing s2: status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}
	noBody := followerRequest(t, handler, http.MethodDelete, "/api/v1/asks/"+askID+"/followers/s1", nil)
	if noBody.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("bearer without a body: status=%d body=%s", noBody.Code, noBody.Body.String())
	}

	removed := followerRequest(t, handler, http.MethodDelete, "/api/v1/asks/"+askID+"/followers/s1", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if removed.Code != http.StatusNoContent {
		t.Fatalf("s1 unfollowing: status=%d body=%s", removed.Code, removed.Body.String())
	}
	if got := readAskFollowers(t, handler, askID); strings.Join(got, ",") != "s2" {
		t.Fatalf("followers after s1 left = %v, want [s2]", got)
	}
	missing := followerRequest(t, handler, http.MethodDelete, "/api/v1/asks/"+askID+"/followers/s1", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"FOLLOWER_NOT_FOUND"`) {
		t.Fatalf("unfollowing twice: status=%d body=%s", missing.Code, missing.Body.String())
	}

	added := followerRequest(t, handler, http.MethodPut, "/api/v1/asks/"+askID+"/followers/s1", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if added.Code != http.StatusNoContent {
		t.Fatalf("s1 following again: status=%d body=%s", added.Code, added.Body.String())
	}
	if got := readAskFollowers(t, handler, askID); strings.Join(got, ",") != "s2,s1" {
		t.Fatalf("followers after s1 rejoined = %v, want [s2 s1]", got)
	}
	again := followerRequest(t, handler, http.MethodPut, "/api/v1/asks/"+askID+"/followers/s1", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if again.Code != http.StatusNoContent {
		t.Fatalf("following twice: status=%d body=%s", again.Code, again.Body.String())
	}
	if got := readAskFollowers(t, handler, askID); strings.Join(got, ",") != "s2,s1" {
		t.Fatalf("followers after a repeat follow = %v, want [s2 s1]", got)
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list events: status=%d body=%s", events.Code, events.Body.String())
	}
	var followerEvents []string
	for _, event := range decodeBody[[]struct {
		Type    string `json:"type"`
		Actor   model.Actor
		Payload struct {
			AskID     string      `json:"ask_id"`
			SessionID string      `json:"session_id"`
			By        model.Actor `json:"by"`
		} `json:"payload"`
	}](t, events) {
		if !strings.HasPrefix(event.Type, "ask.follower_") {
			continue
		}
		if event.Payload.AskID != askID || event.Payload.By.ID != "s1" || event.Actor.ID != "s1" {
			t.Fatalf("follower event = %#v", event)
		}
		followerEvents = append(followerEvents, event.Type+":"+event.Payload.SessionID)
	}
	if strings.Join(followerEvents, " ") != "ask.follower_removed:s1 ask.follower_added:s1" {
		t.Fatalf("follower events = %v", followerEvents)
	}
}

func TestAHumanAddsAndRemovesAnyFollower(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Human follow", "A spec")
	askID := openAskAs(t, handler, issue.Key, "s1", "Ship it?")
	replyToAskAs(t, handler, issue.Key, askID, "s2", "Hold.")

	request := httptest.NewRequest(http.MethodDelete, "/api/v1/asks/"+askID+"/followers/s2", nil)
	request.Header.Set("X-Dispatch-User", "alice")
	removed := httptest.NewRecorder()
	handler.ServeHTTP(removed, request)
	if removed.Code != http.StatusNoContent {
		t.Fatalf("human removing s2 without a body: status=%d body=%s", removed.Code, removed.Body.String())
	}
	if got := readAskFollowers(t, handler, askID); strings.Join(got, ",") != "s1" {
		t.Fatalf("followers after human removed s2 = %v, want [s1]", got)
	}

	request = httptest.NewRequest(http.MethodPut, "/api/v1/asks/"+askID+"/followers/s3", nil)
	request.Header.Set("X-Dispatch-User", "alice")
	added := httptest.NewRecorder()
	handler.ServeHTTP(added, request)
	if added.Code != http.StatusNoContent {
		t.Fatalf("human adding s3: status=%d body=%s", added.Code, added.Body.String())
	}
	if got := readAskFollowers(t, handler, askID); strings.Join(got, ",") != "s1,s3" {
		t.Fatalf("followers after human added s3 = %v, want [s1 s3]", got)
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	var seen []string
	for _, event := range decodeBody[[]struct {
		Type    string `json:"type"`
		Notify  bool   `json:"notify"`
		Payload struct {
			SessionID string      `json:"session_id"`
			By        model.Actor `json:"by"`
		} `json:"payload"`
	}](t, events) {
		if !strings.HasPrefix(event.Type, "ask.follower_") {
			continue
		}
		if event.Payload.By.Kind != "user" || event.Payload.By.ID != "alice" || !event.Notify {
			t.Fatalf("human follower event = %#v", event)
		}
		seen = append(seen, event.Type+":"+event.Payload.SessionID)
	}
	if strings.Join(seen, " ") != "ask.follower_removed:s2 ask.follower_added:s3" {
		t.Fatalf("follower events = %v", seen)
	}

	unknown := httptest.NewRequest(http.MethodDelete, "/api/v1/asks/"+askID+"/followers/nobody", nil)
	unknown.Header.Set("X-Dispatch-User", "alice")
	notFound := httptest.NewRecorder()
	handler.ServeHTTP(notFound, unknown)
	if notFound.Code != http.StatusNotFound || !strings.Contains(notFound.Body.String(), `"code":"FOLLOWER_NOT_FOUND"`) {
		t.Fatalf("human removing a non-follower: status=%d body=%s", notFound.Code, notFound.Body.String())
	}
	badAsk := httptest.NewRequest(http.MethodGet, "/api/v1/asks/not-a-uuid/followers", nil)
	badAsk.Header.Set("X-Dispatch-User", "alice")
	badID := httptest.NewRecorder()
	handler.ServeHTTP(badID, badAsk)
	if badID.Code != http.StatusBadRequest || !strings.Contains(badID.Body.String(), `"code":"ASK_ID_INPUT"`) {
		t.Fatalf("followers of a malformed ask id: status=%d body=%s", badID.Code, badID.Body.String())
	}
}
