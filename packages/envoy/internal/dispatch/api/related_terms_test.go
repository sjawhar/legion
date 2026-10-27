package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// relatedTermsServer is a server whose term statistics and picker a test drives directly, with
// the handler that seeds its items.
func relatedTermsServer(t *testing.T) (http.Handler, *store.Store, *server) {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	return handler, database, directServer(deps)
}

func refreshTermStats(t *testing.T, database *store.Store, project string) int {
	t.Helper()
	items, err := refreshSearchTermStats(context.Background(), database.Pool, project)
	if err != nil {
		t.Fatalf("refresh %s's search term statistics: %v", project, err)
	}
	return items
}

func pickTerms(t *testing.T, srv *server, project, text string, exclude relatedExclusion) relatedTerms {
	t.Helper()
	picked, err := srv.pickRelatedTerms(context.Background(), srv.deps.Store.Pool, project, text, exclude)
	if err != nil {
		t.Fatalf("pick related terms: %v", err)
	}
	return picked
}

func termLexemes(picked relatedTerms) []string {
	lexemes := make([]string, len(picked.Terms))
	for index, term := range picked.Terms {
		lexemes[index] = term.Lexeme
	}
	return lexemes
}

func pickedTerm(picked relatedTerms, lexeme string) (relatedTerm, bool) {
	for _, term := range picked.Terms {
		if term.Lexeme == lexeme {
			return term, true
		}
	}
	return relatedTerm{}, false
}

func addIssueComment(t *testing.T, handler http.Handler, issueKey, body string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", map[string]any{"body": body}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("comment on %s: status=%d body=%s", issueKey, response.Code, response.Body.String())
	}
}

// A 10,000-word spec offers far more distinctive words than the keyword query may OR together;
// the picker keeps relatedTermLimit of them.
func TestRelatedTermsCapAt25(t *testing.T) {
	handler, database, srv := relatedTermsServer(t)
	createSuggestionProject(t, handler, "CAP")
	var rare []string
	for index := range 30 {
		word := fmt.Sprintf("zorb%c%cvex", 'a'+index/26, 'a'+index%26)
		rare = append(rare, word)
		createSearchIssue(t, handler, "CAP", "Calibrate the "+word, "")
	}
	refreshTermStats(t, database, "CAP")

	words := append([]string{}, rare...)
	filler := []string{"lorem", "ipsum", "dolor", "amet", "consectetur", "adipiscing", "elit", "tempor", "incididunt", "labore"}
	for index := 0; len(words) < 10000; index++ {
		words = append(words, filler[index%len(filler)])
	}
	picked := pickTerms(t, srv, "CAP", strings.Join(words, " "), relatedExclusion{})
	if len(picked.Terms) != relatedTermLimit {
		t.Fatalf("picked %d terms %v, want %d of the 30 rare words", len(picked.Terms), termLexemes(picked), relatedTermLimit)
	}
	if got := strings.Count(picked.TSQuery, " | ") + 1; got != relatedTermLimit {
		t.Fatalf("tsquery %q holds %d lexemes, want %d", picked.TSQuery, got, relatedTermLimit)
	}
}

// A word in more than a tenth of the project's items, where a tenth is past relatedCommonFloor,
// is what made the old query match thousands of rows; the picker leaves it out and keeps the rare
// words beside it.
func TestRelatedTermsDropAWordInMoreThanATenthOfTheProject(t *testing.T) {
	handler, database, srv := relatedTermsServer(t)
	createSuggestionProject(t, handler, "COMMON")
	for index := range 110 {
		createSearchIssue(t, handler, "COMMON", fmt.Sprintf("Widget fixture %d", index), "")
	}
	for _, word := range []string{"zephyr", "quokka", "marimba", "obsidian"} {
		createSearchIssue(t, handler, "COMMON", "The "+word+" ledger", "")
	}
	items := refreshTermStats(t, database, "COMMON")
	if items/10 <= relatedCommonFloor {
		t.Fatalf("the project has %d items, too few for a tenth of it to pass %d", items, relatedCommonFloor)
	}

	picked := pickTerms(t, srv, "COMMON", "widget zephyr quokka marimba obsidian", relatedExclusion{})
	if _, ok := pickedTerm(picked, "widget"); ok {
		t.Fatalf("picked %v, want no 'widget': it is in 110 of %d items", termLexemes(picked), items)
	}
	want := []string{"marimba", "obsidian", "quokka", "zephyr"}
	if got := termLexemes(picked); !slices.Equal(got, want) {
		t.Fatalf("picked %v, want the four rare words %v, ordered by lexeme at equal score", got, want)
	}
	if !picked.StatsPresent || picked.Items != items {
		t.Fatalf("stats present=%v items=%d, want true and %d", picked.StatsPresent, picked.Items, items)
	}
}

// Numbers, versions, dates and hashes match nothing related, so the picker never chooses them,
// even where they are the rarest lexemes of the text.
func TestRelatedTermsDropNumbersAndHexRuns(t *testing.T) {
	handler, database, srv := relatedTermsServer(t)
	createSuggestionProject(t, handler, "NOISE")
	title := "Rollout 20261027 broke at deadbeefcafe1234 on 1.2.3"
	createSearchIssue(t, handler, "NOISE", title, "")
	refreshTermStats(t, database, "NOISE")

	picked := pickTerms(t, srv, "NOISE", title, relatedExclusion{})
	for _, noise := range []string{"20261027", "deadbeefcafe1234", "1.2.3"} {
		if _, ok := pickedTerm(picked, noise); ok {
			t.Fatalf("picked %v, want no %q", termLexemes(picked), noise)
		}
	}
	if _, ok := pickedTerm(picked, "rollout"); !ok {
		t.Fatalf("picked %v, want 'rollout' beside the dropped numbers", termLexemes(picked))
	}
}

// The source's own items count toward no df: a word only the source holds, which the statistics
// counted on its issue, its spec and its comment, is never picked, while it is for a check with
// nothing excluded.
func TestRelatedTermsLeaveOutAWordOnlyTheExcludedSourceHolds(t *testing.T) {
	handler, database, srv := relatedTermsServer(t)
	createSuggestionProject(t, handler, "OWN")
	source := createSearchIssue(t, handler, "OWN", "Quixotic rollout plan", "The quixotic part.")
	addIssueComment(t, handler, source, "Still quixotic.")
	createSearchIssue(t, handler, "OWN", "Gizmo inventory", "")
	refreshTermStats(t, database, "OWN")

	unexcluded := pickTerms(t, srv, "OWN", "quixotic gizmo", relatedExclusion{})
	if term, ok := pickedTerm(unexcluded, "quixot"); !ok || term.DF != 3 {
		t.Fatalf("with nothing excluded picked %+v, want 'quixot' at df 3 (issue, spec, comment)", unexcluded.Terms)
	}
	excluded := pickTerms(t, srv, "OWN", "quixotic gizmo", relatedExclusion{issueKey: source})
	if got := termLexemes(excluded); !slices.Equal(got, []string{"gizmo"}) {
		t.Fatalf("excluding %s picked %v, want only 'gizmo'", source, got)
	}
}

// A lexeme written since the last refresh has no statistics row; the picker probes it live,
// leaving out the excluded source's items, and a hit counts as df 1.
func TestRelatedTermsProbeLexemesWrittenSinceTheRefresh(t *testing.T) {
	handler, database, srv := relatedTermsServer(t)
	createSuggestionProject(t, handler, "PROBE")
	createSearchIssue(t, handler, "PROBE", "Harbour crane schedule", "")
	refreshTermStats(t, database, "PROBE")
	createSearchIssue(t, handler, "PROBE", "Flamingo enclosure", "")
	document := createProjectDocument(t, handler, "PROBE", "Island notes", "# Notes\nZanzibar ferries.\n")

	picked := pickTerms(t, srv, "PROBE", "flamingo pelican zanzibar crane", relatedExclusion{})
	for _, lexeme := range []string{"flamingo", "zanzibar"} {
		if term, ok := pickedTerm(picked, lexeme); !ok || term.DF != 1 {
			t.Fatalf("picked %+v, want %q probed at df 1", picked.Terms, lexeme)
		}
	}
	if _, ok := pickedTerm(picked, "pelican"); ok {
		t.Fatalf("picked %v, want no 'pelican': no item holds it", termLexemes(picked))
	}
	if !picked.StatsPresent {
		t.Fatal("stats present = false, want true: the project was refreshed")
	}

	excluded := pickTerms(t, srv, "PROBE", "flamingo pelican zanzibar crane", relatedExclusion{artifactID: document.ID})
	if _, ok := pickedTerm(excluded, "zanzibar"); ok {
		t.Fatalf("excluding the document picked %v, want no 'zanzibar': only it holds the word", termLexemes(excluded))
	}
}

// A project with no statistics is picked in probe mode, and the pick starts its refresh.
func TestRelatedTermsProbeAProjectWithNoStatisticsAndStartItsRefresh(t *testing.T) {
	handler, database, srv := relatedTermsServer(t)
	createSuggestionProject(t, handler, "FRESH")
	createSearchIssue(t, handler, "FRESH", "Lighthouse lens", "")

	picked := pickTerms(t, srv, "FRESH", "lighthouse beacon", relatedExclusion{})
	if picked.StatsPresent || picked.Items != 0 {
		t.Fatalf("stats present=%v items=%d, want probe mode", picked.StatsPresent, picked.Items)
	}
	if term, ok := pickedTerm(picked, "lighthous"); !ok || term.DF != 1 || len(picked.Terms) != 1 {
		t.Fatalf("picked %+v, want only 'lighthous' probed at df 1", picked.Terms)
	}
	select {
	case <-srv.ensureSearchTermStats("FRESH"):
	case <-time.After(30 * time.Second):
		t.Fatal("the refresh the pick started did not end")
	}
	var items int
	if err := database.Pool.QueryRow(context.Background(), `select items from search_term_totals where project_key = 'FRESH'`).Scan(&items); err != nil {
		t.Fatalf("read the refreshed totals: %v", err)
	}
	if items == 0 {
		t.Fatal("the refreshed totals count no items")
	}
}

// The pick stops adding terms once their summed df would pass relatedMatchBudget, which bounds
// the rows the keyword query matches.
func TestRelatedTermsStopAtTheMatchBudget(t *testing.T) {
	handler, database, srv := relatedTermsServer(t)
	createSuggestionProject(t, handler, "BUDGET")
	// Statistics of a project far larger than a test seeds: 100,000 items, so no df below is too
	// common, and three lexemes whose df sum past the budget.
	if _, err := database.Pool.Exec(context.Background(), `
		insert into search_term_totals (project_key, items, refreshed_at) values ('BUDGET', 100000, now());
		insert into search_term_stats (project_key, lexeme, ndoc) values
		  ('BUDGET', 'alpha', 2000), ('BUDGET', 'bravo', 1500), ('BUDGET', 'charli', 100)`); err != nil {
		t.Fatalf("seed statistics: %v", err)
	}

	picked := pickTerms(t, srv, "BUDGET", "alpha bravo charlie", relatedExclusion{})
	if got := termLexemes(picked); !slices.Equal(got, []string{"charli", "bravo"}) {
		t.Fatalf("picked %v, want charli and bravo (df 1,600) and not alpha, which would make 3,600", got)
	}
	if picked.Terms[0].Surface != "charlie" {
		t.Fatalf("charli's surface = %q, want the text's own 'charlie'", picked.Terms[0].Surface)
	}
}

// The refresh reads each document at its latest version only, as search does, and counts it once.
func TestRelatedTermsRefreshCountsADocumentOnceAtItsLatestVersion(t *testing.T) {
	handler, database, _ := relatedTermsServer(t)
	issue := createInteractionIssue(t, handler, "VERS", "Instruments", "The astrolabe measures altitude.\n")
	edit := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":   []map[string]string{{"op": "replace", "find": "astrolabe", "with": "sextant"}},
		"actor": sessionActor(),
	})
	if edit.Code != http.StatusOK {
		t.Fatalf("edit document: status=%d body=%s", edit.Code, edit.Body.String())
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var versions int
		if err := database.Pool.QueryRow(context.Background(), "select count(*) from artifact_versions where artifact_id = $1", issue.PrimaryArtifactID).Scan(&versions); err != nil {
			t.Fatalf("count artifact versions: %v", err)
		}
		if versions >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiting for the second artifact version")
		}
	}

	if items := refreshTermStats(t, database, "VERS"); items != 2 {
		t.Fatalf("items = %d, want 2: the issue and its spec, once", items)
	}
	rows, err := database.Pool.Query(context.Background(), `select lexeme, ndoc from search_term_stats where project_key = 'VERS'`)
	if err != nil {
		t.Fatalf("read statistics: %v", err)
	}
	ndoc := map[string]int{}
	for rows.Next() {
		var lexeme string
		var count int
		if err := rows.Scan(&lexeme, &count); err != nil {
			t.Fatalf("scan statistics: %v", err)
		}
		ndoc[lexeme] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read statistics: %v", err)
	}
	if _, counted := ndoc["astrolab"]; counted {
		t.Fatalf("statistics %v count 'astrolab', which only the older version holds", ndoc)
	}
	if ndoc["sextant"] != 1 || ndoc["altitud"] != 1 {
		t.Fatalf("statistics %v, want 'sextant' and 'altitud' in the one latest version", ndoc)
	}
}

// The pick's tsquery is cast as it stands (relatedQOpener) and, through search's own keyword
// body, finds the items holding the chosen words and none holding only words it left out.
func TestRelatedTermsQueryCastsAndMatchesTheItemsHoldingTheChosenWords(t *testing.T) {
	handler, database, srv := relatedTermsServer(t)
	createSuggestionProject(t, handler, "CAST")
	walrus := createSearchIssue(t, handler, "CAST", "Walrus migration", "")
	tundra := createSearchIssue(t, handler, "CAST", "Tundra survey", "")
	other := createSearchIssue(t, handler, "CAST", "Glacier survey", "")
	refreshTermStats(t, database, "CAST")

	picked := pickTerms(t, srv, "CAST", "Walruses crossing the tundra, 2026", relatedExclusion{})
	if picked.TSQuery != "'walrus' | 'tundra'" && picked.TSQuery != "'tundra' | 'walrus'" {
		t.Fatalf("tsquery = %q, want walrus and tundra quoted and ORed", picked.TSQuery)
	}
	if term, _ := pickedTerm(picked, "walrus"); term.Surface != "Walruses" {
		t.Fatalf("walrus's surface = %q, want the text's own 'Walruses'", term.Surface)
	}
	rows, err := database.Pool.Query(context.Background(), relatedQOpener+searchQueryBody,
		picked.TSQuery, "CAST", searchMaxLimit, "", headlineOptions, 0, contracts.SearchKindDepth, searchFusionK)
	if err != nil {
		t.Fatalf("run the keyword body under the related q: %v", err)
	}
	response, err := scanSearchRows(rows, "", searchMaxLimit, 0)
	rows.Close()
	if err != nil {
		t.Fatalf("scan the keyword results: %v", err)
	}
	found := map[string]bool{}
	for _, result := range response.Results {
		if result.Kind == "issue" {
			found[result.ID] = true
		}
	}
	if !found[walrus] || !found[tundra] || found[other] {
		t.Fatalf("issues found %v, want %s and %s and not %s", found, walrus, tundra, other)
	}
}

// A text with no lexeme, blank or stopwords alone, says so, so the check can answer that it made
// no search.
func TestRelatedTermsSayATextHoldsNoLexeme(t *testing.T) {
	handler, _, srv := relatedTermsServer(t)
	createSuggestionProject(t, handler, "EMPTY")
	for _, text := range []string{"", "   ", "the and of it"} {
		picked := pickTerms(t, srv, "EMPTY", text, relatedExclusion{})
		if !picked.NoLexemes || len(picked.Terms) != 0 {
			t.Fatalf("text %q picked %+v, want no lexemes", text, picked)
		}
	}
}
