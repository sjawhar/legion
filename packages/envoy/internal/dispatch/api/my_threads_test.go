package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestMyThreadsIsOwnAllActivityReadAndKeepsReadThreads(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "THR", "Threads", "Participation")
	ctx := context.Background()
	_, err := database.Pool.Exec(ctx, `
 insert into comments (id,issue_key,author,body,created_at) values
 ('10000000-0000-0000-0000-000000000001',$1,'{"kind":"user","id":" ALICE "}','Old authored thread',now()-interval '5 minutes'),
 ('10000000-0000-0000-0000-000000000002',$1,'{"kind":"user","id":"alice"}','New authored thread',now()-interval '3 minutes'),
 ('10000000-0000-0000-0000-000000000003',$1,'{"kind":"user","id":"bob"}','Unrelated thread',now()-interval '2 minutes');
 insert into comments(id,issue_key,author,body,reply_to,created_at) values
 ('10000000-0000-0000-0000-000000000004',$1,'{"kind":"user","id":"bob"}','Latest activity','10000000-0000-0000-0000-000000000001',now()-interval '1 minute');
 update user_threads set read_through=now() where login='alice';
 `, pgx.QueryExecModeSimpleProtocol, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		ID       string    `json:"thread_id"`
		Subject  string    `json:"subject"`
		Causes   []string  `json:"causes"`
		Activity time.Time `json:"activity_at"`
		Href     string    `json:"href"`
	}
	type page struct {
		Rows   []row  `json:"rows"`
		Total  int    `json:"total"`
		Person string `json:"person"`
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/threads", nil, " ALICE ")
	if response.Code != http.StatusOK {
		t.Fatalf("threads: %d %s", response.Code, response.Body.String())
	}
	got := decodeBody[page](t, response)
	if got.Person != "alice" || got.Total != 2 || len(got.Rows) != 2 || got.Rows[0].Subject != "Old authored thread" || got.Rows[1].Subject != "New authored thread" {
		t.Fatalf("page = %+v", got)
	}
	if !got.Rows[0].Activity.After(got.Rows[1].Activity) {
		t.Fatalf("not activity ordered: %+v", got.Rows)
	}
	if got.Rows[0].Href != "/issues/"+issue.Key+"/comments/10000000-0000-0000-0000-000000000001" {
		t.Fatalf("root href: %+v", got.Rows[0])
	}
	response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/threads?limit=1&offset=10", nil, "alice")
	empty := decodeBody[page](t, response)
	if empty.Total != 2 || len(empty.Rows) != 0 {
		t.Fatalf("empty page loses total: %+v", empty)
	}
	for _, target := range []string{"/api/v1/me/threads?login=bob", "/api/v1/me/threads?person=bob", "/api/v1/me/threads/shadow?login=bob", "/api/v1/me/threads?limit=0", "/api/v1/me/threads?offset=-1"} {
		if got := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice"); got.Code != http.StatusBadRequest {
			t.Fatalf("invalid/identity selector %s: %d %s", target, got.Code, got.Body.String())
		}
	}
	personToken := personalTokenHolding(t, database, model.ScopePerson)
	for _, target := range []string{"/api/v1/me/threads", "/api/v1/me/threads/shadow"} {
		own := agentRequest(t, handler, http.MethodGet, target, nil, personToken)
		if own.Code != http.StatusOK {
			t.Fatalf("person-scoped bearer %s: %d %s", target, own.Code, own.Body.String())
		}
		identity := decodeBody[struct {
			Person string `json:"person"`
		}](t, own)
		if identity.Person != "alice" {
			t.Fatalf("person-scoped bearer resolved %q, want its owner alice", identity.Person)
		}
	}
	mark := "/api/v1/me/threads/10000000-0000-0000-0000-000000000001/read"
	body := map[string]string{"read_through": time.Now().UTC().Format(time.RFC3339Nano)}
	if refused := sessionRequest(t, handler, http.MethodPut, mark, body); refused.Code != http.StatusForbidden {
		t.Fatalf("read mark without person scope: %d %s", refused.Code, refused.Body.String())
	}
	if own := agentRequest(t, handler, http.MethodPut, mark, body, personToken); own.Code != http.StatusNoContent {
		t.Fatalf("person-scoped read mark: %d %s", own.Code, own.Body.String())
	}
	for _, target := range []string{"/api/v1/me/threads", "/api/v1/me/threads/shadow"} {
		if got := sessionRequest(t, handler, http.MethodGet, target, nil); got.Code != http.StatusForbidden {
			t.Fatalf("bearer %s: %d %s", target, got.Code, got.Body.String())
		}
		anonymous := httptest.NewRecorder()
		handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, target, nil))
		if anonymous.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s: %d", target, anonymous.Code)
		}
	}
}

func TestThreadAncestryHasOneBoundedCycleAndOwnerGuard(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "WALK", "Walk", "Root walk")
	ctx := context.Background()
	_, err := database.Pool.Exec(ctx, `
 insert into comments(id,issue_key,author,body) values
 ('20000000-0000-0000-0000-000000000001',$1,'{"kind":"user","id":"alice"}','root'),
 ('20000000-0000-0000-0000-000000000002',$1,'{"kind":"user","id":"bob"}','reply');
 update comments set reply_to='20000000-0000-0000-0000-000000000001' where id='20000000-0000-0000-0000-000000000002';
 `, pgx.QueryExecModeSimpleProtocol, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	var root string
	if err := database.Pool.QueryRow(ctx, `select id::text from dispatch_thread_ancestors(array['comment'],array['20000000-0000-0000-0000-000000000002'::uuid]) where parent_id is null`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if root != "20000000-0000-0000-0000-000000000001" {
		t.Fatal(root)
	}
	if _, err := database.Pool.Exec(ctx, `update comments set reply_to='20000000-0000-0000-0000-000000000002' where id='20000000-0000-0000-0000-000000000001'`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.Pool.QueryRow(ctx, `select count(*) from dispatch_thread_ancestors(array['comment'],array['20000000-0000-0000-0000-000000000002'::uuid])`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("cycle visits %d rows", count)
	}
}
