package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
		update comment_deliveries set state = 'pending', envelope_id = null, error = null, claimed_at = null
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
		update comment_deliveries set state = 'pending', envelope_id = null, error = null, claimed_at = null
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
	if attempt := decodeBody[commentDeliveryRead](t, missingHolder); attempt.Attempt != 1 || attempt.SessionID == nil ||
		*attempt.SessionID != "s1" || attempt.State != "failed" || attempt.Error == nil || *attempt.Error != "no live session s1" {
		t.Fatalf("absent original holder retry = %#v, want the pin kept on a failed no-live-session attempt", attempt)
	}
	if roleLookups != 1 || len(sent) != 0 {
		t.Fatalf("absent original holder retry = lookups:%d sends:%#v, want no role re-resolution and no send attempt", roleLookups, sent)
	}
}

// TestStrandedAttemptRetriedDuringAListenerOutageKeepsThePinAndAcceptsTheReply is Deep1296b's
// case A: a stranded attempt already pinned to its recipient is retried while the listener
// cannot answer whether that recipient still lives, so the recheck behind the retry fails.
// recordCommentDeliveryResolution must not un-name the session holding the frame just because
// the recheck came back empty - that recheck can only report the same session or nothing, and
// the listener being unreachable is the outage LEGION-244 exists to survive. Before the fix the
// row's session_id was overwritten unconditionally with the recheck's result, so the recipient
// was locked out of the reply for the life of the comment, not merely until the next retry: a
// retry only resumes a still-"pending" attempt, and this one settles "failed" right here.
func TestStrandedAttemptRetriedDuringAListenerOutageKeepsThePinAndAcceptsTheReply(t *testing.T) {
	sessionsUnavailable := false
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			if sessionsUnavailable {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Stranded retry during a listener outage", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Attempt 1 goes out fine.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))

	// A sender recorded attempt 2's resolution - pinning it to s1, the session its frame
	// reached - and died before it could send or settle. The claim is old enough to be
	// plainly abandoned.
	if _, err := database.Pool.Exec(context.Background(), `
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state, claimed_at)
		values ($1, 'session:s1', 2, 'steer', 's1', 'pending', now() - interval '5 minutes')
	`, comment.ID); err != nil {
		t.Fatalf("strand attempt 2 pinned to s1: %v", err)
	}

	// The retry resumes attempt 2. Its recheck of the pin fails because the listener cannot
	// answer at all, not because s1 is gone.
	sessionsUnavailable = true
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry during a listener outage: status=%d body=%s", retry.Code, retry.Body.String())
	}
	attempt := decodeBody[commentDeliveryRead](t, retry)
	if attempt.Attempt != 2 || attempt.State != "failed" || attempt.SessionID == nil || *attempt.SessionID != "s1" {
		t.Fatalf("attempt settled during the outage = %#v, want the pin kept on a failed attempt", attempt)
	}

	// The listener recovers; the session the frame reached can still answer it, because the
	// outage failed the recheck rather than the recipient's own claim on the attempt.
	sessionsUnavailable = false
	reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 2,
		"target": "session:s1", "body": "It reached me before the outage.",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("the recipient's reply after an outage-failed recheck: status=%d body=%s", reply.Code, reply.Body.String())
	}
}

// TestStrandedRoleAttemptResumedAfterTheRoleMovesKeepsSendingToItsOriginalRecipient is
// Deep1296b's case B: a role mention's attempt is pinned to the session that held the role
// when it was resolved, retried while that session is briefly unreachable (its recheck fails,
// which the pre-fix write read as "never resolved" and so un-pinned it), and resumed again
// after the role has since moved to a different holder and the unreachable retry's claim has
// lapsed. The resumed attempt must still recheck its ORIGINAL recipient rather than re-run role
// resolution - the same recipient, the same idempotency key, and zero role lookups - because
// that recipient is who the listener already deduplicated this attempt's key for and who holds
// the frame; a role that moved is reached by an attempt of its own, never by re-pointing this
// one (comment_delivery.go, AGENTS.md:239, AGENTS.md:248 - the rule the message path already
// gets from recordPendingMessageDelivery's coalesce).
// TestStrandedRoleAttemptRetriedDuringAnOutageKeepsThePinAndRefusesTheNewHolder is Deep1296b's
// case B: a role mention's attempt is already pinned to the session that held the role when it
// was resolved, and it is retried after the role has since moved to a different holder while
// that pinned session is briefly unreachable, so the retry's recheck of the pin fails without
// ever consulting the role. recordCommentDeliveryResolution must keep the original recipient -
// the same rule the message path already gets from recordPendingMessageDelivery's coalesce
// (comment_delivery.go, AGENTS.md:239, AGENTS.md:248) - rather than treat a failed recheck as
// grounds to re-resolve the role and hand the attempt to whoever holds it now: the original
// recipient is who the listener's key names and who is still allowed to answer it, and the
// role's new holder was never sent this attempt at all.
func TestStrandedRoleAttemptRetriedDuringAnOutageKeepsThePinAndRefusesTheNewHolder(t *testing.T) {
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
	issue := createInteractionIssue(t, handler, "TEST", "Role mention outlives a listener outage", "before")
	patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"route": "role:reviewer"}, "alice")
	if patched.Code != http.StatusOK {
		t.Fatalf("set reviewer route: status=%d body=%s", patched.Code, patched.Body.String())
	}
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Keep this holder through an outage.", "mentions": []map[string]any{{"target": "role:reviewer"}},
	}))

	// Attempt 1 sent to s1, the role's holder at creation. A sender strands it back to
	// pending, claimed long enough ago to be plainly abandoned; the row still names s1, the
	// session the frame reached.
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set state = 'pending', envelope_id = null, error = null, claimed_at = now() - interval '5 minutes'
		where comment_id = $1 and target = 'role:reviewer' and attempt = 1
	`, comment.ID); err != nil {
		t.Fatalf("strand attempt 1 pinned to s1: %v", err)
	}

	// The role moves to s2 and s1 goes briefly unreachable, so the retry's recheck of the
	// pinned session fails without ever consulting the role.
	roleHolder = "s2"
	s1Live = false
	roleLookups = 0
	sent = nil
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry during the outage: status=%d body=%s", retry.Code, retry.Body.String())
	}
	attempt := decodeBody[commentDeliveryRead](t, retry)
	if attempt.Attempt != 1 || attempt.State != "failed" || attempt.SessionID == nil || *attempt.SessionID != "s1" {
		t.Fatalf("attempt settled during the outage = %#v, want the pin on s1 kept", attempt)
	}
	if roleLookups != 0 || len(sent) != 0 {
		t.Fatalf("retry during the outage = lookups:%d sends:%#v, want the pin rechecked directly and no send attempted", roleLookups, sent)
	}

	// s1 still holds the attempt, even though the role has since moved to s2: the role's new
	// holder was never sent this frame and cannot answer it.
	original := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1,
		"target": "role:reviewer", "body": "It reached me before the outage.",
	})
	if original.Code != http.StatusCreated {
		t.Fatalf("the original holder's reply after the outage: status=%d body=%s", original.Code, original.Body.String())
	}
	stolen := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s2"}, "attempt": 1,
		"target": "role:reviewer", "body": "I hold the role now.",
	})
	if stolen.Code != http.StatusForbidden {
		t.Fatalf("the new holder's reply to an attempt it never received: status=%d body=%s", stolen.Code, stolen.Body.String())
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
		update comment_deliveries set state = 'pending', envelope_id = null, error = null, claimed_at = null
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
		update comment_deliveries set state = 'pending', envelope_id = null, error = null, claimed_at = null
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

// TestInitialCommentDeliveryRechecksCapabilityAgainstCurrentRegistration proves the post-commit
// completion of a fresh comment mention -- not just a later retry -- refuses when the target
// loses the requested mode between the pre-commit resolution and the actual send. Resolution
// happens before the creation transaction commits; the send happens after. That gap is real
// even when nothing else races it, so the initial path must recheck exactly like a retry does,
// not trust a decision that predates the commit it was made inside of.
func TestInitialCommentDeliveryRechecksCapabilityAgainstCurrentRegistration(t *testing.T) {
	capabilities := []string{"steer"}
	sessionLookups := 0
	sends := 0
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			sessionLookups++
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"session_id": "s1", "title": "planner", "capabilities": capabilities,
			}})
			capabilities = []string{}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			sends++
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Initial capability race", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body":     "Created while steer was advertised.",
		"mentions": []map[string]any{{"target": "session:s1"}},
		"delivery": "steer",
	}))
	if len(comment.Deliveries) != 1 {
		t.Fatalf("deliveries = %#v, want one", comment.Deliveries)
	}
	attempt := comment.Deliveries[0]
	if attempt.State != "failed" || attempt.Error == nil ||
		*attempt.Error != "session s1 (planner) does not advertise steer" || sends != 0 {
		t.Fatalf("initial delivery reused stale capability: lookups=%d sends=%d attempt=%#v", sessionLookups, sends, attempt)
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
		update comment_deliveries set state = 'pending', envelope_id = null, error = null, claimed_at = null
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
