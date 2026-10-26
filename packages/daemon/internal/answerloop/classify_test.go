package answerloop

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// askAuthors answers each ask's author.
type askAuthors map[string]string

func (a askAuthors) Ask(_ context.Context, id string) (dispatch.Ask, error) {
	return dispatch.Ask{ID: id, IssueKey: new("LEGION-9"), Author: dispatch.Actor{Kind: "session", ID: a[id]}}, nil
}

// Every kind of hand-off is classified before any model call: what the agent answers (a message
// sent to it, a comment mentioning it, a reply on an ask it opened, another agent's Envoy message),
// the decisions (a person's answer to its ask, the ask handed back, a person's reply on its ask,
// which is answered too), and everything it never answers.
func TestEveryHandoffIsClassifiedBeforeAnyModelCall(t *testing.T) {
	const ownAsk, othersAsk = "a-own", "a-other"
	asks := askAuthors{ownAsk: "ses_tester", othersAsk: "ses_reviewer"}
	agent := intake.DispatchActor{Kind: "session", ID: "ses_asker"}
	daemon := intake.DispatchActor{Kind: "session", ID: "legion-daemon:LEGION"}
	comment := func(id string, ask *string, body string) map[string]any {
		return map[string]any{"id": id, "issue_key": "LEGION-9", "body": body, "ask_id": ask}
	}
	mention := map[string]any{"attempt": 1, "mode": "btw", "comment_id": "c-1", "target": "role:legion-legion-legion-9-tester"}
	for _, tc := range []struct {
		name     string
		handoff  intake.Handoff
		kind     Kind // "" when it is no question
		decision bool
		maySkip  bool
	}{
		{name: "a message sent to the agent", handoff: messageHandoff("ses_tester", "m-1", 1, "Why plum?"), kind: KindMessage},
		{name: "a message on the conversation page", kind: KindMessage, handoff: dispatchHandoff("ses_tester", frame("message.created", person, nil,
			map[string]any{"id": "m-2", "issue_key": nil, "body": "Hi?", "target": "session:ses_tester"}, map[string]any{"attempt": 1, "mode": "btw"}))},
		{name: "a message not sent to the agent", handoff: dispatchHandoff("ses_tester", bare("message.created", person, new("LEGION-9"),
			map[string]any{"id": "m-3", "body": "status?"}))},
		{name: "a comment mentioning the agent", kind: KindMention,
			handoff: dispatchHandoff("ses_tester", frame("comment.created", agent, new("LEGION-9"), comment("c-1", nil, "@tester why?"), mention))},
		{name: "an agent's reply on the agent's ask", kind: KindAskReply,
			handoff: dispatchHandoff("ses_tester", bare("comment.created", agent, new("LEGION-9"), comment("c-2", new(ownAsk), "Which run?")))},
		{name: "a person's reply on the agent's ask", kind: KindAskReply, decision: true, maySkip: true,
			handoff: dispatchHandoff("ses_tester", bare("comment.created", person, new("LEGION-9"), comment("c-3", new(ownAsk), "Use B. Why A?")))},
		{name: "a reply on an ask the agent follows", handoff: dispatchHandoff("ses_tester",
			bare("comment.created", person, new("LEGION-9"), comment("c-4", new(othersAsk), "Why?")))},
		{name: "a reply on the agent's comment thread", handoff: dispatchHandoff("ses_tester",
			bare("comment.created", person, new("LEGION-9"), comment("c-5", nil, "ok")))},
		{name: "a person's answer to the agent's ask", decision: true, handoff: dispatchHandoff("ses_tester",
			bare("ask.answered", person, new("LEGION-9"), map[string]any{"id": ownAsk}))},
		{name: "the agent's ask handed back", decision: true, handoff: dispatchHandoff("ses_tester",
			bare("ask.handed_back", person, new("LEGION-9"), map[string]any{"id": ownAsk}))},
		{name: "an answer to another's ask", handoff: dispatchHandoff("ses_tester",
			bare("ask.answered", person, new("LEGION-9"), map[string]any{"id": othersAsk}))},
		{name: "an edit", handoff: dispatchHandoff("ses_tester", bare("comment.edited", person, new("LEGION-9"), comment("c-6", new(ownAsk), "edited")))},
		{name: "a message edit", handoff: dispatchHandoff("ses_tester", bare("message.edited", person, new("LEGION-9"), map[string]any{"id": "m-4"}))},
		{name: "a resolution", handoff: dispatchHandoff("ses_tester", bare("comment.resolved", person, new("LEGION-9"), comment("c-7", new(ownAsk), "")))},
		{name: "an ask resolved", handoff: dispatchHandoff("ses_tester", bare("ask.resolved", person, new("LEGION-9"), map[string]any{"id": ownAsk}))},
		{name: "a reply already answered", handoff: dispatchHandoff("ses_tester", bare("message.answered", agent, new("LEGION-9"), map[string]any{"id": "m-5"}))},
		{name: "a delivery receipt", handoff: dispatchHandoff("ses_tester", bare("message.delivery", agent, new("LEGION-9"), map[string]any{"id": "m-6"}))},
		{name: "a claim", handoff: dispatchHandoff("ses_tester", bare("issue.claimed", agent, new("LEGION-9"), map[string]any{"key": "LEGION-9"}))},
		{name: "an issue event", handoff: dispatchHandoff("ses_tester", bare("issue.updated", person, new("LEGION-9"), map[string]any{"key": "LEGION-9"}))},
		{name: "the daemon's own post", handoff: dispatchHandoff("ses_tester", frame("message.created", daemon, new("LEGION-9"),
			map[string]any{"id": "m-7", "body": "READY"}, map[string]any{"attempt": 1, "mode": "btw"}))},
		{name: "the loop's own answer, by its body marker", handoff: dispatchHandoff("ses_tester", frame("message.created", agent, new("LEGION-9"),
			map[string]any{"id": "m-8", "body": "Plum.\n\n<!-- legion-answer:abc -->"}, map[string]any{"attempt": 1, "mode": "btw"}))},
		{name: "the loop's gh comment, by its footer's answer key", handoff: dispatchHandoff("ses_tester", frame("comment.created", agent, new("LEGION-9"),
			comment("c-8", nil, `Done. <!-- legion: {"session":"ses_x","phase":"testing","answer":"q.1"} -->`), mention))},
		{name: "another agent's Envoy message", kind: KindEnvoy, maySkip: true,
			handoff: envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-1", "agent.ses_tester.0123456789abcdef0123456789abcdef", "Which fruit?")},
		{name: "an Envoy message with the loop's dedupe key", handoff: envoyHandoff("notifications.role.x", "ses_tester", "evt-2", "legion-answer:q1", "Plum.")},
		{name: "the loop's answer sent by Envoy", handoff: envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-3",
			"agent.ses_tester.0123456789abcdef0123456789abcdef", "Plum.\n\n<!-- legion-answer:q2 -->")},
		{name: "the daemon's notice", handoff: envoyHandoff("notifications.role.x", "ses_tester", "evt-4", "legion-outbox:42", "worker-died")},
		{name: "the broker's message", handoff: func() intake.Handoff {
			h := envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-5", "envoy.wake.1", "wake")
			h.Envelope.Source = "envoy"
			return h
		}()},
		{name: "a person's Envoy message", handoff: func() intake.Handoff {
			h := envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-6", "x", "hi")
			h.Envelope.Source, h.Envelope.SourceSession = "human", ""
			return h
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, err := Classify(context.Background(), tc.handoff, store.AgentConversation{Session: "ses_tester"}, asks)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.kind == "" && verdict.Question != nil:
				t.Fatalf("answered as a %s question; want none (%s)", verdict.Question.Kind, verdict.Because)
			case tc.kind != "" && verdict.Question == nil:
				t.Fatalf("not answered (%s); want a %s question", verdict.Because, tc.kind)
			case tc.kind != "" && (verdict.Question.Kind != tc.kind || verdict.Question.MayAskNothing != tc.maySkip):
				t.Fatalf("question = %+v; want kind %s, may ask nothing %t", *verdict.Question, tc.kind, tc.maySkip)
			case tc.decision != (verdict.Decision != nil):
				t.Fatalf("decision = %t; want %t", verdict.Decision != nil, tc.decision)
			}
		})
	}
}

// A question's identity is the Dispatch message or comment it came from, whichever delivery
// attempt carried it, and an Envoy message's dedupe key.
func TestAQuestionsIdentityIsItsMessageOrDedupeKey(t *testing.T) {
	first, err := ReadOrigin(messageHandoff("ses_tester", "m-1", 1, "Why?"))
	if err != nil {
		t.Fatal(err)
	}
	later, err := ReadOrigin(messageHandoff("ses_tester", "m-1", 2, "Why?"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Identity != "dispatch:m-1" || later.Identity != first.Identity || later.Attempt != 2 {
		t.Fatalf("identities = %q, %q; want dispatch:m-1 for both attempts", first.Identity, later.Identity)
	}
	envoy, err := ReadOrigin(envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-1", "agent.ses_tester.k1", "Which?"))
	if err != nil {
		t.Fatal(err)
	}
	if envoy.Identity != "agent.ses_tester.k1" || envoy.Sender != "ses_asker" || envoy.EventID != "evt-1" {
		t.Fatalf("Envoy origin = %+v", envoy)
	}
}

// Who asked is named for the model and for the controller a question is passed to: a person by
// their Dispatch login, an agent by its session, with the session's title when the listener
// stamped one on the envelope; where is the dispatch:// reference or the Envoy event.
func TestAQuestionsAskerIsTheLoginOrTheSessionWithItsTitle(t *testing.T) {
	person, err := ReadOrigin(messageHandoff("ses_tester", "m-1", 1, "Why?"))
	if err != nil {
		t.Fatal(err)
	}
	if person.Asker != "user:person@example.com" || person.Ref != "dispatch://LEGION-9/message/m-1" {
		t.Fatalf("Dispatch origin asker %q, ref %q; want the login and the message's reference", person.Asker, person.Ref)
	}
	untitled := envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-1", "agent.ses_tester.k1", "Which?")
	agent, err := ReadOrigin(untitled)
	if err != nil {
		t.Fatal(err)
	}
	if agent.Asker != "session:ses_asker" || agent.Ref != "envoy:evt-1" {
		t.Fatalf("Envoy origin asker %q, ref %q; want the session and the event", agent.Asker, agent.Ref)
	}
	titled := untitled
	titled.Envelope.Sender = &intake.HandoffSender{SessionID: "ses_asker", Title: "LEGION-9 implementer"}
	if agent, err = ReadOrigin(titled); err != nil {
		t.Fatal(err)
	}
	if agent.Asker != "session:ses_asker (LEGION-9 implementer)" {
		t.Fatalf("Envoy origin with a stamped sender asker %q; want the session and its title", agent.Asker)
	}
}
