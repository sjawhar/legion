package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type answerHistoryRead struct {
	Ask     model.Ask         `json:"ask"`
	Answers []model.AskAnswer `json:"answers"`
}

type answerEventRead struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func createChangeableAsk(t *testing.T, handler http.Handler, issueKey string) model.Ask {
	t.Helper()
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": "Should we ship?",
		"options":  []map[string]string{{"label": "Ship"}, {"label": "Hold"}},
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create changeable ask: status=%d body=%s", created.Code, created.Body.String())
	}
	return decodeBody[model.Ask](t, created)
}

func answerAskForChange(t *testing.T, handler http.Handler, askID, login, selected string, expectedAnswerAt *string) model.Ask {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected":           []string{selected},
		"expected_answer_at": expectedAnswerAt,
	}, login)
	if response.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Ask](t, response)
}

func TestAnswerAskChangesTheCurrentAnswerAndRecordsHistory(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Change an answer", "A spec")
	ask := createChangeableAsk(t, handler, issue.Key)
	first := answerAskForChange(t, handler, ask.ID, "alice", "Ship", nil)
	if first.Answer == nil {
		t.Fatal("first answer is missing")
	}
	firstAt := timestampValue(first.Answer.At)

	changed := answerAskForChange(t, handler, ask.ID, "alice", "Hold", &firstAt)
	if changed.State != "answered" || changed.Answer == nil || len(changed.Answer.Selected) != 1 || changed.Answer.Selected[0] != "Hold" || !changed.Answer.At.After(first.Answer.At) {
		t.Fatalf("changed answer = %#v, want current Hold answer after the first", changed)
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read changed ask: status=%d body=%s", read.Code, read.Body.String())
	}
	history := decodeBody[answerHistoryRead](t, read)
	if len(history.Answers) != 2 || history.Answers[0].At != first.Answer.At || history.Answers[0].Selected[0] != "Ship" || history.Answers[1].At != changed.Answer.At || history.Answers[1].Selected[0] != "Hold" {
		t.Fatalf("answer history = %#v, want first Ship then changed Hold", history.Answers)
	}

	eventsResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if eventsResponse.Code != http.StatusOK {
		t.Fatalf("read issue events: status=%d body=%s", eventsResponse.Code, eventsResponse.Body.String())
	}
	events := decodeBody[[]answerEventRead](t, eventsResponse)
	answerEvents := make([]answerEventRead, 0, 2)
	for _, event := range events {
		if event.Type == "ask.answered" {
			answerEvents = append(answerEvents, event)
		}
	}
	if len(answerEvents) != 2 {
		t.Fatalf("ask.answered events = %#v, want first and changed answer", answerEvents)
	}
	var secondPayload struct {
		Answer         model.AskAnswer  `json:"answer"`
		PreviousAnswer *model.AskAnswer `json:"previous_answer"`
	}
	if err := json.Unmarshal(answerEvents[1].Payload, &secondPayload); err != nil {
		t.Fatalf("decode changed answer payload: %v", err)
	}
	if secondPayload.PreviousAnswer == nil || secondPayload.PreviousAnswer.At != first.Answer.At || secondPayload.PreviousAnswer.Selected[0] != "Ship" || secondPayload.Answer.Selected[0] != "Hold" {
		t.Fatalf("changed answer payload = %#v", secondPayload)
	}
}

func TestAnswerAskChangeUsesTheCurrentAnswerAsCompareAndSet(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Compare answer", "A spec")
	ask := createChangeableAsk(t, handler, issue.Key)
	first := answerAskForChange(t, handler, ask.ID, "alice", "Ship", nil)
	if first.Answer == nil {
		t.Fatal("first answer is missing")
	}
	stale := timestampValue(first.Answer.At)
	current := answerAskForChange(t, handler, ask.ID, "alice", "Hold", &stale)
	if current.Answer == nil {
		t.Fatal("current answer is missing")
	}
	currentAt := timestampValue(current.Answer.At)

	if staleChange := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected": []string{"Ship"}, "expected_answer_at": stale,
	}, "alice"); staleChange.Code != http.StatusConflict || !strings.Contains(staleChange.Body.String(), `"code":"ASK_ANSWER_CHANGED"`) {
		t.Fatalf("stale answer change: status=%d body=%s", staleChange.Code, staleChange.Body.String())
	}
	if otherAnswerer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected": []string{"Ship"}, "expected_answer_at": currentAt,
	}, "bob"); otherAnswerer.Code != http.StatusForbidden || !strings.Contains(otherAnswerer.Body.String(), `"code":"NOT_ANSWERER"`) {
		t.Fatalf("other answerer change: status=%d body=%s", otherAnswerer.Code, otherAnswerer.Body.String())
	}
	if absent := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected": []string{"Ship"},
	}, "alice"); absent.Code != http.StatusConflict || !strings.Contains(absent.Body.String(), `"code":"ASK_CLOSED"`) {
		t.Fatalf("answer without replacement timestamp: status=%d body=%s", absent.Code, absent.Body.String())
	}
}

func TestAnswerAskChangeRefusesClosedAndApprovalAsks(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Answer change refusals", "A spec")
	open := createChangeableAsk(t, handler, issue.Key)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+open.ID+"/answer", map[string]any{
		"selected": []string{"Ship"}, "expected_answer_at": time.Now().UTC().Format(time.RFC3339Nano),
	}, "alice"); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ASK_ANSWER_CHANGED"`) {
		t.Fatalf("replace open ask: status=%d body=%s", response.Code, response.Body.String())
	}
	resolved := createChangeableAsk(t, handler, issue.Key)
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+resolved.ID+"/resolve", map[string]any{
		"kind": "resolved", "reason": "No longer needed", "actor": sessionActor(),
	}); response.Code != http.StatusOK {
		t.Fatalf("resolve ask: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+resolved.ID+"/answer", map[string]any{
		"selected": []string{"Ship"}, "expected_answer_at": time.Now().UTC().Format(time.RFC3339Nano),
	}, "alice"); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ASK_RESOLVED"`) {
		t.Fatalf("replace resolved ask: status=%d body=%s", response.Code, response.Body.String())
	}

	requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	approval := decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, requested).Ask
	answered := answerAskForChange(t, handler, approval.ID, "alice", "Approve", nil)
	if answered.Answer == nil {
		t.Fatal("approval answer is missing")
	}
	approvalAt := timestampValue(answered.Answer.At)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+approval.ID+"/answer", map[string]any{
		"selected": []string{"Approve"}, "expected_answer_at": approvalAt,
	}, "alice"); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ASK_APPROVAL_REVIEW"`) {
		t.Fatalf("replace approval: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAnswerAskHistoryIgnoresARestorationAndKeepsTheLaterChange(t *testing.T) {
	handler, database := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Restored history", "decision", transportAsk, "Which transport?")
	before := readBlockAsk(t, handler, askID)
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected":           []string{"REST"},
		"expected_edited_at": before.EditedAt,
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer block ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if deleted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "delete", "block": "decision"}},
	}, "bob"); deleted.Code != http.StatusOK {
		t.Fatalf("delete answered block: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "history-after-delete")
	// Rows written before settlement left deleted answered blocks answered can still hold its
	// retraction, with the answer only in events.
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
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "history-after-restore")

	read := decodeBody[answerHistoryRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice"))
	if len(read.Answers) != 1 || read.Answers[0].User != "alice" || read.Answers[0].Selected[0] != "REST" {
		t.Fatalf("restored answer history = %#v, want Alice's one answer", read.Answers)
	}
	currentAt := timestampValue(read.Answers[0].At)
	changed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected":           []string{"gRPC"},
		"expected_edited_at": read.Ask.EditedAt,
		"expected_answer_at": currentAt,
	}, "alice")
	if changed.Code != http.StatusOK {
		t.Fatalf("change restored answer: status=%d body=%s", changed.Code, changed.Body.String())
	}
	history := decodeBody[answerHistoryRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice"))
	if len(history.Answers) != 2 || history.Answers[0].Selected[0] != "REST" || history.Answers[1].Selected[0] != "gRPC" {
		t.Fatalf("changed restored history = %#v, want REST then gRPC", history.Answers)
	}
}

func TestAnswerAskChangeStillRequiresTheReviewedQuestionRevision(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Question revision", "A spec")
	ask := createChangeableAsk(t, handler, issue.Key)
	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+ask.ID, map[string]any{
		"question": "Should we ship today?", "actor": sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit ask: status=%d body=%s", edited.Code, edited.Body.String())
	}
	current := decodeBody[model.Ask](t, edited)
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected": []string{"Ship"}, "expected_edited_at": current.EditedAt,
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer edited ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	first := decodeBody[model.Ask](t, answered)
	if first.Answer == nil {
		t.Fatal("answer is missing")
	}
	if stale := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected":           []string{"Hold"},
		"expected_edited_at": nil,
		"expected_answer_at": timestampValue(first.Answer.At),
	}, "alice"); stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), `"code":"ASK_EDITED"`) {
		t.Fatalf("change with stale question revision: status=%d body=%s", stale.Code, stale.Body.String())
	}
}
