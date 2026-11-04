package mentiongraph

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

func TestMentionCensusDetectsMissingAndUnexpectedRecordedResolutions(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	_, err := database.Pool.Exec(ctx, `insert into projects(key,name) values ('CORE','Core');
 insert into issues(key,project_key,number,title,rank,created_by) values ('CORE-1','CORE',1,'Mentions','U','{"kind":"user","id":"alice"}');
 insert into comments(id,issue_key,author,body) values
 ('00000000-0000-0000-0000-000000000001','CORE-1','{"kind":"user","id":"bob"}','See dispatch://people/alice'),
 ('00000000-0000-0000-0000-000000000002','CORE-1','{"kind":"session","id":"reader","owner":"alice"}','Empty mention set');
 insert into comment_mentions(comment_id,target,delivery,resolved_session_id) values ('00000000-0000-0000-0000-000000000001','role:reviewer','steer','reader');
 insert into messages(id,issue_key,author,body,target) values ('00000000-0000-0000-0000-000000000003','CORE-1','{"kind":"user","id":"bob"}','Message','session:reader');
 insert into messages(author,body,target,created_at) values ('{"kind":"user","id":"bob"}','Old','session:reader',now()-interval '31 days');`)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := refs.ReplaceCountedItem(ctx, tx, refs.ItemComment, "00000000-0000-0000-0000-000000000001", "See dispatch://people/alice", "https://dispatch.test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-30 * 24 * time.Hour)
	report, err := Census(ctx, database.Pool, since)
	if err != nil || report.Checked != 3 || report.Recorded != 5 || report.Mismatched != 0 {
		t.Fatalf("census = %+v, %v", report, err)
	}
	t.Logf("positive: checked=%d recorded=%d mismatched=%d", report.Checked, report.Recorded, report.Mismatched)
	_, err = database.Pool.Exec(ctx, `alter view graph_mention_edges rename to real_mention_edges;
 create view graph_mention_edges as select * from real_mention_edges where to_kind <> 'person'
 union all select 'comment','00000000-0000-0000-0000-000000000002','mentions','person','alice',now(),null::bigint;`)
	if err != nil {
		t.Fatal(err)
	}
	report, err = Census(ctx, database.Pool, since)
	if err != nil || report.Mismatched != 3 || report.Missing["comment:person"] != 1 || report.Missing["message:person"] != 1 || report.Unexpected["comment:person"] != 1 {
		t.Fatalf("negative control = %+v, %v", report, err)
	}
	t.Logf("negative: checked=%d mismatched=%d missing=%v unexpected=%v", report.Checked, report.Mismatched, report.Missing, report.Unexpected)
}
