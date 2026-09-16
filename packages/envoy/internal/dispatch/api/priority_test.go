package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type priorityIssue struct {
	Key      string `json:"key"`
	Priority *int   `json:"priority"`
}

func TestIssuePriorityCreatePatchClearAndEvent(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}

	createdResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Priority issue", "priority": 2,
	}, "alice")
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create priority issue: status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	created := decodeBody[priorityIssue](t, createdResponse)
	expectPriority(t, created.Priority, 2)

	updatedResponse := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{
		"priority": 0,
	}, "alice")
	if updatedResponse.Code != http.StatusOK {
		t.Fatalf("set priority: status=%d body=%s", updatedResponse.Code, updatedResponse.Body.String())
	}
	updated := decodeBody[priorityIssue](t, updatedResponse)
	expectPriority(t, updated.Priority, 0)

	eventsResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+created.Key+"/events", nil, "alice")
	if eventsResponse.Code != http.StatusOK {
		t.Fatalf("list issue events: status=%d body=%s", eventsResponse.Code, eventsResponse.Body.String())
	}
	var events []struct {
		Type    string `json:"type"`
		Payload struct {
			Priority *int `json:"priority"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(eventsResponse.Body).Decode(&events); err != nil {
		t.Fatalf("decode issue events: %v", err)
	}
	foundPriorityUpdate := false
	for _, event := range events {
		if event.Type == "issue.updated" {
			foundPriorityUpdate = true
			expectPriority(t, event.Payload.Priority, 0)
		}
	}
	if !foundPriorityUpdate {
		t.Fatal("priority update did not emit issue.updated")
	}

	clearedResponse := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{
		"priority": nil,
	}, "alice")
	if clearedResponse.Code != http.StatusOK {
		t.Fatalf("clear priority: status=%d body=%s", clearedResponse.Code, clearedResponse.Body.String())
	}
	cleared := decodeBody[priorityIssue](t, clearedResponse)
	if cleared.Priority != nil {
		t.Fatalf("cleared priority = %d, want null", *cleared.Priority)
	}
}

func TestIssuePriorityRejectsInvalidInputsOnCreateAndPatch(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	createdResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Existing issue",
	}, "alice")
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	created := decodeBody[model.Issue](t, createdResponse)

	for _, priority := range []any{-1, 4, 1.5, "P0"} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "CORE", "title": "Invalid priority", "priority": priority,
		}, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_PRIORITY"`) {
			t.Fatalf("create priority %#v: status=%d body=%s", priority, response.Code, response.Body.String())
		}

		response = dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{
			"priority": priority,
		}, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_PRIORITY"`) {
			t.Fatalf("patch priority %#v: status=%d body=%s", priority, response.Code, response.Body.String())
		}
	}
}

// Rank is the only order: priority is a badge and a filter, never a sort key, so the
// List and the Board (whose columns keep the list's order) agree even when priorities are set.
func TestListIssuesOrdersByRankRegardlessOfPriority(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	create := func(title string, priority any) priorityIssue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "CORE", "title": title, "priority": priority,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		issue := decodeBody[priorityIssue](t, response)
		if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
			"status": "todo",
		}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("set %s todo: status=%d body=%s", issue.Key, response.Code, response.Body.String())
		}
		return issue
	}
	// Created in rank order: the P0 first, then the unset, then the P3.
	p0 := create("P0 todo", 0)
	unset := create("Unset todo", nil)
	p3 := create("P3 todo", 3)
	// Reverse the ranks with the board's own moves: the P3 to the top, then the P0 to the bottom.
	for _, move := range []struct {
		key   string
		input map[string]string
	}{
		{p3.Key, map[string]string{"before": p0.Key}},
		{p0.Key, map[string]string{"after": unset.Key}},
	} {
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+move.key, map[string]any{
			"rank": move.input,
		}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("rank %s: status=%d body=%s", move.key, response.Code, response.Body.String())
		}
	}

	listedResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE", nil, "alice")
	if listedResponse.Code != http.StatusOK {
		t.Fatalf("list issues: status=%d body=%s", listedResponse.Code, listedResponse.Body.String())
	}
	assertPriorityIssueOrder(t, decodeBody[[]priorityIssue](t, listedResponse), p3.Key, unset.Key, p0.Key)

	for _, issue := range []priorityIssue{p0, unset, p3} {
		response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]bool{"pinned": true}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("pin %s: status=%d body=%s", issue.Key, response.Code, response.Body.String())
		}
	}
	pinnedResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?pinned=true", nil, "alice")
	if pinnedResponse.Code != http.StatusOK {
		t.Fatalf("list pinned issues: status=%d body=%s", pinnedResponse.Code, pinnedResponse.Body.String())
	}
	assertPriorityIssueOrder(t, decodeBody[[]priorityIssue](t, pinnedResponse), p3.Key, unset.Key, p0.Key)
}

func TestInboxOrdersOpenAsksByIssuePriority(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	create := func(title string, priority int) priorityIssue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "CORE", "title": title, "priority": priority,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[priorityIssue](t, response)
	}
	p2 := create("P2 issue", 2)
	p0 := create("P0 issue", 0)
	for _, issue := range []priorityIssue{p2, p0} {
		response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": "Should this ship?", "actor": sessionActor(),
		})
		if response.Code != http.StatusCreated {
			t.Fatalf("create ask for %s: status=%d body=%s", issue.Key, response.Code, response.Body.String())
		}
	}

	inboxResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inboxResponse.Code != http.StatusOK {
		t.Fatalf("list inbox: status=%d body=%s", inboxResponse.Code, inboxResponse.Body.String())
	}
	rows := decodeBody[[]struct {
		Issue struct {
			Key string `json:"key"`
		} `json:"issue"`
		Priority *int `json:"priority"`
	}](t, inboxResponse)
	if len(rows) != 2 {
		t.Fatalf("inbox rows = %#v, want two", rows)
	}
	if rows[0].Issue.Key != p0.Key || rows[1].Issue.Key != p2.Key {
		t.Fatalf("inbox order = %#v, want P0 %s before P2 %s", rows, p0.Key, p2.Key)
	}
	expectPriority(t, rows[0].Priority, 0)
	expectPriority(t, rows[1].Priority, 2)
}

func expectPriority(t *testing.T, got *int, want int) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("priority = %v, want %d", got, want)
	}
}

func assertPriorityIssueOrder(t *testing.T, got []priorityIssue, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("listed issues = %#v, want %d rows", got, len(want))
	}
	for index, key := range want {
		if got[index].Key != key {
			t.Fatalf("listed issue %d = %s, want %s (%#v)", index, got[index].Key, key, got)
		}
	}
}
