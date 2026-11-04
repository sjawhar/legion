package persongraph

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

func TestDeliveryCensusChecksEmptySetsAndDetectsMissingAndUnexpectedRecipients(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('CORE-1', 'CORE', 1, 'Graph', '{"kind":"user","id":"alice"}', 'U');
		insert into comments (id, issue_key, author, body) values
		('00000000-0000-0000-0000-000000000001', 'CORE-1', '{"kind":"user","id":"alice"}', 'Reached');
		insert into messages (id, issue_key, author, body) values
		('00000000-0000-0000-0000-000000000002', 'CORE-1', '{"kind":"user","id":"alice"}', 'No delivery');
		insert into asks (id, issue_key, author, question) values
		('00000000-0000-0000-0000-000000000003', 'CORE-1', '{"kind":"session","id":"asker"}', 'Ready?');
		insert into events (issue_key, seq, type, actor, payload, notify, published_destinations) values
		('CORE-1', 1, 'comment.created', '{"kind":"user","id":"alice"}', '{"id":"00000000-0000-0000-0000-000000000001"}', true,
		array['notifications.agent.reader', 'notifications.role.reviewer', 'notifications.dispatch.issue.CORE-1.comment.created', 'notifications.agent.']),
		('CORE-1', 2, 'ask.answered', '{"kind":"user","id":"alice"}', '{"id":"00000000-0000-0000-0000-000000000003"}', true, array['notifications.agent.asker']);
	`); err != nil {
		t.Fatal(err)
	}
	since := time.Now().AddDate(0, 0, -30)
	report, err := Census(ctx, database.Pool, since)
	if err != nil {
		t.Fatal(err)
	}
	if report.Checked != 3 || report.Mismatched != 0 || report.ByKind["comment"] != 1 || report.ByKind["message"] != 1 || report.ByKind["ask"] != 1 {
		t.Fatalf("census = %+v", report)
	}
	if _, err := database.Pool.Exec(ctx, `
		alter view graph_person_edges rename to fixture_graph_person_edges;
		create view graph_person_edges as select * from fixture_graph_person_edges where kind <> 'delivered_to'
		union all select 'message', '00000000-0000-0000-0000-000000000002', 'delivered_to', 'session', 'unrecorded', now(), null::bigint;
	`); err != nil {
		t.Fatal(err)
	}
	report, err = Census(ctx, database.Pool, since)
	if err != nil {
		t.Fatal(err)
	}
	if report.Checked != 3 || report.Mismatched != 3 || report.Missing["session_topic"] != 2 || report.Missing["role_topic"] != 1 || report.Unexpected["unrecorded"] != 1 {
		t.Fatalf("corrupt view census = %+v", report)
	}
}
