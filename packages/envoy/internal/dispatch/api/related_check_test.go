package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// relatedOwnerKeys is the issue each related item stands for, in order.
func relatedOwnerKeys(related []model.WriteSuggestion) []string {
	keys := make([]string, len(related))
	for index, item := range related {
		keys[index] = item.Owner.Key
	}
	return keys
}

// issueSuggestions reads a write response's advice.suggestions, failing when it has none.
func issueSuggestions(t *testing.T, what string, response *httptest.ResponseRecorder) testSuggestions {
	t.Helper()
	var envelope struct {
		Advice *testAdvisedSuggestions `json:"advice"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %s: %v", what, err)
	}
	if envelope.Advice == nil || envelope.Advice.Suggestions == nil {
		t.Fatalf("%s: advice.suggestions missing: %s", what, response.Body.String())
	}
	return *envelope.Advice.Suggestions
}

// relatedCheckRow is one related_checks row as a test reads it.
type relatedCheckRow struct {
	Project      string
	SourceKind   string
	SourceIssue  *string
	SourceAsk    *string
	ActorKind    string
	ActorID      string
	Outcome      string
	Search       *string
	Reason       *string
	Terms        []string
	KeywordMS    *int
	MeaningMS    *int
	TotalMS      int
	RelatedCount int
}

func readRelatedCheckRows(t *testing.T, database *store.Store, where string, args ...any) []relatedCheckRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select project_key, source_kind, source_issue_key, source_ask_id::text, actor_kind, actor_id, outcome, search,
		       reason, terms, keyword_ms, meaning_ms, total_ms, related_count
		  from related_checks where `+where+` order by created_at`, args...)
	if err != nil {
		t.Fatalf("read related_checks: %v", err)
	}
	defer rows.Close()
	var found []relatedCheckRow
	for rows.Next() {
		var row relatedCheckRow
		if err := rows.Scan(&row.Project, &row.SourceKind, &row.SourceIssue, &row.SourceAsk, &row.ActorKind, &row.ActorID,
			&row.Outcome, &row.Search, &row.Reason, &row.Terms, &row.KeywordMS, &row.MeaningMS, &row.TotalMS, &row.RelatedCount); err != nil {
			t.Fatalf("scan related_checks: %v", err)
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read related_checks: %v", err)
	}
	return found
}

// One word every item of the project shares and words only one item holds: the check searches
// for the rare words, so it lists that one item and none of the thirty that share only the common
// word. The old query ORed every word of the text and listed common-word matches beside it.
func TestRelatedCheckListsTheRareWordMatchNotTheCommonWordMatches(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "RARE")
	var common []string
	for index := range 30 {
		common = append(common, createSearchIssue(t, handler, "RARE", fmt.Sprintf("Dashboard tile quo%c%cvex", 'a'+index/26, 'a'+index%26), ""))
	}
	target := createSearchIssue(t, handler, "RARE", "Dashboard ledger for zymurgy batches", "")
	refreshTermStats(t, database, "RARE")

	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RARE", "title": "Zymurgy ledger totals are wrong on the dashboard", "force": true,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	suggestions := issueSuggestions(t, "the new issue", response)
	if len(suggestions.Related) != 1 || relatedIndex(suggestions.Related, target) != 0 {
		t.Fatalf("related = %+v, want %s alone, none of the issues sharing only \"dashboard\" (%v)", suggestions.Related, target, common[:3])
	}
	if suggestions.Outcome != "found" || suggestions.Search != "keywords_only" || suggestions.SearchNote != "meaning search is off on this server" {
		t.Fatalf("suggestions = %+v, want found, keywords only, because meaning search is off", suggestions)
	}
	if !strings.Contains(suggestions.SearchQuery, "Zymurgy") || !strings.Contains(suggestions.SearchQuery, "ledger") ||
		strings.Contains(strings.ToLower(suggestions.SearchQuery), "dashboard") {
		t.Fatalf("search_query = %q, want the rare words as the title spells them and not the common one", suggestions.SearchQuery)
	}
}

// An item holding three of the text's rare words once each is listed above one repeating a
// fourth, as rare, eight times, by both of the check's queries: an item ranks by the summed idf
// of the chosen words it holds, where ts_rank_cd's cover density counted every repetition and
// listed the repeater first. The meaning stage answers here with no item near the text's
// meaning, so its order is its keyword arms'.
func TestRelatedCheckRanksMoreChosenWordsAboveOneRepeated(t *testing.T) {
	for _, stage := range []struct {
		name    string
		options testServerOptions
		search  string
	}{
		{"keywords only", testServerOptions{}, "keywords_only"},
		{"meaning and keywords", testServerOptions{embedder: &fakeEmbedder{}}, "meaning_and_keywords"},
	} {
		t.Run(stage.name, func(t *testing.T) {
			handler, database, deps := newTestServer(t, stage.options)
			createSuggestionProject(t, handler, "COORD")
			for index := range 5 {
				createSearchIssue(t, handler, "COORD", fmt.Sprintf("Unrelated filler issue %d", index), "")
			}
			repeater := createSearchIssue(t, handler, "COORD", strings.Repeat("Marmot ", 8), "")
			holder := createSearchIssue(t, handler, "COORD", "Zymurgy ledger quokka", "")
			refreshTermStats(t, database, "COORD")

			suggestions := directServer(deps).relatedCheck(context.Background(), relatedInput{project: "COORD", text: "Zymurgy ledger quokka marmot"})
			if suggestions.Search != stage.search {
				t.Fatalf("suggestions = %+v, want searched by %s", suggestions, stage.search)
			}
			if got := relatedOwnerKeys(suggestions.Related); !slices.Equal(got, []string{holder, repeater}) {
				t.Fatalf("related = %v, want %s (three chosen words once each) above %s (one chosen word eight times)", got, holder, repeater)
			}
		})
	}
}

// meaningServer is a server with embedder configured, and the direct server a test runs the check
// on.
func meaningServer(t *testing.T, embedder *fakeEmbedder) (http.Handler, *store.Store, *server) {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	return handler, database, directServer(deps)
}

// An item whose meaning matches and which holds none of the text's words is found by the meaning
// search, and the result says it searched by meaning and keywords.
func TestRelatedCheckMeaningListsASameMeaningItemWithNoSharedWord(t *testing.T) {
	const title, spec = "Nightly backups overrun their window", "The backup job runs past six in the morning."
	const text = "Archive snapshots finish late"
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, spec): angledVector(0.9, 2),
		text:                            queryVector(),
	}}
	handler, database, srv := meaningServer(t, embedder)
	createSuggestionProject(t, handler, "MEAN")
	key := createSearchIssue(t, handler, "MEAN", title, spec)
	processAllPending(t, database, embedder)

	suggestions := srv.relatedCheck(context.Background(), relatedInput{project: "MEAN", text: text})
	if suggestions.Outcome != "found" || suggestions.Search != "meaning_and_keywords" || suggestions.SearchNote != "" {
		t.Fatalf("suggestions = %+v, want found by meaning and keywords", suggestions)
	}
	if !slices.Contains(relatedOwnerKeys(suggestions.Related), key) {
		t.Fatalf("related = %+v, want %s, which means the same and shares no word", suggestions.Related, key)
	}
}

// A failing embedder leaves the check its keyword search, and the result says so and why.
func TestRelatedCheckMeaningEmbedderFailingSearchesByKeywordsOnly(t *testing.T) {
	embedder := &fakeEmbedder{err: errors.New("model service throttled")}
	handler, _, srv := meaningServer(t, embedder)
	createSuggestionProject(t, handler, "MFAIL")
	key := createSearchIssue(t, handler, "MFAIL", "Nightly backups overrun their window", "")

	suggestions := srv.relatedCheck(context.Background(), relatedInput{project: "MFAIL", text: "Nightly snapshots finish late"})
	if suggestions.Outcome != "found" || suggestions.Search != "keywords_only" ||
		suggestions.SearchNote != "the text could not be embedded for meaning search" {
		t.Fatalf("suggestions = %+v, want found by keywords only, the embedding having failed", suggestions)
	}
	if !slices.Contains(relatedOwnerKeys(suggestions.Related), key) {
		t.Fatalf("related = %+v, want %s from the keyword search", suggestions.Related, key)
	}
}

// An embedder slower than the meaning allowance costs the check three seconds, not five: it
// answers its keyword search's result then, saying meaning search did not answer in time.
func TestRelatedCheckMeaningEmbedderPastThreeSecondsSearchesByKeywordsOnly(t *testing.T) {
	embedder := &fakeEmbedder{delay: time.Minute}
	handler, _, srv := meaningServer(t, embedder)
	createSuggestionProject(t, handler, "MSLOW")
	key := createSearchIssue(t, handler, "MSLOW", "Nightly backups overrun their window", "")

	started := time.Now()
	suggestions := srv.relatedCheck(context.Background(), relatedInput{project: "MSLOW", text: "Nightly snapshots finish late"})
	elapsed := time.Since(started)
	if suggestions.Outcome != "found" || suggestions.Search != "keywords_only" ||
		suggestions.SearchNote != "meaning search did not answer within 3s" {
		t.Fatalf("suggestions = %+v, want found by keywords only, meaning search having run out of time", suggestions)
	}
	if !slices.Contains(relatedOwnerKeys(suggestions.Related), key) {
		t.Fatalf("related = %+v, want %s from the keyword search", suggestions.Related, key)
	}
	if elapsed < 3*time.Second || elapsed >= 4*time.Second {
		t.Fatalf("the check took %s, want it to answer when the 3s meaning allowance ended", elapsed)
	}
}

// pgvector's HNSW index hands back hnsw.ef_search nearest vectors (40 by default) before the
// project filter runs. Sixty vectors of another project nearer than the project's own item fill
// those 40, so without the check's wider ef_search the item is never seen. The vectors lie on one
// arc from the query, so the index's graph links each to its neighbours along it and a wide
// enough walk reaches them all; vectors that are all equally far from each other leave some
// unreachable in a graph this small. The check's server here runs with sequential scans and sorts
// made costly, as a corpus large enough for the planner to choose the index does on its own (a
// handful of test rows is otherwise read whole and sorted exactly,
// TestSearchMeaningLegsUseTheHNSWIndexNotASequentialScan), and without JIT, which the disabled
// plans' inflated costs would otherwise spend seconds compiling.
func TestRelatedCheckMeaningFindsAProjectsItemBeyondOtherProjectsNearerVectors(t *testing.T) {
	const text = "Find what this is about"
	const nearer = 60
	arc := func(angle float64) []float32 { return angledVector(math.Cos(angle), 1) }
	vectors := map[string][]float32{text: queryVector(), "Our own distant cousin": arc(0.8)}
	for index := range nearer {
		vectors[fmt.Sprintf("Crowded neighbour %d", index)] = arc(0.005 * float64(index+1))
	}
	embedder := &fakeEmbedder{vectors: vectors}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	createSuggestionProject(t, handler, "CROWD")
	createSuggestionProject(t, handler, "MINE")
	for index := range nearer {
		createSearchIssue(t, handler, "CROWD", fmt.Sprintf("Crowded neighbour %d", index), "")
	}
	own := createSearchIssue(t, handler, "MINE", "Our own distant cousin", "")
	processAllPending(t, database, embedder)

	config := deps.Store.Pool.Config()
	config.ConnConfig.RuntimeParams["enable_seqscan"] = "off"
	config.ConnConfig.RuntimeParams["enable_sort"] = "off"
	config.ConnConfig.RuntimeParams["jit"] = "off"
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open the index-preferring pool: %v", err)
	}
	indexStore := &store.Store{Pool: store.NewPool(opened)}
	t.Cleanup(indexStore.Pool.Close)
	indexDeps := deps
	indexDeps.Store = indexStore

	suggestions := directServer(indexDeps).relatedCheck(context.Background(), relatedInput{project: "MINE", text: text})
	if suggestions.Search != "meaning_and_keywords" {
		t.Fatalf("suggestions = %+v, want the meaning search to have answered", suggestions)
	}
	if !slices.Contains(relatedOwnerKeys(suggestions.Related), own) {
		t.Fatalf("related = %+v, want %s, the project's own nearest item behind %d nearer vectors of another project", suggestions.Related, own, nearer)
	}
}

// A keyword stage held a second, well past the old 300 ms bound, still answers within the
// check's allowance.
func TestRelatedCheckBoundsCompletesBehindAOneSecondLock(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "SLOW")
	key := createSearchIssue(t, handler, "SLOW", "Quarterly zymurgy ledger drifts", "")
	srv := directServer(deps)

	lockTx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), "lock table search_term_stats in access exclusive mode"); err != nil {
		t.Fatalf("lock search_term_stats: %v", err)
	}
	started := time.Now()
	answered := make(chan *model.Suggestions, 1)
	go func() {
		answered <- srv.relatedCheck(context.Background(), relatedInput{project: "SLOW", text: "Zymurgy ledger totals drift"})
	}()
	time.Sleep(time.Second)
	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	suggestions := <-answered
	if elapsed := time.Since(started); elapsed < time.Second {
		t.Fatalf("the check answered in %s, before the lock it reads behind was released", elapsed)
	}
	if suggestions.Outcome != "found" || !slices.Contains(relatedOwnerKeys(suggestions.Related), key) {
		t.Fatalf("suggestions = %+v, want %s found once the lock was released", suggestions, key)
	}
}

// A keyword stage held past the allowance answers that the check was not made, for time, and the
// check is recorded so.
func TestRelatedCheckBoundsPastTheAllowanceIsNotMadeAndRecorded(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "STUCK")
	createSearchIssue(t, handler, "STUCK", "Quarterly zymurgy ledger drifts", "")
	source := createSearchIssue(t, handler, "STUCK", "Zymurgy ledger totals drift", "")
	srv := directServer(deps)
	srv.suggestionTimeout = 500 * time.Millisecond

	lockTx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), "lock table search_term_stats in access exclusive mode"); err != nil {
		t.Fatalf("lock search_term_stats: %v", err)
	}
	suggestions := srv.relatedCheck(context.Background(), relatedInput{
		project: "STUCK", text: "Zymurgy ledger totals drift",
		source: suggestionSource{kind: "issue", issueKey: source, actor: model.Actor{Kind: "user", ID: "alice"}},
		record: true,
	})
	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	if suggestions.Outcome != "not_made" || suggestions.Missing != "the search behind it did not answer within 500ms" ||
		suggestions.Search != "" || len(suggestions.Related) != 0 {
		t.Fatalf("suggestions = %+v, want not made, the search not having answered within the allowance", suggestions)
	}
	rows := readRelatedCheckRows(t, database, "source_issue_key = $1 and reason = 'timeout'", source)
	if len(rows) != 1 {
		t.Fatalf("related_checks timeout rows for %s = %+v, want one", source, rows)
	}
	row := rows[0]
	if row.Outcome != "not_made" || row.Search != nil || row.KeywordMS != nil || row.TotalMS < 500 || row.RelatedCount != 0 ||
		row.Project != "STUCK" || row.SourceKind != "issue" || row.ActorID != "alice" {
		t.Fatalf("related_checks row = %+v (search %q, keyword_ms set %t), want a not-made issue check that took the 500ms allowance", row, deref(row.Search), row.KeywordMS != nil)
	}
}

// A check whose request goes away while its keyword stage is held, long before its allowance
// ends, is not made because the request ended, and is recorded as cancelled: it did not run out of
// time, so the records route must not count it as skipped for time.
func TestRelatedCheckBoundsARequestThatEndsFirstIsRecordedCancelled(t *testing.T) {
	handler, database, deps, served := newRegisteredTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "GONE")
	createSearchIssue(t, handler, "GONE", "Quarterly zymurgy ledger drifts", "")
	source := createSearchIssue(t, handler, "GONE", "Zymurgy ledger totals drift", "")
	// The creates' own checks may have started a refresh of the statistics; wait it out and
	// refresh them here, so the check below starts none and its pick is the one waiter on the lock.
	awaitTermStatsRefresh(t, served, "GONE")
	refreshTermStats(t, database, "GONE")
	srv := directServer(deps)
	srv.suggestionTimeout = time.Minute

	lockTx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), "lock table search_term_stats in access exclusive mode"); err != nil {
		t.Fatalf("lock search_term_stats: %v", err)
	}
	request, endRequest := context.WithCancel(context.Background())
	defer endRequest()
	answered := make(chan *model.Suggestions, 1)
	go func() {
		answered <- srv.relatedCheck(request, relatedInput{
			project: "GONE", text: "Zymurgy ledger totals drift",
			source: suggestionSource{kind: "issue", issueKey: source, actor: model.Actor{Kind: "user", ID: "alice"}},
			record: true,
		})
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var waiting int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from pg_locks where relation = 'search_term_stats'::regclass and not granted
		`).Scan(&waiting); err != nil {
			t.Fatalf("read lock waits: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the check's pick never waited on the term statistics")
		}
	}
	endRequest()
	var suggestions *model.Suggestions
	select {
	case suggestions = <-answered:
	case <-time.After(10 * time.Second):
		t.Fatal("the check did not answer within 10s of its request ending")
	}
	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	if suggestions.Outcome != "not_made" || suggestions.Missing != "the request that ran it ended before the check finished" {
		t.Errorf("suggestions = %+v, want not made, the request having ended first", suggestions)
	}
	rows := readRelatedCheckRows(t, database, "source_issue_key = $1 and outcome = 'not_made'", source)
	if len(rows) != 1 || deref(rows[0].Reason) != "cancelled" {
		reasons := make([]string, len(rows))
		for index, row := range rows {
			reasons[index] = deref(row.Reason)
		}
		t.Fatalf("not-made related_checks rows for %s have reasons %q, want one cancelled", source, reasons)
	}
}

// An edit's decision blocks are all checked under one allowance of the edit's own, and that
// context ends with the check's own cause (relatedBounds): a keyword stage held past it answers
// every block that its check was not made for time, and records each as timeout. Were the cause
// left off, each block's check would read its context's end as the request that ran it going away
// and record cancelled, which the records route does not count as skipped for time.
func TestRelatedCheckBoundsDecisionBlocksPastTheAllowanceAreRecordedTimeout(t *testing.T) {
	handler, database, deps, served := newRegisteredTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "DBTO")
	createSearchIssue(t, handler, "DBTO", "Quarterly zymurgy ledger drifts", "")
	spec := "# Spec\n\nPlanning notes.\n"
	issue := createAdviceIssue(t, handler, "DBTO", "Quarterly planning", &spec)
	// The creates' own checks may have started a refresh of the statistics; wait it out and
	// refresh them here, so the blocks' checks start none behind the lock below.
	awaitTermStatsRefresh(t, served, "DBTO")
	refreshTermStats(t, database, "DBTO")
	srv := directServer(deps)
	srv.suggestionTimeout = 500 * time.Millisecond

	lockTx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), "lock table search_term_stats in access exclusive mode"); err != nil {
		t.Fatalf("lock search_term_stats: %v", err)
	}
	blocks := []docs.AddedAskBlock{
		{ID: "rounding", Question: "Should zymurgy ledger totals round down?"},
		{ID: "window", Question: "Which zymurgy ledger drift closes the quarter?"},
	}
	artifact := model.Artifact{ID: issue.PrimaryArtifactID, IssueKey: &issue.Key, Project: "DBTO"}
	checks := srv.decisionBlockChecks(context.Background(), artifact, blocks,
		model.Actor{Kind: "user", ID: "alice"})
	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	if len(checks) != len(blocks) {
		t.Fatalf("decision block checks = %+v, want one per block", checks)
	}
	for index, check := range checks {
		suggestions := check.Suggestions
		if check.BlockID != blocks[index].ID {
			t.Fatalf("check %d is of block %q, want %q", index, check.BlockID, blocks[index].ID)
		}
		if suggestions == nil || suggestions.Outcome != "not_made" ||
			suggestions.Missing != "the search behind it did not answer within 500ms" ||
			suggestions.Search != "" || len(suggestions.Related) != 0 {
			t.Fatalf("block %s suggestions = %+v, want not made, the search not having answered within the allowance",
				check.BlockID, suggestions)
		}
	}
	// The rows are recorded as each check answers, in no order of the blocks', so each block's
	// row is read by its id.
	rows := map[string]decisionBlockRow{}
	for _, row := range readDecisionBlockRows(t, database) {
		rows[row.BlockID] = row
	}
	if len(rows) != len(blocks) {
		t.Fatalf("decision block related_checks rows = %+v, want one per block", rows)
	}
	for _, block := range blocks {
		row := rows[block.ID]
		if row.IssueKey == nil || *row.IssueKey != issue.Key || row.ArtifactID != nil ||
			row.Outcome != "not_made" || deref(row.Reason) != "timeout" || row.ActorID != "alice" {
			t.Fatalf("block %s row = %+v (reason %q), want it recorded not made for time on %s",
				block.ID, row, deref(row.Reason), issue.Key)
		}
	}
}

// A migration's lock on asks holds the check past its allowance and then holds its record too,
// since related_checks' insert locks every table its foreign keys name even where the key is null.
// When the record's second runs out, pgx cancels its statement, so the row is lost unless it is
// tried again: it is written once the lock is released.
func TestRelatedCheckRecordBlockedPastItsSecondIsWrittenOnceTheLockIsReleased(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "MIGR")
	createSearchIssue(t, handler, "MIGR", "Quarterly zymurgy ledger drifts", "")
	source := createSearchIssue(t, handler, "MIGR", "Zymurgy ledger totals drift", "")
	srv := directServer(deps)
	srv.suggestionTimeout = 500 * time.Millisecond

	lockTx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), "lock table asks in access exclusive mode"); err != nil {
		t.Fatalf("lock asks: %v", err)
	}
	suggestions := srv.relatedCheck(context.Background(), relatedInput{
		project: "MIGR", text: "Zymurgy ledger totals drift",
		source: suggestionSource{kind: "issue", issueKey: source, actor: model.Actor{Kind: "user", ID: "alice"}},
		record: true,
	})
	if suggestions.Outcome != "not_made" || suggestions.Missing != "the search behind it did not answer within 500ms" {
		t.Fatalf("suggestions = %+v, want not made, the search not having answered within the allowance", suggestions)
	}

	// Hold the lock until the cancel pgx sends after the record's first attempt has landed: that
	// attempt started its insert relatedRecordTimeout before the check answered, any retry's
	// only once it had failed, so an insert started more than half that before is the first.
	poll := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !done() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not within 5s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	var answered time.Time
	if err := database.Pool.QueryRow(context.Background(), "select clock_timestamp()").Scan(&answered); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	poll("the first attempt's insert cancelled", func() bool {
		var waiting bool
		if err := database.Pool.QueryRow(context.Background(), `
			select exists (select from pg_stat_activity
			 where datname = current_database() and state = 'active' and pid <> pg_backend_pid()
			   and query like '%insert into related_checks%' and query_start < $1)
		`, answered.Add(-relatedRecordTimeout/2)).Scan(&waiting); err != nil {
			t.Fatalf("read the waiting inserts: %v", err)
		}
		return !waiting
	})
	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatalf("release the lock: %v", err)
	}

	var rows []relatedCheckRow
	poll("the check's related_checks row written after the lock's release", func() bool {
		rows = readRelatedCheckRows(t, database, "source_issue_key = $1 and outcome = 'not_made'", source)
		return len(rows) > 0
	})
	if len(rows) != 1 {
		t.Fatalf("not-made related_checks rows for %s = %+v, want one", source, rows)
	}
	if row := rows[0]; deref(row.Reason) != "timeout" || row.SourceKind != "issue" || row.ActorID != "alice" {
		t.Fatalf("related_checks row = %+v (reason %q), want the timeout check of the issue", row, deref(row.Reason))
	}
}

// Shutdown waits for background work while requests may still be running, so a check's record can
// start its retry after the wait has begun - here while a keyed handler, still in flight, holds the
// wait open. The retry is counted with that work: the wait neither returns while the retry still
// holds its row nor panics, and the row is written once the lock holding it up is released.
func TestRelatedCheckRecordRetryStartedWhileShutdownWaitsIsWaitedFor(t *testing.T) {
	handler, database, _, srv := newRegisteredTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "SHUT")
	createSearchIssue(t, handler, "SHUT", "Quarterly zymurgy ledger drifts", "")
	source := createSearchIssue(t, handler, "SHUT", "Zymurgy ledger totals drift", "")
	srv.suggestionTimeout = 500 * time.Millisecond

	lockTx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), "lock table asks in access exclusive mode"); err != nil {
		t.Fatalf("lock asks: %v", err)
	}

	// A keyed handler still in flight when shutdown begins its wait.
	srv.detached.start()
	handlerDone := sync.OnceFunc(srv.detached.done)
	t.Cleanup(handlerDone)
	waited := make(chan any, 1)
	go func() {
		defer func() { waited <- recover() }()
		srv.waitBackground()
	}()

	srv.relatedCheck(context.Background(), relatedInput{
		project: "SHUT", text: "Zymurgy ledger totals drift",
		source: suggestionSource{kind: "issue", issueKey: source, actor: model.Actor{Kind: "user", ID: "alice"}},
		record: true,
	})
	if got := srv.relatedRecordRetrying.Load(); got != 1 {
		t.Fatalf("records being retried after the check answered = %d, want 1 (its first attempt held up by the lock)", got)
	}
	handlerDone()
	select {
	case recovered := <-waited:
		t.Fatalf("the shutdown wait returned (panic %v) while the record's retry still held its row", recovered)
	case <-time.After(300 * time.Millisecond):
	}

	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	select {
	case recovered := <-waited:
		if recovered != nil {
			t.Fatalf("the shutdown wait panicked: %v", recovered)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the shutdown wait did not return within 10s of the lock's release")
	}
	rows := readRelatedCheckRows(t, database, "source_issue_key = $1 and outcome = 'not_made'", source)
	if len(rows) != 1 || deref(rows[0].Reason) != "timeout" {
		t.Fatalf("not-made related_checks rows for %s once the wait returned = %+v, want the one timeout row", source, rows)
	}
}

// A text with no word to search for, blank or stopwords alone, answers that no check was made
// and why, never nothing.
func TestRelatedCheckBoundsBlankTextIsNotMade(t *testing.T) {
	handler, _, deps := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "BLANK")
	createSearchIssue(t, handler, "BLANK", "Is it to be or not to be", "")
	srv := directServer(deps)
	for _, text := range []string{"   \n\t", "Is it to be or not to be?"} {
		suggestions := srv.relatedCheck(context.Background(), relatedInput{project: "BLANK", text: text})
		if suggestions.Outcome != "not_made" || suggestions.Missing != "the text holds no word to search for" ||
			suggestions.Related == nil || len(suggestions.Related) != 0 || suggestions.SearchQuery != "" {
			t.Fatalf("check of %q = %+v, want not made for a blank text", text, suggestions)
		}
	}
}

// A forced create is told the title overlaps it forced past even when the search behind its check
// cannot run, and the issue is created.
func TestCreateIssueForceNamesTitleMatchesWhenTheSearchFails(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	served.suggestionTimeout = time.Second
	createSuggestionProject(t, handler, "FORCE")
	const title = "Archive search drops rows past offset fifty"
	existing := createSuggestionIssue(t, handler, map[string]any{"project": "FORCE", "title": title})

	lockTx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), "lock table asks in access exclusive mode"); err != nil {
		t.Fatalf("lock asks: %v", err)
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "FORCE", "title": title, "force": true,
	}, "alice")
	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	if response.Code != http.StatusCreated {
		t.Fatalf("forced create: status=%d body=%s", response.Code, response.Body.String())
	}
	created := decodeBody[testIssueWithSuggestions](t, response)
	suggestions := issueSuggestions(t, "the forced create", response)
	if suggestions.Outcome != "not_made" || suggestions.Missing == "" {
		t.Fatalf("suggestions = %+v, want the check not made while asks is locked", suggestions)
	}
	if len(suggestions.TitleMatches) == 0 || suggestions.TitleMatches[0].Key != existing {
		t.Fatalf("title_matches = %+v, want %s, whose title the create forced past", suggestions.TitleMatches, existing)
	}
	if read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+created.Key, nil, "alice"); read.Code != http.StatusOK {
		t.Fatalf("read the forced issue %s: status=%d body=%s", created.Key, read.Code, read.Body.String())
	}
}

// An issue create whose write-advice read fails still answers its check, on advice of its own.
func TestCreateIssueKeepsTheCheckWhenWriteAdviceFails(t *testing.T) {
	handler, _ := newFailingAdviceHandler(t, "session_writes_since_human", 0)
	createAdviceProject(t, handler, "ADVISSUE")
	spec := ""
	existing := createAdviceIssue(t, handler, "ADVISSUE", "CSV export hangs for billing accounts", &spec)

	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "ADVISSUE", "title": "Billing account CSV export never finishes", "force": true,
		"actor": adviceSessionActor("session-issue-advice-fails"),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create with failed write advice: status=%d body=%s", response.Code, response.Body.String())
	}
	suggestions := issueSuggestions(t, "the create with failed write advice", response)
	if suggestions.Outcome != "found" || relatedIndex(suggestions.Related, existing.Key) < 0 {
		t.Fatalf("suggestions = %+v, want %s found despite the failed write-advice read", suggestions, existing.Key)
	}
}

// askStateFixture is one project holding an ask in each state, each on its own issue whose title
// shares no word with the asks, so each ask is its owner's only match.
type askStateFixture struct {
	handler  http.Handler
	database *store.Store
	srv      *server
	asks     map[string]string
}

func newAskStateFixture(t *testing.T, project string) askStateFixture {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, project)
	fixture := askStateFixture{handler: handler, database: database, srv: directServer(deps), asks: map[string]string{}}
	for _, ask := range []struct{ name, title, question string }{
		{"twice", "Storage planning", "Which zeppelin hangar should store the blimps?"},
		{"no-option", "Paint planning", "Which lacquer coats the dirigible gondolas?"},
		{"closed", "Crew planning", "Who pilots the airship on weekends?"},
		{"open", "Route planning", "Should the balloon cross the fjord at dawn?"},
	} {
		issue := createSearchIssue(t, handler, project, ask.title, "")
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{
			"question": ask.question,
			"options":  []map[string]string{{"label": "Brass"}, {"label": "Steel"}},
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("open the %s ask: status=%d body=%s", ask.name, response.Code, response.Body.String())
		}
		fixture.asks[ask.name] = decodeBody[testAskWithSuggestions](t, response).ID
	}
	return fixture
}

func (fixture askStateFixture) answer(t *testing.T, name, login string, body map[string]any) model.Ask {
	t.Helper()
	response := dispatchRequest(t, fixture.handler, http.MethodPost, "/api/v1/asks/"+fixture.asks[name]+"/answer", body, login)
	if response.Code != http.StatusOK {
		t.Fatalf("answer the %s ask: status=%d body=%s", name, response.Code, response.Body.String())
	}
	return decodeBody[model.Ask](t, response)
}

// relatedAsk is the related item for askID, failing when the check did not list it.
func relatedAsk(t *testing.T, suggestions testSuggestions, askID string) testSuggestionItem {
	t.Helper()
	for _, item := range suggestions.Related {
		if item.Kind == "ask" && item.ID == askID {
			return item
		}
	}
	t.Fatalf("related = %+v, want the ask %s", suggestions.Related, askID)
	return testSuggestionItem{}
}

// checkThroughJSON runs the check directly and reads its result back as a client does.
func checkThroughJSON(t *testing.T, srv *server, input relatedInput) testSuggestions {
	t.Helper()
	encoded, err := json.Marshal(srv.relatedCheck(context.Background(), input))
	if err != nil {
		t.Fatalf("encode the check's result: %v", err)
	}
	var decoded testSuggestions
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode the check's result: %v", err)
	}
	return decoded
}

// An ask whose answer was replaced shows its latest answer, who gave it, when and the options
// chosen, on the item the ask route's check lists and on its decision. Only the person who gave
// the current answer may change it, so both answers are alice's; the time, the option and the text
// tell them apart.
func TestRelatedCheckShowsTheLatestAnswer(t *testing.T) {
	fixture := newAskStateFixture(t, "LATEST")
	first := fixture.answer(t, "twice", "alice", map[string]any{"selected": []string{"Brass"}, "text": "Keep the brass one."})
	latest := fixture.answer(t, "twice", "alice", map[string]any{
		"selected": []string{"Steel"}, "text": "Steel after all.", "expected_answer_at": timestampValue(first.Answer.At),
	})

	target := createSearchIssue(t, fixture.handler, "LATEST", "Hangar assignments", "")
	response := dispatchRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+target+"/asks", map[string]any{
		"question": "Which zeppelin hangar stores the blimps now?",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("open the new ask: status=%d body=%s", response.Code, response.Body.String())
	}
	suggestions := issueSuggestions(t, "the new ask", response)
	item := relatedAsk(t, suggestions, fixture.asks["twice"])
	if item.Ask == nil || item.Ask.State != "answered" || item.Ask.Answer == nil {
		t.Fatalf("ask state = %+v, want answered", item.Ask)
	}
	answer := item.Ask.Answer
	if answer.User != "alice" || !answer.At.Equal(latest.Answer.At) || answer.At.Equal(first.Answer.At) ||
		!slices.Equal(answer.Selected, []string{"Steel"}) || answer.Text == nil || *answer.Text != "Steel after all." {
		t.Fatalf("answer = %+v, want alice's latest Steel answer at %s", answer, latest.Answer.At)
	}
	if suggestions.Decision == nil || suggestions.Decision.ID != fixture.asks["twice"] || suggestions.Decision.AnsweredBy != "alice" ||
		suggestions.Decision.AnsweredAt == nil || suggestions.Decision.Ask == nil ||
		!slices.Equal(suggestions.Decision.Ask.Answer.Selected, []string{"Steel"}) {
		t.Fatalf("decision = %+v, want the re-answered ask with its latest answer", suggestions.Decision)
	}
}

// An answer that chose no option says so with an empty list, never null.
func TestRelatedCheckShowsAnAnswerWithNoOptionChosen(t *testing.T) {
	fixture := newAskStateFixture(t, "NOOPT")
	fixture.answer(t, "no-option", "alice", map[string]any{"selected": []string{}, "text": "Neither; use enamel."})

	encoded, err := json.Marshal(fixture.srv.relatedCheck(context.Background(), relatedInput{
		project: "NOOPT", text: "Which lacquer for the dirigible gondolas?",
	}))
	if err != nil {
		t.Fatalf("encode the check's result: %v", err)
	}
	if !strings.Contains(string(encoded), `"selected":[]`) {
		t.Fatalf("check result = %s, want the answer's selected as an empty list", encoded)
	}
	var decoded testSuggestions
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode the check's result: %v", err)
	}
	item := relatedAsk(t, decoded, fixture.asks["no-option"])
	if item.Ask == nil || item.Ask.State != "answered" || item.Ask.Answer == nil || len(item.Ask.Answer.Selected) != 0 ||
		item.Ask.Answer.Text == nil || *item.Ask.Answer.Text != "Neither; use enamel." {
		t.Fatalf("ask state = %+v, want answered with no option chosen", item.Ask)
	}
}

// An ask closed with no answer carries its resolution and no answer.
func TestRelatedCheckShowsAnAskClosedWithNoAnswer(t *testing.T) {
	fixture := newAskStateFixture(t, "CLOSED")
	if response := dispatchRequest(t, fixture.handler, http.MethodPost, "/api/v1/asks/"+fixture.asks["closed"]+"/resolve", map[string]any{
		"kind": "resolved", "reason": "The roster settles it.",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("resolve: status=%d body=%s", response.Code, response.Body.String())
	}

	suggestions := checkThroughJSON(t, fixture.srv, relatedInput{project: "CLOSED", text: "Who pilots the airship?"})
	item := relatedAsk(t, suggestions, fixture.asks["closed"])
	if item.Ask == nil || item.Ask.State != "resolved" || item.Ask.Answer != nil || item.Ask.Resolution == nil ||
		item.Ask.Resolution.Kind != "resolved" || item.Ask.Resolution.Reason != "The roster settles it." {
		t.Fatalf("ask state = %+v, want resolved with its resolution and no answer", item.Ask)
	}
	if suggestions.Decision != nil {
		t.Fatalf("decision = %+v, want none: a closed ask settles nothing", suggestions.Decision)
	}
}

// An open ask says it is open.
func TestRelatedCheckShowsAnOpenAsk(t *testing.T) {
	fixture := newAskStateFixture(t, "OPENASK")
	suggestions := checkThroughJSON(t, fixture.srv, relatedInput{project: "OPENASK", text: "Should the balloon cross the fjord?"})
	item := relatedAsk(t, suggestions, fixture.asks["open"])
	if item.Ask == nil || item.Ask.State != "open" || item.Ask.Answer != nil || item.Ask.Resolution != nil {
		t.Fatalf("ask state = %+v, want open with no answer or resolution", item.Ask)
	}
}

// A failure partway through reading the asks' states leaves no item an ask state. An ask answered
// and then retracted keeps its answer only in its events, read after every row's state; with
// events locked past the read's deadline, keeping the states already read would show that ask as
// closed with no answer and still name the answered one the decision.
func TestRelatedCheckCarriesNoAskStateWhenTheAnswersCannotBeRead(t *testing.T) {
	fixture := newAskStateFixture(t, "PARTIAL")
	fixture.answer(t, "twice", "alice", map[string]any{"selected": []string{"Brass"}})
	retractedAnswer := fixture.answer(t, "closed", "alice", map[string]any{"selected": []string{"Steel"}, "text": "Steel pilots."})
	if _, err := fixture.database.Pool.Exec(context.Background(), `
		update asks
		set state = 'resolved', answer = null,
		    resolution = jsonb_build_object(
		      'kind', 'retracted',
		      'reason', 'removed from the document in version 2',
		      'actor', jsonb_build_object('kind', 'system', 'id', 'document-settlement'),
		      'at', now()
		    )
		where id = $1
	`, fixture.asks["closed"]); err != nil {
		t.Fatalf("retract the answered ask: %v", err)
	}
	var results []model.SearchResult
	for _, name := range []string{"twice", "closed"} {
		var issueKey string
		if err := fixture.database.Pool.QueryRow(context.Background(),
			`select issue_key from asks where id = $1`, fixture.asks[name]).Scan(&issueKey); err != nil {
			t.Fatalf("read the %s ask's issue: %v", name, err)
		}
		results = append(results, model.SearchResult{
			Kind: "ask", ID: fixture.asks[name], Owner: model.SearchOwner{Kind: "issue", Key: issueKey, Status: "todo"},
		})
	}
	fill := func(ctx context.Context) (model.Suggestions, map[string]map[string]json.RawMessage) {
		suggestions := model.Suggestions{}
		fixture.srv.fillRelatedItems(ctx, relatedInput{project: "PARTIAL"}, slices.Clone(results), &suggestions)
		encoded, err := json.Marshal(suggestions)
		if err != nil {
			t.Fatalf("encode the related items: %v", err)
		}
		var wire struct {
			Related []map[string]json.RawMessage `json:"related"`
		}
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatalf("decode the related items: %v", err)
		}
		byID := map[string]map[string]json.RawMessage{}
		for _, item := range wire.Related {
			byID[rawString(t, item["id"])] = item
		}
		return suggestions, byID
	}

	t.Run("answers read", func(t *testing.T) {
		suggestions, wire := fill(context.Background())
		if _, ok := wire[fixture.asks["closed"]]["ask"]; !ok {
			t.Fatalf("related = %v, want the retracted ask's state", wire)
		}
		for _, item := range suggestions.Related {
			if item.ID != fixture.asks["closed"] {
				continue
			}
			if item.Ask == nil || item.Ask.State != "resolved" || item.Ask.Answer == nil ||
				!item.Ask.Answer.At.Equal(retractedAnswer.Answer.At) || !slices.Equal(item.Ask.Answer.Selected, []string{"Steel"}) {
				t.Fatalf("ask state = %+v, want resolved with alice's Steel answer", item.Ask)
			}
		}
		if suggestions.Decision == nil || suggestions.Decision.ID != fixture.asks["twice"] {
			t.Fatalf("decision = %+v, want the answered ask", suggestions.Decision)
		}
	})

	t.Run("answers locked", func(t *testing.T) {
		lockTx, err := fixture.database.Pool.Begin(context.Background())
		if err != nil {
			t.Fatalf("begin lock tx: %v", err)
		}
		t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
		if _, err := lockTx.Exec(context.Background(), "lock table events in access exclusive mode"); err != nil {
			t.Fatalf("lock events: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		suggestions, wire := fill(ctx)
		if len(suggestions.Related) != 2 {
			t.Fatalf("related = %+v, want both asks listed", suggestions.Related)
		}
		for _, item := range suggestions.Related {
			if item.Ask != nil {
				t.Fatalf("ask %s state = %+v, want none once the answers could not be read", item.ID, item.Ask)
			}
		}
		for id, item := range wire {
			if _, ok := item["ask"]; ok {
				t.Fatalf("related item %s = %v, want no ask key", id, item)
			}
		}
		if suggestions.Decision != nil {
			t.Fatalf("decision = %+v, want none once the ask states could not be read", suggestions.Decision)
		}
	})
}

// An issue matched through its title and through its spec fills one place, so the other places go
// to other owners.
func TestRelatedCheckListsOneItemPerOwner(t *testing.T) {
	handler, _, deps := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "OWNER")
	key := createSearchIssue(t, handler, "OWNER", "Zymurgy ledger drifts", "The zymurgy ledger drifts every quarter.")

	suggestions := directServer(deps).relatedCheck(context.Background(), relatedInput{project: "OWNER", text: "zymurgy ledger drift"})
	if keys := relatedOwnerKeys(suggestions.Related); !slices.Equal(keys, []string{key}) {
		t.Fatalf("related = %+v, want %s once, though its title and its spec both match", suggestions.Related, key)
	}
}

// Every issue and ask check is recorded with how it went: a found, a none and a not-made one.
func TestRelatedCheckRecordsEachCheck(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "REC")
	existing := createSearchIssue(t, handler, "REC", "Zymurgy ledger drifts", "")

	createRecorded := func(title string) (string, testSuggestions) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "REC", "title": title, "force": true}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[testIssueWithSuggestions](t, response).Key, issueSuggestions(t, title, response)
	}
	foundKey, found := createRecorded("Zymurgy ledger totals are wrong")
	noneKey, none := createRecorded("Quokka kiosk flickers")
	if found.Outcome != "found" || none.Outcome != "none" {
		t.Fatalf("outcomes = %q and %q, want found and none", found.Outcome, none.Outcome)
	}

	asked := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+existing+"/asks", map[string]any{"question": "Is it to be or not to be?"}, "alice")
	if asked.Code != http.StatusCreated {
		t.Fatalf("open the stopword ask: status=%d body=%s", asked.Code, asked.Body.String())
	}
	askID := decodeBody[testAskWithSuggestions](t, asked).ID
	if notMade := issueSuggestions(t, "the stopword ask", asked); notMade.Outcome != "not_made" {
		t.Fatalf("stopword ask suggestions = %+v, want not made", notMade)
	}

	foundRows := readRelatedCheckRows(t, database, "source_issue_key = $1 and source_kind = 'issue'", foundKey)
	if len(foundRows) != 1 {
		t.Fatalf("related_checks rows for %s = %+v, want one", foundKey, foundRows)
	}
	row := foundRows[0]
	if row.Project != "REC" || row.ActorKind != "user" || row.ActorID != "alice" || row.Outcome != "found" ||
		deref(row.Search) != "keywords_only" || deref(row.Reason) != "no_embedder" || !slices.Contains(row.Terms, "zymurgi") ||
		row.KeywordMS == nil || row.MeaningMS != nil || row.TotalMS < *row.KeywordMS || row.RelatedCount != len(found.Related) {
		t.Fatalf("found row = %+v (search %q, reason %q), want a found keywords-only check of zymurgi", row, deref(row.Search), deref(row.Reason))
	}

	noneRows := readRelatedCheckRows(t, database, "source_issue_key = $1 and source_kind = 'issue'", noneKey)
	if len(noneRows) != 1 {
		t.Fatalf("related_checks rows for %s = %+v, want one", noneKey, noneRows)
	}
	row = noneRows[0]
	if row.Outcome != "none" || deref(row.Search) != "keywords_only" || deref(row.Reason) != "no_embedder" ||
		len(row.Terms) != 0 || row.KeywordMS == nil || row.RelatedCount != 0 {
		t.Fatalf("none row = %+v (search %v, reason %v), want a keywords-only check that found nothing", row, deref(row.Search), deref(row.Reason))
	}

	askRows := readRelatedCheckRows(t, database, "source_ask_id = $1", askID)
	if len(askRows) != 1 {
		t.Fatalf("related_checks rows for the ask %s = %+v, want one", askID, askRows)
	}
	row = askRows[0]
	if row.SourceKind != "ask" || deref(row.SourceIssue) != existing || row.Outcome != "not_made" || row.Search != nil ||
		deref(row.Reason) != "blank_text" || len(row.Terms) != 0 || row.KeywordMS != nil || row.RelatedCount != 0 {
		t.Fatalf("ask row = %+v (search %v, reason %v), want a not-made blank-text ask check on %s", row, deref(row.Search), deref(row.Reason), existing)
	}
}

// A create naming a GitHub issue Dispatch already holds creates nothing, so it makes no check, and
// says so; the check is recorded as not made.
func TestCreateExistingExternalIssueAnswersNoCheckWasMade(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createSuggestionProject(t, handler, "EXT")
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "EXT",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("map external repository: status=%d body=%s", response.Code, response.Body.String())
	}
	first := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{"external": "owner/repo#12"}, "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: status=%d body=%s", first.Code, first.Body.String())
	}
	key := decodeBody[model.Issue](t, first).Key
	again := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{"external": "owner/repo#12"}, "alice")
	if again.Code != http.StatusOK {
		t.Fatalf("second create: status=%d body=%s", again.Code, again.Body.String())
	}
	suggestions := issueSuggestions(t, "the existing external issue", again)
	if suggestions.Outcome != "not_made" || suggestions.Missing != "Dispatch already holds this issue; nothing was created" ||
		len(suggestions.Related) != 0 {
		t.Fatalf("suggestions = %+v, want not made because Dispatch already holds the issue", suggestions)
	}
	if rows := readRelatedCheckRows(t, database, "source_issue_key = $1 and reason = 'already_exists'", key); len(rows) != 1 || rows[0].Outcome != "not_made" {
		t.Fatalf("already_exists rows for %s = %+v, want one not-made check", key, rows)
	}
}
