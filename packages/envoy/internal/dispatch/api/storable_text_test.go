package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// nulText is caller text with U+0000 as its second character, which PostgreSQL's text and jsonb
// cannot hold: a statement that stores it fails.
const nulText = "a\x00b"

// nulPath is nulText as a URL path segment or query value.
const nulPath = "a%00b"

// nulRequest is one request a refusal row sends: a JSON body, or a multipart upload, as a human
// (alice) or as a bearer session.
type nulRequest struct {
	method string
	target string
	bearer bool
	// body is the JSON body; nil sends none.
	body any
	// form, when set, sends a multipart upload of these fields and a markdown file instead.
	form map[string]string
	file []byte
}

func (request nulRequest) send(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	contentType := "application/json"
	switch {
	case request.form != nil:
		writer := multipart.NewWriter(&body)
		for _, name := range slices.Sorted(maps.Keys(request.form)) {
			if err := writer.WriteField(name, request.form[name]); err != nil {
				t.Fatalf("write multipart field %s: %v", name, err)
			}
		}
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", `form-data; name="file"; filename="notes.md"`)
		header.Set("Content-Type", "text/markdown")
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatalf("create multipart file: %v", err)
		}
		if _, err := part.Write(request.file); err != nil {
			t.Fatalf("write multipart file: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("finish multipart body: %v", err)
		}
		contentType = writer.FormDataContentType()
	case request.body != nil:
		if err := json.NewEncoder(&body).Encode(request.body); err != nil {
			t.Fatalf("encode request body: %v", err)
		}
	}
	httpRequest := httptest.NewRequest(request.method, request.target, &body)
	httpRequest.Header.Set("Content-Type", contentType)
	if request.bearer {
		httpRequest.Header.Set("Authorization", "Bearer agent-token")
	} else {
		httpRequest.Header.Set("X-Dispatch-User", "alice")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httpRequest)
	return response
}

// databaseFingerprint hashes every row of every table, so a test can tell a refused request wrote
// nothing anywhere: no row inserted, updated or deleted.
func databaseFingerprint(t *testing.T, database *store.Store) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `select tablename from pg_tables where schemaname = 'public'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read table names: %v", err)
	}
	fingerprint := make(map[string]string, len(tables))
	for _, table := range tables {
		var digest string
		if err := database.Pool.QueryRow(ctx, fmt.Sprintf(
			`select count(*) || ':' || coalesce(md5(string_agg(t::text, ',' order by t::text)), '') from %s t`,
			pgx.Identifier{table}.Sanitize(),
		)).Scan(&digest); err != nil {
			t.Fatalf("fingerprint %s: %v", table, err)
		}
		fingerprint[table] = digest
	}
	return fingerprint
}

// nulFixture is one of everything a storing route writes to: an issue whose spec holds the quote a
// suggestion anchors on, a project document, an ask, a comment, and a comment mention and a
// targeted message, both delivered to the live session s1.
type nulFixture struct {
	issue       string
	spec        string
	block       string
	projectDoc  string
	projectSlug string
	ask         string
	comment     string
	mention     string
	direct      string
}

func newNulFixture(t *testing.T, handler http.Handler) nulFixture {
	t.Helper()
	issue := createInteractionIssue(t, handler, "TEST", "NUL fixture", "before\n")
	fixture := nulFixture{issue: issue.Key, spec: issue.PrimaryArtifactID}

	blocks := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice")
	if blocks.Code != http.StatusOK {
		t.Fatalf("read spec blocks: status=%d body=%s", blocks.Code, blocks.Body.String())
	}
	fixture.block = decodeBody[[]model.ArtifactBlock](t, blocks)[0].ID

	projectDoc := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/TEST/artifacts", map[string]any{
		"name": "plan.md", "content": "# Plan\n\nbefore\n",
	}, "alice")
	if projectDoc.Code != http.StatusCreated {
		t.Fatalf("upload project document: status=%d body=%s", projectDoc.Code, projectDoc.Body.String())
	}
	uploaded := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, projectDoc)
	fixture.projectDoc, fixture.projectSlug = uploaded.Artifact.ID, uploaded.Artifact.Slug

	ask := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which one?", "options": []map[string]any{{"label": "A"}, {"label": "B"}},
		"actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if ask.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", ask.Code, ask.Body.String())
	}
	fixture.ask = decodeBody[model.Ask](t, ask).ID

	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "A plain comment.",
	}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("create comment: status=%d body=%s", comment.Code, comment.Body.String())
	}
	fixture.comment = decodeBody[model.Comment](t, comment).ID

	fixture.mention = decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?", "mentions": []map[string]any{{"target": "session:s1"}},
	})).ID
	fixture.direct = createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Targeted message.", "target": "session:s1", "delivery": "btw",
	}, "alice").ID
	return fixture
}

// nulListener is a live Envoy listener holding the one session the fixture's targeted writes go to.
func nulListener(t *testing.T) *httptest.Server {
	t.Helper()
	listener, _ := newBroadcastListener(t, `[{"session_id":"s1","title":"planner","capabilities":["btw","aside","steer"],"last_seen":1}]`)
	return listener
}

// Every route that stores caller text refuses a U+0000 in any text field it takes, before it
// writes anything: 400 NUL_CHARACTER naming the field, rather than the 500 of the statement
// PostgreSQL refuses. The input layer makes the check once, for a JSON body's every string and
// member name, a multipart upload's every field and its markdown file, and every path and query
// parameter, so each row below is a route and one of its text fields.
func TestEveryStoringRouteRefusesANulInItsText(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: nulListener(t).URL, settle: time.Hour})
	f := newNulFixture(t, handler)
	session := func(id string) map[string]any { return map[string]any{"kind": "session", "id": id} }
	issue := "/api/v1/issues/" + f.issue
	spec := "/api/v1/artifacts/" + f.spec
	projectDoc := "/api/v1/artifacts/" + f.projectDoc
	ask := "/api/v1/asks/" + f.ask
	cutoff := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)

	for _, row := range []struct {
		name    string
		request nulRequest
		field   string
	}{
		{"create project: name", nulRequest{method: http.MethodPost, target: "/api/v1/projects", body: map[string]any{"key": "NUL", "name": nulText}}, "name"},
		{"mint agent token: name", nulRequest{method: http.MethodPost, target: "/api/v1/me/agent-tokens", body: map[string]any{"name": nulText}}, "name"},
		{"map repository: owner", nulRequest{method: http.MethodPut, target: "/api/v1/settings/repo-projects/" + nulPath + "/widgets", body: map[string]any{"project": "TEST"}}, "path parameter owner"},
		{"set architecture source: branch", nulRequest{method: http.MethodPut, target: "/api/v1/projects/TEST/architecture-source", body: map[string]any{"repo": "acme/widgets", "branch": nulText}}, "branch"},

		{"create issue: title", nulRequest{method: http.MethodPost, target: "/api/v1/issues", body: map[string]any{"project": "TEST", "title": nulText}}, "title"},
		{"create issue: spec", nulRequest{method: http.MethodPost, target: "/api/v1/issues", body: map[string]any{"project": "TEST", "title": "Spec with a NUL", "spec": nulText}}, "spec"},
		{"create issue: label", nulRequest{method: http.MethodPost, target: "/api/v1/issues", body: map[string]any{"project": "TEST", "title": "Label with a NUL", "labels": []string{"ok", nulText}}}, "labels[1]"},
		{"create issue: components reason", nulRequest{method: http.MethodPost, target: "/api/v1/issues", body: map[string]any{"project": "TEST", "title": "Reason with a NUL", "components": map[string]any{"mode": "none", "reason": nulText}}}, "components.reason"},
		{"create issue: actor id", nulRequest{method: http.MethodPost, target: "/api/v1/issues", bearer: true, body: map[string]any{"project": "TEST", "title": "Actor with a NUL", "actor": session(nulText)}}, "actor.id"},
		{"create issue: actor origin", nulRequest{method: http.MethodPost, target: "/api/v1/issues", bearer: true, body: map[string]any{"project": "TEST", "title": "Origin with a NUL", "actor": map[string]any{"kind": "session", "id": "s1", "origin": map[string]any{"cwd": nulText}}}}, "actor.origin.cwd"},

		{"update issue: title", nulRequest{method: http.MethodPatch, target: issue, body: map[string]any{"title": nulText}}, "title"},
		{"update issue: label", nulRequest{method: http.MethodPatch, target: issue, body: map[string]any{"labels": []string{nulText}}}, "labels[0]"},
		{"update issue: route", nulRequest{method: http.MethodPatch, target: issue, body: map[string]any{"route": "session:" + nulText}}, "route"},
		{"update issue: external link", nulRequest{method: http.MethodPatch, target: issue, body: map[string]any{"external_links": []map[string]any{{"url": "https://example.com/" + nulText}}}}, "external_links[0].url"},
		{"update issue: components reason", nulRequest{method: http.MethodPatch, target: issue, body: map[string]any{"components": map[string]any{"mode": "none", "reason": nulText}}}, "components.reason"},
		{"claim issue: actor id", nulRequest{method: http.MethodPost, target: issue + "/claim", bearer: true, body: map[string]any{"actor": session(nulText)}}, "actor.id"},
		{"unsubscribe session: session id", nulRequest{method: http.MethodDelete, target: issue + "/subscribers/" + nulPath}, "path parameter session_id"},
		{"read state: dismissed", nulRequest{method: http.MethodPut, target: "/api/v1/me/issues/" + f.issue + "/state", body: map[string]any{"dismissed": []string{nulText}}}, "dismissed[0]"},

		{"post message: body", nulRequest{method: http.MethodPost, target: issue + "/messages", body: map[string]any{"body": nulText}}, "body"},
		{"post message: actor id", nulRequest{method: http.MethodPost, target: issue + "/messages", bearer: true, body: map[string]any{"body": "hi", "actor": session(nulText)}}, "actor.id"},
		{"retry message delivery: actor id", nulRequest{method: http.MethodPost, target: "/api/v1/messages/" + f.direct + "/deliveries", bearer: true, body: map[string]any{"delivery": "btw", "actor": session(nulText)}}, "actor.id"},
		{"reply to message: body", nulRequest{method: http.MethodPost, target: "/api/v1/messages/" + f.direct + "/reply", bearer: true, body: map[string]any{"actor": session("s1"), "attempt": 1, "body": nulText}}, "body"},
		{"reply to message: error", nulRequest{method: http.MethodPost, target: "/api/v1/messages/" + f.direct + "/reply", bearer: true, body: map[string]any{"actor": session("s1"), "attempt": 1, "error": nulText}}, "error"},
		{"message a session: body", nulRequest{method: http.MethodPost, target: "/api/v1/agents/s1/messages", body: map[string]any{"body": nulText, "delivery": "btw"}}, "body"},
		{"message a session: session id", nulRequest{method: http.MethodPost, target: "/api/v1/agents/" + nulPath + "/messages", body: map[string]any{"body": "hi", "delivery": "btw"}}, "path parameter session_id"},
		{"broadcast: body", nulRequest{method: http.MethodPost, target: "/api/v1/broadcasts", body: map[string]any{"body": nulText, "delivery": "btw", "session_ids": []string{"s1"}, "idempotency_key": uuid.NewString()}}, "body"},
		{"agent state: session id", nulRequest{method: http.MethodPut, target: "/api/v1/me/agents/" + nulPath + "/state", body: map[string]any{"cleared_before": cutoff}}, "path parameter session_id"},

		{"open ask: question", nulRequest{method: http.MethodPost, target: issue + "/asks", body: map[string]any{"question": nulText}}, "question"},
		{"open ask: option label", nulRequest{method: http.MethodPost, target: issue + "/asks", body: map[string]any{"question": "Which?", "options": []map[string]any{{"label": "A"}, {"label": nulText}}}}, "options[1].label"},
		{"open ask: option description", nulRequest{method: http.MethodPost, target: issue + "/asks", body: map[string]any{"question": "Which?", "options": []map[string]any{{"label": "A", "description": nulText}}}}, "options[0].description"},
		{"open ask: actor id", nulRequest{method: http.MethodPost, target: issue + "/asks", bearer: true, body: map[string]any{"question": "Which?", "actor": session(nulText)}}, "actor.id"},
		{"open document ask: question", nulRequest{method: http.MethodPost, target: projectDoc + "/asks", body: map[string]any{"question": nulText}}, "question"},
		{"edit ask: question", nulRequest{method: http.MethodPatch, target: ask, body: map[string]any{"question": nulText}}, "question"},
		{"edit ask: option label", nulRequest{method: http.MethodPatch, target: ask, body: map[string]any{"options": []map[string]any{{"label": nulText}}}}, "options[0].label"},
		{"answer ask: text", nulRequest{method: http.MethodPost, target: ask + "/answer", body: map[string]any{"selected": []string{"A"}, "text": nulText, "expected_edited_at": nil}}, "text"},
		{"resolve ask: reason", nulRequest{method: http.MethodPost, target: ask + "/resolve", body: map[string]any{"kind": "retracted", "reason": nulText}}, "reason"},
		{"follow ask: session id", nulRequest{method: http.MethodPut, target: ask + "/followers/" + nulPath}, "path parameter session_id"},

		{"comment: body", nulRequest{method: http.MethodPost, target: issue + "/comments", body: map[string]any{"body": nulText}}, "body"},
		{"comment: suggestion", nulRequest{method: http.MethodPost, target: issue + "/comments", body: map[string]any{"body": "Suggest", "anchor": map[string]any{"artifact": "spec", "quote": "before"}, "suggestion": map[string]any{"replace_with": nulText}}}, "suggestion.replace_with"},
		{"comment: actor id", nulRequest{method: http.MethodPost, target: issue + "/comments", bearer: true, body: map[string]any{"body": "hi", "actor": session(nulText)}}, "actor.id"},
		{"document comment: body", nulRequest{method: http.MethodPost, target: projectDoc + "/comments", body: map[string]any{"body": nulText}}, "body"},
		{"edit comment: body", nulRequest{method: http.MethodPatch, target: "/api/v1/comments/" + f.comment, body: map[string]any{"body": nulText}}, "body"},
		{"resolve comment: actor id", nulRequest{method: http.MethodPost, target: "/api/v1/comments/" + f.comment + "/resolve", bearer: true, body: map[string]any{"actor": session(nulText)}}, "actor.id"},
		{"retry mention delivery: actor id", nulRequest{method: http.MethodPost, target: "/api/v1/comments/" + f.mention + "/deliveries", bearer: true, body: map[string]any{"target": "session:s1", "delivery": "btw", "actor": session(nulText)}}, "actor.id"},
		{"reply to mention: body", nulRequest{method: http.MethodPost, target: "/api/v1/comments/" + f.mention + "/reply", bearer: true, body: map[string]any{"actor": session("s1"), "attempt": 1, "body": nulText}}, "body"},

		{"upload issue document: name", nulRequest{method: http.MethodPost, target: issue + "/artifacts", body: map[string]any{"name": nulText, "content": "# Notes\n"}}, "name"},
		{"upload issue document: content", nulRequest{method: http.MethodPost, target: issue + "/artifacts", body: map[string]any{"name": "notes.md", "content": nulText}}, "content"},
		{"upload issue document: summary", nulRequest{method: http.MethodPost, target: issue + "/artifacts", body: map[string]any{"name": "notes.md", "content": "# Notes\n", "summary": nulText}}, "summary"},
		{"upload issue document: actor id", nulRequest{method: http.MethodPost, target: issue + "/artifacts", bearer: true, body: map[string]any{"name": "notes.md", "content": "# Notes\n", "actor": session(nulText)}}, "actor.id"},
		{"upload issue file: name", nulRequest{method: http.MethodPost, target: issue + "/artifacts", form: map[string]string{"name": nulText}, file: []byte("# Notes\n")}, "name"},
		{"upload issue file: summary", nulRequest{method: http.MethodPost, target: issue + "/artifacts", form: map[string]string{"name": "notes.md", "summary": nulText}, file: []byte("# Notes\n")}, "summary"},
		{"upload issue file: markdown", nulRequest{method: http.MethodPost, target: issue + "/artifacts", form: map[string]string{"name": "notes.md"}, file: []byte(nulText)}, "file"},
		{"upload issue file: actor id", nulRequest{method: http.MethodPost, target: issue + "/artifacts", bearer: true, form: map[string]string{"name": "notes.md", "actor": `{"kind":"session","id":"a\u0000b"}`}, file: []byte("# Notes\n")}, "actor.id"},
		{"upload project document: content", nulRequest{method: http.MethodPost, target: "/api/v1/projects/TEST/artifacts", body: map[string]any{"name": "other.md", "content": nulText}}, "content"},
		{"upload project file: markdown", nulRequest{method: http.MethodPost, target: "/api/v1/projects/TEST/artifacts", form: map[string]string{"name": "other.md"}, file: []byte(nulText)}, "file"},

		{"edit document: replace", nulRequest{method: http.MethodPost, target: spec + "/edits", body: map[string]any{"ops": []map[string]any{{"op": "replace", "find": "before", "with": nulText}}}}, "ops[0].with"},
		{"edit document: insert", nulRequest{method: http.MethodPost, target: spec + "/edits", body: map[string]any{"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": nulText}}}}, "ops[0].markdown"},
		{"edit document: retype attribute", nulRequest{method: http.MethodPost, target: spec + "/edits", body: map[string]any{"ops": []map[string]any{{"op": "retype", "block": f.block, "type": "callout", "attributes": map[string]any{"title": nulText}}}}}, "ops[0].attributes.title"},
		{"edit document: summary", nulRequest{method: http.MethodPost, target: spec + "/edits", body: map[string]any{"summary": nulText, "ops": []map[string]any{{"op": "replace", "find": "before", "with": "after"}}}}, "summary"},
		{"edit issue document by slug", nulRequest{method: http.MethodPost, target: issue + "/artifacts/spec/edits", body: map[string]any{"ops": []map[string]any{{"op": "replace", "find": "before", "with": nulText}}}}, "ops[0].with"},
		{"edit project document by slug", nulRequest{method: http.MethodPost, target: "/api/v1/projects/TEST/artifacts/" + f.projectSlug + "/edits", body: map[string]any{"ops": []map[string]any{{"op": "replace", "find": "before", "with": nulText}}}}, "ops[0].with"},
		{"name version: summary", nulRequest{method: http.MethodPost, target: spec + "/versions", body: map[string]any{"summary": nulText}}, "summary"},
		{"name issue document version by slug", nulRequest{method: http.MethodPost, target: issue + "/artifacts/spec/versions", body: map[string]any{"summary": nulText}}, "summary"},
		{"name project document version by slug", nulRequest{method: http.MethodPost, target: "/api/v1/projects/TEST/artifacts/" + f.projectSlug + "/versions", body: map[string]any{"summary": nulText}}, "summary"},
		{"rebuild document: markdown", nulRequest{method: http.MethodPost, target: spec + "/rebuild", body: map[string]any{"markdown": nulText}}, "markdown"},
		{"review document: reason", nulRequest{method: http.MethodPost, target: spec + "/reviews", body: map[string]any{"state": "changes_requested", "reason": nulText}}, "reason"},
		{"request approval: summary", nulRequest{method: http.MethodPost, target: spec + "/approval-requests", body: map[string]any{"summary": nulText}}, "summary"},
	} {
		t.Run(row.name, func(t *testing.T) {
			before := databaseFingerprint(t, database)
			response := row.request.send(t, handler)
			assertNulRefusal(t, response, row.request.method+" "+row.request.target, row.field)
			after := databaseFingerprint(t, database)
			for _, table := range slices.Sorted(maps.Keys(after)) {
				if before[table] != after[table] {
					t.Errorf("%s %s changed table %s", row.request.method, row.request.target, table)
				}
			}
		})
	}
}

// A path or query parameter reaches a query as a read's text too, where PostgreSQL refuses a NUL as
// it refuses one written, so a read refuses it the same way.
func TestAReadRefusesANulInItsParameters(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "NUL reads", "before\n")
	for _, row := range []struct {
		target string
		field  string
	}{
		{"/api/v1/issues/" + issue.Key + "%00", "path parameter key"},
		{"/api/v1/issues/" + issue.Key + "/artifacts/" + nulPath, "path parameter slug"},
		{"/api/v1/issues?label=" + nulPath, "query parameter label"},
		{"/api/v1/search?q=" + nulPath + "cd", "query parameter q"},
	} {
		t.Run(row.target, func(t *testing.T) {
			assertNulRefusal(t, dispatchRequest(t, handler, http.MethodGet, row.target, nil, "alice"), "GET "+row.target, row.field)
		})
	}
}

// A credential decision's or lookup's typed code is relayed to the secrets broker, which reads it
// against its own PostgreSQL, and Dispatch is the only caller of those routes: a code holding a NUL
// is refused here and never relayed.
func TestTheCredentialRelayRefusesANulBeforeTheBroker(t *testing.T) {
	rig := newFakeBrokerRig(t)
	handler := newCredentialTestHandler(t, rig.server.URL)
	for _, target := range []string{
		"/api/v1/credential-requests/rec1/approve",
		"/api/v1/credential-requests/rec1/deny",
		"/api/v1/credential-requests/machine-lookup",
	} {
		t.Run(target, func(t *testing.T) {
			before := rig.calls
			response := dispatchRequest(t, handler, http.MethodPost, target, map[string]any{"code": nulText}, "alice")
			assertNulRefusal(t, response, "POST "+target, "code")
			if rig.calls != before {
				t.Errorf("POST %s relayed the code to the broker", target)
			}
		})
	}
}

// A browser's edit reaches the live document as a Yjs update, past every route's check, so it can
// put a U+0000 into the document's text: typed or pasted into a paragraph, into an ask block's
// question, or into text a comment anchors on. Text Dispatch takes from the tree to store carries
// it as U+FFFD, what CommonMark reads it as: settlement writes the version, indexes the ask and
// refreshes the anchor's quote, and agents go on editing and versioning the document. Stored as it
// is, the character fails settlement every time it runs, and every edit or named version of the
// document with it.
func TestABrowsersNulLeavesTheDocumentVersionable(t *testing.T) {
	for _, test := range []struct {
		name string
		spec string
		// anchor is a quote a comment anchors on before the browser edits.
		anchor string
		edit   func(tree *pmdoc.Node)
	}{
		{
			name: "paragraph",
			spec: "before\n",
			edit: func(tree *pmdoc.Node) { tree.Children[0].Children[0].Text += nulText },
		},
		{
			name: "ask question",
			spec: "before\n\n:::ask{#q1 urgency=\"med\"}\nWhich one?\n\n- A\n- B\n:::\n",
			edit: func(tree *pmdoc.Node) { tree.Children[1].Children[0].Children[0].Text = "Which " + nulText + "?" },
		},
		{
			name:   "anchored text",
			spec:   "before\n",
			anchor: "before",
			edit:   func(tree *pmdoc.Node) { tree.Children[0].Children[0].Text = "bef" + nulText + "ore" },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			documentService, handler, database := browserDocumentService(t)
			issue := createInteractionIssue(t, handler, "TEST", "Browser NUL", test.spec)
			if test.anchor != "" {
				if comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
					"body": "On this.", "anchor": map[string]any{"artifact": "spec", "quote": test.anchor},
				}, "alice"); comment.Code != http.StatusCreated {
					t.Fatalf("anchor a comment: status=%d body=%s", comment.Code, comment.Body.String())
				}
			}
			peer := connectBrowserPeer(t, documentService, issue.PrimaryArtifactID)
			peer.edit(t, func(tree *pmdoc.Node) error {
				test.edit(tree)
				return nil
			})
			peer.barrier(t)
			peer.closeAndWait(t)

			var versions, owed int
			if err := database.Pool.QueryRow(context.Background(), `
				select (select count(*) from artifact_versions where artifact_id = $1),
					(select count(*) from doc_settlements_pending where artifact_id = $1)
			`, issue.PrimaryArtifactID).Scan(&versions, &owed); err != nil {
				t.Fatalf("read the document's settlement: %v", err)
			}
			if versions != 2 || owed != 0 {
				t.Fatalf("after the browser's edit the document has %d versions and owes %d settlements, want 2 and 0", versions, owed)
			}
			text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
			markdown := decodeBody[struct {
				Markdown string `json:"markdown"`
			}](t, text).Markdown
			if strings.IndexByte(markdown, 0) >= 0 || !strings.Contains(markdown, "a\uFFFDb") {
				t.Fatalf("GET /text = %q, want the NUL written as U+FFFD", markdown)
			}
			if test.name == "ask question" {
				asks := decodeBody[[]model.Ask](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice"))
				if len(asks) != 1 || asks[0].Question != "Which a\uFFFDb?" {
					t.Fatalf("indexed asks = %+v, want the block's question with U+FFFD", asks)
				}
			}
			if test.anchor != "" {
				comments := decodeBody[[]model.Comment](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/comments", nil, "alice"))
				if len(comments) != 1 || comments[0].Anchor == nil || comments[0].Anchor.Quote != "befa\uFFFDbore" {
					t.Fatalf("comments = %+v, want the anchor's quote refreshed with U+FFFD", comments)
				}
			}
			if edit := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": "An agent's line."}},
			}, "alice"); edit.Code != http.StatusOK {
				t.Fatalf("agent edit: status=%d body=%s", edit.Code, edit.Body.String())
			}
			if named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions", map[string]any{
				"summary": "Named.",
			}, "alice"); named.Code != http.StatusCreated {
				t.Fatalf("named version: status=%d body=%s", named.Code, named.Body.String())
			}
		})
	}
}

// A hand-built client can set a block's id to any string, such as one holding a U+0000, which an
// ask row cannot store and a typed block's `#id` cannot render. Settlement mints such an id again
// as it mints a missing one, so the document settles: the ask is indexed under the new id and the
// version holds no NUL.
func TestABlockIDOutsideTheIDFormatIsMintedAgain(t *testing.T) {
	for _, test := range []struct {
		name string
		spec string
	}{
		{"ask block", "before\n\n:::ask{#q1 urgency=\"med\"}\nWhich one?\n\n- A\n- B\n:::\n"},
		{"callout", "before\n\n:::callout{#c1 kind=\"note\"}\ninside\n:::\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			documentService, handler, database := browserDocumentService(t)
			issue := createInteractionIssue(t, handler, "TEST", "Crafted block id", test.spec)
			peer := connectBrowserPeer(t, documentService, issue.PrimaryArtifactID)
			peer.edit(t, func(tree *pmdoc.Node) error {
				tree.Children[1].Attrs[pmdoc.BlockIDAttr] = "x" + nulText
				return nil
			})
			peer.barrier(t)
			peer.closeAndWait(t)

			var versions, owed int
			var markdown string
			if err := database.Pool.QueryRow(context.Background(), `
				select (select count(*) from artifact_versions where artifact_id = $1),
					(select count(*) from doc_settlements_pending where artifact_id = $1),
					(select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1)
			`, issue.PrimaryArtifactID).Scan(&versions, &owed, &markdown); err != nil {
				t.Fatalf("read the document's settlement: %v", err)
			}
			if versions != 2 || owed != 0 || strings.IndexByte(markdown, 0) >= 0 || strings.Contains(markdown, "{#x") {
				t.Fatalf("after the crafted id: %d versions, %d settlements owed, latest version %q; want 2, 0 and a minted id", versions, owed, markdown)
			}
			if test.name == "ask block" {
				asks := decodeBody[[]model.Ask](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?state=open", nil, "alice"))
				if len(asks) != 1 || asks[0].BlockID == nil || strings.IndexByte(*asks[0].BlockID, 0) >= 0 || asks[0].Question != "Which one?" {
					t.Fatalf("open asks = %+v, want the block's ask under a minted id", asks)
				}
			}
			if edit := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": "An agent's line."}},
			}, "alice"); edit.Code != http.StatusOK {
				t.Fatalf("agent edit: status=%d body=%s", edit.Code, edit.Body.String())
			}
		})
	}
}

func assertNulRefusal(t *testing.T, response *httptest.ResponseRecorder, request, field string) {
	t.Helper()
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != http.StatusBadRequest || refusal.Code != "NUL_CHARACTER" || !strings.HasPrefix(refusal.Error, field+" holds a NUL character") {
		t.Errorf("%s = %d %s %q, want 400 NUL_CHARACTER naming %s", request, response.Code, refusal.Code, refusal.Error, field)
	}
}
