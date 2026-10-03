package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestEventPayloadsAreServedAsStoredForEveryEventKind(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Event response bytes", "A spec")
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
	type storedEvent struct {
		eventType string
		ask       bool
		payload   []byte
	}
	var lastSeq int
	if err := database.Pool.QueryRow(context.Background(), `select last_seq from issues where key = $1`, issue.Key).Scan(&lastSeq); err != nil {
		t.Fatalf("read issue sequence: %v", err)
	}
	firstSeq := lastSeq
	var firstID int64
	stored := map[int64]storedEvent{}
	const nonAskPayload = `{"zeta":"last","scaled":1.00,"precise":9007199254740993,"alpha":{"kept":true},"values":["one","two"]}`
	for _, eventType := range eventTypes {
		lastSeq++
		payload := nonAskPayload
		isAsk := askEventTypes[eventType]
		if isAsk {
			payload = fmt.Sprintf(`{"id":%q,"anchor":null}`, ask.ID)
		}
		var id int64
		var text string
		if err := database.Pool.QueryRow(context.Background(), `
			insert into events (issue_key, seq, type, actor, payload, notify)
			values ($1, $2, $3, '{"kind":"session","id":"session-0123456789abcdef"}'::jsonb, $4::jsonb, false)
			returning id, payload::text
		`, issue.Key, lastSeq, eventType, payload).Scan(&id, &text); err != nil {
			t.Fatalf("insert %s event: %v", eventType, err)
		}
		if firstID == 0 {
			firstID = id
		}
		var compacted bytes.Buffer
		if err := json.Compact(&compacted, []byte(text)); err != nil {
			t.Fatalf("compact stored %s payload %s: %v", eventType, text, err)
		}
		stored[id] = storedEvent{eventType: eventType, ask: isAsk, payload: compacted.Bytes()}
	}

	type servedEvent struct {
		ID      int64           `json:"id"`
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	checkServed := func(surface string, served []servedEvent) {
		t.Helper()
		seen := map[int64]bool{}
		for _, event := range served {
			want, ok := stored[event.ID]
			if !ok {
				continue
			}
			seen[event.ID] = true
			if event.Type != want.eventType {
				t.Fatalf("%s event %d type = %s, want %s", surface, event.ID, event.Type, want.eventType)
			}
			if !want.ask {
				if !bytes.Equal(event.Payload, want.payload) {
					t.Fatalf("%s %s payload = %s, want stored jsonb text %s", surface, event.Type, event.Payload, want.payload)
				}
				continue
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("%s decode %s payload: %v", surface, event.Type, err)
			}
			for _, field := range []string{"opened_event_id", "referenced_by_count"} {
				if _, ok := payload[field]; !ok {
					t.Fatalf("%s %s payload = %s, want hydrated %s", surface, event.Type, event.Payload, field)
				}
			}
			if string(payload["id"]) != fmt.Sprintf("%q", ask.ID) {
				t.Fatalf("%s %s payload id = %s, want %q", surface, event.Type, payload["id"], ask.ID)
			}
		}
		if len(seen) != len(stored) {
			t.Fatalf("%s returned %d of %d inserted events", surface, len(seen), len(stored))
		}
	}

	response := dispatchRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/v1/issues/%s/events?after=%d&limit=200", issue.Key, firstSeq), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read issue events: status=%d body=%s", response.Code, response.Body.String())
	}
	checkServed("HTTP", decodeBody[[]servedEvent](t, response))

	server := httptest.NewServer(handler)
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/events?since=%d", server.URL, firstID-1), nil)
	if err != nil {
		t.Fatalf("construct SSE request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer agent-token")
	stream, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open SSE stream: %v", err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d, want 200", stream.StatusCode)
	}
	scanner := bufio.NewScanner(stream.Body)
	frames := make([]servedEvent, 0, len(stored))
	for read := 0; len(frames) < len(stored); read++ {
		if read == len(stored)+20 {
			t.Fatalf("SSE replay held %d of %d inserted events after %d frames", len(frames), len(stored), read)
		}
		frame := readSSEFrame(t, scanner)
		if len(frame) != 3 || !strings.HasPrefix(frame[2], "data: ") {
			t.Fatalf("SSE frame = %#v, want id, event and data", frame)
		}
		var event servedEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frame[2], "data: ")), &event); err != nil {
			t.Fatalf("decode SSE frame %#v: %v", frame, err)
		}
		if _, ok := stored[event.ID]; ok {
			frames = append(frames, event)
		}
	}
	checkServed("SSE replay", frames)
}
