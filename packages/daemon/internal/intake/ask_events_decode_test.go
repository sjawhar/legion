package intake

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// decodeEvent decodes one Dispatch event of LEGION-7, built from its fields, as the consumer
// would: the fact, and the fields the decoder could not read.
func decodeEvent(t *testing.T, eventType string, actor, payload map[string]any) decodedMessage {
	t.Helper()
	event, err := json.Marshal(map[string]any{
		"id": 40, "issue_key": "LEGION-7", "seq": 12, "type": eventType, "notify": true,
		"created_at": "2026-10-25T12:00:00Z", "actor": actor, "payload": payload,
	})
	if err != nil {
		t.Fatalf("encode the event: %v", err)
	}
	decoded, err := decodeMessage("notifications.dispatch.issue.LEGION-7."+eventType, "LEGION", nil, envelopeJSON(t, "dispatch-40", "dispatch", string(event)))
	if err != nil {
		t.Fatalf("decode %s: %v, want no poison", eventType, err)
	}
	return decoded
}

var askEventMark = record.EventMark{Seq: 12, At: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}

// A session's reply sent through a delivery's callback arrives as comment.answered, with the
// thread's ask, its state and whose turn it leaves, as a comment.created reply does: both decode to
// the same reply, and a reply on a closed ask decodes with no turn. A comment on no ask is no fact.
func TestACallbackReplyDecodesAsTheReplyItIs(t *testing.T) {
	architect := map[string]any{"kind": "session", "id": "session-architect"}
	for _, eventType := range []string{"comment.created", "comment.answered"} {
		decoded := decodeEvent(t, eventType, architect, map[string]any{"id": "comment-1", "ask_id": "ask-approval", "ask_state": "open", "ask_waiting_on": "human"})
		want := DispatchAskReply{Key: "LEGION-7", Ask: "ask-approval", WaitingOn: "human",
			Actor: dispatch.Actor{Kind: "session", ID: "session-architect"}, Mark: askEventMark}
		if !reflect.DeepEqual(decoded.Fact, want) {
			t.Fatalf("%s decoded to %#v, want %#v", eventType, decoded.Fact, want)
		}
		closed := decodeEvent(t, eventType, architect, map[string]any{"id": "comment-3", "ask_id": "ask-approval", "ask_state": "answered"})
		if want.WaitingOn = ""; !reflect.DeepEqual(closed.Fact, want) || len(closed.Unread) != 0 {
			t.Fatalf("%s on a closed ask decoded to %#v, unread %q; want %#v", eventType, closed.Fact, closed.Unread, want)
		}
		if plain := decodeEvent(t, eventType, architect, map[string]any{"id": "comment-2"}); plain.Fact != nil {
			t.Fatalf("%s on no ask decoded to %#v, want no fact", eventType, plain.Fact)
		}
	}
}

// Settlement restoring a decision block it retracted appends the ask's event again — ask.answered
// with the answer given before the retraction, or ask.opened with the block's original author —
// marked restored: nobody asked or answered anything, so each decodes to the ask's state alone,
// never the act the same event without the mark decodes as.
func TestARestoredAskEventIsNobodysAct(t *testing.T) {
	for _, tc := range []struct {
		name      string
		eventType string
		actor     map[string]any
		payload   map[string]any
		act       Fact
		restored  Fact
	}{
		{name: "an answer", eventType: "ask.answered", actor: map[string]any{"kind": "user", "id": "alice"},
			payload:  map[string]any{"id": "ask-question", "kind": "question", "state": "answered"},
			act:      DispatchAskClosed{Key: "LEGION-7", Ask: "ask-question", Kind: "question", State: "answered", Actor: dispatch.Actor{Kind: "user", ID: "alice"}, Mark: askEventMark},
			restored: DispatchAskRestored{Key: "LEGION-7", Ask: "ask-question", Kind: "question", Mark: askEventMark}},
		{name: "a question opened", eventType: "ask.opened", actor: map[string]any{"kind": "session", "id": "session-architect"},
			payload:  map[string]any{"id": "ask-question", "kind": "question", "author": map[string]any{"kind": "session", "id": "session-architect"}},
			act:      DispatchAskOpened{Key: "LEGION-7", Ask: "ask-question", Kind: "question", AuthorSession: "session-architect", Mark: askEventMark},
			restored: DispatchAskRestored{Key: "LEGION-7", Ask: "ask-question", Kind: "question", Open: true, Mark: askEventMark}},
	} {
		for _, restored := range []bool{false, true} {
			name, want := tc.name, tc.act
			payload := map[string]any{}
			for key, value := range tc.payload {
				payload[key] = value
			}
			if restored {
				name, want = name+", restored by settlement", tc.restored
				payload["restored"] = true
			}
			t.Run(name, func(t *testing.T) {
				if decoded := decodeEvent(t, tc.eventType, tc.actor, payload); !reflect.DeepEqual(decoded.Fact, want) {
					t.Fatalf("%s decoded to %#v, want %#v", name, decoded.Fact, want)
				}
			})
		}
	}
}

// An approval request its asker hands back to the person is Dispatch's ask.handed_back, whose
// payload is the ask: decoded as a reply that leaves the turn with the human, from the actor who
// handed it back.
func TestADispatchAskHandedBackIsAHumanTurnReplyFromItsActor(t *testing.T) {
	decoded := decodeEvent(t, "ask.handed_back", map[string]any{"kind": "session", "id": "session-architect"},
		map[string]any{"id": "ask-approval", "kind": "approval", "state": "open"})
	want := DispatchAskReply{Key: "LEGION-7", Ask: "ask-approval", Kind: "approval", WaitingOn: "human",
		Actor: dispatch.Actor{Kind: "session", ID: "session-architect"}, Mark: askEventMark}
	if !reflect.DeepEqual(decoded.Fact, want) {
		t.Fatalf("decoded ask.handed_back = %#v, want %#v", decoded.Fact, want)
	}
}

// Every ask.* event of an approval request carries the document it asks a person to approve, and
// each decodes with it; a question's events carry none.
func TestAnApprovalRequestsEventsCarryItsDocument(t *testing.T) {
	person := map[string]any{"kind": "user", "id": "alice"}
	approval := map[string]any{"artifact_id": "artifact-7", "version": 3, "requested_version": 3}
	for _, tc := range []struct {
		eventType string
		want      Fact
	}{
		{eventType: "ask.opened", want: DispatchAskOpened{Key: "LEGION-7", Ask: "ask-approval", Kind: "approval", Artifact: "artifact-7", Mark: askEventMark}},
		{eventType: "ask.answered", want: DispatchAskClosed{Key: "LEGION-7", Ask: "ask-approval", Kind: "approval", Artifact: "artifact-7", State: "answered",
			Actor: dispatch.Actor{Kind: "user", ID: "alice"}, Mark: askEventMark}},
		{eventType: "ask.handed_back", want: DispatchAskReply{Key: "LEGION-7", Ask: "ask-approval", Kind: "approval", Artifact: "artifact-7", WaitingOn: "human",
			Actor: dispatch.Actor{Kind: "user", ID: "alice"}, Mark: askEventMark}},
	} {
		decoded := decodeEvent(t, tc.eventType, person, map[string]any{"id": "ask-approval", "kind": "approval", "approval": approval})
		if !reflect.DeepEqual(decoded.Fact, tc.want) {
			t.Fatalf("%s decoded to %#v, want %#v", tc.eventType, decoded.Fact, tc.want)
		}
	}
	question := decodeEvent(t, "ask.opened", person, map[string]any{"id": "ask-q", "kind": "question", "approval": approval})
	if opened, ok := question.Fact.(DispatchAskOpened); !ok || opened.Artifact != "" {
		t.Fatalf("a question's opening decoded to %#v, want no document", question.Fact)
	}
}

// Dispatch appends ask.edited for the document's open approval request in a version's own
// transaction: an edit that leaves requested_version below the version it names moved the request,
// which Dispatch then shows waiting on its agent, and decodes as that move. Any other edit, of a
// request still at the version handed to the person or of a question, is no fact.
func TestAnEditThatMovesAnApprovalRequestDecodesAsItsMove(t *testing.T) {
	person := map[string]any{"kind": "user", "id": "alice"}
	moved := decodeEvent(t, "ask.edited", person, map[string]any{"id": "ask-approval", "kind": "approval", "quiet": true,
		"approval": map[string]any{"artifact_id": "artifact-7", "version": 4, "requested_version": 3}})
	if want := (DispatchApprovalMoved{Key: "LEGION-7", Ask: "ask-approval", Artifact: "artifact-7", Mark: askEventMark}); !reflect.DeepEqual(moved.Fact, want) {
		t.Fatalf("a moving edit decoded to %#v, want %#v", moved.Fact, want)
	}
	for name, payload := range map[string]map[string]any{
		"a request at its handed version": {"id": "ask-approval", "kind": "approval", "approval": map[string]any{"artifact_id": "artifact-7", "version": 4, "requested_version": 4}},
		"a question":                      {"id": "ask-q", "kind": "question"},
	} {
		if decoded := decodeEvent(t, "ask.edited", person, payload); decoded.Fact != nil {
			t.Fatalf("an edit of %s decoded to %#v, want no fact", name, decoded.Fact)
		}
	}
}

// An ask kind the daemon does not know — an event Dispatch recorded before it had questions and
// approvals, whose kind is `action` — decodes with no kind and names the kind as unread: the ask's
// event is not poison, and nothing it carries is lost but the kind.
func TestAnUnknownAskKindDecodesWithNoKindAndIsReported(t *testing.T) {
	person := map[string]any{"kind": "user", "id": "alice"}
	for _, tc := range []struct {
		eventType string
		payload   map[string]any
		want      Fact
	}{
		{eventType: "ask.opened", payload: map[string]any{"id": "ask-old", "kind": "action"},
			want: DispatchAskOpened{Key: "LEGION-7", Ask: "ask-old", Mark: askEventMark}},
		{eventType: "ask.answered", payload: map[string]any{"id": "ask-old", "kind": "action"},
			want: DispatchAskClosed{Key: "LEGION-7", Ask: "ask-old", State: "answered", Actor: dispatch.Actor{Kind: "user", ID: "alice"}, Mark: askEventMark}},
		{eventType: "ask.handed_back", payload: map[string]any{"id": "ask-old", "kind": "action"},
			want: DispatchAskReply{Key: "LEGION-7", Ask: "ask-old", WaitingOn: "human", Actor: dispatch.Actor{Kind: "user", ID: "alice"}, Mark: askEventMark}},
	} {
		t.Run(tc.eventType, func(t *testing.T) {
			decoded := decodeEvent(t, tc.eventType, person, tc.payload)
			if !reflect.DeepEqual(decoded.Fact, tc.want) {
				t.Fatalf("%s with kind action decoded to %#v, want %#v", tc.eventType, decoded.Fact, tc.want)
			}
			if len(decoded.Unread) != 1 || !strings.Contains(decoded.Unread[0], `"action"`) {
				t.Fatalf("%s with kind action reported %q, want the kind named as unread", tc.eventType, decoded.Unread)
			}
		})
	}
}

// An issue event carries the issue's claim: a holder, null for none, or nothing at all from a
// Dispatch that predates claims, which says nothing of the claim. A claim that names no actor is
// unknown and reported, never poison: the issue event is one the workflow acts on.
func TestADispatchIssueEventCarriesItsClaimHolder(t *testing.T) {
	original := string(capturedIssueUpdatedEnvelope(t))
	anchor := `\"resolved_repo\":\"sjawhar/legion\"`
	if !strings.Contains(original, anchor) || strings.Contains(original, `\"claim\"`) {
		t.Fatal("captured issue.updated fixture has no resolved_repo to place a claim beside, or already has a claim")
	}
	for _, tc := range []struct {
		name   string
		claim  string
		holder string
		known  bool
		unread bool
	}{
		{name: "a person's claim", claim: `,\"claim\":{\"actor\":{\"kind\":\"user\",\"id\":\"alice\"},\"at\":\"2026-09-23T00:46:36Z\"}`, holder: "user:alice", known: true},
		{name: "no claim", claim: `,\"claim\":null`, known: true},
		{name: "an event from before claims"},
		{name: "a claim naming no actor", claim: `,\"claim\":{\"actor\":{\"kind\":\"user\",\"id\":\"\"}}`, unread: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := decodeMessage("notifications.dispatch.issue.CAPTURE-3.issue.updated", "CAPTURE", capturedRepositories,
				[]byte(strings.Replace(original, anchor, anchor+tc.claim, 1)))
			if err != nil {
				t.Fatalf("decode: %v, want no poison", err)
			}
			issue := decoded.Fact.(DispatchIssue)
			if issue.ClaimedBy != tc.holder || issue.ClaimKnown != tc.known || (len(decoded.Unread) != 0) != tc.unread {
				t.Fatalf("claim = %q known %t unread %q, want %q known %t unread %t", issue.ClaimedBy, issue.ClaimKnown, decoded.Unread, tc.holder, tc.known, tc.unread)
			}
		})
	}
}

// A claim event whose claim cannot be named — issue.claimed with no claim, or a claim naming no
// actor — records nothing and is reported, as a reply whose turn the daemon cannot read is.
func TestAnUnreadableClaimOrTurnIsNoFact(t *testing.T) {
	person := map[string]any{"kind": "user", "id": "alice"}
	for _, tc := range []struct {
		name      string
		eventType string
		payload   map[string]any
	}{
		{name: "a claim with no claim", eventType: "issue.claimed", payload: map[string]any{"key": "LEGION-7", "claim": nil}},
		{name: "a claim naming no actor", eventType: "issue.claimed", payload: map[string]any{"key": "LEGION-7", "claim": map[string]any{"actor": map[string]any{"kind": "user"}}}},
		{name: "a release with no claim field", eventType: "issue.released", payload: map[string]any{"key": "LEGION-7"}},
		{name: "a reply on no turn", eventType: "comment.created", payload: map[string]any{"ask_id": "ask-q", "ask_waiting_on": "someone"}},
		{name: "a reply on an open ask with no turn", eventType: "comment.created", payload: map[string]any{"ask_id": "ask-q", "ask_state": "open"}},
		{name: "a reply on a closed ask with a turn", eventType: "comment.created", payload: map[string]any{"ask_id": "ask-q", "ask_state": "answered", "ask_waiting_on": "human"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded := decodeEvent(t, tc.eventType, person, tc.payload)
			if decoded.Fact != nil || len(decoded.Unread) == 0 && tc.eventType != "issue.released" {
				t.Fatalf("decoded %#v, unread %q; want no fact and the field reported", decoded.Fact, decoded.Unread)
			}
		})
	}
}
