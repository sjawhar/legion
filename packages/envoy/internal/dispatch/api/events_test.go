package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestReadEventsKeepsNonAskPayloadRaw(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "TEST", "Raw event payload", "A spec")
	fixtures := []struct {
		eventType string
		payload   string
	}{
		{"message.created", `{"body":"preserve","target":{"kind":"session","id":"other"}}`},
		{"message.answered", `{"body":"separate","target":{"kind":"session","id":"another"}}`},
	}
	for index, fixture := range fixtures {
		if _, err := database.Pool.Exec(context.Background(), `
			insert into events (issue_key, seq, type, actor, payload, notify)
			values ($1, $2, $3, '{"kind":"session","id":"session-0123456789abcdef"}'::jsonb, $4::jsonb, false)
		`, issue.Key, index+2, fixture.eventType, fixture.payload); err != nil {
			t.Fatalf("insert %s event: %v", fixture.eventType, err)
		}
	}

	events, err := directServer(deps).readEvents(context.Background(), issueOwner(issue.Key), eventListOptions{limit: 200})
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for _, fixture := range fixtures {
		var persisted json.RawMessage
		if err := database.Pool.QueryRow(context.Background(), `
			select payload from events where issue_key = $1 and type = $2
		`, issue.Key, fixture.eventType).Scan(&persisted); err != nil {
			t.Fatalf("read persisted %s payload: %v", fixture.eventType, err)
		}
		found := false
		for _, event := range events {
			if event.Type != fixture.eventType {
				continue
			}
			payload, ok := event.Payload.(json.RawMessage)
			if !ok {
				t.Fatalf("%s payload type = %T, want json.RawMessage", fixture.eventType, event.Payload)
			}
			if !bytes.Equal(payload, persisted) {
				t.Fatalf("%s payload = %s, want stored JSON %s", fixture.eventType, payload, persisted)
			}
			found = true
			break
		}
		if !found {
			t.Fatalf("%s event was not returned", fixture.eventType)
		}
	}
}

func TestEventResponsesMatchLegacyPayloadEncodingForEveryEventKind(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "TEST", "Event response compatibility", "A spec")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which path?",
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)

	eventTypes := []string{
		"issue.created", "issue.updated", "issue.closed", "issue.reopened", "issue.claimed", "issue.released",
		"child.added", "child.removed", "child.status",
		"artifact.created", "artifact.version", "artifact.approved", "artifact.changes_requested",
		"ask.opened", "ask.anchor_refreshed", "ask.answered", "ask.resolved", "ask.edited", "ask.handed_back",
		"ask.follower_added", "ask.follower_removed",
		"comment.created", "comment.delivery", "comment.answered", "comment.resolved", "comment.reopened",
		"comment.edited", "comment.anchor_refreshed", "suggestion.accepted", "suggestion.rejected",
		"subscription.remove_requested", "subscription.removed",
		"message.created", "message.answered", "message.delivery", "message.accepted",
		"block.repaired", "block.invalid",
		"project.created", "project.updated", "settings.repo_project.updated", "settings.architecture_source.updated",
		"architecture.synced", "architecture.sync_failed", "user_state.updated", "user_agent_state.updated",
	}
	askEventTypes := map[string]bool{
		"ask.opened": true, "ask.anchor_refreshed": true, "ask.answered": true, "ask.resolved": true,
		"ask.edited": true, "ask.handed_back": true,
	}
	var nextSeq int
	if err := database.Pool.QueryRow(context.Background(), `select last_seq from issues where key = $1`, issue.Key).Scan(&nextSeq); err != nil {
		t.Fatalf("read issue sequence: %v", err)
	}
	for _, eventType := range eventTypes {
		nextSeq++
		payload := `{"body":"payload compatibility","nested":{"kept":true},"values":["one","two"]}`
		if askEventTypes[eventType] {
			payload = fmt.Sprintf(`{"id":%q,"anchor":null}`, ask.ID)
		}
		if _, err := database.Pool.Exec(context.Background(), `
			insert into events (issue_key, seq, type, actor, payload, notify)
			values ($1, $2, $3, '{"kind":"session","id":"session-0123456789abcdef"}'::jsonb, $4::jsonb, false)
		`, issue.Key, nextSeq, eventType, payload); err != nil {
			t.Fatalf("insert %s event: %v", eventType, err)
		}
	}

	events, err := directServer(deps).readEvents(context.Background(), issueOwner(issue.Key), eventListOptions{limit: 200})
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	current, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("encode current response: %v", err)
	}
	legacy := append([]model.Event(nil), events...)
	for index := range legacy {
		raw, ok := legacy[index].Payload.(json.RawMessage)
		if !ok {
			t.Fatalf("%s payload type = %T, want json.RawMessage", legacy[index].Type, legacy[index].Payload)
		}
		if err := json.Unmarshal(raw, &legacy[index].Payload); err != nil {
			t.Fatalf("decode %s legacy payload: %v", legacy[index].Type, err)
		}
	}
	previous, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("encode legacy response: %v", err)
	}
	var currentJSON, previousJSON any
	if err := json.Unmarshal(current, &currentJSON); err != nil {
		t.Fatalf("decode current response: %v", err)
	}
	if err := json.Unmarshal(previous, &previousJSON); err != nil {
		t.Fatalf("decode legacy response: %v", err)
	}
	if !reflect.DeepEqual(currentJSON, previousJSON) {
		t.Fatalf("event response changed after raw payload pass-through:\ncurrent: %s\nlegacy: %s", current, previous)
	}
}
