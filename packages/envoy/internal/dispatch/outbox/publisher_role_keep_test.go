package outbox

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A role-routed event is published raw onto the role lane, which keeps a message it cannot deliver
// for the role's next holder. This publisher holds its own outbox and its own receipts and
// redelivers from them, so a copy the listener kept would reach that holder a second time: the
// envelope says so (contracts.Envelope.Keep false) and the lane raises no_holder for it instead.
// The event's own owner topic is published as it always was, with no flag on it: the stream
// retains it and no role lane ever sees it.
func TestARoleRoutedEventTellsTheRoleLaneToKeepNothing(t *testing.T) {
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
			if item.Keep == nil || *item.Keep {
				t.Fatalf("the copy on %s carries keep %v, want false: this publisher redelivers it itself", item.Topic, item.Keep)
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
