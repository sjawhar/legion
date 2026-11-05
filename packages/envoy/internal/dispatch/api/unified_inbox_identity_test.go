package api

import (
	"context"
	"net/http"
	"testing"
)

func TestInboxShadowDistinguishesEqualIDsOfDifferentNodeKinds(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
 insert into projects(key,name)values('CORE','Core');
 insert into issues(key,project_key,number,title,created_by,rank)values('CORE-1','CORE',1,'Graph','{"kind":"user","id":"alice"}','U');
 insert into comments(id,issue_key,author,body,created_at)values
 ('00000000-0000-0000-0000-000000000010','CORE-1','{"kind":"user","id":"alice"}','Comment root',now()-interval '1 hour');
 insert into comments(id,issue_key,author,body,reply_to)values
 ('00000000-0000-0000-0000-000000000099','CORE-1','{"kind":"session","id":"agent"}','Comment reply','00000000-0000-0000-0000-000000000010');
 insert into messages(id,issue_key,author,body,created_at)values
 ('00000000-0000-0000-0000-000000000020','CORE-1','{"kind":"user","id":"alice"}','Message root',now()-interval '1 hour');
 insert into messages(id,issue_key,author,body,in_reply_to)values
 ('00000000-0000-0000-0000-000000000099','CORE-1','{"kind":"session","id":"agent"}','Message reply','00000000-0000-0000-0000-000000000020');
 `); err != nil {
		t.Fatal(err)
	}
	read := func() inboxShadowReport {
		t.Helper()
		if err := observeInboxShadow(ctx, database.Pool, 0); err != nil {
			t.Fatal(err)
		}
		r := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/inbox/shadow", nil, "alice")
		if r.Code != 200 {
			t.Fatalf("shadow %d %s", r.Code, r.Body.String())
		}
		return decodeBody[inboxShadowReport](t, r)
	}
	report := read()
	found := false
	for _, user := range report.Users {
		if user.Person == "alice" {
			found = true
			if user.CheckedItems != 2 {
				t.Fatalf("distinct node kinds collapsed: %+v", user)
			}
		}
	}
	if !found {
		t.Fatal("alice was not checked")
	}
	if _, err := database.Pool.Exec(ctx, `alter view graph_inbox_edges rename to fixture_graph_inbox_edges;
 create view graph_inbox_edges as select * from fixture_graph_inbox_edges where not(from_kind='message' and from_id='00000000-0000-0000-0000-000000000099')`); err != nil {
		t.Fatal(err)
	}
	report = read()
	if report.Mismatched != 1 || report.Missing["thread_reply"] != 1 {
		t.Fatalf("same-id comment masked dropped message: %+v", report)
	}
}
