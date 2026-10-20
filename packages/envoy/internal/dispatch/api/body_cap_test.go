package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A comment or message body is held to its writer's cap: a person's is contracts.HumanBodyMax
// UTF-16 units, and an agent's session's contracts.DispatchBodyMax, the cap the Dispatch tools
// hold their callers to. A refusal names the writer's own cap, so one trim lands.
func TestCommentAndMessageBodiesAreCappedByWhoWritesThem(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "BODY", "Body caps", "before")
	overBy := func(limit int) string {
		return fmt.Sprintf("body is 1 characters over the %d-character limit (%d/%d)", limit, limit+1, limit)
	}
	for _, kind := range []string{"comments", "messages"} {
		target := "/api/v1/issues/" + issue.Key + "/" + kind

		person := dispatchRequest(t, handler, http.MethodPost, target, map[string]string{
			"body": strings.Repeat("a", contracts.HumanBodyMax),
		}, "alice")
		if person.Code != http.StatusCreated {
			t.Fatalf("%s: a person's body at the cap: status=%d body=%.300s", kind, person.Code, person.Body.String())
		}
		personOver := dispatchRequest(t, handler, http.MethodPost, target, map[string]string{
			"body": strings.Repeat("b", contracts.HumanBodyMax+1),
		}, "alice")
		if personOver.Code != http.StatusBadRequest || !strings.Contains(personOver.Body.String(), `"code":"CAP_EXCEEDED"`) ||
			!strings.Contains(personOver.Body.String(), overBy(contracts.HumanBodyMax)) {
			t.Fatalf("%s: a person's body over the cap: status=%d body=%.300s", kind, personOver.Code, personOver.Body.String())
		}

		agent := sessionRequest(t, handler, http.MethodPost, target, map[string]any{
			"body": strings.Repeat("c", contracts.DispatchBodyMax), "actor": sessionActor(),
		})
		if agent.Code != http.StatusCreated {
			t.Fatalf("%s: an agent's body at its cap: status=%d body=%.300s", kind, agent.Code, agent.Body.String())
		}
		agentOver := sessionRequest(t, handler, http.MethodPost, target, map[string]any{
			"body": strings.Repeat("d", contracts.DispatchBodyMax+1), "actor": sessionActor(),
		})
		if agentOver.Code != http.StatusBadRequest || !strings.Contains(agentOver.Body.String(), `"code":"CAP_EXCEEDED"`) ||
			!strings.Contains(agentOver.Body.String(), overBy(contracts.DispatchBodyMax)) {
			t.Fatalf("%s: an agent's body over its cap: status=%d body=%.300s", kind, agentOver.Code, agentOver.Body.String())
		}
	}
}

// The routes only a person writes take a person's cap: a broadcast and a direct message to a
// session from the Agents page past the agent's 2,000 are stored, and reach the listener, whole.
func TestAPersonsBroadcastAndDirectMessageTakeThePersonsCap(t *testing.T) {
	listener, sends := newBroadcastListener(t, broadcastSessions)
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	body := strings.Repeat("e", contracts.HumanBodyMax)

	created := postBroadcast(t, handler, map[string]any{
		"body": body, "delivery": "steer", "session_ids": []string{"planner"},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("broadcast at the cap: status=%d body=%.300s", created.Code, created.Body.String())
	}
	if delivered := awaitBroadcastDeliveries(t, handler, decodeBody[broadcastResponse](t, created).ID); delivered.Recipients[0].Message.Body != body {
		t.Fatalf("stored broadcast message is %d bytes, want the %d-unit body", len(delivered.Recipients[0].Message.Body), contracts.HumanBodyMax)
	}
	direct := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/planner/messages", map[string]any{
		"body": body, "delivery": "steer",
	}, "alice")
	if direct.Code != http.StatusCreated {
		t.Fatalf("direct message at the cap: status=%d body=%.300s", direct.Code, direct.Body.String())
	}
	over := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/planner/messages", map[string]any{
		"body": body + "e", "delivery": "steer",
	}, "alice")
	if over.Code != http.StatusBadRequest || !strings.Contains(over.Body.String(), `"code":"CAP_EXCEEDED"`) {
		t.Fatalf("direct message past the cap: status=%d body=%.300s", over.Code, over.Body.String())
	}
	sends.Lock()
	defer sends.Unlock()
	for _, send := range sends.sends {
		if send.body != body {
			t.Fatalf("wire body to %s is %d bytes, want the %d-unit body", send.target, len(send.body), contracts.HumanBodyMax)
		}
	}
	if len(sends.sends) != 2 {
		t.Fatalf("listener sends = %d, want the broadcast's and the direct message's", len(sends.sends))
	}
}

// A person's anchored comment at the cap, every unit a three-byte character, is 192 KiB of
// UTF-8: inside the 256 KiB a document's margin record holds (docs maxMarginRecordBytes), so a
// comment at the cap is never refused for its own size there, and an edit to the cap passes the
// same way. A thread's replies share that record, and every anchored comment a document has had
// its 1 MiB margin, so those can still be refused (413 CAP_EXCEEDED) past their bounds. A character
// outside the Basic Multilingual Plane counts two units, as the dashboard's maxLength counts it,
// so a body of them reaches the cap at half as many characters.
func TestAPersonsAnchoredCommentAtTheCapFitsItsMarginRecord(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "MARGIN", "Margin record", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": strings.Repeat("語", contracts.HumanBodyMax), "anchor": map[string]any{"artifact": "spec", "quote": "before"},
	}, "alice")

	edited := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]string{
		"body": strings.Repeat("😀", contracts.HumanBodyMax/2),
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("edit to the cap: status=%d body=%.300s", edited.Code, edited.Body.String())
	}
	if updated := decodeBody[model.Comment](t, edited); updated.Body != strings.Repeat("😀", contracts.HumanBodyMax/2) {
		t.Fatalf("edited body is %d bytes, want the %d-unit edit", len(updated.Body), contracts.HumanBodyMax)
	}
	over := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]string{
		"body": strings.Repeat("😀", contracts.HumanBodyMax/2) + "a",
	}, "alice")
	if over.Code != http.StatusBadRequest || !strings.Contains(over.Body.String(), `"code":"CAP_EXCEEDED"`) {
		t.Fatalf("edit one unit past the cap: status=%d body=%.300s", over.Code, over.Body.String())
	}
}
