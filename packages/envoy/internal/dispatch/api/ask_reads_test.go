package api

import (
	"context"
	"net/http"
	"testing"
)

type askIdentity struct {
	ID            string `json:"id"`
	OpenedEventID int64  `json:"opened_event_id"`
}

func TestAskReadsAttachCanonicalOpenedEventID(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ask read identity", "A spec")
	openResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Keep open?",
		"options":  []map[string]string{{"label": "Yes"}},
		"actor":    sessionActor(),
	})
	if openResponse.Code != http.StatusCreated {
		t.Fatalf("create open ask: status=%d body=%s", openResponse.Code, openResponse.Body.String())
	}
	openAsk := decodeBody[askIdentity](t, openResponse)
	legacyResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Answer for fallback?",
		"options":  []map[string]string{{"label": "Yes"}},
		"actor":    sessionActor(),
	})
	if legacyResponse.Code != http.StatusCreated {
		t.Fatalf("create fallback ask: status=%d body=%s", legacyResponse.Code, legacyResponse.Body.String())
	}
	legacyAsk := decodeBody[askIdentity](t, legacyResponse)
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+legacyAsk.ID+"/answer", map[string]any{
		"selected": []string{"Yes"},
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer fallback ask: status=%d body=%s", answered.Code, answered.Body.String())
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list events: status=%d body=%s", events.Code, events.Body.String())
	}
	rows := decodeBody[[]struct {
		ID      int64 `json:"id"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
		Type string `json:"type"`
	}](t, events)
	var opened, answeredEvent int64
	for _, row := range rows {
		if row.Payload.ID != legacyAsk.ID {
			continue
		}
		if row.Type == "ask.opened" {
			opened = row.ID
		}
		if row.Type == "ask.answered" {
			answeredEvent = row.ID
		}
	}
	if opened == 0 || answeredEvent == 0 {
		t.Fatalf("missing fallback events: opened=%d answered=%d", opened, answeredEvent)
	}
	if _, err := database.Pool.Exec(context.Background(), `delete from events where id = $1`, opened); err != nil {
		t.Fatalf("delete legacy opened event: %v", err)
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+legacyAsk.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("get ask: status=%d body=%s", read.Code, read.Body.String())
	}
	readAsk := decodeBody[struct {
		Ask askIdentity `json:"ask"`
	}](t, read).Ask
	if readAsk.OpenedEventID != answeredEvent {
		t.Fatalf("get ask opened_event_id=%d, want legacy fallback %d", readAsk.OpenedEventID, answeredEvent)
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?state=all", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list asks: status=%d body=%s", listed.Code, listed.Body.String())
	}
	listedLegacy := false
	for _, ask := range decodeBody[[]askIdentity](t, listed) {
		if ask.ID == legacyAsk.ID {
			listedLegacy = true
			if ask.OpenedEventID != answeredEvent {
				t.Fatalf("listed ask opened_event_id=%d, want legacy fallback %d", ask.OpenedEventID, answeredEvent)
			}
		}
	}
	if !listedLegacy {
		t.Fatal("fallback ask missing from issue ask list")
	}

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("get issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	detailOpen := false
	for _, ask := range decodeBody[struct {
		OpenAsks []askIdentity `json:"open_asks"`
	}](t, detail).OpenAsks {
		if ask.ID == openAsk.ID {
			detailOpen = true
			if ask.OpenedEventID != openAsk.OpenedEventID {
				t.Fatalf("issue open ask opened_event_id=%d, want %d", ask.OpenedEventID, openAsk.OpenedEventID)
			}
		}
	}
	if !detailOpen {
		t.Fatal("open ask missing from issue detail")
	}

	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("list inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	inboxOpen := false
	for _, ask := range decodeBody[[]askIdentity](t, inbox) {
		if ask.ID == openAsk.ID {
			inboxOpen = true
			if ask.OpenedEventID != openAsk.OpenedEventID {
				t.Fatalf("inbox ask opened_event_id=%d, want %d", ask.OpenedEventID, openAsk.OpenedEventID)
			}
		}
	}
	if !inboxOpen {
		t.Fatal("open ask missing from inbox")
	}
}
