package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestThreadAncestryPreservesCommentCapAndRejectsCrossOwnerParents(t *testing.T) {
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
	}{{"comment", "comment-32", 1}, {"comment", "comment-33", 0}, {"message", "message-65", 1}, {"message", "message-66", 1}} {
		var got int
		if err := database.Pool.QueryRow(ctx, `select count(*) from dispatch_thread_ancestors(array[$1::text],array[md5($2)::uuid]) where parent_id is null`, test.kind, test.id).Scan(&got); err != nil {
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
	if err := database.Pool.QueryRow(ctx, `select kind from dispatch_thread_ancestors(array['comment'],array[md5('comment-32')::uuid]) where parent_id is null`).Scan(&kind); err != nil {
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
	if err := database.Pool.QueryRow(ctx, `select count(*) from dispatch_thread_ancestors(array['comment'],array[md5('comment-2')::uuid]) where parent_id is null`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("cross-owner ancestry produced a root")
	}
}

func TestMessageAncestryMatchesUnboundedLegacyWalk(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "DEEP", "Deep", "Message roots")
	ctx := context.Background()
	_, err := database.Pool.Exec(ctx, `
 insert into messages(id,issue_key,author,body,in_reply_to)
 select md5('deep-'||n::text)::uuid,$1,'{"kind":"session","id":"bot"}','Deep reply',
        case when n>1 then md5('deep-'||(n-1)::text)::uuid end
 from generate_series(1,130) n order by n;
 insert into messages(id,author,body,target,in_reply_to)
 select md5('direct-deep-'||n::text)::uuid,'{"kind":"user","id":"alice"}','Direct reply',
        'session:bot',
        case when n>1 then md5('direct-deep-'||(n-1)::text)::uuid end
 from generate_series(1,130) n order by n;
 `, pgx.QueryExecModeSimpleProtocol, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range []string{"deep-65", "deep-66", "deep-130", "direct-deep-130"} {
		var id string
		if err := database.Pool.QueryRow(ctx, `select md5($1)::uuid::text`, seed).Scan(&id); err != nil {
			t.Fatal(err)
		}
		var oldIDs, newIDs []string
		// The prior messageThreadCTE, independent of dispatch_thread_ancestors.
		if err := database.Pool.QueryRow(ctx, `with recursive thread as (
 select id,target,in_reply_to from messages where id=$1
 union all select m.id,m.target,m.in_reply_to from messages m join thread t on m.id=t.in_reply_to
 ) select array_agg(id::text order by id) from thread`, id).Scan(&oldIDs); err != nil {
			t.Fatal(err)
		}
		if err := database.Pool.QueryRow(ctx, messageThreadCTE+` select array_agg(id::text order by id) from thread`, id).Scan(&newIDs); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(oldIDs, newIDs) {
			t.Fatalf("%s: shared walk has %d ancestors, legacy has %d", seed, len(newIDs), len(oldIDs))
		}
		for _, suffix := range []string{"", "/blocks"} {
			response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/messages/"+id+suffix, nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("deep read %s%s: %d %s", seed, suffix, response.Code, response.Body.String())
			}
		}
		if seed == "deep-130" {
			response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
				"body": "A valid deeper reply", "in_reply_to": id, "actor": sessionActor(),
			})
			if response.Code != http.StatusCreated {
				t.Fatalf("deep issue reply: %d %s", response.Code, response.Body.String())
			}
		}
		if seed == "direct-deep-130" {
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			target, _, err := threadRootDelivery(ctx, tx, id)
			_ = tx.Rollback(ctx)
			if err != nil || target == nil || *target != "session:bot" {
				t.Fatalf("deep direct delivery target=%v err=%v", target, err)
			}
		}
	}
}
