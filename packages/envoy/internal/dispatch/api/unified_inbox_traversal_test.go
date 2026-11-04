package api

import (
	"context"
	"net/http"
	"testing"
)

func TestUnifiedInboxIncludesDepth64AndIgnoresAnOrphanCycle(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
 insert into messages(id,author,body,target,created_at) values
 (md5('depth-0')::uuid,'{"kind":"user","id":"alice"}','Depth root','session:agent',now()-interval '1 hour');
 insert into messages(id,author,body,target,in_reply_to)
 select md5('depth-'||n)::uuid,'{"kind":"session","id":"agent"}','Depth reply '||n,'session:agent',md5('depth-'||(n-1))::uuid from generate_series(1,64)n;
 insert into messages(id,author,body,target) values(md5('cycle-a')::uuid,'{"kind":"session","id":"agent"}','Orphan cycle a','session:agent');
 insert into messages(id,author,body,target,in_reply_to) values(md5('cycle-b')::uuid,'{"kind":"session","id":"agent"}','Orphan cycle b','session:agent',md5('cycle-a')::uuid);
 update messages set in_reply_to=md5('cycle-b')::uuid where id=md5('cycle-a')::uuid;
 `); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_inbox_edges where to_id='alice' and kind='delivered_to'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 64 {
		t.Fatalf("depth64/cycle graph=%d want64", count)
	}
	for _, offset := range []string{"0", "50"} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/inbox?offset="+offset, nil, "alice")
		if response.Code != 200 {
			t.Fatalf("page %s: %d %s", offset, response.Code, response.Body.String())
		}
		page := decodeBody[unifiedInboxPage](t, response)
		want := 50
		if offset == "50" {
			want = 14
		}
		if page.Total != 64 || len(page.Rows) != want {
			t.Fatalf("page %s=%+v", offset, page)
		}
	}
	if err := observeInboxShadow(ctx, database.Pool, 0); err != nil {
		t.Fatal(err)
	}
	report, err := readInboxShadowReport(ctx, database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if report.Mismatched != 0 || report.Errors != 0 {
		t.Fatalf("traversal shadow=%+v", report)
	}
}
