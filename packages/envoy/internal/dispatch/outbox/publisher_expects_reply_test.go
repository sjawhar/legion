package outbox

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An answered ask and an approved artifact are receipts: a person decided, and the session the
// event reaches has nothing to send back. Their envelopes say so, and a session that answered one
// anyway cost that person a read (INFRA-1225). Every other event type says nothing about a reply,
// an ask event the same branch gives an urgency included.
//
// What is read here is the marshalled envelope, because that is what the bus carries: the no-reply
// value is the JSON literal false, not the word it used to be. And each envelope is validated as
// resolveDestinations validates it before publishing, because an envelope Validate() refuses is
// backed off forever and holds every later event of its owner behind it.
func TestReceiptEnvelopesWantNoReplyAndEveryOtherEventSaysNothing(t *testing.T) {
	const ask = "00000000-0000-0000-0000-000000000030"
	for _, tc := range []struct {
		name  string
		event model.Event
		// want is the envelope's expects_reply as JSON, and "" is the absent field.
		want string
	}{
		{
			name: "an answered ask",
			event: model.Event{
				ID: 1, IssueKey: new("T-1"), Type: "ask.answered", CreatedAt: time.Now(),
				Payload: map[string]any{"id": ask, "answer": "Approve", "urgency": "high"},
			},
			want: "false",
		},
		{
			name: "an approved artifact",
			event: model.Event{
				ID: 2, IssueKey: new("T-1"), Type: "artifact.approved", CreatedAt: time.Now(),
				Payload: map[string]any{
					"artifact_id": "00000000-0000-0000-0000-000000000040",
					"name":        "spec.md", "version": 3, "ask_id": ask,
				},
			},
			want: "false",
		},
		{
			name: "an opened ask, which wants an answer the envelope does not name",
			event: model.Event{
				ID: 3, IssueKey: new("T-1"), Type: "ask.opened", CreatedAt: time.Now(),
				Payload: map[string]any{"id": ask, "question": "Which approach?", "urgency": "high"},
			},
		},
		{
			name: "a comment",
			event: model.Event{
				ID: 4, IssueKey: new("T-1"), Type: "comment.created", CreatedAt: time.Now(),
				Payload: map[string]any{"id": "comment-9", "body": "Slice 3 is pushed."},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item, err := envelope(tc.event, "")
			if err != nil {
				t.Fatalf("envelope: %v", err)
			}
			if err := item.Validate(); err != nil {
				t.Fatalf("validate envelope: %v", err)
			}
			raw, err := json.Marshal(item)
			if err != nil {
				t.Fatalf("marshal envelope: %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatalf("decode envelope %s: %v", raw, err)
			}
			got := string(fields["expects_reply"])
			if _, present := fields["expects_reply"]; !present {
				got = ""
			}
			if got != tc.want {
				t.Fatalf("expects_reply of %s = %q, want %q", tc.event.Type, got, tc.want)
			}
		})
	}
}
