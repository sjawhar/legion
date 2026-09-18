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

func TestIssueAskEventsHydrateAnchorDocument(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ask event document", "A quoted passage.")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]string{"artifact": "spec", "quote": "quoted passage"},
		"question": "What does this mean?",
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create anchored ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)
	if _, err := database.Pool.Exec(context.Background(), `
		update events
		set payload = jsonb_set(payload, '{anchor_artifact}', jsonb_build_object(
			'project', 'STALE', 'slug', 'old-spec', 'name', 'Old spec', 'primary', false
		))
		where type = 'ask.opened' and payload->>'id' = $1
	`, ask.ID); err != nil {
		t.Fatalf("replace persisted anchor document: %v", err)
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list events: status=%d body=%s", events.Code, events.Body.String())
	}
	type anchorArtifact struct {
		Project string `json:"project"`
		Slug    string `json:"slug"`
		Name    string `json:"name"`
		Primary bool   `json:"primary"`
	}
	rows := decodeBody[[]struct {
		Type    string `json:"type"`
		Payload struct {
			ID             string          `json:"id"`
			AnchorArtifact *anchorArtifact `json:"anchor_artifact,omitempty"`
		} `json:"payload"`
	}](t, events)
	for _, row := range rows {
		if row.Type != "ask.opened" || row.Payload.ID != ask.ID {
			continue
		}
		if row.Payload.AnchorArtifact == nil || row.Payload.AnchorArtifact.Project != "TEST" ||
			row.Payload.AnchorArtifact.Slug != "spec" || row.Payload.AnchorArtifact.Name != "spec.md" ||
			!row.Payload.AnchorArtifact.Primary {
			t.Fatalf("ask.opened anchor document = %#v, want TEST/spec spec.md primary", row.Payload.AnchorArtifact)
		}
		return
	}
	t.Fatalf("event rows = %#v, want ask.opened for %s", rows, ask.ID)
}

func TestIssueAskEventsKeepDeletedAnchorDocumentReadable(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Deleted anchor document", "A spec")
	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "archived.md", "content": "A quoted passage.",
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("create anchor document: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, uploaded)
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]string{"artifact": artifact.Artifact.ID, "quote": "quoted passage"},
		"question": "What does this mean?",
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create anchored ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)
	for _, table := range []string{"doc_snapshots", "doc_updates", "doc_checkpoints", "artifact_versions", "events"} {
		if _, err := database.Pool.Exec(
			context.Background(),
			"delete from "+table+" where artifact_id = $1",
			artifact.Artifact.ID,
		); err != nil {
			t.Fatalf("delete %s for anchor document: %v", table, err)
		}
	}
	if _, err := database.Pool.Exec(context.Background(), `
		delete from artifacts where id = $1
	`, artifact.Artifact.ID); err != nil {
		t.Fatalf("delete anchor document: %v", err)
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list events with deleted anchor document: status=%d body=%s", events.Code, events.Body.String())
	}
	rows := decodeBody[[]struct {
		Type    string `json:"type"`
		Payload struct {
			ID             string `json:"id"`
			AnchorArtifact any    `json:"anchor_artifact,omitempty"`
		} `json:"payload"`
	}](t, events)
	for _, row := range rows {
		if row.Type == "ask.opened" && row.Payload.ID == ask.ID {
			if row.Payload.AnchorArtifact != nil {
				t.Fatalf("ask.opened anchor_artifact = %#v, want omitted for deleted document", row.Payload.AnchorArtifact)
			}
			return
		}
	}
	t.Fatalf("event rows = %#v, want ask.opened for %s", rows, ask.ID)
}
