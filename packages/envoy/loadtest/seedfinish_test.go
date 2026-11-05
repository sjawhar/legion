package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// fakeSeedDB answers seed-finish's queries: each count query its counts in turn (the last one
// again once they run out), the block-index table check `indexed`, the request_keys table check
// `keyed`. ran is every statement in order.
type fakeSeedDB struct {
	counts  map[string][]int64
	indexed bool
	keyed   bool
	ran     []string
}

type fakeRow struct{ v any }

func (r fakeRow) Scan(dest ...any) error {
	switch d := dest[0].(type) {
	case *int64:
		*d = r.v.(int64)
	case *bool:
		*d = r.v.(bool)
	default:
		return fmt.Errorf("fakeRow: cannot scan into %T", d)
	}
	return nil
}

func (db *fakeSeedDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	db.ran = append(db.ran, sql)
	switch sql {
	case sqlBlockIndexTables:
		return fakeRow{db.indexed}
	case sqlRequestKeysTable:
		return fakeRow{db.keyed}
	}
	seq := db.counts[sql]
	if len(seq) > 1 {
		db.counts[sql] = seq[1:]
	}
	return fakeRow{seq[0]}
}

func (db *fakeSeedDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	db.ran = append(db.ran, sql)
	return pgconn.CommandTag{}, nil
}

// fakeFinisher is a seedFinisher on a fake clock that each sleep advances; it logs into logged.
func fakeFinisher(timeout time.Duration, logged *bytes.Buffer) (*seedFinisher, *time.Time) {
	clock := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f := &seedFinisher{
		timeout: timeout, poll: seedFinishPoll,
		now:   func() time.Time { return clock },
		sleep: func(_ context.Context, d time.Duration) bool { clock = clock.Add(d); return true },
		lg:    log.New(logged, "", 0),
	}
	return f, &clock
}

// count is how many times sql is in ran.
func count(ran []string, sql string) int {
	n := 0
	for _, s := range ran {
		if s == sql {
			n++
		}
	}
	return n
}

// TestSeedFinishWaitsForTheOutboxAndTheBackfillThenVacuumsAndInstallsTheTrigger: each wait polls
// every 2 s until its count is 0, in the shell's order, and the phases are logged.
func TestSeedFinishWaitsForTheOutboxAndTheBackfillThenVacuumsAndInstallsTheTrigger(t *testing.T) {
	var logged bytes.Buffer
	f, clock := fakeFinisher(15*time.Minute, &logged)
	start := *clock
	db := &fakeSeedDB{indexed: true, keyed: true, counts: map[string][]int64{sqlUnpublishedEvents: {5, 3, 0}, sqlBlockIndexOwed: {7, 0}, sqlRequestKeysCount: {0}}}
	if err := f.finish(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	want := []string{sqlUnpublishedEvents, sqlUnpublishedEvents, sqlUnpublishedEvents, sqlBlockIndexTables, sqlBlockIndexOwed, sqlBlockIndexOwed,
		sqlRequestKeysTable, sqlRequestKeysDel, sqlRequestKeysCount, sqlVacuumAnalyze, commitTriggerSQL}
	if !slices.Equal(db.ran, want) {
		t.Fatalf("statements run %q, want %q", db.ran, want)
	}
	if waited := clock.Sub(start); waited != 3*seedFinishPoll {
		t.Errorf("waited %s, want three polls of 2 s", waited)
	}
	for _, phase := range []string{"waiting for the outbox", "block-index backfill", "idempotency keys", "vacuum analyze", "installing the commit-time trigger"} {
		if !strings.Contains(logged.String(), phase) {
			t.Errorf("the log lacks %q:\n%s", phase, logged.String())
		}
	}
}

// TestSeedFinishSkipsTheBackfillWaitOnABuildWithoutItsTables: a build older than migration 0121 has
// no block-index tables, so seed-finish never queries them.
func TestSeedFinishSkipsTheBackfillWaitOnABuildWithoutItsTables(t *testing.T) {
	var logged bytes.Buffer
	f, _ := fakeFinisher(15*time.Minute, &logged)
	db := &fakeSeedDB{counts: map[string][]int64{sqlUnpublishedEvents: {0}}}
	if err := f.finish(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if count(db.ran, sqlBlockIndexOwed) != 0 || count(db.ran, sqlRequestKeysDel) != 0 || count(db.ran, sqlVacuumAnalyze) != 1 || count(db.ran, commitTriggerSQL) != 1 {
		t.Fatalf("statements run %q: want no block-index count, no key delete, one vacuum and one trigger install", db.ran)
	}
}

// TestSeedFinishRefusesATemplateThatStillHoldsIdempotencyKeys: keys left after the delete would
// be swept inside a later step's window, so seed-finish fails before vacuum and the trigger.
func TestSeedFinishRefusesATemplateThatStillHoldsIdempotencyKeys(t *testing.T) {
	var logged bytes.Buffer
	f, _ := fakeFinisher(15*time.Minute, &logged)
	db := &fakeSeedDB{keyed: true, counts: map[string][]int64{sqlUnpublishedEvents: {0}, sqlRequestKeysCount: {3}}}
	err := f.finish(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "request_keys still holds 3 rows") {
		t.Fatalf("error %v, want the 3 rows request_keys still holds named", err)
	}
	if count(db.ran, sqlVacuumAnalyze)+count(db.ran, commitTriggerSQL) != 0 {
		t.Errorf("vacuum or the trigger ran after the keys were left: %q", db.ran)
	}
}

// TestSeedFinishFailsAfterTheTimeoutNamingWhatIsPending: each wait fails once it has waited
// -timeout, saying which wait and how many are still pending, and nothing after it runs.
func TestSeedFinishFailsAfterTheTimeoutNamingWhatIsPending(t *testing.T) {
	for _, c := range []struct {
		counts  map[string][]int64
		want    string
		pending string
	}{
		{map[string][]int64{sqlUnpublishedEvents: {4}}, "the outbox still holds 4 unpublished events after 10 s", sqlUnpublishedEvents},
		{map[string][]int64{sqlUnpublishedEvents: {0}, sqlBlockIndexOwed: {9}}, "Dispatch still owes 9 block-index entries after 10 s", sqlBlockIndexOwed},
	} {
		var logged bytes.Buffer
		f, clock := fakeFinisher(10*time.Second, &logged)
		start := *clock
		db := &fakeSeedDB{indexed: true, counts: c.counts}
		err := f.finish(context.Background(), db)
		if err == nil || err.Error() != c.want {
			t.Errorf("error %v, want %q", err, c.want)
		}
		if waited := clock.Sub(start); waited < 10*time.Second || waited > 10*time.Second+seedFinishPoll {
			t.Errorf("%s: failed after %s of waiting, want 10 s", c.want, waited)
		}
		// Polls at 0, 2, ..., 10 s: six reads of the pending count.
		if n := count(db.ran, c.pending); n != 6 {
			t.Errorf("%s: read the count %d times, want 6", c.want, n)
		}
		if count(db.ran, sqlVacuumAnalyze)+count(db.ran, commitTriggerSQL) != 0 {
			t.Errorf("%s: vacuum or the trigger ran after the wait failed: %q", c.want, db.ran)
		}
	}
}

// coverageStep is one answer fakeCoverage gives the embedding gate: a body, or the error a
// non-200 answer or a body that does not decode becomes (dispatchCoverage returns both as
// errors).
type coverageStep struct {
	body model.SearchCoverage
	err  error
}

// fakeCoverage answers the gate's reads in turn, the last step again once they run out, as
// fakeSeedDB repeats its last count; reads is how many times the gate read.
type fakeCoverage struct {
	steps []coverageStep
	reads int
}

func (c *fakeCoverage) readCoverage(context.Context) (model.SearchCoverage, error) {
	step := c.steps[min(c.reads, len(c.steps)-1)]
	c.reads++
	return step.body, step.err
}

// kindCounts is one kind's counts: current items, lacking ones whose oldest has waited oldest
// seconds (null when none lacks, as the report answers), and ones the queue gave up on.
func kindCounts(current, lacking, failed, oldest int64) model.SearchCoverageCounts {
	counts := model.SearchCoverageCounts{Items: current + lacking + failed, Current: current, Lacking: lacking, Failed: failed}
	if lacking > 0 {
		counts.OldestLackingSeconds = &oldest
	}
	return counts
}

// coverageBody is a report body of a deployment that has an embedder: the kinds given, and the
// total Dispatch sums from them (the longest wait of any kind is the total's).
func coverageBody(kinds model.SearchCoverageKinds) model.SearchCoverage {
	identity, place, name := "azure:stub.invalid/embed-v-4-0", "azure", "embed-v-4-0"
	var total model.SearchCoverageCounts
	for _, kind := range coverageKinds(kinds) {
		total.Items += kind.counts.Items
		total.Current += kind.counts.Current
		total.Lacking += kind.counts.Lacking
		total.Failed += kind.counts.Failed
		if waited := kind.counts.OldestLackingSeconds; waited != nil && (total.OldestLackingSeconds == nil || *waited > *total.OldestLackingSeconds) {
			total.OldestLackingSeconds = waited
		}
	}
	return model.SearchCoverage{Identity: &identity, Place: &place, Model: &name, Total: total, Kinds: kinds}
}

// fakeEmbedFinisher is fakeFinisher with the embedding gate wired to cov, its record written to
// path (empty writes none) and bound by timeout.
func fakeEmbedFinisher(timeout time.Duration, cov coverageReader, path string, logged *bytes.Buffer) (*seedFinisher, *time.Time) {
	f, clock := fakeFinisher(15*time.Minute, logged)
	f.embedTimeout, f.embedPoll, f.coverage, f.embedProgress = timeout, seedEmbedPoll, cov, path
	return f, clock
}

// readCoverageRecord reads the record the gate wrote at path.
func readCoverageRecord(t *testing.T, path string) coverageRecord {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record coverageRecord
	if err := json.Unmarshal(b, &record); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return record
}

// TestSeedFinishEmbeddingGateReturnsOnceEveryKindIsCovered: the gate reads coverage every
// seedEmbedPoll and returns at the first body where no kind lacks a vector; its record holds one
// entry per poll, with the counts and the time of that poll.
func TestSeedFinishEmbeddingGateReturnsOnceEveryKindIsCovered(t *testing.T) {
	var logged bytes.Buffer
	path := filepath.Join(t.TempDir(), "seedfinish.embed.json")
	cov := &fakeCoverage{steps: []coverageStep{
		{body: coverageBody(model.SearchCoverageKinds{Issue: kindCounts(10, 5, 0, 120), Comment: kindCounts(100, 20, 0, 300)})},
		{body: coverageBody(model.SearchCoverageKinds{Issue: kindCounts(15, 0, 0, 0), Comment: kindCounts(110, 10, 0, 330)})},
		{body: coverageBody(model.SearchCoverageKinds{Issue: kindCounts(15, 0, 0, 0), Comment: kindCounts(120, 0, 0, 0)})},
	}}
	f, clock := fakeEmbedFinisher(8*time.Hour, cov, path, &logged)
	start := *clock
	if err := f.waitEmbedded(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cov.reads != 3 {
		t.Errorf("read coverage %d times, want 3", cov.reads)
	}
	if waited := clock.Sub(start); waited != 2*seedEmbedPoll {
		t.Errorf("waited %s, want two polls of %s", waited, seedEmbedPoll)
	}
	record := readCoverageRecord(t, path)
	if len(record.Polls) != 3 {
		t.Fatalf("the record holds %d polls, want 3: %+v", len(record.Polls), record)
	}
	for i, want := range []model.SearchCoverageCounts{{Items: 135, Current: 110, Lacking: 25}, {Items: 135, Current: 125, Lacking: 10}, {Items: 135, Current: 135}} {
		poll := record.Polls[i]
		if poll.Total == nil {
			t.Fatalf("poll %d holds no counts: %+v", i, poll)
		}
		if poll.Total.Items != want.Items || poll.Total.Current != want.Current || poll.Total.Lacking != want.Lacking {
			t.Errorf("poll %d counted %d items, %d current, %d lacking; want %d, %d, %d",
				i, poll.Total.Items, poll.Total.Current, poll.Total.Lacking, want.Items, want.Current, want.Lacking)
		}
		if at := start.Add(time.Duration(i) * seedEmbedPoll); !poll.At.Equal(at) {
			t.Errorf("poll %d was read at %s, want %s", i, poll.At, at)
		}
	}
	if oldest := record.Polls[0].Total.OldestLackingSeconds; oldest == nil || *oldest != 300 {
		t.Errorf("the first poll's oldest lacking item is %v, want 300 s", oldest)
	}
	if record.Polls[2].Total.OldestLackingSeconds != nil {
		t.Errorf("the covered poll names an oldest lacking item: %+v", record.Polls[2])
	}
	if record.Error != "" || record.WaitedSeconds != int64((2*seedEmbedPoll).Seconds()) {
		t.Errorf("the record ended %q after %d s, want no error after %.0f s", record.Error, record.WaitedSeconds, (2 * seedEmbedPoll).Seconds())
	}
	if record.Identity == nil || *record.Identity != "azure:stub.invalid/embed-v-4-0" {
		t.Errorf("the record names identity %v, want the body's", record.Identity)
	}
	if !strings.Contains(logged.String(), "meaning search covers the seed") {
		t.Errorf("the log does not say the seed is covered:\n%s", logged.String())
	}
}

// TestSeedFinishEmbeddingGateRefusesAKindTheQueueGaveUpOn: rows that failed for good never
// become current, so the gate names the kind and the count and refuses at that poll.
func TestSeedFinishEmbeddingGateRefusesAKindTheQueueGaveUpOn(t *testing.T) {
	var logged bytes.Buffer
	path := filepath.Join(t.TempDir(), "seedfinish.embed.json")
	cov := &fakeCoverage{steps: []coverageStep{
		{body: coverageBody(model.SearchCoverageKinds{Issue: kindCounts(10, 0, 0, 0), Comment: kindCounts(100, 4, 3, 90)})},
	}}
	f, clock := fakeEmbedFinisher(8*time.Hour, cov, path, &logged)
	start := *clock
	err := f.waitEmbedded(context.Background())
	if err == nil || !strings.Contains(err.Error(), "comment 3") {
		t.Fatalf("error %v, want the 3 comment rows the queue gave up on named", err)
	}
	if cov.reads != 1 || !clock.Equal(start) {
		t.Errorf("read coverage %d times over %s, want one read and no waiting", cov.reads, clock.Sub(start))
	}
	if record := readCoverageRecord(t, path); !strings.Contains(record.Error, "comment 3") || len(record.Polls) != 1 {
		t.Errorf("the record holds %d polls and error %q, want one poll and the refusal", len(record.Polls), record.Error)
	}
}

// TestSeedFinishEmbeddingGateRefusesTheBoundNamingEachLackingKind: once it has waited
// -embed-timeout the gate fails, naming every kind still lacking vectors, how many and how long
// its oldest has waited; it logs its progress at an interval, not once a poll.
func TestSeedFinishEmbeddingGateRefusesTheBoundNamingEachLackingKind(t *testing.T) {
	var logged bytes.Buffer
	path := filepath.Join(t.TempDir(), "seedfinish.embed.json")
	cov := &fakeCoverage{steps: []coverageStep{
		{body: coverageBody(model.SearchCoverageKinds{Issue: kindCounts(80, 12, 0, 900), Comment: kindCounts(500, 4, 0, 60)})},
	}}
	f, clock := fakeEmbedFinisher(time.Hour, cov, path, &logged)
	start := *clock
	err := f.waitEmbedded(context.Background())
	want := "meaning search still lacks vectors after 3600 s: issue 12 (oldest 900 s), comment 4 (oldest 60 s)"
	if err == nil || err.Error() != want {
		t.Fatalf("error %v, want %q", err, want)
	}
	if waited := clock.Sub(start); waited != time.Hour {
		t.Errorf("failed after %s of waiting, want an hour", waited)
	}
	// Polls at 0, 15 s, ..., 3,600 s: 241 reads.
	if cov.reads != 241 {
		t.Errorf("read coverage %d times, want 241", cov.reads)
	}
	record := readCoverageRecord(t, path)
	if len(record.Polls) != 241 || record.Error != want || record.WaitedSeconds != 3600 {
		t.Errorf("the record holds %d polls, error %q and %d s waited; want 241 polls, the refusal and 3,600 s",
			len(record.Polls), record.Error, record.WaitedSeconds)
	}
	// The first poll, then one every seedEmbedLogEvery: 0, 15, 30, 45 and 60 minutes.
	if lines := strings.Count(logged.String(), " lacking (oldest "); lines != 5 {
		t.Errorf("logged %d progress lines over 241 polls, want 5:\n%s", lines, logged.String())
	}
}

// TestSeedFinishEmbeddingGateRefusesADeploymentWithNoEmbedderAtTheFirstPoll: identity, place and
// model answered null means nothing is ever embedded, so the gate refuses at once rather than
// waiting out its bound.
func TestSeedFinishEmbeddingGateRefusesADeploymentWithNoEmbedderAtTheFirstPoll(t *testing.T) {
	var logged bytes.Buffer
	cov := &fakeCoverage{steps: []coverageStep{{body: model.SearchCoverage{}}}}
	f, clock := fakeEmbedFinisher(8*time.Hour, cov, "", &logged)
	start := *clock
	err := f.waitEmbedded(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the deployment has no embedder") {
		t.Fatalf("error %v, want the deployment's missing embedder named", err)
	}
	if cov.reads != 1 || !clock.Equal(start) {
		t.Errorf("read coverage %d times over %s, want one read and no waiting", cov.reads, clock.Sub(start))
	}
}

// TestSeedFinishEmbeddingGateRetriesAnAnswerItCannotRead: a Dispatch that is restarting answers
// a non-200 or a body that does not decode; neither fails the seed, and the record keeps both
// polls with no counts.
func TestSeedFinishEmbeddingGateRetriesAnAnswerItCannotRead(t *testing.T) {
	var logged bytes.Buffer
	path := filepath.Join(t.TempDir(), "seedfinish.embed.json")
	cov := &fakeCoverage{steps: []coverageStep{
		{err: errors.New("GET /api/v1/search/coverage: HTTP 502 bad gateway")},
		{err: errors.New("GET /api/v1/search/coverage: invalid character '<' looking for beginning of value")},
		{body: coverageBody(model.SearchCoverageKinds{Issue: kindCounts(10, 0, 0, 0)})},
	}}
	f, clock := fakeEmbedFinisher(8*time.Hour, cov, path, &logged)
	start := *clock
	if err := f.waitEmbedded(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cov.reads != 3 {
		t.Errorf("read coverage %d times, want 3", cov.reads)
	}
	if waited := clock.Sub(start); waited != 2*seedEmbedPoll {
		t.Errorf("waited %s, want two polls of %s", waited, seedEmbedPoll)
	}
	record := readCoverageRecord(t, path)
	if len(record.Polls) != 3 {
		t.Fatalf("the record holds %d polls, want 3: %+v", len(record.Polls), record)
	}
	for i, poll := range record.Polls[:2] {
		if poll.Total != nil || !strings.Contains(poll.Error, "GET /api/v1/search/coverage") {
			t.Errorf("poll %d holds counts %+v and error %q, want no counts and the read's error", i, poll.Total, poll.Error)
		}
	}
	if record.Polls[2].Total == nil || record.Error != "" {
		t.Errorf("the gate ended %q with a last poll of %+v, want the covered body and no error", record.Error, record.Polls[2])
	}
	if !strings.Contains(logged.String(), "HTTP 502 bad gateway") {
		t.Errorf("the log does not say what it could not read:\n%s", logged.String())
	}
}

// TestSeedFinishRunsTheEmbeddingGateAfterTheBackfillAndBeforeTheVacuum: the gate sits between the
// block-index wait and the work that ends the seed, so a deployment whose queue gave up is
// refused before the idempotency keys are deleted, the vacuum runs or the trigger is installed.
func TestSeedFinishRunsTheEmbeddingGateAfterTheBackfillAndBeforeTheVacuum(t *testing.T) {
	var logged bytes.Buffer
	cov := &fakeCoverage{steps: []coverageStep{
		{body: coverageBody(model.SearchCoverageKinds{Issue: kindCounts(5, 0, 2, 0)})},
	}}
	f, _ := fakeEmbedFinisher(8*time.Hour, cov, "", &logged)
	db := &fakeSeedDB{indexed: true, keyed: true, counts: map[string][]int64{sqlUnpublishedEvents: {0}, sqlBlockIndexOwed: {0}, sqlRequestKeysCount: {0}}}
	err := f.finish(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "issue 2") {
		t.Fatalf("error %v, want the 2 issue rows the queue gave up on named", err)
	}
	want := []string{sqlUnpublishedEvents, sqlBlockIndexTables, sqlBlockIndexOwed}
	if !slices.Equal(db.ran, want) {
		t.Fatalf("statements run %q, want %q: nothing after the gate", db.ran, want)
	}
}

// coverageJSON is the body GET /api/v1/search/coverage answers, as the server writes it
// (model.SearchCoverage): every kind, and a null oldest wait where nothing lacks.
const coverageJSON = `{"identity":"azure:stub.invalid/embed-v-4-0","place":"azure","model":"embed-v-4-0",
 "total":{"items":10,"current":7,"lacking":3,"failed":0,"oldest_lacking_seconds":42},
 "kinds":{"issue":{"items":4,"current":4,"lacking":0,"failed":0,"oldest_lacking_seconds":null},
  "document":{"items":2,"current":1,"lacking":1,"failed":0,"oldest_lacking_seconds":42},
  "comment":{"items":2,"current":1,"lacking":1,"failed":0,"oldest_lacking_seconds":12},
  "ask":{"items":1,"current":0,"lacking":1,"failed":0,"oldest_lacking_seconds":5},
  "message":{"items":1,"current":1,"lacking":0,"failed":0,"oldest_lacking_seconds":null}}}`

// TestDispatchCoverageReadsTheReportAndReportsWhatItCannotRead: the reader asks for the coverage
// path with the deployment's agent bearer and decodes that body; a non-200 answer and a body
// that is not the report are errors, which the gate takes as transient and reads again.
func TestDispatchCoverageReadsTheReportAndReportsWhatItCannotRead(t *testing.T) {
	status, body := http.StatusOK, coverageJSON
	var path, authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, authorization = r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	reader := dispatchCoverage{a: newAPI(server.URL, "agent-token", 2)}

	coverage, err := reader.readCoverage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if path != searchCoveragePath || authorization != "Bearer agent-token" {
		t.Errorf("read %s as %q, want %s with the agent bearer", path, authorization, searchCoveragePath)
	}
	if coverage.Identity == nil || *coverage.Identity != "azure:stub.invalid/embed-v-4-0" || coverage.Total.Lacking != 3 {
		t.Errorf("read %+v, want the body's identity and its 3 lacking items", coverage)
	}
	if lacking := coverageLacking(coverage.Kinds); lacking != "document 1 (oldest 42 s), comment 1 (oldest 12 s), ask 1 (oldest 5 s)" {
		t.Errorf("the body's lacking kinds read %q", lacking)
	}

	status, body = http.StatusServiceUnavailable, "upstream connect error"
	if _, err := reader.readCoverage(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("error %v, want the 503 named", err)
	}
	status, body = http.StatusOK, "<html>a proxy's page</html>"
	if _, err := reader.readCoverage(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid character") {
		t.Errorf("error %v, want the body that does not decode named", err)
	}
}

// seedFinishResolve parses args as `loadgen seed-finish` parses them and resolves the flags, so
// a test reads what a command line resolves to without a database to connect to.
func seedFinishResolve(t *testing.T, args ...string) (seedFinishFlags, error) {
	t.Helper()
	fs := flag.NewFlagSet("seed-finish", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var f seedFinishFlags
	f.register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("seed-finish %q: %v", args, err)
	}
	return f, f.resolve()
}

// TestSeedFinishReadsItsCredentialsFromFiles: seed-finish runs on the pod that drives the
// deployment, so its two credentials -- the Postgres URL with its password and the Dispatch
// agent bearer -- would be arguments anything that can list processes there reads for the whole
// embedding gate, which waits hours (dispatch://INFRA-988). Each file's content with its
// trailing white space trimmed is the value, and the four refusals are the ones every credential
// of this package is read with (readCredentials).
func TestSeedFinishReadsItsCredentialsFromFiles(t *testing.T) {
	const (
		pg          = "postgres://dispatch:pg-secret@10.139.0.3:5432/dispatch?sslmode=require"
		token       = "dispatch-agent-token"
		dispatchURL = "http://10.138.0.21:8766"
	)
	// Each file ends in white space, as one written with printf does (vm/vm.sh writes these
	// under the run's own directory).
	pgFile := credentialFileAt(t, "pg-url", pg+"\n")
	tokenFile := credentialFileAt(t, "dispatch-token", token+" \t\n")
	// bothFiles is a command line that gives each credential as a file alone; given replaces one
	// of those files with another path.
	bothFiles := func() []string {
		return []string{"-pg-file", pgFile, "-url", dispatchURL, "-token-file", tokenFile}
	}
	given := func(replace, with string) []string {
		args := bothFiles()
		for i, a := range args {
			if a == replace {
				args[i] = with
			}
		}
		return args
	}

	f, err := seedFinishResolve(t, bothFiles()...)
	if err != nil {
		t.Fatalf("seed-finish given both its credentials as files was refused: %v", err)
	}
	if f.pg != pg || f.token != token {
		t.Errorf("-pg is %q and -token %q, want each its file's content with the trailing white space trimmed",
			redactPG(f.pg), f.token)
	}

	for _, c := range []struct{ flag, fileFlag, value, file string }{
		{"-pg", "-pg-file", pg, pgFile},
		{"-token", "-token-file", token, tokenFile},
	} {
		_, err := seedFinishResolve(t, append(bothFiles(), c.flag, c.value)...)
		var both *bothCredentialFormsError
		if !errors.As(err, &both) || both.Flag != c.flag || both.FileFlag != c.fileFlag {
			t.Errorf("%s given with %s too: refusal %v, want one naming the two forms", c.flag, c.fileFlag, err)
		}
		// A file that cannot be read, and one that holds no credential: each refused by the flag
		// and the path, before the first connection.
		absent := filepath.Join(t.TempDir(), "absent")
		for _, bad := range []string{absent, credentialFileAt(t, "empty", "\n")} {
			_, err := seedFinishResolve(t, given(c.file, bad)...)
			if err == nil || !strings.Contains(err.Error(), c.fileFlag) || !strings.Contains(err.Error(), bad) {
				t.Errorf("%s %s: refusal %v, want one naming the flag and the path", c.fileFlag, bad, err)
			}
		}
	}

	// Neither form of either credential: refused naming both forms, so a caller that gave none
	// reads which flags it has.
	if _, err := seedFinishResolve(t); err == nil || !strings.Contains(err.Error(), "-pg or -pg-file is required") {
		t.Errorf("no -pg and no -pg-file: refusal %v, want one naming both forms", err)
	}
	_, err = seedFinishResolve(t, "-pg-file", pgFile, "-url", dispatchURL)
	if err == nil || !strings.Contains(err.Error(), "-token or -token-file is required with -url") {
		t.Errorf("-url with no bearer in either form: refusal %v, want one naming both forms", err)
	}
}
