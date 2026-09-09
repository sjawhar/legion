package api

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func TestCreateExternalIssueIsIdempotentDuringConcurrentCreation(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}

	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			<-start
			responses <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
				"external": "owner/repo#777",
			}, "alice")
		}()
	}
	close(start)

	statuses := map[int]int{}
	keys := map[string]struct{}{}
	for range 2 {
		response := awaitResponse(t, responses)
		statuses[response.Code]++
		issue := decodeBody[model.Issue](t, response)
		keys[issue.Key] = struct{}{}
	}
	if statuses[http.StatusCreated] != 1 || statuses[http.StatusOK] != 1 {
		t.Fatalf("concurrent create statuses = %#v, want one 201 and one 200", statuses)
	}
	if len(keys) != 1 {
		t.Fatalf("concurrent create keys = %#v, want one issue key", keys)
	}

	var issueRows, linkRows int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from issues`).Scan(&issueRows); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from issue_external_links where url = $1
	`, externalURL("owner/repo", "777")).Scan(&linkRows); err != nil {
		t.Fatalf("count external links: %v", err)
	}
	if issueRows != 1 || linkRows != 1 {
		t.Fatalf("created rows: issues=%d links=%d, want one each", issueRows, linkRows)
	}
}
