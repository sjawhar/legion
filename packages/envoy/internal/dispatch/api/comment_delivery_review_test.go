package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
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
		update comment_deliveries set state = 'pending', envelope_id = null, error = null
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

func TestPendingRoleMentionRetryKeepsTheCreateTimeHolder(t *testing.T) {
	roleHolder := "s1"
	s1Live := true
	roleLookups := 0
	sent := []map[string]any{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			roleLookups++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"role": "reviewer", "holder": roleHolder, "title": "reviewer", "capabilities": []string{"steer"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			if !s1Live {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode role mention send: %v", err)
			}
			sent = append(sent, request)
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Pending role mention", "before")
	patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"route": "role:reviewer"}, "alice")
	if patched.Code != http.StatusOK {
		t.Fatalf("set reviewer route: status=%d body=%s", patched.Code, patched.Body.String())
	}
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Keep this holder.", "mentions": []map[string]any{{"target": "role:reviewer"}},
	}))
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set state = 'pending', envelope_id = null, error = null
		where comment_id = $1 and target = 'role:reviewer' and attempt = 1
	`, comment.ID); err != nil {
		t.Fatalf("restore pending role delivery: %v", err)
	}
	roleHolder = "s2"
	sent = nil
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry pending role mention: status=%d body=%s", retry.Code, retry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, retry); attempt.Attempt != 1 || attempt.SessionID == nil || *attempt.SessionID != "s1" || attempt.State != "sent" {
		t.Fatalf("pending role retry attempt = %#v, want original-holder attempt 1", attempt)
	}
	if roleLookups != 1 || len(sent) != 1 || sent[0]["target_session"] != "s1" {
		t.Fatalf("pending role retry = lookups:%d sends:%#v, want no role re-resolution or s2 delivery", roleLookups, sent)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set state = 'pending', envelope_id = null, error = null
		where comment_id = $1 and target = 'role:reviewer' and attempt = 1
	`, comment.ID); err != nil {
		t.Fatalf("restore pending disappeared-holder delivery: %v", err)
	}
	s1Live = false
	sent = nil
	missingHolder := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if missingHolder.Code != http.StatusCreated {
		t.Fatalf("complete pending absent-holder mention: status=%d body=%s", missingHolder.Code, missingHolder.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, missingHolder); attempt.Attempt != 1 || attempt.SessionID != nil ||
		attempt.State != "failed" || attempt.Error == nil || *attempt.Error != "no live session s1" {
		t.Fatalf("absent original holder retry = %#v, want a failed no-live-session attempt", attempt)
	}
	if roleLookups != 1 || len(sent) != 0 {
		t.Fatalf("absent original holder retry = lookups:%d sends:%#v, want no role re-resolution and no send attempt", roleLookups, sent)
	}
}

func newDirectMentionDeliveryServer(t *testing.T, envoyURL string) (*server, http.Handler, *store.Store) {
	t.Helper()
	database := openEmptyTestStore(t)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{
		Store: database, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	allowed := map[string]struct{}{"alice": {}, "bob": {}}
	deps, err := NewDeps(DepsInput{
		Store:           database,
		Identity:        identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: allowed},
		AllowedLogins:   allowed,
		AgentToken:      "agent-token",
		RepoProjectsRaw: "owner/repo=TEST",
		ServerURL:       "https://dispatch.example",
		EnvoyURL:        envoyURL,
		Docs:            documentService,
		Events:          broker,
	})
	if err != nil {
		t.Fatalf("new mention delivery dependencies: %v", err)
	}
	direct := &server{deps: deps}
	mux := http.NewServeMux()
	Register(mux, deps)
	return direct, mux, database
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
	direct, handler, database := newDirectMentionDeliveryServer(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Answered pending mention", "before")
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Reply before sender updates.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set state = 'pending', envelope_id = null, error = null
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

func TestPendingMentionRetryKeepsItsOriginalDeliveryMode(t *testing.T) {
	sentModes := []string{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["btw","steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				Payload string `json:"payload"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode mention mode send: %v", err)
			}
			var frame struct {
				Delivery struct {
					Mode string `json:"mode"`
				} `json:"delivery"`
			}
			if err := json.Unmarshal([]byte(request.Payload), &frame); err != nil {
				t.Fatalf("decode mention mode frame: %v", err)
			}
			sentModes = append(sentModes, frame.Delivery.Mode)
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Pending mention mode", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Keep the original mode.", "mentions": []map[string]any{{"target": "session:s1"}}, "delivery": "btw",
	}))
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set state = 'pending', envelope_id = null, error = null
		where comment_id = $1 and target = 'session:s1' and attempt = 1
	`, comment.ID); err != nil {
		t.Fatalf("restore pending BTW attempt: %v", err)
	}
	sentModes = nil
	completePending := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if completePending.Code != http.StatusCreated {
		t.Fatalf("complete pending BTW attempt: status=%d body=%s", completePending.Code, completePending.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, completePending); attempt.Attempt != 1 || attempt.Delivery != "btw" || attempt.State != "sent" {
		t.Fatalf("completed pending attempt = %#v, want the stored BTW attempt", attempt)
	}
	if len(sentModes) != 1 || sentModes[0] != "btw" {
		t.Fatalf("completed pending modes = %#v, want [btw]", sentModes)
	}
	newRetry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if newRetry.Code != http.StatusCreated {
		t.Fatalf("create fresh steer retry: status=%d body=%s", newRetry.Code, newRetry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, newRetry); attempt.Attempt != 2 || attempt.Delivery != "steer" || attempt.State != "sent" {
		t.Fatalf("fresh retry attempt = %#v, want a second steer attempt", attempt)
	}
	if len(sentModes) != 2 || sentModes[1] != "steer" {
		t.Fatalf("sent modes = %#v, want [btw steer]", sentModes)
	}
}

// TestPendingMentionRetryRechecksCapabilityAgainstCurrentRegistration proves a retry that
// completes a still-pending attempt refuses when the pinned session has since lost the
// requested delivery mode. The comment is created while s1 advertises steer and the send is
// deliberately held pending; before the retry runs, s1's live registration drops steer. A
// human retrying this delivery must see the identical refusal a fresh send would produce, not
// a stale "sent" recorded from a capability check that ran before the session's registration
// changed. This is the observable contract createCommentDelivery must honor for every retry.
func TestPendingMentionRetryRechecksCapabilityAgainstCurrentRegistration(t *testing.T) {
	capabilities := []string{"steer"}
	sessionLookups := 0
	sends := 0
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			sessionLookups++
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"session_id": "s1", "title": "planner", "capabilities": capabilities},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			sends++
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Pending mention capability drift", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Created while steer was advertised.", "mentions": []map[string]any{{"target": "session:s1"}}, "delivery": "steer",
	}))
	if len(comment.Deliveries) != 1 || comment.Deliveries[0].State != "sent" {
		t.Fatalf("initial delivery = %#v, want sent", comment.Deliveries)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set state = 'pending', envelope_id = null, error = null
		where comment_id = $1 and target = 'session:s1' and attempt = 1
	`, comment.ID); err != nil {
		t.Fatalf("restore pending delivery: %v", err)
	}
	capabilities = []string{}
	sends = 0
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry pending delivery: status=%d body=%s", retry.Code, retry.Body.String())
	}
	attempt := decodeBody[commentDeliveryRead](t, retry)
	if attempt.Attempt != 1 || attempt.State != "failed" || attempt.Error == nil ||
		*attempt.Error != "session s1 (planner) does not advertise steer" || sends != 0 {
		t.Fatalf("pending retry reused stale capability: lookups=%d sends=%d attempt=%#v", sessionLookups, sends, attempt)
	}
}

func TestPendingUnresolvedMentionRetryKeepsTheOriginalError(t *testing.T) {
	roleAvailable := false
	roleLookups := 0
	sends := 0
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			roleLookups++
			if !roleAvailable {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no holder for role reviewer"}`))
				return
			}
			_, _ = w.Write([]byte(`{"role":"reviewer","holder":"s2","title":"replacement","capabilities":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			sends++
			_, _ = w.Write([]byte(`{"event_id":"unexpected-send","recipient":"s2"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Pending unresolved mention", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Keep the original resolution error.", "mentions": []map[string]any{{"target": "role:reviewer"}},
	}))
	if len(comment.Deliveries) != 1 || comment.Deliveries[0].Error == nil ||
		*comment.Deliveries[0].Error != "no holder for role reviewer" {
		t.Fatalf("initial unresolved delivery = %#v", comment.Deliveries)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set state = 'pending', envelope_id = null, error = null
		where comment_id = $1 and target = 'role:reviewer' and attempt = 1
	`, comment.ID); err != nil {
		t.Fatalf("restore pending unresolved delivery: %v", err)
	}
	roleAvailable = true
	sends = 0
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("complete pending unresolved attempt: status=%d body=%s", retry.Code, retry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, retry); attempt.Attempt != 1 || attempt.SessionID != nil ||
		attempt.Delivery != "steer" || attempt.State != "failed" || attempt.Error == nil ||
		*attempt.Error != "no holder for role reviewer" {
		t.Fatalf("pending unresolved retry = %#v, want the original failed resolution", attempt)
	}
	if roleLookups != 1 || sends != 0 {
		t.Fatalf("pending unresolved retry = lookups:%d sends:%d, want no fresh decisions or sends", roleLookups, sends)
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
	direct, handler, database := newDirectMentionDeliveryServer(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Failed answered pending mention", "before")
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Error reply before sender updates.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set state = 'pending', envelope_id = null, error = null
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
	direct, handler, database := newDirectMentionDeliveryServer(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Concurrent mention retry", "before")
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Retry with the new holder after the original completes.", "mentions": []map[string]any{{"target": "role:reviewer"}},
	}))
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set state = 'pending', envelope_id = null, error = null
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
	if roleLookups != 2 || len(sent) != 2 || sent[0] != "s1" || sent[1] != "s2" {
		t.Fatalf("concurrent retry decisions = lookups:%d sends:%#v, want original s1 then fresh s2", roleLookups, sent)
	}
}
