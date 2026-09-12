package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func createActionAsk(t *testing.T, handler http.Handler, issueKey string) model.Ask {
	t.Helper()
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": "Confirm the release is deployed.",
		"kind":     "action",
		"options":  []map[string]string{{"label": "Ship"}},
		"multiple": true,
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create action ask: status=%d body=%s", created.Code, created.Body.String())
	}
	return decodeBody[model.Ask](t, created)
}

func TestActionAskUsesFixedOptions(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Action ask", "A spec")

	ask := createActionAsk(t, handler, issue.Key)
	if ask.Kind != "action" || ask.Multiple || len(ask.Options) != 2 || ask.Options[0].Label != "Done" || ask.Options[1].Label != "Can't" {
		t.Fatalf("action ask = %#v, want action kind with fixed Done / Can't options", ask)
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), `"type":"ask.opened"`) || !strings.Contains(events.Body.String(), `"kind":"action"`) {
		t.Fatalf("action ask opening event: status=%d body=%s", events.Code, events.Body.String())
	}

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+ask.ID, map[string]any{
		"question": "Confirm the release has been deployed.",
		"actor":    sessionActor(),
	})
	if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"question":"Confirm the release has been deployed."`) {
		t.Fatalf("edit action wording: status=%d body=%s", edited.Code, edited.Body.String())
	}
}

func TestActionAskCantRequiresText(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Action cannot", "A spec")
	ask := createActionAsk(t, handler, issue.Key)

	withoutReason := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected": []string{"Can't"},
	}, "alice")
	if withoutReason.Code != http.StatusBadRequest || !strings.Contains(withoutReason.Body.String(), `"code":"INVALID_ANSWER"`) {
		t.Fatalf("answer action Can't without text: status=%d body=%s", withoutReason.Code, withoutReason.Body.String())
	}

	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected": []string{"Can't"},
		"text":     "The deployment permission is missing.",
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer action Can't with text: status=%d body=%s", answered.Code, answered.Body.String())
	}
	result := decodeBody[model.Ask](t, answered)
	if result.State != "answered" || result.Answer == nil || len(result.Answer.Selected) != 1 || result.Answer.Selected[0] != "Can't" || result.Answer.Text == nil || *result.Answer.Text != "The deployment permission is missing." {
		t.Fatalf("answered action ask = %#v", result)
	}
}

func TestActionAskDoneAnswers(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Action complete", "A spec")
	ask := createActionAsk(t, handler, issue.Key)

	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected": []string{"Done"},
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer action Done: status=%d body=%s", answered.Code, answered.Body.String())
	}
	result := decodeBody[model.Ask](t, answered)
	if result.State != "answered" || result.Answer == nil || len(result.Answer.Selected) != 1 || result.Answer.Selected[0] != "Done" {
		t.Fatalf("answered action ask = %#v", result)
	}
}

func TestSingleSelectAnswerKeepsOptionalText(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Annotated answer", "A spec")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"actor":    sessionActor(),
		"options":  []map[string]string{{"label": "Ship"}, {"label": "Hold"}},
		"question": "Should we ship?",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create single-select ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[model.Ask](t, created)

	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected": []string{"Ship"},
		"text":     "After the release note is published.",
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer single-select ask with note: status=%d body=%s", answered.Code, answered.Body.String())
	}
	result := decodeBody[model.Ask](t, answered)
	if result.Answer == nil || len(result.Answer.Selected) != 1 || result.Answer.Selected[0] != "Ship" || result.Answer.Text == nil || *result.Answer.Text != "After the release note is published." {
		t.Fatalf("single-select answer = %#v", result)
	}
}

func TestCreateAskRejectsApprovalKind(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Server-created approval", "A spec")

	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Approve this?",
		"kind":     "approval",
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusBadRequest || !strings.Contains(created.Body.String(), `"code":"ASK_KIND_INPUT"`) {
		t.Fatalf("create client approval ask: status=%d body=%s", created.Code, created.Body.String())
	}
}
