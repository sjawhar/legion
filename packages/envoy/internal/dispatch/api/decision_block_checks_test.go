package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// testDecisionBlockCheck is one entry of an edit's advice.decision_block_checks.
type testDecisionBlockCheck struct {
	BlockID     string             `json:"block_id"`
	Question    string             `json:"question"`
	Suggestions *model.Suggestions `json:"suggestions"`
}

// decisionBlockChecksOf reads an edit response's advice.decision_block_checks, and whether the
// advice holds the field at all.
func decisionBlockChecksOf(t *testing.T, response *httptest.ResponseRecorder) ([]testDecisionBlockCheck, bool) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("edit: status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Advice map[string]json.RawMessage `json:"advice"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode edit response: %v", err)
	}
	raw, present := envelope.Advice["decision_block_checks"]
	if !present {
		return nil, false
	}
	var checks []testDecisionBlockCheck
	if err := json.Unmarshal(raw, &checks); err != nil {
		t.Fatalf("decode decision_block_checks %s: %v", raw, err)
	}
	return checks, true
}

// decisionBlockRow is one related_checks row of a decision block source.
type decisionBlockRow struct {
	IssueKey   *string
	ArtifactID *string
	BlockID    string
	Outcome    string
	Reason     *string
	ActorID    string
}

func readDecisionBlockRows(t *testing.T, database *store.Store) []decisionBlockRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select source_issue_key, source_artifact_id::text, source_block_id, outcome, reason, actor_id
		  from related_checks where source_kind = 'decision_block' order by created_at, source_block_id`)
	if err != nil {
		t.Fatalf("read decision block checks: %v", err)
	}
	defer rows.Close()
	var found []decisionBlockRow
	for rows.Next() {
		var row decisionBlockRow
		if err := rows.Scan(&row.IssueKey, &row.ArtifactID, &row.BlockID, &row.Outcome, &row.Reason, &row.ActorID); err != nil {
			t.Fatalf("scan decision block check: %v", err)
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read decision block checks: %v", err)
	}
	return found
}

// suggestedOwnerKeys is the issue each related item of suggestions stands for.
func suggestedOwnerKeys(suggestions *model.Suggestions) []string {
	if suggestions == nil {
		return nil
	}
	return relatedOwnerKeys(suggestions.Related)
}

func editDocument(t *testing.T, handler http.Handler, artifactID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", body, "alice")
}

func insertAtEnd(markdown string) map[string]any {
	return map[string]any{"ops": []map[string]any{{"op": "insert", "markdown": markdown, "after": "end"}}}
}

// A decision block an edit adds is checked like an ask: its question finds the issue that holds
// its rare words, the answer names the block, and the check is recorded against the document's
// issue. An edit after it that adds no block - plain text, a reworded block - answers no check.
func TestDecisionBlockCheckAnswersTheEditsNewBlock(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "DBLK")
	target := createSearchIssue(t, handler, "DBLK", "Dashboard ledger for zymurgy batches", "")
	spec := "# Spec\n\nPlanning notes.\n"
	issue := createAdviceIssue(t, handler, "DBLK", "Quarterly planning", &spec)
	refreshTermStats(t, database, "DBLK")

	const question = "Should zymurgy ledger totals round down?"
	checks, present := decisionBlockChecksOf(t, editDocument(t, handler, issue.PrimaryArtifactID,
		insertAtEnd(":::ask{#rounding}\n"+question+"\n:::")))
	if !present || len(checks) != 1 {
		t.Fatalf("decision_block_checks = %+v (present %t), want one", checks, present)
	}
	check := checks[0]
	if check.BlockID != "rounding" || check.Question != question {
		t.Fatalf("check = %+v, want the block rounding and its question", check)
	}
	if check.Suggestions == nil || check.Suggestions.Outcome != "found" || !slices.Contains(suggestedOwnerKeys(check.Suggestions), target) {
		t.Fatalf("check suggestions = %+v, want %s found", check.Suggestions, target)
	}
	if slices.Contains(suggestedOwnerKeys(check.Suggestions), issue.Key) {
		t.Fatalf("check suggestions = %+v, want the document's own issue %s left out", check.Suggestions, issue.Key)
	}
	rows := readDecisionBlockRows(t, database)
	if len(rows) != 1 || rows[0].BlockID != "rounding" || rows[0].IssueKey == nil || *rows[0].IssueKey != issue.Key ||
		rows[0].ArtifactID != nil || rows[0].Outcome != "found" || rows[0].ActorID != "alice" {
		t.Fatalf("decision block rows = %+v, want one found check of rounding on %s", rows, issue.Key)
	}

	for name, body := range map[string]map[string]any{
		"plain text": insertAtEnd("A plain revision."),
		"a reworded block": {"ops": []map[string]any{{
			"op": "replace", "find": question, "with": "Should zymurgy ledger totals round up?",
		}}},
	} {
		checks, present := decisionBlockChecksOf(t, editDocument(t, handler, issue.PrimaryArtifactID, body))
		if present {
			t.Fatalf("%s: decision_block_checks = %+v, want the field absent", name, checks)
		}
	}
	if rows := readDecisionBlockRows(t, database); len(rows) != 1 {
		t.Fatalf("decision block rows = %+v, want only the first edit's check", rows)
	}
}

// An edit adding six decision blocks checks the first five, in document order, and answers the
// sixth that its check was not made, recording that too.
func TestDecisionBlockChecksPastFiveAreNotMade(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "SIX")
	createSearchIssue(t, handler, "SIX", "Dashboard ledger for zymurgy batches", "")
	spec := "# Spec\n\nPlanning notes.\n"
	issue := createAdviceIssue(t, handler, "SIX", "Six decisions", &spec)
	refreshTermStats(t, database, "SIX")

	var markdown []string
	for index := range 6 {
		markdown = append(markdown, fmt.Sprintf(":::ask{#choice%d}\nWhich zymurgy ledger layout %d?\n:::", index, index))
	}
	checks, present := decisionBlockChecksOf(t, editDocument(t, handler, issue.PrimaryArtifactID, insertAtEnd(strings.Join(markdown, "\n\n"))))
	if !present || len(checks) != 6 {
		t.Fatalf("decision_block_checks = %+v (present %t), want six", checks, present)
	}
	for index, check := range checks {
		if check.BlockID != fmt.Sprintf("choice%d", index) || check.Question != fmt.Sprintf("Which zymurgy ledger layout %d?", index) {
			t.Fatalf("check %d = %+v, want block choice%d in document order", index, check, index)
		}
		if check.Suggestions == nil {
			t.Fatalf("check %d has no suggestions", index)
		}
		if index < 5 && check.Suggestions.Outcome == "not_made" {
			t.Fatalf("check %d = %+v, want it made", index, check.Suggestions)
		}
	}
	sixth := checks[5].Suggestions
	if sixth.Outcome != "not_made" || sixth.Missing != "more than five new decision blocks in one edit" ||
		sixth.Related == nil || len(sixth.Related) != 0 {
		t.Fatalf("sixth check = %+v, want not made for too many blocks", sixth)
	}
	rows := readDecisionBlockRows(t, database)
	if len(rows) != 6 {
		t.Fatalf("decision block rows = %+v, want six", rows)
	}
	var tooMany []string
	for _, row := range rows {
		if row.Reason != nil && *row.Reason == "too_many_blocks" {
			if row.Outcome != "not_made" {
				t.Fatalf("too_many_blocks row = %+v, want not made", row)
			}
			tooMany = append(tooMany, row.BlockID)
		}
	}
	if !slices.Equal(tooMany, []string{"choice5"}) {
		t.Fatalf("too_many_blocks rows = %v, want choice5 alone", tooMany)
	}
}

// A conditional edit's gate is released at its commit: a second conditional edit of the same
// document completes while the first is still waiting on its decision block's check, here held
// behind a lock on the term statistics every check reads.
func TestDecisionBlockCheckDoesNotHoldTheConditionalEditGate(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "GATE")
	spec := "# Spec\n\nPlanning notes.\n"
	issue := createAdviceIssue(t, handler, "GATE", "Gated edits", &spec)

	lockTx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), "lock table search_term_stats in access exclusive mode"); err != nil {
		t.Fatalf("lock search_term_stats: %v", err)
	}

	const question = "Which zymurgy deployment window?"
	first := insertAtEnd(":::ask{#window}\n" + question + "\n:::")
	first["precondition"] = map[string]string{"document": readDocumentPrecondition(t, handler, issue.PrimaryArtifactID).Token}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- editDocument(t, handler, issue.PrimaryArtifactID, first) }()

	// The first edit has committed once the document holds its block, and its check is running
	// once something waits on the lock. (Another waiter, the term statistics refresh the issue's
	// own check started, may be the one seen: the commit is what the second edit needs.) The second
	// edit is guarded by the paragraph it rewrites, which the first block's settlement leaves alone.
	deadline := time.Now().Add(10 * time.Second)
	var second map[string]any
	for {
		var waiting int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from pg_locks where relation = 'search_term_stats'::regclass and not granted
		`).Scan(&waiting); err != nil {
			t.Fatalf("read lock waits: %v", err)
		}
		if waiting > 0 && strings.Contains(readDocumentPrecondition(t, handler, issue.PrimaryArtifactID).Markdown, question) {
			paragraph := readDocumentPreconditionBlocks(t, handler, issue.PrimaryArtifactID)[1]
			second = map[string]any{
				"ops":          []map[string]string{{"op": "replace", "find": "Planning notes.", "with": "Planning notes, revised."}},
				"precondition": map[string]any{"blocks": []map[string]string{{"id": paragraph.ID, "token": paragraph.Token}}},
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first edit never committed with a check waiting on the term statistics")
		}
		time.Sleep(10 * time.Millisecond)
	}

	secondResponse := editDocument(t, handler, issue.PrimaryArtifactID, second)
	select {
	case response := <-firstDone:
		t.Fatalf("the first edit answered (status %d) before the second, which waited on its gate", response.Code)
	default:
	}
	if secondResponse.Code != http.StatusOK {
		t.Fatalf("second edit: status=%d body=%s", secondResponse.Code, secondResponse.Body.String())
	}

	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	checks, present := decisionBlockChecksOf(t, <-firstDone)
	if !present || len(checks) != 1 || checks[0].BlockID != "window" {
		t.Fatalf("first edit's decision_block_checks = %+v, want the block window", checks)
	}
}

// A decision block on a project document, which has no issue, is checked in the document's
// project with the document itself left out, though it holds the block's words, and is recorded
// against that document.
func TestDecisionBlockCheckOnAProjectDocumentLeavesTheDocumentOut(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "PDBL")
	target := createSearchIssue(t, handler, "PDBL", "Dashboard ledger for zymurgy batches", "")
	upload := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/PDBL/artifacts", map[string]string{
		"name": "zymurgy-notes.md", "content": "# Zymurgy ledger notes\n\nThe zymurgy ledger totals drift.\n",
	}, "alice")
	if upload.Code != http.StatusCreated {
		t.Fatalf("project upload: status=%d body=%s", upload.Code, upload.Body.String())
	}
	document := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, upload).Artifact.ID
	refreshTermStats(t, database, "PDBL")

	checks, present := decisionBlockChecksOf(t, editDocument(t, handler, document,
		insertAtEnd(":::ask{#rounding}\nShould zymurgy ledger totals round down?\n:::")))
	if !present || len(checks) != 1 || checks[0].BlockID != "rounding" || checks[0].Suggestions == nil {
		t.Fatalf("decision_block_checks = %+v (present %t), want the block rounding checked", checks, present)
	}
	suggestions := checks[0].Suggestions
	if suggestions.Outcome != "found" || !slices.Contains(suggestedOwnerKeys(suggestions), target) {
		t.Fatalf("suggestions = %+v, want %s found", suggestions, target)
	}
	for _, item := range suggestions.Related {
		if item.ID == document || item.Owner.ArtifactID == document {
			t.Fatalf("suggestions = %+v, want the document %s itself left out", suggestions, document)
		}
	}
	rows := readDecisionBlockRows(t, database)
	if len(rows) != 1 || rows[0].IssueKey != nil || rows[0].ArtifactID == nil || *rows[0].ArtifactID != document ||
		rows[0].BlockID != "rounding" || rows[0].Outcome != "found" {
		t.Fatalf("decision block rows = %+v, want one found check of rounding on the document %s", rows, document)
	}
}

// routeWeightOf is the admission weight the table gives one route: the most request-pool
// connections a request of it may hold at once (apiRoute.Weight, a weight below one being one).
func routeWeightOf(t *testing.T, method, pattern string) int32 {
	t.Helper()
	for _, route := range (&server{}).routes() {
		if route.Method == method && route.Pattern == pattern {
			return int32(max(route.Weight, 1))
		}
	}
	t.Fatalf("no route %s %s in the table", method, pattern)
	return 0
}

// An edit holds no more request-pool connections than its route's admission weight, whatever its
// decision blocks: its checks run one after another, so only one check's reads are in flight,
// where five at once asked for a connection each - two each where meaning search is on - against
// a route admission counted one connection for. The checks are held at the term statistics every
// one of them reads first, so what the edit holds is read while they wait there rather than
// raced for.
func TestAnEditHoldsNoMoreConnectionsThanItsRouteWeighs(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "WGHT")
	createSearchIssue(t, handler, "WGHT", "Dashboard ledger for zymurgy batches", "")
	spec := "# Spec\n\nPlanning notes.\n"
	issue := createAdviceIssue(t, handler, "WGHT", "Five decisions", &spec)
	refreshTermStats(t, database, "WGHT")
	awaitQuietRequestPool(t, database.Pool)

	ctx := context.Background()
	lockTx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(ctx, "lock table search_term_stats in access exclusive mode"); err != nil {
		t.Fatalf("lock search_term_stats: %v", err)
	}
	// The lock's own connection is not the edit's, and the waits are read on it, so the test adds
	// nothing to what the pool hands out while the edit runs.
	ours := database.Pool.Stat().AcquiredConns()

	markdown := make([]string, 0, decisionBlockCheckLimit)
	for index := range decisionBlockCheckLimit {
		markdown = append(markdown, fmt.Sprintf(":::ask{#choice%d}\nWhich zymurgy ledger layout %d?\n:::", index, index))
	}
	edited := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		edited <- editDocument(t, handler, issue.PrimaryArtifactID, insertAtEnd(strings.Join(markdown, "\n\n")))
	}()

	waiting := func() int {
		var waits int
		if err := lockTx.QueryRow(ctx, `
			select count(*) from pg_locks where relation = 'search_term_stats'::regclass and not granted
		`).Scan(&waits); err != nil {
			t.Fatalf("read lock waits: %v", err)
		}
		return waits
	}
	deadline := time.Now().Add(20 * time.Second)
	for waiting() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the edit never committed with a decision block check waiting on the term statistics")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Every check the edit runs at once is held at the lock, so the connections they hold stand
	// still while they are counted.
	var peak int32
	for range 100 {
		peak = max(peak, database.Pool.Stat().AcquiredConns()-ours)
		time.Sleep(5 * time.Millisecond)
	}
	if weight := routeWeightOf(t, http.MethodPost, "/api/v1/artifacts/{id}/edits"); peak > weight {
		t.Errorf("an edit adding %d decision blocks held %d request-pool connections, want at most its route's weight %d",
			decisionBlockCheckLimit, peak, weight)
	}

	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	checks, present := decisionBlockChecksOf(t, <-edited)
	if !present || len(checks) != decisionBlockCheckLimit {
		t.Fatalf("decision_block_checks = %+v (present %t), want %d", checks, present, decisionBlockCheckLimit)
	}
	for index, check := range checks {
		if check.Suggestions == nil || check.Suggestions.Outcome == "not_made" {
			t.Fatalf("check %d = %+v, want it made once the lock was released", index, check.Suggestions)
		}
	}
}

// Every one of an edit's blocks is checked whole, and each reads its items' ask states and
// writes its record: every block whose question restates an answered ask is answered with that
// decision, its ask state on the item, and every block's check is recorded. The checks ran at
// once once, and on the request's one connection mark one check's open cursor had the pool
// refuse another's ask-state read and record (store.ErrNestedAcquire), which dropped the state
// and the row; they run one after another now (decisionBlockChecks), each on a mark of its own.
// Every ask shares a word with every block and carries a long answer, so each check reads every
// ask's state over several network reads.
func TestDecisionBlockChecksEachReadTheirAskStatesAndRecord(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "RACE")
	const shared = " For the aerostat programme."
	blocks := []struct{ id, title, asked, restated string }{
		{"hangar", "Storage planning", "Which zeppelin hangar should store the blimps?", "Which zeppelin hangar stores the blimps?"},
		{"lacquer", "Paint planning", "Which lacquer coats the dirigible gondolas?", "Which lacquer should coat dirigible gondolas?"},
		{"pilot", "Crew planning", "Who pilots the airship on weekends?", "Who should pilot the airship at weekends?"},
		{"fjord", "Route planning", "Should the balloon cross the fjord at dawn?", "Does the balloon cross the fjord at dawn?"},
		{"harbour", "Dock planning", "Which harbour moors the hovercraft?", "Which harbour should moor the hovercraft?"},
	}
	asks := map[string]string{}
	for _, block := range blocks {
		owner := createSearchIssue(t, handler, "RACE", block.title, "")
		opened := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+owner+"/asks", map[string]any{
			"question": block.asked + shared,
			"options":  []map[string]string{{"label": "Brass"}, {"label": "Steel"}},
		}, "alice")
		if opened.Code != http.StatusCreated {
			t.Fatalf("open the %s ask: status=%d body=%s", block.id, opened.Code, opened.Body.String())
		}
		asks[block.id] = decodeBody[testAskWithSuggestions](t, opened).ID
		answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+asks[block.id]+"/answer",
			map[string]any{"selected": []string{"Brass"}, "text": strings.Repeat("ballast ", 40_000)}, "alice")
		if answered.Code != http.StatusOK {
			t.Fatalf("answer the %s ask: status=%d body=%s", block.id, answered.Code, answered.Body.String())
		}
	}
	spec := "# Spec\n\nPlanning notes.\n"
	issue := createAdviceIssue(t, handler, "RACE", "Fleet decisions", &spec)
	refreshTermStats(t, database, "RACE")

	var markdown []string
	for _, block := range blocks {
		markdown = append(markdown, ":::ask{#"+block.id+"}\n"+block.restated+shared+"\n:::")
	}
	checks, present := decisionBlockChecksOf(t, editDocument(t, handler, issue.PrimaryArtifactID, insertAtEnd(strings.Join(markdown, "\n\n"))))
	if !present || len(checks) != len(blocks) {
		t.Fatalf("decision_block_checks = %+v (present %t), want %d", checks, present, len(blocks))
	}
	for index, check := range checks {
		block := blocks[index]
		if check.BlockID != block.id || check.Suggestions == nil {
			t.Fatalf("check %d = %+v, want block %s checked", index, check, block.id)
		}
		decision := check.Suggestions.Decision
		if decision == nil || decision.ID != asks[block.id] || decision.Ask == nil || decision.Ask.State != "answered" {
			t.Fatalf("block %s: decision = %+v (suggestions %+v), want the answered ask %s with its state",
				block.id, decision, check.Suggestions, asks[block.id])
		}
		listed := false
		for _, item := range check.Suggestions.Related {
			if item.Kind == "ask" && item.ID == asks[block.id] {
				listed = item.Ask != nil && item.Ask.State == "answered"
			}
		}
		if !listed {
			t.Fatalf("block %s: related = %+v, want the ask %s listed with its answered state", block.id, check.Suggestions.Related, asks[block.id])
		}
	}
	var recorded []string
	for _, row := range readDecisionBlockRows(t, database) {
		recorded = append(recorded, row.BlockID)
	}
	slices.Sort(recorded)
	if want := []string{"fjord", "hangar", "harbour", "lacquer", "pilot"}; !slices.Equal(recorded, want) {
		t.Fatalf("recorded decision blocks = %v, want %v", recorded, want)
	}
}
