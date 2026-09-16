package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
