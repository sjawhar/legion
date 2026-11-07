package api

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestNearCopyRefusalReplay replays a project's history against the near-copy gate, to set and to
// keep the two bars in duplicates.go. It is env-gated, so CI never runs it.
//
// The corpus is what scripts/near-copy-replay.sh exported, and it is frozen: a second export
// holds more issues and gives another false-refusal rate, which would no longer match the
// threshold in code. The corpus this tree calibrated on is attached to dispatch://INFRA-1160.
//
// DISPATCH_NEARCOPY_CORPUS   the corpus file (required; without it the test skips)
// DISPATCH_TEST_DATABASE_URL a Postgres of the run's own (required by every store test)
// DISPATCH_NEARCOPY_DUPLICATES
//
//	the comma-separated keys of the replayed creates the implementer marked real duplicates,
//	which the 5% ceiling leaves out. The pairs the corpus names are always marked.
//
// The project's term statistics are refreshed on the corpus's own clock, every
// SearchTermStatsInterval of replayed time, which is the cadence a live server keeps
// (RunSearchTermStatsRefresh). Refreshing by a count of creates instead lets the statistics lag
// a create meets in production: INFRA's window holds about one create every twenty minutes,
// so a create there meets current statistics, and a replay that lags refuses a pair the
// current statistics do not (dispatch://INFRA-1160 test handoff version 1, k4).

// nearCopyRecord is one line of the corpus: an issue, a query (a create to replay) or a pair.
type nearCopyRecord struct {
	Record        string            `json:"record"`
	Key           string            `json:"key"`
	CreatedAt     time.Time         `json:"created_at"`
	SpecArtifact  string            `json:"spec_artifact"`
	StatusHistory []nearCopyStatus  `json:"status_history"`
	ParentHistory []nearCopyParent  `json:"parent_history"`
	TitleHistory  []nearCopyTitle   `json:"title_history"`
	Ancestors     []string          `json:"ancestors"`
	SpecVersions  []nearCopyVersion `json:"spec_versions"`
	Query         string            `json:"query"`
	Target        string            `json:"target"`
}

type nearCopyStatus struct {
	At     time.Time `json:"at"`
	Status string    `json:"status"`
}

type nearCopyParent struct {
	At     time.Time `json:"at"`
	Parent *string   `json:"parent"`
}

type nearCopyTitle struct {
	At    time.Time `json:"at"`
	Title string    `json:"title"`
}

type nearCopyVersion struct {
	Number    int       `json:"number"`
	CreatedAt time.Time `json:"created_at"`
	Markdown  string    `json:"markdown"`
}

type nearCopyCorpus struct {
	project string
	issues  map[string]nearCopyRecord
	queries []string
	pairs   []nearCopyRecord
}

func readNearCopyCorpus(t *testing.T, path string) nearCopyCorpus {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the near-copy corpus: %v", err)
	}
	defer file.Close()
	corpus := nearCopyCorpus{issues: map[string]nearCopyRecord{}}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		var record nearCopyRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode a near-copy corpus line: %v", err)
		}
		switch record.Record {
		case "issue":
			corpus.issues[record.Key] = record
			corpus.project = record.Key[:strings.LastIndex(record.Key, "-")]
		case "query":
			corpus.queries = append(corpus.queries, record.Key)
		case "pair":
			corpus.pairs = append(corpus.pairs, record)
		default:
			t.Fatalf("near-copy corpus record %q is none of issue, query or pair", record.Record)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read the near-copy corpus: %v", err)
	}
	if len(corpus.issues) == 0 || len(corpus.queries) == 0 {
		t.Fatal("the near-copy corpus holds no issue or no query")
	}
	return corpus
}

// specAt is the issue's spec as the corpus holds it at the given time: the latest exported
// version created by then.
func (corpus nearCopyCorpus) specAt(key string, at time.Time) string {
	spec := ""
	for _, version := range corpus.issues[key].SpecVersions {
		if !version.CreatedAt.After(at) {
			spec = version.Markdown
		}
	}
	return spec
}

func (corpus nearCopyCorpus) firstSpec(key string) string {
	for _, version := range corpus.issues[key].SpecVersions {
		if version.Number == 1 {
			return version.Markdown
		}
	}
	return ""
}

func (corpus nearCopyCorpus) firstTitle(key string) string {
	if history := corpus.issues[key].TitleHistory; len(history) > 0 {
		return history[0].Title
	}
	return ""
}

// nearCopyEvent is one change the walk applies, in the project's own order.
type nearCopyEvent struct {
	at   time.Time
	key  string
	kind string // "create", "title", "status", "parent" or "spec"
	// what the change sets, by kind.
	title   string
	status  string
	parent  *string
	version nearCopyVersion
}

// nearCopyTimeline is every change the corpus holds, oldest first. A create carries the issue's
// title, status and parent as it was filed and its first spec version, so the walk inserts the
// issue whole and then applies only what changed after it.
func nearCopyTimeline(corpus nearCopyCorpus) []nearCopyEvent {
	events := make([]nearCopyEvent, 0, len(corpus.issues)*3)
	for key, issue := range corpus.issues {
		create := nearCopyEvent{at: issue.CreatedAt, key: key, kind: "create"}
		if len(issue.TitleHistory) > 0 {
			create.title = issue.TitleHistory[0].Title
		}
		if len(issue.StatusHistory) > 0 {
			create.status = issue.StatusHistory[0].Status
		}
		if len(issue.ParentHistory) > 0 {
			create.parent = issue.ParentHistory[0].Parent
		}
		events = append(events, create)
		for _, entry := range issue.TitleHistory[min(1, len(issue.TitleHistory)):] {
			events = append(events, nearCopyEvent{at: entry.At, key: key, kind: "title", title: entry.Title})
		}
		for _, entry := range issue.StatusHistory[min(1, len(issue.StatusHistory)):] {
			events = append(events, nearCopyEvent{at: entry.At, key: key, kind: "status", status: entry.Status})
		}
		for _, entry := range issue.ParentHistory[min(1, len(issue.ParentHistory)):] {
			events = append(events, nearCopyEvent{at: entry.At, key: key, kind: "parent", parent: entry.Parent})
		}
		for _, version := range issue.SpecVersions {
			if version.Number == 1 {
				continue
			}
			events = append(events, nearCopyEvent{at: version.CreatedAt, key: key, kind: "spec", version: version})
		}
	}
	slices.SortFunc(events, func(a, b nearCopyEvent) int {
		if order := a.at.Compare(b.at); order != 0 {
			return order
		}
		return strings.Compare(a.key, b.key)
	})
	return events
}

// nearCopyScore is what the gate answered for one replayed create, and how long it took. The
// time is what the gate costs on a project of this size, which is the other number this replay
// is run for.
type nearCopyScore struct {
	key            string
	candidate      string
	candidateTitle string
	score          float64
	shared         int
	answered       bool
	took           time.Duration
}

// nearCopyGateBound is the bound the replay runs the gate under. Production's is 500 ms
// (duplicateGateTimeout), which this replay must not be held to: it measures what the gate
// costs, and a read cut off at the bound measures nothing. The gate prices the words a cold
// project's statistics do not hold rather than probing for them (priceUnknownTerms), so no
// early create pays for probes, but the replay's own walk writes the project as it reads it,
// and a read here can take far longer than a warm server's. What the gate costs is reported
// rather than enforced, and the report says how many of the replay's reads production's own
// bound would have cut.
const nearCopyGateBound = time.Minute

// nearCopyMarkedDuplicates are the replayed creates the implementer read and marked real
// duplicates of the issue the gate named, on 2026-11-05 over the INFRA corpus. k4's 5% ceiling
// is of the creates that are not duplicates, so these are left out of it, and
// dispatch://INFRA-1160/artifact/near-copy-replay-md carries the reason for each. The four
// pairs the corpus names are marked by their own record and are not repeated here.
//
// Each is a create whose text restates an open issue's work, judged by reading both titles and
// both specs: an identical title (INFRA-782, INFRA-375, INFRA-452, INFRA-451, INFRA-756,
// INFRA-454, INFRA-453), or the same defect in other words (INFRA-806, INFRA-442, INFRA-757,
// INFRA-70).
var nearCopyMarkedDuplicates = []string{
	"INFRA-806", "INFRA-782", "INFRA-375", "INFRA-452", "INFRA-442",
	"INFRA-451", "INFRA-756", "INFRA-454", "INFRA-757", "INFRA-70", "INFRA-453",
}

func TestNearCopyRefusalReplay(t *testing.T) {
	path := os.Getenv("DISPATCH_NEARCOPY_CORPUS")
	if path == "" {
		t.Skip("DISPATCH_NEARCOPY_CORPUS must name a file scripts/near-copy-replay.sh wrote")
	}
	corpus := readNearCopyCorpus(t, path)
	marked := map[string]bool{}
	for _, pair := range corpus.pairs {
		marked[pair.Query] = true
	}
	for _, key := range nearCopyMarkedDuplicates {
		marked[key] = true
	}
	for _, key := range strings.Split(os.Getenv("DISPATCH_NEARCOPY_DUPLICATES"), ",") {
		if key = strings.TrimSpace(key); key != "" {
			marked[key] = true
		}
	}

	ctx := context.Background()
	handler, database, deps, served := newRegisteredTestServer(t, testServerOptions{})
	// The related-item check is not what is being measured, and it runs on every create: give it
	// no time, so each create answers that it was not made and the replay pays only for the gate.
	served.suggestionTimeout = time.Millisecond
	served.duplicateBound = nearCopyGateBound
	srv := directServer(deps)
	srv.duplicateBound = nearCopyGateBound
	createSuggestionProject(t, handler, corpus.project)

	queries := map[string]bool{}
	for _, key := range corpus.queries {
		queries[key] = true
	}
	scores := make([]nearCopyScore, 0, len(corpus.queries))
	present := map[string]bool{}
	created := 0
	started := time.Now()
	var refreshedAt time.Time
	refreshes := 0
	for _, event := range nearCopyTimeline(corpus) {
		// The statistics as a live server keeps them: rebuilt every SearchTermStatsInterval of
		// the corpus's own time, before the create that first passes the interval, so each
		// replayed create meets statistics no older than production's.
		if refreshedAt.IsZero() || event.at.Sub(refreshedAt) >= SearchTermStatsInterval {
			refreshNearCopyStats(t, ctx, database, corpus.project)
			refreshedAt = event.at
			refreshes++
		}
		switch event.kind {
		case "create":
			if queries[event.key] {
				scores = append(scores, replayNearCopyGate(t, srv, corpus, event.key))
			}
			insertNearCopyIssue(t, ctx, database, handler, corpus, event, present)
			present[event.key] = true
			created++
		case "title":
			execNearCopy(t, ctx, database, `update issues set title = $2, updated_at = $3 where key = $1`,
				event.key, event.title, event.at)
		case "status":
			closedAt := any(nil)
			if event.status == "done" {
				closedAt = event.at
			}
			execNearCopy(t, ctx, database,
				`update issues set status = $2, closed_at = $3, updated_at = $4 where key = $1`,
				event.key, event.status, closedAt, event.at)
		case "parent":
			if event.parent == nil || present[*event.parent] {
				execNearCopy(t, ctx, database, `update issues set parent_key = $2 where key = $1`,
					event.key, event.parent)
			}
		case "spec":
			insertNearCopySpec(t, ctx, database, event.key, event.version)
		}
	}
	refreshNearCopyStats(t, ctx, database, corpus.project)
	t.Logf("replayed %d creates over %d issues in %v (term statistics refreshed %d times, every %v of replayed time)",
		len(scores), len(corpus.issues), time.Since(started).Round(time.Second), refreshes, SearchTermStatsInterval)

	measureNearCopyGate(t, ctx, srv, database, corpus)
	reportNearCopyReplay(t, corpus, scores, marked, refreshes)
}

// measureNearCopyGate times the gate on the loaded project, which holds the whole history the
// corpus carries and so is the production-like surface this measurement needs. It times the
// three parts the gate is made of beside the whole of it, for an ordinary text and for a very
// long spec, and prints the scoring statement's plan.
func measureNearCopyGate(
	t *testing.T, ctx context.Context, srv *server, database *store.Store, corpus nearCopyCorpus,
) {
	t.Helper()
	const runs = 20
	// An ordinary text: the longest-specced create of the replay, and a very long one built from
	// the corpus's own words, as a 10,000-word spec would be.
	ordinary, longest := "", 0
	words := make([]string, 0, 16384)
	for key := range corpus.issues {
		spec := corpus.firstSpec(key)
		if len(spec) > longest {
			ordinary, longest = corpus.firstTitle(key)+"\n"+spec, len(spec)
		}
		if len(words) < 10000 {
			words = append(words, strings.Fields(spec)...)
		}
	}
	if len(words) > 10000 {
		words = words[:10000]
	}
	texts := map[string]string{"an ordinary text": ordinary, "a 10,000-word spec": strings.Join(words, " ")}
	// The gate drops every lexeme an issue key contributes before it scores, so the timings and
	// the plan below are of the parameters the gate really sends (nearCopyScoreTerms).
	keyLexemes, err := duplicateKeyLexemes(ctx, database.Pool, corpus.project)
	if err != nil {
		t.Fatalf("read the project's issue-key lexemes: %v", err)
	}

	for name, text := range texts {
		title, _, _ := strings.Cut(text, "\n")
		whole := make([]time.Duration, 0, runs)
		ancestors := make([]time.Duration, 0, runs)
		picks := make([]time.Duration, 0, runs)
		scoring := make([]time.Duration, 0, runs)
		for run := range runs + 1 {
			started := time.Now()
			if _, err := srv.readDuplicateCandidates(ctx, corpus.project, title, text, nil, nearCopyGateBound, duplicateNudgeScore); err != nil {
				t.Fatalf("the gate did not answer for %s: %v", name, err)
			}
			gate := time.Since(started)

			started = time.Now()
			if _, err := refs.IssueAncestors(ctx, database.Pool, corpus.queries[len(corpus.queries)-1]); err != nil {
				t.Fatalf("read the ancestors: %v", err)
			}
			ancestor := time.Since(started)

			started = time.Now()
			picked, err := srv.pickRelatedTerms(ctx, database.Pool, corpus.project, text, title, relatedExclusion{}, priceUnknownTerms)
			if err != nil {
				t.Fatalf("pick the terms for %s: %v", name, err)
			}
			pick := time.Since(started)

			lexemes, weights := nearCopyScoreTerms(picked, keyLexemes)
			started = time.Now()
			rows, err := database.Pool.Query(ctx, duplicateScoreQuery,
				corpus.project, lexemes, weights, []string{}, duplicateNudgeScore, duplicateHeadlineOptions)
			if err != nil {
				t.Fatalf("score for %s: %v", name, err)
			}
			if _, err := scanDuplicateCandidates(rows); err != nil {
				t.Fatalf("read the scores for %s: %v", name, err)
			}
			score := time.Since(started)

			if run == 0 {
				continue // the first run warms the plans and the cache
			}
			whole = append(whole, gate)
			ancestors = append(ancestors, ancestor)
			picks = append(picks, pick)
			scoring = append(scoring, score)
		}
		for label, times := range map[string][]time.Duration{
			"the whole gate": whole, "the ancestor read": ancestors,
			"the term pick": picks, "the scoring statement": scoring,
		} {
			slices.Sort(times)
			t.Logf("latency, %s, %s over %d runs: p50 %v, p95 %v, max %v", name, label, len(times),
				times[len(times)/2].Round(time.Millisecond),
				times[min(int(0.95*float64(len(times))), len(times)-1)].Round(time.Millisecond),
				times[len(times)-1].Round(time.Millisecond))
		}
	}

	title, _, _ := strings.Cut(ordinary, "\n")
	picked, err := srv.pickRelatedTerms(ctx, database.Pool, corpus.project, ordinary, title, relatedExclusion{}, priceUnknownTerms)
	if err != nil {
		t.Fatalf("pick the terms for the plan: %v", err)
	}
	lexemes, weights := nearCopyScoreTerms(picked, keyLexemes)
	plan, err := database.Pool.Query(ctx, "explain (analyze, buffers) "+duplicateScoreQuery,
		corpus.project, lexemes, weights, []string{}, duplicateNudgeScore, duplicateHeadlineOptions)
	if err != nil {
		t.Fatalf("explain the scoring statement: %v", err)
	}
	defer plan.Close()
	for plan.Next() {
		var line string
		if err := plan.Scan(&line); err != nil {
			t.Fatalf("read the plan: %v", err)
		}
		t.Logf("plan: %s", line)
	}
}

// replayNearCopyGate runs the real gate for one create, against the project as the walk has left
// it: the create's own issue is not inserted yet, so it is no competitor of its own text.
func replayNearCopyGate(t *testing.T, srv *server, corpus nearCopyCorpus, key string) nearCopyScore {
	t.Helper()
	issue := corpus.issues[key]
	started := time.Now()
	// From zero, not from the gate's own floor: the replay must see the whole distribution to
	// report the rate at a bar below today's.
	candidates, err := srv.readDuplicateCandidates(context.Background(), corpus.project,
		corpus.firstTitle(key), corpus.firstSpec(key), issue.Ancestors, nearCopyGateBound, 0)
	score := nearCopyScore{key: key, answered: err == nil, took: time.Since(started)}
	if err != nil {
		// A read that runs past the bound is cut by statement_timeout, and the create goes in
		// with no check made: that is the gate's own behaviour, not a fault, and its share is
		// what the bound is judged on, so the replay counts it and goes on. Any other error is
		// the change's and fails the run.
		if !nearCopyBoundHit(err) {
			t.Fatalf("the gate did not answer for %s: %v", key, err)
		}
		return score
	}
	if len(candidates) > 0 {
		closest := candidates[0]
		score.candidate, score.score, score.shared = closest.Key, closest.Score, closest.SharedTerms
		score.candidateTitle = closest.Title
	}
	return score
}

func execNearCopy(t *testing.T, ctx context.Context, database *store.Store, statement string, arguments ...any) {
	t.Helper()
	if _, err := database.Pool.Exec(ctx, statement, arguments...); err != nil {
		t.Fatalf("%s: %v", strings.SplitN(strings.TrimSpace(statement), "\n", 2)[0], err)
	}
}

func refreshNearCopyStats(t *testing.T, ctx context.Context, database *store.Store, project string) {
	t.Helper()
	if _, err := refreshSearchTermStats(ctx, database.Pool, project); err != nil {
		t.Fatalf("refresh the term statistics: %v", err)
	}
}

// insertNearCopyIssue creates the issue through the API, under its own key, then sets the
// columns the corpus holds and the API does not take: its creation time and the status it was
// filed in. The create is forced, so the gate never refuses one; its own gate run is the price
// of going through the real write path.
func insertNearCopyIssue(
	t *testing.T, ctx context.Context, database *store.Store, handler http.Handler,
	corpus nearCopyCorpus, event nearCopyEvent, present map[string]bool,
) {
	t.Helper()
	number := nearCopyNumber(t, event.key)
	execNearCopy(t, ctx, database, `update projects set next_number = $2 where key = $1`, corpus.project, number)
	body := map[string]any{
		"project": corpus.project, "title": event.title,
		"spec": corpus.firstSpec(event.key), "force": true,
	}
	// A parent the walk has not reached yet is left off: an event log that lost its issue.created
	// gives the parent a later update set, and the create would be refused for an issue that does
	// not exist. No read of the gate's is on issues.parent_key - the ancestors it leaves out come
	// from the corpus - so the link only has to be right where the history has it.
	if event.parent != nil && present[*event.parent] {
		body["parent"] = *event.parent
	}
	response := createIssueRequest(t, handler, body)
	if response.Code != http.StatusCreated {
		t.Fatalf("create %s: status=%d body=%s", event.key, response.Code, response.Body.String())
	}
	if created := decodeBody[model.Issue](t, response).Key; created != event.key {
		t.Fatalf("created %s, want %s", created, event.key)
	}
	closedAt := any(nil)
	if event.status == "done" {
		closedAt = event.at
	}
	execNearCopy(t, ctx, database, `
		update issues set created_at = $2, updated_at = $2, status = $3, closed_at = $4 where key = $1
	`, event.key, event.at, event.status, closedAt)
}

// insertNearCopySpec adds one later version of an issue's primary document, as the server's own
// write does; the document_search trigger keeps the latest version's vector.
func insertNearCopySpec(t *testing.T, ctx context.Context, database *store.Store, key string, version nearCopyVersion) {
	t.Helper()
	execNearCopy(t, ctx, database, `
		insert into artifact_versions (artifact_id, number, markdown, authors, created_at, doc_update_version)
		select a.id, $2, $3, (select authors from artifact_versions where artifact_id = a.id and number = 1), $4,
		       coalesce((select max(version) from doc_updates where artifact_id = a.id), 0)
		  from artifacts a
		 where a.issue_key = $1 and a.is_primary
		on conflict (artifact_id, number) do update set markdown = excluded.markdown
	`, key, version.Number, version.Markdown, version.CreatedAt)
}

func nearCopyNumber(t *testing.T, key string) int {
	t.Helper()
	number, err := strconv.Atoi(key[strings.LastIndex(key, "-")+1:])
	if err != nil {
		t.Fatalf("read the number of %s: %v", key, err)
	}
	return number
}

// reportNearCopyReplay prints the report the pull request and the artifact carry, then holds the
// two bars in code to what it measured.
func reportNearCopyReplay(
	t *testing.T, corpus nearCopyCorpus, scores []nearCopyScore, marked map[string]bool, refreshes int,
) {
	t.Helper()
	// A create whose gate ran past its bound got no check at all, so it is no evidence of a bar:
	// it is counted and reported on its own, and the rates below are of the creates the gate
	// answered. Its share is what says whether the bound is set where the creates meet it.
	answered := make([]nearCopyScore, 0, len(scores))
	boundHits := 0
	for _, score := range scores {
		if score.answered {
			answered = append(answered, score)
			continue
		}
		boundHits++
		t.Logf("the bound: %s got no check; the gate's read took %v",
			score.key, score.took.Round(time.Millisecond))
	}
	t.Logf("the bound (%v): %d of %d replayed creates got no check (%.2f%%)",
		nearCopyGateBound, boundHits, len(scores), 100*float64(boundHits)/float64(max(len(scores), 1)))
	scores = answered
	byKey := map[string]nearCopyScore{}
	took := make([]time.Duration, 0, len(scores))
	for _, score := range scores {
		byKey[score.key] = score
		took = append(took, score.took)
	}
	slices.Sort(took)
	if len(took) > 0 {
		at := func(share float64) time.Duration {
			return took[min(int(share*float64(len(took))), len(took)-1)]
		}
		t.Logf("the gate's own time over %d answered creates: p50 %v, p95 %v, max %v",
			len(took), at(0.5).Round(time.Millisecond), at(0.95).Round(time.Millisecond),
			took[len(took)-1].Round(time.Millisecond))
		// The replay runs under a bound of its own, so these creates all answered. What a
		// reader needs is the share production's bound would have cut, since such a create
		// gets no check there: the reads at or past duplicateGateTimeout.
		overBound := 0
		for _, one := range took {
			if one >= duplicateGateTimeout {
				overBound++
			}
		}
		t.Logf("production's bound (%v): %d of %d answered creates read that long or longer (%.2f%%), and would have got no check",
			duplicateGateTimeout, overBound, len(took), 100*float64(overBound)/float64(len(took)))
	}
	for _, pair := range corpus.pairs {
		score := byKey[pair.Query]
		verdict := "not refused"
		if score.score >= duplicateRefuseScore {
			verdict = "refused"
		}
		t.Logf("named pair: %s against %s: closest %s at %.3f (%d shared terms), %s",
			pair.Query, pair.Target, score.candidate, score.score, score.shared, verdict)
	}

	buckets := make([]int, 21)
	for _, score := range scores {
		bucket := int(score.score * 20)
		buckets[min(max(bucket, 0), 20)]++
	}
	for bucket, count := range buckets {
		if count > 0 {
			t.Logf("histogram: score in [%.2f, %.2f): %d creates", float64(bucket)/20, float64(bucket+1)/20, count)
		}
	}

	// Every create the gate would name, for the implementer to mark a duplicate or not. The
	// report artifact carries the markings, and every create not marked counts against the 5%
	// ceiling.
	named := make([]nearCopyScore, 0, len(scores))
	for _, score := range scores {
		if score.score >= duplicateNudgeScore {
			named = append(named, score)
		}
	}
	slices.SortFunc(named, func(a, b nearCopyScore) int { return cmp.Compare(b.score, a.score) })
	for _, score := range named {
		t.Logf("named: %.3f %s %q -> %s %q (%d shared terms)",
			score.score, score.key, corpus.firstTitle(score.key), score.candidate, score.candidateTitle, score.shared)
	}

	rateAt := func(bar float64) (int, int) {
		refused, false_ := 0, 0
		for _, score := range scores {
			if score.score < bar {
				continue
			}
			refused++
			if !marked[score.key] {
				false_++
			}
		}
		return refused, false_
	}
	nudgedAt := func(nudge, refuse float64) int {
		nudged := 0
		for _, score := range scores {
			if score.score >= nudge && score.score < refuse {
				nudged++
			}
		}
		return nudged
	}
	total := len(scores)
	notDuplicates := 0
	for _, score := range scores {
		if !marked[score.key] {
			notDuplicates++
		}
	}
	for bar := 0.50; bar <= 0.96; bar += 0.05 {
		refused, false_ := rateAt(bar)
		t.Logf("refuse bar %.2f: %d refused, %d of them not marked duplicates (%.2f%% of the %d creates not marked)",
			bar, refused, false_, 100*float64(false_)/float64(notDuplicates), notDuplicates)
	}
	for bar := 0.30; bar < duplicateRefuseScore+0.001; bar += 0.05 {
		nudged := nudgedAt(bar, duplicateRefuseScore)
		t.Logf("nudge bar %.2f (refuse %.2f): %d nudged (%.2f%% of %d creates)",
			bar, duplicateRefuseScore, nudged, 100*float64(nudged)/float64(total), total)
	}
	t.Logf("corpus: %s; creates replayed: %d; marked duplicates: %d; term statistics refreshed %d times, every %v of replayed time",
		os.Getenv("DISPATCH_NEARCOPY_CORPUS"), total, len(marked), refreshes, SearchTermStatsInterval)
	t.Logf("distortions a later reader must know: in a project with no term statistics the gate prices every "+
		"lexeme at document frequency 1, nothing being priced, and reads N as 0, since it prices rather than probes; "+
		"document frequency is counted per project while the index scan is not; every lexeme an issue key "+
		"contributes is dropped; the picker keeps at most %d terms, spends at most %d document frequency, "+
		"and drops a term in more than a tenth of the project or under %d items; a text left with fewer "+
		"than %d terms, or holding no more lexemes than that, is scored by no share at all - it refuses "+
		"only an open issue whose title and spec hold the same lexemes, which the report counts at 1.000. "+
		"A change to any of those invalidates this calibration.",
		relatedTermLimit, relatedMatchBudget, relatedCommonFloor, duplicateMinTerms)

	for _, pair := range corpus.pairs {
		if byKey[pair.Query].score < duplicateRefuseScore {
			t.Errorf("the named pair %s against %s scores %.3f, under the refuse bar %.2f",
				pair.Query, pair.Target, byKey[pair.Query].score, duplicateRefuseScore)
		}
	}
	_, falseRefusals := rateAt(duplicateRefuseScore)
	if share := float64(falseRefusals) / float64(notDuplicates); share > 0.05 {
		t.Errorf("the refuse bar %.2f refuses %.2f%% of the creates not marked duplicates, over the 5%% ceiling",
			duplicateRefuseScore, 100*share)
	}
	if share := float64(nudgedAt(duplicateNudgeScore, duplicateRefuseScore)) / float64(total); share > 0.15 {
		t.Errorf("the nudge bar %.2f nudges %.2f%% of the creates, over the 15%% ceiling",
			duplicateNudgeScore, 100*share)
	}
}
