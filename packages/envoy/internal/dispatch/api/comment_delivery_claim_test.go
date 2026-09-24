package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func TestMentionDeliveryPersistsPendingBeforeSendAndRetryCompletesIt(t *testing.T) {
	var database *store.Store
	pendingDuringSend := "not observed"
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				Payload string `json:"payload"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode Envoy send request: %v", err)
			}
			var frame struct {
				Event struct {
					Payload struct {
						ID string `json:"id"`
					} `json:"payload"`
				} `json:"event"`
			}
			if err := json.Unmarshal([]byte(request.Payload), &frame); err != nil {
				t.Fatalf("decode mention delivery frame: %v", err)
			}
			var state string
			err := database.Pool.QueryRow(context.Background(), `
				select state from comment_deliveries where comment_id = $1 and target = 'session:s1' and attempt = 1
			`, frame.Event.Payload.ID).Scan(&state)
			if err != nil {
				pendingDuringSend = err.Error()
			} else {
				pendingDuringSend = state
			}
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	var handler http.Handler
	handler, database = newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Durable pending mention", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "This delivery must be durable before its send.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if pendingDuringSend != "pending" {
		t.Fatalf("delivery state during send = %q, want pending row committed before sending", pendingDuringSend)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set state = 'pending', envelope_id = null, error = null, claimed_at = null
		where comment_id = $1 and target = 'session:s1' and attempt = 1
	`, comment.ID); err != nil {
		t.Fatalf("restore pending delivery: %v", err)
	}
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry pending mention: status=%d body=%s", retry.Code, retry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, retry); attempt.Attempt != 1 || attempt.State != "sent" {
		t.Fatalf("retried pending attempt = %#v, want attempt 1 completed as sent", attempt)
	}
	var attempts int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from comment_deliveries where comment_id = $1 and target = 'session:s1'
	`, comment.ID).Scan(&attempts); err != nil {
		t.Fatalf("count mention attempts: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("mention attempts after pending retry = %d, want 1", attempts)
	}
}

func TestConcurrentMentionRetryResolvesNewAttemptUnderLock(t *testing.T) {
	var mu sync.Mutex
	roleHolder := "s1"
	roleLookups := 0
	blockS1 := false
	started := make(chan struct{})
	release := make(chan struct{})
	sent := []string{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			mu.Lock()
			roleLookups++
			holder := roleHolder
			mu.Unlock()
			_, _ = w.Write([]byte(`{"role":"reviewer","holder":"` + holder + `","title":"reviewer","capabilities":["steer"]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				TargetSession string `json:"target_session"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode retry send: %v", err)
			}
			mu.Lock()
			block := blockS1 && request.TargetSession == "s1"
			if block {
				blockS1 = false
				close(started)
			}
			mu.Unlock()
			if block {
				<-release
			}
			mu.Lock()
			sent = append(sent, request.TargetSession)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"` + request.TargetSession + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	direct := directServer(t, database, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Concurrent mention retry", "before")
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Retry with the new holder after the original completes.", "mentions": []map[string]any{{"target": "role:reviewer"}},
	}))
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set state = 'pending', envelope_id = null, error = null, claimed_at = null
		where comment_id = $1 and target = 'role:reviewer' and attempt = 1
	`, created.ID); err != nil {
		t.Fatalf("restore concurrent pending delivery: %v", err)
	}
	comment, err := direct.loadComment(context.Background(), database.Pool, created.ID)
	if err != nil {
		t.Fatalf("load retry comment: %v", err)
	}
	event, err := direct.loadCommentCreatedEvent(context.Background(), database.Pool, created.ID)
	if err != nil {
		t.Fatalf("load retry event: %v", err)
	}
	stale := ResolvedMention{Target: "role:reviewer", Delivery: "steer", SessionID: new("s1"), attemptSessionID: "s1"}
	mu.Lock()
	blockS1 = true
	sent = nil
	mu.Unlock()
	type deliveryResult struct {
		attempt model.CommentDelivery
		err     error
	}
	first := make(chan deliveryResult, 1)
	second := make(chan deliveryResult, 1)
	go func() {
		attempt, err := direct.deliverResolvedCommentMention(
			context.Background(), comment, event, stale, model.Actor{Kind: "user", ID: "alice"}, false,
		)
		first <- deliveryResult{attempt: attempt, err: err}
	}()
	<-started
	mu.Lock()
	roleHolder = "s2"
	mu.Unlock()
	go func() {
		attempt, err := direct.deliverResolvedCommentMention(
			context.Background(), comment, event, stale, model.Actor{Kind: "user", ID: "bob"}, false,
		)
		second <- deliveryResult{attempt: attempt, err: err}
	}()
	close(release)
	firstResult := <-first
	secondResult := <-second
	if firstResult.err != nil || secondResult.err != nil {
		t.Fatalf("concurrent retries: first=%#v second=%#v", firstResult, secondResult)
	}
	if firstResult.attempt.Attempt != 1 || firstResult.attempt.SessionID == nil || *firstResult.attempt.SessionID != "s1" {
		t.Fatalf("first retry = %#v, want original pending attempt for s1", firstResult.attempt)
	}
	if secondResult.attempt.Attempt != 2 || secondResult.attempt.SessionID == nil || *secondResult.attempt.SessionID != "s2" {
		t.Fatalf("second retry = %#v, want new attempt resolved to s2", secondResult.attempt)
	}
	mu.Lock()
	defer mu.Unlock()
	// Each retry sends exactly once, to the session its own attempt resolved. The two sends
	// no longer run one after the other: the claim transaction commits before the listener is
	// called, so which of them reaches the listener first is a race by design.
	sort.Strings(sent)
	if roleLookups != 2 || len(sent) != 2 || sent[0] != "s1" || sent[1] != "s2" {
		t.Fatalf("concurrent retry decisions = lookups:%d sends:%#v, want one send to the original s1 and one to the fresh s2", roleLookups, sent)
	}
}

// A sender that dies between committing its attempt and recording the outcome leaves the
// attempt pending and claimed. While that claim could still belong to a live sender a retry
// takes an attempt of its own; once it is plainly abandoned a retry resumes the original
// attempt, under its original number, so the listener deduplicates a send that did land.
func TestAbandonedMentionAttemptIsResumedUnderItsOriginalIdempotencyKey(t *testing.T) {
	keys := []string{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				IdempotencyKey string `json:"idempotency_key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode resumed send: %v", err)
			}
			keys = append(keys, request.IdempotencyKey)
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Abandoned mention attempt", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "The sender dies before it hears back.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	original := comment.ID + ":session:s1:1"
	if len(keys) != 1 || keys[0] != original {
		t.Fatalf("original send keys = %#v, want %q", keys, original)
	}

	// A sender committed attempt 1 and has not come back yet.
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set state = 'pending', envelope_id = null, error = null, claimed_at = now()
		where comment_id = $1 and target = 'session:s1' and attempt = 1
	`, comment.ID); err != nil {
		t.Fatalf("hold attempt 1: %v", err)
	}
	keys = nil
	held := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if held.Code != http.StatusCreated {
		t.Fatalf("retry beside a held attempt: status=%d body=%s", held.Code, held.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, held); attempt.Attempt != 2 || attempt.State != "sent" {
		t.Fatalf("retry beside a held attempt = %#v, want its own attempt 2", attempt)
	}
	if len(keys) != 1 || keys[0] != comment.ID+":session:s1:2" {
		t.Fatalf("held-attempt retry keys = %#v, want a second attempt's own key", keys)
	}

	// The holder never came back: the claim is old enough to be plainly abandoned.
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set claimed_at = now() - interval '5 minutes'
		where comment_id = $1 and target = 'session:s1' and attempt = 1
	`, comment.ID); err != nil {
		t.Fatalf("abandon attempt 1: %v", err)
	}
	keys = nil
	resumed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if resumed.Code != http.StatusCreated {
		t.Fatalf("resume the abandoned attempt: status=%d body=%s", resumed.Code, resumed.Body.String())
	}
	attempt := decodeBody[commentDeliveryRead](t, resumed)
	if attempt.Attempt != 1 || attempt.State != "sent" || attempt.Error != nil {
		t.Fatalf("resumed attempt = %#v, want the original attempt 1 completed", attempt)
	}
	if len(keys) != 1 || keys[0] != original {
		t.Fatalf("resumed send keys = %#v, want the original key %q so the listener deduplicates it", keys, original)
	}
	var attempts int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from comment_deliveries where comment_id = $1 and target = 'session:s1'
	`, comment.ID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want the original and the one taken beside its held claim", attempts)
	}
}

// A claim transaction commits its attempt as pending before anything is resolved, so a sender
// that never returns strands a row with no pinned session and no resolve error. That attempt was
// never sent: once its claim lapses the next retry must resolve it and send it, under its own
// number and idempotency key, not report it undeliverable without ever calling the listener.
func TestStrandedMentionAttemptIsResolvedRatherThanFailedUnsent(t *testing.T) {
	keys := []string{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				IdempotencyKey string `json:"idempotency_key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode stranded send: %v", err)
			}
			keys = append(keys, request.IdempotencyKey)
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Stranded mention attempt", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body":     "The client gives up while the listener is still answering.",
		"mentions": []map[string]any{{"target": "session:s1"}},
	}))

	// Exactly what openCommentDeliveryAttempt leaves behind when its sender never comes back:
	// pending, claimed, and carrying neither a session nor a resolve error. The claim is old
	// enough to be plainly abandoned.
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries
		set state = 'pending', session_id = null, envelope_id = null, error = null, resolve_error = null,
		    claimed_at = now() - interval '5 minutes'
		where comment_id = $1 and target = 'session:s1' and attempt = 1
	`, comment.ID); err != nil {
		t.Fatalf("strand attempt 1: %v", err)
	}
	keys = nil

	resumed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if resumed.Code != http.StatusCreated {
		t.Fatalf("resume the stranded attempt: status=%d body=%s", resumed.Code, resumed.Body.String())
	}
	attempt := decodeBody[commentDeliveryRead](t, resumed)
	if attempt.Attempt != 1 || attempt.State != "sent" || attempt.Error != nil ||
		attempt.SessionID == nil || *attempt.SessionID != "s1" {
		t.Fatalf("resumed stranded attempt = %#v, want attempt 1 sent to the live session s1", attempt)
	}
	original := comment.ID + ":session:s1:1"
	if len(keys) != 1 || keys[0] != original {
		t.Fatalf("stranded-attempt send keys = %#v, want one send under the original key %q", keys, original)
	}
	var attempts int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from comment_deliveries where comment_id = $1 and target = 'session:s1'
	`, comment.ID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want the stranded attempt resumed rather than a second one opened", attempts)
	}
}
