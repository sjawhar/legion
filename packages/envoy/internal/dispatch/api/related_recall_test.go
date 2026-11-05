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

// The tests in this file measure the related-item check on a corpus copied from a
// Dispatch server by scripts/related-recall.sh. They are env-gated so CI never runs them, and they
// set no bound by default: they print what they measured, which is quoted on the pull request.

// recallRecord is one line of the corpus file: an issue (record "issue") or a pair of issues, one
// closed into the other (record "pair"). Times are the server's.
type recallRecord struct {
	Record    string    `json:"record"`
	Key       string    `json:"key"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Spec      string    `json:"spec"`
	// SpecVersions holds the versions a pair needs besides the latest: a query's first, a
	// target's as of each of its queries' filing.
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

	// Closed is the done issue whose close reason names Survivor, in Wording ("duplicate of",
	// "folded into" and the rest). Query is the later-filed of the two, searched with as it was
	// filed, and Target the earlier, the issue the check should list: Direction is "forward"
	// when Query is Closed, "reversed" when it is Survivor.
	Closed         string    `json:"closed"`
	Survivor       string    `json:"survivor"`
	Wording        string    `json:"wording"`
	Direction      string    `json:"direction"`
	Query          string    `json:"query"`
	Target         string    `json:"target"`
	QueryCreatedAt time.Time `json:"query_created_at"`
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

// queryTitle is a pair's query's title at its filing, the part of queryText the check reads
// surfaces from.
func (corpus recallCorpus) queryTitle(pair recallRecord) string {
	return corpus.titleAt(pair.Query, corpus.issues[pair.Query].CreatedAt)
}

// queryText is the text the check reads for a pair's query at its filing: its title and its first
// spec version, as an issue create joins them.
func (corpus recallCorpus) queryText(pair recallRecord) string {
	first := ""
	for _, version := range corpus.issues[pair.Query].SpecVersions {
		if version.Number == 1 {
			first = version.Markdown
		}
	}
	return corpus.queryTitle(pair) + "\n" + first
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
// keys (a text naming an issue key matches that issue, as on the server it came from), each with
// its latest title and spec and its server's creation and update times, every one open, so the
// open-before-done order favours neither query. recallStagePair sets a pair's two issues as they
// stood at its query's filing. Then it refreshes the project's term statistics, as the leased pass
// would have, and answers how long that took.
func loadRecallCorpus(t *testing.T, corpus recallCorpus) (*server, *store.Store, time.Duration) {
	t.Helper()
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, corpus.project)

	keys := make([]string, 0, len(corpus.issues))
	for key := range corpus.issues {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b string) int { return issueNumber(t, a) - issueNumber(t, b) })

	for _, key := range keys {
		issue := corpus.issues[key]
		if _, err := database.Pool.Exec(ctx, `update projects set next_number = $2 where key = $1`,
			corpus.project, issueNumber(t, key)); err != nil {
			t.Fatalf("number %s: %v", key, err)
		}
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": corpus.project, "title": issue.Title, "spec": issue.Spec, "force": true,
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
	refresh, err := refreshSearchTermStats(ctx, database.Pool, corpus.project)
	if err != nil {
		t.Fatalf("refresh the term statistics: %v", err)
	}
	refreshed := time.Since(started)
	t.Logf("loaded %d issues (%d searchable items); refresh took %v", len(keys), refresh.Items, refreshed)
	return srv, database, refreshed
}

// recallStagePair sets the database as it stood when a pair's query was filed, as far as the
// check reads it, and answers what puts it back as loadRecallCorpus left it. The target takes its
// title and spec as of that filing, so an issue that is the target of several queries filed at
// different times holds, for each, its text at that query's filing. The query is taken out of
// the search, its issue's and its spec's vectors emptied: it is not a competitor of its own text.
// Every other issue stays as it is now. The term statistics are refreshed after, as each pair's
// picker would have read them over that state.
func recallStagePair(t *testing.T, database *store.Store, corpus recallCorpus, pair recallRecord) (restore func()) {
	t.Helper()
	ctx := context.Background()
	exec := func(what, statement string, arguments ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, statement, arguments...); err != nil {
			t.Fatalf("%s for %s: %v", what, pair.Query, err)
		}
	}
	var title, artifactID, markdown string
	var number int
	if err := database.Pool.QueryRow(ctx, `
		select i.title, v.artifact_id::text, v.number, v.markdown
		  from issues i
		  join artifacts a on a.issue_key = i.key and a.is_primary
		  join lateral (select artifact_id, number, markdown from artifact_versions
		                 where artifact_id = a.id order by number desc limit 1) v on true
		 where i.key = $1`, pair.Target).Scan(&title, &artifactID, &number, &markdown); err != nil {
		t.Fatalf("read %s's title and spec: %v", pair.Target, err)
	}
	const setTitle = `update issues set title = $2 where key = $1`
	const setSpec = `update artifact_versions set markdown = $3 where artifact_id = $1::uuid and number = $2`
	exec("set the target's title", setTitle, pair.Target, corpus.titleAt(pair.Target, pair.QueryCreatedAt))
	exec("set the target's spec", setSpec, artifactID, number, corpus.specAt(pair.Target, pair.QueryCreatedAt))
	// The search triggers fill a vector only when its title or markdown is written, so an emptied
	// vector stays empty until restore writes them back unchanged.
	exec("empty the query's issue vector", `update issues set search = ''::tsvector where key = $1`, pair.Query)
	exec("empty the query's spec vectors", `
		update artifact_versions v set search = ''::tsvector
		  from artifacts a where a.id = v.artifact_id and a.issue_key = $1 and a.is_primary`, pair.Query)
	if _, err := refreshSearchTermStats(ctx, database.Pool, corpus.project); err != nil {
		t.Fatalf("refresh the term statistics for %s: %v", pair.Query, err)
	}
	return func() {
		t.Helper()
		exec("restore the target's title", setTitle, pair.Target, title)
		exec("restore the target's spec", setSpec, artifactID, number, markdown)
		exec("refill the query's issue vector", `update issues set title = title where key = $1`, pair.Query)
		exec("refill the query's spec vectors", `
			update artifact_versions v set markdown = v.markdown
			  from artifacts a where a.id = v.artifact_id and a.issue_key = $1 and a.is_primary`, pair.Query)
	}
}

// recallOldQuery is the check's query before this rebuild, suggestionQuery as it stood at 08c345f7,
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
// (searchQuery), here with no time limit.
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

// recallPick is the check's term selection (runRelatedCheck's stage 1): the text's rarest words.
func recallPick(ctx context.Context, srv *server, project, text, title, exclude string) (relatedTerms, error) {
	return srv.pickRelatedTerms(ctx, srv.deps.Store.Pool, project, text, title, relatedExclusion{issueKey: exclude})
}

// recallNewSearch runs the check's keyword query (relatedKeywordQuery) over the picked terms;
// nothing when no term was picked.
func recallNewSearch(ctx context.Context, srv *server, project string, picked relatedTerms) ([]model.SearchResult, error) {
	if picked.NoLexemes || len(picked.Terms) == 0 {
		return nil, nil
	}
	response, err := srv.relatedSearch(ctx, srv.deps.Store.Pool, relatedKeywordQuery, project, picked)
	return response.Results, err
}

// recallRank is the 1-based position of the target's first item (the issue, its spec or anything
// else it owns) among the search's results, before any post-processing, or 0.
func recallRank(results []model.SearchResult, target string) int {
	for index, result := range results {
		if ownedByIssue(result, target) {
			return index + 1
		}
	}
	return 0
}

// recallListed is the owners of the check's related items for results, in order: an issue's key,
// or a project document's id. Every query's results go through the check's own post-processing
// (fillRelatedItems): the query's own items left out, one item per owner, open owners first, the
// best three.
func recallListed(ctx context.Context, srv *server, project, query string, results []model.SearchResult) []string {
	suggestions := &model.Suggestions{}
	srv.fillRelatedItems(ctx, relatedInput{project: project, source: suggestionSource{kind: "issue", issueKey: query}}, results, suggestions)
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

// TestRelatedRecallOnDuplicates measures the check's recall of duplicates: for each pair of issues one was
// closed into ("Duplicate of", "Folded into" and the other wordings related-recall.sh reads),
// does the related-item check list the earlier-filed issue (the target), searching with the
// later-filed one's text as it was filed (the query) against every other issue of its project?
// The query is the closed issue (forward) or, when the issue it was closed into was filed later,
// that issue (reversed). It runs the old 40-word OR query and the check's keyword query under
// identical post-processing, and prints for each pair its wording and direction, where the target
// ranked among each search's results and whether it was listed, then each query's count per
// wording and over every pair. DISPATCH_RECALL_REQUIRE_NO_LOSS=1 fails it if the check lists
// fewer targets than the old query.
func TestRelatedRecallOnDuplicates(t *testing.T) {
	path := os.Getenv("DISPATCH_RECALL_CORPUS")
	if path == "" {
		t.Skip("DISPATCH_RECALL_CORPUS must name a file scripts/related-recall.sh wrote")
	}
	corpus := readRecallCorpus(t, path)
	if len(corpus.pairs) == 0 {
		t.Fatal("the recall corpus holds no pair")
	}
	srv, database, _ := loadRecallCorpus(t, corpus)
	ctx := context.Background()

	rankings := []string{"old 40-word OR query", "the check's keyword query"}
	// listed[wording][ranking] counts the pairs whose target that ranking's search listed; pairs
	// counts each wording's pairs.
	listed := map[string][]int{}
	pairs := map[string]int{}
	for _, pair := range corpus.pairs {
		restore := recallStagePair(t, database, corpus, pair)
		text := corpus.queryText(pair)
		picked, err := recallPick(ctx, srv, corpus.project, text, corpus.queryTitle(pair), pair.Query)
		if err != nil {
			t.Fatalf("pick terms for %s: %v", pair.Query, err)
		}
		searches := make([][]model.SearchResult, len(rankings))
		if searches[0], err = recallOldSearch(ctx, srv, corpus.project, text); err != nil {
			t.Fatalf("old query for %s: %v", pair.Query, err)
		}
		if searches[1], err = recallNewSearch(ctx, srv, corpus.project, picked); err != nil {
			t.Fatalf("the check's keyword query for %s: %v", pair.Query, err)
		}
		if listed[pair.Wording] == nil {
			listed[pair.Wording] = make([]int, len(rankings))
		}
		pairs[pair.Wording]++
		var report []string
		for index, results := range searches {
			rank := "absent"
			if position := recallRank(results, pair.Target); position > 0 {
				rank = fmt.Sprintf("%d of %d", position, len(results))
			}
			owners := recallListed(ctx, srv, corpus.project, pair.Query, results)
			shown := "not listed"
			if position := slices.Index(owners, pair.Target); position >= 0 {
				shown = fmt.Sprintf("listed %d of %d", position+1, len(owners))
				listed[pair.Wording][index]++
			}
			report = append(report, fmt.Sprintf("%s: ranked %s, %s %v", rankings[index], rank, shown, owners))
		}
		restore()
		t.Logf("%s %q %s, %s: query %s, target %s (%d terms): %s",
			pair.Closed, pair.Wording, pair.Survivor, pair.Direction, pair.Query, pair.Target, len(picked.Terms), strings.Join(report, "; "))
	}

	wordings := make([]string, 0, len(pairs))
	for wording := range pairs {
		wordings = append(wordings, wording)
	}
	slices.Sort(wordings)
	total := make([]int, len(rankings))
	for _, wording := range wordings {
		for index := range rankings {
			total[index] += listed[wording][index]
		}
	}
	totals := func(group string, counts []int, of int) {
		parts := make([]string, len(rankings))
		for index, name := range rankings {
			parts[index] = fmt.Sprintf("%s %d of %d", name, counts[index], of)
		}
		t.Logf("target among the three related items, %s: %s", group, strings.Join(parts, ", "))
	}
	for _, wording := range wordings {
		totals(fmt.Sprintf("%q", wording), listed[wording], pairs[wording])
	}
	totals("every pair", total, len(corpus.pairs))
	if os.Getenv("DISPATCH_RECALL_REQUIRE_NO_LOSS") == "1" && total[1] < total[0] {
		t.Fatalf("the check's ranking lists %d targets, the old query %d", total[1], total[0])
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

	// The long spec's title is its first line, as a filer's title is the first thing they write.
	title, _, _ := strings.Cut(long.String(), "\n")
	for _, run := range []struct {
		name, text, title, exclude string
	}{
		{timed + "'s title and spec", issue.Title + "\n" + issue.Spec, issue.Title, timed},
		{"a 10,000-word spec", long.String(), title, corpus.project + "-0"},
	} {
		stageOne := func() ([]model.SearchResult, relatedTerms, error) {
			picked, err := recallPick(ctx, srv, corpus.project, run.text, run.title, run.exclude)
			if err != nil {
				return nil, picked, err
			}
			results, err := recallNewSearch(ctx, srv, corpus.project, picked)
			return results, picked, err
		}
		results, picked, err := stageOne()
		if err != nil {
			t.Fatalf("stage 1 for %s: %v", run.name, err)
		}
		const rounds = 20
		latencies := make([]time.Duration, 0, rounds)
		for range rounds {
			started := time.Now()
			if _, _, err := stageOne(); err != nil {
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
