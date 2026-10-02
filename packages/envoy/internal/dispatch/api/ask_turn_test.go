package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type turnAskRow struct {
	ID        string              `json:"id"`
	Question  string              `json:"question"`
	WaitingOn string              `json:"waiting_on"`
	EditedAt  *string             `json:"edited_at"`
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

// Priority outranks whose turn it is: the inbox orders every row by the owning
// issue's priority (unset last) and only then puts the human-turn asks before the
// agent-turn ones, so a P0 ask an agent is still working on sits above a P2 ask
// nobody has answered.
func TestInboxOrdersByPriorityBeforeWhoseTurn(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	createIssue := func(title string, priority any) string {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "TEST", "title": title,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		key := decodeBody[struct {
			Key string `json:"key"`
		}](t, response).Key
		if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
			"priority": priority,
		}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("set %s priority %v: status=%d body=%s", key, priority, response.Code, response.Body.String())
		}
		return key
	}
	progressNote := func(issueKey, askID string) {
		t.Helper()
		if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", map[string]any{
			"body": "Working on it.", "ask_id": askID, "turn": "agent", "actor": sessionActor(),
		}); response.Code != http.StatusCreated {
			t.Fatalf("progress note on %s: status=%d body=%s", askID, response.Code, response.Body.String())
		}
	}

	// Opened newest-last so recency alone would list them in reverse.
	unsetIssue := createIssue("Unset issue", nil)
	openTurnAsk(t, handler, unsetIssue, "Unset, waiting on human")
	p2AgentIssue := createIssue("P2 agent issue", 2)
	p2Agent := openTurnAsk(t, handler, p2AgentIssue, "P2, waiting on agent")
	progressNote(p2AgentIssue, p2Agent)
	p2HumanIssue := createIssue("P2 human issue", 2)
	openTurnAsk(t, handler, p2HumanIssue, "P2, waiting on human")
	p0Issue := createIssue("P0 issue", 0)
	p0Agent := openTurnAsk(t, handler, p0Issue, "P0, waiting on agent")
	progressNote(p0Issue, p0Agent)

	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	rows := decodeBody[[]turnAskRow](t, inbox)
	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, row.Question+" ("+row.WaitingOn+")")
	}
	want := []string{
		"P0, waiting on agent (agent)",
		"P2, waiting on human (human)",
		"P2, waiting on agent (agent)",
		"Unset, waiting on human (human)",
	}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Fatalf("inbox order = %q, want %q", got, want)
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

// A comment's created_at is when its insert ran (migration 0065's default), after the reply took
// the owner row, so the thread's newest reply is the one that committed last. Here the agent's
// reply begins first and waits for the owner row while a human's reply, begun after it, commits;
// the agent's reply is the newer one, and the ask's turn, its Inbox row and its place in the Inbox
// follow it. The test holds the owner row and writes the human's reply directly, in a transaction
// that begins after the agent's: a reply that took the owner row itself would commit at that
// moment or later, with a created_at no later than its commit. Stamped at its transaction's start,
// the agent's reply would sort before the human's, and the ask would wait on the agent that had
// already answered.
func TestAReplyThatWaitedForTheOwnerRowIsTheNewest(t *testing.T) {
	for _, path := range []struct {
		name   string
		author string
		// open readies the agent's reply in askID's thread and returns the call that sends it.
		open func(t *testing.T, handler http.Handler, issueKey, askID string) func() *httptest.ResponseRecorder
	}{
		{
			name:   "a comment posted to the ask",
			author: sessionActor()["id"].(string),
			open: func(t *testing.T, handler http.Handler, issueKey, askID string) func() *httptest.ResponseRecorder {
				return func() *httptest.ResponseRecorder {
					return sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", map[string]any{
						"body": "The second approach.", "ask_id": askID, "actor": sessionActor(),
					})
				}
			},
		},
		{
			name:   "a mention's callback reply",
			author: "s1",
			open: func(t *testing.T, handler http.Handler, issueKey, askID string) func() *httptest.ResponseRecorder {
				clarification := decodeMentionedComment(t, postMentionedComment(t, handler, issueKey, map[string]any{
					"body": "Say more, @session:s1.", "ask_id": askID,
					"mentions": []map[string]any{{"target": "session:s1"}},
				}))
				return func() *httptest.ResponseRecorder {
					return bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+clarification.ID+"/reply", map[string]any{
						"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "The second approach.",
					})
				}
			},
		},
	} {
		t.Run(path.name, func(t *testing.T) {
			ctx := context.Background()
			live := true
			sent := []map[string]any{}
			listener := sessionListener(t, &live, &sent)
			defer listener.Close()
			handler, database := newTargetedMessageHandler(t, listener.URL)
			issue := createInteractionIssue(t, handler, "TEST", "Racing replies", "spec")
			askID := openTurnAsk(t, handler, issue.Key, "Which approach?")
			other := createInteractionIssue(t, handler, "OTHER", "Another thread", "spec")
			otherAskID := openTurnAsk(t, handler, other.Key, "Which region?")
			agentReply := path.open(t, handler, issue.Key, askID)

			hold, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the owner row hold: %v", err)
			}
			defer hold.Rollback(ctx)
			if _, err := hold.Exec(ctx, `select 1 from issues where key = $1 for no key update`, issue.Key); err != nil {
				t.Fatalf("hold the owner row: %v", err)
			}
			replied := make(chan *httptest.ResponseRecorder, 1)
			go func() { replied <- agentReply() }()
			waitForDatabaseLocks(t, hold, 1)
			var humanReplyID string
			if err := database.Pool.QueryRow(ctx, `
				insert into comments (issue_key, author, body, ask_id, turn)
				values ($1, '{"kind":"user","id":"alice"}', 'Can it ship today?', $2, 'agent')
				returning id::text
			`, issue.Key, askID).Scan(&humanReplyID); err != nil {
				t.Fatalf("commit the human's reply: %v", err)
			}
			// The other ask's newest reply commits after the human's and before the agent's.
			if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+other.Key+"/comments", map[string]any{
				"body": "Frankfurt.", "ask_id": otherAskID, "actor": sessionActor(),
			}); response.Code != http.StatusCreated {
				t.Fatalf("reply on the other ask: status=%d body=%s", response.Code, response.Body.String())
			}
			if err := hold.Rollback(ctx); err != nil {
				t.Fatalf("release the owner row: %v", err)
			}
			response := awaitResponse(t, replied)
			if response.Code != http.StatusCreated {
				t.Fatalf("agent's reply: status=%d body=%s", response.Code, response.Body.String())
			}
			reply := decodeBody[struct {
				model.Comment
				AskWaitingOn string `json:"ask_waiting_on"`
			}](t, response)
			if reply.AskWaitingOn != "human" {
				t.Fatalf("agent's reply ask_waiting_on = %q, want human", reply.AskWaitingOn)
			}
			events := decodeBody[[]struct {
				Type    string         `json:"type"`
				Payload map[string]any `json:"payload"`
			}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice"))
			recorded := false
			for _, event := range events {
				if event.Payload["id"] != reply.ID {
					continue
				}
				recorded = true
				if event.Payload["ask_waiting_on"] != "human" {
					t.Fatalf("%s ask_waiting_on = %#v, want human", event.Type, event.Payload["ask_waiting_on"])
				}
			}
			if !recorded {
				t.Fatalf("no event records the agent's reply %s", reply.ID)
			}

			detail := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil)
			if detail.Code != http.StatusOK {
				t.Fatalf("read ask: status=%d body=%s", detail.Code, detail.Body.String())
			}
			thread := decodeBody[struct {
				Ask     turnAskRow      `json:"ask"`
				Replies []model.Comment `json:"replies"`
			}](t, detail)
			if thread.Ask.WaitingOn != "human" {
				t.Fatalf("ask detail waiting_on = %q, want human", thread.Ask.WaitingOn)
			}
			if count := len(thread.Replies); count < 2 || thread.Replies[count-2].ID != humanReplyID || thread.Replies[count-1].ID != reply.ID {
				t.Fatalf("ask replies = %#v, want the human's reply %s then the agent's %s", thread.Replies, humanReplyID, reply.ID)
			}

			inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
			if inbox.Code != http.StatusOK {
				t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
			}
			rows := decodeBody[[]turnAskRow](t, inbox)
			if len(rows) != 2 || rows[0].ID != askID || rows[0].WaitingOn != "human" || rows[0].LastReply == nil ||
				rows[0].LastReply.Author.ID != path.author || rows[1].ID != otherAskID || rows[1].WaitingOn != "human" {
				t.Fatalf("inbox rows = %#v, want %s (waiting on human after %s's reply) above %s", rows, askID, path.author, otherAskID)
			}
		})
	}
}
