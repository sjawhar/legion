package intake

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A session's reply sent through a delivery's callback arrives as comment.answered, with the
// thread's ask and whose turn it leaves, as a comment.created reply does: a hand-back sent that way
// decodes to the same reply, so it starts the root's wait.
func TestACallbackReplyDecodesAsTheReplyItIs(t *testing.T) {
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for _, eventType := range []string{"comment.created", "comment.answered"} {
		t.Run(eventType, func(t *testing.T) {
			event, err := json.Marshal(map[string]any{
				"id": 40, "issue_key": "LEGION-7", "seq": 12, "type": eventType, "notify": true, "created_at": at,
				"actor":   map[string]any{"kind": "session", "id": "session-architect"},
				"payload": map[string]any{"id": "comment-1", "ask_id": "ask-approval", "ask_waiting_on": "human"},
			})
			if err != nil {
				t.Fatalf("encode the event: %v", err)
			}
			fact, err := DecodeDispatchEvent("LEGION", "LEGION-7", event)
			if err != nil {
				t.Fatalf("decode %s: %v", eventType, err)
			}
			want := DispatchAskReply{Key: "LEGION-7", Ask: "ask-approval", WaitingOn: "human",
				Actor: dispatch.Actor{Kind: "session", ID: "session-architect"}, Mark: record.EventMark{Seq: 12, At: at}}
			if !reflect.DeepEqual(fact, want) {
				t.Fatalf("%s decoded to %#v, want %#v", eventType, fact, want)
			}
		})
	}
}

// Settlement restoring a decision block it retracted appends ask.answered with the answer given
// before the retraction, credited to the person whose edit restored it, and marks it restored: no
// one answered anything, so it decodes to no fact, while a person's own answer still decodes.
func TestARestoredAnswerIsNoPersonsAnswer(t *testing.T) {
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		restored bool
	}{{name: "a person's answer"}, {name: "settlement's restoration", restored: true}} {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"id": "ask-question", "kind": "question", "state": "answered"}
			if tc.restored {
				payload["restored"] = true
			}
			event, err := json.Marshal(map[string]any{
				"id": 41, "issue_key": "LEGION-7", "seq": 13, "type": "ask.answered", "notify": true, "created_at": at,
				"actor": map[string]any{"kind": "user", "id": "alice"}, "payload": payload,
			})
			if err != nil {
				t.Fatalf("encode the event: %v", err)
			}
			fact, err := DecodeDispatchEvent("LEGION", "LEGION-7", event)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if tc.restored != (fact == nil) {
				t.Fatalf("%s decoded to %#v, want a fact %t", tc.name, fact, !tc.restored)
			}
		})
	}
}
