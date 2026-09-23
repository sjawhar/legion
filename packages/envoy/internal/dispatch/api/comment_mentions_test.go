package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type commentMentionRead struct {
	Target    string  `json:"target"`
	Delivery  string  `json:"delivery"`
	SessionID *string `json:"session_id"`
}

type commentDeliveryRead struct {
	CommentID string  `json:"comment_id"`
	Target    string  `json:"target"`
	Attempt   int     `json:"attempt"`
	Delivery  string  `json:"delivery"`
	SessionID *string `json:"session_id"`
	State     string  `json:"state"`
	Error     *string `json:"error"`
	ReplyID   *string `json:"reply_id"`
}

type mentionedCommentRead struct {
	ID         string                `json:"id"`
	Body       string                `json:"body"`
	ReplyTo    *string               `json:"reply_to"`
	Mentions   []commentMentionRead  `json:"mentions"`
	Deliveries []commentDeliveryRead `json:"deliveries"`
}

func postMentionedComment(t *testing.T, handler http.Handler, issueKey string, input map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", input, "alice")
}

func decodeMentionedComment(t *testing.T, response *httptest.ResponseRecorder) mentionedCommentRead {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("create mentioned comment: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[mentionedCommentRead](t, response)
}

func TestMentionedCommentRecordsFailedOfflineSession(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/sessions" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Offline mention", "before")

	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Please investigate this.", "mentions": []map[string]any{{"target": "session:gone"}},
	}))
	if len(created.Mentions) != 1 || created.Mentions[0].SessionID != nil {
		t.Fatalf("offline mention = %#v, want an unresolved mention", created.Mentions)
	}
	if len(created.Deliveries) != 1 || created.Deliveries[0].State != "failed" ||
		created.Deliveries[0].SessionID != nil ||
		created.Deliveries[0].Error == nil || *created.Deliveries[0].Error != "no live session gone" {
		t.Fatalf("offline delivery = %#v, want a failed delivery with no resolved session", created.Deliveries)
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), `"type":"comment.delivery"`) ||
		!strings.Contains(events.Body.String(), `"state":"failed"`) {
		t.Fatalf("offline mention events: status=%d body=%s", events.Code, events.Body.String())
	}
}

func TestMentionedCommentUsesRequestedOrDefaultDeliveryMode(t *testing.T) {
	capabilities := []string{"btw", "steer"}
	sent := []map[string]any{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"session_id": "s1", "title": "planner", "capabilities": capabilities,
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatalf("decode mention send: %v", err)
			}
			sent = append(sent, input)
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Mention mode", "before")

	defaultMode := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Default mode.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if len(defaultMode.Mentions) != 1 || defaultMode.Mentions[0].Delivery != "steer" ||
		len(defaultMode.Deliveries) != 1 || defaultMode.Deliveries[0].Delivery != "steer" ||
		defaultMode.Deliveries[0].State != "sent" {
		t.Fatalf("default mode result = %#v", defaultMode)
	}
	btwMode := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "BTW mode.", "mentions": []map[string]any{{"target": "session:s1"}}, "delivery": "btw",
	}))
	if len(btwMode.Deliveries) != 1 || btwMode.Deliveries[0].Delivery != "btw" || btwMode.Deliveries[0].State != "sent" {
		t.Fatalf("btw mode result = %#v", btwMode)
	}
	if len(sent) != 2 {
		t.Fatalf("mention sends = %#v, want default and btw sends", sent)
	}
	var frame struct {
		Delivery struct {
			Mode string `json:"mode"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal([]byte(sent[1]["payload"].(string)), &frame); err != nil {
		t.Fatalf("decode BTW frame: %v", err)
	}
	if frame.Delivery.Mode != "btw" {
		t.Fatalf("BTW frame mode = %q, want btw", frame.Delivery.Mode)
	}
	capabilities = []string{"btw"}
	unadvertisedSteer := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Unavailable steer mode.", "mentions": []map[string]any{{"target": "session:s1"}}, "delivery": "steer",
	}))
	if len(unadvertisedSteer.Deliveries) != 1 || unadvertisedSteer.Deliveries[0].State != "failed" ||
		unadvertisedSteer.Deliveries[0].Error == nil ||
		*unadvertisedSteer.Deliveries[0].Error != "session s1 (planner) does not advertise steer" {
		t.Fatalf("unadvertised steer result = %#v", unadvertisedSteer.Deliveries)
	}

	capabilities = nil
	unadvertised := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Unavailable BTW mode.", "mentions": []map[string]any{{"target": "session:s1"}}, "delivery": "btw",
	}))
	if len(unadvertised.Deliveries) != 1 || unadvertised.Deliveries[0].State != "failed" ||
		unadvertised.Deliveries[0].Error == nil || *unadvertised.Deliveries[0].Error != "session s1 (planner) does not advertise btw" {
		t.Fatalf("unadvertised mode result = %#v", unadvertised.Deliveries)
	}
}

func TestMentionedCommentRejectsInvalidMentionInput(t *testing.T) {
	handler, _ := newTargetedMessageHandler(t, "")
	issue := createInteractionIssue(t, handler, "TEST", "Mention validation", "before")
	tooMany := make([]map[string]any, 11)
	for index := range tooMany {
		tooMany[index] = map[string]any{"target": fmt.Sprintf("session:s%d", index)}
	}
	for _, tc := range []struct {
		name  string
		input map[string]any
	}{
		{"invalid target", map[string]any{"body": "Bad target.", "mentions": []map[string]any{{"target": "agent:s1"}}}},
		{"duplicate target", map[string]any{"body": "Duplicate target.", "mentions": []map[string]any{{"target": "session:s1"}, {"target": "session:s1"}}}},
		{"more than ten targets", map[string]any{"body": "Too many targets.", "mentions": tooMany}},
		{"delivery without target", map[string]any{"body": "No target.", "delivery": "btw"}},
		{"invalid delivery", map[string]any{"body": "Bad mode.", "mentions": []map[string]any{{"target": "session:s1"}}, "delivery": "interrupt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := postMentionedComment(t, handler, issue.Key, tc.input)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"MENTION_INPUT"`) {
				t.Fatalf("%s: status=%d body=%s", tc.name, response.Code, response.Body.String())
			}
		})
	}
}

func TestMentionedCommentPinsRoleHolderBeforeSending(t *testing.T) {
	roleHolder := "s1"
	roleLookups := 0
	sent := []map[string]any{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			roleLookups++
			holder := roleHolder
			roleHolder = "s2"
			_ = json.NewEncoder(w).Encode(map[string]any{
				"role": "reviewer", "holder": holder, "title": "reviewer", "capabilities": []string{"btw", "steer"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["btw","steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatalf("decode role mention send: %v", err)
			}
			sent = append(sent, input)
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Pinned mention", "before")
	patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"route": "role:reviewer"}, "alice")
	if patched.Code != http.StatusOK {
		t.Fatalf("set route: status=%d body=%s", patched.Code, patched.Body.String())
	}

	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Review this exact version.", "mentions": []map[string]any{{"target": "role:reviewer"}},
	}))
	if roleLookups != 1 || roleHolder != "s2" || len(created.Mentions) != 1 || created.Mentions[0].SessionID == nil ||
		*created.Mentions[0].SessionID != "s1" || len(sent) != 1 || sent[0]["target_session"] != "s1" {
		t.Fatalf("role resolution/sends = holder:%s lookups:%d mentions:%#v sends:%#v, want one pinned s1 delivery after role moves to s2", roleHolder, roleLookups, created.Mentions, sent)
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), `"suppress_route":true`) ||
		!strings.Contains(events.Body.String(), `"suppressed_route":"role:reviewer"`) ||
		!strings.Contains(events.Body.String(), `"suppressed_route_session_id":"s1"`) {
		t.Fatalf("pinned mention payload: status=%d body=%s", events.Code, events.Body.String())
	}
}

func TestMentionReplyRequiresItsAttemptTargetAndIsIdempotent(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["btw","steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Mention reply", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?", "mentions": []map[string]any{{"target": "session:s1"}},
	}))

	human := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Human cannot answer.",
	}, "alice")
	if human.Code != http.StatusForbidden || !strings.Contains(human.Body.String(), `"code":"REPLY_FORBIDDEN"`) {
		t.Fatalf("human comment reply: status=%d body=%s", human.Code, human.Body.String())
	}
	wrongSession := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s2"}, "attempt": 1, "body": "Wrong session.",
	})
	if wrongSession.Code != http.StatusForbidden || !strings.Contains(wrongSession.Body.String(), `"code":"REPLY_FORBIDDEN"`) {
		t.Fatalf("wrong-session comment reply: status=%d body=%s", wrongSession.Code, wrongSession.Body.String())
	}
	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Yes, it can ship.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("answer mention: status=%d body=%s", answered.Code, answered.Body.String())
	}
	answer := decodeBody[mentionedCommentRead](t, answered)
	if answer.ReplyTo == nil || *answer.ReplyTo != comment.ID || answer.Body != "Yes, it can ship." {
		t.Fatalf("answered comment = %#v", answer)
	}
	repeated := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "A duplicate answer.",
	})
	if repeated.Code != http.StatusOK || decodeBody[mentionedCommentRead](t, repeated).ID != answer.ID {
		t.Fatalf("repeated answer: status=%d body=%s", repeated.Code, repeated.Body.String())
	}
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated || decodeBody[commentDeliveryRead](t, retry).Attempt != 2 {
		t.Fatalf("create stale reply attempt: status=%d body=%s", retry.Code, retry.Body.String())
	}
	staleAnswer := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 2, "body": "The stale attempt also arrived.",
	})
	if staleAnswer.Code != http.StatusCreated {
		t.Fatalf("answer stale mention attempt: status=%d body=%s", staleAnswer.Code, staleAnswer.Body.String())
	}
	if stale := decodeBody[mentionedCommentRead](t, staleAnswer); stale.ID == answer.ID || stale.ReplyTo == nil || *stale.ReplyTo != comment.ID {
		t.Fatalf("stale attempt answer = %#v, want its own reply to %s", stale, comment.ID)
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), `"type":"comment.answered"`) {
		t.Fatalf("answer events: status=%d body=%s", events.Code, events.Body.String())
	}
}

func TestMentionReplyErrorRecordsAttemptWithoutComment(t *testing.T) {
	live := true
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/sessions" {
			if live {
				_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send" {
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Mention error reply", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Are you there?", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	failure := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "error": "No active model.",
	})
	if failure.Code != http.StatusOK {
		t.Fatalf("report mention failure: status=%d body=%s", failure.Code, failure.Body.String())
	}
	attempt := decodeBody[commentDeliveryRead](t, failure)
	if attempt.State != "failed" || attempt.Error == nil || *attempt.Error != "No active model." || attempt.ReplyID != nil {
		t.Fatalf("failed answer attempt = %#v", attempt)
	}
	reloaded := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
	if reloaded.Code != http.StatusOK {
		t.Fatalf("reload comment after failed answer: status=%d body=%s", reloaded.Code, reloaded.Body.String())
	}
	var thread struct {
		Comment mentionedCommentRead   `json:"comment"`
		Replies []mentionedCommentRead `json:"replies"`
	}
	if err := json.Unmarshal(reloaded.Body.Bytes(), &thread); err != nil {
		t.Fatalf("decode reloaded comment: %v", err)
	}
	if len(thread.Replies) != 0 || len(thread.Comment.Deliveries) != 1 || thread.Comment.Deliveries[0].Error == nil ||
		*thread.Comment.Deliveries[0].Error != "No active model." {
		t.Fatalf("failed answer readback = %#v", thread)
	}
}

func TestMentionRetrySelectsOneTargetAndReresolvesIt(t *testing.T) {
	live := false
	roleHolder := "s1"
	sent := []map[string]any{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			if live {
				_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["btw","steer"]},{"session_id":"s2","title":"reviewer","capabilities":["btw","steer"]}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			if roleHolder == "" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no holder for role reviewer"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"role": "reviewer", "holder": roleHolder, "title": "reviewer", "capabilities": []string{"btw", "steer"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatalf("decode retry send: %v", err)
			}
			sent = append(sent, input)
			_, _ = w.Write([]byte(`{"event_id":"retry-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Mention retry", "before")
	single := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Retry a single mention.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if len(single.Deliveries) != 1 || single.Deliveries[0].State != "failed" {
		t.Fatalf("initial retry candidate = %#v", single.Deliveries)
	}
	live = true
	humanRetry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+single.ID+"/deliveries", map[string]any{"delivery": "steer"}, "alice")
	if humanRetry.Code != http.StatusCreated {
		t.Fatalf("human retry: status=%d body=%s", humanRetry.Code, humanRetry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, humanRetry); attempt.Attempt != 2 || attempt.Target != "session:s1" || attempt.State != "sent" {
		t.Fatalf("human retry attempt = %#v", attempt)
	}
	bearerRetry := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+single.ID+"/deliveries", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s2"}, "delivery": "btw",
	})
	if bearerRetry.Code != http.StatusCreated {
		t.Fatalf("bearer retry: status=%d body=%s", bearerRetry.Code, bearerRetry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, bearerRetry); attempt.Attempt != 3 || attempt.Delivery != "btw" || attempt.State != "sent" {
		t.Fatalf("bearer retry attempt = %#v", attempt)
	}

	multiple := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Select exactly one retry target.", "mentions": []map[string]any{{"target": "session:s1"}, {"target": "session:s2"}},
	}))
	missingTarget := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+multiple.ID+"/deliveries", map[string]any{"delivery": "steer"}, "alice")
	if missingTarget.Code != http.StatusBadRequest || !strings.Contains(missingTarget.Body.String(), `"code":"MENTION_INPUT"`) {
		t.Fatalf("multi-target retry without target: status=%d body=%s", missingTarget.Code, missingTarget.Body.String())
	}
	namedRetry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+multiple.ID+"/deliveries", map[string]any{
		"target": "session:s2", "delivery": "steer",
	}, "alice")
	if namedRetry.Code != http.StatusCreated {
		t.Fatalf("named retry: status=%d body=%s", namedRetry.Code, namedRetry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, namedRetry); attempt.Target != "session:s2" || attempt.Attempt != 2 || attempt.SessionID == nil || *attempt.SessionID != "s2" {
		t.Fatalf("named retry attempt = %#v", attempt)
	}

	role := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Retry current role holder.", "mentions": []map[string]any{{"target": "role:reviewer"}},
	}))
	roleHolder = "s2"
	roleRetry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+role.ID+"/deliveries", map[string]any{"delivery": "steer"}, "alice")
	if roleRetry.Code != http.StatusCreated {
		t.Fatalf("role retry: status=%d body=%s", roleRetry.Code, roleRetry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, roleRetry); attempt.SessionID == nil || *attempt.SessionID != "s2" || attempt.State != "sent" {
		t.Fatalf("role retry attempt = %#v", attempt)
	}
	roleHolder = ""
	noHolderRetry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+role.ID+"/deliveries", map[string]any{"delivery": "steer"}, "alice")
	if noHolderRetry.Code != http.StatusCreated {
		t.Fatalf("no-holder retry: status=%d body=%s", noHolderRetry.Code, noHolderRetry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, noHolderRetry); attempt.SessionID != nil || attempt.State != "failed" ||
		attempt.Error == nil || *attempt.Error != "no holder for role reviewer" {
		t.Fatalf("no-holder retry attempt = %#v", attempt)
	}
}

func TestCommentMentionReadProjectionIncludesReplyChain(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"projection-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Mention projection", "before")
	root := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Root mention.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	reply := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Reply mention.", "reply_to": root.ID, "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if reply.ReplyTo == nil || *reply.ReplyTo != root.ID {
		t.Fatalf("reply mention = %#v", reply)
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+root.ID, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read mention thread: status=%d body=%s", response.Code, response.Body.String())
	}
	var thread struct {
		Comment mentionedCommentRead   `json:"comment"`
		Replies []mentionedCommentRead `json:"replies"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &thread); err != nil {
		t.Fatalf("decode mention thread: %v", err)
	}
	if len(thread.Comment.Mentions) != 1 || len(thread.Comment.Deliveries) != 1 ||
		len(thread.Replies) != 1 || thread.Replies[0].ID != reply.ID || len(thread.Replies[0].Mentions) != 1 ||
		len(thread.Replies[0].Deliveries) != 1 {
		t.Fatalf("mention thread projection = %#v", thread)
	}
}

func TestMentionedCommentDoesNotReresolveMissingRoleHolder(t *testing.T) {
	roleLookups := 0
	sends := 0
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			roleLookups++
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no holder for role reviewer"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			sends++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Missing role holder", "before")
	patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"route": "role:reviewer"}, "alice")
	if patched.Code != http.StatusOK {
		t.Fatalf("set role route: status=%d body=%s", patched.Code, patched.Body.String())
	}

	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "No holder should remain unresolved.", "mentions": []map[string]any{{"target": "role:reviewer"}},
	}))
	if roleLookups != 1 || sends != 0 || len(created.Mentions) != 1 || created.Mentions[0].SessionID != nil ||
		len(created.Deliveries) != 1 || created.Deliveries[0].SessionID != nil || created.Deliveries[0].State != "failed" {
		t.Fatalf("missing-holder mention = lookups:%d sends:%d comment:%#v, want one unresolved failed attempt", roleLookups, sends, created)
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), `"suppress_route":false`) {
		t.Fatalf("missing-holder payload: status=%d body=%s", events.Code, events.Body.String())
	}
}

// A mention inside an ask's thread is delivered to a session, and that session answers through
// the delivery callback. Both the receipt and the answer have to name the ask: the ask card and
// the Inbox row select every reply on ask_id, and the stream keys its refresh on the event's
// own ask_id.
func TestCallbackReplyUnderAnAskJoinsTheAskThread(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["btw","steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Ask thread callback", "before")
	askResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which approach?", "actor": sessionActor(),
	})
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[model.Ask](t, askResponse)
	clarification := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Say more, @session:s1.", "ask_id": ask.ID,
		"mentions": []map[string]any{{"target": "session:s1"}},
	}))

	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+clarification.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "The second approach.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("callback reply: status=%d body=%s", answered.Code, answered.Body.String())
	}
	reply := decodeBody[model.Comment](t, answered)
	if reply.AskID == nil || *reply.AskID != ask.ID || reply.ReplyTo != nil {
		t.Fatalf("callback reply ask_id=%#v reply_to=%#v, want the ask and no reply_to", reply.AskID, reply.ReplyTo)
	}
	if reply.Turn == nil || *reply.Turn != "human" {
		t.Fatalf("callback reply turn = %#v, want human", reply.Turn)
	}

	thread := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice")
	if thread.Code != http.StatusOK {
		t.Fatalf("read ask thread: status=%d body=%s", thread.Code, thread.Body.String())
	}
	read := decodeBody[struct {
		Replies []model.Comment `json:"replies"`
	}](t, thread)
	if len(read.Replies) != 2 || read.Replies[1].ID != reply.ID {
		t.Fatalf("ask replies = %#v, want the clarification then the callback reply", read.Replies)
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", events.Code, events.Body.String())
	}
	var log []struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.NewDecoder(events.Body).Decode(&log); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	named := map[string]bool{}
	for _, entry := range log {
		if entry.Type == "comment.delivery" || entry.Type == "comment.answered" {
			named[entry.Type] = entry.Payload["ask_id"] == ask.ID
		}
	}
	if !named["comment.delivery"] || !named["comment.answered"] {
		t.Fatalf("events naming the ask = %#v, want comment.delivery and comment.answered both naming %s", named, ask.ID)
	}
}
