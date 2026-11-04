package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPersonNodesListsGraphRelationsAndDoesNotWidenMessageReads(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "PER", "People", "Graph nodes.")
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (id, issue_key, author, body, created_at) values
		('00000000-0000-0000-0000-000000000001', $1, '{"kind":"user","id":" ALICE "}', 'Authored first line' || E'\nRest', now() - interval '3 minutes'),
		('00000000-0000-0000-0000-000000000002', $1, '{"kind":"session","id":"s-owned","owner":"alice"}', 'Owned session', now() - interval '2 minutes'),
		('00000000-0000-0000-0000-000000000003', $1, '{"kind":"user","id":"bob"}', 'Received', now() - interval '1 minute'),
		('00000000-0000-0000-0000-000000000004', $1, '{"kind":"user","id":"bob"}', 'Not related', now());
	`, issue.Key); err != nil { t.Fatal(err) }
	if _, err := database.Pool.Exec(ctx, `
		insert into messages (id, author, body, target) values
		('00000000-0000-0000-0000-000000000005', '{"kind":"user","id":"alice"}', 'Direct private address', 'session:s-owned');
	`); err != nil { t.Fatal(err) }
	if _, err := database.Pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, published_destinations)
		values ($1, 200, 'comment.created', '{"kind":"user","id":"bob"}', '{"id":"00000000-0000-0000-0000-000000000003"}',
		array['notifications.agent.s-owned', 'notifications.agent.s-unowned']);
	`, issue.Key); err != nil { t.Fatal(err) }
	type nodeRow struct {
		Kind string `json:"kind"`
		ID string `json:"id"`
		Title string `json:"title"`
		Relations []string `json:"relations"`
		Ref *string `json:"ref"`
		Href *string `json:"href"`
		CreatedAt time.Time `json:"created_at"`
	}
	type nodePage struct {
		Person string `json:"person"`
		Rows []nodeRow `json:"rows"`
		Total int `json:"total"`
		Limit int `json:"limit"`
		Offset int `json:"offset"`
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/%20ALICE%20/nodes?limit=2", nil, "alice")
	if response.Code != http.StatusOK { t.Fatalf("person nodes: %d %s", response.Code, response.Body.String()) }
	page := decodeBody[nodePage](t, response)
	if page.Person != "alice" || page.Total != 3 || page.Limit != 2 || len(page.Rows) != 2 { t.Fatalf("page = %+v", page) }
	if row := page.Rows[0]; row.Kind != "message" || row.Ref != nil || row.Title != "Direct private address" { t.Fatalf("direct message = %+v", row) }
	if row := page.Rows[1]; row.Title != "Received" || len(row.Relations) != 1 || row.Relations[0] != "delivered_to" || row.Ref == nil { t.Fatalf("received = %+v", row) }
	response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/alice/nodes?limit=2&offset=2", nil, "alice")
	if response.Code != http.StatusOK { t.Fatalf("second page: %d %s", response.Code, response.Body.String()) }
	page = decodeBody[nodePage](t, response)
	if len(page.Rows) != 1 || page.Rows[0].Title != "Authored first line" { t.Fatalf("second page = %+v", page) }
	for _, target := range []string{"/api/v1/people/alice/nodes", "/api/v1/people/bob/nodes"} {
		response := sessionRequest(t, handler, http.MethodGet, target, nil)
		if response.Code != http.StatusForbidden { t.Fatalf("bearer %s: %d %s", target, response.Code, response.Body.String()) }
		anonymous := httptest.NewRecorder()
		handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, target, nil))
		if anonymous.Code != http.StatusUnauthorized { t.Fatalf("anonymous %s: %d", target, anonymous.Code) }
	}
	for _, target := range []string{"/api/v1/people/alice/nodes?limit=0", "/api/v1/people/alice/nodes?offset=-1"} {
		if response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice"); response.Code != http.StatusBadRequest { t.Fatalf("invalid page: %d %s", response.Code, response.Body.String()) }
	}
	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/nobody/nodes", nil, "alice"); response.Code != http.StatusNotFound { t.Fatalf("unknown person: %d %s", response.Code, response.Body.String()) }
}
