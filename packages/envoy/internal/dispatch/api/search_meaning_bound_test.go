package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// endMeaningStatementsAt points searchMeaningScan's bound (searchMeaningStatementBound) at `at`
// for one test, so that a meaning statement is ended by the server rather than answered, without
// a test waiting production's fifteen seconds out. The bound it replaces must be in the list: a
// list that holds none is the defect INFRA-988 fixed, and a test that quietly added its own
// bound would pass on a server that holds none.
//
// Swapping a package-level statement is safe only because no test in internal/dispatch/api calls
// t.Parallel, so no other test's meaning query can read the shortened list before the cleanup
// puts it back; a test added here that does call t.Parallel breaks these. It is the same seam,
// under the same constraint, as the claim path's (issue_claim_test.go).
//
// A millisecond is not a race against a Go timer: a meaning query is five index arms fused,
// ranked and paged, which no database plans and runs in a millisecond. Were a millisecond ever
// enough for the two `set local` statements the bound precedes, beginMeaningScan would fail with
// that same cancellation instead, and each caller below reports it the same way, so what these
// tests assert does not depend on which of the transaction's statements the bound ends.
func endMeaningStatementsAt(t *testing.T, at string) {
	t.Helper()
	healthy := searchMeaningScan
	shortened := slices.Clone(searchMeaningScan)
	bounded := false
	for index, setting := range shortened {
		if setting == searchMeaningStatementBound {
			shortened[index] = "set local statement_timeout = '" + at + "'"
			bounded = true
		}
	}
	if !bounded {
		t.Fatalf("searchMeaningScan = %q, want it to hold the server's own bound on a meaning statement (%q)",
			searchMeaningScan, searchMeaningStatementBound)
	}
	searchMeaningScan = shortened
	t.Cleanup(func() { searchMeaningScan = healthy })
}

// A meaning query's transaction carries the server's own bound, read from the transaction itself
// rather than from the list that set it: a search whose caller has gone cannot be cancelled while
// the pool is full, since pgx cancels out of band and a CancelRequest needs a connection of its
// own, so the statement has to end itself. The two index settings are read beside it: all three
// reach a query the same way, and a list that lost one of them is read here too.
//
// The bound is local to the transaction, so the connection goes back to the pool without it.
func TestMeaningScanAppliesTheServersBoundToItsTransaction(t *testing.T) {
	_, database, deps := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	tx, err := directServer(deps).beginMeaningScan(ctx)
	if err != nil {
		t.Fatalf("begin the meaning scan: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var timeout, efSearch, iterativeScan string
	if err := tx.QueryRow(ctx, `select current_setting('statement_timeout'), current_setting('hnsw.ef_search'),
		current_setting('hnsw.iterative_scan')`).Scan(&timeout, &efSearch, &iterativeScan); err != nil {
		t.Fatalf("read the settings the meaning transaction applies: %v", err)
	}
	if timeout != "15s" {
		t.Fatalf("statement_timeout = %q in a meaning query's transaction, want 15s: the server's own bound is what "+
			"ends a search whose caller has gone, since pgx cannot deliver a cancellation when the pool is full", timeout)
	}
	if efSearch != "400" || iterativeScan != "relaxed_order" {
		t.Fatalf("hnsw.ef_search = %q and hnsw.iterative_scan = %q, want 400 and relaxed_order", efSearch, iterativeScan)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll the meaning transaction back: %v", err)
	}
	var pooled string
	if err := database.Pool.QueryRow(ctx, `show statement_timeout`).Scan(&pooled); err != nil {
		t.Fatalf("read statement_timeout outside a meaning transaction: %v", err)
	}
	if pooled == "15s" {
		t.Fatalf("statement_timeout = %q outside the meaning transaction, want the server's own default: "+
			"`set local` must not follow a pooled connection to the next request", pooled)
	}
}

// The search route reports a meaning statement the server ended as the failure of this request it
// is - 500 INTERNAL - and never as an answer. runSearch keeps its keyword-only answer for a query
// whose embedding could not be had; papering a cut-off search over as a degraded result would
// serve a caller a page of keyword hits as though meaning search had run.
func TestSearchReportsAMeaningStatementTheServerEnded(t *testing.T) {
	const title, spec = "Celestial navigation device maintenance", "Routine upkeep notes."
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, spec): angledVector(0.9, 2),
		"wibbleflorp":                   queryVector(),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "BOUND", title, spec)
	processAllPending(t, database, embedder)

	// The query shares no word with the issue, so the hit below came from the meaning query
	// alone: this server answered the same search before the bound was shortened, and what the
	// 500 after it reports is the statement the server ended, not a broken fixture.
	healthy := searchResponse(t, handler, "q=wibbleflorp")
	if healthy.Degraded != "" || len(healthy.Results) != 1 || healthy.Results[0].ID != issue.Key {
		t.Fatalf("search before the bound was shortened = %+v, want the issue %s found by meaning", healthy, issue.Key)
	}

	endMeaningStatementsAt(t, "1ms")
	ended := searchRequest(t, handler, "q=wibbleflorp")
	if ended.Code != http.StatusInternalServerError || !strings.Contains(ended.Body.String(), `"code":"INTERNAL"`) {
		t.Fatalf("search under a statement the server ends: status=%d body=%s, want 500 INTERNAL", ended.Code, ended.Body.String())
	}
	if strings.Contains(ended.Body.String(), issue.Key) {
		t.Fatalf("search under a statement the server ends answered results: %s", ended.Body.String())
	}
}

// The related-item check reports a meaning statement the server ended the way INFRA-148 made it
// report a meaning search that did not answer: the check is still made, by keywords alone, and
// says why. Before this it would have read the server's own bound as "meaning search failed",
// which is what it says when something broke.
//
// The check answers at once rather than at the end of its three-second meaning allowance, which
// is what tells the two apart: the statement was ended, not waited out. The text shares one word
// with the issue and means the same thing, so the keyword stage has the item to fall back on and
// the meaning stage has a reason to be running at all.
func TestRelatedCheckReportsAMeaningStatementTheServerEndedAsATimeout(t *testing.T) {
	const title, spec = "Nightly backups overrun their window", "The backup job runs past six in the morning."
	const text = "Nightly snapshots finish late"
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, spec): angledVector(0.9, 2),
		text:                            queryVector(),
	}}
	handler, database, srv := meaningServer(t, embedder)
	createSuggestionProject(t, handler, "MBOUND")
	key := createSearchIssue(t, handler, "MBOUND", title, spec)
	processAllPending(t, database, embedder)

	// The check searched by meaning on this server before the bound was shortened.
	healthy := srv.relatedCheck(context.Background(), relatedInput{project: "MBOUND", text: text})
	if healthy.Search != "meaning_and_keywords" || healthy.SearchNote != "" {
		t.Fatalf("suggestions = %+v before the bound was shortened, want found by meaning and keywords", healthy)
	}

	endMeaningStatementsAt(t, "1ms")
	started := time.Now()
	suggestions := srv.relatedCheck(context.Background(), relatedInput{project: "MBOUND", text: text})
	elapsed := time.Since(started)
	if suggestions.Outcome != "found" || suggestions.Search != "keywords_only" ||
		suggestions.SearchNote != "meaning search did not answer within 3s" {
		t.Fatalf("suggestions = %+v, want found by keywords only, the server having ended the meaning statement", suggestions)
	}
	if !slices.Contains(relatedOwnerKeys(suggestions.Related), key) {
		t.Fatalf("related = %+v, want %s from the keyword search", suggestions.Related, key)
	}
	if elapsed >= relatedMeaningAllowance {
		t.Fatalf("the check took %s, want it answered when the statement was ended rather than at the end of the "+
			"%s meaning allowance", elapsed, relatedMeaningAllowance)
	}
}
