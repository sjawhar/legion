package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// pinAgent is PUT /api/v1/me/agents/{session}/state carrying `pinned` and nothing else, the body
// the sidebar's pin control sends, and the state the route answers with for that session.
func pinAgent(t *testing.T, handler http.Handler, login, sessionID string, pinned bool) userAgentState {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/"+sessionID+"/state", map[string]any{
		"pinned": pinned,
	}, login)
	if response.Code != http.StatusOK {
		t.Fatalf("pin %v for %s/%s: status=%d body=%s, want 200", pinned, login, sessionID, response.Code, response.Body.String())
	}
	return decodeBody[userAgentState](t, response)
}

// A body carrying `pinned` alone is a whole write: the "at least one field" gate takes it without
// a Clear or a read mark, and the session is then in the answer of GET /api/v1/me/agents/state
// with its pin and no other mark. That second half is what the three-way join exists for: a
// session a person pinned and never cleared, read or had a reply from is named by neither
// direct_marks nor unread_direct_replies, and a two-way join would leave it out of the answer
// altogether, so the sidebar would have nothing to draw.
func TestUserAgentPinIsAcceptedAloneAndReadBackWithNoOtherMark(t *testing.T) {
	handler := newTestHandler(t)
	before := time.Now().UTC().Add(-time.Second)
	written := pinAgent(t, handler, "alice", "s1", true)
	if written.PinnedAt == nil || written.ClearedBefore != nil || written.ReadThrough != nil || written.UnreadReplies != 0 {
		t.Fatalf("the pin write answered %#v, want the pin alone", written)
	}
	at := parseTimestamp(t, *written.PinnedAt)
	if at.Before(before) || at.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("pinned_at = %s, want the server's own clock at the write", at)
	}
	got := unreadReplies(t, handler, "alice", "s1")
	if got.PinnedAt == nil || !parseTimestamp(t, *got.PinnedAt).Equal(at) ||
		got.ClearedBefore != nil || got.ReadThrough != nil || got.UnreadReplies != 0 {
		t.Fatalf("the state read answers %#v, want the pin alone at %s", got, at)
	}
}

// A gate that still demanded a Clear, a read mark or replies read by id would refuse the pin
// write, so the refusal's own message names every field the route takes.
func TestUserAgentPinIsNamedByTheRefusalOfAnEmptyBody(t *testing.T) {
	handler := newTestHandler(t)
	empty := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{}, "alice")
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("an empty body: status=%d body=%s, want 400", empty.Code, empty.Body.String())
	}
	for _, field := range []string{"cleared_before", "read_through", "read_replies", "pinned"} {
		if !strings.Contains(empty.Body.String(), field) {
			t.Fatalf("the refusal %s does not name %s", empty.Body.String(), field)
		}
	}
}

// The pin is keyed on the canonical login, so one person holds one pin however their identity
// source spells them: aLiCe pins and alice reads that pin, not a second one of their own.
func TestUserAgentPinIsOnePinWhateverTheCasingOfTheirLogin(t *testing.T) {
	handler := newTestHandler(t)
	written := pinAgent(t, handler, "aLiCe", "s1", true)
	if written.PinnedAt == nil {
		t.Fatalf("aLiCe's pin answered %#v, want the pin", written)
	}
	for _, login := range []string{"aLiCe", "alice", "ALICE"} {
		got := unreadReplies(t, handler, login, "s1")
		if got.PinnedAt == nil || !parseTimestamp(t, *got.PinnedAt).Equal(parseTimestamp(t, *written.PinnedAt)) {
			t.Fatalf("%s reads %#v, want the one pin aLiCe wrote at %s", login, got, *written.PinnedAt)
		}
	}
	if unpinned := pinAgent(t, handler, "ALICE", "s1", false); unpinned.PinnedAt != nil {
		t.Fatalf("after ALICE unpinned it: %#v, want no pin: one person, one pin", unpinned)
	}
	if got := unreadReplies(t, handler, "alice", "s1"); got.PinnedAt != nil {
		t.Fatalf("alice reads %#v after ALICE unpinned it, want no pin", got)
	}
}

// The pin and the Clear are two writes of one state and come back as one record: the pin write
// leaves the Clear exactly as it was — pinning a conversation is not reading it — and the Clear
// leaves the pin, so a person who clears a pinned agent keeps it in their sidebar.
func TestUserAgentPinAndClearAreOneRecordAndNeitherWriteTouchesTheOther(t *testing.T) {
	handler := newTestHandler(t)
	// Truncated as TestPerUserAgentStateRoundTripsAndIsIsolated truncates: timestamptz holds
	// microseconds, so a nanosecond the write sends is not one the read answers with.
	cleanedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	cleared := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{
		"cleared_before": cleanedAt.Format(time.RFC3339Nano),
	}, "alice")
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear: status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	written := pinAgent(t, handler, "alice", "s1", true)
	if written.PinnedAt == nil || written.ClearedBefore == nil ||
		!parseTimestamp(t, *written.ClearedBefore).Equal(cleanedAt) {
		t.Fatalf("after the pin write: %#v, want the pin beside the Clear at %s", written, cleanedAt)
	}
	got := unreadReplies(t, handler, "alice", "s1")
	if got.PinnedAt == nil || got.ClearedBefore == nil || !parseTimestamp(t, *got.ClearedBefore).Equal(cleanedAt) {
		t.Fatalf("the state read answers %#v, want one record holding the pin and the Clear at %s", got, cleanedAt)
	}
	unpinned := pinAgent(t, handler, "alice", "s1", false)
	if unpinned.PinnedAt != nil || unpinned.ClearedBefore == nil ||
		!parseTimestamp(t, *unpinned.ClearedBefore).Equal(cleanedAt) {
		t.Fatalf("after the unpin: %#v, want the Clear at %s and no pin", unpinned, cleanedAt)
	}
	if got := unreadReplies(t, handler, "alice", "s1"); got.PinnedAt != nil || got.ClearedBefore == nil {
		t.Fatalf("the state read answers %#v after the unpin, want the Clear alone", got)
	}
}

// A pin is announced on the live event feed like a Clear or a read mark, so the person's other
// open tabs and devices refetch their state and draw the same sidebar instead of waiting for a
// focus. The handler commits only where it announces, so an unannounced pin would also be a pin
// that was never written.
func TestUserAgentPinWriteAnnouncesItselfToTheViewersOtherDevices(t *testing.T) {
	for _, pinned := range []bool{true, false} {
		t.Run(map[bool]string{true: "pinned", false: "unpinned"}[pinned], func(t *testing.T) {
			handler, database := newTestHandlerWithStore(t)
			pinAgent(t, handler, "alice", "s1", pinned)
			var login, session string
			if err := database.Pool.QueryRow(context.Background(), `
				select payload->>'login', payload->>'session_id' from events where type = 'user_agent_state.updated'
			`).Scan(&login, &session); err != nil {
				t.Fatalf("read the user_agent_state.updated event: %v", err)
			}
			if login != "alice" || session != "s1" {
				t.Fatalf("event names %s/%s, want alice/s1", login, session)
			}
		})
	}
}
