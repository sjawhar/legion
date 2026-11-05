package store

import (
	"context"
	"testing"
)

func TestIssueEdgeMigrationKeepsLegacyViewAndOldInsert(t *testing.T) {
	ctx := context.Background()
	database := openEmptyTestStore(t)
	migrateThrough(t, database, 20261104090146)
	var before, after string
	if err := database.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_edges')`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_edges')`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("issue migration changed the legacy graph view")
	}
	if _, err := database.Pool.Exec(ctx, `
 insert into projects (key,name) values ('OLD','Old writer');
 insert into issues (key,project_key,number,title,created_by,rank) values ('OLD-1','OLD',1,'Old shape','{"kind":"user","id":"alice"}','U');
 insert into issues (key,project_key,number,title,created_by,rank,status,closed_at) values ('OLD-2','OLD',2,'Duplicate','{"kind":"user","id":"alice"}','V','done',now()+interval '1 second');
 insert into comments (issue_key,author,body) values ('OLD-2','{"kind":"user","id":"alice"}','Duplicate of dispatch://OLD-1.');
 `); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_issue_edges where from_id='OLD-2' and kind='duplicate_of' and to_id='OLD-1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("comment duplicate count=%d", count)
	}
	if _, err := database.Pool.Exec(ctx, `update issues set status='backlog',closed_at=null where key='OLD-2'`); err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_issue_edges where from_id='OLD-2'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("reopening retained the duplicate edge")
	}
	var compatible bool
	if err := database.Pool.QueryRow(ctx, `select array(select attname||':'||atttypid::text from pg_attribute where attrelid='graph_edges'::regclass and attnum>0 order by attnum)=array(select attname||':'||atttypid::text from pg_attribute where attrelid='graph_issue_edges'::regclass and attnum>0 order by attnum)`).Scan(&compatible); err != nil {
		t.Fatal(err)
	}
	if !compatible {
		t.Fatal("issue edge columns differ from the graph")
	}
}

func TestIssueDuplicateUsesEditsAndIgnoresExamples(t *testing.T) {
	database := openEmptyTestStore(t)
	ctx := t.Context()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key,name) values ('NOTES','Notes');
		insert into issues (key,project_key,number,title,created_by,rank,status,closed_at)
		select 'NOTES-'||n,'NOTES',n,'Topic '||n,'{"kind":"user","id":"alice"}',n::text,'done','2026-11-01T12:00:00Z' from generate_series(1,4) n;
		insert into comments (issue_key,author,body,created_at,edited_at) values
		('NOTES-3','{"kind":"user","id":"alice"}','Duplicate of dispatch://NOTES-2','2026-11-01T09:00:00Z','2026-11-01T11:00:00Z'),
		('NOTES-3','{"kind":"user","id":"alice"}','Duplicate of dispatch://NOTES-1','2026-11-01T10:00:00Z',null),
		('NOTES-4','{"kind":"user","id":"alice"}',E'\tDuplicate of dispatch://NOTES-1','2026-11-01T10:00:00Z',null);
	`); err != nil {
		t.Fatal(err)
	}
	var target string
	if err := database.Pool.QueryRow(ctx, `select to_id from graph_issue_edges where from_id='NOTES-3' and kind='duplicate_of'`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target != "NOTES-2" {
		t.Fatalf("edited declaration points to %s, want NOTES-2", target)
	}
	for _, body := range []string{"\tDuplicate of dispatch://NOTES-1", "    Duplicate of dispatch://NOTES-1", "> Duplicate of dispatch://NOTES-1", "```\nDuplicate of dispatch://NOTES-1\n```"} {
		if _, err := database.Pool.Exec(ctx, `update comments set body=$1 where issue_key='NOTES-4'`, body); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := database.Pool.QueryRow(ctx, `select count(*) from graph_issue_edges where from_id='NOTES-4' and kind='duplicate_of'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("example %q declares a duplicate", body)
		}
	}
}
