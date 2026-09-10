package api

import (
	"net/http"
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
