package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type editableAsk struct {
	ID            string `json:"id"`
	OpenedEventID int64  `json:"opened_event_id"`
}

func createEditableAsk(t *testing.T, handler http.Handler, issueKey string) editableAsk {
	t.Helper()
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": "Which implementation?",
		"options":  []map[string]string{{"label": "HTTP"}, {"label": "MCP"}},
		"urgency":  "low",
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", created.Code, created.Body.String())
	}
	return decodeBody[editableAsk](t, created)
}

func TestEditOpenAskByAuthorPersistsUpdatedAskAndEditEvent(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Edit ask", "A spec")
	ask := createEditableAsk(t, handler, issue.Key)

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+ask.ID, map[string]any{
		"question": "Which transport should we implement?",
		"options":  []map[string]string{{"label": "REST"}, {"label": "gRPC"}},
		"multiple": true,
		"urgency":  "high",
		"actor":    sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit ask: status=%d body=%s", edited.Code, edited.Body.String())
	}
	updated := decodeBody[struct {
		ID       string `json:"id"`
		Question string `json:"question"`
		Options  []struct {
			Label string `json:"label"`
		} `json:"options"`
		Multiple      bool    `json:"multiple"`
		Urgency       string  `json:"urgency"`
		OpenedEventID int64   `json:"opened_event_id"`
		EditedAt      *string `json:"edited_at"`
	}](t, edited)
	if updated.ID != ask.ID || updated.Question != "Which transport should we implement?" || len(updated.Options) != 2 || updated.Options[0].Label != "REST" || updated.Options[1].Label != "gRPC" || !updated.Multiple || updated.Urgency != "high" || updated.OpenedEventID != ask.OpenedEventID || updated.EditedAt == nil {
		t.Fatalf("edited ask = %#v", updated)
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice")
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"question":"Which transport should we implement?"`) || !strings.Contains(read.Body.String(), `"edited_at":"`) {
		t.Fatalf("read updated ask: status=%d body=%s", read.Code, read.Body.String())
	}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?state=open", nil, "alice")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"question":"Which transport should we implement?"`) {
		t.Fatalf("list updated ask: status=%d body=%s", listed.Code, listed.Body.String())
	}
	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox?project=TEST", nil, "alice")
	if inbox.Code != http.StatusOK || !strings.Contains(inbox.Body.String(), `"question":"Which transport should we implement?"`) || !strings.Contains(inbox.Body.String(), `"edited_at":"`) {
		t.Fatalf("inbox updated ask: status=%d body=%s", inbox.Code, inbox.Body.String())
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list events: status=%d body=%s", events.Code, events.Body.String())
	}
	rows := decodeBody[[]struct {
		ID      int64  `json:"id"`
		Type    string `json:"type"`
		Payload struct {
			ID            string `json:"id"`
			Question      string `json:"question"`
			OpenedEventID int64  `json:"opened_event_id"`
			Previous      struct {
				Question string `json:"question"`
				Urgency  string `json:"urgency"`
			} `json:"previous"`
			EditedBy struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
			} `json:"edited_by"`
		} `json:"payload"`
	}](t, events)
	var editedEventID int64
	for _, row := range rows {
		if row.Type != "ask.edited" || row.Payload.ID != ask.ID {
			continue
		}
		editedEventID = row.ID
		if row.Payload.Question != "Which transport should we implement?" || row.Payload.OpenedEventID != ask.OpenedEventID || row.Payload.Previous.Question != "Which implementation?" || row.Payload.Previous.Urgency != "low" || row.Payload.EditedBy.Kind != "session" || row.Payload.EditedBy.ID != sessionActor()["id"] {
			t.Fatalf("ask.edited payload = %#v", row.Payload)
		}
	}
	if editedEventID == 0 {
		t.Fatalf("event log = %#v, want ask.edited", rows)
	}
	var persisted struct {
		Question string `json:"question"`
		Previous struct {
			Question string `json:"question"`
		} `json:"previous"`
	}
	var unpublished bool
	var rawPayload []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select payload, published_at is null from events where id = $1
	`, editedEventID).Scan(&rawPayload, &unpublished); err != nil {
		t.Fatalf("read persisted ask.edited event: %v", err)
	}
	if err := json.Unmarshal(rawPayload, &persisted); err != nil {
		t.Fatalf("decode persisted ask.edited payload: %v", err)
	}
	if persisted.Question != "Which transport should we implement?" || persisted.Previous.Question != "Which implementation?" || !unpublished {
		t.Fatalf("persisted ask.edited payload=%#v unpublished=%t", persisted, unpublished)
	}
}

func TestEditAskRejectsAnsweredAsk(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Answered ask", "A spec")
	ask := createEditableAsk(t, handler, issue.Key)
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{"selected": []string{"HTTP"}}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", answered.Code, answered.Body.String())
	}

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+ask.ID, map[string]any{
		"question": "Should not update", "actor": sessionActor(),
	})
	if edited.Code != http.StatusConflict || !strings.Contains(edited.Body.String(), `"code":"ASK_NOT_OPEN"`) {
		t.Fatalf("edit answered ask: status=%d body=%s", edited.Code, edited.Body.String())
	}
}

func TestEditAskRejectsOtherAgentSession(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Agent ownership", "A spec")
	ask := createEditableAsk(t, handler, issue.Key)

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+ask.ID, map[string]any{
		"question": "Should not update",
		"actor":    map[string]any{"kind": "session", "id": "session-other"},
	})
	if edited.Code != http.StatusForbidden || !strings.Contains(edited.Body.String(), `"code":"NOT_AUTHOR"`) {
		t.Fatalf("edit by another session: status=%d body=%s", edited.Code, edited.Body.String())
	}
}

func TestEditAskAllowsHumanEditor(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Human ask edit", "A spec")
	ask := createEditableAsk(t, handler, issue.Key)

	edited := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+ask.ID, map[string]any{
		"question": "A human correction",
	}, "alice")
	if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"question":"A human correction"`) {
		t.Fatalf("human edit: status=%d body=%s", edited.Code, edited.Body.String())
	}
}

func TestEditAskRejectsEmptyPatch(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Empty ask edit", "A spec")
	ask := createEditableAsk(t, handler, issue.Key)

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+ask.ID, map[string]any{"actor": sessionActor()})
	if edited.Code != http.StatusBadRequest || !strings.Contains(edited.Body.String(), `"code":"INVALID_ASK"`) {
		t.Fatalf("empty edit: status=%d body=%s", edited.Code, edited.Body.String())
	}
}

func TestEditAskValidatesChangedFields(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Validate ask edit", "A spec")
	ask := createEditableAsk(t, handler, issue.Key)

	for _, input := range []map[string]any{
		{"urgency": "immediate", "actor": sessionActor()},
		{"options": []map[string]string{{"label": ""}}, "actor": sessionActor()},
	} {
		response := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+ask.ID, input)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_ASK"`) {
			t.Fatalf("invalid edit input %#v: status=%d body=%s", input, response.Code, response.Body.String())
		}
	}
}
