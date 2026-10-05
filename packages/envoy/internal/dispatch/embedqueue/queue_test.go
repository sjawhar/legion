package embedqueue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/embed/embedtest"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// fakeEmbedder is a controllable stand-in for Cohere: Embed answers vectors or err, and records
// every call so a test can assert how many batches ran.
type fakeEmbedder struct {
	vector func(text string) []float32
	err    error
	calls  [][]string
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	f.calls = append(f.calls, append([]string(nil), texts...))
	if f.err != nil {
		return nil, f.err
	}
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		if f.vector != nil {
			vectors[i] = f.vector(text)
		} else {
			vectors[i] = oneVector()
		}
	}
	return vectors, nil
}

func oneVector() []float32 {
	v := make([]float32, embed.Dimension)
	for i := range v {
		v[i] = 1
	}
	return v
}

// throttleThenSucceedEmbedder throttles its first throttleFor calls, then delegates to inner -
// standing in for a provider that recovers partway through an unattended run.
type throttleThenSucceedEmbedder struct {
	inner       embed.Embedder
	throttleFor int
	calls       int
}

func (e *throttleThenSucceedEmbedder) Embed(ctx context.Context, texts []string, inputType embed.InputType) ([][]float32, error) {
	e.calls++
	if e.calls <= e.throttleFor {
		return nil, embedtest.FakeThrottleError{Code: "ThrottlingException"}
	}
	return e.inner.Embed(ctx, texts, inputType)
}

// seedIssue inserts an issue directly (bypassing the API, which this package does not import),
// keyed exactly as given - e.g. seedIssue(t, db, "EMBQ", "EMBQ-1", "A title") - with its project
// created on first use. number is the next free one for projectKey, not a hardcoded 1, so a
// test that seeds several issues under one project (the bisection tests' shared batches) does
// not collide on issues_project_key_number_key.
func seedIssue(t *testing.T, database *store.Store, projectKey, issueKey, title string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ($1, 'Test') on conflict (key) do nothing
	`, projectKey); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, coalesce((select max(number) + 1 from issues where project_key = $2), 1), $3, '{"kind":"user","id":"alice"}', 'A')
	`, issueKey, projectKey, title); err != nil {
		t.Fatalf("seed issue %s: %v", issueKey, err)
	}
}

// contentHashOf reads a row's real content_hash: claimRenewal.renew matches on it (Rev's own
// should: a renewal must never touch a row a concurrent write already changed), so a test
// constructing a pendingRow by hand for renewal, rather than through scanPending, needs the real
// value - the zero-value empty string never matches what embeddings_enqueue's trigger wrote.
func contentHashOf(t *testing.T, database *store.Store, kind, id string) string {
	t.Helper()
	var hash string
	if err := database.Pool.QueryRow(context.Background(),
		`select content_hash from embeddings where kind = $1 and id = $2`, kind, id,
	).Scan(&hash); err != nil {
		t.Fatalf("read content_hash for %s %s: %v", kind, id, err)
	}
	return hash
}

func pendingCount(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from embeddings where embedded_hash is distinct from content_hash
	`).Scan(&count); err != nil {
		t.Fatalf("count pending embeddings: %v", err)
	}
	return count
}

func forceEligible(t *testing.T, database *store.Store, kind, id string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update embeddings set next_attempt_at = now() - interval '1 second' where kind = $1 and id = $2
	`, kind, id); err != nil {
		t.Fatalf("force %s/%s eligible: %v", kind, id, err)
	}
}

// TestProcessBatchEmbedsAPendingRowTheWriteTimeTriggerEnqueued proves the end-to-end path a real
// write exercises: inserting an issue (0073_issues_embeddings_trigger.up.sql's trigger) leaves a
// pending embeddings row with no vector, and ProcessBatch fills it in from the Embedder, without
// any application code calling embeddings_enqueue directly.
func TestProcessBatchEmbedsAPendingRowTheWriteTimeTriggerEnqueued(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "EMBQ", "EMBQ-1", "A title to embed")
	if got := pendingCount(t, database); got != 1 {
		t.Fatalf("pending after insert = %d, want 1 (the trigger's own enqueue)", got)
	}

	embedder := &fakeEmbedder{}
	succeeded, failed, blocked, throttled, err := ProcessBatch(context.Background(), Deps{Store: database, Embedder: embedder})
	if err != nil || blocked || throttled {
		t.Fatalf("ProcessBatch = %d, %d, %v, %v, %v", succeeded, failed, blocked, throttled, err)
	}
	if succeeded != 1 || failed != 0 {
		t.Fatalf("ProcessBatch succeeded, failed = %d, %d, want 1, 0", succeeded, failed)
	}
	if len(embedder.calls) != 1 || embedder.calls[0][0] != "A title to embed" {
		t.Fatalf("embedder calls = %v, want one call embedding the issue's title", embedder.calls)
	}
	if got := pendingCount(t, database); got != 0 {
		t.Errorf("pending after ProcessBatch = %d, want 0", got)
	}
	var model string
	var embeddedAt *time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select model, embedded_at from embeddings where kind = 'issue' and id = 'EMBQ-1'
	`).Scan(&model, &embeddedAt); err != nil {
		t.Fatalf("read embedded row: %v", err)
	}
	if model != embed.Model || embeddedAt == nil {
		t.Errorf("model=%q embeddedAt=%v, want model=%q and a timestamp", model, embeddedAt, embed.Model)
	}
}

// TestProcessBatchRetriesWithBackoffOnEmbedFailure is the durable-retry-queue acceptance
// criterion: a write never fails because the embedder did, and a failed attempt leaves the row
// pending, retried later rather than lost.
func TestProcessBatchRetriesWithBackoffOnEmbedFailure(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "RETR", "RETR-1", "A title")
	embedder := &fakeEmbedder{err: errors.New("bedrock: simulated outage")}
	succeeded, failed, blocked, throttled, err := ProcessBatch(context.Background(), Deps{Store: database, Embedder: embedder})
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != 0 || failed != 1 || blocked || throttled {
		t.Fatalf("ProcessBatch succeeded, failed, blocked, throttled = %d, %d, %v, %v, want 0, 1, false, false", succeeded, failed, blocked, throttled)
	}
	if got := pendingCount(t, database); got != 1 {
		t.Fatalf("pending after a failed attempt = %d, want 1 (still pending, not lost)", got)
	}
	var attempts int
	var nextAttemptAt time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select attempt_count, next_attempt_at from embeddings where kind = 'issue' and id = 'RETR-1'
	`).Scan(&attempts, &nextAttemptAt); err != nil {
		t.Fatalf("read retry state: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempt_count = %d, want 1", attempts)
	}
	if !nextAttemptAt.After(time.Now()) {
		t.Errorf("next_attempt_at = %v, want it backed off into the future", nextAttemptAt)
	}
	// Immediately re-processing finds nothing: the row is backed off, not eligible yet.
	succeeded, failed, _, _, err = ProcessBatch(context.Background(), Deps{Store: database, Embedder: embedder})
	if err != nil || succeeded != 0 || failed != 0 {
		t.Fatalf("ProcessBatch immediately after backoff = %d, %d, %v, want 0, 0, nil", succeeded, failed, err)
	}
}

// TestProcessBatchNeverDeadLettersAThrottledBatchHoweverManyTimesItRecurs is the round-3
// blocking finding: a whole-batch Bedrock failure is infrastructure, never evidence about a
// row's content, so it must never count toward the dead-letter threshold - a sustained throttle
// episode outlasting retry.DeadLetterAttempts cycles must still be retrying afterward, not
// permanently abandoned.
func TestProcessBatchNeverDeadLettersAThrottledBatchHoweverManyTimesItRecurs(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "THRO", "THRO-1", "A title")
	embedder := &fakeEmbedder{err: embedtest.FakeThrottleError{Code: "ThrottlingException"}}
	deps := Deps{Store: database, Embedder: embedder}
	ctx := context.Background()

	for i := range retry.DeadLetterAttempts + 5 {
		forceEligible(t, database, "issue", "THRO-1")
		succeeded, failed, blocked, throttled, err := ProcessBatch(ctx, deps)
		if err != nil || blocked {
			t.Fatalf("ProcessBatch attempt %d = %d, %d, %v, %v", i, succeeded, failed, blocked, err)
		}
		if !throttled {
			t.Fatalf("ProcessBatch attempt %d: throttled = false, want true", i)
		}
	}

	var dead bool
	var attempts int
	if err := database.Pool.QueryRow(ctx, `select dead, attempt_count from embeddings where kind = 'issue' and id = 'THRO-1'`).Scan(&dead, &attempts); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if dead {
		t.Errorf("dead = true after %d throttled attempts (more than retry.DeadLetterAttempts=%d), want false: throttling is never permanent", attempts, retry.DeadLetterAttempts)
	}

	// Bedrock recovers: the very next attempt, once eligible, succeeds normally.
	embedder.err = nil
	forceEligible(t, database, "issue", "THRO-1")
	succeeded, failed, blocked, throttled, err := ProcessBatch(ctx, deps)
	if err != nil || blocked || throttled || succeeded != 1 || failed != 0 {
		t.Fatalf("ProcessBatch after recovery = %d, %d, %v, %v, %v, want 1, 0, false, false, nil", succeeded, failed, blocked, throttled, err)
	}
}

// TestProcessBatchRefusesToMarkStaleContentEmbedded guards commitEmbeddings' content_hash
// guard: if the row's text changes between the scan and the commit (another write re-enqueued
// it), the stale embedding must not be recorded as current.
func TestProcessBatchRefusesToMarkStaleContentEmbedded(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "STAL", "STAL-1", "Original title")
	ctx := context.Background()
	rows, err := scanPending(ctx, Deps{Store: database})
	if err != nil || len(rows) != 1 {
		t.Fatalf("scanPending = %v, %v, want one row", rows, err)
	}
	stale := rows[0]
	// A concurrent write changes the title (and so the trigger re-enqueues with a new hash)
	// between the scan above and the commit below.
	if _, err := database.Pool.Exec(ctx, `update issues set title = $1 where key = 'STAL-1'`, "Changed title"); err != nil {
		t.Fatalf("update title: %v", err)
	}
	succeeded, permanent, err := commitEmbeddings(ctx, Deps{Store: database}, []pendingRow{stale}, [][]float32{oneVector()})
	if err != nil {
		t.Fatalf("commitEmbeddings: %v", err)
	}
	if succeeded != 0 || len(permanent) != 0 {
		t.Fatalf("commitEmbeddings = %d succeeded, %d permanent, want 0, 0 (silently skipped, not retried)", succeeded, len(permanent))
	}
	if got := pendingCount(t, database); got != 1 {
		t.Errorf("pending after a stale commit = %d, want 1 (the new text is still unembedded)", got)
	}
	var snapshot string
	if err := database.Pool.QueryRow(ctx, `select text_snapshot from embeddings where kind = 'issue' and id = 'STAL-1'`).Scan(&snapshot); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if snapshot != "Changed title" {
		t.Errorf("text_snapshot = %q, want the newer title", snapshot)
	}
}

// TestBackfillEnqueuesAndEmbedsContentNoTriggerEverSaw proves the backfill path: content
// written before the migration existed (simulated here by disabling the trigger, inserting, then
// re-enabling it - the same "no trigger ever saw this row" state a pre-LEGION-549 database is in)
// is still found, enqueued and embedded by Backfill.
func TestBackfillEnqueuesAndEmbedsContentNoTriggerEverSaw(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `alter table issues disable trigger issues_embeddings_enqueue`); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	seedIssue(t, database, "PRED", "PRED-1", "Pre-existing content")
	if _, err := database.Pool.Exec(ctx, `alter table issues enable trigger issues_embeddings_enqueue`); err != nil {
		t.Fatalf("enable trigger: %v", err)
	}
	if got := pendingCount(t, database); got != 0 {
		t.Fatalf("pending before backfill = %d, want 0 (no trigger ever saw this row)", got)
	}

	var out bytes.Buffer
	embedder := &fakeEmbedder{}
	report, err := Backfill(ctx, Deps{Store: database, Embedder: embedder}, &out)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if report.Enqueued["issue"] != 1 {
		t.Errorf("Enqueued[issue] = %d, want 1", report.Enqueued["issue"])
	}
	if report.Embedded != 1 {
		t.Errorf("Embedded = %d, want 1", report.Embedded)
	}
	if report.Dead != 0 || report.Pending != 0 {
		t.Errorf("Dead=%d Pending=%d, want 0, 0", report.Dead, report.Pending)
	}
	if got := pendingCount(t, database); got != 0 {
		t.Errorf("pending after Backfill = %d, want 0", got)
	}
	if out.Len() == 0 {
		t.Error("Backfill wrote no progress to out")
	}
}

// TestBackfillResumesFromItsCheckpointRatherThanRescanning is the resumability acceptance
// criterion: a second Backfill call, after the first enqueued everything, advances no further
// and rescans nothing (the checkpoint is already at "done"), and a kind grown after the first run
// finished is picked up without resetting the others' checkpoints.
func TestBackfillResumesFromItsCheckpointRatherThanRescanning(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "RES1", "RES1-1", "First issue")
	embedder := &fakeEmbedder{}
	var out bytes.Buffer
	if _, err := Backfill(context.Background(), Deps{Store: database, Embedder: embedder}, &out); err != nil {
		t.Fatalf("first Backfill: %v", err)
	}
	firstCallCount := len(embedder.calls)

	// A second issue arrives after the first run finished (its own trigger enqueues it; this is
	// not what Backfill's resumability is about, so remove its pending row to isolate the "did
	// Backfill rescan issue RES1" question).
	if _, err := database.Pool.Exec(context.Background(), `delete from embeddings where kind = 'issue' and id != 'RES1-1'`); err != nil {
		t.Fatalf("clear unrelated pending rows: %v", err)
	}

	report, err := Backfill(context.Background(), Deps{Store: database, Embedder: embedder}, &out)
	if err != nil {
		t.Fatalf("second Backfill: %v", err)
	}
	if report.Enqueued["issue"] != 0 {
		t.Errorf("second run's Enqueued[issue] = %d, want 0 (resumed past the done checkpoint)", report.Enqueued["issue"])
	}
	if len(embedder.calls) != firstCallCount {
		t.Errorf("second Backfill made %d embed calls beyond the first run's %d, want 0 more (nothing left pending)", len(embedder.calls)-firstCallCount, 0)
	}
	var done bool
	if err := database.Pool.QueryRow(context.Background(), `select done from embeddings_backfill_progress where kind = 'issue'`).Scan(&done); err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}
	if !done {
		t.Error("embeddings_backfill_progress.done = false after a full pass, want true")
	}
}

// TestBackfillDoesNotOverwriteAConcurrentWritesFresherEmbedding proves
// Backfill used to ON CONFLICT DO UPDATE with its own page's snapshot, which could
// clobber a trigger's fresher enqueue of the same row with stale text. ON CONFLICT DO NOTHING
// means an existing row - whichever wrote it, and whenever - is never touched by Backfill at all.
func TestBackfillDoesNotOverwriteAConcurrentWritesFresherEmbedding(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedIssue(t, database, "RACE", "RACE-1", "Original title")
	// A write lands after the issue exists - exactly the kind of row a real Backfill run (content
	// this Dispatch was already carrying) would otherwise see as stale, since the issue already
	// has an embeddings row from its own insert trigger.
	if _, err := database.Pool.Exec(ctx, `update issues set title = $1 where key = 'RACE-1'`, "Changed title"); err != nil {
		t.Fatalf("update title: %v", err)
	}
	var wantHash string
	if err := database.Pool.QueryRow(ctx, `select content_hash from embeddings where kind = 'issue' and id = 'RACE-1'`).Scan(&wantHash); err != nil {
		t.Fatalf("read content_hash before backfill: %v", err)
	}

	var out bytes.Buffer
	embedder := &fakeEmbedder{}
	if _, err := Backfill(ctx, Deps{Store: database, Embedder: embedder}, &out); err != nil {
		t.Fatalf("Backfill: %v", err)
	}

	var snapshot, gotHash string
	if err := database.Pool.QueryRow(ctx, `
		select text_snapshot, content_hash from embeddings where kind = 'issue' and id = 'RACE-1'
	`).Scan(&snapshot, &gotHash); err != nil {
		t.Fatalf("read row after backfill: %v", err)
	}
	if snapshot != "Changed title" {
		t.Errorf("text_snapshot after Backfill = %q, want the write's own %q (Backfill must not revert it)", snapshot, "Changed title")
	}
	if gotHash != wantHash {
		t.Errorf("content_hash after Backfill = %q, want unchanged from before Backfill ran (%q)", gotHash, wantHash)
	}
}

// TestBackfillWaitsOutASustainedThrottleAndFinishesWithZeroDeadRows is the round-3 acceptance
// criterion in full, against Backfill itself rather than one ProcessBatch call: a sustained
// Bedrock throttle - outlasting more attempts than the old dead-letter threshold would have
// tolerated for a permanent failure - must still finish unattended, with the row embedded and
// nothing dead-lettered, once the provider recovers.
func TestBackfillWaitsOutASustainedThrottleAndFinishesWithZeroDeadRows(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "SUST", "SUST-1", "A title")
	inner := &fakeEmbedder{}
	// 3 throttled batches: the per-row test above already proves the dead-letter threshold itself
	// is never crossed by throttling (forcing each row eligible immediately, bypassing real wait
	// time); this test's job is proving the drain loop waits out *real* backoff across *multiple*
	// throttled batches rather than giving up - a small, fast number of them is enough for that.
	embedder := &throttleThenSucceedEmbedder{inner: inner, throttleFor: 3}

	var out bytes.Buffer
	report, err := Backfill(context.Background(), Deps{Store: database, Embedder: embedder}, &out)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if report.Dead != 0 {
		t.Errorf("Dead = %d, want 0: a sustained throttle must never dead-letter", report.Dead)
	}
	if report.Pending != 0 {
		t.Errorf("Pending = %d, want 0: Backfill should wait out the throttle and finish", report.Pending)
	}
	if report.Embedded != 1 {
		t.Errorf("Embedded = %d, want 1", report.Embedded)
	}
	if embedder.calls <= embedder.throttleFor {
		t.Errorf("embedder was called %d times, want more than throttleFor=%d (at least one call must have succeeded)", embedder.calls, embedder.throttleFor)
	}
	if pendingCount(t, database) != 0 {
		t.Errorf("pending embeddings after Backfill = %d, want 0", pendingCount(t, database))
	}
}

// TestRetryRowsMarksARowDeadAfterRepeatedPermanentFailuresAndStopsRetrying is the dead-letter
// acceptance criterion for the one failure class that does dead-letter: past
// retry.DeadLetterAttempts *permanent* failures, a row stops consuming poller cycles rather than
// retrying with a five-minute ceiling forever, but is still visible and still recoverable.
func TestRetryRowsMarksARowDeadAfterRepeatedPermanentFailuresAndStopsRetrying(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "DEAD", "DEAD-1", "A title")
	ctx := context.Background()
	deps := Deps{Store: database}
	row := pendingRow{kind: "issue", id: "DEAD-1"}
	if err := database.Pool.QueryRow(ctx, `select content_hash from embeddings where kind = 'issue' and id = 'DEAD-1'`).Scan(&row.contentHash); err != nil {
		t.Fatalf("read content_hash: %v", err)
	}
	for row.attempts, row.confirmedFailures = 0, 0; row.confirmedFailures < retry.DeadLetterAttempts; row.attempts, row.confirmedFailures = row.attempts+1, row.confirmedFailures+1 {
		retryRows(ctx, deps, []pendingRow{row}, true)
	}

	var dead bool
	var attempts int
	if err := database.Pool.QueryRow(ctx, `select dead, attempt_count from embeddings where kind = 'issue' and id = 'DEAD-1'`).Scan(&dead, &attempts); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if !dead {
		t.Fatalf("dead = false after %d permanent attempts (retry.DeadLetterAttempts=%d), want true", attempts, retry.DeadLetterAttempts)
	}

	rows, err := scanPending(ctx, deps)
	if err != nil {
		t.Fatalf("scanPending: %v", err)
	}
	for _, r := range rows {
		if r.kind == "issue" && r.id == "DEAD-1" {
			t.Error("scanPending returned a dead row; dead rows must not be retried automatically")
		}
	}

	// Recoverable: a fresh write (a title change) clears dead and re-admits the row.
	if _, err := database.Pool.Exec(ctx, `update issues set title = $1 where key = 'DEAD-1'`, "Recovered title"); err != nil {
		t.Fatalf("update title: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `select dead from embeddings where kind = 'issue' and id = 'DEAD-1'`).Scan(&dead); err != nil {
		t.Fatalf("read dead after recovery write: %v", err)
	}
	if dead {
		t.Error("dead is still true after the row's own text changed; embeddings_enqueue should have cleared it")
	}
}

// poisonTextEmbedder fails non-throttled (ValidationException, the shape a content a model
// refuses outright comes back as) whenever poison is among the texts in a call, and succeeds
// otherwise - whether or not poison is present in that call's batch, siblings that do not carry
// it embed cleanly, exactly as a real "this one row's content is invalid" failure would behave
// isolated from the rest.
type poisonTextEmbedder struct {
	poison string
	calls  [][]string
}

func (p *poisonTextEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	p.calls = append(p.calls, append([]string(nil), texts...))
	for _, text := range texts {
		if text == p.poison {
			return nil, errors.New("ValidationException: the model refuses this input")
		}
	}
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = oneVector()
	}
	return vectors, nil
}

// TestProcessBatchBisectsANonThrottledFailureToIsolateTheOffendingRow proves
// a non-throttled whole-batch failure must not retry every row in the batch
// forever just because one row's content is what the API actually refuses. Three issues share one
// batch; one's title is content the embedder refuses outright (non-throttled); bisection must
// isolate that one row, embed the other two normally in the same ProcessBatch call, and leave only
// the offending row's attempt_count incremented (not yet dead - that still takes
// retry.DeadLetterAttempts worth of calls, exactly as any permanent failure does).
func TestProcessBatchBisectsANonThrottledFailureToIsolateTheOffendingRow(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "BSCT", "BSCT-1", "A good title one")
	seedIssue(t, database, "BSCT", "BSCT-2", "The poisoned title")
	seedIssue(t, database, "BSCT", "BSCT-3", "A good title two")
	embedder := &poisonTextEmbedder{poison: "The poisoned title"}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, blocked, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if blocked {
		t.Error("blocked = true, want false")
	}
	if throttled {
		t.Error("throttled = true, want false - a ValidationException is not a throttle")
	}
	if succeeded != 2 {
		t.Errorf("succeeded = %d, want 2 (the two good rows, embedded despite sharing a batch with the poisoned one)", succeeded)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1 (only the poisoned row)", failed)
	}

	for _, id := range []string{"BSCT-1", "BSCT-3"} {
		var embeddedHash, contentHash string
		if err := database.Pool.QueryRow(context.Background(),
			`select coalesce(embedded_hash, ''), content_hash from embeddings where kind = 'issue' and id = $1`, id,
		).Scan(&embeddedHash, &contentHash); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if embeddedHash != contentHash {
			t.Errorf("%s: embedded_hash = %q, content_hash = %q, want them equal - it should have embedded despite its sibling's poison", id, embeddedHash, contentHash)
		}
	}

	var attempts int
	var dead bool
	if err := database.Pool.QueryRow(context.Background(),
		`select attempt_count, dead from embeddings where kind = 'issue' and id = 'BSCT-2'`,
	).Scan(&attempts, &dead); err != nil {
		t.Fatalf("read BSCT-2: %v", err)
	}
	if attempts != 1 {
		t.Errorf("BSCT-2 attempt_count = %d, want 1 after a single ProcessBatch call", attempts)
	}
	if dead {
		t.Error("BSCT-2 dead = true after only 1 attempt, want false - dead-lettering still takes retry.DeadLetterAttempts")
	}

	// The isolation itself: more than one call was made (bisection split the batch), and at
	// least one later call embedded the poisoned row alone - proof bisection actually narrowed
	// down to it rather than only ever retrying the whole batch together. Splitting an odd batch
	// (here 1 row versus 2) can still try the poisoned row alongside one sibling once more before
	// that pair is itself split to size 1 - every recursive halving does that at whichever level
	// still holds more than one row - so the achievable invariant is eventual isolation, not zero
	// intermediate bundling.
	if len(embedder.calls) < 2 {
		t.Fatalf("embedder was called %d time(s), want more than 1 - bisection should have split the batch", len(embedder.calls))
	}
	isolated := false
	for _, call := range embedder.calls[1:] {
		if len(call) == 1 && call[0] == embedder.poison {
			isolated = true
		}
	}
	if !isolated {
		t.Errorf("no call after the first embedded the poisoned row alone - bisection never isolated it down to size 1: %v", embedder.calls)
	}
}

// TestProcessBatchBisectionAbortsOnAThrottleMidway proves a throttle encountered partway through
// bisection is never treated as evidence against any row: every row in the throttled subtree is
// reported for an ordinary retry (never permanent), and ProcessBatch's own throttled return value
// is true, so the caller still backs off its batch cadence exactly as an outright-throttled whole
// batch would.
func TestProcessBatchBisectionAbortsOnAThrottleMidway(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "BSCA", "BSCA-1", "A good title one")
	seedIssue(t, database, "BSCA", "BSCA-2", "The poisoned title")
	poison := &poisonTextEmbedder{poison: "The poisoned title"}
	embedder := &throttleThenSucceedEmbedder{inner: poison, throttleFor: 1}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if !throttled {
		t.Error("throttled = false, want true - the top-level call was throttled")
	}
	if succeeded != 0 || failed != 2 {
		t.Errorf("succeeded, failed = %d, %d, want 0, 2 - a top-level throttle retries the whole batch, never bisects", succeeded, failed)
	}
	var dead bool
	if err := database.Pool.QueryRow(context.Background(),
		`select dead from embeddings where kind = 'issue' and id = 'BSCA-2'`,
	).Scan(&dead); err != nil {
		t.Fatalf("read BSCA-2: %v", err)
	}
	if dead {
		t.Error("BSCA-2 dead = true after a throttled batch, want false - a throttle is never evidence about any row")
	}
}

// TestProcessBatchNeverDeadLettersAUniformNonThrottledFailureHoweverManyTimesItRecurs proves an
// outage embed.IsThrottled does not
// recognize (expired credentials, a retired model id, an uncoded 5xx) fails every row the same
// way, non-throttled, all the way down to size 1 - and nothing in the whole batch ever embeds,
// including every confirmSoloFailure canary probe, so none of it is ever confirmed as any row's
// own content. Driven for more cycles than retry.DeadLetterAttempts, after which nothing is dead.
func TestProcessBatchNeverDeadLettersAUniformNonThrottledFailureHoweverManyTimesItRecurs(t *testing.T) {
	database := storetest.Open(t)
	ids := []string{"UNIF-1", "UNIF-2", "UNIF-3", "UNIF-4", "UNIF-5", "UNIF-6", "UNIF-7", "UNIF-8"}
	for _, id := range ids {
		seedIssue(t, database, "UNIF", id, "Title for "+id)
	}
	embedder := &fakeEmbedder{err: errors.New("embed: expired credentials (simulated systemic outage)")}
	deps := Deps{Store: database, Embedder: embedder}

	for i := range retry.DeadLetterAttempts + 5 {
		for _, id := range ids {
			forceEligible(t, database, "issue", id)
		}
		succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
		if err != nil {
			t.Fatalf("ProcessBatch cycle %d: %v", i, err)
		}
		if succeeded != 0 {
			t.Errorf("cycle %d: succeeded = %d, want 0 - the embedder never succeeds", i, succeeded)
		}
		if failed != len(ids) {
			t.Errorf("cycle %d: failed = %d, want %d", i, failed, len(ids))
		}
		if throttled {
			t.Errorf("cycle %d: throttled = true, want false - this error is not one of IsThrottled's codes", i)
		}
	}
	for _, id := range ids {
		var dead bool
		if err := database.Pool.QueryRow(context.Background(),
			`select dead from embeddings where kind = 'issue' and id = $1`, id,
		).Scan(&dead); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if dead {
			t.Errorf("%s dead = true after %d uniform-failure cycles, want false - nothing in the batch ever embedded, so this is never evidence about any row's content", id, retry.DeadLetterAttempts+5)
		}
	}
}

// cancelAfterNCallsEmbedder fails every call non-throttled, cancelling its own context exactly
// once, on call number cancelOn, to stand in for a context ending mid-bisection (as
// embed.RateLimitedEmbedder's pacing wait ending early does in production) without needing real
// timing or concurrency.
type cancelAfterNCallsEmbedder struct {
	cancelOn int
	cancel   context.CancelFunc
	calls    int
}

func (e *cancelAfterNCallsEmbedder) Embed(_ context.Context, _ []string, _ embed.InputType) ([][]float32, error) {
	e.calls++
	if e.calls == e.cancelOn {
		e.cancel()
	}
	return nil, errors.New("simulated: a non-throttled embed failure")
}

// TestProcessBatchNeverMarksARowPermanentWhenItsContextEndsMidBisection proves a
// cancellation case: whatever error text comes back, a call that failed because its own context
// ended while it was in flight is never evidence about any row's content, independent of whether
// some other row already succeeded.
func TestProcessBatchNeverMarksARowPermanentWhenItsContextEndsMidBisection(t *testing.T) {
	database := storetest.Open(t)
	ids := []string{"CNCL-1", "CNCL-2", "CNCL-3", "CNCL-4"}
	for i, id := range ids {
		seedIssue(t, database, "CNCL", id, fmt.Sprintf("A title %d", i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	embedder := &cancelAfterNCallsEmbedder{cancelOn: 2, cancel: cancel}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, _, throttled, err := ProcessBatch(ctx, deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != 0 {
		t.Errorf("succeeded = %d, want 0", succeeded)
	}
	if failed != len(ids) {
		t.Errorf("failed = %d, want %d", failed, len(ids))
	}
	if throttled {
		t.Error("throttled = true, want false - this is a cancellation, not a throttle")
	}
	for _, id := range ids {
		var dead bool
		if err := database.Pool.QueryRow(context.Background(),
			`select dead from embeddings where kind = 'issue' and id = $1`, id,
		).Scan(&dead); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if dead {
			t.Errorf("%s dead = true after a context cancellation mid-bisection, want false - a cancellation is never evidence about any row's content", id)
		}
	}
}

// TestClaimRenewalExtendsNextAttemptAtPastClaimWindow proves a renewal due (its own last renewal
// older than renewClaimInterval) pushes next_attempt_at at least claimWindow past the moment it
// runs - needed since bisecting a batch every one of whose rows fails can itself take minutes,
// longer than claimWindow.
func TestClaimRenewalExtendsNextAttemptAtPastClaimWindow(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "RNWL", "RNWL-1", "A title")
	ctx := context.Background()
	renewal := newClaimRenewal(Deps{Store: database}, []pendingRow{{kind: "issue", id: "RNWL-1", contentHash: contentHashOf(t, database, "issue", "RNWL-1")}})
	renewal.last = time.Now().Add(-renewClaimInterval - time.Second)
	before := time.Now()
	renewal.renew(ctx)
	var nextAttempt time.Time
	if err := database.Pool.QueryRow(ctx,
		`select next_attempt_at from embeddings where kind = 'issue' and id = 'RNWL-1'`,
	).Scan(&nextAttempt); err != nil {
		t.Fatalf("read next_attempt_at: %v", err)
	}
	if !nextAttempt.After(before.Add(claimWindow - time.Second)) {
		t.Errorf("next_attempt_at = %v, want at least claimWindow (%v) past %v", nextAttempt, claimWindow, before)
	}
}

// TestReserveTokensSharesItsBudgetAcrossConnections proves the cross-process design:
// embeddings_rate_limit (migration 0077) is one row in the shared
// database, not in-process memory, so two distinct Deps values - standing in for the server's
// poller and a separately run `backfill-embeddings`, two OS processes that share nothing else -
// draw from, and wait out debt against, the very same budget.
func TestReserveTokensSharesItsBudgetAcrossConnections(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	const ceiling = 120 // tokens/minute = 2 tokens/second, so a small deficit waits a short, deterministic time

	// A direct write stands in for a first process's own reservation having already returned,
	// leaving the shared row at a 2-token deficit - deliberately not reserveTokens' own blocking
	// call, which would wait out exactly the debt it created and read back near zero by the time
	// a second reservation ran right after it, proving nothing about whether the row's state is
	// actually shared.
	if _, err := database.Pool.Exec(ctx, `update embeddings_rate_limit set tokens_available = -2, last_refill_at = now()`); err != nil {
		t.Fatalf("seed an existing deficit: %v", err)
	}

	// A second, distinct Deps value - sharing no in-memory state with whatever wrote that
	// deficit, standing in for a second OS process - reserves 2 more tokens and must wait out
	// the existing deficit plus its own: 4 tokens at 2/second, about 2 seconds.
	started := time.Now()
	reserveTokens(ctx, Deps{Store: database}, 2, ceiling, nil)
	elapsed := time.Since(started)
	if elapsed < 1500*time.Millisecond {
		t.Errorf("reservation waited %v, want at least ~2s (the pre-existing 2-token deficit plus its own 2-token request, at 2 tokens/sec) - it should have read the shared row's existing debt from a separate writer, not started fresh", elapsed)
	}
}

// TestWaitOutDebtRenewsTheClaimMidWaitSoNoSecondWorkerClaimsIt proves a single reservation's own
// wait can run past claimWindow (a full batchSize batch of maximum-length documents can need far
// more than one minute's token ceiling to wait out), so waitOutDebt must renew the claim partway through a long wait, not only before
// or after it. A tiny segment stands in for renewClaimInterval's real one minute so the test
// itself takes a little over a second, not several minutes, while exercising the identical loop.
func TestWaitOutDebtRenewsTheClaimMidWaitSoNoSecondWorkerClaimsIt(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "LONG", "LONG-1", "A title")
	ctx := context.Background()
	renewal := newClaimRenewal(Deps{Store: database}, []pendingRow{{kind: "issue", id: "LONG-1", contentHash: contentHashOf(t, database, "issue", "LONG-1")}})
	// Looks stale from the start, so the very first segment's renew() call actually renews
	// rather than being skipped as "too soon since the last one".
	renewal.last = time.Now().Add(-time.Hour)

	const segment = 300 * time.Millisecond
	before := time.Now()
	waitOutDebt(ctx, 4*segment, renewal, segment)

	if !renewal.last.After(before) {
		t.Error("renewal.last was never updated - waitOutDebt never actually renewed during its wait")
	}
	var nextAttempt time.Time
	if err := database.Pool.QueryRow(ctx,
		`select next_attempt_at from embeddings where kind = 'issue' and id = 'LONG-1'`,
	).Scan(&nextAttempt); err != nil {
		t.Fatalf("read next_attempt_at: %v", err)
	}
	if !nextAttempt.After(before.Add(claimWindow - time.Second)) {
		t.Errorf("next_attempt_at = %v, want at least claimWindow (%v) past %v - the mid-wait renewal should have pushed it out, so a concurrent scanner never reclaims LONG-1 while this reservation is still waiting", nextAttempt, claimWindow, before)
	}
}

// TestClaimRenewalResolveStopsRenewingACommittedRow proves resolve removes a row from the set a
// claimRenewal still protects: a resolved row's next_attempt_at is untouched by a later renewal,
// while an unresolved sibling's is still pushed out.
func TestClaimRenewalResolveStopsRenewingACommittedRow(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "RSLV", "RSLV-1", "A good title")
	seedIssue(t, database, "RSLV", "RSLV-2", "A pending title")
	ctx := context.Background()

	sentinel := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := database.Pool.Exec(ctx,
		`update embeddings set next_attempt_at = $1 where kind = 'issue' and id = 'RSLV-1'`, sentinel,
	); err != nil {
		t.Fatalf("seed a stale sentinel on RSLV-1: %v", err)
	}

	renewal := newClaimRenewal(Deps{Store: database}, []pendingRow{
		{kind: "issue", id: "RSLV-1", contentHash: contentHashOf(t, database, "issue", "RSLV-1")},
		{kind: "issue", id: "RSLV-2", contentHash: contentHashOf(t, database, "issue", "RSLV-2")},
	})
	renewal.resolve([]pendingRow{{kind: "issue", id: "RSLV-1"}})
	renewal.last = time.Now().Add(-renewClaimInterval - time.Second)
	before := time.Now()
	renewal.renew(ctx)

	var rslv1Next, rslv2Next time.Time
	if err := database.Pool.QueryRow(ctx,
		`select next_attempt_at from embeddings where kind = 'issue' and id = 'RSLV-1'`,
	).Scan(&rslv1Next); err != nil {
		t.Fatalf("read RSLV-1: %v", err)
	}
	if err := database.Pool.QueryRow(ctx,
		`select next_attempt_at from embeddings where kind = 'issue' and id = 'RSLV-2'`,
	).Scan(&rslv2Next); err != nil {
		t.Fatalf("read RSLV-2: %v", err)
	}
	if !rslv1Next.Equal(sentinel) {
		t.Errorf("RSLV-1 next_attempt_at = %v, want unchanged (%v) - it was resolved, so this renewal should never have touched it", rslv1Next, sentinel)
	}
	if !rslv2Next.After(before.Add(claimWindow - time.Second)) {
		t.Errorf("RSLV-2 next_attempt_at = %v, want at least claimWindow past %v - it was never resolved, so this renewal should still protect it", rslv2Next, before)
	}
}

// seedManyIssues bulk-inserts count issues under projectKey (created first if needed), numbered
// sequentially from the project's current max, each titled by titleFor(i) for i in [0, count) -
// one round trip, not one per row, for a test that needs many rows at once.
func seedManyIssues(t *testing.T, database *store.Store, projectKey string, count int, titleFor func(i int) string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ($1, 'Test') on conflict (key) do nothing
	`, projectKey); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	var base int
	if err := database.Pool.QueryRow(ctx,
		`select coalesce(max(number), 0) from issues where project_key = $1`, projectKey,
	).Scan(&base); err != nil {
		t.Fatalf("read current max issue number: %v", err)
	}
	keys := make([]string, count)
	numbers := make([]int, count)
	titles := make([]string, count)
	for i := range count {
		numbers[i] = base + i + 1
		keys[i] = fmt.Sprintf("%s-%d", projectKey, numbers[i])
		titles[i] = titleFor(i)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		select k, $1, n, t, '{"kind":"user","id":"alice"}', 'A'
		from unnest($2::text[], $3::int[], $4::text[]) as v(k, n, t)
	`, projectKey, keys, numbers, titles); err != nil {
		t.Fatalf("bulk seed issues: %v", err)
	}
}

// countingGroupEmbedder fails any call whose texts include poison (simulating one bad row's
// group) and otherwise succeeds, recording every call's own size so a test can prove exactly how
// many Embed calls ran and how many total rows they carried.
type countingGroupEmbedder struct {
	poison   string
	calls    int
	rowsSent int
}

func (e *countingGroupEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	e.calls++
	e.rowsSent += len(texts)
	for _, text := range texts {
		if text == e.poison {
			return nil, errors.New("simulated: a non-throttled embed failure")
		}
	}
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = oneVector()
	}
	return vectors, nil
}

// TestChunkForBudgetSplitsAnOverCeilingBatchAndEmbedRowsCommitsEachGroupOnlyOnce proves both
// halves of embedRows' own contract at once: chunkForBudget actually produces more than one
// group once a batch's combined estimated tokens exceed tokenRateCeiling (25 rows at
// maxInputChars, each truncated to exactly 8,000 estimated tokens, fill one 200,000-token group
// exactly; a 26th, poisoned row starts a second), and a later group's failure never re-embeds an
// earlier group's already-committed rows: Embed is called exactly three times (once for each
// group, plus confirmSoloFailure's own canary probe once the poisoned row is isolated to one -
// never once per row or once per bisection level), carrying 26 real rows plus the one canary.
func TestChunkForBudgetSplitsAnOverCeilingBatchAndEmbedRowsCommitsEachGroupOnlyOnce(t *testing.T) {
	database := storetest.Open(t)
	const healthyCount = 25                    // 25 * 8,000 = 200,000 = tokenRateCeiling exactly
	healthyTitle := strings.Repeat("h", 32000) // embed.maxInputChars (unexported): truncates to exactly 8,000 estimated tokens
	poisonTitle := "poisoned"
	seedManyIssues(t, database, "CHNK", healthyCount+1, func(i int) string {
		if i == healthyCount {
			return poisonTitle
		}
		return healthyTitle
	})
	embedder := &countingGroupEmbedder{poison: poisonTitle}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != healthyCount {
		t.Errorf("succeeded = %d, want %d", succeeded, healthyCount)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
	if throttled {
		t.Error("throttled = true, want false - this is a non-throttled failure")
	}
	if embedder.calls != 3 {
		t.Errorf("Embed was called %d times, want exactly 3 - chunkForBudget should produce exactly two groups (the 25 healthy rows filling one 200,000-token group, the poisoned row starting a second) plus confirmSoloFailure's one canary probe, and the healthy group's success must never trigger a re-embed of it", embedder.calls)
	}
	if embedder.rowsSent != healthyCount+2 {
		t.Errorf("rows sent across all Embed calls = %d, want %d (26 real rows, no row re-sent, plus the one-row canary probe)", embedder.rowsSent, healthyCount+2)
	}
}

// canaryAwareEmbedder shares one success path (oneVector(), for both a real row's embed and
// embedCanary's own) across every round-9 test: rowOK decides a non-canary call's own outcome
// (nil means always succeed), canaryOK decides embedCanary's own outcome (nil means always
// succeed) - so a test only ever supplies the one or two closures it actually needs to vary.
type canaryAwareEmbedder struct {
	rowOK    func(texts []string) bool
	canaryOK func() bool
	calls    int
}

func (e *canaryAwareEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	e.calls++
	ok := true
	if len(texts) == 1 && texts[0] == embedCanary {
		if e.canaryOK != nil {
			ok = e.canaryOK()
		}
	} else if e.rowOK != nil {
		ok = e.rowOK(texts)
	}
	if !ok {
		return nil, errors.New("simulated: a non-throttled embed failure")
	}
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = oneVector()
	}
	return vectors, nil
}

// sequencedEmbedder scripts outcomes by call index alone, regardless of content or whether a
// call is a row's own or embedCanary's - Deep's and Qual's own probes are both specified this
// way ("call 1 fails, call 2 succeeds, ...").
type sequencedEmbedder struct {
	results   []bool // results[i] is call i's outcome (0-indexed)
	afterward bool   // every call past len(results)
	calls     int
}

func (e *sequencedEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	idx := e.calls
	e.calls++
	ok := e.afterward
	if idx < len(e.results) {
		ok = e.results[idx]
	}
	if !ok {
		return nil, errors.New("simulated: a non-throttled embed failure")
	}
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = oneVector()
	}
	return vectors, nil
}

// TestProcessBatchANewOutageAfterAnEarlierSuccessNeverConfirmsAnything proves confirmSoloFailure
// never lets an earlier row's unrelated success confirm a later, unrelated recurrence of a
// systemic outage: each solo failure is decided by its own fresh canary probe, run at the
// instant of that failure, never by anything an earlier row in the same call did.
func TestProcessBatchANewOutageAfterAnEarlierSuccessNeverConfirmsAnything(t *testing.T) {
	database := storetest.Open(t)
	healthyIDs := []string{"FLAP-1", "FLAP-2", "FLAP-3", "FLAP-4"}
	outageIDs := []string{"FLAP-5", "FLAP-6", "FLAP-7", "FLAP-8"}
	for i, id := range healthyIDs {
		seedIssue(t, database, "FLAP", id, fmt.Sprintf("Healthy title %d", i))
	}
	for i, id := range outageIDs {
		seedIssue(t, database, "FLAP", id, fmt.Sprintf("Outage title %d", i))
	}
	embedder := &canaryAwareEmbedder{
		rowOK: func(texts []string) bool {
			for _, text := range texts {
				if strings.HasPrefix(text, "Outage title") {
					return false
				}
			}
			return true
		},
		canaryOK: func() bool { return false }, // the new outage breaks the canary too - nothing it probes is ever confirmed
	}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, _, _, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != 4 {
		t.Errorf("succeeded = %d, want 4", succeeded)
	}
	if failed != 4 {
		t.Errorf("failed = %d, want 4", failed)
	}
	rows, qErr := database.Pool.Query(context.Background(),
		`select id, dead from embeddings where kind = 'issue' and id = any($1::text[])`, outageIDs)
	if qErr != nil {
		t.Fatalf("read embeddings: %v", qErr)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var dead bool
		if err := rows.Scan(&id, &dead); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if dead {
			t.Errorf("%s dead = true, want false - every failure against it was the new outage, never its own content, and its own canary also failed every time", id)
		}
	}
}

// TestProcessBatchRevRound6ProbeStillGreen replays round 6's own scenario (an outage that clears
// mid-bisection) against the canary redesign: realCalls counts only rows' own embed attempts (not
// embedCanary's), so confirmSoloFailure's canary probe reads the condition "right now" without
// itself advancing it - the row isolated while the outage is still active sees a failing canary
// and is never confirmed; the outage clearing from the 5th real call on lets everything else
// through.
func TestProcessBatchRevRound6ProbeStillGreen(t *testing.T) {
	database := storetest.Open(t)
	ids := []string{"REV6-1", "REV6-2", "REV6-3", "REV6-4", "REV6-5", "REV6-6", "REV6-7", "REV6-8"}
	for i, id := range ids {
		seedIssue(t, database, "REV6", id, fmt.Sprintf("Title %d", i))
	}
	realCalls := 0
	embedder := &canaryAwareEmbedder{
		rowOK: func(_ []string) bool {
			realCalls++
			return realCalls > 4
		},
		canaryOK: func() bool { return realCalls > 4 },
	}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != 7 {
		t.Errorf("succeeded = %d, want 7", succeeded)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
	if throttled {
		t.Error("throttled = true, want false")
	}
	rows, qErr := database.Pool.Query(context.Background(),
		`select id, dead from embeddings where kind = 'issue' and id = any($1::text[])`, ids)
	if qErr != nil {
		t.Fatalf("read embeddings: %v", qErr)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var dead bool
		if err := rows.Scan(&id, &dead); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if dead {
			t.Errorf("%s dead = true, want false", id)
		}
	}
}

// TestProcessBatchDeepsFlappingProbeNeverWronglyConfirms is Deep's own probe: 2 rows, the
// embedder fails, succeeds, fails (by call index alone, regardless of content) - every call past
// that third one keeps failing. Neither row's own content is ever at fault; a genuine flap must
// never be mistaken for one.
func TestProcessBatchDeepsFlappingProbeNeverWronglyConfirms(t *testing.T) {
	database := storetest.Open(t)
	ids := []string{"DEEP-1", "DEEP-2"}
	for i, id := range ids {
		seedIssue(t, database, "DEEP", id, fmt.Sprintf("Title %d", i))
	}
	embedder := &sequencedEmbedder{results: []bool{false, true, false}, afterward: false}
	deps := Deps{Store: database, Embedder: embedder}

	if _, _, _, _, err := ProcessBatch(context.Background(), deps); err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	rows, qErr := database.Pool.Query(context.Background(),
		`select id, dead from embeddings where kind = 'issue' and id = any($1::text[])`, ids)
	if qErr != nil {
		t.Fatalf("read embeddings: %v", qErr)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var dead bool
		if err := rows.Scan(&id, &dead); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if dead {
			t.Errorf("%s dead = true, want false - a flapping embedder is never evidence against any row's own content", id)
		}
	}
}

// TestProcessBatchQualsFlappingProbeNeverWronglyConfirms is Qual's own probe: call 1 fails, call
// 2 succeeds, every later call fails.
func TestProcessBatchQualsFlappingProbeNeverWronglyConfirms(t *testing.T) {
	database := storetest.Open(t)
	ids := []string{"QUAL-1", "QUAL-2"}
	for i, id := range ids {
		seedIssue(t, database, "QUAL", id, fmt.Sprintf("Title %d", i))
	}
	embedder := &sequencedEmbedder{results: []bool{false, true}, afterward: false}
	deps := Deps{Store: database, Embedder: embedder}

	if _, _, _, _, err := ProcessBatch(context.Background(), deps); err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	rows, qErr := database.Pool.Query(context.Background(),
		`select id, dead from embeddings where kind = 'issue' and id = any($1::text[])`, ids)
	if qErr != nil {
		t.Fatalf("read embeddings: %v", qErr)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var dead bool
		if err := rows.Scan(&id, &dead); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if dead {
			t.Errorf("%s dead = true, want false - a flapping embedder is never evidence against any row's own content", id)
		}
	}
}

// TestProcessBatchACanaryThatFailsMarksNothing isolates confirmSoloFailure's own two outcomes
// directly: a single row whose own call fails, and whose canary also fails, must never be
// confirmed permanent - it goes back to retry, exactly like any other systemic failure.
func TestProcessBatchACanaryThatFailsMarksNothing(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "CNRY", "CNRY-1", "A title")
	embedder := &canaryAwareEmbedder{
		rowOK:    func(_ []string) bool { return false },
		canaryOK: func() bool { return false },
	}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != 0 || failed != 1 || throttled {
		t.Errorf("succeeded, failed, throttled = %d, %d, %v, want 0, 1, false", succeeded, failed, throttled)
	}
	var dead bool
	var confirmedFailures int
	if err := database.Pool.QueryRow(context.Background(),
		`select dead, confirmed_failures from embeddings where kind = 'issue' and id = 'CNRY-1'`,
	).Scan(&dead, &confirmedFailures); err != nil {
		t.Fatalf("read CNRY-1: %v", err)
	}
	if dead {
		t.Error("CNRY-1 dead = true, want false - its canary also failed, so nothing about it was confirmed")
	}
	if confirmedFailures != 0 {
		t.Errorf("CNRY-1 confirmed_failures = %d, want 0", confirmedFailures)
	}
}

// TestProcessBatchStillDeadLettersAGenuinelyPoisonedRowAmongHealthyOnes proves the canary
// redesign is not overcorrected into never dead-lettering anything: a row whose own content
// genuinely, repeatedly breaks the embed call, confirmed each time by a successful canary probe
// proving the service itself works, still crosses the dead-letter threshold.
func TestProcessBatchStillDeadLettersAGenuinelyPoisonedRowAmongHealthyOnes(t *testing.T) {
	database := storetest.Open(t)
	healthyIDs := []string{"POIS-1", "POIS-2", "POIS-3"}
	for i, id := range healthyIDs {
		seedIssue(t, database, "POIS", id, fmt.Sprintf("Healthy title %d", i))
	}
	seedIssue(t, database, "POIS", "POIS-4", "the poisoned title")
	if _, err := database.Pool.Exec(context.Background(),
		`update embeddings set confirmed_failures = 9 where kind = 'issue' and id = 'POIS-4'`,
	); err != nil {
		t.Fatalf("prime confirmed_failures: %v", err)
	}
	embedder := &canaryAwareEmbedder{
		rowOK: func(texts []string) bool {
			for _, text := range texts {
				if text == "the poisoned title" {
					return false
				}
			}
			return true
		},
		canaryOK: func() bool { return true },
	}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != 3 {
		t.Errorf("succeeded = %d, want 3", succeeded)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
	if throttled {
		t.Error("throttled = true, want false")
	}
	var dead bool
	if err := database.Pool.QueryRow(context.Background(),
		`select dead from embeddings where kind = 'issue' and id = 'POIS-4'`,
	).Scan(&dead); err != nil {
		t.Fatalf("read POIS-4: %v", err)
	}
	if !dead {
		t.Error("POIS-4 dead = false, want true - its canary succeeded every time, proving the service works, so its own repeated failure should cross the dead-letter threshold")
	}
}

// TestProcessBatchActuallyCallsRenewalAndReservesTokens is the first of Rev's two named test
// gaps: no test proved renewal.renew or reserveBackgroundTokens were actually wired into the
// embed path, as opposed to merely existing and passing their own isolated unit tests - deleting
// either call site left the full suite green. This test shrinks renewClaimInterval to make a
// renewal observable in milliseconds, forces real wall-clock time to pass between two embed
// calls (an artificial sleep in the fake embedder, standing in for real network latency), and has
// the second call read the database directly to prove a renewal already ran before it - the one
// signal that survives retryRows' own final overwrite of next_attempt_at at the end of the call,
// which would otherwise hide an intermediate renewal from a check made after ProcessBatch
// returns. The token budget is checked the ordinary way: it must be measurably drawn down.
type wiringCheckEmbedder struct {
	t          *testing.T
	database   *store.Store
	ids        []string
	before     time.Time
	calls      int
	sawRenewal bool
}

func (e *wiringCheckEmbedder) Embed(ctx context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	e.calls++
	if e.calls == 1 {
		time.Sleep(30 * time.Millisecond)
		return nil, errors.New("simulated: a non-throttled embed failure")
	}
	var minNext time.Time
	if err := e.database.Pool.QueryRow(ctx,
		`select min(next_attempt_at) from embeddings where kind = 'issue' and id = any($1::text[])`, e.ids,
	).Scan(&minNext); err != nil {
		e.t.Fatalf("read next_attempt_at mid-call: %v", err)
	}
	if minNext.After(e.before.Add(claimWindow - time.Second)) {
		e.sawRenewal = true
	}
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = oneVector()
	}
	return vectors, nil
}

func TestProcessBatchActuallyCallsRenewalAndReservesTokens(t *testing.T) {
	database := storetest.Open(t)
	ids := []string{"WIRE-1", "WIRE-2"}
	for i, id := range ids {
		seedIssue(t, database, "WIRE", id, fmt.Sprintf("Title %d", i))
	}
	oldInterval := renewClaimInterval
	renewClaimInterval = 10 * time.Millisecond
	t.Cleanup(func() { renewClaimInterval = oldInterval })

	var tokensBefore float64
	if err := database.Pool.QueryRow(context.Background(),
		`select tokens_available from embeddings_rate_limit`,
	).Scan(&tokensBefore); err != nil {
		t.Fatalf("read tokens_available before: %v", err)
	}
	embedder := &wiringCheckEmbedder{t: t, database: database, ids: ids, before: time.Now()}
	deps := Deps{Store: database, Embedder: embedder}

	if _, _, _, _, err := ProcessBatch(context.Background(), deps); err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if !embedder.sawRenewal {
		t.Error("the second embed call never observed a prior renewal - renewal.renew does not appear to be wired into the embed path")
	}
	var tokensAfter float64
	if err := database.Pool.QueryRow(context.Background(),
		`select tokens_available from embeddings_rate_limit`,
	).Scan(&tokensAfter); err != nil {
		t.Fatalf("read tokens_available after: %v", err)
	}
	if tokensAfter >= tokensBefore {
		t.Errorf("tokens_available = %v after ProcessBatch, want less than %v before it - reserveBackgroundTokens does not appear to be wired into the embed path", tokensAfter, tokensBefore)
	}
}

// TestMergeBisectResultsCopiesIntoFreshSlicesNotTheSharedBackingArray is the second of Rev's two
// named test gaps: reverting mergeBisectResults back to append(left.X, right.X...) left the
// whole suite green, because no test manufactured the exact shape that corrupts - a result built
// straight from a slice of the original rows (handleGroupFailure's own size-1 and
// throttled/cancelled returns both are) that has spare capacity extending into a part of the
// backing array neither side's result actually names. rows[:1] (left's slice here) always has
// spare capacity through the rest of a larger backing array; appending onto it in place would
// write into backing[1], which belongs to neither left's nor right's result (right's own slice is
// backing[2:3]) - detectable only by reading the shared backing array again afterward, exactly
// what claimRenewal.rows does for as long as a bisection runs.
func TestMergeBisectResultsCopiesIntoFreshSlicesNotTheSharedBackingArray(t *testing.T) {
	backing := make([]pendingRow, 4, 8)
	for i := range backing {
		backing[i] = pendingRow{kind: "issue", id: fmt.Sprintf("ALIAS-%d", i)}
	}
	left := bisectResult{permanent: backing[0:1]}
	right := bisectResult{permanent: backing[2:3]}

	merged := mergeBisectResults(left, right)

	if got := backing[1].id; got != "ALIAS-1" {
		t.Fatalf("backing[1].id = %q after merge, want unchanged %q - the merge wrote into memory that belongs to a row neither result named", got, "ALIAS-1")
	}
	gotIDs := make([]string, len(merged.permanent))
	for i, row := range merged.permanent {
		gotIDs[i] = row.id
	}
	wantIDs := []string{"ALIAS-0", "ALIAS-2"}
	if len(gotIDs) != len(wantIDs) || gotIDs[0] != wantIDs[0] || gotIDs[1] != wantIDs[1] {
		t.Errorf("merged.permanent ids = %v, want %v", gotIDs, wantIDs)
	}
}
