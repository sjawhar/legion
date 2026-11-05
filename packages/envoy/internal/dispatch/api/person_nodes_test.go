package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestPersonNodesListsGraphRelationsAndDoesNotWidenMessageReads(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "PER", "People", "Graph nodes.")
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (id, issue_key, author, body, created_at) values
		('00000000-0000-0000-0000-000000000001', $1, '{"kind":"user","id":" ALICE "}', 'Authored first line' || E'\nRest', now() - interval '3 minutes'),
		('00000000-0000-0000-0000-000000000002', $1, '{"kind":"session","id":"s-owned","owner":"alice","session_verified":true}', 'Owned session', now() - interval '2 minutes'),
		('00000000-0000-0000-0000-000000000003', $1, '{"kind":"user","id":"bob"}', 'Received', now() - interval '1 minute'),
		('00000000-0000-0000-0000-000000000004', $1, '{"kind":"user","id":"bob"}', 'Not related', now());
	`, issue.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into messages (id, author, body, target) values
		('00000000-0000-0000-0000-000000000005', '{"kind":"user","id":"alice"}', 'Direct private address', 'session:s-owned');
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify, published_destinations)
		values ($1, 200, 'comment.created', '{"kind":"user","id":"bob"}', '{"id":"00000000-0000-0000-0000-000000000003"}', true,
		array['notifications.agent.s-owned', 'notifications.agent.s-unowned']);
	`, issue.Key); err != nil {
		t.Fatal(err)
	}
	type nodeRow struct {
		Kind      string    `json:"kind"`
		ID        string    `json:"id"`
		Title     string    `json:"title"`
		Relations []string  `json:"relations"`
		Ref       *string   `json:"ref"`
		Href      *string   `json:"href"`
		CreatedAt time.Time `json:"created_at"`
	}
	type nodePage struct {
		Person string    `json:"person"`
		Rows   []nodeRow `json:"rows"`
		Total  int       `json:"total"`
		Limit  int       `json:"limit"`
		Offset int       `json:"offset"`
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/%20ALICE%20/nodes?limit=2", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("person nodes: %d %s", response.Code, response.Body.String())
	}
	page := decodeBody[nodePage](t, response)
	if page.Person != "alice" || page.Total != 3 || page.Limit != 2 || len(page.Rows) != 2 {
		t.Fatalf("page = %+v", page)
	}
	if row := page.Rows[0]; row.Kind != "message" || row.Ref != nil || row.Title != "Direct private address" {
		t.Fatalf("direct message = %+v", row)
	}
	if row := page.Rows[1]; row.Title != "Received" || len(row.Relations) != 1 || row.Relations[0] != "delivered_to" || row.Ref == nil {
		t.Fatalf("received = %+v", row)
	}
	response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/alice/nodes?limit=2&offset=2", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("second page: %d %s", response.Code, response.Body.String())
	}
	page = decodeBody[nodePage](t, response)
	if len(page.Rows) != 1 || page.Rows[0].Title != "Authored first line" {
		t.Fatalf("second page = %+v", page)
	}
	for _, target := range []string{"/api/v1/people/alice/nodes", "/api/v1/people/bob/nodes"} {
		response := sessionRequest(t, handler, http.MethodGet, target, nil)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"`+codeScopeRequired+`"`) ||
			!strings.Contains(response.Body.String(), model.ScopePerson) {
			t.Fatalf("the deployment's shared token on %s: status=%d body=%s, want 403 %s naming the %s scope",
				target, response.Code, response.Body.String(), codeScopeRequired, model.ScopePerson)
		}
		anonymous := httptest.NewRecorder()
		handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, target, nil))
		if anonymous.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s: %d", target, anonymous.Code)
		}
	}
	// A credential that holds the person scope reads the route, as the person's browser does:
	// the routeScopes row is the only gate and the handler makes no second check.
	scoped := agentRequest(t, handler, http.MethodGet, "/api/v1/people/alice/nodes", nil,
		personalTokenHolding(t, database, model.ScopePerson))
	if scoped.Code != http.StatusOK {
		t.Fatalf("a personal token holding the person scope: status=%d body=%s", scoped.Code, scoped.Body.String())
	}
	if holder := decodeBody[nodePage](t, scoped); holder.Total != 3 {
		t.Fatalf("the person-scoped token's page = %+v, want the same three nodes", holder)
	}
	for _, target := range []string{"/api/v1/people/alice/nodes?limit=0", "/api/v1/people/alice/nodes?offset=-1"} {
		if response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice"); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid page: %d %s", response.Code, response.Body.String())
		}
	}
	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/nobody/nodes", nil, "alice"); response.Code != http.StatusNotFound {
		t.Fatalf("unknown person: %d %s", response.Code, response.Body.String())
	}
}

func TestPersonGraphHTTPPreservesRoleOnlyAndMixedDeliveries(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "ROLE", "Role deliveries", "Recipients")
	for index, destinations := range [][]string{
		{"notifications.role.reviewer"},
		{"notifications.agent.s-recipient", "notifications.role.reviewer"},
	} {
		comment := createReferenceAPIComment(t, handler, issue.Key, "Delivered")
		if _, err := database.Pool.Exec(context.Background(), `
			insert into events (issue_key, seq, type, actor, payload, published_destinations, notify)
			values ($1, $2, 'comment.created', '{"kind":"user","id":"alice"}', jsonb_build_object('id', $3::text), $4, true)
		`, issue.Key, 500+index, comment.ID, destinations); err != nil {
			t.Fatal(err)
		}
		response := graphEdges(t, handler, url.Values{"from": {"dispatch://" + issue.Key + "/comment/" + comment.ID}, "kind": {"delivered_to"}})
		var endpoints []string
		for _, edge := range response.Edges {
			endpoints = append(endpoints, edge.Node.Kind+"/"+edge.Node.ID)
			if edge.Node.Kind == "role" && (len(edge.Node.MadeBy) != 0 || len(edge.Node.ChangedBy) != 0) {
				t.Fatalf("role invents an actor: %+v", edge.Node)
			}
		}
		slices.Sort(endpoints)
		want := []string{"role/reviewer"}
		if index == 1 {
			want = append(want, "session/s-recipient")
		}
		if !slices.Equal(endpoints, want) {
			t.Fatalf("destinations %v resolved to %v, want %v", destinations, endpoints, want)
		}
	}
}

func TestPersonGraphDoesNotAliasCallerChosenSessions(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "CLAIM", "Claimed session", "Credentials")
	response := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "A caller-chosen session", "actor": map[string]any{
			"kind": "session", "id": "s-victim", "owner": "bob", "session_verified": true,
		},
	}, personalTokenHolding(t, database))
	if response.Code != http.StatusCreated {
		t.Fatalf("write with personal token: %d %s", response.Code, response.Body.String())
	}
	comment := decodeBody[model.Comment](t, response)
	if comment.Author.SessionVerified || comment.Author.Owner == nil || *comment.Author.Owner != "alice" {
		t.Fatalf("body forged a verified identity: %+v", comment.Author)
	}
	var aliases int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from person_aliases where alias_kind = 'session' and alias = 's-victim'`).Scan(&aliases); err != nil {
		t.Fatal(err)
	}
	if aliases != 0 {
		t.Fatalf("caller-chosen session acquired %d person aliases", aliases)
	}
}

func TestPersonNodesBoundsReceiptsWithoutLimitingAuthorship(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "WINDOW", "Delivery window", "Records")
	if _, err := database.Pool.Exec(context.Background(), `
		insert into comments (id, issue_key, author, body, created_at) values
		('00000000-0000-0000-0000-000000000011', $1, '{"kind":"user","id":"alice"}', 'Old authored', now() - interval '60 days'),
		('00000000-0000-0000-0000-000000000012', $1, '{"kind":"session","id":"s-proved","owner":"alice","session_verified":true}', 'Session identity', now()),
		('00000000-0000-0000-0000-000000000013', $1, '{"kind":"user","id":"bob"}', 'Old receipt', now() - interval '60 days'),
		('00000000-0000-0000-0000-000000000014', $1, '{"kind":"user","id":"bob"}', 'Sent comment', now());
	`, issue.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into messages (id, author, body, target) values
		('00000000-0000-0000-0000-000000000015', '{"kind":"user","id":"bob"}', 'Sent message', 'session:s-proved');
		insert into events (issue_key, seq, type, actor, payload, published_destinations, created_at, notify)
		select issue_key, 600, 'comment.created', '{"kind":"user","id":"bob"}', '{"id":"00000000-0000-0000-0000-000000000013"}', array['notifications.agent.s-proved'], now() - interval '60 days', true
		from comments where id = '00000000-0000-0000-0000-000000000013';
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state) values
		('00000000-0000-0000-0000-000000000014', 'role:reviewer', 1, 'aside', 's-proved', 'sent');
		insert into message_deliveries (message_id, attempt, delivery, session_id, state) values
		('00000000-0000-0000-0000-000000000015', 1, 'aside', 's-proved', 'sent');
	`); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		query string
		want  []string
	}{
		{"", []string{"Old authored", "Sent comment", "Sent message"}},
		{"?since=" + url.QueryEscape(time.Now().Add(-90*24*time.Hour).Format(time.RFC3339)), []string{"Old authored", "Old receipt", "Sent comment", "Sent message"}},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/alice/nodes"+test.query, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("window %s: %d %s", test.query, response.Code, response.Body.String())
		}
		page := decodeBody[personNodes](t, response)
		var titles []string
		for _, row := range page.Rows {
			titles = append(titles, row.Title)
			if row.Title != "Old authored" && !slices.Equal(row.Relations, []string{"delivered_to"}) {
				t.Fatalf("receipt lost its relation: %+v", row)
			}
		}
		slices.Sort(titles)
		if !slices.Equal(titles, test.want) || page.Total != len(test.want) {
			t.Fatalf("window %s: titles=%v total=%d, want %v", test.query, titles, page.Total, test.want)
		}
	}
	for _, query := range []string{"?since=bad", "?since=2026-01-01T00:00:00Z&since=2026-01-02T00:00:00Z"} {
		if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/alice/nodes"+query, nil, "alice"); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid window %s: %d %s", query, response.Code, response.Body.String())
		}
	}
}
