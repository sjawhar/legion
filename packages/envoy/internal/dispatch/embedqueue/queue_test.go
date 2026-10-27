package embedqueue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/embed/embedtest"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pglisten"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// bedrockShaped gives a test embedder the Identity and Limits embed.Embedder asks for: Bedrock's
// own, so the batch size and token ceiling the tests below count against are Bedrock's. Every
// fake in this file embeds it.
type bedrockShaped struct{}

func (bedrockShaped) Identity() string     { return embed.BedrockModel }
func (bedrockShaped) Limits() embed.Limits { return new(embed.BedrockClient).Limits() }

// fakeEmbedder is a controllable stand-in for Cohere: Embed answers vectors or err, and records
// every call so a test can assert how many batches ran.
type fakeEmbedder struct {
	bedrockShaped
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

// fakeVectors returns n healthy, identical vectors - the success path every fake embedder in
// this file shares once it has decided a call succeeds.
func fakeVectors(n int) [][]float32 {
	vectors := make([][]float32, n)
	for i := range vectors {
		vectors[i] = oneVector()
	}
	return vectors
}

// throttleThenSucceedEmbedder throttles its first throttleFor calls, then delegates to inner -
// standing in for a provider that recovers partway through an unattended run.
type throttleThenSucceedEmbedder struct {
	bedrockShaped
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
	if model != embedder.Identity() || embeddedAt == nil {
		t.Errorf("model=%q embeddedAt=%v, want model=%q (the embedder's Identity) and a timestamp", model, embeddedAt, embedder.Identity())
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
// blocking finding: a whole-batch throttle from the model service is infrastructure, never
// evidence about a row's content, so it must never count toward the dead-letter threshold - a
// sustained throttle episode outlasting retry.DeadLetterAttempts cycles must still be retrying
// afterward, not permanently abandoned.
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

	// The model service recovers: the very next attempt, once eligible, succeeds normally.
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
	deps := Deps{Store: database, Embedder: &fakeEmbedder{}}
	rows, settled, err := scanPending(ctx, deps)
	if err != nil || len(rows) != 1 || settled != 0 {
		t.Fatalf("scanPending = %v, %d settled, %v, want one row", rows, settled, err)
	}
	stale := rows[0]
	// A concurrent write changes the title (and so the trigger re-enqueues with a new hash)
	// between the scan above and the commit below.
	if _, err := database.Pool.Exec(ctx, `update issues set title = $1 where key = 'STAL-1'`, "Changed title"); err != nil {
		t.Fatalf("update title: %v", err)
	}
	succeeded, permanent, err := commitEmbeddings(ctx, deps, []pendingRow{stale}, [][]float32{oneVector()})
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

// TestRetryRowsMarksARowDeadAfterRepeatedPermanentFailuresAndStopsRetrying is the dead-letter
// acceptance criterion for the one failure class that does dead-letter: past
// retry.DeadLetterAttempts *permanent* failures, a row stops consuming poller cycles rather than
// retrying with a five-minute ceiling forever, records the identity it died under, and is still
// visible and still recoverable (a text change clears dead and dead_model).
func TestRetryRowsMarksARowDeadAfterRepeatedPermanentFailuresAndStopsRetrying(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "DEAD", "DEAD-1", "A title")
	ctx := context.Background()
	embedder := &fakeEmbedder{}
	deps := Deps{Store: database, Embedder: embedder}
	row := pendingRow{kind: "issue", id: "DEAD-1"}
	if err := database.Pool.QueryRow(ctx, `select content_hash from embeddings where kind = 'issue' and id = 'DEAD-1'`).Scan(&row.contentHash); err != nil {
		t.Fatalf("read content_hash: %v", err)
	}
	for row.attempts, row.confirmedFailures = 0, 0; row.confirmedFailures < retry.DeadLetterAttempts; row.attempts, row.confirmedFailures = row.attempts+1, row.confirmedFailures+1 {
		retryRows(ctx, deps, []pendingRow{row}, true)
	}

	var dead bool
	var attempts int
	var deadModel *string
	if err := database.Pool.QueryRow(ctx, `select dead, attempt_count, dead_model from embeddings where kind = 'issue' and id = 'DEAD-1'`).Scan(&dead, &attempts, &deadModel); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if !dead {
		t.Fatalf("dead = false after %d permanent attempts (retry.DeadLetterAttempts=%d), want true", attempts, retry.DeadLetterAttempts)
	}
	if deadModel == nil || *deadModel != embedder.Identity() {
		t.Errorf("dead_model = %v, want the embedder's Identity %q", deadModel, embedder.Identity())
	}

	rows, _, err := scanPending(ctx, deps)
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
	if err := database.Pool.QueryRow(ctx, `select dead, dead_model from embeddings where kind = 'issue' and id = 'DEAD-1'`).Scan(&dead, &deadModel); err != nil {
		t.Fatalf("read dead after recovery write: %v", err)
	}
	if dead || deadModel != nil {
		t.Errorf("dead = %t, dead_model = %v after the row's own text changed; embeddings_enqueue should have cleared both", dead, deadModel)
	}
}

// poisonTextEmbedder fails non-throttled (ValidationException, the shape a content a model
// refuses outright comes back as) whenever poison is among the texts in a call, and succeeds
// otherwise - whether or not poison is present in that call's batch, siblings that do not carry
// it embed cleanly, exactly as a real "this one row's content is invalid" failure would behave
// isolated from the rest.
type poisonTextEmbedder struct {
	bedrockShaped
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
	return fakeVectors(len(texts)), nil
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
	bedrockShaped
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
// embeddings_rate_limit (migration 0078) is one row in the shared database, not in-process
// memory, so two distinct Deps values - standing in for the pollers of two envoy-dispatch
// processes, which share nothing else - draw from, and wait out debt against, the very same
// budget.
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
// wait can run past claimWindow (a full batch of maximum-length documents can need far more than
// one minute's token ceiling to wait out), so waitOutDebt must renew the claim partway through a
// long wait, not only before or after it. A tiny segment stands in for renewClaimInterval's real
// one minute so the test itself takes a little over a second, not several minutes, while
// exercising the identical loop.
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
	bedrockShaped
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
	return fakeVectors(len(texts)), nil
}

// TestChunkForBudgetSplitsAnOverCeilingBatchAndEmbedRowsCommitsEachGroupOnlyOnce proves both
// halves of embedRows' own contract at once: chunkForBudget actually produces more than one
// group once a batch's combined estimated tokens exceed the embedder's BackgroundTokensPerMinute
// (Bedrock's 200,000: 25 rows at maxInputChars, each truncated to exactly 8,000 estimated tokens,
// fill one 200,000-token group exactly; a 26th, poisoned row starts a second), and a later
// group's failure never re-embeds an earlier group's already-committed rows: Embed is called
// exactly three times (once for each group, plus confirmSoloFailure's own canary probe once the
// poisoned row is isolated to one - never once per row or once per bisection level), carrying 26
// real rows plus the one canary.
func TestChunkForBudgetSplitsAnOverCeilingBatchAndEmbedRowsCommitsEachGroupOnlyOnce(t *testing.T) {
	database := storetest.Open(t)
	const healthyCount = 25                    // 25 * 8,000 = 200,000 = Bedrock's BackgroundTokensPerMinute exactly
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
	bedrockShaped
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
	return fakeVectors(len(texts)), nil
}

// sequencedEmbedder scripts outcomes by call index alone, regardless of content or whether a
// call is a row's own or embedCanary's - Deep's and Qual's own probes are both specified this
// way ("call 1 fails, call 2 succeeds, ...").
type sequencedEmbedder struct {
	bedrockShaped
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
	return fakeVectors(len(texts)), nil
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

// TestProcessBatchALonePoisonedRowWithNoBatchSiblingsStillDeadLetters proves confirmSoloFailure
// needs no sibling at all: a row that is the entire batch, every cycle, with no other row ever
// pending beside it, is confirmed by its own canary exactly as a row bisection isolated to one
// would be, and dead-letters once confirmed_failures reaches retry.DeadLetterAttempts.
func TestProcessBatchALonePoisonedRowWithNoBatchSiblingsStillDeadLetters(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "LONE", "LONE-1", "the poisoned title")
	embedder := &canaryAwareEmbedder{
		rowOK:    func(_ []string) bool { return false },
		canaryOK: func() bool { return true },
	}
	deps := Deps{Store: database, Embedder: embedder}

	for i := 0; i < retry.DeadLetterAttempts; i++ {
		succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
		if err != nil {
			t.Fatalf("cycle %d: ProcessBatch: %v", i, err)
		}
		if succeeded != 0 || failed != 1 || throttled {
			t.Fatalf("cycle %d: succeeded, failed, throttled = %d, %d, %v, want 0, 1, false", i, succeeded, failed, throttled)
		}
		forceEligible(t, database, "issue", "LONE-1")
	}
	var dead bool
	var confirmedFailures int
	if err := database.Pool.QueryRow(context.Background(),
		`select dead, confirmed_failures from embeddings where kind = 'issue' and id = 'LONE-1'`,
	).Scan(&dead, &confirmedFailures); err != nil {
		t.Fatalf("read LONE-1: %v", err)
	}
	if !dead {
		t.Error("LONE-1 dead = false, want true - its canary succeeded every cycle, proving the service works, and it was never bisected (it is the whole batch every time)")
	}
	if confirmedFailures != retry.DeadLetterAttempts {
		t.Errorf("LONE-1 confirmed_failures = %d, want %d", confirmedFailures, retry.DeadLetterAttempts)
	}
}

// TestProcessBatchALoneHealthyRowDuringAnOutageNeverDeadLetters is the mirror of the test above:
// a row that is the entire batch every cycle, whose own content is fine, but whose canary also
// fails every cycle (a genuine outage, not this row's fault) is never confirmed and never
// dead-letters, however many cycles it runs for - run here for more cycles than
// retry.DeadLetterAttempts would take to confirm a genuinely poisoned row.
func TestProcessBatchALoneHealthyRowDuringAnOutageNeverDeadLetters(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "LONE", "LONE-2", "a perfectly good title")
	embedder := &canaryAwareEmbedder{
		rowOK:    func(_ []string) bool { return false },
		canaryOK: func() bool { return false },
	}
	deps := Deps{Store: database, Embedder: embedder}

	for i := 0; i < retry.DeadLetterAttempts+5; i++ {
		succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
		if err != nil {
			t.Fatalf("cycle %d: ProcessBatch: %v", i, err)
		}
		if succeeded != 0 || failed != 1 || throttled {
			t.Fatalf("cycle %d: succeeded, failed, throttled = %d, %d, %v, want 0, 1, false", i, succeeded, failed, throttled)
		}
		forceEligible(t, database, "issue", "LONE-2")
	}
	var dead bool
	var confirmedFailures int
	if err := database.Pool.QueryRow(context.Background(),
		`select dead, confirmed_failures from embeddings where kind = 'issue' and id = 'LONE-2'`,
	).Scan(&dead, &confirmedFailures); err != nil {
		t.Fatalf("read LONE-2: %v", err)
	}
	if dead {
		t.Error("LONE-2 dead = true, want false - its canary failed every cycle too, so nothing about its own content was ever confirmed")
	}
	if confirmedFailures != 0 {
		t.Errorf("LONE-2 confirmed_failures = %d, want 0", confirmedFailures)
	}
}

// TestProcessBatchACanaryThatIsItselfThrottledNeverConfirmsAndPropagatesThrottled exercises
// confirmSoloFailure's one remaining branch no other committed test reaches: the canary probe
// itself throttles (rather than failing outright). Nothing is confirmed - a throttled canary is
// exactly as systemic as a throttled row - and ProcessBatch reports throttled so the caller backs
// off its own cadence exactly as an outright-throttled call would.
func TestProcessBatchACanaryThatIsItselfThrottledNeverConfirmsAndPropagatesThrottled(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "CNRY", "CNRY-2", "a title")
	deps := Deps{Store: database, Embedder: canaryThrottledEmbedder{}}

	succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != 0 || failed != 1 {
		t.Errorf("succeeded, failed = %d, %d, want 0, 1", succeeded, failed)
	}
	if !throttled {
		t.Error("throttled = false, want true - the canary itself was throttled, so the caller should back off exactly as an outright-throttled call would")
	}
	var dead bool
	var confirmedFailures int
	if err := database.Pool.QueryRow(context.Background(),
		`select dead, confirmed_failures from embeddings where kind = 'issue' and id = 'CNRY-2'`,
	).Scan(&dead, &confirmedFailures); err != nil {
		t.Fatalf("read CNRY-2: %v", err)
	}
	if dead {
		t.Error("CNRY-2 dead = true, want false - a throttled canary confirms nothing")
	}
	if confirmedFailures != 0 {
		t.Errorf("CNRY-2 confirmed_failures = %d, want 0", confirmedFailures)
	}
}

// canaryThrottledEmbedder fails every row's own call non-throttled, and throttles embedCanary's
// call specifically - confirmSoloFailure's one branch no other committed test exercises.
type canaryThrottledEmbedder struct{ bedrockShaped }

func (canaryThrottledEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	if len(texts) == 1 && texts[0] == embedCanary {
		return nil, embedtest.FakeThrottleError{Code: "ThrottlingException"}
	}
	return nil, errors.New("simulated: a non-throttled embed failure")
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
	bedrockShaped
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
	return fakeVectors(len(texts)), nil
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

// syncEmbedder is a fake embedder safe for Run's two loops to call at once: every call answers
// vectors unless err is set, and it counts the canary calls it sees.
type syncEmbedder struct {
	identity string
	mu       sync.Mutex
	err      error
	canaries int
}

func newSyncEmbedder(identity string) *syncEmbedder { return &syncEmbedder{identity: identity} }

func (e *syncEmbedder) Identity() string     { return e.identity }
func (e *syncEmbedder) Limits() embed.Limits { return new(embed.BedrockClient).Limits() }

func (e *syncEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, text := range texts {
		if text == embedCanary {
			e.canaries++
		}
	}
	if e.err != nil {
		return nil, e.err
	}
	return fakeVectors(len(texts)), nil
}

func (e *syncEmbedder) setErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.err = err
}

func (e *syncEmbedder) canaryCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.canaries
}

// statementCounter counts every statement started on the pool it traces.
type statementCounter struct{ count atomic.Int64 }

func (c *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.count.Add(1)
	return ctx
}

func (c *statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// countedStore is database through a pool of its own whose every statement the returned counter
// counts. Only the code under test is given it, so everything it counts is that code's.
func countedStore(t *testing.T, database *store.Store) (*store.Store, *statementCounter) {
	t.Helper()
	config := database.Pool.Config().Copy()
	counter := &statementCounter{}
	config.ConnConfig.Tracer = counter
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a counted pool: %v", err)
	}
	pool := store.NewPool(opened)
	t.Cleanup(pool.Close)
	return &store.Store{Pool: pool}, counter
}

// awaitQuiet waits until counter has counted no statement for quiet, failing once within passes.
func awaitQuiet(t *testing.T, counter *statementCounter, quiet, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	last, since := counter.count.Load(), time.Now()
	for time.Since(since) < quiet {
		if time.Now().After(deadline) {
			t.Fatalf("statements did not stop for %v within %v", quiet, within)
		}
		time.Sleep(20 * time.Millisecond)
		if n := counter.count.Load(); n != last {
			last, since = n, time.Now()
		}
	}
}

// requireNoStatements fails if counter counts any statement over window.
func requireNoStatements(t *testing.T, counter *statementCounter, window time.Duration, what string) {
	t.Helper()
	before := counter.count.Load()
	time.Sleep(window)
	if made := counter.count.Load() - before; made != 0 {
		t.Errorf("%s: %d statements over %v, want none", what, made, window)
	}
}

// eventually polls done until it reports true, failing once within passes.
func eventually(t *testing.T, within time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// runQueue runs Run with deps until the test ends, and fails the test if Run does not return
// promptly once its context ends.
func runQueue(t *testing.T, deps Deps) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, deps)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return within 10s of its context ending")
		}
	})
}

// runHub runs hub until the test ends.
func runHub(t *testing.T, hub *pglisten.Hub) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// insertComment writes a comment on issueKey through its trigger, which enqueues it.
func insertComment(t *testing.T, q querier, issueKey, body string) {
	t.Helper()
	mustExec(t, q, `insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', $2)`, issueKey, body)
}

// Once Run has embedded what was pending at boot and its coverage pass has added and the queue
// embedded what no trigger saw, nothing changes, so Run makes no statement at all: no scan
// timer, no due time, no coverage pass. The window is longer than the 5-second period the scan
// ticker ran at before.
func TestRunMakesNoStatementOnceItsBootDrainAndPassAreDone(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "IDLE", "IDLE-1", "Enqueued by its trigger")
	withTriggersOff(t, database, func(q querier) { insertComment(t, q, "IDLE-1", "Written with the triggers off") })
	counted, counter := countedStore(t, database)
	runQueue(t, Deps{Store: counted, Embedder: newSyncEmbedder(configuredIdentity)})
	eventually(t, 15*time.Second, "both items current", func() bool {
		items, current, _, _, _ := coverage(t, database, configuredIdentity)
		return items == 2 && current == 2
	})
	awaitQuiet(t, counter, time.Second, 10*time.Second)
	requireNoStatements(t, counter, 7*time.Second, "Run with nothing pending and nothing changing")
}

// A write's notification wakes the idle queue, through a real pglisten.Hub on WakeChannel: a
// comment written once the queue has drained, and once the scan the hub's catch-up signal after
// its LISTEN earned has run, is embedded within 6 seconds, and nothing else would have scanned
// for it.
func TestAWriteIsEmbeddedOnItsNotification(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "NTFY", "NTFY-1", "Written before the queue started")
	hub := &pglisten.Hub{Owner: "embedqueue test", Dial: database.Pool.Listener}
	wake := hub.Wake(WakeChannel)
	connected := make(chan struct{})
	hub.OnConnected(sync.OnceFunc(func() { close(connected) }))
	runHub(t, hub)
	counted, counter := countedStore(t, database)
	started := time.Now()
	runQueue(t, Deps{Store: counted, Embedder: newSyncEmbedder(configuredIdentity), Wake: wake})
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("the hub did not connect within 10s")
	}
	eventually(t, 10*time.Second, "NTFY-1 embedded at boot", func() bool { return pendingCount(t, database) == 0 })
	// The catch-up signal earns a scan no sooner than retryInterval after the boot scan began.
	time.Sleep(time.Until(started.Add(retryInterval + time.Second)))
	awaitQuiet(t, counter, 500*time.Millisecond, 10*time.Second)

	insertComment(t, database.Pool, "NTFY-1", "Written while the queue is idle")
	if got := pendingCount(t, database); got != 1 {
		t.Fatalf("pending after the comment = %d, want 1", got)
	}
	eventually(t, 6*time.Second, "the comment embedded after its notification", func() bool { return pendingCount(t, database) == 0 })
}

// A claim another process left on a row - it crashed before embedding it - is taken once the
// claim lapses, by the scan the due time the queue read after its last scan arms: no write, no
// wake and no ticker brings it.
func TestAClaimLeftByAnotherProcessIsTakenWhenItLapses(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedIssue(t, database, "CLAM", "CLAM-1", "Claimed by a process that crashed")
	claimed, _, err := scanPending(ctx, Deps{Store: storetest.SecondProcess(t, database), Embedder: newPlaceEmbedder(configuredIdentity)})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("the other process's claim = %v, %v; want CLAM-1", claimed, err)
	}
	// As if most of claimWindow had already passed since that claim.
	var lapses time.Time
	if err := database.Pool.QueryRow(ctx, `
		update embeddings set next_attempt_at = now() + interval '6 seconds' where id = 'CLAM-1'
		returning next_attempt_at`).Scan(&lapses); err != nil {
		t.Fatalf("move the claim's lapse: %v", err)
	}
	runQueue(t, Deps{Store: database, Embedder: newSyncEmbedder(configuredIdentity)})
	eventually(t, 15*time.Second, "CLAM-1 embedded", func() bool { return pendingCount(t, database) == 0 })
	var embeddedAt time.Time
	if err := database.Pool.QueryRow(ctx, `select embedded_at from embeddings where id = 'CLAM-1'`).Scan(&embeddedAt); err != nil {
		t.Fatalf("read embedded_at: %v", err)
	}
	if embeddedAt.Before(lapses) || embeddedAt.After(lapses.Add(3*time.Second)) {
		t.Errorf("CLAM-1 embedded at %v, want within 3s after its claim lapsed at %v", embeddedAt, lapses)
	}
}

// scanPass returns no due time once nothing is pending; the lapse of another process's claim when
// one is; and never a time sooner than retryInterval from its end - not for a claim lapsing
// sooner, and not for a row already due that another process holds under skip locked, which the
// scan could not claim and an immediate rescan would not either.
func TestScanPassReturnsWhenThePendingRowsFallDue(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedIssue(t, database, "DUE", "DUE-1", "Drained at once")
	deps := Deps{Store: database, Embedder: newPlaceEmbedder(configuredIdentity)}
	if next := scanPass(ctx, deps); !next.IsZero() {
		t.Errorf("after draining every row: due %v, want none", next)
	}

	seedIssue(t, database, "DUE", "DUE-2", "Claimed elsewhere")
	if claimed, _, err := scanPending(ctx, Deps{Store: storetest.SecondProcess(t, database), Embedder: newPlaceEmbedder(configuredIdentity)}); err != nil || len(claimed) != 1 {
		t.Fatalf("the other process's claim = %v, %v; want DUE-2", claimed, err)
	}
	var lapses time.Time
	if err := database.Pool.QueryRow(ctx, `select next_attempt_at from embeddings where id = 'DUE-2'`).Scan(&lapses); err != nil {
		t.Fatalf("read the claim's lapse: %v", err)
	}
	if next := scanPass(ctx, deps); !next.Equal(lapses) {
		t.Errorf("with DUE-2 claimed elsewhere: due %v, want the claim's lapse %v", next, lapses)
	}

	requireFloor := func(when string) {
		t.Helper()
		before := time.Now()
		next := scanPass(ctx, deps)
		if next.Before(before.Add(retryInterval)) || next.After(time.Now().Add(retryInterval)) {
			t.Errorf("%s: due %v, want retryInterval after the pass, between %v and %v", when, next, before.Add(retryInterval), time.Now().Add(retryInterval))
		}
	}
	mustExec(t, database.Pool, `update embeddings set next_attempt_at = now() + interval '1 second' where id = 'DUE-2'`)
	requireFloor("a claim lapsing in a second")

	mustExec(t, database.Pool, `update embeddings set next_attempt_at = now() - interval '1 second' where id = 'DUE-2'`)
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holding transaction: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `select 1 from embeddings where id = 'DUE-2' for update`); err != nil {
		t.Fatalf("hold DUE-2: %v", err)
	}
	requireFloor("a due row another process holds")
	if got := pendingCount(t, database); got != 1 {
		t.Errorf("pending = %d, want DUE-2 still pending: the scan skips a held row", got)
	}
}

// A throttled batch waits retryInterval plus its backoff before the next one, so a throttle
// episode never sends calls more often than the scan ticker did.
func TestAThrottledScanWaitsAtLeastRetryIntervalBeforeItsNextBatch(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "THSC", "THSC-1", "A title")
	embedder := &fakeEmbedder{err: embedtest.FakeThrottleError{Code: "ThrottlingException"}}
	ctx, cancel := context.WithTimeout(context.Background(), retryInterval-500*time.Millisecond)
	defer cancel()
	scan(ctx, Deps{Store: database, Embedder: embedder})
	if len(embedder.calls) != 1 {
		t.Errorf("%d embed calls in %v of throttling, want 1", len(embedder.calls), retryInterval-500*time.Millisecond)
	}
}

// embeddings_enqueue announces on WakeChannel when its upsert writes a row - a new item, or a
// changed text - and not when the text is the one the row already holds.
func TestEnqueueNotifiesOnlyWhenItWritesARow(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	listener, err := database.Pool.Listener(ctx)
	if err != nil {
		t.Fatalf("open a listening connection: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close(context.Background()) })
	if _, err := listener.Exec(ctx, "listen "+WakeChannel); err != nil {
		t.Fatalf("listen: %v", err)
	}
	notified := func(within time.Duration) bool {
		t.Helper()
		waitCtx, cancel := context.WithTimeout(ctx, within)
		defer cancel()
		notification, err := listener.WaitForNotification(waitCtx)
		if err != nil {
			if waitCtx.Err() == nil {
				t.Fatalf("wait for a notification: %v", err)
			}
			return false
		}
		if notification.Channel != WakeChannel {
			t.Fatalf("notification on %q, want %q", notification.Channel, WakeChannel)
		}
		return true
	}
	for _, step := range []struct {
		text string
		want bool
	}{
		{"First text", true},
		{"First text", false},
		{"Changed text", true},
	} {
		mustExec(t, database.Pool, `select embeddings_enqueue('issue', 'NOTE-1', $1)`, step.text)
		within := 5 * time.Second
		if !step.want {
			within = 500 * time.Millisecond
		}
		if got := notified(within); got != step.want {
			t.Errorf("enqueue %q: notified %t, want %t", step.text, got, step.want)
		}
	}
}
