package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestIssueDetailPreservesHistoricalNullCreator(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Historical issue",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[model.Issue](t, created)
	if _, err := database.Pool.Exec(
		context.Background(),
		`update issues set created_by = 'null'::jsonb where key = $1`,
		issue.Key,
	); err != nil {
		t.Fatalf("clear issue creator: %v", err)
	}

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("read issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(detail.Body).Decode(&body); err != nil {
		t.Fatalf("decode issue detail: %v", err)
	}
	createdBy, present := body["created_by"]
	if !present || string(createdBy) != "null" {
		t.Fatalf("historical issue created_by = %s, want explicit null", createdBy)
	}
}

// The issue header decides whose turn it is from the newest reply in each open
// ask's thread, exactly as the inbox does, so the detail carries the same
// last_reply: null until someone replies, then the newest comment's author.
func TestIssueDetailOpenAsksCarryLastReply(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Whose turn", "A spec")
	opened := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which layout?", "actor": sessionActor(),
	})
	if opened.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", opened.Code, opened.Body.String())
	}
	askID := decodeBody[struct {
		ID string `json:"id"`
	}](t, opened).ID
	type openAsk struct {
		ID        string `json:"id"`
		LastReply *struct {
			Author    model.Actor `json:"author"`
			CreatedAt string      `json:"created_at"`
		} `json:"last_reply"`
	}
	readOpenAsk := func() (openAsk, bool) {
		detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")
		if detail.Code != http.StatusOK {
			t.Fatalf("read issue: status=%d body=%s", detail.Code, detail.Body.String())
		}
		var body struct {
			OpenAsks []json.RawMessage `json:"open_asks"`
		}
		if err := json.NewDecoder(detail.Body).Decode(&body); err != nil {
			t.Fatalf("decode issue detail: %v", err)
		}
		if len(body.OpenAsks) != 1 {
			t.Fatalf("open asks = %s, want one ask", body.OpenAsks)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(body.OpenAsks[0], &keys); err != nil {
			t.Fatalf("decode open ask keys: %v", err)
		}
		_, present := keys["last_reply"]
		var ask openAsk
		if err := json.Unmarshal(body.OpenAsks[0], &ask); err != nil {
			t.Fatalf("decode open ask: %v", err)
		}
		return ask, present
	}

	fresh, present := readOpenAsk()
	if !present || fresh.ID != askID || fresh.LastReply != nil {
		t.Fatalf("unreplied open ask = %#v (last_reply key present=%t), want the ask with an explicit null last_reply", fresh, present)
	}

	human := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Which widths matter?", "ask_id": askID,
	}, "alice")
	if human.Code != http.StatusCreated {
		t.Fatalf("human reply: status=%d body=%s", human.Code, human.Body.String())
	}
	clarified, _ := readOpenAsk()
	if clarified.LastReply == nil || clarified.LastReply.Author.Kind != "user" || clarified.LastReply.Author.ID != "alice" || clarified.LastReply.CreatedAt == "" {
		t.Fatalf("open ask after a human reply = %#v, want alice as the newest reply", clarified)
	}

	agent := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "390, 1280 and 1536.", "ask_id": askID, "actor": sessionActor(),
	})
	if agent.Code != http.StatusCreated {
		t.Fatalf("agent reply: status=%d body=%s", agent.Code, agent.Body.String())
	}
	answered, _ := readOpenAsk()
	if answered.LastReply == nil || answered.LastReply.Author.Kind != "session" || answered.LastReply.Author.ID != "session-0123456789abcdef" {
		t.Fatalf("open ask after the agent's reply = %#v, want the session as the newest reply", answered)
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
	openOnly := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST&open=true", nil, "alice")
	if openOnly.Code != http.StatusOK || len(decodeBody[[]model.IssueSummary](t, openOnly)) != 0 {
		t.Fatalf("open issue list: status=%d body=%s", openOnly.Code, openOnly.Body.String())
	}
	if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"status": "todo",
	}, "alice"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen issue: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	assertOpenAskCount(1)
}

func TestListIssuesFiltersByUpdatedSince(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	for _, project := range []string{"TEST", "OTHER"} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": project, "name": project + " project",
		}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
		}
	}
	createIssue := func(project string) model.Issue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project, "title": project + " issue", "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create issue in %s: status=%d body=%s", project, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}

	before := createIssue("TEST")
	boundary := createIssue("TEST")
	after := createIssue("OTHER")
	updatedAt := map[string]time.Time{
		before.Key:   time.Date(2026, time.September, 10, 11, 59, 59, 0, time.UTC),
		boundary.Key: time.Date(2026, time.September, 10, 12, 0, 0, 0, time.UTC),
		after.Key:    time.Date(2026, time.September, 10, 12, 0, 0, 123457000, time.UTC),
	}
	for key, value := range updatedAt {
		if _, err := database.Pool.Exec(context.Background(), "update issues set updated_at = $2 where key = $1", key, value); err != nil {
			t.Fatalf("set %s updated_at: %v", key, err)
		}
	}

	listed := dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/issues?project=TEST&updated_since="+url.QueryEscape(updatedAt[boundary.Key].Format(time.RFC3339)), nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list issues since boundary: status=%d body=%s", listed.Code, listed.Body.String())
	}
	summaries := decodeBody[[]model.IssueSummary](t, listed)
	if len(summaries) != 1 || summaries[0].Key != boundary.Key {
		t.Fatalf("issues at or after boundary = %#v, want only %s", summaries, boundary.Key)
	}

	nanosecondListed := dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/issues?project=OTHER&updated_since="+url.QueryEscape("2026-09-10T12:00:00.123456789Z"), nil, "alice")
	if nanosecondListed.Code != http.StatusOK {
		t.Fatalf("list issues with nanosecond timestamp: status=%d body=%s", nanosecondListed.Code, nanosecondListed.Body.String())
	}
	nanosecondSummaries := decodeBody[[]model.IssueSummary](t, nanosecondListed)
	if len(nanosecondSummaries) != 1 || nanosecondSummaries[0].Key != after.Key {
		t.Fatalf("issues after nanosecond timestamp = %#v, want only %s", nanosecondSummaries, after.Key)
	}

	invalid := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?updated_since=not-a-timestamp", nil, "alice")
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"INVALID_UPDATED_SINCE"`) {
		t.Fatalf("invalid updated_since: status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestListIssuesIncludesEventUpdatedIssuesSinceWatermark(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	createIssue := func(title string) model.Issue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": "TEST", "title": title,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create issue %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}

	updatedByEvent := createIssue("Updated by message")
	atWatermark := createIssue("At watermark")
	watermark := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
	for key, updatedAt := range map[string]time.Time{
		updatedByEvent.Key: watermark.Add(-time.Minute),
		atWatermark.Key:    watermark,
	} {
		if _, err := database.Pool.Exec(context.Background(), "update issues set updated_at = $2 where key = $1", key, updatedAt); err != nil {
			t.Fatalf("set %s updated_at: %v", key, err)
		}
	}
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+updatedByEvent.Key+"/messages", map[string]string{
		"body": "Advance this issue after the watermark.",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", response.Code, response.Body.String())
	}

	listed := dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/issues?project=TEST&updated_since="+url.QueryEscape(watermark.Format(time.RFC3339Nano)), nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list issues since watermark: status=%d body=%s", listed.Code, listed.Body.String())
	}
	summaries := decodeBody[[]model.IssueSummary](t, listed)
	byKey := make(map[string]model.IssueSummary, len(summaries))
	for _, summary := range summaries {
		byKey[summary.Key] = summary
	}
	if len(byKey) != 2 || byKey[updatedByEvent.Key].Key == "" || byKey[atWatermark.Key].Key == "" {
		t.Fatalf("issues at or after watermark = %#v, want %s and %s", summaries, updatedByEvent.Key, atWatermark.Key)
	}

	eventsResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+updatedByEvent.Key+"/events", nil, "alice")
	if eventsResponse.Code != http.StatusOK {
		t.Fatalf("read issue events: status=%d body=%s", eventsResponse.Code, eventsResponse.Body.String())
	}
	events := decodeBody[[]model.Event](t, eventsResponse)
	var messageEvent *model.Event
	for index := range events {
		if events[index].Type == "message.created" {
			messageEvent = &events[index]
			break
		}
	}
	if messageEvent == nil {
		t.Fatalf("message event not found in %#v", events)
	}
	if got := byKey[updatedByEvent.Key].UpdatedAt; !got.Equal(messageEvent.CreatedAt) {
		t.Fatalf("event-updated issue timestamp = %s, want event timestamp %s", got, messageEvent.CreatedAt)
	}
}

func TestEventDoesNotMoveIssueUpdatedAtBackward(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Future issue update",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[model.Issue](t, created)
	future := time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
	if _, err := database.Pool.Exec(context.Background(), "update issues set updated_at = $2 where key = $1", issue.Key, future); err != nil {
		t.Fatalf("set future updated_at: %v", err)
	}
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]string{
		"body": "Do not move this timestamp backward.",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", response.Code, response.Body.String())
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list issues: status=%d body=%s", listed.Code, listed.Body.String())
	}
	summaries := decodeBody[[]model.IssueSummary](t, listed)
	if len(summaries) != 1 {
		t.Fatalf("listed issues = %#v, want one summary", summaries)
	}
	if got := summaries[0].UpdatedAt; !got.Equal(future) {
		t.Fatalf("issue timestamp after event = %s, want later timestamp %s", got, future)
	}
}

func TestListIssuesReportsLastSequenceAfterEvent(t *testing.T) {
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
	if message := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]string{
		"body": "An event advances the sequence.",
	}, "alice"); message.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", message.Code, message.Body.String())
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list issues: status=%d body=%s", listed.Code, listed.Body.String())
	}
	summaries := decodeBody[[]model.IssueSummary](t, listed)
	if len(summaries) != 1 {
		t.Fatalf("listed issues = %#v, want one summary", summaries)
	}
	if got, want := summaries[0].LastSeq, issue.LastSeq+1; got != want {
		t.Fatalf("list last_seq = %d, want %d after creating a message", got, want)
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
	if err := tx.QueryRow(ctx, "explain (format json) "+listIssuesQuery, "", "", "", nil, []string{}, false, []int16{}, false).Scan(&planJSON); err != nil {
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

// GET /issues/{key}/asks?state=open must also stay on the asks_open partial index even
// after Postgres switches the underlying prepared statement from a per-execution custom
// plan (built for that call's actual bound values) to a cached generic plan, which it
// automatically considers starting on a statement's 6th execution. queryOwnerAsks bakes the
// open case's state predicate into the query text as a literal instead of a bound
// parameter specifically so that no plan kind can lose the fact that it always matches
// state = 'open'; a $-parameterized predicate keeps the index for a custom plan (built
// knowing the actual bound value) but loses it for a generic one, which cannot assume the
// parameter is always 'open' and so cannot prove the partial index applies at all -
// forcing a sequential scan of asks.
//
// plan_cache_mode = force_generic_plan pins every execution (including the first) to a
// generic plan instead of relying on Postgres's cost-based custom/generic switch, which
// this test's otherwise-empty table cannot trigger reliably: with enable_seqscan = off
// also active, the cost comparison that decides whether to switch sees a near-zero custom
// plan (using the index) against an artificially inflated generic-plan estimate (assuming
// the seq scan the buggy query would need), so Postgres keeps the cheap custom plan and
// never organically reaches the generic plan this test exists to catch.
func TestListIssueAsksOpenQueryUsesAsksOpenIndex(t *testing.T) {
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
	if _, err := tx.Exec(ctx, "set local plan_cache_mode = force_generic_plan"); err != nil {
		t.Fatalf("force generic plan mode: %v", err)
	}
	if _, err := tx.Exec(ctx, "prepare list_open_asks (text) as "+listIssueAsksQueryOpen); err != nil {
		t.Fatalf("prepare open asks query: %v", err)
	}
	for i := range 6 {
		if _, err := tx.Exec(ctx, "execute list_open_asks('nonexistent-issue')"); err != nil {
			t.Fatalf("execute open asks query %d: %v", i, err)
		}
	}

	var planJSON []byte
	if err := tx.QueryRow(ctx, "explain (format json) execute list_open_asks('nonexistent-issue')").Scan(&planJSON); err != nil {
		t.Fatalf("explain execute open asks query: %v", err)
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
		t.Fatalf("open asks generic query plan sequentially scans asks; want an index scan via asks_open:\n%s", planJSON)
	}
}

func TestListIssuesPinnedFilterAndLabels(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	firstResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "First",
	}, "alice")
	if firstResponse.Code != http.StatusCreated {
		t.Fatalf("create first issue: status=%d body=%s", firstResponse.Code, firstResponse.Body.String())
	}
	first := decodeBody[model.Issue](t, firstResponse)
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+first.Key, map[string][]string{"labels": {"repo:x"}}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("set issue labels: status=%d body=%s", response.Code, response.Body.String())
	}
	secondResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Second",
	}, "alice")
	if secondResponse.Code != http.StatusCreated {
		t.Fatalf("create second issue: status=%d body=%s", secondResponse.Code, secondResponse.Body.String())
	}
	second := decodeBody[model.Issue](t, secondResponse)

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list project issues: status=%d body=%s", listed.Code, listed.Body.String())
	}
	var listedIssues []model.IssueSummary
	if listedIssues = decodeBody[[]model.IssueSummary](t, listed); len(listedIssues) != 2 {
		t.Fatalf("project issues = %#v, want two", listedIssues)
	}
	foundLabels := false
	for _, issue := range listedIssues {
		if issue.Key == first.Key && len(issue.Labels) == 1 && issue.Labels[0] == "repo:x" {
			foundLabels = true
		}
	}
	if !foundLabels {
		t.Fatalf("project issue labels = %#v, want repo:x on %s", listedIssues, first.Key)
	}

	pinned := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+second.Key+"/state", map[string]bool{"pinned": true}, "alice")
	if pinned.Code != http.StatusOK {
		t.Fatalf("pin second issue: status=%d body=%s", pinned.Code, pinned.Body.String())
	}
	onlyPinned := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?pinned=true", nil, "alice")
	if onlyPinned.Code != http.StatusOK {
		t.Fatalf("list pinned issues: status=%d body=%s", onlyPinned.Code, onlyPinned.Body.String())
	}
	if issues := decodeBody[[]model.IssueSummary](t, onlyPinned); len(issues) != 1 || issues[0].Key != second.Key {
		t.Fatalf("alice pinned issues = %#v, want %s", issues, second.Key)
	}
	if other := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?pinned=true", nil, "bob"); other.Code != http.StatusOK || len(decodeBody[[]model.IssueSummary](t, other)) != 0 {
		t.Fatalf("bob pinned issues: status=%d body=%s", other.Code, other.Body.String())
	}
	if bearer := agentRequest(t, handler, http.MethodGet, "/api/v1/issues?pinned=true", nil, "agent-token"); bearer.Code != http.StatusForbidden || !strings.Contains(bearer.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("bearer pinned issues: status=%d body=%s", bearer.Code, bearer.Body.String())
	}
}

func TestIssueLabelsCreateNormalizePatchAndFilter(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	create := func(title string, labels []string) model.Issue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "CORE", "title": title, "labels": labels,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}
	expectLabels := func(got []string, want ...string) {
		t.Helper()
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("labels = %#v, want %#v", got, want)
		}
	}

	first := create("First", []string{"Frontend", "frontend", "api"})
	expectLabels(first.Labels, "Frontend", "api")
	second := create("Second", []string{"frontend", "docs"})
	third := create("Third", []string{"backend", "docs"})
	crossCase := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE&label=frontend&label=api", nil, "alice")
	if crossCase.Code != http.StatusOK {
		t.Fatalf("filter labels case-insensitively: status=%d body=%s", crossCase.Code, crossCase.Body.String())
	}
	if issues := decodeBody[[]model.IssueSummary](t, crossCase); len(issues) != 1 || issues[0].Key != first.Key {
		t.Fatalf("issues matching frontend and api = %#v, want only %s", issues, first.Key)
	}
	duplicateLabel := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE&label=Frontend&label=frontend&label=api", nil, "alice")
	if duplicateLabel.Code != http.StatusOK {
		t.Fatalf("filter duplicate labels case-insensitively: status=%d body=%s", duplicateLabel.Code, duplicateLabel.Body.String())
	}
	if issues := decodeBody[[]model.IssueSummary](t, duplicateLabel); len(issues) != 1 || issues[0].Key != first.Key {
		t.Fatalf("issues matching duplicated frontend and api = %#v, want only %s", issues, first.Key)
	}

	updatedResponse := sessionRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+first.Key, map[string]any{
		"labels": []string{" urgent ", "URGENT", "api"},
		"actor":  sessionActor(),
	})
	if updatedResponse.Code != http.StatusOK {
		t.Fatalf("update labels: status=%d body=%s", updatedResponse.Code, updatedResponse.Body.String())
	}
	updated := decodeBody[model.Issue](t, updatedResponse)
	expectLabels(updated.Labels, "urgent", "api")

	eventsResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+first.Key+"/events", nil, "alice")
	if eventsResponse.Code != http.StatusOK {
		t.Fatalf("list label update events: status=%d body=%s", eventsResponse.Code, eventsResponse.Body.String())
	}
	var events []struct {
		Type    string `json:"type"`
		Payload struct {
			Labels []string `json:"labels"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(eventsResponse.Body).Decode(&events); err != nil {
		t.Fatalf("decode label update events: %v", err)
	}
	foundLabelUpdate := false
	for _, event := range events {
		if event.Type == "issue.updated" {
			foundLabelUpdate = true
			expectLabels(event.Payload.Labels, "urgent", "api")
			break
		}
	}
	if !foundLabelUpdate {
		t.Fatal("label update did not append an issue.updated event")
	}

	filtered := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE&label=frontend&label=docs", nil, "alice")
	if filtered.Code != http.StatusOK {
		t.Fatalf("filter labels: status=%d body=%s", filtered.Code, filtered.Body.String())
	}
	issues := decodeBody[[]model.IssueSummary](t, filtered)
	if len(issues) != 1 || issues[0].Key != second.Key {
		t.Fatalf("issues matching frontend and docs = %#v, want only %s (not %s)", issues, second.Key, third.Key)
	}
}

func TestIssueLabelsRejectInvalidInput(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Labels",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[model.Issue](t, created)

	for _, labels := range [][]string{
		{" "},
		{strings.Repeat("x", 41)},
		strings.Fields(strings.Repeat("label ", 21)),
	} {
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string][]string{
			"labels": labels,
		}, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"LABELS_INPUT"`) {
			t.Fatalf("reject labels %#v: status=%d body=%s", labels, response.Code, response.Body.String())
		}
	}
}

// A URL links exactly one issue; a second issue asking for it gets a 409 naming the
// owner instead of the unique-index violation surfacing as a 500.
func TestIssuePatchExternalLinkTakenByAnotherIssue(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	create := func(title string) model.Issue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": "CORE", "title": title,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}
	first := create("First")
	second := create("Second")
	pullRequest := "https://github.com/owner/repo/pull/7"
	link := func(key string) *httptest.ResponseRecorder {
		t.Helper()
		return sessionRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
			"external_links": []map[string]string{{"url": pullRequest}},
			"actor":          sessionActor(),
		})
	}
	if response := link(first.Key); response.Code != http.StatusOK {
		t.Fatalf("link %s: status=%d body=%s", first.Key, response.Code, response.Body.String())
	}
	// Re-linking the same URL on its owner is idempotent, not a conflict with itself.
	if response := link(first.Key); response.Code != http.StatusOK {
		t.Fatalf("re-link %s: status=%d body=%s", first.Key, response.Code, response.Body.String())
	}
	taken := link(second.Key)
	if taken.Code != http.StatusConflict {
		t.Fatalf("link taken URL on %s: status=%d body=%s", second.Key, taken.Code, taken.Body.String())
	}
	body := taken.Body.String()
	if !strings.Contains(body, `"code":"EXTERNAL_LINK_TAKEN"`) || !strings.Contains(body, first.Key) || !strings.Contains(body, pullRequest) {
		t.Fatalf("conflict body = %s, want EXTERNAL_LINK_TAKEN naming %s and %s", body, first.Key, pullRequest)
	}
	unchanged := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+second.Key, nil, "alice")
	if unchanged.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", second.Key, unchanged.Code, unchanged.Body.String())
	}
	if links := decodeBody[model.Issue](t, unchanged).ExternalLinks; len(links) != 0 {
		t.Fatalf("%s external links = %#v, want none after the refused link", second.Key, links)
	}
}

// PATCH `parent` is tri-state: a key moves the issue, null clears it. The issue's own
// issue.updated carries the new parent, the old parent hears child.removed, and the new
// parent hears child.added.
func TestIssuePatchParentSetClearAndEvents(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	create := func(title string, fields map[string]any) model.Issue {
		t.Helper()
		body := map[string]any{"project": "CORE", "title": title, "force": true}
		for name, value := range fields {
			body[name] = value
		}
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}
	oldParent := create("Old parent", nil)
	newParent := create("New parent", nil)
	child := create("Child", map[string]any{"parent": oldParent.Key})

	moved := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]any{
		"parent": newParent.Key,
	}, "alice")
	if moved.Code != http.StatusOK {
		t.Fatalf("reparent: status=%d body=%s", moved.Code, moved.Body.String())
	}
	if parent := decodeBody[model.Issue](t, moved).Parent; parent == nil || *parent != newParent.Key {
		t.Fatalf("reparented issue parent = %v, want %s", parent, newParent.Key)
	}
	updatedEvents := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+child.Key+"/events", nil, "alice")
	var childLog []struct {
		Type    string `json:"type"`
		Payload struct {
			Parent *string `json:"parent"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(updatedEvents.Body).Decode(&childLog); err != nil {
		t.Fatalf("decode child events: %v", err)
	}
	last := childLog[len(childLog)-1]
	if last.Type != "issue.updated" || last.Payload.Parent == nil || *last.Payload.Parent != newParent.Key {
		t.Fatalf("child's newest event = %#v, want issue.updated carrying parent %s", last, newParent.Key)
	}
	childEvents := func(key string) map[string]string {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, "alice")
		var log []struct {
			Type    string `json:"type"`
			Payload struct {
				ChildKey string `json:"child_key"`
			} `json:"payload"`
		}
		if err := json.NewDecoder(response.Body).Decode(&log); err != nil {
			t.Fatalf("decode %s events: %v", key, err)
		}
		found := map[string]string{}
		for _, event := range log {
			if event.Type == "child.added" || event.Type == "child.removed" {
				found[event.Type] = event.Payload.ChildKey
			}
		}
		return found
	}
	if events := childEvents(oldParent.Key); events["child.removed"] != child.Key {
		t.Fatalf("old parent events = %#v, want child.removed for %s", events, child.Key)
	}
	if events := childEvents(newParent.Key); events["child.added"] != child.Key {
		t.Fatalf("new parent events = %#v, want child.added for %s", events, child.Key)
	}

	cleared := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]any{
		"parent": nil,
	}, "alice")
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear parent: status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	if parent := decodeBody[model.Issue](t, cleared).Parent; parent != nil {
		t.Fatalf("cleared issue parent = %q, want null", *parent)
	}
	if events := childEvents(newParent.Key); events["child.removed"] != child.Key {
		t.Fatalf("new parent events after clear = %#v, want child.removed for %s", events, child.Key)
	}
}

func TestIssuePatchParentValidation(t *testing.T) {
	handler := newTestHandler(t)
	for _, project := range []string{"CORE", "SIDE"} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": project, "name": project,
		}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
		}
	}
	create := func(project, title string, fields map[string]any) model.Issue {
		t.Helper()
		body := map[string]any{"project": project, "title": title, "force": true}
		for name, value := range fields {
			body[name] = value
		}
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}
	patchParent := func(key string, parent any) *httptest.ResponseRecorder {
		t.Helper()
		return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
			"parent": parent,
		}, "alice")
	}
	issueA := create("CORE", "A", nil)
	issueB := create("CORE", "B", map[string]any{"parent": issueA.Key})
	issueC := create("CORE", "C", map[string]any{"parent": issueB.Key})
	foreign := create("SIDE", "Elsewhere", nil)

	expectRefusal := func(name string, response *httptest.ResponseRecorder, status int, fragment string) {
		t.Helper()
		body := response.Body.String()
		if response.Code != status || !strings.Contains(body, `"code":"PARENT_INPUT"`) || !strings.Contains(body, fragment) {
			t.Fatalf("%s: status=%d body=%s, want %d PARENT_INPUT containing %q", name, response.Code, body, status, fragment)
		}
	}
	expectRefusal("self parent", patchParent(issueA.Key, issueA.Key), http.StatusBadRequest, "own parent")
	expectRefusal("unknown parent", patchParent(issueA.Key, "CORE-999"), http.StatusBadRequest, "CORE-999 not found")
	expectRefusal("foreign project", patchParent(issueA.Key, foreign.Key), http.StatusBadRequest, "same project")
	expectRefusal("blank parent", patchParent(issueA.Key, "  "), http.StatusBadRequest, "blank")
	// A ← B ← C stands; making C the parent of A closes the loop.
	expectRefusal("cycle", patchParent(issueA.Key, issueC.Key), http.StatusConflict, "would create a cycle")

	closed := create("CORE", "Closed", nil)
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+closed.Key, map[string]string{
		"status": "done",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", response.Code, response.Body.String())
	}
	refused := patchParent(closed.Key, issueA.Key)
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), `"code":"ISSUE_CLOSED"`) {
		t.Fatalf("reparent closed issue: status=%d body=%s, want 409 ISSUE_CLOSED", refused.Code, refused.Body.String())
	}
}

// Creating with a bad parent is a named 400, not the FK's opaque 500, and a parent from
// another project is refused.
func TestIssueCreateParentValidation(t *testing.T) {
	handler := newTestHandler(t)
	for _, project := range []string{"CORE", "SIDE"} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": project, "name": project,
		}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
		}
	}
	foreignResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "SIDE", "title": "Elsewhere",
	}, "alice")
	if foreignResponse.Code != http.StatusCreated {
		t.Fatalf("create foreign issue: status=%d body=%s", foreignResponse.Code, foreignResponse.Body.String())
	}
	foreign := decodeBody[model.Issue](t, foreignResponse)
	for name, parent := range map[string]string{
		"unknown parent": "CORE-999",
		"foreign parent": foreign.Key,
	} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "CORE", "title": "Child", "parent": parent, "force": true,
		}, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"PARENT_INPUT"`) {
			t.Fatalf("%s: status=%d body=%s, want 400 PARENT_INPUT", name, response.Code, response.Body.String())
		}
	}
}

// Each Children row rolls up its whole subtree: the child itself plus every descendant,
// done = status 'done', active_at = the subtree's newest updated_at, and the child's own
// external links ride along.
func TestIssueChildrenSubtreeRollup(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	create := func(title string, fields map[string]any) model.Issue {
		t.Helper()
		body := map[string]any{"project": "CORE", "title": title, "force": true}
		for name, value := range fields {
			body[name] = value
		}
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}
	root := create("Root", nil)
	branch := create("Branch", map[string]any{"parent": root.Key})
	grandchild := create("Grandchild", map[string]any{"parent": branch.Key})
	leaf := create("Leaf", map[string]any{"parent": root.Key})

	pullRequest := "https://github.com/owner/repo/pull/12"
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+branch.Key, map[string]any{
		"external_links": []map[string]string{{"url": pullRequest, "kind": "github_pr"}},
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("link branch: status=%d body=%s", response.Code, response.Body.String())
	}
	closedResponse := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+grandchild.Key, map[string]string{
		"status": "done",
	}, "alice")
	if closedResponse.Code != http.StatusOK {
		t.Fatalf("close grandchild: status=%d body=%s", closedResponse.Code, closedResponse.Body.String())
	}
	closedGrandchild := decodeBody[model.Issue](t, closedResponse)

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+root.Key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("read root: status=%d body=%s", detail.Code, detail.Body.String())
	}
	children := decodeBody[struct {
		Children []model.IssueChild `json:"children"`
	}](t, detail).Children
	if len(children) != 2 {
		t.Fatalf("root children = %#v, want branch and leaf", children)
	}
	rows := map[string]model.IssueChild{}
	for _, child := range children {
		rows[child.Key] = child
	}
	branchRow := rows[branch.Key]
	if branchRow.SubtreeDone != 1 || branchRow.SubtreeTotal != 2 {
		t.Fatalf("branch rollup = %d/%d, want 1/2", branchRow.SubtreeDone, branchRow.SubtreeTotal)
	}
	if !branchRow.ActiveAt.Equal(closedGrandchild.UpdatedAt) {
		t.Fatalf("branch active_at = %s, want the done grandchild's %s", branchRow.ActiveAt, closedGrandchild.UpdatedAt)
	}
	if len(branchRow.ExternalLinks) != 1 || branchRow.ExternalLinks[0].URL != pullRequest {
		t.Fatalf("branch external links = %#v, want %s", branchRow.ExternalLinks, pullRequest)
	}
	leafRow := rows[leaf.Key]
	if leafRow.SubtreeDone != 0 || leafRow.SubtreeTotal != 1 {
		t.Fatalf("leaf rollup = %d/%d, want 0/1", leafRow.SubtreeDone, leafRow.SubtreeTotal)
	}
	if !leafRow.ActiveAt.Equal(leaf.UpdatedAt) {
		t.Fatalf("leaf active_at = %s, want its own %s", leafRow.ActiveAt, leaf.UpdatedAt)
	}
	if len(leafRow.ExternalLinks) != 0 {
		t.Fatalf("leaf external links = %#v, want none", leafRow.ExternalLinks)
	}
}

// blocked_by is an issue-to-issue dependency: create and PATCH replace its complete target
// set, every issue payload exposes it, and list rows include it with the primary artifact id.
func TestIssueBlockedByRoundTrip(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	type issueRead struct {
		Key               string   `json:"key"`
		BlockedBy         []string `json:"blocked_by"`
		PrimaryArtifactID string   `json:"primary_artifact_id"`
	}
	create := func(title string, fields map[string]any) issueRead {
		t.Helper()
		body := map[string]any{"project": "CORE", "title": title}
		for name, value := range fields {
			body[name] = value
		}
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[issueRead](t, response)
	}
	first := create("First blocker", nil)
	second := create("Second blocker", nil)
	dependent := create("Dependent", map[string]any{"blocked_by": []string{second.Key, first.Key}})
	want := []string{first.Key, second.Key}
	if !slices.Equal(dependent.BlockedBy, want) {
		t.Fatalf("created blocked_by = %v, want %v", dependent.BlockedBy, want)
	}

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+dependent.Key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("read issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	if got := decodeBody[issueRead](t, detail).BlockedBy; !slices.Equal(got, want) {
		t.Fatalf("read blocked_by = %v, want %v", got, want)
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list issues: status=%d body=%s", listed.Code, listed.Body.String())
	}
	var listedDependent *issueRead
	for _, row := range decodeBody[[]issueRead](t, listed) {
		if row.Key == dependent.Key {
			listedDependent = &row
			break
		}
	}
	if listedDependent == nil || !slices.Equal(listedDependent.BlockedBy, want) || listedDependent.PrimaryArtifactID == "" {
		t.Fatalf("list row = %#v, want blocked_by=%v and a primary_artifact_id", listedDependent, want)
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+dependent.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list events: status=%d body=%s", events.Code, events.Body.String())
	}
	var log []struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(events.Body).Decode(&log); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	foundCreated := false
	for _, event := range log {
		if event.Type != "issue.created" {
			continue
		}
		foundCreated = true
		var payload issueRead
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode issue.created payload: %v", err)
		}
		if !slices.Equal(payload.BlockedBy, want) {
			t.Fatalf("issue.created blocked_by = %v, want %v", payload.BlockedBy, want)
		}
	}
	if !foundCreated {
		t.Fatalf("issue events = %s, want issue.created", events.Body.String())
	}

	cleared := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+dependent.Key, map[string]any{
		"blocked_by": []string{},
	}, "alice")
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear blocked_by: status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	if got := decodeBody[issueRead](t, cleared).BlockedBy; len(got) != 0 {
		t.Fatalf("patched blocked_by = %v, want []", got)
	}
}

// A blocked_by dependency cannot make a child wait on its parent, a parent wait on its descendant,
// or any issue wait on itself. One sibling may wait on another, and blockers never cross a
// Dispatch project boundary.
func TestIssueBlockedByRefusesDependencyCycles(t *testing.T) {
	handler := newTestHandler(t)
	for _, project := range []string{"CORE", "SIDE"} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": project, "name": project,
		}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
		}
	}
	create := func(project, title string, fields map[string]any) model.Issue {
		t.Helper()
		body := map[string]any{"project": project, "title": title, "force": true}
		for name, value := range fields {
			body[name] = value
		}
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}
	expectRefusal := func(name string, response *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if response.Code != status || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("%s: status=%d body=%s, want %d %s", name, response.Code, response.Body.String(), status, code)
		}
	}

	first := create("CORE", "First", nil)
	second := create("CORE", "Second", map[string]any{"blocked_by": []string{first.Key}})
	expectRefusal("dependency loop", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+first.Key, map[string]any{
		"blocked_by": []string{second.Key},
	}, "alice"), http.StatusConflict, "DEPENDENCY_CYCLE")

	parent := create("CORE", "Parent", nil)
	child := create("CORE", "Child", map[string]any{"parent": parent.Key})
	expectRefusal("child waits on its parent", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]any{
		"blocked_by": []string{parent.Key},
	}, "alice"), http.StatusConflict, "DEPENDENCY_CYCLE")
	create("CORE", "Sibling", map[string]any{"parent": parent.Key, "blocked_by": []string{child.Key}})

	childToReparent := create("CORE", "Child to reparent", nil)
	intermediate := create("CORE", "Intermediate", map[string]any{"blocked_by": []string{childToReparent.Key}})
	parentWithBlocker := create("CORE", "Parent with blocker", map[string]any{
		"blocked_by": []string{intermediate.Key},
	})
	expectRefusal("reparent closes dependency loop", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+childToReparent.Key, map[string]any{
		"parent": parentWithBlocker.Key,
	}, "alice"), http.StatusConflict, "DEPENDENCY_CYCLE")

	foreign := create("SIDE", "Foreign", nil)
	expectRefusal("foreign-project blocker", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Cross-project dependency", "blocked_by": []string{foreign.Key},
	}, "alice"), http.StatusBadRequest, "BLOCKED_BY_OUTSIDE_PROJECT")
}

func TestIssueBlockedByConcurrentWritesDoNotDeadlock(t *testing.T) {
	for _, scenario := range []string{"reciprocal blockers", "blocker and reparent", "old parent and reparent"} {
		t.Run(scenario, func(t *testing.T) {
			handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
			if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
				"key": "CORE", "name": "Core",
			}, "alice"); response.Code != http.StatusCreated {
				t.Fatalf("create project: %d %s", response.Code, response.Body.String())
			}
			create := func(title string, parent *string) string {
				t.Helper()
				response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
					"project": "CORE", "title": title, "force": true, "parent": parent,
				}, "alice")
				if response.Code != http.StatusCreated {
					t.Fatalf("create issue: %d %s", response.Code, response.Body.String())
				}
				return decodeBody[model.Issue](t, response).Key
			}
			for round := range 8 {
				first := create(fmt.Sprintf("First %d", round), nil)
				second := create(fmt.Sprintf("Second %d", round), nil)
				gateKey := first
				firstKey, secondKey := first, second
				firstBody := map[string]any{"blocked_by": []string{second}}
				secondBody := map[string]any{"blocked_by": []string{first}}
				wantConflicts := 1
				switch scenario {
				case "blocker and reparent":
					firstBody = map[string]any{"parent": second}
				case "old parent and reparent":
					child := create(fmt.Sprintf("Child %d", round), &first)
					gateKey, firstKey, secondKey = child, child, first
					firstBody = map[string]any{"parent": second}
					secondBody = map[string]any{"blocked_by": []string{child}}
					wantConflicts = 0
				}
				gate, err := database.Pool.Begin(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
				if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, gateKey); err != nil {
					t.Fatal(err)
				}
				responses := make(chan *httptest.ResponseRecorder, 2)
				go func() {
					responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+firstKey, firstBody, "alice")
				}()
				waitForDatabaseLocks(t, gate, 1)
				go func() {
					responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+secondKey, secondBody, "alice")
				}()
				waitForDatabaseLocks(t, gate, 2)
				if err := gate.Commit(context.Background()); err != nil {
					t.Fatal(err)
				}
				successes, conflicts := 0, 0
				for range 2 {
					response := awaitResponse(t, responses)
					switch response.Code {
					case http.StatusOK:
						successes++
					case http.StatusConflict:
						refusal := decodeBody[struct {
							Code string `json:"code"`
						}](t, response)
						if refusal.Code != "DEPENDENCY_CYCLE" {
							t.Fatalf("round %d: refusal = %s, want DEPENDENCY_CYCLE", round, response.Body.String())
						}
						conflicts++
					default:
						t.Errorf("round %d: %d %s", round, response.Code, response.Body.String())
					}
				}
				if successes != 2-wantConflicts || conflicts != wantConflicts {
					t.Fatalf("round %d: successes=%d conflicts=%d, want %d and %d",
						round, successes, conflicts, 2-wantConflicts, wantConflicts)
				}
			}
		})
	}
}

func TestIssueBlockedByCountLimitOnCreateAndPatch(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: %d %s", response.Code, response.Body.String())
	}
	var blockers []string
	for index := range 21 {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "CORE", "title": fmt.Sprintf("Blocker %d", index), "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create blocker: %d %s", response.Code, response.Body.String())
		}
		blockers = append(blockers, decodeBody[model.Issue](t, response).Key)
	}
	atCap := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Dependent at cap", "blocked_by": blockers[:20],
	}, "alice")
	if atCap.Code != http.StatusCreated {
		t.Fatalf("create at cap: %d %s", atCap.Code, atCap.Body.String())
	}
	dependent := decodeBody[model.Issue](t, atCap)
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			path := "/api/v1/issues"
			body := map[string]any{"project": "CORE", "title": "Too many blockers", "blocked_by": blockers, "force": true}
			if method == http.MethodPatch {
				path += "/" + dependent.Key
				body = map[string]any{"blocked_by": blockers}
			}
			response := dispatchRequest(t, handler, method, path, body, "alice")
			if response.Code != http.StatusBadRequest {
				t.Fatalf("over cap: %d %s, want 400", response.Code, response.Body.String())
			}
			refusal := decodeBody[struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}](t, response)
			if refusal.Code != "BLOCKED_BY_INPUT" || !strings.Contains(refusal.Error, "20") || !strings.Contains(refusal.Error, "blocked_by") {
				t.Fatalf("over-cap refusal must name blocked_by and its limit: %+v", refusal)
			}
		})
	}
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+dependent.Key, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read after refusal: %d %s", read.Code, read.Body.String())
	}
	if got := decodeBody[model.Issue](t, read).BlockedBy; !slices.Equal(got, dependent.BlockedBy) {
		t.Fatalf("refused replacement changed blockers: %v, want %v", got, dependent.BlockedBy)
	}
}

func TestIssueBlockedByIgnoresIndirectRowActivity(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: %d %s", response.Code, response.Body.String())
	}
	var keys []string
	for index := range 3 {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "CORE", "title": fmt.Sprintf("Node %d", index), "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create node: %d %s", response.Code, response.Body.String())
		}
		keys = append(keys, decodeBody[model.Issue](t, response).Key)
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+keys[1], map[string]any{
		"blocked_by": []string{keys[2]},
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("seed dependency: %d %s", response.Code, response.Body.String())
	}
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, keys[2]); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	body := map[string]any{"blocked_by": []string{keys[1]}}
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+keys[0], body, "alice")
	}()
	response := awaitResponse(t, responses)
	if response.Code != http.StatusOK || !slices.Equal(decodeBody[model.Issue](t, response).BlockedBy, []string{keys[1]}) {
		t.Fatalf("busy indirect target must not block the dependency write: %d %s", response.Code, response.Body.String())
	}
}

func TestStatusPatchRacingReparentUsesCommittedParent(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 3)
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, keys[1], keys[2]); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+keys[2], map[string]string{"status": "todo"}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 1)
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	response := awaitResponse(t, responses)
	if response.Code != http.StatusOK {
		t.Fatalf("status after reparent: %d %s", response.Code, response.Body.String())
	}
	issue := decodeBody[model.Issue](t, response)
	if issue.Status != "todo" || issue.Parent == nil || *issue.Parent != keys[1] {
		t.Fatalf("status write lost the committed parent: %+v", issue)
	}
}

func TestReparentIgnoresUnrelatedGrandchildRowActivity(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 4)
	if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, keys[3], keys[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, keys[0], keys[1]); err != nil {
		t.Fatal(err)
	}
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, keys[1]); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+keys[0], map[string]string{"parent": keys[2]}, "alice")
	}()
	response := awaitResponse(t, responses)
	if response.Code != http.StatusOK {
		t.Fatalf("reparent with a busy descendant: %d %s", response.Code, response.Body.String())
	}
	if parent := decodeBody[model.Issue](t, response).Parent; parent == nil || *parent != keys[2] {
		t.Fatalf("reparent did not persist: %v", parent)
	}
}

// A status write locks its issue and then its parent. Reparenting that issue's sibling under it
// must not hold the shared parent while it waits for the new parent's row: the status write would
// hold that row and wait for the shared parent, and Postgres would abort one of them.
func TestReparentUnderSiblingDoesNotDeadlockWithItsStatusWrite(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 3)
	parent, moving, sibling := keys[0], keys[1], keys[2]
	if _, err := database.Pool.Exec(context.Background(), `
		update issues set parent_key = $1 where key = any($2::text[])
	`, parent, []string{moving, sibling}); err != nil {
		t.Fatal(err)
	}
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, parent); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+moving, map[string]string{"parent": sibling}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+sibling, map[string]string{"status": "todo"}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 2)
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if response := awaitResponse(t, responses); response.Code != http.StatusOK {
			t.Errorf("reparent beside a status write on the new parent: %d %s", response.Code, response.Body.String())
		}
	}
	if issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+moving, nil, "alice")); issue.Parent == nil || *issue.Parent != sibling {
		t.Fatalf("reparent did not persist: %v", issue.Parent)
	}
	if issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+sibling, nil, "alice")); issue.Status != "todo" {
		t.Fatalf("status write did not persist: %s", issue.Status)
	}
}

// A board move locks its two rank neighbours, and a reparent locks the issue and its new parent,
// each pair in key order. Reparenting one neighbour under the other while an issue moves between
// them therefore queues one write behind the other, whichever of the two the board shows first,
// rather than each holding one neighbour while waiting on the other.
func TestReparentDoesNotDeadlockWithBoardMoveBetweenItsEnds(t *testing.T) {
	for _, scenario := range []struct {
		name string
		// moving, parent and child index the three issues in creation order, which is key order.
		moving, parent, child int
		// childFirst moves the child ahead of its parent on the board before the race.
		childFirst bool
	}{
		{name: "board order matches key order", moving: 0, parent: 1, child: 2},
		{name: "board order reverses key order", moving: 2, parent: 0, child: 1, childFirst: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
			keys := createListedIssues(t, handler, "CORE", 3)
			moving, parent, child := keys[scenario.moving], keys[scenario.parent], keys[scenario.child]
			after, before := parent, child
			if scenario.childFirst {
				if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]any{
					"rank": map[string]string{"before": parent},
				}, "alice"); response.Code != http.StatusOK {
					t.Fatalf("rank the child first: %d %s", response.Code, response.Body.String())
				}
				after, before = child, parent
			}
			// The gate holds the neighbour the move names second, so the reparent waits for it and
			// the move reaches the other neighbour before the gate opens.
			gate, err := database.Pool.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Rollback(context.Background())
			if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, before); err != nil {
				t.Fatal(err)
			}
			responses := make(chan *httptest.ResponseRecorder, 2)
			go func() {
				responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]string{"parent": parent}, "alice")
			}()
			waitForDatabaseLocks(t, gate, 1)
			go func() {
				responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+moving, map[string]any{
					"rank": map[string]string{"after": after, "before": before},
				}, "alice")
			}()
			waitForDatabaseLocks(t, gate, 2)
			if err := gate.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if response := awaitResponse(t, responses); response.Code != http.StatusOK {
					t.Errorf("reparent beside a board move between its ends: %d %s", response.Code, response.Body.String())
				}
			}
			issues := map[string]model.Issue{}
			for _, key := range keys {
				issues[key] = decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice"))
			}
			if got := issues[child].Parent; got == nil || *got != parent {
				t.Fatalf("reparent did not persist: %v", got)
			}
			if !(issues[after].Rank < issues[moving].Rank && issues[moving].Rank < issues[before].Rank) {
				t.Fatalf("board move did not put the issue between its neighbours: %q < %q < %q",
					issues[after].Rank, issues[moving].Rank, issues[before].Rank)
			}
		})
	}
}

// A PATCH locks the issue, its new parent and its rank neighbours together, in one key order.
// Moving an issue on the board while it is reparented under one of its neighbours therefore never
// has one write holding the issue while the other holds the neighbour: a move waiting for its
// gated first neighbour holds nothing yet, so the reparent finishes before the gate opens.
func TestReparentDoesNotDeadlockWithABoardMoveOfTheSameIssue(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 3)
	after, before, moving := keys[0], keys[1], keys[2]
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, after); err != nil {
		t.Fatal(err)
	}
	moves := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		moves <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+moving, map[string]any{
			"rank": map[string]string{"after": after, "before": before},
		}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 1)
	reparents := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		reparents <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+moving, map[string]string{"parent": before}, "alice")
	}()
	select {
	case response := <-reparents:
		if response.Code != http.StatusOK {
			t.Fatalf("reparent beside a waiting board move of the same issue: %d %s", response.Code, response.Body.String())
		}
	case <-time.After(2 * time.Second):
		// The reparent is waiting for the move. Open the gate and report how the two writes end.
		if err := gate.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		reparent, move := awaitResponse(t, reparents), awaitResponse(t, moves)
		t.Fatalf("reparent waited for a board move of the same issue; once the gate opened the reparent answered %d %s and the move %d %s",
			reparent.Code, reparent.Body.String(), move.Code, move.Body.String())
	}
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if response := awaitResponse(t, moves); response.Code != http.StatusOK {
		t.Fatalf("board move after the gate opened: %d %s", response.Code, response.Body.String())
	}
	issues := map[string]model.Issue{}
	for _, key := range keys {
		issues[key] = decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice"))
	}
	if got := issues[moving].Parent; got == nil || *got != before {
		t.Fatalf("reparent did not persist: %v", got)
	}
	if !(issues[after].Rank < issues[moving].Rank && issues[moving].Rank < issues[before].Rank) {
		t.Fatalf("board move did not put the issue between its neighbours: %q < %q < %q",
			issues[after].Rank, issues[moving].Rank, issues[before].Rank)
	}
}

// A status write appends to its issue's parent, so it locks the issue and that parent together, in
// key order, as a board move locks its neighbours. A child shown above its parent on the board,
// with an issue moved between the two while the child's status is written, therefore never has the
// move holding the parent while the status write holds the child.
func TestStatusWriteDoesNotDeadlockWithBoardMoveBetweenTheIssueAndItsParent(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 3)
	parent, child, moving := keys[0], keys[1], keys[2]
	if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, parent, child); err != nil {
		t.Fatal(err)
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]any{
		"rank": map[string]string{"before": parent},
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("rank the child above its parent: %d %s", response.Code, response.Body.String())
	}
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, child); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]string{"status": "todo"}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+moving, map[string]any{
			"rank": map[string]string{"after": child, "before": parent},
		}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 2)
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if response := awaitResponse(t, responses); response.Code != http.StatusOK {
			t.Errorf("status write beside a board move between the issue and its parent: %d %s", response.Code, response.Body.String())
		}
	}
	issues := map[string]model.Issue{}
	for _, key := range keys {
		issues[key] = decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice"))
	}
	if issues[child].Status != "todo" {
		t.Fatalf("status write did not persist: %s", issues[child].Status)
	}
	if !(issues[child].Rank < issues[moving].Rank && issues[moving].Rank < issues[parent].Rank) {
		t.Fatalf("board move did not put the issue between its neighbours: %q < %q < %q",
			issues[child].Rank, issues[moving].Rank, issues[parent].Rank)
	}
}

// A reparent locks the issue's old parent together with the issue and its new parent, in key
// order. Moving an issue up to its grandparent while its old parent's status is written therefore
// never has the reparent holding the grandparent while the status write holds the old parent.
func TestReparentToGrandparentDoesNotDeadlockWithAStatusWriteOnTheOldParent(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 3)
	grandparent, parent, child := keys[0], keys[1], keys[2]
	for _, link := range [][2]string{{parent, grandparent}, {child, parent}} {
		if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, link[1], link[0]); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, grandparent); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]string{"parent": grandparent}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+parent, map[string]string{"status": "todo"}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 2)
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if response := awaitResponse(t, responses); response.Code != http.StatusOK {
			t.Errorf("reparent to the grandparent beside a status write on the old parent: %d %s", response.Code, response.Body.String())
		}
	}
	if issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+child, nil, "alice")); issue.Parent == nil || *issue.Parent != grandparent {
		t.Fatalf("reparent did not persist: %v", issue.Parent)
	}
	if issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+parent, nil, "alice")); issue.Status != "todo" {
		t.Fatalf("status write did not persist: %s", issue.Status)
	}
}

// A status write reads its issue's parent before it locks the pair, and a reparent of the issue
// can commit in between. It then takes its locks again for the parent that stands; a parent that
// moves at every attempt is refused with 409 PARENT_CONTENDED and nothing applied, never locked
// out of key order.
func TestStatusWriteRefusesAParentThatMovesAtEveryAttempt(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 4)
	first, second, third, child := keys[0], keys[1], keys[2], keys[3]
	if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, first, child); err != nil {
		t.Fatal(err)
	}
	// The first attempt locks the first parent and waits for the child, which this gate holds.
	childGate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer childGate.Rollback(context.Background())
	if _, err := childGate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, child); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]string{"status": "todo"}, "alice")
	}()
	waitForDatabaseLocks(t, childGate, 1)
	// The second attempt will lock the second parent first, which this gate holds.
	parentGate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer parentGate.Rollback(context.Background())
	if _, err := parentGate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, second); err != nil {
		t.Fatal(err)
	}
	if _, err := childGate.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, second, child); err != nil {
		t.Fatal(err)
	}
	if err := childGate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForDatabaseLocks(t, parentGate, 1)
	if _, err := parentGate.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, third, child); err != nil {
		t.Fatal(err)
	}
	if err := parentGate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	response := awaitResponse(t, responses)
	if response.Code != http.StatusConflict {
		t.Fatalf("status write whose parent moved at every attempt: %d %s", response.Code, response.Body.String())
	}
	if refusal := decodeBody[struct {
		Code string `json:"code"`
	}](t, response); refusal.Code != "PARENT_CONTENDED" {
		t.Fatalf("refusal = %s, want PARENT_CONTENDED", response.Body.String())
	}
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+child, nil, "alice"))
	if issue.Status == "todo" || issue.Parent == nil || *issue.Parent != third {
		t.Fatalf("the refused write applied something: status %s, parent %v", issue.Status, issue.Parent)
	}
}

func TestChildCreateWithoutBlockersDoesNotWaitOnParentRow(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	parent := createListedIssues(t, handler, "CORE", 1)[0]
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, parent); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "CORE", "title": "Child while parent is busy", "parent": parent, "force": true,
		}, "alice")
	}()
	response := awaitResponse(t, responses)
	if response.Code != http.StatusCreated {
		t.Fatalf("child with a busy parent: %d %s", response.Code, response.Body.String())
	}
	if actual := decodeBody[model.Issue](t, response).Parent; actual == nil || *actual != parent {
		t.Fatalf("created parent = %v, want %s", actual, parent)
	}
}

func TestConcurrentThreeWayDependenciesDoNotCommitCycle(t *testing.T) {
	handler := newTestHandler(t)
	keys := createListedIssues(t, handler, "CORE", 3)
	for round := range 20 {
		for _, key := range keys {
			response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"blocked_by": []string{}}, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("clear round %d: %d %s", round, response.Code, response.Body.String())
			}
		}
		start := make(chan struct{})
		responses := make(chan *httptest.ResponseRecorder, 3)
		for index, key := range keys {
			go func() {
				<-start
				responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
					"blocked_by": []string{keys[(index+1)%3]},
				}, "alice")
			}()
		}
		close(start)
		successes, refused := 0, 0
		for range 3 {
			response := awaitResponse(t, responses)
			switch {
			case response.Code == http.StatusOK:
				successes++
			case response.Code == http.StatusConflict && strings.Contains(response.Body.String(), `"code":"DEPENDENCY_CYCLE"`):
				refused++
			default:
				t.Fatalf("ring round %d: %d %s", round, response.Code, response.Body.String())
			}
		}
		edges := 0
		for index, key := range keys {
			response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("read ring: %d %s", response.Code, response.Body.String())
			}
			targets := decodeBody[model.Issue](t, response).BlockedBy
			if slices.Equal(targets, []string{keys[(index+1)%3]}) {
				edges++
			} else if len(targets) != 0 {
				t.Fatalf("unexpected ring edge: %s -> %v", key, targets)
			}
		}
		if successes != 2 || refused != 1 || edges != 2 {
			t.Fatalf("ring round %d: successes=%d refusals=%d committed edges=%d", round, successes, refused, edges)
		}
	}
}

// A board move locks its rank neighbours with every other row it names, in one key order, so while
// it waits for its gated anchor it holds its other neighbour (CORE-12 sorts before CORE-2). A
// status write on that neighbour waits for the move and finishes once the gate opens; a dependency
// write naming the neighbour finishes while the move still waits.
func TestBoardMoveStatusAndDependencyWriteDoNotDeadlock(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 12)
	anchor, parent, moving, dependent, neighbor := keys[1], keys[2], keys[3], keys[4], keys[11]
	if _, err := database.Pool.Exec(context.Background(), `
		update issues set parent_key = $1 where key = any($2::text[])
	`, parent, []string{moving, neighbor}); err != nil {
		t.Fatal(err)
	}
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(context.Background(), `select key from issues where key = $1 for no key update`, anchor); err != nil {
		t.Fatal(err)
	}
	moves := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		moves <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+moving, map[string]any{
			"rank": map[string]string{"after": anchor, "before": neighbor}, "status": "todo",
		}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 1)
	statusWrites := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		statusWrites <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+neighbor, map[string]string{"status": "todo"}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 2)
	dependencyWrites := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		dependencyWrites <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+dependent, map[string]any{"blocked_by": []string{neighbor}}, "alice")
	}()
	if response := awaitResponse(t, dependencyWrites); response.Code != http.StatusOK {
		t.Fatalf("dependency write beside a waiting board move: %d %s", response.Code, response.Body.String())
	}
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if response := awaitResponse(t, moves); response.Code != http.StatusOK || decodeBody[model.Issue](t, response).Status != "todo" {
		t.Fatalf("board move: %d %s", response.Code, response.Body.String())
	}
	if response := awaitResponse(t, statusWrites); response.Code != http.StatusOK || decodeBody[model.Issue](t, response).Status != "todo" {
		t.Fatalf("status write on the board move's neighbour: %d %s", response.Code, response.Body.String())
	}
}

func TestDependencyWaitDoesNotHoldIssueRankOrNumberLocks(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	keys := createListedIssues(t, handler, "CORE", 3)
	other := createListedIssues(t, handler, "SIDE", 2)
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if err := lockProjectIssueDependencies(context.Background(), gate, "CORE"); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		waiting <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+keys[0], map[string]any{
			"blocked_by": []string{keys[1]}, "rank": map[string]string{"after": keys[1]},
		}, "alice")
	}()
	go func() {
		waiting <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "CORE", "title": "Waiting dependent", "blocked_by": []string{keys[1]}, "force": true,
		}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 2)
	for _, write := range []struct {
		method string
		path   string
		body   map[string]any
		status int
	}{
		{http.MethodPatch, "/api/v1/issues/" + keys[0], map[string]any{"status": "todo"}, http.StatusOK},
		{http.MethodPatch, "/api/v1/issues/" + keys[2], map[string]any{"rank": map[string]string{"after": keys[1]}}, http.StatusOK},
		{http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Independent root", "force": true}, http.StatusCreated},
		{http.MethodPatch, "/api/v1/issues/" + other[0], map[string]any{"blocked_by": []string{other[1]}}, http.StatusOK},
	} {
		responses := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			responses <- dispatchRequest(t, handler, write.method, write.path, write.body, "alice")
		}()
		response := awaitResponse(t, responses)
		if response.Code != write.status {
			t.Fatalf("unrelated write during dependency wait: %s %s: %d %s", write.method, write.path, response.Code, response.Body.String())
		}
	}
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		response := awaitResponse(t, waiting)
		if response.Code != http.StatusOK && response.Code != http.StatusCreated {
			t.Fatalf("dependency write after release: %d %s", response.Code, response.Body.String())
		}
		if got := decodeBody[model.Issue](t, response).BlockedBy; !slices.Equal(got, []string{keys[1]}) {
			t.Fatalf("dependency write lost its target: %v", got)
		}
	}
}
