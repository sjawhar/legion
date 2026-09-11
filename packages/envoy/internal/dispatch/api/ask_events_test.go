package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestIssueAskEventsExposeOpenedEventID(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ask event identity", "A spec")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Ship it?",
		"options":  []map[string]string{{"label": "Ship"}},
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		ID            string `json:"id"`
		OpenedEventID int64  `json:"opened_event_id"`
	}](t, created)
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected": []string{"Ship"},
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", answered.Code, answered.Body.String())
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list events: status=%d body=%s", events.Code, events.Body.String())
	}
	rows := decodeBody[[]struct {
		ID      int64 `json:"id"`
		Payload struct {
			OpenedEventID int64 `json:"opened_event_id"`
		} `json:"payload"`
		Type string `json:"type"`
	}](t, events)

	var openedEventID int64
	for _, row := range rows {
		if row.Type == "ask.opened" {
			openedEventID = row.ID
		}
	}
	if openedEventID == 0 {
		t.Fatal("ask.opened event was not returned")
	}
	var rawPayload []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select payload from events where id = $1
	`, openedEventID).Scan(&rawPayload); err != nil {
		t.Fatalf("read persisted ask.opened payload: %v", err)
	}
	var persisted struct {
		OpenedEventID int64 `json:"opened_event_id"`
	}
	if err := json.Unmarshal(rawPayload, &persisted); err != nil {
		t.Fatalf("decode persisted ask.opened payload: %v", err)
	}
	if persisted.OpenedEventID != openedEventID {
		t.Fatalf("persisted ask.opened opened_event_id=%d, want %d", persisted.OpenedEventID, openedEventID)
	}
	if ask.OpenedEventID != openedEventID {
		t.Fatalf("created ask opened_event_id=%d, want %d", ask.OpenedEventID, openedEventID)
	}
	for _, row := range rows {
		if row.Type == "ask.opened" || row.Type == "ask.answered" {
			if row.Payload.OpenedEventID != openedEventID {
				t.Fatalf("%s opened_event_id=%d, want %d", row.Type, row.Payload.OpenedEventID, openedEventID)
			}
		}
	}
}
