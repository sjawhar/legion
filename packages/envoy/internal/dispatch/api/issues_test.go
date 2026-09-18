package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	if err := tx.QueryRow(ctx, "explain (format json) "+listIssuesQuery, "", "", "", nil, []string{}, false).Scan(&planJSON); err != nil {
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
