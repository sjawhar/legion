package outbox

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestPersonGraphDeliveredToEqualsTopicsReachedByPublisher(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", new("session:reader"))
	seedIssue(t, database, "T-2", new("role:reviewer"))
	alice := model.Actor{Kind: "user", ID: "alice"}
	ask := seedAsk(t, database, "T-1", model.Actor{Kind: "session", ID: "asker"}, "Ready?")
	comment := seedComment(t, database, "T-1", alice, "A comment", nil)
	message := seedMessage(t, database, "T-1", alice, "A message", nil)
	roleComment := seedComment(t, database, "T-2", alice, "For the role", nil)
	type source struct { kind, id, issue, event string }
	sources := []source{{"ask", ask, "T-1", "ask.answered"}, {"comment", comment, "T-1", "comment.created"}, {"message", message, "T-1", "message.created"}, {"comment", roleComment, "T-2", "comment.created"}}
	eventIDs := map[string]string{}
	for _, source := range sources {
		event := appendEvent(t, database, broker, model.Event{IssueKey: new(source.issue), Type: source.event, Actor: alice, Payload: map[string]any{"id": source.id, "body": "Words"}})
		eventIDs[source.id] = fmt.Sprint(event.ID)
	}
	publisher := &recordingPublisher{}
	deps := withHeldRoles(Deps{Store: database, Publisher: publisher})
	if _, err := scanBatch(ctx, deps, unfenced{}, holding(t, deps).instance); err != nil { t.Fatal(err) }
	for _, source := range sources {
		want := []string{}
		for _, envelope := range publisher.all() {
			if envelope.SourceEventID != eventIDs[source.id] { continue }
			if id, ok := strings.CutPrefix(envelope.Topic, contracts.AgentTopicPrefix); ok { want = append(want, "session:"+id) }
			if id, ok := strings.CutPrefix(envelope.Topic, contracts.RoleTopicPrefix); ok { want = append(want, "role:"+id) }
		}
		slices.Sort(want)
		want = slices.Compact(want)
		if len(want) == 0 { t.Fatalf("publisher reached no recorded recipient for %s:%s", source.kind, source.id) }
		rows, err := database.Pool.Query(ctx, `select to_kind || ':' || to_id from graph_edges where from_kind = $1 and from_id = $2 and kind = 'delivered_to' order by 1`, source.kind, source.id)
		if err != nil { t.Fatal(err) }
		got := []string{}
		for rows.Next() {
			var recipient string
			if err := rows.Scan(&recipient); err != nil { t.Fatal(err) }
			got = append(got, recipient)
		}
		if err := rows.Err(); err != nil { t.Fatal(err) }
		rows.Close()
		got = slices.Compact(got)
		if !slices.Equal(got, want) { t.Fatal(fmt.Sprintf("%s:%s graph=%v publisher=%v", source.kind, source.id, got, want)) }
	}
}
