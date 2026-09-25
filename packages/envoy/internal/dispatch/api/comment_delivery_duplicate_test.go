package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// LEGION-271, acceptance 5. A comment mention's key was attempt-scoped exactly like a targeted
// message's, and its claim path resumes only a pending row, so a mention whose send landed and
// then missed the listener client's window was recorded failed and its retry reached the agent a
// second time. The key is now scoped to the mention's target and mode, so the stream recognises
// the repeat and the retry's row records it as a duplicate rather than a fresh delivery.
func TestCommentMentionThatLandedThenMissedTheReceiptIsNotDeliveredTwiceOnRetry(t *testing.T) {
	var listenerState struct {
		sync.Mutex
		keys      []string
		delivered map[string]struct{}
	}
	listenerState.delivered = map[string]struct{}{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				TargetSession  string `json:"target_session"`
				IdempotencyKey string `json:"idempotency_key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode mention send: %v", err)
			}
			listenerState.Lock()
			listenerState.keys = append(listenerState.keys, request.IdempotencyKey)
			first := len(listenerState.keys) == 1
			key := request.IdempotencyKey + "@" + request.TargetSession
			_, held := listenerState.delivered[key]
			listenerState.delivered[key] = struct{}{}
			listenerState.Unlock()
			if first {
				// The send lands; the answer misses the client's window.
				time.Sleep(400 * time.Millisecond)
			}
			if held {
				_, _ = w.Write([]byte(`{"event_id":"mention-envelope-2","recipient":"s1","duplicate":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL, 150*time.Millisecond)
	issue := createInteractionIssue(t, handler, "TEST", "A mention that landed then timed out", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "The mention landed; the answer did not.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))

	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry the failed mention: status=%d body=%s", retry.Code, retry.Body.String())
	}

	listenerState.Lock()
	keys := append([]string(nil), listenerState.keys...)
	delivered := len(listenerState.delivered)
	listenerState.Unlock()
	original := comment.ID + ":session:s1:steer"
	if len(keys) != 2 || keys[0] != original || keys[1] != original {
		t.Fatalf("mention send keys = %#v, want both sends under the target-and-mode key %q", keys, original)
	}
	if delivered != 1 {
		t.Fatalf("the listener delivered %d distinct keys %#v, want the agent to receive the mention once", delivered, keys)
	}

	var state string
	var duplicate bool
	if err := database.Pool.QueryRow(context.Background(), `
		select state, duplicate from comment_deliveries
		where comment_id = $1 and target = 'session:s1' order by attempt desc limit 1
	`, comment.ID).Scan(&state, &duplicate); err != nil {
		t.Fatalf("read the retried mention attempt: %v", err)
	}
	if state != "sent" || !duplicate {
		t.Fatalf("retried mention attempt = %s/duplicate=%v, want sent and duplicate", state, duplicate)
	}
}

// LEGION-271, acceptance 7's other half. A timeout resolving the target sent nothing, so its
// text must not tell a human the message may already have been delivered; only the send half
// carries that promise. deliveryErrorText is shared between the two, which is why this is its
// own test rather than a line in the one above.
func TestResolutionTimeoutDoesNotClaimTheMessageMayHaveLanded(t *testing.T) {
	held := make(chan struct{})
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/sessions" {
			<-held
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer func() {
		close(held)
		listener.Close()
	}()
	handler, _ := newTargetedMessageHandler(t, listener.URL, 150*time.Millisecond)
	issue := createInteractionIssue(t, handler, "TEST", "A resolution that timed out", "before")

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "The listener never answered the lookup.", "target": "session:s1", "delivery": "steer",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create targeted message: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	if len(message.Deliveries) != 1 || message.Deliveries[0].State != "failed" {
		t.Fatalf("attempt = %#v, want one attempt recorded failed", message.Deliveries)
	}
	failure := ""
	if message.Deliveries[0].Error != nil {
		failure = *message.Deliveries[0].Error
	}
	if strings.Contains(failure, "may already have been delivered") {
		t.Fatalf("resolution-timeout error = %q, but nothing was sent", failure)
	}
	if failure == "" {
		t.Fatalf("resolution-timeout attempt recorded no error at all")
	}
}
