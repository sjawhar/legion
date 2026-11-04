package persongraph

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestGraphAndCensusIncludeOnlySentAttemptsAndDeduplicateRecipients(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('CORE-1', 'CORE', 1, 'Graph', '{"kind":"user","id":"alice"}', 'U');
		insert into comments (id, issue_key, author, body) values
		('00000000-0000-0000-0000-000000000001', 'CORE-1', '{"kind":"user","id":"alice"}', 'Mention');
		insert into messages (id, author, body, target) values
		('00000000-0000-0000-0000-000000000002', '{"kind":"user","id":"alice"}', 'Direct', 'session:message-reader');
		insert into messages (id, author, body, created_at, target) values
		('00000000-0000-0000-0000-000000000003', '{"kind":"user","id":"alice"}', 'Before the window', now() - interval '31 days', 'session:old-reader');
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state) values
		('00000000-0000-0000-0000-000000000001', 'session:comment-reader', 1, 'aside', 'comment-reader', 'sent'),
		('00000000-0000-0000-0000-000000000001', 'session:comment-reader', 2, 'aside', 'comment-reader', 'sent'),
		('00000000-0000-0000-0000-000000000001', 'session:failed', 1, 'aside', 'failed', 'failed'),
		('00000000-0000-0000-0000-000000000001', 'role:pending', 1, 'aside', null, 'pending');
		insert into message_deliveries (message_id, attempt, delivery, session_id, state) values
		('00000000-0000-0000-0000-000000000002', 1, 'aside', 'message-reader', 'sent'),
		('00000000-0000-0000-0000-000000000002', 2, 'aside', 'failed', 'failed'),
		('00000000-0000-0000-0000-000000000003', 1, 'aside', 'old-reader', 'sent');
	`); err != nil {
		t.Fatal(err)
	}
	report, err := Census(ctx, database.Pool, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.Checked != 2 || report.Mismatched != 0 {
		t.Fatalf("sent-attempt census = %+v", report)
	}
	rows, err := database.Pool.Query(ctx, `select distinct from_kind || ':' || to_id from graph_person_edges where kind = 'delivered_to' order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		got = append(got, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	want := []string{"comment:comment-reader", "message:message-reader", "message:old-reader"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sent delivery graph = %v, want %v", got, want)
	}
}
