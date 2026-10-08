package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type myAnswerRowRead struct {
	Kind     string           `json:"kind"`
	At       string           `json:"at"`
	AskID    string           `json:"ask_id"`
	Ref      string           `json:"ref"`
	Question string           `json:"question"`
	AskKind  string           `json:"ask_kind"`
	AskState string           `json:"ask_state"`
	EditedAt *string          `json:"edited_at"`
	Owner    openAskOwner     `json:"owner"`
	Answer   *model.AskAnswer `json:"answer"`
	Current  bool             `json:"current"`
	Reply    *struct {
		ID   string `json:"id"`
		Body string `json:"body"`
	} `json:"reply"`
}

type myAnswersRead struct {
	Rows   []myAnswerRowRead `json:"rows"`
	Total  int               `json:"total"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

func createAnswerListAsk(t *testing.T, handler http.Handler, issueKey, question string) model.Ask {
	t.Helper()
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": question,
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask %q: status=%d body=%s", question, created.Code, created.Body.String())
	}
	return decodeBody[model.Ask](t, created)
}

func createAnswerListIssue(t *testing.T, handler http.Handler, title string) model.Issue {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST",
		"title":   title,
		"spec":    "A spec",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue %q: status=%d body=%s", title, created.Code, created.Body.String())
	}
	return decodeBody[model.Issue](t, created)
}

func getMyAnswers(t *testing.T, handler http.Handler, login, query string) myAnswersRead {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/answers"+query, nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("list my answers: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[myAnswersRead](t, response)
}

func TestListMyAnswersReturnsOwnAnswersAndRepliesNewestFirst(t *testing.T) {
	handler := newTestHandler(t)
	firstIssue := createInteractionIssue(t, handler, "TEST", "First answer", "A spec")
	secondIssue := createAnswerListIssue(t, handler, "Second answer")
	thirdIssue := createAnswerListIssue(t, handler, "Reply only")
	firstAsk := createAnswerListAsk(t, handler, firstIssue.Key, "First question")
	secondAsk := createAnswerListAsk(t, handler, secondIssue.Key, "Second question")
	thirdAsk := createAnswerListAsk(t, handler, thirdIssue.Key, "Reply question")

	for _, answer := range []struct {
		ask   model.Ask
		login string
		text  string
	}{
		{firstAsk, "alice", "First answer."},
		{secondAsk, "alice", "Second answer."},
		{createAnswerListAsk(t, handler, thirdIssue.Key, "Bob question"), "bob", "Bob answer."},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+answer.ask.ID+"/answer", map[string]any{
			"text": answer.text,
		}, answer.login)
		if response.Code != http.StatusOK {
			t.Fatalf("answer %q: status=%d body=%s", answer.ask.Question, response.Code, response.Body.String())
		}
	}
	replied := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+thirdIssue.Key+"/comments", map[string]string{
		"ask_id": thirdAsk.ID,
		"body":   "Alice's reply.",
	}, "alice")
	if replied.Code != http.StatusCreated {
		t.Fatalf("write ask reply: status=%d body=%s", replied.Code, replied.Body.String())
	}

	answers := getMyAnswers(t, handler, "alice", "")
	if answers.Total != 3 || len(answers.Rows) != 3 || answers.Limit != 50 || answers.Offset != 0 {
		t.Fatalf("answer page = %#v, want three default-page rows", answers)
	}
	if got := []string{answers.Rows[0].Kind, answers.Rows[1].AskID, answers.Rows[2].AskID}; strings.Join(got, ",") != "reply,"+secondAsk.ID+","+firstAsk.ID {
		t.Fatalf("answer order = %v, want reply, second answer, first answer", got)
	}
	if reply := answers.Rows[0].Reply; reply == nil || reply.Body != "Alice's reply." {
		t.Fatalf("reply row = %#v", answers.Rows[0])
	}
	for _, row := range answers.Rows[1:] {
		if row.Kind != "answer" || row.Answer == nil || !row.Current || row.Answer.User != "alice" {
			t.Fatalf("answer row = %#v, want Alice current answer", row)
		}
		if row.Owner.Issue == nil || row.Ref != "/issues/"+row.Owner.Issue.Key+"?ask="+row.AskID {
			t.Fatalf("answer owner/ref = %#v", row)
		}
	}
	for _, row := range answers.Rows {
		if row.Question == "Bob question" {
			t.Fatalf("Bob's answer leaked into Alice's page: %#v", row)
		}
	}

	page := getMyAnswers(t, handler, "alice", "?limit=1&offset=1")
	if page.Total != 3 || page.Limit != 1 || page.Offset != 1 || len(page.Rows) != 1 || page.Rows[0].AskID != secondAsk.ID {
		t.Fatalf("answer page at offset one = %#v", page)
	}
	if invalid := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/answers?limit=0", nil, "alice"); invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"INVALID_QUERY"`) {
		t.Fatalf("invalid answer page: status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	if bearer := sessionRequest(t, handler, http.MethodGet, "/api/v1/me/answers", nil); bearer.Code != http.StatusForbidden || !strings.Contains(bearer.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("agent answer page: status=%d body=%s", bearer.Code, bearer.Body.String())
	}
}

func TestListMyAnswersIncludesDocumentAnswers(t *testing.T) {
	handler := newTestHandler(t)
	if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key":  "TEST",
		"name": "Test project",
	}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", created.Code, created.Body.String())
	}
	document := createProjectDocument(t, handler, "TEST", "Runbook", "# Runbook\n")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks", map[string]any{
		"question": "Document question",
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create document ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[model.Ask](t, created)
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]string{"text": "Document answer."}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer document ask: status=%d body=%s", answered.Code, answered.Body.String())
	}

	answers := getMyAnswers(t, handler, "alice", "")
	if len(answers.Rows) != 1 {
		t.Fatalf("document answers = %#v", answers)
	}
	row := answers.Rows[0]
	if row.Owner.Document == nil || row.Owner.Document.Project != "TEST" || row.Owner.Document.Slug != document.Slug || row.Ref != "/projects/TEST/documents/"+document.Slug+"?ask="+ask.ID {
		t.Fatalf("document answer owner/ref = %#v", row)
	}
}

func TestListMyAnswersCollapsesRestoredAnswerEvents(t *testing.T) {
	handler, database := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Restored answer", "decision", transportAsk, "Which transport?")
	before := readBlockAsk(t, handler, askID)
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected":           []string{"REST"},
		"expected_edited_at": before.EditedAt,
	}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer block ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if deleted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "delete", "block": "decision"}},
	}, "bob"); deleted.Code != http.StatusOK {
		t.Fatalf("delete answered block: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-delete")
	// Current settlement leaves an answered row when its block is deleted. Rows written before that
	// rule can still hold settlement's retraction while their answer survives only in events.
	if _, err := database.Pool.Exec(context.Background(), `
		update asks
		set state = 'resolved', answer = null,
		    resolution = jsonb_build_object(
		      'kind', 'retracted',
		      'reason', 'removed from the document in version 3',
		      'actor', jsonb_build_object('kind', 'system', 'id', 'document-settlement'),
		      'at', now()
		    )
		where id = $1
	`, askID); err != nil {
		t.Fatalf("seed legacy settlement retraction: %v", err)
	}
	if restored := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": transportAsk}},
	}, "bob"); restored.Code != http.StatusOK {
		t.Fatalf("restore answered block: status=%d body=%s", restored.Code, restored.Body.String())
	}
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-restore")

	answeredEvents := 0
	for _, event := range issueAskEvents(t, handler, issue.Key) {
		if event.Type == "ask.answered" && event.Payload.ID == askID {
			answeredEvents++
		}
	}
	if answeredEvents != 2 {
		t.Fatalf("ask.answered events = %d, want initial answer and restoration", answeredEvents)
	}
	if bob := getMyAnswers(t, handler, "bob", ""); len(bob.Rows) != 0 {
		t.Fatalf("restorer got an answer row: %#v", bob.Rows)
	}
	alice := getMyAnswers(t, handler, "alice", "")
	if len(alice.Rows) != 1 || alice.Rows[0].AskID != askID || !alice.Rows[0].Current {
		t.Fatalf("restored answer rows = %#v, want Alice's one current answer", alice.Rows)
	}
}
