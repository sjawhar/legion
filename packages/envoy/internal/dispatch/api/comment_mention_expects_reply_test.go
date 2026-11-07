package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// What a mention of a comment asks of the session it reaches. A comment that joins no ask thread
// and answers no comment is a remark: nothing is wanted back, and a session that answered one
// wrote a receipt that cost the person who commented a read (INFRA-1225). A reply in a thread and
// a comment under an ask are conversation, so both keep inviting a reply, and a btw interrupts the
// session to ask it something, so it waits on an answer whatever the comment is.
//
// The value is read off the body the listener was sent, which is what the delivery carries: the
// no-reply value is the JSON literal false, not a word.
func TestCommentMentionExpectsReplyFollowsTheComment(t *testing.T) {
	const session = `{"session_id":"s1","title":"planner","capabilities":["aside","btw","steer"],"last_seen":200}`
	var guard sync.Mutex
	sends := map[string]string{}
	envelopes := 0
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			_, _ = w.Write([]byte(session))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte("[" + session + "]"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var input struct {
				Message      string          `json:"message"`
				ExpectsReply json.RawMessage `json:"expects_reply"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode mention send: %v", err)
			}
			guard.Lock()
			sends[input.Message] = string(input.ExpectsReply)
			envelopes++
			id := envelopes
			guard.Unlock()
			_, _ = fmt.Fprintf(w, `{"event_id":"mention-envelope-%d","recipient":"s1"}`, id)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Mention receipts", "before")
	mention := []map[string]any{{"target": "session:s1"}}
	post := func(body string, input map[string]any) string {
		t.Helper()
		input["body"], input["mentions"] = body, mention
		created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, input))
		if len(created.Deliveries) != 1 || created.Deliveries[0].State != "sent" {
			t.Fatalf("deliveries of %q = %#v, want one sent delivery", body, created.Deliveries)
		}
		return created.ID
	}

	const plain = "The staging deploy is green."
	root := post(plain, map[string]any{})
	const threaded = "It was green here too."
	post(threaded, map[string]any{"reply_to": root})
	const underAsk = "Say more about the second option."
	post(underAsk, map[string]any{"ask_id": openTurnAsk(t, handler, issue.Key, "Which approach?")})
	const interrupting = "Stop the deploy."
	post(interrupting, map[string]any{"delivery": "btw"})

	guard.Lock()
	defer guard.Unlock()
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "a plain comment wants nothing back", body: plain, want: "false"},
		{name: "a reply in a thread invites one", body: threaded, want: `"optional"`},
		{name: "a comment under an ask invites one", body: underAsk, want: `"optional"`},
		{name: "a btw waits on one", body: interrupting, want: `"required"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, sent := sends[tc.body]
			if !sent {
				t.Fatalf("the listener was sent no delivery of %q", tc.body)
			}
			if got != tc.want {
				t.Fatalf("expects_reply of %q = %s, want %s", tc.body, got, tc.want)
			}
		})
	}
}
