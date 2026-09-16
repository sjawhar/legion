package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestCreateAskRejectsRemovedActionKind(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Removed action kind", "A spec")

	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Confirm the release is deployed.",
		"kind":     "action",
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusBadRequest || !strings.Contains(created.Body.String(), `"code":"ASK_KIND_INPUT"`) || !strings.Contains(created.Body.String(), "action") {
		t.Fatalf("create action ask: status=%d body=%s, want 400 ASK_KIND_INPUT naming the removed kind", created.Code, created.Body.String())
	}
}

// A to-do handed to a human is an ordinary question whose asker chose Done / Can't; no option
// label carries a server rule of its own.
func TestDoneCantQuestionAnswersLikeAnyQuestion(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Human to-do", "A spec")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Confirm the release is deployed.",
		"options":  []map[string]string{{"label": "Done"}, {"label": "Can't"}},
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create Done / Can't question: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[model.Ask](t, created)
	if ask.Kind != "question" {
		t.Fatalf("ask kind = %q, want question", ask.Kind)
	}

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+ask.ID, map[string]any{
		"options": []map[string]string{{"label": "Done"}, {"label": "Can't"}, {"label": "Later"}},
		"actor":   sessionActor(),
	})
	if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"label":"Later"`) {
		t.Fatalf("edit Done / Can't options: status=%d body=%s", edited.Code, edited.Body.String())
	}
	current := decodeBody[struct {
		EditedAt *string `json:"edited_at"`
	}](t, edited)

	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected":           []string{"Can't"},
		"expected_edited_at": current.EditedAt,
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer Can't without text: status=%d body=%s", answered.Code, answered.Body.String())
	}
	result := decodeBody[model.Ask](t, answered)
	if result.State != "answered" || result.Answer == nil || len(result.Answer.Selected) != 1 || result.Answer.Selected[0] != "Can't" || result.Answer.Text != nil {
		t.Fatalf("answered Done / Can't question = %#v", result)
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
