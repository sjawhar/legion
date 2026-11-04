package api

import (
	"context"
	"net/http"
	"testing"
)

func TestInboxMentionPaginationFiltersPrivateNodesBeforePaging(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	if _, err := database.Pool.Exec(context.Background(), `
 insert into projects(key,name)values('CORE','Core');
 insert into issues(key,project_key,number,title,created_by,rank)values('CORE-1','CORE',1,'Graph','{"kind":"user","id":"alice"}','U');
 insert into messages(id,issue_key,author,body,created_at)values
 ('00000000-0000-0000-0000-000000000001','CORE-1','{"kind":"session","id":"agent"}','Public mention',now()-interval '1 hour');
 insert into messages(id,author,body,target)
 select md5('private-'||n)::uuid,'{"kind":"user","id":"bob"}','Private mention','session:other' from generate_series(1,51)n;
 insert into refs(from_kind,from_id,kind,to_kind,to_id,created_at)
 select 'message',id::text,'mentions','person','alice',created_at from messages;
 `); err != nil {
		t.Fatal(err)
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/inbox?mentions=true&limit=1", nil, "alice")
	if response.Code != 200 {
		t.Fatalf("mentions %d %s", response.Code, response.Body.String())
	}
	page := decodeBody[unifiedInboxPage](t, response)
	if len(page.Expansions) != 1 || page.Expansions[0].Body != "Public mention" {
		t.Fatalf("ineligible newer nodes hid a public mention: %+v", page)
	}
}
