package outbox

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestIndexedSessionMentionNotifiesOnceAlongsideExplicitMentions(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedIssue(t, database, "T-1", nil)
	for i, tc := range []struct {
		name, outcome         string
		explicit, wantSession bool
	}{
		{"indexed only", "", false, true},
		{"both failed", "failed", true, true},
		{"both already sent", "sent", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := []string{"00000000-0000-0000-0000-000000000021", "00000000-0000-0000-0000-000000000022", "00000000-0000-0000-0000-000000000023"}[i]
			if _, err := database.Pool.Exec(ctx, `insert into comments(id,issue_key,author,body) values ($1,'T-1','{"kind":"session","id":"writer"}','Body dispatch://sessions/reader')`, id); err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{"id": id, "body": "Body dispatch://sessions/reader"}
			if tc.explicit {
				payload["mentions"] = mentionsOf("reader")
				seedMentionDelivery(t, database, id, "reader", tc.outcome)
			}
			publisher := &recordingPublisher{}
			event := model.Event{ID: 100, IssueKey: new("T-1"), Type: "comment.created", Actor: model.Actor{Kind: "session", ID: "writer"}, Payload: payload, CreatedAt: time.Now()}
			if err := publish(ctx, Deps{Store: database, Publisher: publisher}, event, "", routing{}, map[string]struct{}{}); err != nil {
				t.Fatal(err)
			}
			want := []string{"notifications.dispatch.issue.T-1.comment.created"}
			if tc.wantSession {
				want = append(want, "notifications.agent.reader")
			}
			if got := topicsOf(publisher.all()); !reflect.DeepEqual(got, want) {
				t.Fatalf("topics=%v want=%v", got, want)
			}
		})
	}
}
