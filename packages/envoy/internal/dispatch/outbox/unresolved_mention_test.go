package outbox

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMentionFallbackWaitsForAPendingRoleWithoutASessionPin(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	const id = "00000000-0000-0000-0000-000000000054"
	seedMentionDelivery(t, database, id, "reader", "abandoned")
	if _, err := database.Pool.Exec(context.Background(), `update comment_deliveries set target='role:reviewer',session_id=null where comment_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	publisher := &recordingPublisher{}
	event := model.Event{ID: 104, IssueKey: new("T-1"), Type: "comment.created", Actor: model.Actor{Kind: "session", ID: "writer"}, Payload: map[string]any{"id": id, "body": "dispatch://sessions/reader"}, CreatedAt: time.Now()}
	err := publish(context.Background(), Deps{Store: database, Publisher: publisher}, event, "", routing{}, map[string]struct{}{})
	if !errors.Is(err, errMentionPending) {
		t.Fatalf("unresolved pending role=%v", err)
	}
	if got := topicsOf(publisher.all()); !reflect.DeepEqual(got, []string{"notifications.dispatch.issue.T-1.comment.created"}) {
		t.Fatalf("unresolved role may still resolve to fallback recipient: %v", got)
	}
}
