package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The tests in this file measure the related-item check (INFRA-568) on a corpus copied from a
// Dispatch server by scripts/related-recall.sh. They are env-gated so CI never runs them, and they
// set no bound by default: they print what they measured, which is quoted on the pull request.

// recallRecord is one line of the corpus file: an issue (record "issue") or a duplicate pair
// (record "pair"). Times are the server's.
type recallRecord struct {
	Record    string    `json:"record"`
	Key       string    `json:"key"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Spec      string    `json:"spec"`
	// SpecVersions holds the versions a pair needs besides the latest: a duplicate's first, a
	// survivor's as of each of its duplicates' filing.
	SpecVersions []struct {
		Number    int       `json:"number"`
		CreatedAt time.Time `json:"created_at"`
		Markdown  string    `json:"markdown"`
	} `json:"spec_versions"`
	// TitleVersions is a pair's issue's titles over time, present only when it changed.
	TitleVersions []struct {
		CreatedAt time.Time `json:"created_at"`
		Title     string    `json:"title"`
	} `json:"title_versions"`

	Duplicate          string    `json:"duplicate"`
	Survivor           string    `json:"survivor"`
	DuplicateCreatedAt time.Time `json:"duplicate_created_at"`
}

type recallCorpus struct {
	project string
	issues  map[string]recallRecord
	pairs   []recallRecord
}

func readRecallCorpus(t *testing.T, path string) recallCorpus {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the recall corpus: %v", err)
	}
	defer file.Close()
	corpus := recallCorpus{issues: map[string]recallRecord{}}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		var record recallRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode a recall corpus line: %v", err)
		}
		switch record.Record {
		case "issue":
			corpus.issues[record.Key] = record
			corpus.project = record.Key[:strings.LastIndex(record.Key, "-")]
		case "pair":
			corpus.pairs = append(corpus.pairs, record)
		default:
			t.Fatalf("recall corpus record %q is neither an issue nor a pair", record.Record)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read the recall corpus: %v", err)
	}
	if len(corpus.issues) == 0 {
		t.Fatal("the recall corpus holds no issue")
	}
	return corpus
}

// specAt is the issue's spec as it stood at the given time: the latest copied version created by
// then, or none.
func (corpus recallCorpus) specAt(key string, at time.Time) string {
	spec := ""
	for _, version := range corpus.issues[key].SpecVersions {
		if !version.CreatedAt.After(at) {
			spec = version.Markdown
		}
	}
	return spec
}

// titleAt is the issue's title as it stood at the given time; an issue whose title never changed
// has its title.
func (corpus recallCorpus) titleAt(key string, at time.Time) string {
	issue := corpus.issues[key]
	title := issue.Title
	for index, version := range issue.TitleVersions {
		if index == 0 || !version.CreatedAt.After(at) {
			title = version.Title
		}
	}
	return title
}

// source is the text the check reads for a pair's duplicate at its filing: its title and its first
// spec version, as an issue create joins them.
func (corpus recallCorpus) source(pair recallRecord) string {
	duplicate := corpus.issues[pair.Duplicate]
	first := ""
	for _, version := range duplicate.SpecVersions {
		if version.Number == 1 {
			first = version.Markdown
		}
	}
	return corpus.titleAt(pair.Duplicate, duplicate.CreatedAt) + "\n" + first
}

func issueNumber(t *testing.T, key string) int {
	t.Helper()
	number, err := strconv.Atoi(key[strings.LastIndex(key, "-")+1:])
	if err != nil {
		t.Fatalf("issue key %q ends in no number: %v", key, err)
	}
	return number
}

// loadRecallCorpus creates the corpus's project and issues in a fresh database under their own
// keys (a text naming an issue key matches that issue, as on the server it came from), with
// their server's creation and update times, every one open, so the open-before-done order
// favours neither query. A survivor carries its title and spec as of its earliest duplicate's
// filing; every other issue its latest. A duplicate is the check's source, not a competitor, so
// it is not created unless it is also a survivor (a chain), in which case its own items are left
// out when it is the source. Then it refreshes the project's term statistics, as the leased pass
// would have, and answers how long that took.
func loadRecallCorpus(t *testing.T, corpus recallCorpus) (*server, *store.Store, time.Duration) {
	t.Helper()
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, corpus.project)

	survivorAt := map[string]time.Time{}
	duplicates := map[string]bool{}
	for _, pair := range corpus.pairs {
		duplicates[pair.Duplicate] = true
		if at, ok := survivorAt[pair.Survivor]; !ok || pair.DuplicateCreatedAt.Before(at) {
			survivorAt[pair.Survivor] = pair.DuplicateCreatedAt
		}
	}
	keys := make([]string, 0, len(corpus.issues))
	for key := range corpus.issues {
		if _, survivor := survivorAt[key]; survivor || !duplicates[key] {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b string) int { return issueNumber(t, a) - issueNumber(t, b) })

	for _, key := range keys {
		issue := corpus.issues[key]
		title, spec := issue.Title, issue.Spec
		if at, survivor := survivorAt[key]; survivor {
			title, spec = corpus.titleAt(key, at), corpus.specAt(key, at)
		}
		if _, err := database.Pool.Exec(ctx, `update projects set next_number = $2 where key = $1`,
			corpus.project, issueNumber(t, key)); err != nil {
			t.Fatalf("number %s: %v", key, err)
		}
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": corpus.project, "title": title, "spec": spec, "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
		if created := decodeBody[model.Issue](t, response).Key; created != key {
			t.Fatalf("created %s, want %s", created, key)
		}
		if _, err := database.Pool.Exec(ctx, `update issues set created_at = $2, updated_at = $3 where key = $1`,
			key, issue.CreatedAt, issue.UpdatedAt); err != nil {
			t.Fatalf("date %s: %v", key, err)
		}
	}

	started := time.Now()
	items, err := refreshSearchTermStats(ctx, database.Pool, corpus.project)
	if err != nil {
		t.Fatalf("refresh the term statistics: %v", err)
	}
	refreshed := time.Since(started)
	t.Logf("loaded %d issues of %d (%d searchable items); refresh took %v", len(keys), len(corpus.issues), items, refreshed)
	return srv, database, refreshed
}

// recallOldQuery is the check's query before INFRA-568, suggestionQuery as it stood at 08c345f7,
// kept here only to measure the new keyword search against: up to 40 distinct words of the text
// joined with OR for websearch_to_tsquery.
func recallOldQuery(text string) string {
	const wordLimit = 40
	seen := map[string]bool{}
	terms := make([]string, 0, wordLimit)
	for _, word := range strings.Fields(text) {
		word = strings.ToLower(strings.Trim(word, ".,!?;:\"'()[]{}*_`"))
		if word == "" || word == "or" || word == "and" || seen[word] {
			continue
		}
		seen[word] = true
		terms = append(terms, word)
		if len(terms) >= wordLimit {
			break
		}
	}
	return strings.Join(terms, " OR ")
}

// recallOldSearch runs the old query as the check ran it then (runFusedSearch at 08c345f7):
// nothing for a text with no word or a query of stopwords alone, else search's own keyword query
// (searchQOpener + searchQueryBody), here with no time limit.
func recallOldSearch(ctx context.Context, srv *server, project, text string) ([]model.SearchResult, error) {
	query := recallOldQuery(text)
	if query == "" {
		return nil, nil
	}
	var nodes int
	if err := srv.deps.Store.Pool.QueryRow(ctx, "select numnode(websearch_to_tsquery('english', $1))", query).Scan(&nodes); err != nil {
		return nil, err
	}
	if nodes == 0 {
		return nil, nil
	}
	response, err := srv.runFusedSearchQuery(ctx, query, project, suggestionSearchDepth, 0)
	return response.Results, err
}

// recallNewSearch is the check's stage 1 (runRelatedCheck): the text's rarest words, then search's
// keyword body over them; it answers the terms it chose too.
func recallNewSearch(ctx context.Context, srv *server, project, text, exclude string) ([]model.SearchResult, relatedTerms, error) {
	picked, err := srv.pickRelatedTerms(ctx, srv.deps.Store.Pool, project, text, relatedExclusion{issueKey: exclude})
	if err != nil || picked.NoLexemes || len(picked.Terms) == 0 {
		return nil, picked, err
	}
	response, err := srv.relatedSearch(ctx, srv.deps.Store.Pool, relatedKeywordQuery, project, picked)
	return response.Results, picked, err
}

// recallListed is the owners of the check's related items for results, in order: an issue's key,
// or a project document's id. Both queries' results go through the check's own post-processing
// (fillRelatedItems): the duplicate's own items left out, one item per owner, open owners first,
// the best three.
func recallListed(ctx context.Context, srv *server, project, duplicate string, results []model.SearchResult) []string {
	suggestions := &model.Suggestions{}
	srv.fillRelatedItems(ctx, relatedInput{project: project, source: suggestionSource{kind: "issue", issueKey: duplicate}}, results, suggestions)
	listed := make([]string, 0, len(suggestions.Related))
	for _, item := range suggestions.Related {
		if key := suggestedIssueKey(item); key != nil {
			listed = append(listed, *key)
		} else {
			listed = append(listed, item.Owner.ArtifactID)
		}
	}
	return listed
}

// TestRelatedRecallOnDuplicates is INFRA-568's acceptance 3: for each issue closed as a duplicate
// of another, does the related-item check list the issue it was closed into, searching with the
// duplicate's own text against every other issue of its project? It runs the old 40-word OR
// query and the new keyword search under identical post-processing and prints each pair's result
// and both totals. DISPATCH_RECALL_REQUIRE_GAIN=1 fails it unless the new count is the higher.
func TestRelatedRecallOnDuplicates(t *testing.T) {
	path := os.Getenv("DISPATCH_RECALL_CORPUS")
	if path == "" {
		t.Skip("DISPATCH_RECALL_CORPUS must name a file scripts/related-recall.sh wrote")
	}
	corpus := readRecallCorpus(t, path)
	if len(corpus.pairs) == 0 {
		t.Fatal("the recall corpus holds no duplicate pair")
	}
	srv, _, _ := loadRecallCorpus(t, corpus)
	ctx := context.Background()

	found := func(listed []string, survivor string) string {
		if rank := slices.Index(listed, survivor); rank >= 0 {
			return fmt.Sprintf("listed at %d of %v", rank+1, listed)
		}
		return fmt.Sprintf("not listed in %v", listed)
	}
	oldCount, newCount := 0, 0
	for _, pair := range corpus.pairs {
		text := corpus.source(pair)
		oldResults, err := recallOldSearch(ctx, srv, corpus.project, text)
		if err != nil {
			t.Fatalf("old query for %s: %v", pair.Duplicate, err)
		}
		newResults, picked, err := recallNewSearch(ctx, srv, corpus.project, text, pair.Duplicate)
		if err != nil {
			t.Fatalf("new keyword search for %s: %v", pair.Duplicate, err)
		}
		oldListed := recallListed(ctx, srv, corpus.project, pair.Duplicate, oldResults)
		newListed := recallListed(ctx, srv, corpus.project, pair.Duplicate, newResults)
		if slices.Contains(oldListed, pair.Survivor) {
			oldCount++
		}
		if slices.Contains(newListed, pair.Survivor) {
			newCount++
		}
		t.Logf("%s closed into %s: old query %s; new keyword search %s (%d terms)",
			pair.Duplicate, pair.Survivor, found(oldListed, pair.Survivor), found(newListed, pair.Survivor), len(picked.Terms))
	}
	t.Logf("survivor among the three related items: old 40-word OR query %d of %d, new keyword search %d of %d",
		oldCount, len(corpus.pairs), newCount, len(corpus.pairs))
	if os.Getenv("DISPATCH_RECALL_REQUIRE_GAIN") == "1" && newCount <= oldCount {
		t.Fatalf("new keyword search lists %d survivors, the old query %d: no gain", newCount, oldCount)
	}
}

// TestRelatedCheckLatencyOnRecallCorpus times the check's stage 1 (term pick and keyword query)
// on the recall corpus, loaded as TestRelatedRecallOnDuplicates loads it, for one issue's own
// text (DISPATCH_RECALL_LATENCY_ISSUE names it) and for a 10,000-word spec built from the
// corpus's words: p50, p95 and max over 20 runs each, after one untimed run. It also times five
// more refreshes of the project's term statistics.
func TestRelatedCheckLatencyOnRecallCorpus(t *testing.T) {
	path := os.Getenv("DISPATCH_RECALL_CORPUS")
	if path == "" {
		t.Skip("DISPATCH_RECALL_CORPUS must name a file scripts/related-recall.sh wrote")
	}
	timed := os.Getenv("DISPATCH_RECALL_LATENCY_ISSUE")
	if timed == "" {
		t.Skip("DISPATCH_RECALL_LATENCY_ISSUE must name the corpus issue whose text is timed")
	}
	corpus := readRecallCorpus(t, path)
	issue, ok := corpus.issues[timed]
	if !ok {
		t.Fatalf("the recall corpus holds no issue %s", timed)
	}
	srv, database, refreshed := loadRecallCorpus(t, corpus)
	ctx := context.Background()

	refreshes := []time.Duration{refreshed}
	for range 5 {
		started := time.Now()
		if _, err := refreshSearchTermStats(ctx, database.Pool, corpus.project); err != nil {
			t.Fatalf("refresh the term statistics: %v", err)
		}
		refreshes = append(refreshes, time.Since(started))
	}
	t.Logf("refresh of %s's term statistics: %v", corpus.project, refreshes)

	// The corpus's words in the order they first appear, repeated until there are 10,000.
	var vocabulary []string
	seen := map[string]bool{}
	keys := make([]string, 0, len(corpus.issues))
	for key := range corpus.issues {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b string) int { return issueNumber(t, a) - issueNumber(t, b) })
	for _, key := range keys {
		for _, word := range strings.Fields(corpus.issues[key].Title + " " + corpus.issues[key].Spec) {
			if !seen[word] {
				seen[word] = true
				vocabulary = append(vocabulary, word)
			}
		}
	}
	var long strings.Builder
	for index := range 10000 {
		switch {
		case index == 0:
		case index%20 == 0:
			long.WriteString("\n")
		default:
			long.WriteString(" ")
		}
		long.WriteString(vocabulary[index%len(vocabulary)])
	}

	for _, run := range []struct {
		name, text, exclude string
	}{
		{timed + "'s title and spec", issue.Title + "\n" + issue.Spec, timed},
		{"a 10,000-word spec", long.String(), corpus.project + "-0"},
	} {
		results, picked, err := recallNewSearch(ctx, srv, corpus.project, run.text, run.exclude)
		if err != nil {
			t.Fatalf("stage 1 for %s: %v", run.name, err)
		}
		const rounds = 20
		latencies := make([]time.Duration, 0, rounds)
		for range rounds {
			started := time.Now()
			if _, _, err := recallNewSearch(ctx, srv, corpus.project, run.text, run.exclude); err != nil {
				t.Fatalf("stage 1 for %s: %v", run.name, err)
			}
			latencies = append(latencies, time.Since(started))
		}
		slices.Sort(latencies)
		t.Logf("stage 1 for %s (%d words, %d terms, %d results) over %d runs: p50=%v p95=%v max=%v",
			run.name, len(strings.Fields(run.text)), len(picked.Terms), len(results), rounds,
			latencies[(rounds-1)/2], latencies[(rounds*95+99)/100-1], latencies[rounds-1])
	}
}
