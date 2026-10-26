package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
		"issue.created", "issue.updated", "issue.closed", "issue.reopened", "issue.claimed", "issue.released", "issue.deployed",
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
		"settings.project_default_assignee.updated",
		"architecture.synced", "architecture.sync_failed", "user_state.updated", "user_agent_state.updated",
		"user_thread_state.updated",
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
	const nonAskPayload = `{"zeta":"last","scaled":1.00,"precise":9007199254740993,"alpha":{"kept":true},"values":["one","two"],"html":"a < b && c > d","separators":"x\u2028y\u2029z"}`
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
		var escaped bytes.Buffer
		json.HTMLEscape(&escaped, compacted.Bytes())
		stored[id] = storedEvent{eventType: eventType, ask: isAsk, payload: escaped.Bytes()}
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

// issueEventFields is what an issue.created, issue.updated or issue.closed says about its write.
type issueEventFields struct {
	Key               string                   `json:"key"`
	Title             string                   `json:"title"`
	Status            string                   `json:"status"`
	Changed           []string                 `json:"changed"`
	Ancestors         []string                 `json:"ancestors"`
	ThreadLogins      []string                 `json:"thread_logins"`
	ReferencesChanged []model.ChangedReference `json:"references_changed"`
}

// newestEventPayload decodes the stored payload of the newest eventType event on issueKey, or on
// no issue when issueKey is "" (a project's or a conversation's).
func newestEventPayload[T any](t *testing.T, database *store.Store, eventType, issueKey string) T {
	t.Helper()
	var payload []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select payload from events where type = $1 and coalesce(issue_key, '') = $2 order by id desc limit 1
	`, eventType, issueKey).Scan(&payload); err != nil {
		t.Fatalf("read the newest %s event of %q: %v", eventType, issueKey, err)
	}
	var decoded T
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode %s payload %s: %v", eventType, payload, err)
	}
	return decoded
}

func createChildIssue(t *testing.T, handler http.Handler, title, parent string) model.Issue {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "TEST", "title": title, "parent": parent, "force": true,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create %q under %s: status=%d body=%s", title, parent, response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// An issue event says what its write changed, beside the issue's fields as the write left them:
// issue.created every field the new issue holds, a PATCH the fields whose values it moved, each
// with the issue's ancestors, parent first. A status move names everyone with a Replies row on the
// issue's threads, and nobody else; a retitle names the issues whose panels show the title.
func TestIssueEventsSayWhatTheWriteChanged(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	root := createInteractionIssue(t, handler, "TEST", "Root of the tree", "A spec")
	child := createChildIssue(t, handler, "Middle of the tree", root.Key)
	grandchild := createChildIssue(t, handler, "Leaf of the tree", child.Key)

	rootCreated := newestEventPayload[issueEventFields](t, database, "issue.created", root.Key)
	if rootCreated.Ancestors == nil || len(rootCreated.Ancestors) != 0 {
		t.Fatalf("a root's issue.created ancestors = %#v, want []", rootCreated.Ancestors)
	}
	created := newestEventPayload[issueEventFields](t, database, "issue.created", grandchild.Key)
	if want := []string{"title", "status", "rank", "assignee", "parent"}; !slices.Equal(created.Changed, want) {
		t.Fatalf("issue.created changed = %#v, want every field the new issue holds, %#v", created.Changed, want)
	}
	if want := []string{child.Key, root.Key}; !slices.Equal(created.Ancestors, want) {
		t.Fatalf("a grandchild's issue.created ancestors = %#v, want %#v", created.Ancestors, want)
	}
	if created.Key != grandchild.Key || created.Title != "Leaf of the tree" || created.ThreadLogins != nil {
		t.Fatalf("issue.created = %#v, want the issue's own fields and no thread_logins", created)
	}

	patch := func(body map[string]any) {
		t.Helper()
		if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+grandchild.Key, body, "alice"); response.Code != http.StatusOK {
			t.Fatalf("patch %v: status=%d body=%s", body, response.Code, response.Body.String())
		}
	}
	patch(map[string]any{"title": "Renamed leaf"})
	retitled := newestEventPayload[issueEventFields](t, database, "issue.updated", grandchild.Key)
	if !slices.Equal(retitled.Changed, []string{"title"}) || retitled.Title != "Renamed leaf" {
		t.Fatalf("a title-only PATCH's issue.updated = %#v, want changed [title] and the new title", retitled)
	}
	if want := []string{child.Key, root.Key}; !slices.Equal(retitled.Ancestors, want) || retitled.ThreadLogins != nil {
		t.Fatalf("a title-only PATCH's issue.updated = %#v, want ancestors %#v and no thread_logins", retitled, want)
	}
	// The child_of edge to the parent stays, and the parent's referenced-by panel shows this title.
	if want := []model.ChangedReference{{Kind: "issue", ID: child.Key}}; !reflect.DeepEqual(retitled.ReferencesChanged, want) {
		t.Fatalf("a retitle's references_changed = %#v, want the parent whose panel shows the title, %#v", retitled.ReferencesChanged, want)
	}
	patch(map[string]any{"priority": 1})
	if reprioritised := newestEventPayload[issueEventFields](t, database, "issue.updated", grandchild.Key); !slices.Equal(reprioritised.Changed, []string{"priority"}) || reprioritised.ReferencesChanged != nil {
		t.Fatalf("a priority-only PATCH's issue.updated = %#v, want changed [priority] naming no reference", reprioritised)
	}

	// Alice writes in the thread of one of the issue's open asks, the rows a close or reopen moves
	// between the Inbox and the Replies page; Bob writes only in a plain comment thread, which no
	// close moves.
	ask := createAnswerListAsk(t, handler, grandchild.Key, "Still needed?")
	createThreadComment(t, handler, grandchild.Key, map[string]any{"body": "Is this still needed?", "ask_id": ask.ID}, "alice")
	createThreadComment(t, handler, grandchild.Key, map[string]any{"body": "Unrelated."}, "bob")
	patch(map[string]any{"status": "done"})
	closed := newestEventPayload[issueEventFields](t, database, "issue.closed", grandchild.Key)
	if !slices.Equal(closed.Changed, []string{"status", "closed_at"}) || !slices.Equal(closed.ThreadLogins, []string{"alice"}) {
		t.Fatalf("issue.closed = %#v, want changed [status closed_at] and thread_logins [alice]", closed)
	}
	patch(map[string]any{"status": "backlog"})
	reopened := newestEventPayload[issueEventFields](t, database, "issue.updated", grandchild.Key)
	if !slices.Equal(reopened.Changed, []string{"status", "closed_at"}) || reopened.Status != "backlog" {
		t.Fatalf("a reopen's issue.updated = %#v, want changed [status closed_at] and the new status", reopened)
	}
	if !slices.Equal(reopened.ThreadLogins, []string{"alice"}) || slices.Contains(reopened.ThreadLogins, "bob") {
		t.Fatalf("a reopen's thread_logins = %#v, want alice, who wrote in a thread, and not bob", reopened.ThreadLogins)
	}
	if want := []string{child.Key, root.Key}; !slices.Equal(reopened.Ancestors, want) {
		t.Fatalf("a reopen's ancestors = %#v, want %#v", reopened.Ancestors, want)
	}
}

// user_state.updated says which of pinned, last_read_seq and dismissed the write moved, so a tab
// refreshes its pinned list for a pin and not for a read mark. It carries no issue key.
func TestUserStateEventSaysWhichFieldsMoved(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Pinned", "A spec")
	type stateEvent struct {
		Login   string   `json:"login"`
		Changed []string `json:"changed"`
		Key     *string  `json:"key"`
	}
	put := func(body map[string]any, want []string) {
		t.Helper()
		if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", body, "alice"); response.Code != http.StatusOK {
			t.Fatalf("put state %v: status=%d body=%s", body, response.Code, response.Body.String())
		}
		event := newestEventPayload[stateEvent](t, database, "user_state.updated", "")
		if event.Login != "alice" || event.Key != nil || event.Changed == nil || !slices.Equal(event.Changed, want) {
			t.Fatalf("user_state.updated after %v = %#v, want alice's changed %#v and no issue key", body, event, want)
		}
	}
	put(map[string]any{"pinned": true}, []string{"pinned"})
	put(map[string]any{"last_read_seq": 3}, []string{"last_read_seq"})
	put(map[string]any{"dismissed": []string{"ask-1"}}, []string{"dismissed"})
	put(map[string]any{"pinned": true, "last_read_seq": 3}, []string{})
	put(map[string]any{"pinned": false, "last_read_seq": 5}, []string{"pinned", "last_read_seq"})
}

// A resuming connection whose since is at or below the log's head is replayed everything after it,
// and says so in X-Dispatch-Event-Replay, by since= or by Last-Event-ID alike; a cold connect
// (nothing replayed) and a since above the head (an id of a log since replaced) do not.
func TestEventStreamSaysWhenItsReplayCoversTheGap(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createInteractionIssue(t, handler, "TEST", "Replayed", "A spec")
	head := eventHead(t, database)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	for _, connection := range []struct {
		name   string
		resume func(*http.Request)
		want   string
	}{
		{name: "cold", resume: func(*http.Request) {}, want: ""},
		{name: "resuming below the head by since", resume: func(request *http.Request) {
			request.URL.RawQuery = fmt.Sprintf("since=%d", head-1)
		}, want: "complete"},
		{name: "resuming at the head by since", resume: func(request *http.Request) {
			request.URL.RawQuery = fmt.Sprintf("since=%d", head)
		}, want: "complete"},
		{name: "resuming below the head by Last-Event-ID", resume: func(request *http.Request) {
			request.Header.Set("Last-Event-ID", strconv.FormatInt(head-1, 10))
		}, want: "complete"},
		{name: "since above the head", resume: func(request *http.Request) {
			request.URL.RawQuery = fmt.Sprintf("since=%d", head+100)
		}, want: ""},
	} {
		t.Run(connection.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), sseStreamDeadline)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/v1/events", nil)
			if err != nil {
				t.Fatalf("construct SSE request: %v", err)
			}
			request.Header.Set("Authorization", "Bearer agent-token")
			connection.resume(request)
			stream, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("open SSE stream: %v", err)
			}
			defer stream.Body.Close()
			if stream.StatusCode != http.StatusOK {
				t.Fatalf("SSE status = %d, want 200", stream.StatusCode)
			}
			if got := stream.Header.Get(eventReplayHeader); got != connection.want {
				t.Fatalf("%s = %q, want %q", eventReplayHeader, got, connection.want)
			}
		})
	}
}
