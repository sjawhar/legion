package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type turnAskRow struct {
	ID        string              `json:"id"`
	Question  string              `json:"question"`
	WaitingOn string              `json:"waiting_on"`
	LastReply *model.AskLastReply `json:"last_reply"`
}

func openTurnAsk(t *testing.T, handler http.Handler, issueKey, question string) string {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": question, "actor": sessionActor(),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create ask %q: status=%d body=%s", question, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		ID string `json:"id"`
	}](t, response).ID
}

func readInboxRow(t *testing.T, handler http.Handler, askID string) turnAskRow {
	t.Helper()
	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	for _, row := range decodeBody[[]turnAskRow](t, inbox) {
		if row.ID == askID {
			return row
		}
	}
	t.Fatalf("inbox has no row for ask %s", askID)
	return turnAskRow{}
}

func readAskDetail(t *testing.T, handler http.Handler, askID string) turnAskRow {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("read ask: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Ask turnAskRow `json:"ask"`
	}](t, response).Ask
}

func latestCommentCreatedPayload(t *testing.T, handler http.Handler, issueKey string) map[string]any {
	t.Helper()
	log := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/events", nil, "alice")
	if log.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", log.Code, log.Body.String())
	}
	events := decodeBody[[]struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}](t, log)
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != "comment.created" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(events[index].Payload, &payload); err != nil {
			t.Fatalf("decode comment.created payload: %v", err)
		}
		return payload
	}
	t.Fatalf("no comment.created event on %s", issueKey)
	return nil
}

// An open ask waits on the human until someone replies; a session's plain reply
// keeps it that way, a session's progress note (turn agent) keeps the turn with
// the agent, and a human's reply always hands the turn to the agent, whatever
// turn the request claims.
func TestAskReplyTurnDecidesWhoTheAskWaitsOn(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Turns", "spec")
	askID := openTurnAsk(t, handler, issue.Key, "Which auditor?")

	fresh := readInboxRow(t, handler, askID)
	if fresh.WaitingOn != "human" || fresh.LastReply != nil {
		t.Fatalf("unreplied inbox row = %#v, want waiting_on human with no last_reply", fresh)
	}

	human := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Which auditors are available?", "ask_id": askID, "turn": "human",
	}, "alice")
	if human.Code != http.StatusCreated {
		t.Fatalf("human reply: status=%d body=%s", human.Code, human.Body.String())
	}
	if comment := decodeBody[model.Comment](t, human); comment.Turn == nil || *comment.Turn != "agent" {
		t.Fatalf("human reply turn = %#v, want agent regardless of the requested turn", comment.Turn)
	}
	if row := readInboxRow(t, handler, askID); row.WaitingOn != "agent" || row.LastReply == nil || row.LastReply.Author.Kind != "user" {
		t.Fatalf("inbox row after human reply = %#v, want waiting_on agent", row)
	}
	if payload := latestCommentCreatedPayload(t, handler, issue.Key); payload["ask_waiting_on"] != "agent" {
		t.Fatalf("comment.created after human reply = %#v, want ask_waiting_on agent", payload)
	}

	progress := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Dispatched two auditors, back with results.", "ask_id": askID, "turn": "agent", "actor": sessionActor(),
	})
	if progress.Code != http.StatusCreated {
		t.Fatalf("progress note: status=%d body=%s", progress.Code, progress.Body.String())
	}
	if comment := decodeBody[model.Comment](t, progress); comment.Turn == nil || *comment.Turn != "agent" {
		t.Fatalf("progress note turn = %#v, want agent", comment.Turn)
	}
	if row := readInboxRow(t, handler, askID); row.WaitingOn != "agent" || row.LastReply == nil || row.LastReply.Author.Kind != "session" {
		t.Fatalf("inbox row after progress note = %#v, want waiting_on agent with the session's last_reply", row)
	}
	if detail := readAskDetail(t, handler, askID); detail.WaitingOn != "agent" {
		t.Fatalf("ask detail after progress note = %#v, want waiting_on agent", detail)
	}
	if payload := latestCommentCreatedPayload(t, handler, issue.Key); payload["ask_waiting_on"] != "agent" || payload["turn"] != "agent" {
		t.Fatalf("comment.created after progress note = %#v, want ask_waiting_on and turn agent", payload)
	}

	plain := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Both auditors pick the first option; your call.", "ask_id": askID, "actor": sessionActor(),
	})
	if plain.Code != http.StatusCreated {
		t.Fatalf("plain reply: status=%d body=%s", plain.Code, plain.Body.String())
	}
	if comment := decodeBody[model.Comment](t, plain); comment.Turn == nil || *comment.Turn != "human" {
		t.Fatalf("plain session reply turn = %#v, want human by default", comment.Turn)
	}
	if row := readInboxRow(t, handler, askID); row.WaitingOn != "human" {
		t.Fatalf("inbox row after plain reply = %#v, want waiting_on human", row)
	}
	if payload := latestCommentCreatedPayload(t, handler, issue.Key); payload["ask_waiting_on"] != "human" {
		t.Fatalf("comment.created after plain reply = %#v, want ask_waiting_on human", payload)
	}

	asks := sessionRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?state=open", nil)
	if asks.Code != http.StatusOK {
		t.Fatalf("list issue asks: status=%d body=%s", asks.Code, asks.Body.String())
	}
	if listed := decodeBody[[]turnAskRow](t, asks); len(listed) != 1 || listed[0].WaitingOn != "human" {
		t.Fatalf("issue asks = %#v, want the one open ask waiting on human", listed)
	}
	detail := sessionRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil)
	if detail.Code != http.StatusOK {
		t.Fatalf("read issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	if open := decodeBody[struct {
		OpenAsks []turnAskRow `json:"open_asks"`
	}](t, detail).OpenAsks; len(open) != 1 || open[0].WaitingOn != "human" || open[0].LastReply == nil || open[0].LastReply.Author.Kind != "session" {
		t.Fatalf("issue open_asks = %#v, want waiting_on human beside the session's last_reply", open)
	}
}

// The inbox groups asks by whose turn it is, not by who replied last: a progress
// note keeps its ask with the human-replied ones under "waiting on agents".
func TestInboxGroupsProgressNotedAskWithThoseWaitingOnAgents(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Inbox turns", "spec")
	noted := openTurnAsk(t, handler, issue.Key, "Noted")
	fresh := openTurnAsk(t, handler, issue.Key, "Fresh")
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Working on it.", "ask_id": noted, "turn": "agent", "actor": sessionActor(),
	}); response.Code != http.StatusCreated {
		t.Fatalf("progress note: status=%d body=%s", response.Code, response.Body.String())
	}
	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	rows := decodeBody[[]turnAskRow](t, inbox)
	if len(rows) != 2 || rows[0].ID != fresh || rows[0].WaitingOn != "human" || rows[1].ID != noted || rows[1].WaitingOn != "agent" {
		t.Fatalf("inbox rows = %#v, want the fresh ask (waiting on human) before the noted one (waiting on agent)", rows)
	}
}

func TestCommentTurnRequiresAnAskReply(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Turn validation", "spec")
	root := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "A root comment.", "turn": "agent", "actor": sessionActor(),
	})
	if root.Code != http.StatusBadRequest || !strings.Contains(root.Body.String(), `"code":"TURN_REQUIRES_ASK"`) {
		t.Fatalf("turn on a root comment: status=%d body=%s, want 400 TURN_REQUIRES_ASK", root.Code, root.Body.String())
	}
	askID := openTurnAsk(t, handler, issue.Key, "Which one?")
	invalid := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Soon.", "ask_id": askID, "turn": "nobody", "actor": sessionActor(),
	})
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"INVALID_COMMENT"`) {
		t.Fatalf("invalid turn value: status=%d body=%s, want 400 INVALID_COMMENT", invalid.Code, invalid.Body.String())
	}
	plain := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Not an ask reply.", "actor": sessionActor(),
	})
	if plain.Code != http.StatusCreated {
		t.Fatalf("root comment: status=%d body=%s", plain.Code, plain.Body.String())
	}
	if comment := decodeBody[model.Comment](t, plain); comment.Turn != nil {
		t.Fatalf("root comment turn = %q, want null", *comment.Turn)
	}
}

// A closed ask has no turn to hold: a reply under an answered ask records no turn
// even when the request asks for one, and its comment.created carries no
// ask_waiting_on.
func TestReplyUnderAnAnsweredAskRecordsNoTurn(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Closed turns", "spec")
	askID := openTurnAsk(t, handler, issue.Key, "Ship it?")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"text": "Yes.",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", response.Code, response.Body.String())
	}
	reply := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Shipped in #42.", "ask_id": askID, "turn": "agent", "actor": sessionActor(),
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply under answered ask: status=%d body=%s", reply.Code, reply.Body.String())
	}
	if comment := decodeBody[model.Comment](t, reply); comment.Turn != nil {
		t.Fatalf("reply under answered ask turn = %q, want null", *comment.Turn)
	}
	payload := latestCommentCreatedPayload(t, handler, issue.Key)
	if _, present := payload["ask_waiting_on"]; present || payload["ask_state"] != "answered" || payload["turn"] != nil {
		t.Fatalf("comment.created under answered ask = %#v, want ask_state answered with no turn and no ask_waiting_on", payload)
	}
	if detail := readAskDetail(t, handler, askID); detail.WaitingOn != "" {
		t.Fatalf("answered ask detail waiting_on = %q, want none", detail.WaitingOn)
	}
}
