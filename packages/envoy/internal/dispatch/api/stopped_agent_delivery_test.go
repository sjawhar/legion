package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A Legion agent whose session has stopped is still a message's recipient while the listener's
// agent registry names it: the listener hands what is sent to it to the daemon that answers for
// it and answers the send as delivered (cmd/listener/legion_handoff.go). So Dispatch records that
// session on the attempt whatever mode it was sent in, the attempt is sent, and the reply the
// daemon posts as that session is the attempt's own.

const (
	stoppedAgent        = "stopped-1"
	stoppedNewest       = "stopped-2"
	registeredRole      = "legion-test-planner"
	unregisteredSession = "gone"
)

// stoppedAgentListener is an Envoy listener with no live session and no live role holder, whose
// Legion agent registry holds two stopped sessions of the role legion-test-planner, stopped-2
// newest and stopped-1 before it. Its registry reads fail while registryDown is set. It takes
// every send, as its hand-off does, and records the session each one is for; while refuseSends is
// set it records each and refuses it as a listener with no daemon answering for the agent does.
type stoppedAgentListener struct {
	*httptest.Server
	registryDown atomic.Bool
	refuseSends  atomic.Bool

	mu    sync.Mutex
	sends []string
}

func newStoppedAgentListener(t *testing.T) *stoppedAgentListener {
	t.Helper()
	listener := &stoppedAgentListener{}
	agent := func(sessionID string) map[string]any {
		return map[string]any{
			"session_id": sessionID, "legion_project": "legion-test", "issue": "TEST-1", "role": "planner",
			"role_token": registeredRole, "title": "planner " + sessionID, "harness": "omp",
			"started_at": "2026-10-26T00:00:00Z", "recorded_at": "2026-10-26T00:00:00Z", "live": false,
		}
	}
	listener.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry := strings.HasPrefix(r.URL.Path, "/v1/legion/agents")
		switch {
		case registry && listener.registryDown.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"nats: timeout"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no live session ` + id + `"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/roles/"):
			role := strings.TrimPrefix(r.URL.Path, "/v1/roles/")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no holder for role ` + role + `","reason":"unclaimed"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/legion/agents/sessions/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/legion/agents/sessions/")
			if id != stoppedAgent && id != stoppedNewest {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"unknown legion agent ` + id + `"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(agent(id))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/legion/agents/roles/"+registeredRole:
			_ = json.NewEncoder(w).Encode(map[string]any{"entries": []any{agent(stoppedNewest), agent(stoppedAgent)}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/legion/agents/roles/"):
			_, _ = w.Write([]byte(`{"entries":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var input struct {
				TargetSession string `json:"target_session"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode listener send: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			listener.mu.Lock()
			listener.sends = append(listener.sends, input.TargetSession)
			count := len(listener.sends)
			listener.mu.Unlock()
			if listener.refuseSends.Load() {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no live session ` + input.TargetSession + `"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"event_id": "handed-off-" + strconv.Itoa(count), "recipient": input.TargetSession,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	t.Cleanup(listener.Close)
	return listener
}

func (l *stoppedAgentListener) sent() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.sends...)
}

// assertHandedOff checks a message's one attempt was recorded for sessionID and sent.
func assertHandedOff(t *testing.T, name string, message model.Message, sessionID string) {
	t.Helper()
	if got := message.Deliveries; len(got) != 1 || got[0].Attempt != 1 || got[0].State != "sent" ||
		got[0].SessionID != sessionID || got[0].Error != nil {
		t.Fatalf("%s: deliveries = %#v, want attempt 1 sent to %s", name, got, sessionID)
	}
}

// replyAs posts the daemon's reply to a message's first attempt as sessionID.
func replyAs(t *testing.T, handler http.Handler, messageID, sessionID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+messageID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": sessionID}, "attempt": 1, "body": body,
	})
}

func assertAnswered(t *testing.T, name string, response *httptest.ResponseRecorder, messageID string) {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("%s: reply status=%d body=%s, want 201", name, response.Code, response.Body.String())
	}
	if reply := decodeBody[model.Message](t, response); reply.InReplyTo == nil || *reply.InReplyTo != messageID {
		t.Fatalf("%s: reply = %#v, want it in %s's thread", name, reply, messageID)
	}
}

func TestMessageToAStoppedRegisteredAgentIsSentToItsSessionAndTakesItsReply(t *testing.T) {
	listener := newStoppedAgentListener(t)
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Stopped agent", "before")

	// steer: the registry entry advertises no capabilities, so a live session's check would
	// refuse it; the stopped agent's daemon answers every mode by reply.
	onIssue := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Why did you pick this?", "target": "session:" + stoppedAgent, "delivery": "steer",
	}, "alice")
	assertHandedOff(t, "an issue message", onIssue, stoppedAgent)

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/"+stoppedAgent+"/messages", map[string]any{
		"body": "And on the conversation page?", "delivery": "btw",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("issue-less message: status=%d body=%s", created.Code, created.Body.String())
	}
	issueless := decodeBody[model.Message](t, created)
	if issueless.IssueKey != nil {
		t.Fatalf("conversation page message = %#v, want no issue", issueless)
	}
	assertHandedOff(t, "an issue-less message", issueless, stoppedAgent)

	if got := listener.sent(); len(got) != 2 || got[0] != stoppedAgent || got[1] != stoppedAgent {
		t.Fatalf("listener sends = %v, want both messages sent to %s", got, stoppedAgent)
	}

	assertAnswered(t, "the issue message", replyAs(t, handler, onIssue.ID, stoppedAgent, "Because of the spec."), onIssue.ID)
	assertAnswered(t, "the issue-less message", replyAs(t, handler, issueless.ID, stoppedAgent, "Same answer."), issueless.ID)
}

func TestRoleMessageToAStoppedRegisteredRoleGoesToItsNewestSession(t *testing.T) {
	listener := newStoppedAgentListener(t)
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Stopped role", "before")

	message := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Who planned this?", "target": "role:" + registeredRole, "delivery": "aside",
	}, "alice")
	assertHandedOff(t, "a role message", message, stoppedNewest)
	if got := listener.sent(); len(got) != 1 || got[0] != stoppedNewest {
		t.Fatalf("listener sends = %v, want one to %s", got, stoppedNewest)
	}

	// The role's older session is not the attempt's recipient, so its reply is refused.
	older := replyAs(t, handler, message.ID, stoppedAgent, "Not me.")
	if older.Code != http.StatusForbidden || !strings.Contains(older.Body.String(), `"code":"REPLY_FORBIDDEN"`) {
		t.Fatalf("older session's reply: status=%d body=%s, want 403 REPLY_FORBIDDEN", older.Code, older.Body.String())
	}
	assertAnswered(t, "the role message", replyAs(t, handler, message.ID, stoppedNewest, "I did."), message.ID)
}

func TestMentionOfAStoppedRegisteredAgentIsSentToItsSessionAndTakesItsReply(t *testing.T) {
	listener := newStoppedAgentListener(t)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Stopped mention", "before")

	sessionTarget, roleTarget := "session:"+stoppedAgent, "role:"+registeredRole
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can either of you explain this?", "delivery": "btw",
		"mentions": []map[string]any{{"target": sessionTarget}, {"target": roleTarget}},
	}))
	want := map[string]string{sessionTarget: stoppedAgent, roleTarget: stoppedNewest}
	if len(comment.Deliveries) != 2 {
		t.Fatalf("mention deliveries = %#v, want one per mention", comment.Deliveries)
	}
	for _, delivery := range comment.Deliveries {
		if delivery.State != "sent" || delivery.Attempt != 1 || delivery.Error != nil ||
			delivery.SessionID == nil || *delivery.SessionID != want[delivery.Target] {
			t.Fatalf("mention delivery = %#v (session %v, error %q), want attempt 1 sent to %s",
				delivery, deref(delivery.SessionID), deref(delivery.Error), want[delivery.Target])
		}
		if row := commentAttemptRow(t, database, comment.ID, delivery.Target, 1); row != "sent///"+want[delivery.Target] {
			t.Fatalf("%s attempt row = %q, want sent to %s", delivery.Target, row, want[delivery.Target])
		}
	}
	if got := listener.sent(); len(got) != 2 {
		t.Fatalf("listener sends = %v, want one per mention", got)
	}

	for target, sessionID := range want {
		answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
			"actor": map[string]any{"kind": "session", "id": sessionID}, "attempt": 1, "target": target,
			"body": "Answered by " + sessionID + "'s daemon.",
		})
		if answered.Code != http.StatusCreated {
			t.Fatalf("%s's reply to %s: status=%d body=%s", sessionID, target, answered.Code, answered.Body.String())
		}
		if reply := decodeBody[model.Comment](t, answered); reply.ReplyTo == nil || *reply.ReplyTo != comment.ID {
			t.Fatalf("%s's reply = %#v, want it in the mentioning comment's thread", sessionID, reply)
		}
	}
}

// A target the registry does not name, every target while the registry does not answer, and a
// target the registry names whose daemon does not answer - the listener refuses the send, having
// handed it to no daemon - are refused exactly as before the registry existed: the listener's own
// failure recorded, with the session it recorded then.
func TestTargetTheRegistryDoesNotNameKeepsTheListenersFailure(t *testing.T) {
	for _, tc := range []struct {
		name         string
		registryDown bool
		refuseSends  bool
		session      string
		role         string
		wantSends    []string
	}{
		{name: "unregistered", session: unregisteredSession, role: "legion-test-reviewer"},
		{name: "registry unavailable", registryDown: true, session: stoppedAgent, role: registeredRole},
		{
			name: "no daemon answers", refuseSends: true, session: stoppedAgent, role: registeredRole,
			wantSends: []string{stoppedAgent, stoppedAgent, stoppedNewest, stoppedAgent, stoppedNewest},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := newStoppedAgentListener(t)
			listener.registryDown.Store(tc.registryDown)
			listener.refuseSends.Store(tc.refuseSends)
			handler, database := newTargetedMessageHandler(t, listener.URL)
			issue := createInteractionIssue(t, handler, "TEST", "Unanswered", "before")

			failed := func(name string, message model.Message, sessionID, failure string) {
				t.Helper()
				if got := message.Deliveries; len(got) != 1 || got[0].State != "failed" || got[0].SessionID != sessionID ||
					got[0].Error == nil || *got[0].Error != failure {
					t.Fatalf("%s: deliveries = %#v, want failed %q", name, got, failure)
				}
			}
			noLive := "no live session " + tc.session
			noHolder := "no holder for role " + tc.role
			failed("an issue message", createIssueMessage(t, handler, issue.Key, map[string]any{
				"body": "Anyone?", "target": "session:" + tc.session, "delivery": "btw",
			}, "alice"), tc.session, noLive)

			created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/"+tc.session+"/messages", map[string]any{
				"body": "Anyone here?", "delivery": "btw",
			}, "alice")
			if created.Code != http.StatusCreated {
				t.Fatalf("issue-less message: status=%d body=%s", created.Code, created.Body.String())
			}
			failed("an issue-less message", decodeBody[model.Message](t, created), tc.session, noLive)

			failed("a role message", createIssueMessage(t, handler, issue.Key, map[string]any{
				"body": "Who holds this?", "target": "role:" + tc.role, "delivery": "btw",
			}, "alice"), "", noHolder)

			sessionTarget, roleTarget := "session:"+tc.session, "role:"+tc.role
			comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
				"body":     "Mentioning nobody live.",
				"mentions": []map[string]any{{"target": sessionTarget}, {"target": roleTarget}},
			}))
			want := map[string]string{sessionTarget: noLive, roleTarget: noHolder}
			if len(comment.Deliveries) != 2 {
				t.Fatalf("mention deliveries = %#v, want one per mention", comment.Deliveries)
			}
			for _, delivery := range comment.Deliveries {
				failure := want[delivery.Target]
				if delivery.State != "failed" || delivery.SessionID != nil || delivery.Error == nil || *delivery.Error != failure {
					t.Fatalf("%s mention delivery = %#v (session %v, error %q), want failed %q with no session",
						delivery.Target, delivery, deref(delivery.SessionID), deref(delivery.Error), failure)
				}
				if row := commentAttemptRow(t, database, comment.ID, delivery.Target, 1); row != "failed/"+failure+"//" {
					t.Fatalf("%s mention attempt row = %q, want failed with no session", delivery.Target, row)
				}
			}

			if got := listener.sent(); strings.Join(got, ",") != strings.Join(tc.wantSends, ",") {
				t.Fatalf("listener sends = %v, want %v", got, tc.wantSends)
			}
		})
	}
}
