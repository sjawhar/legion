package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestInboxShadowReportsUnexpectedWrongDelivery(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedPeople(t, database, "alice", "bob")
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
 insert into projects(key,name) values('CORE','Core');
 insert into issues(key,project_key,number,title,created_by,assignee,rank) values
 ('CORE-1','CORE',1,'Alice work','{"kind":"user","id":"alice"}','alice','U');
 insert into asks(id,issue_key,author,question) values
 ('00000000-0000-0000-0000-000000000001','CORE-1','{"kind":"session","id":"agent"}','Alice question');
 insert into comments(id,issue_key,author,body,created_at) values
 ('00000000-0000-0000-0000-000000000010','CORE-1','{"kind":"user","id":"alice"}','Root',now()-interval '10 minutes');
 insert into comments(id,issue_key,author,body,reply_to) values
 ('00000000-0000-0000-0000-000000000011','CORE-1','{"kind":"session","id":"agent"}','Reply','00000000-0000-0000-0000-000000000010');
 insert into messages(id,author,body,target,created_at) values
 ('00000000-0000-0000-0000-000000000020','{"kind":"user","id":"alice"}','Direct root','session:agent',now()-interval '10 minutes');
 insert into messages(id,author,body,target,in_reply_to) values
 ('00000000-0000-0000-0000-000000000021','{"kind":"session","id":"agent"}','Direct reply','session:agent','00000000-0000-0000-0000-000000000020');
 alter view graph_inbox_edges rename to fixture_graph_inbox_edges;
 create view graph_inbox_edges as
 select (jsonb_populate_record(null::fixture_graph_inbox_edges,
   to_jsonb(g)||jsonb_build_object('to_id',p.email))).*
 from fixture_graph_inbox_edges g cross join people p;
 `); err != nil {
		t.Fatal(err)
	}
	page := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/inbox", nil, "bob")
	if page.Code != 200 {
		t.Fatalf("inbox: %d %s", page.Code, page.Body.String())
	}
	got := decodeBody[unifiedInboxPage](t, page)
	if got.Total != 2 {
		t.Fatalf("wrong edges were filtered by legacy membership: total=%d want 2", got.Total)
	}
	if err := observeInboxShadow(ctx, database.Pool, 0); err != nil {
		t.Fatal(err)
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/inbox/shadow", nil, "bob")
	if response.Code != 200 {
		t.Fatalf("shadow: %d %s", response.Code, response.Body.String())
	}
	report := decodeBody[inboxShadowReport](t, response)
	if report.Mismatched != 2 || report.Unexpected["ask"] != 1 || report.Unexpected["direct_reply"] != 1 || report.Errors != 0 {
		t.Fatalf("wrong-edge probe: %+v", report)
	}
	t.Logf("wrong-edge probe: bob inbox=%d mismatched=%d unexpected.ask=%d unexpected.direct_reply=%d errors=%d", got.Total, report.Mismatched, report.Unexpected["ask"], report.Unexpected["direct_reply"], report.Errors)
}

func TestInboxShadowReadIsOwnAndNeverWritesObservations(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedPeople(t, database, "alice", "bob")
	ctx := context.Background()
	read := func() inboxShadowReport {
		t.Helper()
		r := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/inbox/shadow", nil, "alice")
		if r.Code != 200 {
			t.Fatalf("shadow: %d %s", r.Code, r.Body.String())
		}
		return decodeBody[inboxShadowReport](t, r)
	}
	initial := read()
	if initial.CheckedUsers != 0 || initial.UncoveredUsers != 2 || len(initial.Users) != 0 {
		t.Fatalf("GET invented evidence: %+v", initial)
	}
	if err := observeInboxShadow(ctx, database.Pool, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `update inbox_shadow_latest set mismatched=3,unexpected='{"ask":3}' where person='bob'`); err != nil {
		t.Fatal(err)
	}
	report := read()
	if report.CheckedUsers != 2 || report.Mismatched != 3 || len(report.Users) != 1 || report.Users[0].Person != "alice" || report.Users[0].Mismatched != 0 {
		t.Fatalf("GET must return caller row plus fleet totals: %+v", report)
	}
	var samples int
	if err := database.Pool.QueryRow(ctx, `select sum(samples)::int from inbox_shadow_days`).Scan(&samples); err != nil {
		t.Fatal(err)
	}
	if samples != 2 {
		t.Fatalf("GET wrote observations: samples=%d want 2", samples)
	}
}

func TestOwnInboxRoutesRequirePersonScope(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	unscoped := personalTokenHolding(t, database)
	person := personalTokenHolding(t, database, "person")
	for _, path := range []string{"/api/v1/me/inbox", "/api/v1/me/inbox/shadow"} {
		t.Run(path, func(t *testing.T) {
			denied := agentRequest(t, handler, http.MethodGet, path, nil, unscoped)
			if denied.Code != 403 || !strings.Contains(denied.Body.String(), codeScopeRequired) {
				t.Fatalf("unscoped bearer: %d %s", denied.Code, denied.Body.String())
			}
			allowed := agentRequest(t, handler, http.MethodGet, path, nil, person)
			if allowed.Code != 200 {
				t.Fatalf("person-scoped bearer: %d %s", allowed.Code, allowed.Body.String())
			}
			t.Logf("unscoped bearer=%d person-scoped bearer=%d", denied.Code, allowed.Code)
		})
	}
}
