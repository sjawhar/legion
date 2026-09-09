package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The issue detail carries the open asks themselves, not a count: agents read
// them from here because the inbox is human-only.
func TestIssueDetailCarriesOpenAsks(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[model.Issue](t, created)
	ask := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which API?",
		"urgency":  "high",
	}, "alice")
	if ask.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", ask.Code, ask.Body.String())
	}

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("read issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	body := decodeBody[struct {
		OpenAsks []model.Ask `json:"open_asks"`
	}](t, detail)
	if len(body.OpenAsks) != 1 {
		t.Fatalf("open asks = %#v, want one ask", body.OpenAsks)
	}
	if body.OpenAsks[0].Question != "Which API?" || body.OpenAsks[0].State != "open" {
		t.Fatalf("open ask = %#v", body.OpenAsks[0])
	}
}
