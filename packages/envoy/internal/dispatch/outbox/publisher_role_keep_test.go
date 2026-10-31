package outbox

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A role-routed event is published raw onto the role lane, and carries no keep flag
// (contracts.Envelope.Keep), so the listener keeps it for the role's next holder as it keeps any
// role message. This publisher sends a role route once and never again: it marks the event
// published as soon as NATS accepted it, which for a role topic is a core flush with no receipt
// behind it, and nothing here reads the no_holder exception, so without the kept lane an event
// routed to a role no session held was dropped. The only repeat it makes is the at-least-once
// after a claim-window race, whose copy the holder drops by the dedupe key a kept copy carries
// too. The event's own owner topic carries no flag either: the stream retains it and no role lane
// ever sees it.
func TestARoleRoutedEventIsKeptForTheRolesNextHolder(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", new("role:reviewer"))
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}

	commentID := seedComment(t, database, "T-1", alice, "Reviewers, please look.", nil)
	appendEvent(t, database, broker, model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
		"id": commentID, "body": "Reviewers, please look.",
	}})

	publisher := &recordingPublisher{}
	scan(ctx, Deps{Store: database, Publisher: publisher})

	roleTopic := contracts.RoleTopicPrefix + "reviewer"
	var onTheRoleLane, onTheOwnerTopic int
	for _, item := range publisher.all() {
		if item.Topic == roleTopic {
			onTheRoleLane++
			if item.Keep != nil {
				t.Fatalf("the copy on %s carries keep %v, want no flag at all: the listener keeps it for the role's next holder", item.Topic, *item.Keep)
			}
			continue
		}
		onTheOwnerTopic++
		if item.Keep != nil {
			t.Fatalf("the copy on %s carries keep %v, want no flag at all", item.Topic, *item.Keep)
		}
	}
	if onTheRoleLane != 1 || onTheOwnerTopic == 0 {
		t.Fatalf("the event reached %d role lanes and %d other topics, want one of each at least", onTheRoleLane, onTheOwnerTopic)
	}
}
