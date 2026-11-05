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

func TestAnExplicitCommentMentionNotifiesASessionOutsideEveryOtherRoute(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	for i, tc := range []struct {
		name, outcome        string
		notify               bool
		actor                model.Actor
		wantSession, pending bool
	}{
		{"failed agent mention", "failed", false, model.Actor{Kind: "session", ID: "writer"}, true, false},
		{"no attempt", "", false, model.Actor{Kind: "session", ID: "writer"}, true, false},
		{"lapsed pending", "abandoned", true, model.Actor{Kind: "user", ID: "alice"}, false, true},
		{"sent", "sent", true, model.Actor{Kind: "user", ID: "alice"}, false, false},
		{"pending", "pending", false, model.Actor{Kind: "session", ID: "writer"}, false, true},
		{"self", "failed", false, model.Actor{Kind: "session", ID: "reader"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := []string{"00000000-0000-0000-0000-000000000011", "00000000-0000-0000-0000-000000000012", "00000000-0000-0000-0000-000000000013", "00000000-0000-0000-0000-000000000014", "00000000-0000-0000-0000-000000000015", "00000000-0000-0000-0000-000000000016"}[i]
			seedMentionDelivery(t, database, id, "reader", tc.outcome)
			publisher := &recordingPublisher{}
			event := model.Event{ID: 100, IssueKey: new("T-1"), Type: "comment.created", Actor: tc.actor,
				Payload: map[string]any{"id": id, "body": "Please read", "mentions": mentionsOf("reader")}, Notify: tc.notify, CreatedAt: time.Now()}
			err := publish(context.Background(), Deps{Store: database, Publisher: publisher}, event, "", routing{}, map[string]struct{}{})
			if tc.pending && !errors.Is(err, errMentionPending) || !tc.pending && err != nil {
				t.Fatalf("publish = %v", err)
			}
			want := []string{"notifications.dispatch.issue.T-1.comment.created"}
			if tc.wantSession {
				want = append(want, "notifications.agent.reader")
			}
			if got := topicsOf(publisher.all()); !reflect.DeepEqual(got, want) {
				t.Fatalf("topics = %v, want %v", got, want)
			}
		})
	}
}
