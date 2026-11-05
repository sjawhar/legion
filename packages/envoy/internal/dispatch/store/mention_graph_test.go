package store

import (
	"context"
	"reflect"
	"slices"
	"testing"
)

func TestMentionGraphUsesRecordedTargetsAndResolvedSessions(t *testing.T) {
	database := openEmptyTestStore(t)
	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key,name) values ('CORE','Core');
		insert into issues (key,project_key,number,title,rank,created_by) values ('CORE-1','CORE',1,'Mentions','U','{"kind":"user","id":"bob"}');
		insert into comments (id,issue_key,author,body) values
		('00000000-0000-0000-0000-000000000001','CORE-1','{"kind":"user","id":"bob"}','No body reparse'),
		('00000000-0000-0000-0000-000000000002','CORE-1','{"kind":"session","id":"reader","owner":" Alice "}','Verified ownership');
		insert into comment_mentions (comment_id,target,delivery,resolved_session_id) values
		('00000000-0000-0000-0000-000000000001','role:reviewer','steer','reader'),
		('00000000-0000-0000-0000-000000000001','session:missing','aside',null);
		insert into messages (id,issue_key,author,body,target) values
		('00000000-0000-0000-0000-000000000003','CORE-1','{"kind":"user","id":"bob"}','Message','role:reviewer');
		insert into message_deliveries (message_id,attempt,delivery,session_id,state) values
		('00000000-0000-0000-0000-000000000003',1,'steer','reader','sent');
	`); err != nil {
		t.Fatal(err)
	}
	rows, err := database.Pool.Query(ctx, `select from_kind || ':' || from_id || '->' || to_kind || ':' || to_id from graph_mention_edges order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	slices.Sort(got)
	want := []string{
		"comment:00000000-0000-0000-0000-000000000001->person:alice",
		"comment:00000000-0000-0000-0000-000000000001->role:reviewer",
		"comment:00000000-0000-0000-0000-000000000001->session:missing",
		"comment:00000000-0000-0000-0000-000000000001->session:reader",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mentions = %v; want %v", got, want)
	}
}
