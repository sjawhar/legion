package api

import (
	"context"
	"net/http"
	"testing"
)

func TestUnifiedInboxDocumentAnswerOverlapAndMentionExpansion(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
 insert into projects(key,name) values('CORE','Core');
 insert into issues(key,project_key,number,title,created_by,assignee,rank) values
 ('CORE-1','CORE',1,'Graph','{"kind":"user","id":"alice"}','alice','U');
 insert into artifacts(id,project_key,slug,name,kind,created_by) values
 ('00000000-0000-0000-0000-000000000100','CORE','notes','notes.md','doc','{"kind":"user","id":"bob"}');
 insert into artifacts(id,issue_key,project_key,slug,name,kind,is_primary,created_by) values
 ('00000000-0000-0000-0000-000000000200','CORE-1','CORE','spec','spec.md','doc',true,'{"kind":"user","id":"bob"}');
 insert into document_blocks(artifact_id,block_id,node_id,type,text_sha256,excerpt,changed_version,changed_at) values
 ('00000000-0000-0000-0000-000000000100','part:one','CORE/artifact/notes#part:one','paragraph',repeat('a',64),'Project mention',1,now()),
 ('00000000-0000-0000-0000-000000000200','part:one','CORE-1/spec#part:one','paragraph',repeat('b',64),'Primary mention',1,now());
 insert into asks(id,artifact_id,author,question,state,answer,created_at) values
 ('00000000-0000-0000-0000-000000000101','00000000-0000-0000-0000-000000000100','{"kind":"user","id":"alice"}','Document question','answered',jsonb_build_object('user','bob','selected',jsonb_build_array('Ship'),'text','Agreed','at',now()-interval '9 minutes'),now()-interval '10 minutes');
 insert into asks(id,issue_key,author,question,created_at) values
 ('00000000-0000-0000-0000-000000000102','CORE-1','{"kind":"user","id":"alice"}','Open overlap',now()-interval '10 minutes');
 insert into comments(id,issue_key,ask_id,author,body) values
 ('00000000-0000-0000-0000-000000000103','CORE-1','00000000-0000-0000-0000-000000000102','{"kind":"session","id":"agent"}','Open ask reply');
 insert into messages(id,issue_key,author,body) values
 ('00000000-0000-0000-0000-000000000104','CORE-1','{"kind":"session","id":"agent"}','Mention expansion');
 insert into messages(id,author,body,target) values
 ('00000000-0000-0000-0000-000000000105','{"kind":"user","id":"bob"}','Private direct mention','session:other');
 insert into refs(from_kind,from_id,kind,to_kind,to_id,created_at) values
 ('message','00000000-0000-0000-0000-000000000104','mentions','person_mention','alice',now()),
 ('message','00000000-0000-0000-0000-000000000105','mentions','person_mention','alice',now());
 insert into block_refs(artifact_id,block_id,from_node,kind,to_kind,to_id) values
 ('00000000-0000-0000-0000-000000000100','part:one','CORE/artifact/notes#part:one','mentions','person_mention','alice'),
 ('00000000-0000-0000-0000-000000000200','part:one','CORE-1/spec#part:one','mentions','person_mention','alice');
 `); err != nil {
		t.Fatal(err)
	}
	read := func(path string) unifiedInboxPage {
		t.Helper()
		r := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
		if r.Code != 200 {
			t.Fatalf("%s %d %s", path, r.Code, r.Body.String())
		}
		return decodeBody[unifiedInboxPage](t, r)
	}
	page := read("/api/v1/me/inbox")
	if page.Total != 2 || len(page.Expansions) != 0 {
		t.Fatalf("default membership=%+v", page)
	}
	foundAnswer := false
	for _, row := range page.Rows {
		if row.ID == "00000000-0000-0000-0000-000000000101" {
			foundAnswer = true
			if row.Body != "Ship - Agreed" || row.Author.ID != "bob" || row.Href != "/projects/CORE/documents/notes?ask=00000000-0000-0000-0000-000000000101" {
				t.Fatalf("answer=%+v", row)
			}
		}
		if row.ID == "00000000-0000-0000-0000-000000000103" {
			t.Fatal("open ask reply duplicated outside its ask")
		}
	}
	if !foundAnswer {
		t.Fatal("document answer dropped")
	}
	expanded := read("/api/v1/me/inbox?mentions=true")
	if expanded.Total != 2 || len(expanded.Expansions) != 3 {
		t.Fatalf("expansion/privacy=%+v", expanded)
	}
	wantLinks := map[string]string{
		"00000000-0000-0000-0000-000000000104": "/issues/CORE-1/messages/00000000-0000-0000-0000-000000000104",
		"CORE/artifact/notes#part:one":         "/projects/CORE/documents/notes#b-part%3Aone",
		"CORE-1/spec#part:one":                 "/issues/CORE-1/spec#b-part%3Aone",
	}
	for _, row := range expanded.Expansions {
		if wantLinks[row.ID] != row.Href {
			t.Fatalf("mention deep link=%+v", row)
		}
		delete(wantLinks, row.ID)
	}
	if len(wantLinks) != 0 {
		t.Fatalf("missing mention nodes=%v", wantLinks)
	}
	empty := read("/api/v1/me/inbox?limit=1&offset=10")
	if empty.Total != 2 || len(empty.Rows) != 0 {
		t.Fatalf("empty page total=%+v", empty)
	}
}

func TestInboxShadowCutoverRequiresEveryCompleteUTCUserDay(t *testing.T) {
	_, database, _ := newTestServer(t, testServerOptions{})
	seedPeople(t, database, "alice", "bob")
	ctx := context.Background()
	if err := observeInboxShadow(ctx, database.Pool, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into inbox_shadow_days(day,person,first_checked,last_checked,samples,max_gap_seconds)
 select day,person,(day::timestamp at time zone 'UTC')+interval '1 minute',((day+1)::timestamp at time zone 'UTC')-interval '1 minute',1440,60
 from (select ((statement_timestamp() at time zone 'UTC')::date-n)::date as day from generate_series(1,3)n) days
 cross join (values('alice'),('bob')) people(person)`); err != nil {
		t.Fatal(err)
	}
	report, err := readInboxShadowReport(ctx, database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Ready || report.CompleteZeroDays != 3 {
		t.Fatalf("full coverage=%+v", report)
	}
	if _, err := database.Pool.Exec(ctx, `update inbox_shadow_days set errors=1 where person='bob' and day=(statement_timestamp() at time zone 'UTC')::date-2`); err != nil {
		t.Fatal(err)
	}
	report, err = readInboxShadowReport(ctx, database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if report.Ready || report.CompleteZeroDays != 2 {
		t.Fatalf("bad user day allowed cutover: %+v", report)
	}
	if _, err := database.Pool.Exec(ctx, `update inbox_shadow_days set errors=0,max_gap_seconds=121 where person='bob'`); err != nil {
		t.Fatal(err)
	}
	report, err = readInboxShadowReport(ctx, database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if report.Ready || report.CompleteZeroDays != 0 {
		t.Fatalf("coverage gaps allowed cutover: %+v", report)
	}
}
