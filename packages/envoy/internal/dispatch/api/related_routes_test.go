package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// relatedWrites counts the rows a write or a recorded check leaves: the no-write route must leave
// every count as it found it.
func relatedWrites(t *testing.T, database *store.Store) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range []string{"issues", "asks", "write_suggestions", "related_checks"} {
		var count int
		if err := database.Pool.QueryRow(context.Background(), "select count(*) from "+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = count
	}
	return counts
}

// expectNothingWritten fails when any count relatedWrites reads moved from before.
func expectNothingWritten(t *testing.T, database *store.Store, before map[string]int) {
	t.Helper()
	if after := relatedWrites(t, database); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("row counts went from %v to %v, want the no-write route to write nothing", before, after)
	}
}

// getRelated GETs the no-write route with params, failing unless it answers 200.
func getRelated(t *testing.T, handler http.Handler, project string, params url.Values) testSuggestions {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/"+project+"/related?"+params.Encode(), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET related %v: status=%d body=%s", params, response.Code, response.Body.String())
	}
	return decodeBody[testSuggestions](t, response)
}

func relatedOwners(suggestions testSuggestions) []string {
	owners := make([]string, len(suggestions.Related))
	for index, item := range suggestions.Related {
		owners[index] = item.Owner.Key
	}
	return owners
}

// A title through GET finds the item holding its rare words and none of the thirty sharing only
// a common one, and the check leaves no row behind.
func TestRelatedRouteGetWithATitleFindsTheRareWordMatchAndWritesNothing(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "RGET")
	for index := range 30 {
		createSearchIssue(t, handler, "RGET", fmt.Sprintf("Dashboard tile quo%c%cvex", 'a'+index/26, 'a'+index%26), "")
	}
	target := createSearchIssue(t, handler, "RGET", "Dashboard ledger for zymurgy batches", "")
	refreshTermStats(t, database, "RGET")
	before := relatedWrites(t, database)

	suggestions := getRelated(t, handler, "RGET", url.Values{"title": {"Zymurgy ledger totals are wrong on the dashboard"}})
	if suggestions.Outcome != "found" || !slices.Equal(relatedOwners(suggestions), []string{target}) {
		t.Fatalf("suggestions = %+v, want %s alone, found", suggestions, target)
	}
	if suggestions.Search != "keywords_only" || suggestions.SearchNote != "meaning search is off on this server" {
		t.Fatalf("suggestions = %+v, want keywords only, meaning search being off", suggestions)
	}
	expectNothingWritten(t, database, before)
}

// A spec longer than a URL may carry goes through POST, is checked whole (the rare words sit at
// its end) and writes nothing; the same text through GET is over the search cap.
func TestRelatedRoutePostChecksALongSpec(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "RPOST")
	target := createSearchIssue(t, handler, "RPOST", "Brewing records", "Our zymurgy fermentation log.")
	createSearchIssue(t, handler, "RPOST", "Harbour crane schedule", "")
	refreshTermStats(t, database, "RPOST")
	before := relatedWrites(t, database)

	spec := strings.Repeat("Notes about the weather today. ", 300) + "The zymurgy fermentation totals drift."
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/RPOST/related", map[string]any{
		"title": "Quarterly notes", "text": spec,
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("POST related: status=%d body=%s", response.Code, response.Body.String())
	}
	suggestions := decodeBody[testSuggestions](t, response)
	if suggestions.Outcome != "found" || !slices.Equal(relatedOwners(suggestions), []string{target}) {
		t.Fatalf("suggestions = %+v, want %s alone, its spec holding the words at the long text's end", suggestions, target)
	}
	expectNothingWritten(t, database, before)

	tooLong := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/RPOST/related?"+url.Values{
		"title": {"Quarterly notes"}, "text": {spec},
	}.Encode(), nil, "alice")
	if tooLong.Code != http.StatusBadRequest || !strings.Contains(tooLong.Body.String(), `"code":"CAP_EXCEEDED"`) {
		t.Fatalf("GET of the long spec: status=%d body=%s, want 400 CAP_EXCEEDED", tooLong.Code, tooLong.Body.String())
	}
}

// exclude_issue leaves that issue and what it holds (here a comment with the same words) out of
// the items; another issue holding the words is still listed. An issue that is not the project's
// is refused.
func TestRelatedRouteExcludeIssueLeavesItsOwnItemsOut(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "REXCL")
	createSuggestionProject(t, handler, "ROTHER")
	excluded := createSearchIssue(t, handler, "REXCL", "Zymurgy ledger drifts", "")
	addIssueComment(t, handler, excluded, "The zymurgy ledger drifted again.")
	other := createSearchIssue(t, handler, "REXCL", "Zymurgy ledger totals", "")
	elsewhere := createSearchIssue(t, handler, "ROTHER", "Unrelated", "")
	refreshTermStats(t, database, "REXCL")
	before := relatedWrites(t, database)

	params := url.Values{"title": {"Zymurgy ledger"}}
	if owners := relatedOwners(getRelated(t, handler, "REXCL", params)); !slices.Contains(owners, excluded) {
		t.Fatalf("related owners without exclude_issue = %v, want %s among them", owners, excluded)
	}
	params.Set("exclude_issue", excluded)
	suggestions := getRelated(t, handler, "REXCL", params)
	for _, item := range suggestions.Related {
		if item.Owner.Key == excluded || item.ID == excluded {
			t.Fatalf("related = %+v, want nothing of %s", suggestions.Related, excluded)
		}
	}
	if !slices.Contains(relatedOwners(suggestions), other) {
		t.Fatalf("related owners = %v, want %s", relatedOwners(suggestions), other)
	}
	expectNothingWritten(t, database, before)

	for _, key := range []string{elsewhere, "REXCL-99", "not-a-key"} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/REXCL/related?"+url.Values{
			"title": {"Zymurgy ledger"}, "exclude_issue": {key},
		}.Encode(), nil, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"EXCLUDE_ISSUE_INPUT"`) {
			t.Fatalf("exclude_issue %s: status=%d body=%s, want 400 EXCLUDE_ISSUE_INPUT", key, response.Code, response.Body.String())
		}
	}
}

// keywords_only skips the meaning search a configured embedder would otherwise answer: the result
// says it searched by keywords only because that was asked for, and the text is never embedded.
func TestRelatedRouteKeywordsOnlySkipsMeaning(t *testing.T) {
	const title, text = "Nightly backups overrun their window", "Nightly snapshots finish late"
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, ""): angledVector(0.9, 2),
		text:                          queryVector(),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	createSuggestionProject(t, handler, "RKEY")
	key := createSearchIssue(t, handler, "RKEY", title, "")
	processAllPending(t, database, embedder)
	refreshTermStats(t, database, "RKEY")
	before := relatedWrites(t, database)

	if meaning := getRelated(t, handler, "RKEY", url.Values{"title": {text}}); meaning.Search != "meaning_and_keywords" {
		t.Fatalf("suggestions without keywords_only = %+v, want the meaning search to answer", meaning)
	}
	embedder.callsMu.Lock()
	calls := embedder.calls
	embedder.callsMu.Unlock()
	suggestions := getRelated(t, handler, "RKEY", url.Values{"title": {text}, "keywords_only": {"true"}})
	if suggestions.Outcome != "found" || suggestions.Search != "keywords_only" || suggestions.SearchNote != "keywords only was asked for" ||
		!slices.Contains(relatedOwners(suggestions), key) {
		t.Fatalf("suggestions = %+v, want %s found by keywords only, as asked", suggestions, key)
	}
	embedder.callsMu.Lock()
	defer embedder.callsMu.Unlock()
	if embedder.calls != calls {
		t.Fatalf("the embedder was called %d times for a keywords-only check, want none", embedder.calls-calls)
	}
	expectNothingWritten(t, database, before)

	bad := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/RKEY/related?title=x&keywords_only=yes", nil, "alice")
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), `"code":"INVALID_QUERY"`) {
		t.Fatalf("keywords_only=yes: status=%d body=%s, want 400 INVALID_QUERY", bad.Code, bad.Body.String())
	}
}

// The bare path, as the deployment's production check GETs it, answers 200 with a check not made
// for a blank text; an unknown project is 404 as on every {key} route.
func TestRelatedRouteBareGetIsNotMadeAndAnUnknownProjectIs404(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "RBARE")
	createSearchIssue(t, handler, "RBARE", "Zymurgy ledger drifts", "")
	before := relatedWrites(t, database)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/RBARE/related", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("bare GET: status=%d body=%s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); !strings.Contains(body, `"related":[]`) {
		t.Fatalf("bare GET body = %s, want related as an empty array", body)
	}
	suggestions := decodeBody[testSuggestions](t, response)
	if suggestions.Outcome != "not_made" || suggestions.Missing != "the text holds no word to search for" || suggestions.Search != "" {
		t.Fatalf("bare GET = %+v, want not made for a blank text", suggestions)
	}
	expectNothingWritten(t, database, before)

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		var body any
		if method == http.MethodPost {
			body = map[string]string{"title": "Zymurgy"}
		}
		missing := dispatchRequest(t, handler, method, "/api/v1/projects/NOPE/related?title=Zymurgy", body, "alice")
		if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"PROJECT_NOT_FOUND"`) {
			t.Fatalf("%s related of an unknown project: status=%d body=%s, want 404 PROJECT_NOT_FOUND", method, missing.Code, missing.Body.String())
		}
	}
}

// seededCheck is one related_checks row a records test writes directly.
type seededCheck struct {
	project, issue, kind, outcome, search, reason string
	keywordMS                                     *int
	totalMS                                       int
	at                                            time.Time
}

// seedRelatedChecks replaces every related_checks row (the fixture's own writes recorded some)
// with checks, an ask check on askID and a decision block check on block b1.
func seedRelatedChecks(t *testing.T, database *store.Store, askID string, checks []seededCheck) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), "delete from related_checks"); err != nil {
		t.Fatalf("clear related_checks: %v", err)
	}
	for _, check := range checks {
		var ask, block any
		switch check.kind {
		case "ask":
			ask = askID
		case "decision_block":
			block = "b1"
		}
		if _, err := database.Pool.Exec(context.Background(), `
			insert into related_checks
				(created_at, project_key, source_kind, source_issue_key, source_ask_id, source_block_id, actor_kind, actor_id,
				 outcome, search, reason, keyword_ms, total_ms)
			values ($1, $2, $3, $4, $5, $6, 'user', 'alice', $7, $8, $9, $10, $11)
		`, check.at, check.project, check.kind, check.issue, ask, block, check.outcome,
			nullableText(check.search), nullableText(check.reason), check.keywordMS, check.totalMS); err != nil {
			t.Fatalf("seed related_checks %+v: %v", check, err)
		}
	}
}

func getRelatedChecks(t *testing.T, handler http.Handler, params url.Values) (model.RelatedChecksReport, string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/related-checks?"+params.Encode(), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET related-checks %v: status=%d body=%s", params, response.Code, response.Body.String())
	}
	return decodeBody[model.RelatedChecksReport](t, response), response.Body.String()
}

func timingOf(count, p50, p95, max, over int) model.RelatedCheckTiming {
	return model.RelatedCheckTiming{Count: count, P50: new(p50), P95: new(p95), Max: new(max), Over300MS: over}
}

func sameTiming(got, want model.RelatedCheckTiming) bool {
	same := func(a, b *int) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	return got.Count == want.Count && got.Over300MS == want.Over300MS && same(got.P50, want.P50) && same(got.P95, want.P95) && same(got.Max, want.Max)
}

// The records route counts the checks in its window by source kind, outcome, search mode and
// reason, and summarizes the keyword and total times; from is inclusive, to exclusive, and the
// project and source kind filters narrow it.
func TestRelatedChecksRouteCountsAndPercentilesOverTheWindow(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "REC")
	createSuggestionProject(t, handler, "RECB")
	issue := createSearchIssue(t, handler, "REC", "Records fixture", "")
	other := createSearchIssue(t, handler, "RECB", "Other records fixture", "")
	askResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{"question": "Which one?"}, "alice")
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("open the ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	askID := decodeBody[testAskWithSuggestions](t, askResponse).ID
	start := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	hour := func(n int) time.Time { return start.Add(time.Duration(n) * time.Hour) }
	seedRelatedChecks(t, database, askID, []seededCheck{
		{"REC", issue, "issue", "found", "meaning_and_keywords", "", new(100), 400, hour(1)},
		{"REC", issue, "issue", "none", "keywords_only", "no_embedder", new(200), 250, hour(2)},
		{"REC", issue, "ask", "found", "keywords_only", "meaning_timeout", new(350), 3100, hour(3)},
		{"REC", issue, "ask", "not_made", "", "timeout", nil, 5000, hour(4)},
		{"REC", issue, "decision_block", "not_made", "", "blank_text", nil, 1, hour(5)},
		{"RECB", other, "issue", "found", "keywords_only", "no_embedder", new(50), 60, hour(1)},
		{"REC", issue, "issue", "found", "keywords_only", "no_embedder", new(400), 500, hour(30)},
	})
	stamp := func(at time.Time) string { return at.Format(time.RFC3339) }

	report, _ := getRelatedChecks(t, handler, url.Values{"project": {"REC"}, "from": {stamp(start)}, "to": {stamp(hour(24))}})
	if report.Project == nil || *report.Project != "REC" || report.SourceKind != nil ||
		report.From == nil || !report.From.Equal(start) || report.To == nil || !report.To.Equal(hour(24)) {
		t.Fatalf("filters echoed = project %v source_kind %v from %v to %v, want REC, null, the window", report.Project, report.SourceKind, report.From, report.To)
	}
	want := map[string]map[string]int{
		"source kind": {"issue": 2, "ask": 2, "decision_block": 1},
		"outcome":     {"found": 2, "none": 1, "not_made": 2},
		"search":      {"meaning_and_keywords": 1, "keywords_only": 2, "not_made": 2},
		"reason":      {"no_embedder": 1, "meaning_timeout": 1, "timeout": 1, "blank_text": 1},
	}
	got := map[string]map[string]int{
		"source kind": report.BySourceKind, "outcome": report.ByOutcome, "search": report.BySearch, "reason": report.ByReason,
	}
	if report.Total != 5 || report.Completed != 3 || report.SkippedForTime != 1 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("report = total %d completed %d skipped %d counts %v, want 5, 3, 1, %v", report.Total, report.Completed, report.SkippedForTime, got, want)
	}
	// Discrete percentiles: keyword times 100, 200, 350 give p95 350 (an interpolated one would
	// be 335); total times 1, 250, 400, 3100, 5000 give p50 400 and p95 5000.
	if !sameTiming(report.KeywordMS, timingOf(3, 200, 350, 350, 1)) || !sameTiming(report.TotalMS, timingOf(5, 400, 5000, 5000, 3)) {
		t.Fatalf("timings = keyword %+v total %+v, want keyword {3 200 350 350 1} and total {5 400 5000 5000 3}", report.KeywordMS, report.TotalMS)
	}

	for _, filter := range []struct {
		name   string
		params url.Values
		total  int
	}{
		{"every project, every time", url.Values{}, 7},
		{"to is exclusive", url.Values{"project": {"REC"}, "from": {stamp(start)}, "to": {stamp(hour(3))}}, 2},
		{"from is inclusive, every project", url.Values{"from": {stamp(hour(1))}, "to": {stamp(hour(2))}}, 2},
		{"from alone", url.Values{"from": {stamp(hour(24))}}, 1},
		{"one other project", url.Values{"project": {"RECB"}}, 1},
	} {
		if report, _ := getRelatedChecks(t, handler, filter.params); report.Total != filter.total {
			t.Errorf("%s (%v): total %d, want %d", filter.name, filter.params, report.Total, filter.total)
		}
	}

	asks, _ := getRelatedChecks(t, handler, url.Values{"project": {"REC"}, "source_kind": {"ask"}, "to": {stamp(hour(24))}})
	if asks.SourceKind == nil || *asks.SourceKind != "ask" || asks.Total != 2 || asks.Completed != 1 || asks.SkippedForTime != 1 ||
		!sameTiming(asks.KeywordMS, timingOf(1, 350, 350, 350, 1)) || !sameTiming(asks.TotalMS, timingOf(2, 3100, 5000, 5000, 2)) {
		t.Fatalf("ask checks = %+v, want the two ask checks", asks)
	}

	none, body := getRelatedChecks(t, handler, url.Values{"project": {"NOPE"}})
	if none.Total != 0 || !strings.Contains(body, `"by_outcome":{}`) || !strings.Contains(body, `"keyword_ms":{"count":0,"p50":null,"p95":null,"max":null,"over_300ms":0}`) {
		t.Fatalf("a project with no checks answers %s, want zero counts, empty maps and null times", body)
	}
}

// A source kind the table does not hold, or a bound that is not a time or ends the window before
// it starts, is refused.
func TestRelatedChecksRouteRefusesABadFilter(t *testing.T) {
	handler := newTestHandler(t)
	for _, query := range []string{
		"source_kind=comment",
		"from=yesterday",
		"to=2026-01-01",
		"from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z",
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/related-checks?"+query, nil, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_QUERY"`) {
			t.Errorf("related-checks?%s: status=%d body=%s, want 400 INVALID_QUERY", query, response.Code, response.Body.String())
		}
	}
}
