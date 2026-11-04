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
// DISPATCH_NEARCOPY_REFRESH_EVERY
//
//	how many creates run between two refreshes of the project's term statistics (default 1).
//	A live server refreshes every ten minutes, so a larger number is the more faithful replay
//	and the faster one; the report names what was used.

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

// nearCopyScore is what the gate answered for one replayed create.
type nearCopyScore struct {
	key       string
	candidate string
	score     float64
	shared    int
	terms     int
	answered  bool
}

func TestNearCopyRefusalReplay(t *testing.T) {
	path := os.Getenv("DISPATCH_NEARCOPY_CORPUS")
	if path == "" {
		t.Skip("DISPATCH_NEARCOPY_CORPUS must name a file scripts/near-copy-replay.sh wrote")
	}
	corpus := readNearCopyCorpus(t, path)
	refreshEvery := 1
	if every := os.Getenv("DISPATCH_NEARCOPY_REFRESH_EVERY"); every != "" {
		parsed, err := strconv.Atoi(every)
		if err != nil || parsed < 1 {
			t.Fatalf("DISPATCH_NEARCOPY_REFRESH_EVERY = %q", every)
		}
		refreshEvery = parsed
	}
	marked := map[string]bool{}
	for _, pair := range corpus.pairs {
		marked[pair.Query] = true
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
	srv := directServer(deps)
	createSuggestionProject(t, handler, corpus.project)

	queries := map[string]bool{}
	for _, key := range corpus.queries {
		queries[key] = true
	}
	scores := make([]nearCopyScore, 0, len(corpus.queries))
	created := 0
	started := time.Now()
	for _, event := range nearCopyTimeline(corpus) {
		switch event.kind {
		case "create":
			if queries[event.key] {
				scores = append(scores, replayNearCopyGate(t, srv, corpus, event.key))
			}
			insertNearCopyIssue(t, ctx, database, handler, corpus, event)
			created++
			if created%refreshEvery == 0 {
				refreshNearCopyStats(t, ctx, database, corpus.project)
			}
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
			execNearCopy(t, ctx, database, `update issues set parent_key = $2 where key = $1`,
				event.key, event.parent)
		case "spec":
			insertNearCopySpec(t, ctx, database, event.key, event.version)
		}
	}
	refreshNearCopyStats(t, ctx, database, corpus.project)
	t.Logf("replayed %d creates over %d issues in %v (term statistics refreshed every %d creates)",
		len(scores), len(corpus.issues), time.Since(started).Round(time.Second), refreshEvery)

	reportNearCopyReplay(t, corpus, scores, marked, refreshEvery)
}

// replayNearCopyGate runs the real gate for one create, against the project as the walk has left
// it: the create's own issue is not inserted yet, so it is no competitor of its own text.
func replayNearCopyGate(t *testing.T, srv *server, corpus nearCopyCorpus, key string) nearCopyScore {
	t.Helper()
	issue := corpus.issues[key]
	result, err := srv.duplicateGate(context.Background(), corpus.project,
		corpus.firstTitle(key), corpus.firstSpec(key), issue.Ancestors)
	score := nearCopyScore{key: key, answered: err == nil}
	if err != nil {
		t.Fatalf("the gate did not answer for %s: %v", key, err)
	}
	if len(result.Candidates) > 0 {
		closest := result.Candidates[0]
		score.candidate, score.score, score.shared = closest.Key, closest.Score, closest.SharedTerms
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
	corpus nearCopyCorpus, event nearCopyEvent,
) {
	t.Helper()
	number := nearCopyNumber(t, event.key)
	execNearCopy(t, ctx, database, `update projects set next_number = $2 where key = $1`, corpus.project, number)
	body := map[string]any{
		"project": corpus.project, "title": event.title,
		"spec": corpus.firstSpec(event.key), "force": true,
	}
	if event.parent != nil && *event.parent != "" {
		if _, known := corpus.issues[*event.parent]; known {
			body["parent"] = *event.parent
		}
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
	t *testing.T, corpus nearCopyCorpus, scores []nearCopyScore, marked map[string]bool, refreshEvery int,
) {
	t.Helper()
	byKey := map[string]nearCopyScore{}
	for _, score := range scores {
		byKey[score.key] = score
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

	buckets := make([]int, 11)
	for _, score := range scores {
		bucket := int(score.score * 10)
		buckets[min(max(bucket, 0), 10)]++
	}
	for bucket, count := range buckets {
		t.Logf("histogram: score in [%.1f, %.1f): %d creates", float64(bucket)/10, float64(bucket+1)/10, count)
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
	for _, delta := range []float64{-0.05, 0, 0.05} {
		bar := duplicateRefuseScore + delta
		refused, false_ := rateAt(bar)
		t.Logf("refuse bar %.2f: %d refused, %d of them not marked duplicates (%.2f%% of the %d creates not marked)",
			bar, refused, false_, 100*float64(false_)/float64(notDuplicates), notDuplicates)
	}
	for _, delta := range []float64{-0.05, 0, 0.05} {
		bar := duplicateNudgeScore + delta
		nudged := nudgedAt(bar, duplicateRefuseScore)
		t.Logf("nudge bar %.2f: %d nudged (%.2f%% of %d creates)",
			bar, nudged, 100*float64(nudged)/float64(total), total)
	}
	t.Logf("corpus: %s; creates replayed: %d; marked duplicates: %d; refresh every %d creates",
		os.Getenv("DISPATCH_NEARCOPY_CORPUS"), total, len(marked), refreshEvery)
	t.Logf("distortions a later reader must know: a project with no term statistics scores in probe mode; "+
		"document frequency is counted per project while the index scan is not; every lexeme an issue key "+
		"contributes is dropped; the picker keeps at most %d terms, spends at most %d document frequency, "+
		"and drops a term in more than a tenth of the project or under %d items; the gate judges nothing "+
		"under %d terms. A change to any of those invalidates this calibration.",
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
	_ = fmt.Sprint(duplicateCandidateLimit)
}
