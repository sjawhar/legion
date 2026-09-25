package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func TestMentionRepliesAddressEachDeliveredTarget(t *testing.T) {
	t.Run("different sessions", func(t *testing.T) {
		listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
				_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]},{"session_id":"s2","title":"reviewer","capabilities":["steer"]}]`))
			case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
				_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer listener.Close()
		handler, _ := newTargetedMessageHandler(t, listener.URL)
		issue := createInteractionIssue(t, handler, "TEST", "Mention reply targets", "before")
		comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
			"body":     "Both reviewers need to answer.",
			"mentions": []map[string]any{{"target": "session:s1"}, {"target": "session:s2"}},
		}))
		for _, target := range []string{"session:s1", "session:s2"} {
			sessionID := target[len("session:"):]
			response := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
				"actor": map[string]any{"kind": "session", "id": sessionID}, "target": target, "attempt": 1,
				"body": "Reply from " + sessionID,
			})
			if response.Code != http.StatusCreated {
				t.Fatalf("%s reply: status=%d body=%s", target, response.Code, response.Body.String())
			}
		}
	})

	t.Run("role and direct mention share a session", func(t *testing.T) {
		listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
				_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
			case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
				_, _ = w.Write([]byte(`{"role":"reviewer","holder":"s1","title":"planner","capabilities":["steer"]}`))
			case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
				_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer listener.Close()
		handler, _ := newTargetedMessageHandler(t, listener.URL)
		issue := createInteractionIssue(t, handler, "TEST", "Same-session mention reply targets", "before")
		comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
			"body":     "One session received two mention targets.",
			"mentions": []map[string]any{{"target": "role:reviewer"}, {"target": "session:s1"}},
		}))
		for _, target := range []string{"role:reviewer", "session:s1"} {
			response := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
				"actor": map[string]any{"kind": "session", "id": "s1"}, "target": target, "attempt": 1,
				"body": "Reply for " + target,
			})
			if response.Code != http.StatusCreated {
				t.Fatalf("%s reply: status=%d body=%s", target, response.Code, response.Body.String())
			}
		}
	})
}

func TestInitialMentionDeliveryDoesNotAppendAfterReplyConsumesPending(t *testing.T) {
	sent := []map[string]any{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode initial mention send: %v", err)
			}
			sent = append(sent, request)
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	direct := directServer(deps)
	issue := createInteractionIssue(t, handler, "TEST", "Answered pending mention", "before")
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Reply before sender updates.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set state = 'pending', envelope_id = null, error = null, claimed_at = null
		where comment_id = $1 and target = 'session:s1' and attempt = 1
	`, created.ID); err != nil {
		t.Fatalf("restore pending attempt: %v", err)
	}
	reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+created.ID+"/reply", map[string]any{
		"actor":   map[string]any{"kind": "session", "id": "s1"},
		"target":  "session:s1",
		"attempt": 1,
		"body":    "The frame arrived.",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("consume pending attempt: status=%d body=%s", reply.Code, reply.Body.String())
	}
	comment, err := direct.loadComment(context.Background(), database.Pool, created.ID)
	if err != nil {
		t.Fatalf("load comment for initial delivery: %v", err)
	}
	event, err := direct.loadCommentCreatedEvent(context.Background(), database.Pool, created.ID)
	if err != nil {
		t.Fatalf("load comment-created event: %v", err)
	}
	sent = nil
	attempt, err := direct.deliverResolvedCommentMention(
		context.Background(),
		comment,
		event,
		ResolvedMention{Target: "session:s1", Delivery: "steer", SessionID: new("s1"), attemptSessionID: "s1"},
		model.Actor{Kind: "user", ID: "alice"},
		true,
	)
	if err != nil {
		t.Fatalf("complete initial mention after its answer: %v", err)
	}
	if attempt.Attempt != 1 || attempt.ReplyID == nil || len(sent) != 0 {
		t.Fatalf("initial mention after answer = attempt:%#v sends:%#v, want its existing answered attempt and no second send", attempt, sent)
	}
	var attempts int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from comment_deliveries where comment_id = $1 and target = 'session:s1'
	`, created.ID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts after initial race: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts after initial race = %d, want 1", attempts)
	}
}

func TestInitialMentionDeliveryReturnsFailedAttemptAfterErrorReplyConsumesPending(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	direct := directServer(deps)
	issue := createInteractionIssue(t, handler, "TEST", "Failed answered pending mention", "before")
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Error reply before sender updates.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set state = 'pending', envelope_id = null, error = null, claimed_at = null
		where comment_id = $1 and target = 'session:s1' and attempt = 1
	`, created.ID); err != nil {
		t.Fatalf("restore pending attempt: %v", err)
	}
	reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+created.ID+"/reply", map[string]any{
		"actor":   map[string]any{"kind": "session", "id": "s1"},
		"target":  "session:s1",
		"attempt": 1,
		"error":   "The agent could not answer.",
	})
	if reply.Code != http.StatusOK {
		t.Fatalf("consume pending attempt with error: status=%d body=%s", reply.Code, reply.Body.String())
	}
	comment, err := direct.loadComment(context.Background(), database.Pool, created.ID)
	if err != nil {
		t.Fatalf("load comment for initial delivery: %v", err)
	}
	event, err := direct.loadCommentCreatedEvent(context.Background(), database.Pool, created.ID)
	if err != nil {
		t.Fatalf("load comment-created event: %v", err)
	}
	attempt, err := direct.deliverResolvedCommentMention(
		context.Background(),
		comment,
		event,
		ResolvedMention{Target: "session:s1", Delivery: "steer", SessionID: new("s1"), attemptSessionID: "s1"},
		model.Actor{Kind: "user", ID: "alice"},
		true,
	)
	if err != nil {
		t.Fatalf("complete initial mention after an error reply: %v", err)
	}
	if attempt.Attempt != 1 || attempt.State != "failed" || attempt.Error == nil ||
		*attempt.Error != "The agent could not answer." || attempt.ReplyID != nil {
		t.Fatalf("initial mention after error reply = %#v, want its terminal failed attempt", attempt)
	}
	var attempts int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from comment_deliveries where comment_id = $1 and target = 'session:s1'
	`, created.ID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts after error race: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts after error race = %d, want 1", attempts)
	}
}

// Two senders hold one attempt when the first outlives the claim lease and a retry resumes
// what it left pending. If a reply then settles that attempt with a body mid-send, the receipt
// the answered row owes belongs to the sender the claim names: the one whose claim lapsed pays
// nothing, or the attempt carries two receipts saying the same thing.
func TestASenderPastItsLeaseLeavesTheResumedAttemptsReceiptAlone(t *testing.T) {
	var handler http.Handler
	answerTheSend := false
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			if answerTheSend {
				var request struct {
					Payload string `json:"payload"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode mention send: %v", err)
				}
				var frame struct {
					Delivery struct {
						Attempt   int    `json:"attempt"`
						CommentID string `json:"comment_id"`
						Target    string `json:"target"`
					} `json:"delivery"`
				}
				if err := json.Unmarshal([]byte(request.Payload), &frame); err != nil {
					t.Errorf("decode mention delivery frame: %v", err)
				}
				reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+frame.Delivery.CommentID+"/reply", map[string]any{
					"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": frame.Delivery.Attempt,
					"target": frame.Delivery.Target, "body": "Answered before the resumed send returned.",
				})
				if reply.Code != http.StatusCreated {
					t.Errorf("reply from inside the resumed send: status=%d body=%s", reply.Code, reply.Body.String())
				}
			}
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	issue := createInteractionIssue(t, handler, "TEST", "Mention answered under a resumed claim", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Answer this while a lapsed sender is still out there.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))

	// Attempt 1 as a sender that claimed it two minutes ago and has not come back left it:
	// pending, and past the claim lease. lapsed is that sender's own claim, the token its
	// settle transaction carries.
	ctx := context.Background()
	var lapsed time.Time
	if err := database.Pool.QueryRow(ctx, `
		update comment_deliveries
		set state = 'pending', envelope_id = null, error = null, claimed_at = now() - interval '2 minutes'
		where comment_id = $1 and target = 'session:s1' and attempt = 1
		returning claimed_at
	`, comment.ID).Scan(&lapsed); err != nil {
		t.Fatalf("leave attempt 1 claimed by a stalled sender: %v", err)
	}

	// The retry resumes attempt 1 under a claim of its own, and the session answers its frame
	// mid-send, so that sender's settle transaction pays the receipt the answered row owes.
	answerTheSend = true
	resumed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if resumed.Code != http.StatusCreated {
		t.Fatalf("resume the lapsed attempt: status=%d body=%s", resumed.Code, resumed.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, resumed); attempt.Attempt != 1 {
		t.Fatalf("the retry took attempt %d, want the lapsed attempt 1 resumed", attempt.Attempt)
	}
	answerTheSend = false
	// Attempt 1 already carried the receipt its creation send recorded, which the reset above
	// left behind; the resumed send pays the one the reply left owing, and it is that one that
	// has to agree with the row.
	row := commentAttemptRow(t, database, comment.ID, "session:s1", 1)
	before := commentDeliveryReceipts(t, database, comment.ID, "session:s1", 1)
	if before != "sent///s1 | "+row {
		t.Fatalf("receipts after the resumed send = %q, want the creation receipt and the one %q the answered row owes", before, row)
	}

	// The stalled sender finally returns and settles the attempt it still thinks it holds.
	stalled := directServer(deps)
	stored, err := stalled.loadComment(ctx, database.Pool, comment.ID)
	if err != nil {
		t.Fatalf("load the comment: %v", err)
	}
	session := "s1"
	envelope := "stalled-envelope"
	settled, err := stalled.completeCommentDelivery(
		ctx, stored, model.CommentDelivery{
			CommentID: stored.ID, Target: "session:s1", Attempt: 1, Delivery: "steer", SessionID: &session,
		},
		model.Actor{Kind: "user", ID: "alice"}, attemptClaim{attempt: 1, claimedAt: lapsed}, &envelope, "",
	)
	if err != nil {
		t.Fatalf("the stalled sender's settle: %v", err)
	}
	if settled.State != "sent" || settled.ReplyID == nil {
		t.Fatalf("the stalled sender read back %#v, want the row the reply settled", settled)
	}

	if after := commentDeliveryReceipts(t, database, comment.ID, "session:s1", 1); after != before {
		t.Fatalf("receipts after the stalled sender returned = %q, want %q, unchanged", after, before)
	}
	if now := commentAttemptRow(t, database, comment.ID, "session:s1", 1); now != row {
		t.Fatalf("attempt row = %q, want %q: the stalled sender must not overwrite it", now, row)
	}
}

// A retry's attempt row is opened before anything is resolved, so what names the session that
// attempt is being sent to is the send itself rather than the comment that created the mention.
// The mentioned session answers the frame from inside that send, exactly as it does on the
// attempt the comment's creation opened; an attempt row that named nobody would refuse its own
// recipient's reply, and the attempt would carry no receipt at all.
func TestReplyDuringAMentionRetrySendStillRecordsTheDeliveryReceipt(t *testing.T) {
	var handler http.Handler
	answerTheSend := false
	replies := []int{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			if answerTheSend {
				var request struct {
					Payload string `json:"payload"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode mention send: %v", err)
				}
				var frame struct {
					Delivery struct {
						Attempt   int    `json:"attempt"`
						CommentID string `json:"comment_id"`
						Target    string `json:"target"`
					} `json:"delivery"`
				}
				if err := json.Unmarshal([]byte(request.Payload), &frame); err != nil {
					t.Errorf("decode mention delivery frame: %v", err)
				}
				reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+frame.Delivery.CommentID+"/reply", map[string]any{
					"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": frame.Delivery.Attempt,
					"target": frame.Delivery.Target, "body": "Answered before the retry's send returned.",
				})
				replies = append(replies, reply.Code)
			}
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Mention retry answered mid-send", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "The first send goes unanswered.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))

	// The retry opens attempt 2 beside the settled first one, and the session answers that
	// frame while the retry's send is still in flight.
	answerTheSend = true
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry the mention: status=%d body=%s", retry.Code, retry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, retry); attempt.Attempt != 2 {
		t.Fatalf("the retry took attempt %d, want a second attempt beside the first", attempt.Attempt)
	}
	if len(replies) != 1 || replies[0] != http.StatusCreated {
		t.Fatalf("the session's reply from inside the retry's send = %v, want exactly one 201", replies)
	}
	row := commentAttemptRow(t, database, comment.ID, "session:s1", 2)
	if !strings.HasPrefix(row, "sent//") || strings.HasPrefix(row, "sent///") {
		t.Fatalf("attempt 2 = %q, want the reply recorded against a sent attempt", row)
	}
	if receipts := commentDeliveryReceipts(t, database, comment.ID, "session:s1", 2); receipts != row {
		t.Fatalf("comment.delivery events for attempt 2 = %q, want the one receipt %q the answered row owes", receipts, row)
	}
}

// A sender that dies between its send and its settle leaves the attempt pending, and the session
// that frame reached has to be able to answer it still: the row names that session before the
// send, so the answer is accepted whether or not the sender ever comes back. The attempt's own
// receipt is owed by the sender that died and nobody else pays it, but the answer is not lost
// and the reply is not refused for the life of the comment.
func TestASessionAnswersAnAttemptStrandedBetweenItsSendAndItsSettle(t *testing.T) {
	var listenerState struct {
		sync.Mutex
		sends int
	}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			listenerState.Lock()
			listenerState.sends++
			stall := listenerState.sends == 2
			listenerState.Unlock()
			if stall {
				// The retry's client gives up while its send is still in flight, so that
				// process never reaches its settle transaction.
				time.Sleep(1500 * time.Millisecond)
			}
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Mention stranded after its send", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "The retry's sender will not come back.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))

	abandoned, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	payload, err := json.Marshal(map[string]any{"delivery": "steer"})
	if err != nil {
		t.Fatalf("encode retry: %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", bytes.NewReader(payload),
	).WithContext(abandoned)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code == http.StatusCreated {
		t.Fatalf("the 700ms client should not have seen the 1.5s send complete: body=%s", recorder.Body.String())
	}
	if stranded := commentAttemptRow(t, database, comment.ID, "session:s1", 2); stranded != "pending///s1" {
		t.Fatalf("attempt 2 = %q, want it left pending by the sender that never returned", stranded)
	}

	// The frame did reach the session, and it answers the attempt its sender abandoned.
	reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 2,
		"target": "session:s1", "body": "It arrived before that sender died.",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("the recipient's reply to the stranded attempt: status=%d body=%s", reply.Code, reply.Body.String())
	}
	if row := commentAttemptRow(t, database, comment.ID, "session:s1", 2); !strings.HasPrefix(row, "sent//") || strings.HasPrefix(row, "sent///") {
		t.Fatalf("attempt 2 = %q, want the answer recorded against it", row)
	}
}

// commentDeliveryReceipts is every comment.delivery event recorded for one attempt of one of a
// comment's mention targets, oldest first and pipe-separated, each rendered the way
// commentAttemptRow renders the row itself - the session the receipt names included, so a
// receipt that disagrees with its own row about who holds the frame fails wherever the two are
// compared.
func commentDeliveryReceipts(t *testing.T, database *store.Store, commentID, target string, attempt int) string {
	t.Helper()
	var receipts string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(string_agg(
			(payload ->> 'state') || '/' || coalesce(payload ->> 'error', '') || '/' ||
			coalesce(payload ->> 'reply_id', '') || '/' || coalesce(payload ->> 'session_id', ''),
			' | ' order by id
		), '')
		from events
		where type = 'comment.delivery' and payload ->> 'comment_id' = $1
		  and payload ->> 'target' = $2 and (payload ->> 'attempt')::int = $3
	`, commentID, target, attempt).Scan(&receipts); err != nil {
		t.Fatalf("read delivery receipts: %v", err)
	}
	return receipts
}

// commentAttemptRow renders one attempt of one of a comment's mention targets the way a receipt
// for it reads, so the two can be compared directly.
func commentAttemptRow(t *testing.T, database *store.Store, commentID, target string, attempt int) string {
	t.Helper()
	var row string
	if err := database.Pool.QueryRow(context.Background(), `
		select state || '/' || coalesce(error, '') || '/' || coalesce(reply_id::text, '') || '/' ||
			coalesce(session_id, '')
		from comment_deliveries where comment_id = $1 and target = $2 and attempt = $3
	`, commentID, target, attempt).Scan(&row); err != nil {
		t.Fatalf("read attempt row: %v", err)
	}
	return row
}
