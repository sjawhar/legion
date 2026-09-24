package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestInboxCarriesDocumentForDocumentAsks(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create CORE project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Issue",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	issue := decodeBody[model.Issue](t, issueResponse)
	issueAsk := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Issue question", "actor": sessionActor(),
	})
	if issueAsk.Code != http.StatusCreated {
		t.Fatalf("create issue ask: status=%d body=%s", issueAsk.Code, issueAsk.Body.String())
	}
	artifact := createProjectDocument(t, handler, "CORE", "Design notes", "# Design notes\n")
	documentAsk := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact.ID+"/asks", map[string]any{
		"question": "Document question", "actor": sessionActor(),
	})
	if documentAsk.Code != http.StatusCreated {
		t.Fatalf("create document ask: status=%d body=%s", documentAsk.Code, documentAsk.Body.String())
	}

	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	asks := decodeBody[[]struct {
		Question string `json:"question"`
		Issue    *struct {
			Key   string `json:"key"`
			Title string `json:"title"`
		} `json:"issue,omitempty"`
		Document *struct {
			Project string `json:"project"`
			Slug    string `json:"slug"`
			Name    string `json:"name"`
		} `json:"document,omitempty"`
	}](t, inbox)
	if len(asks) != 2 {
		t.Fatalf("inbox asks = %#v, want issue and document asks", asks)
	}
	var issueRow, documentRow *struct {
		Question string `json:"question"`
		Issue    *struct {
			Key   string `json:"key"`
			Title string `json:"title"`
		} `json:"issue,omitempty"`
		Document *struct {
			Project string `json:"project"`
			Slug    string `json:"slug"`
			Name    string `json:"name"`
		} `json:"document,omitempty"`
	}
	for index := range asks {
		switch asks[index].Question {
		case "Issue question":
			issueRow = &asks[index]
		case "Document question":
			documentRow = &asks[index]
		}
	}
	if issueRow == nil || issueRow.Issue == nil || issueRow.Issue.Key != issue.Key || issueRow.Issue.Title != "Issue" || issueRow.Document != nil {
		t.Fatalf("issue inbox row = %#v", issueRow)
	}
	if documentRow == nil || documentRow.Document == nil || documentRow.Document.Project != "CORE" || documentRow.Document.Slug != "design-notes" || documentRow.Document.Name != "Design notes" || documentRow.Issue != nil {
		t.Fatalf("document inbox row = %#v", documentRow)
	}

	if filtered := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox?project=OPS", nil, "alice"); filtered.Code != http.StatusOK || len(decodeBody[[]model.Ask](t, filtered)) != 0 {
		t.Fatalf("filter inbox to OPS: status=%d body=%s", filtered.Code, filtered.Body.String())
	}
	if filtered := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox?project=CORE", nil, "alice"); filtered.Code != http.StatusOK || len(decodeBody[[]model.Ask](t, filtered)) != 2 {
		t.Fatalf("filter inbox to CORE: status=%d body=%s", filtered.Code, filtered.Body.String())
	}
}

func TestInboxHydratesTheSameThreadsAsAskReads(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Inbox threads", "A spec")
	edited := createEditableAsk(t, handler, issue.Key)
	replyToAskAs(t, handler, issue.Key, edited.ID, "session-replier", "The reply stays visible.")
	if response := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+edited.ID, map[string]any{
		"question": "Which transport is ready?",
		"actor":    sessionActor(),
	}); response.Code != http.StatusOK {
		t.Fatalf("edit ask: status=%d body=%s", response.Code, response.Body.String())
	}
	untouched := openAskAs(t, handler, issue.Key, "session-untouched", "Which release?")

	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	type thread struct {
		Replies   []model.Comment     `json:"replies"`
		Edits     []model.AskEdit     `json:"edits"`
		Followers []model.AskFollower `json:"followers"`
	}
	rows := decodeBody[[]struct {
		ID     string `json:"id"`
		Thread thread `json:"thread"`
	}](t, inbox)
	threads := make(map[string]thread, len(rows))
	for _, row := range rows {
		threads[row.ID] = row.Thread
	}

	for _, askID := range []string{edited.ID, untouched} {
		read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
		if read.Code != http.StatusOK {
			t.Fatalf("read ask %q: status=%d body=%s", askID, read.Code, read.Body.String())
		}
		var expected thread
		expected = decodeBody[thread](t, read)
		if actual, ok := threads[askID]; !ok || !reflect.DeepEqual(actual, expected) {
			t.Fatalf("inbox thread for %q = %#v, want ask read %#v", askID, actual, expected)
		}
	}
}

func TestInboxCarriesAnchorDocumentForIssueAsk(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Anchored inbox", "A quoted passage.")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]string{"artifact": "spec", "quote": "quoted passage"},
		"question": "What does this mean?",
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create anchored issue ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[model.Ask](t, created)

	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	type inboxAnchorAsk struct {
		ID             string `json:"id"`
		AnchorArtifact *struct {
			Project string `json:"project"`
			Slug    string `json:"slug"`
			Name    string `json:"name"`
			Primary bool   `json:"primary"`
		} `json:"anchor_artifact,omitempty"`
	}
	rows := decodeBody[[]inboxAnchorAsk](t, inbox)
	var row *inboxAnchorAsk
	for index := range rows {
		if rows[index].ID == ask.ID {
			row = &rows[index]
			break
		}
	}
	if row == nil || row.AnchorArtifact == nil || row.AnchorArtifact.Project != "TEST" ||
		row.AnchorArtifact.Slug != "spec" || row.AnchorArtifact.Name != "spec.md" ||
		!row.AnchorArtifact.Primary {
		t.Fatalf("inbox anchor document = %#v, want TEST/spec spec.md primary", row)
	}
}

// The inbox partitions by the owning issue's assignee: ?assignee=me is the caller's own issues,
// ?assignee=unassigned is issues nobody owns plus every document ask (a document has no
// assignee), and no filter is everything. A login is canonicalised before the allowlist check.
func TestInboxFiltersByAssignee(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create CORE project: status=%d body=%s", response.Code, response.Body.String())
	}
	createIssueWithAsk := func(title string, body map[string]any, login string) string {
		t.Helper()
		body["project"] = "CORE"
		body["title"] = title
		var response *httptest.ResponseRecorder
		if login != "" {
			response = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, login)
		} else {
			body["actor"] = sessionActor()
			response = agentRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "agent-token")
		}
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", title, response.Code, response.Body.String())
		}
		issue := decodeBody[model.Issue](t, response)
		ask := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": title + " question", "actor": sessionActor(),
		})
		if ask.Code != http.StatusCreated {
			t.Fatalf("create ask on %s: status=%d body=%s", title, ask.Code, ask.Body.String())
		}
		return issue.Key
	}
	aliceKey := createIssueWithAsk("Alice's", map[string]any{}, "alice")
	bobKey := createIssueWithAsk("Bob's", map[string]any{}, "bob")
	nobodyKey := createIssueWithAsk("Nobody's", map[string]any{}, "")
	artifact := createProjectDocument(t, handler, "CORE", "Design notes", "# Design notes\n")
	if documentAsk := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact.ID+"/asks", map[string]any{
		"question": "Document question", "actor": sessionActor(),
	}); documentAsk.Code != http.StatusCreated {
		t.Fatalf("create document ask: status=%d body=%s", documentAsk.Code, documentAsk.Body.String())
	}

	type inboxRow struct {
		Question string `json:"question"`
		Issue    *struct {
			Key      string  `json:"key"`
			Assignee *string `json:"assignee"`
		} `json:"issue,omitempty"`
	}
	readInbox := func(query, login string) []inboxRow {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox"+query, nil, login)
		if response.Code != http.StatusOK {
			t.Fatalf("read inbox %q: status=%d body=%s", query, response.Code, response.Body.String())
		}
		return decodeBody[[]inboxRow](t, response)
	}
	questions := func(rows []inboxRow) map[string]inboxRow {
		byQuestion := map[string]inboxRow{}
		for _, row := range rows {
			byQuestion[row.Question] = row
		}
		return byQuestion
	}

	mine := questions(readInbox("?assignee=me", "Alice"))
	if len(mine) != 1 || mine["Alice's question"].Issue == nil || mine["Alice's question"].Issue.Key != aliceKey {
		t.Fatalf("alice's ?assignee=me = %#v, want only her issue's ask", mine)
	}
	if got := mine["Alice's question"].Issue.Assignee; got == nil || *got != "alice" {
		t.Fatalf("inbox row assignee = %v, want alice", got)
	}
	byLogin := questions(readInbox("?assignee=Bob", "alice"))
	if len(byLogin) != 1 || byLogin["Bob's question"].Issue == nil || byLogin["Bob's question"].Issue.Key != bobKey {
		t.Fatalf("?assignee=Bob = %#v, want only bob's issue's ask", byLogin)
	}
	unassigned := questions(readInbox("?assignee=unassigned", "alice"))
	if len(unassigned) != 2 || unassigned["Nobody's question"].Issue == nil || unassigned["Nobody's question"].Issue.Key != nobodyKey || unassigned["Document question"].Issue != nil {
		t.Fatalf("?assignee=unassigned = %#v, want the unassigned issue's ask and the document ask", unassigned)
	}
	if got := unassigned["Nobody's question"].Issue.Assignee; got != nil {
		t.Fatalf("unassigned inbox row assignee = %q, want null", *got)
	}
	if everything := readInbox("", "alice"); len(everything) != 4 {
		t.Fatalf("unfiltered inbox has %d rows, want 4: %#v", len(everything), everything)
	}
	if combined := readInbox("?assignee=me&project=OPS", "alice"); len(combined) != 0 {
		t.Fatalf("?assignee=me&project=OPS = %#v, want nothing", combined)
	}
	refused := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox?assignee=mallory", nil, "alice")
	if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"ASSIGNEE_NOT_ALLOWED"`) {
		t.Fatalf("?assignee=mallory: status=%d body=%s", refused.Code, refused.Body.String())
	}
}

// comments.reply_to carries no acyclicity constraint - the outbox keeps a self-referential
// comment in its own fixtures - and the two reads that walk it down a thread, the ask card's
// reply chain and the Inbox card's, are both seeded from the ask's own replies. A cycle among
// those rows has to be a visited row, not a request that never returns.
func TestAskThreadReadsTerminateOnACyclicReplyChain(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "TEST", "Cyclic ask thread", "A spec")
	askID := openAskAs(t, handler, issue.Key, "session-asker", "Which approach?")
	replyID := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "The second approach.", "ask_id": askID,
		"actor": map[string]any{"kind": "session", "id": "session-replier"},
	}, "").ID
	// A legacy reply_to-chained descendant, so the walk has work to do below the cycle: with
	// nothing to join against, the recursion short-circuits and reports a false clean bill.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body, reply_to)
		values ($1, '{"kind":"user","id":"alice"}', 'And the deadline?', $2)
	`, issue.Key, replyID); err != nil {
		t.Fatalf("seed a legacy reply below the ask reply: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update comments set reply_to = $1 where id = $1`, replyID); err != nil {
		t.Fatalf("make the ask reply self-referential: %v", err)
	}

	// The deadline rides on the request, so a walk that does spin is cancelled in Postgres
	// rather than left burning a backend for the rest of the package.
	for _, read := range []struct {
		name string
		path string
	}{
		{"ask card", "/api/v1/asks/" + askID},
		{"inbox", "/api/v1/inbox"},
	} {
		bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
		request := httptest.NewRequestWithContext(bounded, http.MethodGet, read.path, nil)
		request.Header.Set("X-Dispatch-User", "alice")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		cancel()
		if response.Code != http.StatusOK {
			t.Fatalf("%s read over a cyclic thread: status=%d body=%s", read.name, response.Code, response.Body.String())
		}
		// The seed row proves the read returned; the descendant proves the recursion still
		// walked past the cycle rather than stopping at the rows it was seeded with.
		for _, body := range []string{"The second approach.", "And the deadline?"} {
			if !strings.Contains(response.Body.String(), body) {
				t.Fatalf("%s read over a cyclic thread lost %q: %s", read.name, body, response.Body.String())
			}
		}
	}
}
