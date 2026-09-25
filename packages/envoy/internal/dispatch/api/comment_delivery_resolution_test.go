package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// roleMentionRegistry is the listener state a role-mention retry test moves between requests:
// who holds the role, whether the session an attempt is pinned to is still live, how many role
// lookups the listener answered, and every send it accepted.
type roleMentionRegistry struct {
	holder  string
	s1Live  bool
	lookups int
	sent    []map[string]any
}

// roleMentionListener is the Envoy listener the role-mention retry tests run against: it
// resolves reviewer to the registry's holder, lists s1 only while the registry says it is live,
// and records each send.
func roleMentionListener(t *testing.T, registry *roleMentionRegistry) *httptest.Server {
	t.Helper()
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			registry.lookups++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"role": "reviewer", "holder": registry.holder, "title": "reviewer", "capabilities": []string{"steer"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			if !registry.s1Live {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode role mention send: %v", err)
			}
			registry.sent = append(registry.sent, request)
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	return listener
}

func TestPendingRoleMentionRetryKeepsTheCreateTimeHolder(t *testing.T) {
	registry := &roleMentionRegistry{holder: "s1", s1Live: true}
	listener := roleMentionListener(t, registry)
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
	registry.holder = "s2"
	registry.sent = nil
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry pending role mention: status=%d body=%s", retry.Code, retry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, retry); attempt.Attempt != 1 || attempt.SessionID == nil || *attempt.SessionID != "s1" || attempt.State != "sent" {
		t.Fatalf("pending role retry attempt = %#v, want original-holder attempt 1", attempt)
	}
	if registry.lookups != 1 || len(registry.sent) != 1 || registry.sent[0]["target_session"] != "s1" {
		t.Fatalf("pending role retry = lookups:%d sends:%#v, want no role re-resolution or s2 delivery", registry.lookups, registry.sent)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set state = 'pending', envelope_id = null, error = null, claimed_at = null
		where comment_id = $1 and target = 'role:reviewer' and attempt = 1
	`, comment.ID); err != nil {
		t.Fatalf("restore pending disappeared-holder delivery: %v", err)
	}
	registry.s1Live = false
	registry.sent = nil
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
	if registry.lookups != 1 || len(registry.sent) != 0 {
		t.Fatalf("absent original holder retry = lookups:%d sends:%#v, want no role re-resolution and no send attempt", registry.lookups, registry.sent)
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

	// The receipt that settle appended says what its row says, the session holding the frame
	// included: a recheck the listener could not answer must not un-name the recipient in the
	// event either.
	if receipts, row := commentDeliveryReceipts(t, database, comment.ID, "session:s1", 2),
		commentAttemptRow(t, database, comment.ID, "session:s1", 2); receipts != row {
		t.Fatalf("attempt 2 receipts = %q, want the one receipt %q its row owes", receipts, row)
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

// TestStrandedRoleAttemptRetriedDuringAnOutageKeepsThePinAndRefusesTheNewHolder is Deep1296b's
// case B: a role mention's attempt is already pinned to the session that held the role when it
// was resolved, and it is retried after the role has since moved to a different holder while
// that pinned session is briefly unreachable, so the retry's recheck of the pin fails without
// ever consulting the role. recordCommentDeliveryResolution must keep the original recipient -
// the same rule the message path already gets from recordPendingMessageDelivery's coalesce
// (comment_delivery.go, and the pin rule in packages/envoy/AGENTS.md: "A resumed attempt keeps
// the recipient it was opened for ... only whether the original recipient can still receive the
// mode is re-derived") - rather than treat a failed recheck as grounds to re-resolve the role
// and hand the attempt to whoever holds it now: the original recipient is who the listener's key
// names and who is still allowed to answer it, and the role's new holder was never sent this
// attempt at all.
func TestStrandedRoleAttemptRetriedDuringAnOutageKeepsThePinAndRefusesTheNewHolder(t *testing.T) {
	registry := &roleMentionRegistry{holder: "s1", s1Live: true}
	listener := roleMentionListener(t, registry)
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
	registry.holder = "s2"
	registry.s1Live = false
	registry.lookups = 0
	registry.sent = nil
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
	if registry.lookups != 0 || len(registry.sent) != 0 {
		t.Fatalf("retry during the outage = lookups:%d sends:%#v, want the pin rechecked directly and no send attempted", registry.lookups, registry.sent)
	}

	// Attempt 1 already carried the receipt its creation send recorded; the outage settle pays
	// the second, and that one names s1 exactly as the row it was built from does.
	row := commentAttemptRow(t, database, comment.ID, "role:reviewer", 1)
	if receipts := commentDeliveryReceipts(t, database, comment.ID, "role:reviewer", 1); receipts != "sent///s1 | "+row {
		t.Fatalf("attempt 1 receipts = %q, want the creation receipt and the one %q the outage settle owes", receipts, row)
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
