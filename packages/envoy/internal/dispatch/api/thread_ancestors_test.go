package api

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestThreadAncestryPreservesDepthCapsAndRejectsCrossOwnerParents(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "BOUND", "Bounds", "Depth caps")
	ctx := context.Background()
	_, err := database.Pool.Exec(ctx, `
 insert into comments(id,issue_key,author,body)
 select md5('comment-'||n::text)::uuid,$1,'{"kind":"session","id":"bot"}','chain' from generate_series(1,33) n;
 update comments c set reply_to=md5('comment-'||(n-1)::text)::uuid from generate_series(2,33) n where c.id=md5('comment-'||n::text)::uuid;
 insert into messages(id,issue_key,author,body,in_reply_to)
 select md5('message-'||n::text)::uuid,$1,'{"kind":"session","id":"bot"}','chain',case when n>1 then md5('message-'||(n-1)::text)::uuid end from generate_series(1,66) n order by n;
 `, pgx.QueryExecModeSimpleProtocol, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		kind, id string
		want     int
	}{{"comment", "comment-32", 1}, {"comment", "comment-33", 0}, {"message", "message-65", 1}, {"message", "message-66", 0}} {
		var got int
		if err := database.Pool.QueryRow(ctx, `select count(*) from dispatch_thread_ancestors($1,md5($2)::uuid) where parent_id is null`, test.kind, test.id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Fatalf("%s %s terminals=%d want %d", test.kind, test.id, got, test.want)
		}
	}
	_, err = database.Pool.Exec(ctx, `
 insert into asks(id,issue_key,author,question) values('40000000-0000-0000-0000-000000000001',$1,'{"kind":"session","id":"bot"}','An ask');
 update comments set ask_id='40000000-0000-0000-0000-000000000001' where id=md5('comment-1')::uuid;
 `, pgx.QueryExecModeSimpleProtocol, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	var kind string
	if err := database.Pool.QueryRow(ctx, `select kind from dispatch_thread_ancestors('comment',md5('comment-32')::uuid) where parent_id is null`).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "ask" {
		t.Fatalf("ask root kind=%s", kind)
	}
	// The root belongs to no owner after this corruption. A child cannot cross the
	// boundary merely because its stored reply_to still names that root.
	if _, err := database.Pool.Exec(ctx, `update comments set issue_key=null,artifact_id=$1 where id=md5('comment-1')::uuid`, issue.PrimaryArtifactID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.Pool.QueryRow(ctx, `select count(*) from dispatch_thread_ancestors('comment',md5('comment-2')::uuid) where parent_id is null`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("cross-owner ancestry produced a root")
	}
}
