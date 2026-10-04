package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type testSuggestionOwner struct {
	Kind   string `json:"kind"`
	Key    string `json:"key"`
	Status string `json:"status"`
	Title  string `json:"title"`
}

type testSuggestionItem struct {
	Kind       string              `json:"kind"`
	ID         string              `json:"id"`
	Href       string              `json:"href"`
	Owner      testSuggestionOwner `json:"owner"`
	AnsweredBy string              `json:"answered_by"`
	AnsweredAt *string             `json:"answered_at"`
}

type testSuggestions struct {
	Related  []testSuggestionItem `json:"related"`
	Decision *testSuggestionItem  `json:"decision"`
	Missing  string               `json:"missing"`
}

type testAdvisedSuggestions struct {
	Suggestions *testSuggestions `json:"suggestions"`
}

type testIssueWithSuggestions struct {
	Key    string                  `json:"key"`
	Advice *testAdvisedSuggestions `json:"advice"`
}

type testAskWithSuggestions struct {
	ID     string                  `json:"id"`
	Advice *testAdvisedSuggestions `json:"advice"`
}

func createSuggestionProject(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": key, "name": key + " project",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create project %q: status=%d body=%s", key, response.Code, response.Body.String())
	}
}

// TestCreateIssueSuggestsSimilarOpenIssue is LEGION-550's first acceptance line: filing an issue
// whose title and body restate an open issue returns that issue among the three suggestions,
// with its status and a link.
func TestCreateIssueSuggestsSimilarOpenIssue(t *testing.T) {
	handler := newTestHandler(t)
	createSuggestionProject(t, handler, "SUGG")

	existing := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "SUGG",
		"title":   "Archive search drops rows past offset fifty",
		"spec":    "The archive search view loses rows once the offset passes fifty because the cursor resets to the start.",
	}, "alice")
	if existing.Code != http.StatusCreated {
		t.Fatalf("create existing issue: status=%d body=%s", existing.Code, existing.Body.String())
	}
	existingIssue := decodeBody[testIssueWithSuggestions](t, existing)

	// force bypasses the create-time title near-duplicate gate (duplicates.go), which this test
	// does not exercise: LEGION-550's suggestions are a separate, non-blocking signal that
	// coexists with it.
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "SUGG",
		"title":   "Rows past offset fifty disappear from the archive search",
		"spec":    "A user reports the archive search view loses rows once the offset passes fifty.",
		"force":   true,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create new issue: status=%d body=%s", response.Code, response.Body.String())
	}
	created := decodeBody[testIssueWithSuggestions](t, response)
	if created.Advice == nil || created.Advice.Suggestions == nil {
		t.Fatalf("advice.suggestions missing from %+v", created)
	}
	suggestions := created.Advice.Suggestions
	found := false
	for _, item := range suggestions.Related {
		if item.Kind == "issue" && item.ID == existingIssue.Key {
			found = true
			if item.Owner.Status != "triage" || item.Owner.Key != existingIssue.Key || item.Href == "" {
				t.Fatalf("suggested item = %+v, want status, key and href for %s", item, existingIssue.Key)
			}
		}
		if item.ID == created.Key {
			t.Fatalf("suggestions include the issue that was just filed: %+v", suggestions.Related)
		}
	}
	if !found {
		t.Fatalf("related suggestions = %+v, want %s among them", suggestions.Related, existingIssue.Key)
	}
}

// TestCreateAskSuggestsAnsweredPastDecision is LEGION-550's second acceptance line: filing an
// ask that a past answered ask settles returns that decision with who answered and when.
func TestCreateAskSuggestsAnsweredPastDecision(t *testing.T) {
	handler := newTestHandler(t)
	createSuggestionProject(t, handler, "DEC")

	issue := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "DEC", "title": "Choose a transport for the billing service",
	}, "alice")
	if issue.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issue.Code, issue.Body.String())
	}
	issueKey := decodeBody[testIssueWithSuggestions](t, issue).Key

	opened := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": "Should the billing service use REST or GraphQL for its public API?",
	}, "alice")
	if opened.Code != http.StatusCreated {
		t.Fatalf("open ask: status=%d body=%s", opened.Code, opened.Body.String())
	}
	askID := decodeBody[testAskWithSuggestions](t, opened).ID

	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"text": "REST: the rest of the platform already standardizes on it.",
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", answered.Code, answered.Body.String())
	}

	secondIssue := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "DEC", "title": "Pick an API transport for the new notifications service",
	}, "alice")
	if secondIssue.Code != http.StatusCreated {
		t.Fatalf("create second issue: status=%d body=%s", secondIssue.Code, secondIssue.Body.String())
	}
	secondIssueKey := decodeBody[testIssueWithSuggestions](t, secondIssue).Key

	newAsk := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+secondIssueKey+"/asks", map[string]any{
		"question": "Should the notifications service use REST or GraphQL for its public API?",
	}, "alice")
	if newAsk.Code != http.StatusCreated {
		t.Fatalf("open second ask: status=%d body=%s", newAsk.Code, newAsk.Body.String())
	}
	created := decodeBody[testAskWithSuggestions](t, newAsk)
	if created.Advice == nil || created.Advice.Suggestions == nil || created.Advice.Suggestions.Decision == nil {
		t.Fatalf("advice.suggestions.decision missing from %+v", created)
	}
	decision := created.Advice.Suggestions.Decision
	if decision.Kind != "ask" || decision.ID != askID {
		t.Fatalf("decision = %+v, want the answered ask %s", decision, askID)
	}
	if decision.AnsweredBy != "alice" || decision.AnsweredAt == nil || *decision.AnsweredAt == "" {
		t.Fatalf("decision = %+v, want who answered and when", decision)
	}
}

// TestComputeSuggestionsReportsMissingWhenSearchTimesOut is LEGION-550's latency acceptance
// line: the write succeeds and returns as fast as today when search is slow or down, and the
// suggestions say they are missing and why. computeSuggestions runs after the write already
// committed, so this calls it directly with an already-expired context rather than contriving a
// genuinely slow query.
func TestComputeSuggestionsReportsMissingWhenSearchTimesOut(t *testing.T) {
	_, _, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)

	expired, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-expired.Done()

	suggestions := srv.computeSuggestions(expired, "ANY", "a title that would otherwise search",
		suggestionSource{kind: "issue", issueKey: "ANY-1"})
	if suggestions == nil || suggestions.Missing == "" {
		t.Fatalf("suggestions = %+v, want Missing set when the search context is already done", suggestions)
	}
	if len(suggestions.Related) != 0 || suggestions.Decision != nil {
		t.Fatalf("suggestions = %+v, want nothing related or decided when search could not run", suggestions)
	}
}

// TestSuggestionsDecisionIsNotReorderedByOwnerStatus: Decision picks the best-ranked answered ask
// in the search's own order, even when a worse-matching answered ask whose issue is still open
// would sort ahead of it in Related (closedOwner's open-before-done rule applies only there).
func TestSuggestionsDecisionIsNotReorderedByOwnerStatus(t *testing.T) {
	handler := newTestHandler(t)
	createSuggestionProject(t, handler, "DECORD")

	weakOpen := createSuggestionIssue(t, handler, map[string]any{
		"project": "DECORD", "title": "Choose a transport for the notifications service",
	})
	weakAsk := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+weakOpen+"/asks", map[string]any{
		"question": "Should the notifications service use REST for its API?",
	}, "alice")
	weakAskID := decodeBody[testAskWithSuggestions](t, weakAsk).ID
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+weakAskID+"/answer", map[string]any{
		"text": "REST.",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("answer weak ask: status=%d body=%s", response.Code, response.Body.String())
	}

	strongDone := createSuggestionIssue(t, handler, map[string]any{
		"project": "DECORD", "title": "Billing service transport",
	})
	strongAsk := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+strongDone+"/asks", map[string]any{
		"question": "Should the billing service public API use REST or GraphQL?",
	}, "alice")
	strongAskID := decodeBody[testAskWithSuggestions](t, strongAsk).ID
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+strongAskID+"/answer", map[string]any{
		"text": "REST.",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("answer strong ask: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+strongDone, map[string]any{"status": "done"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("close strong issue: status=%d body=%s", response.Code, response.Body.String())
	}

	target := createSuggestionIssue(t, handler, map[string]any{
		"project": "DECORD", "title": "Pick an API transport for the checkout service",
	})
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+target+"/asks", map[string]any{
		"question": "Should the billing service public API use REST or GraphQL?",
	}, "alice")
	created := decodeBody[testAskWithSuggestions](t, response)
	if created.Advice == nil || created.Advice.Suggestions == nil || created.Advice.Suggestions.Decision == nil {
		t.Fatalf("advice.suggestions.decision missing: %s", response.Body.String())
	}
	if decision := created.Advice.Suggestions.Decision; decision.ID != strongAskID {
		t.Fatalf("decision = %+v, want the better-matching ask %s even though its issue %s is done", decision, strongAskID, strongDone)
	}
}

// TestComputeAndPersistSuggestionsRespectsDeadlineUnderLock: a reviewer measured the decision
// lookup alone blocking ~1.2s behind a table lock when it ran on the request's unbounded
// context. computeAndPersistSuggestions now derives one deadline that covers the search, the
// decision lookup, and persistence together, so the whole call returns at or near
// writeSuggestionTimeout even while `asks` is locked, never near the lock's own hold time.
func TestComputeAndPersistSuggestionsRespectsDeadlineUnderLock(t *testing.T) {
	handler, _, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, "DEADLINE")
	issue := createSuggestionIssue(t, handler, map[string]any{
		"project": "DEADLINE", "title": "Should the service use REST or GraphQL?",
	})
	asked := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{
		"question": "Should the service use REST or GraphQL for its public API?",
	}, "alice")
	askID := decodeBody[testAskWithSuggestions](t, asked).ID
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"text": "REST.",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", response.Code, response.Body.String())
	}

	lockTx, err := srv.deps.Store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	if _, err := lockTx.Exec(context.Background(), "lock table asks in access exclusive mode"); err != nil {
		t.Fatalf("lock asks: %v", err)
	}

	source := suggestionSource{kind: "ask", issueKey: issue, askID: "locked-probe", actor: model.Actor{Kind: "user", ID: "alice"}}
	started := time.Now()
	suggestions := srv.computeAndPersistSuggestions(context.Background(), "DEADLINE", "Should the service use REST or GraphQL for its public API?", source)
	elapsed := time.Since(started)

	const bound = writeSuggestionTimeout + 500*time.Millisecond // headroom for the search leg before the lock is hit
	if elapsed > bound {
		t.Fatalf("computeAndPersistSuggestions took %s with asks locked, want at or near %s, well under the lock's own hold time", elapsed, writeSuggestionTimeout)
	}
	if suggestions == nil {
		t.Fatalf("suggestions = nil, want a result (Missing or otherwise) even when the decision lookup is blocked")
	}
}

func createSuggestionIssue(t *testing.T, handler http.Handler, body map[string]any) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue %v: status=%d body=%s", body["title"], response.Code, response.Body.String())
	}
	return decodeBody[testIssueWithSuggestions](t, response).Key
}

func relatedIndex(related []testSuggestionItem, issueKey string) int {
	for i, item := range related {
		if item.Kind == "issue" && item.ID == issueKey {
			return i
		}
	}
	return -1
}

// TestCreateIssueSuggestsTheOpenSurvivorAboveAClosedDuplicate is the live run's case: an issue
// already closed as a duplicate shares the new issue's exact title, so keyword search ranks it
// above the open issue it was closed into. The open issue is the one to act on, so it comes first.
func TestCreateIssueSuggestsTheOpenSurvivorAboveAClosedDuplicate(t *testing.T) {
	handler := newTestHandler(t)
	createSuggestionProject(t, handler, "SURV")
	survivor := createSuggestionIssue(t, handler, map[string]any{
		"project": "SURV", "title": "CSV export hangs for billing accounts with many invoices",
	})
	const restated = "Billing account page export to CSV never completes above fifty thousand invoices"
	closed := createSuggestionIssue(t, handler, map[string]any{"project": "SURV", "title": restated, "force": true})
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+closed, map[string]any{"status": "done"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("close %s: status=%d body=%s", closed, response.Code, response.Body.String())
	}

	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "SURV", "title": restated, "force": true,
	}, "alice")
	created := decodeBody[testIssueWithSuggestions](t, response)
	if created.Advice == nil || created.Advice.Suggestions == nil {
		t.Fatalf("advice.suggestions missing: %s", response.Body.String())
	}
	related := created.Advice.Suggestions.Related
	open, done := relatedIndex(related, survivor), relatedIndex(related, closed)
	if open < 0 || (done >= 0 && done < open) {
		t.Fatalf("related = %+v, want the open %s ahead of the closed %s", related, survivor, closed)
	}
}

// TestCreateAskExcludesItsOwnIssue: an ask's own issue, and everything it owns, is never among the
// ask's suggestions, so a reply on that issue can never be counted as acting on one.
func TestCreateAskExcludesItsOwnIssue(t *testing.T) {
	handler := newTestHandler(t)
	createSuggestionProject(t, handler, "OWN")
	own := createSuggestionIssue(t, handler, map[string]any{
		"project": "OWN", "title": "Choose a transport for the billing service public API",
		"spec": "The billing service public API needs a transport: REST or GraphQL.",
	})
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+own+"/asks", map[string]any{
		"question": "Should the billing service public API use REST or GraphQL?",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("open ask: status=%d body=%s", response.Code, response.Body.String())
	}
	created := decodeBody[testAskWithSuggestions](t, response)
	if created.Advice == nil || created.Advice.Suggestions == nil {
		t.Fatalf("advice.suggestions missing: %s", response.Body.String())
	}
	for _, item := range created.Advice.Suggestions.Related {
		if item.ID == own || item.Owner.Key == own {
			t.Fatalf("related = %+v, want nothing owned by the ask's own issue %s", created.Advice.Suggestions.Related, own)
		}
	}
}

// TestCreateAskOnProjectDocumentSuggests: an ask on an unlinked project document gets suggestions
// too, searched within that document's project.
func TestCreateAskOnProjectDocumentSuggests(t *testing.T) {
	handler := newTestHandler(t)
	createSuggestionProject(t, handler, "PDOC")
	match := createSuggestionIssue(t, handler, map[string]any{
		"project": "PDOC", "title": "Pick a transport for the billing service public API",
	})
	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/PDOC/artifacts", map[string]any{
		"name": "roadmap.md", "content": "# Roadmap\n\nPlanning notes.\n",
	}, "alice")
	if uploaded.Code != http.StatusCreated && uploaded.Code != http.StatusOK {
		t.Fatalf("upload project document: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	document := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, uploaded)
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.Artifact.ID+"/asks", map[string]any{
		"question": "Should the billing service public API use REST or GraphQL?",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("open document ask: status=%d body=%s", response.Code, response.Body.String())
	}
	created := decodeBody[testAskWithSuggestions](t, response)
	if created.Advice == nil || created.Advice.Suggestions == nil {
		t.Fatalf("advice.suggestions missing on a project-document ask: %s", response.Body.String())
	}
	if relatedIndex(created.Advice.Suggestions.Related, match) < 0 {
		t.Fatalf("related = %+v, want %s from the document's project", created.Advice.Suggestions.Related, match)
	}
}

func seedWriteSuggestion(
	t *testing.T, srv *server, sourceKind, sourceIssueKey, sourceAskID, actorID, suggestedKind, suggestedID, suggestedIssueKey string,
) {
	t.Helper()
	var askID, issueKey any
	if sourceAskID != "" {
		askID = sourceAskID
	}
	if suggestedIssueKey != "" {
		issueKey = suggestedIssueKey
	}
	if _, err := srv.deps.Store.Pool.Exec(context.Background(), `
		insert into write_suggestions
			(source_kind, source_issue_key, source_ask_id, actor_kind, actor_id, role, rank,
			 suggested_kind, suggested_id, suggested_issue_key)
		values ($1, $2, $3, 'user', $4, 'related', 0, $5, $6, $7)
	`, sourceKind, sourceIssueKey, askID, actorID, suggestedKind, suggestedID, issueKey); err != nil {
		t.Fatalf("seed write_suggestions: %v", err)
	}
}

type suggestionOutcomeRow struct {
	Outcome string
	Detail  string
}

func readSuggestionOutcome(t *testing.T, srv *server, sourceIssueKey, suggestedID string) suggestionOutcomeRow {
	t.Helper()
	var row suggestionOutcomeRow
	if err := srv.deps.Store.Pool.QueryRow(context.Background(), `
		select outcome, coalesce(outcome_detail, '') from write_suggestions
		 where source_issue_key = $1 and suggested_id = $2
	`, sourceIssueKey, suggestedID).Scan(&row.Outcome, &row.Detail); err != nil {
		t.Fatalf("read write_suggestions outcome: %v", err)
	}
	return row
}

// TestSuggestionOutcomeSweepMarksActedOnWhenSourceCitesTarget covers "closed its own as
// duplicate of it": a comment on the source issue citing the suggested issue creates a
// reference-graph edge, which the sweep reads as acted_on.
func TestSuggestionOutcomeSweepMarksActedOnWhenSourceCitesTarget(t *testing.T) {
	handler, _, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, "OUT1")

	target := decodeBody[testIssueWithSuggestions](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "OUT1", "title": "Target issue",
	}, "alice"))
	source := decodeBody[testIssueWithSuggestions](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "OUT1", "title": "Source issue", "force": true,
	}, "alice"))

	seedWriteSuggestion(t, srv, "issue", source.Key, "", "alice", "issue", target.Key, target.Key)
	time.Sleep(10 * time.Millisecond) // the citation must postdate the suggestion row

	closing := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+source.Key+"/messages", map[string]any{
		"body": "Duplicate of dispatch://" + target.Key,
	}, "alice")
	if closing.Code != http.StatusCreated {
		t.Fatalf("post closing note: status=%d body=%s", closing.Code, closing.Body.String())
	}

	if err := sweepSuggestionOutcomes(context.Background(), srv.deps.Store.Pool); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if row := readSuggestionOutcome(t, srv, source.Key, target.Key); row.Outcome != "acted_on" {
		t.Fatalf("outcome = %+v, want acted_on", row)
	}
}

// TestSuggestionOutcomeSweepMarksActedOnWhenTargetUpdatedDirectly covers "update the existing
// issue instead of filing another" done without citing the new issue back.
func TestSuggestionOutcomeSweepMarksActedOnWhenTargetUpdatedDirectly(t *testing.T) {
	handler, _, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, "OUT2")

	target := decodeBody[testIssueWithSuggestions](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "OUT2", "title": "Target issue",
	}, "alice"))
	source := decodeBody[testIssueWithSuggestions](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "OUT2", "title": "Source issue", "force": true,
	}, "alice"))

	seedWriteSuggestion(t, srv, "issue", source.Key, "", "alice", "issue", target.Key, target.Key)
	time.Sleep(10 * time.Millisecond)

	updated := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+target.Key+"/messages", map[string]any{
		"body": "Adding the new detail here instead.",
	}, "alice")
	if updated.Code != http.StatusCreated {
		t.Fatalf("update target issue: status=%d body=%s", updated.Code, updated.Body.String())
	}

	if err := sweepSuggestionOutcomes(context.Background(), srv.deps.Store.Pool); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if row := readSuggestionOutcome(t, srv, source.Key, target.Key); row.Outcome != "acted_on" {
		t.Fatalf("outcome = %+v, want acted_on", row)
	}
}

// TestSuggestionOutcomeSweepMarksOverriddenWhenSourceContinuesWithoutCitingTarget covers an
// agent that saw the suggestion and kept working on what it filed instead.
func TestSuggestionOutcomeSweepMarksOverriddenWhenSourceContinuesWithoutCitingTarget(t *testing.T) {
	handler, _, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, "OUT3")

	target := decodeBody[testIssueWithSuggestions](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "OUT3", "title": "Target issue",
	}, "alice"))
	source := decodeBody[testIssueWithSuggestions](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "OUT3", "title": "Source issue", "force": true,
	}, "alice"))

	seedWriteSuggestion(t, srv, "issue", source.Key, "", "alice", "issue", target.Key, target.Key)
	time.Sleep(10 * time.Millisecond)

	continued := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+source.Key+"/messages", map[string]any{
		"body": "This is in fact a separate problem; continuing here.",
	}, "alice")
	if continued.Code != http.StatusCreated {
		t.Fatalf("continue source issue: status=%d body=%s", continued.Code, continued.Body.String())
	}

	if err := sweepSuggestionOutcomes(context.Background(), srv.deps.Store.Pool); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if row := readSuggestionOutcome(t, srv, source.Key, target.Key); row.Outcome != "overridden" {
		t.Fatalf("outcome = %+v, want overridden", row)
	}
}

// TestSuggestionOutcomeSweepLeavesSuggestionIgnoredWithNoFurtherActivity is the default: nothing
// refuses the write and nothing is counted until some signal arrives.
func TestSuggestionOutcomeSweepLeavesSuggestionIgnoredWithNoFurtherActivity(t *testing.T) {
	handler, _, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, "OUT4")

	target := decodeBody[testIssueWithSuggestions](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "OUT4", "title": "Target issue",
	}, "alice"))
	source := decodeBody[testIssueWithSuggestions](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "OUT4", "title": "Source issue", "force": true,
	}, "alice"))

	seedWriteSuggestion(t, srv, "issue", source.Key, "", "alice", "issue", target.Key, target.Key)

	if err := sweepSuggestionOutcomes(context.Background(), srv.deps.Store.Pool); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if row := readSuggestionOutcome(t, srv, source.Key, target.Key); row.Outcome != "ignored" {
		t.Fatalf("outcome = %+v, want ignored", row)
	}
}
