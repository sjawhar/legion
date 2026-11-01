package events

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestCommentDeliveryDoesNotNotifyRoutesForHumanAuthors(t *testing.T) {
	broker := NewBroker()
	if broker.Notify(model.Event{Type: "comment.delivery", Actor: model.Actor{Kind: "user", ID: "alice"}}) {
		t.Fatal("comment.delivery woke routes for a human author, want delivery-attempt status events to stay non-notifying")
	}
}
