package refs

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestPersonNodesResolveCanonicalAliasesWithoutChangingDefaultReferenceReads(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	comment := createReferenceComment(t, tx, "CORE-1")
	if _, err := tx.Exec(ctx, `update comments set author = '{"kind":"user","id":" ALICE "}' where id = $1`, comment); err != nil {
		t.Fatal(err)
	}
	person, found, err := Node(ctx, tx, "person", "alice", false)
	if err != nil || !found || person.Kind != "person" || person.ID != "alice" || person.Ref != "" {
		t.Fatalf("person node = %+v found=%v err=%v", person, found, err)
	}
	if _, found, err := Node(ctx, tx, "person", "nobody", false); err != nil || found {
		t.Fatalf("unknown person found=%v err=%v", found, err)
	}
	links, err := Edges(ctx, tx, Query{Direction: "out", Kind: "comment", ID: comment})
	if err != nil || len(links) != 0 {
		t.Fatalf("default links = %+v err=%v", links, err)
	}
	links, err = Edges(ctx, tx, Query{Direction: "out", Kind: "comment", ID: comment, Kinds: []string{"authored_by"}})
	if err != nil || len(links) != 1 || links[0].Node.Kind != "person" || links[0].Node.ID != "alice" {
		t.Fatalf("explicit authorship = %+v err=%v", links, err)
	}
}

// The person page and the delivery census read receipts through SessionReceiptsSQL alone, so
// what it answers is what both of them see: the recorded destinations naming one of the asked
// sessions, and that session's sent attempts, each no older than the asked bound. Another
// session's topic, a role topic, an attempt that failed or is still pending, and anything
// before the bound are not receipts.
func TestSessionReceiptsReadsOnlyTheAskedSessionsRecordedDeliveries(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('CORE-1', 'CORE', 1, 'Receipts', '{"kind":"user","id":"alice"}', 'U');
		insert into comments (id, issue_key, author, body) values
		('00000000-0000-0000-0000-000000000001', 'CORE-1', '{"kind":"user","id":"alice"}', 'Reached by topic'),
		('00000000-0000-0000-0000-000000000002', 'CORE-1', '{"kind":"user","id":"alice"}', 'Reached by attempt'),
		('00000000-0000-0000-0000-000000000003', 'CORE-1', '{"kind":"user","id":"alice"}', 'Reached by nobody');
		insert into messages (id, issue_key, author, body, target) values
		('00000000-0000-0000-0000-000000000004', 'CORE-1', '{"kind":"user","id":"alice"}', 'Direct', 'session:wanted');
		insert into asks (id, issue_key, author, question) values
		('00000000-0000-0000-0000-000000000005', 'CORE-1', '{"kind":"user","id":"alice"}', 'Ready?');
		insert into events (issue_key, seq, type, actor, payload, notify, published_destinations) values
		('CORE-1', 1, 'comment.created', '{"kind":"user","id":"alice"}', '{"id":"00000000-0000-0000-0000-000000000001"}', true,
		 array['notifications.agent.wanted', 'notifications.agent.other', 'notifications.role.reviewer']),
		('CORE-1', 2, 'ask.answered', '{"kind":"user","id":"alice"}', '{"id":"00000000-0000-0000-0000-000000000005"}', true,
		 array['notifications.agent.wanted']),
		('CORE-1', 3, 'comment.created', '{"kind":"user","id":"alice"}', '{"id":"00000000-0000-0000-0000-000000000003"}', true,
		 array['notifications.agent.other']);
		update events set created_at = now() - interval '31 days' where seq = 2;
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state) values
		('00000000-0000-0000-0000-000000000002', 'session:wanted', 1, 'aside', 'wanted', 'sent'),
		('00000000-0000-0000-0000-000000000002', 'session:wanted', 2, 'aside', 'wanted', 'sent'),
		('00000000-0000-0000-0000-000000000003', 'session:wanted', 1, 'aside', 'wanted', 'failed'),
		('00000000-0000-0000-0000-000000000003', 'role:pending', 1, 'aside', null, 'pending');
		insert into message_deliveries (message_id, attempt, delivery, session_id, state) values
		('00000000-0000-0000-0000-000000000004', 1, 'aside', 'wanted', 'sent'),
		('00000000-0000-0000-0000-000000000004', 2, 'aside', 'other', 'sent');
	`); err != nil {
		t.Fatal(err)
	}
	rows, err := database.Pool.Query(ctx,
		`select from_kind || ':' || from_id || '->' || session_id from `+SessionReceiptsSQL("$1", "$2")+` r`,
		[]string{"wanted"}, time.Now().Add(-30*24*time.Hour))
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
	slices.Sort(got)
	got = slices.Compact(got)
	want := []string{
		"comment:00000000-0000-0000-0000-000000000001->wanted",
		"comment:00000000-0000-0000-0000-000000000002->wanted",
		"message:00000000-0000-0000-0000-000000000004->wanted",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("receipts = %#v, want %#v", got, want)
	}
}
