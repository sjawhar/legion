package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const dispatchToken = "dispatch-test-token"

func TestListIssuesFiltersStatusesWithoutChangingDispatchOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/issues" {
			t.Fatalf("request = %s %s, want GET /api/v1/issues", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("project"); got != "LEGION" {
			t.Fatalf("project query = %q, want LEGION", got)
		}
		if got := r.URL.Query().Get("status"); got != "" {
			t.Fatalf("status query = %q, want no server-side status filter", got)
		}
		assertBearer(t, r)
		writeJSON(t, w, []map[string]any{
			{"key": "LEGION-1", "title": "Triage", "status": "triage", "rank": "a"},
			{"key": "LEGION-3", "title": "First todo", "status": "todo", "rank": "b"},
			{"key": "LEGION-2", "title": "Second todo", "status": "todo", "rank": "c", "parent": "LEGION-1"},
		})
	}))
	defer server.Close()

	issues, err := New(server.URL, dispatchToken).ListIssues(context.Background(), "LEGION", []string{"todo"})
	if err != nil {
		t.Fatalf("list issues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("listed issues = %#v, want two todo issues", issues)
	}
	if issues[0].Key != "LEGION-3" || fmt.Sprint(issues[0].Rank) != "b" {
		t.Fatalf("first filtered issue = %#v, want LEGION-3 with Dispatch rank b", issues[0])
	}
	if issues[1].Key != "LEGION-2" || fmt.Sprint(issues[1].Rank) != "c" || issues[1].Parent == nil || *issues[1].Parent != "LEGION-1" {
		t.Fatalf("second filtered issue = %#v, want LEGION-2 with Dispatch rank c under LEGION-1", issues[1])
	}
}

func TestGetIssueReturnsWorkflowFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/issues/LEGION-8" {
			t.Fatalf("request = %s %s, want GET /api/v1/issues/LEGION-8", r.Method, r.URL.Path)
		}
		assertBearer(t, r)
		writeJSON(t, w, map[string]any{
			"key":                 "LEGION-8",
			"project":             "LEGION",
			"title":               "Ship Dispatch client",
			"status":              "in_progress",
			"rank":                "opaque-rank",
			"parent":              "LEGION-1",
			"primary_artifact_id": "artifact-8",
			"last_seq":            47,
		})
	}))
	defer server.Close()

	issue, err := New(server.URL, dispatchToken).GetIssue(context.Background(), "LEGION-8")
	if err != nil {
		t.Fatalf("get issue: %v", err)
	}
	if issue.Key != "LEGION-8" || issue.Project != "LEGION" || issue.Title != "Ship Dispatch client" || issue.Status != "in_progress" || issue.PrimaryArtifactID != "artifact-8" || issue.LastSeq != 47 {
		t.Fatalf("issue = %#v, want workflow fields from Dispatch", issue)
	}
	if issue.Parent == nil || *issue.Parent != "LEGION-1" {
		t.Fatalf("issue parent = %v, want LEGION-1", issue.Parent)
	}
}

func TestSetStatusSendsDaemonActor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/issues/LEGION-4" {
			t.Fatalf("request = %s %s, want PATCH /api/v1/issues/LEGION-4", r.Method, r.URL.Path)
		}
		assertBearer(t, r)
		var body struct {
			Status string `json:"status"`
			Actor  struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
			} `json:"actor"`
		}
		decodeJSON(t, r, &body)
		if body.Status != "testing" || body.Actor.Kind != "session" || body.Actor.ID != "legion-daemon:LEGION" {
			t.Fatalf("status request = %#v, want testing from legion-daemon:LEGION", body)
		}
		writeJSON(t, w, map[string]string{"status": "testing"})
	}))
	defer server.Close()

	if err := New(server.URL, dispatchToken).SetStatus(context.Background(), "LEGION-4", "testing"); err != nil {
		t.Fatalf("set status: %v", err)
	}
}

func TestPostMessageSendsBodyWithoutAnUnsupportedIdempotencyKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/issues/LEGION-5/messages" {
			t.Fatalf("request = %s %s, want POST /api/v1/issues/LEGION-5/messages", r.Method, r.URL.Path)
		}
		assertBearer(t, r)
		var body map[string]json.RawMessage
		decodeJSON(t, r, &body)
		var message string
		if err := json.Unmarshal(body["body"], &message); err != nil {
			t.Fatalf("decode message body: %v", err)
		}
		if message != "Pull request checks are blocked." {
			t.Fatalf("message body = %q, want Dispatch notice", message)
		}
		if _, ok := body["idempotency_key"]; ok {
			t.Fatalf("message request = %s, want no unsupported idempotency key", mustJSON(t, body))
		}
		var actor struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(body["actor"], &actor); err != nil {
			t.Fatalf("decode actor: %v", err)
		}
		if actor.Kind != "session" || actor.ID != "legion-daemon:LEGION" {
			t.Fatalf("message actor = %#v, want legion daemon session", actor)
		}
		writeJSON(t, w, map[string]string{"id": "message-5"})
	}))
	defer server.Close()

	if err := New(server.URL, dispatchToken).PostMessage(context.Background(), "LEGION-5", "Pull request checks are blocked."); err != nil {
		t.Fatalf("post message: %v", err)
	}
}

func TestApprovalReturnsCurrentDocumentApproval(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/artifacts/artifact-6" {
			t.Fatalf("request = %s %s, want GET /api/v1/artifacts/artifact-6", r.Method, r.URL.Path)
		}
		assertBearer(t, r)
		writeJSON(t, w, map[string]any{
			"id": "artifact-6",
			"approval": map[string]any{
				"state":          "approved",
				"latest_version": 3,
				"version":        3,
			},
		})
	}))
	defer server.Close()

	approval, err := New(server.URL, dispatchToken).Approval(context.Background(), "artifact-6")
	if err != nil {
		t.Fatalf("read approval: %v", err)
	}
	if approval.State != "approved" || approval.LatestVersion != 3 || approval.Version == nil || *approval.Version != 3 {
		t.Fatalf("approval = %#v, want approved version 3", approval)
	}
}

func TestClientReturnsDispatchErrorForCoded4xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONStatus(t, w, http.StatusConflict, map[string]string{"code": "ISSUE_CLOSED", "error": "issue is closed"})
	}))
	defer server.Close()

	_, err := New(server.URL, dispatchToken).GetIssue(context.Background(), "LEGION-9")
	var dispatchErr *Error
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("get issue error = %T %v, want *dispatch.Error", err, err)
	}
	if dispatchErr.Status != http.StatusConflict || dispatchErr.Code != "ISSUE_CLOSED" || dispatchErr.Message != "issue is closed" {
		t.Fatalf("Dispatch error = %#v, want 409 ISSUE_CLOSED", dispatchErr)
	}
}

func TestClientReturnsDispatchErrorFor5xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONStatus(t, w, http.StatusServiceUnavailable, map[string]string{"error": "maintenance"})
	}))
	defer server.Close()

	err := New(server.URL, dispatchToken).SetStatus(context.Background(), "LEGION-10", "testing")
	var dispatchErr *Error
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("set status error = %T %v, want *dispatch.Error", err, err)
	}
	if dispatchErr.Status != http.StatusServiceUnavailable || dispatchErr.Code != "" || dispatchErr.Message != "maintenance" {
		t.Fatalf("Dispatch error = %#v, want 503 with no code", dispatchErr)
	}
}

func TestClientBoundsEveryCall(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	client := New(server.URL, dispatchToken)
	client.timeout = 20 * time.Millisecond
	_, err := client.GetIssue(context.Background(), "LEGION-11")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("get issue error = %v, want context deadline exceeded", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("server never received the bounded request")
	}
}

func assertBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer "+dispatchToken {
		t.Fatalf("Authorization = %q, want Dispatch bearer", got)
	}
	if got := r.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q, want application/json", got)
	}
}

func decodeJSON(t *testing.T, r *http.Request, into any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		t.Fatalf("decode request: %v", err)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	writeJSONStatus(t, w, http.StatusOK, body)
}

func writeJSONStatus(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("write response: %v", err)
	}
}

func mustJSON(t *testing.T, body any) string {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	return string(encoded)
}

func TestMessageBodiesSincePagesNewestFirstUntilTheOutboxWindow(t *testing.T) {
	since := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/issues/LEGION-5/events" {
			t.Fatalf("request = %s %s, want GET /api/v1/issues/LEGION-5/events", r.Method, r.URL.Path)
		}
		assertBearer(t, r)
		requests++
		switch requests {
		case 1:
			if got := r.URL.Query(); got.Get("order") != "desc" || got.Get("limit") != "200" || got.Get("before") != "" {
				t.Fatalf("first query = %s, want newest-first page", r.URL.RawQuery)
			}
			events := make([]map[string]any, 200)
			for i := range events {
				events[i] = map[string]any{"seq": 202 - i, "type": "issue.updated", "created_at": "2026-09-23T01:01:00Z", "payload": map[string]string{"status": "testing"}}
			}
			events[0] = map[string]any{"seq": 202, "type": "message.created", "created_at": "2026-09-23T01:02:00Z", "payload": map[string]string{"body": "Existing daemon notice."}}
			writeJSON(t, w, events)
		case 2:
			if got := r.URL.Query(); got.Get("order") != "desc" || got.Get("limit") != "200" || got.Get("before") != "3" {
				t.Fatalf("second query = %s, want next newest-first page", r.URL.RawQuery)
			}
			writeJSON(t, w, []map[string]any{
				{"seq": 2, "type": "message.created", "created_at": "2026-09-23T00:59:59Z", "payload": map[string]string{"body": "Outside the outbox window."}},
			})
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	bodies, err := New(server.URL, dispatchToken).MessageBodiesSince(context.Background(), "LEGION-5", since)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if requests != 2 || len(bodies) != 1 || bodies[0] != "Existing daemon notice." {
		t.Fatalf("requests=%d bodies=%#v, want one recent message", requests, bodies)
	}
}
