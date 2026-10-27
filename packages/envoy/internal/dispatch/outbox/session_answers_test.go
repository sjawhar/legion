package outbox

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestSessionAnswersReadsWhatThePublisherRecorded publishes real events through the outbox's own
// publish path (scan, which calls publish for each pending event) and reads the session's list back
// with store.SessionAnswers, the read GET /api/v1/agents/{session_id}/answers serves: S1 holds
// exactly the person's events whose published_destinations the publisher gave the session's own
// topic, and never an agent's event that reached it, nor a person's event that reached only a role.
func TestSessionAnswersReadsWhatThePublisherRecorded(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", new("role:reviewer"))
	ctx := context.Background()
	asker := model.Actor{Kind: "session", ID: "session-asker"}
	helper := model.Actor{Kind: "session", ID: "session-helper"}
	alice := model.Actor{Kind: "user", ID: "alice"}

	askID := seedAsk(t, database, "T-1", asker, "Which database backs the queue?")
	at := time.Now().UTC().Truncate(time.Microsecond)
	answer := model.AskAnswer{User: "alice", Selected: []string{"Postgres"}, Text: new("Postgres, for the pool."), At: at}
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode answer: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1`, askID, encoded); err != nil {
		t.Fatalf("answer ask: %v", err)
	}
	answered := appendEvent(t, database, broker, model.Event{IssueKey: new("T-1"), Type: "ask.answered", Actor: alice, Payload: model.Ask{
		ID: askID, IssueKey: new("T-1"), Question: "Which database backs the queue?", State: "answered", Answer: &answer,
	}})

	// replyOnAsk writes a reply on the ask as author, and its comment.created event.
	replyOnAsk := func(author model.Actor, body string) (string, model.Event) {
		t.Helper()
		authorJSON, err := json.Marshal(author)
		if err != nil {
			t.Fatalf("encode reply author: %v", err)
		}
		var id string
		if err := database.Pool.QueryRow(ctx, `
			insert into comments (issue_key, author, body, ask_id) values ('T-1', $1, $2, $3) returning id::text
		`, authorJSON, body, askID).Scan(&id); err != nil {
			t.Fatalf("create reply: %v", err)
		}
		event := appendEvent(t, database, broker, model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: author, Payload: map[string]any{
			"id": id, "ask_id": askID, "body": body,
		}})
		return id, event
	}
	seedFollower(t, database, askID, helper.ID)
	agentReplyID, agentReply := replyOnAsk(helper, "Postgres, I would say.")
	personReplyID, personReply := replyOnAsk(alice, "And the pool size is 4.")

	// A person's comment on the issue, which is routed to a role: its topic is no session's.
	commentID := seedComment(t, database, "T-1", alice, "Reviewers, please look.", nil)
	roleComment := appendEvent(t, database, broker, model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
		"id": commentID, "body": "Reviewers, please look.",
	}})

	scan(ctx, Deps{Store: database, Publisher: &recordingPublisher{}})
	destinations := func(event model.Event) []string {
		t.Helper()
		var topics []string
		if err := database.Pool.QueryRow(ctx, `select published_destinations from events where id = $1 and published_at is not null`, event.ID).Scan(&topics); err != nil {
			t.Fatalf("read event %d destinations: %v", event.ID, err)
		}
		return topics
	}
	askerTopic := contracts.AgentSubject(asker.ID)
	for name, event := range map[string]model.Event{"the answer": answered, "the agent's reply": agentReply, "the person's reply": personReply} {
		if topics := destinations(event); !slices.Contains(topics, askerTopic) {
			t.Fatalf("%s reached %v, want the asker's own topic", name, topics)
		}
	}
	if topics := destinations(roleComment); !slices.Contains(topics, contracts.RoleTopicPrefix+"reviewer") || slices.Contains(topics, askerTopic) {
		t.Fatalf("the routed comment reached %v, want the role's topic and not the asker's", topics)
	}

	items, err := store.SessionAnswers(ctx, database.Pool, asker.ID, time.Now().Add(-72*time.Hour))
	if err != nil {
		t.Fatalf("read the asker's answers: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("asker's answers = %+v, want the person's reply and the answer, and not the agent's reply %s or the routed comment %s", items, agentReplyID, commentID)
	}
	reply, current := items[0], items[1]
	if reply.Kind != model.SessionAnswerKindComment || reply.ID != personReplyID || reply.Relation != model.SessionAnswerRelationRepliedOnYourAsk ||
		reply.Text != "And the pool size is 4." || reply.Ask == nil || reply.Ask.ID != askID {
		t.Fatalf("newest item = %+v, want alice's reply on the ask", reply)
	}
	if current.Kind != model.SessionAnswerKindAnswer || current.ID != askID || current.Relation != model.SessionAnswerRelationAnsweredYourAsk ||
		current.Author != alice || current.Text != "Postgres, for the pool." || !current.At.Equal(at) || current.Replaced {
		t.Fatalf("older item = %+v, want alice's answer", current)
	}
	if want := "dispatch://T-1/ask/" + askID; current.Ref == nil || *current.Ref != want || current.Href != "/issues/T-1/asks/"+askID {
		t.Fatalf("answer place = %v %q, want %s", current.Ref, current.Href, want)
	}
	if other, err := store.SessionAnswers(ctx, database.Pool, "session-nobody", time.Now().Add(-72*time.Hour)); err != nil || len(other) != 0 || other == nil {
		t.Fatalf("a session nothing reached = %#v, %v; want an empty list", other, err)
	}
}
