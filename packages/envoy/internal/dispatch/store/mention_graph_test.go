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
		('00000000-0000-0000-0000-000000000002','CORE-1','{"kind":"session","id":"reader","owner":" Alice ","session_verified":true}','Verified ownership');
		insert into refs(from_kind,from_id,to_kind,to_id) values
		('comment','00000000-0000-0000-0000-000000000001','issue','CORE-1');
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
	var legacy string
	if err := database.Pool.QueryRow(ctx, `select string_agg(to_kind||':'||to_id,',' order by to_kind,to_id) from graph_edges where from_kind='comment' and from_id='00000000-0000-0000-0000-000000000001' and kind='mentions'`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != "issue:CORE-1" {
		t.Fatalf("ordinary graph_edges reference changed: %s", legacy)
	}
}

func TestMentionGraphDoesNotTrustAnUnverifiedSessionsClaimedOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		actor string
	}{
		{"verification absent", `{"kind":"session","id":"reader","owner":"alice"}`},
		{"verification false", `{"kind":"session","id":"reader","owner":"alice","session_verified":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openEmptyTestStore(t)
			ctx := context.Background()
			if err := database.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Pool.Exec(ctx, `
				insert into projects(key,name) values ('CORE','Core');
				insert into issues(key,project_key,number,title,rank,created_by) values ('CORE-1','CORE',1,'Mentions','U','{"kind":"user","id":"bob"}');
				insert into comments(id,issue_key,author,body) values
				('00000000-0000-0000-0000-000000000081','CORE-1','{"kind":"user","id":"bob"}','Explicit session mention'),
				('00000000-0000-0000-0000-000000000083','CORE-1','{"kind":"session","id":"proved-other-session","owner":"alice","session_verified":true}','Alice owns a different proved session');
				insert into comment_mentions(comment_id,target,delivery,resolved_session_id) values
				('00000000-0000-0000-0000-000000000081','session:reader','steer','reader');
			`); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Pool.Exec(ctx, `insert into comments(id,issue_key,author,body) values ('00000000-0000-0000-0000-000000000082','CORE-1',$1,'Caller-chosen ownership')`, tc.actor); err != nil {
				t.Fatal(err)
			}
			var sessions, people int
			if err := database.Pool.QueryRow(ctx, `select count(*) filter (where to_kind='session' and to_id='reader'), count(*) filter (where to_kind='person' and to_id='alice') from graph_mention_edges where from_kind='comment' and from_id='00000000-0000-0000-0000-000000000081'`).Scan(&sessions, &people); err != nil {
				t.Fatal(err)
			}
			if sessions != 1 || people != 0 {
				t.Fatalf("unverified session mention expanded to a person: sessions=%d people=%d", sessions, people)
			}
		})
	}
}
