package api

import (
	"context"
	"encoding/json"
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
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "TEST",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("map external repository: status=%d body=%s", response.Code, response.Body.String())
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

func TestListIssuesExcludesOpenAsksOnClosedIssues(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue with an ask",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[model.Issue](t, created)
	if ask := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]string{
		"question": "Which option?",
	}, "alice"); ask.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", ask.Code, ask.Body.String())
	}

	assertOpenAskCount := func(want int) {
		t.Helper()
		listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST", nil, "alice")
		if listed.Code != http.StatusOK {
			t.Fatalf("list issues: status=%d body=%s", listed.Code, listed.Body.String())
		}
		issues := decodeBody[[]model.IssueSummary](t, listed)
		if len(issues) != 1 || issues[0].OpenAsks != want {
			t.Fatalf("listed issues = %#v, want one issue with open_asks=%d", issues, want)
		}
	}

	assertOpenAskCount(1)
	if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"status": "done",
	}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
	assertOpenAskCount(0)
	if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"status": "todo",
	}, "alice"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen issue: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	assertOpenAskCount(1)
}

func TestListIssuesIncludesLastSequence(t *testing.T) {
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

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list issues: status=%d body=%s", listed.Code, listed.Body.String())
	}
	summaries := decodeBody[[]map[string]any](t, listed)
	if len(summaries) != 1 {
		t.Fatalf("listed issues = %#v, want one summary", summaries)
	}
	if got, ok := summaries[0]["last_seq"].(float64); !ok || int(got) != issue.LastSeq {
		t.Fatalf("list last_seq = %#v, want %d", summaries[0]["last_seq"], issue.LastSeq)
	}
}

// The issue list query's open-ask count must be covered by the partial
// asks_open(issue_key) where state = 'open' index rather than a sequential
// scan of the asks table: a listing call is on Dispatch's hottest path and
// must not scale with total ask volume across the instance.
func TestListIssuesQueryUsesAsksOpenIndex(t *testing.T) {
	_, database := newTestHandlerWithStore(t)
	ctx := context.Background()

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}

	var planJSON []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+listIssuesQuery, "", "", "").Scan(&planJSON); err != nil {
		t.Fatalf("explain list query: %v", err)
	}

	var plans []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal(planJSON, &plans); err != nil {
		t.Fatalf("decode explain output: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("explain plans = %#v, want one plan", plans)
	}
	if planNodeSeqScansRelation(t, plans[0].Plan, "asks") {
		t.Fatalf("list query plan sequentially scans asks; want an index scan via asks_open:\n%s", planJSON)
	}
}

func planNodeSeqScansRelation(t *testing.T, planJSON json.RawMessage, relation string) bool {
	t.Helper()
	var node struct {
		NodeType     string            `json:"Node Type"`
		RelationName string            `json:"Relation Name"`
		Plans        []json.RawMessage `json:"Plans"`
	}
	if err := json.Unmarshal(planJSON, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	if node.NodeType == "Seq Scan" && node.RelationName == relation {
		return true
	}
	for _, child := range node.Plans {
		if planNodeSeqScansRelation(t, child, relation) {
			return true
		}
	}
	return false
}
