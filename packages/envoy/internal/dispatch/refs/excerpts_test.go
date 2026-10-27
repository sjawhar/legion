package refs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A document's mentions are owed an excerpt until one is recorded at its latest version; once
// recorded, the reconciliation's drift query finds nothing, and it reads refs only through the
// partial index that holds the owed rows, never every document's mentions. A version a binary
// predating the excerpts wrote leaves its mentions' excerpts behind, and MarkStaleExcerpts makes
// them owed again without dropping the excerpt the route still serves.
func TestOwedExcerptsAreFoundThroughTheDriftIndexAndRecordedOnce(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	createReferenceProject(t, tx, "CORE")
	document, _ := createReferenceArtifact(t, tx, "", "CORE", "design")
	if _, err := tx.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, 1, 'v1', '[]')`, document); err != nil {
		t.Fatalf("write version 1: %v", err)
	}
	if _, err := replace(ctx, tx, "artifact", document, "see dispatch://CORE-1 and dispatch://CORE-2", "https://dispatch.example"); err != nil {
		t.Fatalf("index mentions: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	drifted, err := DriftedExcerpts(ctx, database.Pool, 20)
	if err != nil || len(drifted) != 1 || drifted[0] != (ExcerptDrift{FromID: document, Version: 1}) {
		t.Fatalf("drift before recording = %#v, %v; want the document at version 1", drifted, err)
	}
	excerpt := model.GraphExcerpt{BlockID: "b1", Text: "see dispatch://CORE-1"}
	if err := RecordExcerpts(ctx, database.Pool, document, 1, map[Target]model.GraphExcerpt{{Kind: "issue", ID: "CORE-1"}: excerpt}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if drifted, err := DriftedExcerpts(ctx, database.Pool, 20); err != nil || len(drifted) != 0 {
		t.Fatalf("drift after recording = %#v, %v; want none", drifted, err)
	}
	if stored, err := StoredExcerpts(ctx, database.Pool, "issue", "CORE-1"); err != nil || stored[document] != excerpt {
		t.Fatalf("stored = %#v, %v; want %#v", stored, err, excerpt)
	}
	if stored, _ := StoredExcerpts(ctx, database.Pool, "issue", "CORE-2"); len(stored) != 0 {
		t.Fatalf("a mention in no block stored %#v; want none", stored)
	}

	// With sequential scans priced out, the drift query must still have a plan, and it must read
	// refs through refs_excerpt_drift alone.
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `set enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, "explain (costs off) "+strings.Replace(driftedExcerptsQuery, "$1", "20", 1))
	if err != nil {
		t.Fatalf("explain drift query: %v", err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if _, err := conn.Exec(ctx, `reset enable_seqscan`); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	if text := plan.String(); strings.Count(text, " on refs") == 0 || strings.Count(text, " on refs") != strings.Count(text, "refs_excerpt_drift on refs") {
		t.Fatalf("drift plan does not read refs through refs_excerpt_drift alone:\n%s", text)
	}

	if _, err := database.Pool.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, 2, 'v2', '[]')`, document); err != nil {
		t.Fatalf("write version 2 as an older binary would: %v", err)
	}
	if marked, err := MarkStaleExcerpts(ctx, database.Pool); err != nil || marked != 2 {
		t.Fatalf("marked stale = %d, %v; want both mentions", marked, err)
	}
	if drifted, err := DriftedExcerpts(ctx, database.Pool, 20); err != nil || len(drifted) != 1 || drifted[0].Version != 2 {
		t.Fatalf("drift after an older binary's version = %#v, %v; want the document at version 2", drifted, err)
	}
	if stored, _ := StoredExcerpts(ctx, database.Pool, "issue", "CORE-1"); len(stored) != 0 {
		t.Fatalf("a mention owed an excerpt serves %#v; want no stored excerpt", stored)
	}
	if marked, err := MarkStaleExcerpts(ctx, database.Pool); err != nil || marked != 0 {
		t.Fatalf("marking again = %d, %v; want none, owed rows are not stale", marked, err)
	}
}

// owedDocument commits a document at version 1 whose one mention, of CORE-1, is owed an excerpt.
func owedDocument(t *testing.T, database *store.Store, slug string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `insert into projects (key, name) values ('CORE', 'CORE project') on conflict (key) do nothing`); err != nil {
		t.Fatalf("create project: %v", err)
	}
	document, _ := createReferenceArtifact(t, tx, "", "CORE", slug)
	if _, err := tx.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, 1, 'v1', '[]')`, document); err != nil {
		t.Fatalf("write version 1: %v", err)
	}
	if _, err := replace(ctx, tx, "artifact", document, "see dispatch://CORE-1", "https://dispatch.example"); err != nil {
		t.Fatalf("index mentions: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return document
}

// backoffOf reads the document's backoff row: its version, its attempts, and how far after now
// its next attempt falls.
func backoffOf(t *testing.T, database *store.Store, document string) (version, attempts int, wait time.Duration) {
	t.Helper()
	var seconds float64
	if err := database.Pool.QueryRow(context.Background(), `
		select version, attempts, extract(epoch from next_attempt_at - now())::float8
		from reference_excerpt_backoff where artifact_id = $1::uuid
	`, document).Scan(&version, &attempts, &seconds); err != nil {
		t.Fatalf("read the backoff of %s: %v", document, err)
	}
	return version, attempts, time.Duration(seconds * float64(time.Second))
}

// A document the reconciliation defers is left out of the drift until its next attempt falls
// due, and each failure at one version waits twice as long as the one before, plus at most a
// fifth again, up to the ceiling; the attempt count is held where the doubling would leave double
// precision. The earliest due time is the reconciliation's next retry. A deferral at a version
// the document has since replaced holds nothing back, and the next failure counts from one.
func TestADeferredDocumentWaitsOutAGrowingBackoffKeyedByItsVersion(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document := owedDocument(t, database, "design")
	const base, ceiling = time.Hour, 6 * time.Hour
	for attempt, want := range []time.Duration{base, 2 * base, 4 * base, ceiling} {
		if err := DeferExcerpts(ctx, database.Pool, document, 1, base, ceiling); err != nil {
			t.Fatalf("defer %d: %v", attempt+1, err)
		}
		version, attempts, wait := backoffOf(t, database, document)
		if version != 1 || attempts != attempt+1 || wait < want-time.Minute || wait > want+want/5 {
			t.Fatalf("after failure %d: version %d, attempts %d, next in %v; want version 1, attempts %d, next in [%v, %v]",
				attempt+1, version, attempts, wait, attempt+1, want, want+want/5)
		}
		if drifted, err := DriftedExcerpts(ctx, database.Pool, 20); err != nil || len(drifted) != 0 {
			t.Fatalf("drift while deferred = %#v, %v; want none", drifted, err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `update reference_excerpt_backoff set attempts = 5000`); err != nil {
		t.Fatal(err)
	}
	if err := DeferExcerpts(ctx, database.Pool, document, 1, base, ceiling); err != nil {
		t.Fatalf("defer after many failures: %v", err)
	}
	if _, attempts, wait := backoffOf(t, database, document); attempts != 5001 || wait < ceiling-time.Minute || wait > ceiling+ceiling/5 {
		t.Fatalf("after many failures: attempts %d, next in %v; want 5001 and the ceiling", attempts, wait)
	}
	var due time.Time
	if err := database.Pool.QueryRow(ctx, `select next_attempt_at from reference_excerpt_backoff`).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if next, err := NextExcerptRetry(ctx, database.Pool); err != nil || !next.Equal(due) {
		t.Fatalf("next retry = %v, %v; want the deferred document's %v", next, err, due)
	}

	if _, err := database.Pool.Exec(ctx, `update reference_excerpt_backoff set next_attempt_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if drifted, err := DriftedExcerpts(ctx, database.Pool, 20); err != nil || len(drifted) != 1 || drifted[0] != (ExcerptDrift{FromID: document, Version: 1, BackedOff: true}) {
		t.Fatalf("drift once due = %#v, %v; want the document, backed off", drifted, err)
	}

	if _, err := database.Pool.Exec(ctx, `update reference_excerpt_backoff set next_attempt_at = now() + interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, 2, 'v2', '[]')`, document); err != nil {
		t.Fatalf("write version 2: %v", err)
	}
	if drifted, err := DriftedExcerpts(ctx, database.Pool, 20); err != nil || len(drifted) != 1 || drifted[0] != (ExcerptDrift{FromID: document, Version: 2, BackedOff: true}) {
		t.Fatalf("drift after a new version = %#v, %v; want the document at version 2, its old row no hold", drifted, err)
	}
	if next, err := NextExcerptRetry(ctx, database.Pool); err != nil || !next.IsZero() {
		t.Fatalf("next retry with only an older version's row = %v, %v; want none", next, err)
	}
	if err := DeferExcerpts(ctx, database.Pool, document, 2, base, ceiling); err != nil {
		t.Fatalf("defer version 2: %v", err)
	}
	if version, attempts, wait := backoffOf(t, database, document); version != 2 || attempts != 1 || wait > base+base/5 {
		t.Fatalf("after version 2's first failure: version %d, attempts %d, next in %v; want version 2, attempts 1, the base", version, attempts, wait)
	}
	if err := ClearExcerptBackoff(ctx, database.Pool, document); err != nil {
		t.Fatalf("clear: %v", err)
	}
	var rows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from reference_excerpt_backoff`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("backoff rows after clearing = %d, %v; want none", rows, err)
	}
}

// A backoff row outlives its failure when a later version write cuts the document's excerpts
// itself; reading the next retry drops it, and a document that owes nothing sets no retry.
func TestTheNextRetryDropsTheBackoffOfADocumentThatOwesNothing(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	settled, owed := owedDocument(t, database, "settled"), owedDocument(t, database, "owed")
	for _, document := range []string{settled, owed} {
		if err := DeferExcerpts(ctx, database.Pool, document, 1, time.Hour, 6*time.Hour); err != nil {
			t.Fatalf("defer %s: %v", document, err)
		}
	}
	if err := RecordExcerpts(ctx, database.Pool, settled, 1, nil); err != nil {
		t.Fatalf("record: %v", err)
	}
	var due time.Time
	if err := database.Pool.QueryRow(ctx, `select next_attempt_at from reference_excerpt_backoff where artifact_id = $1::uuid`, owed).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if next, err := NextExcerptRetry(ctx, database.Pool); err != nil || !next.Equal(due) {
		t.Fatalf("next retry = %v, %v; want the owed document's %v", next, err, due)
	}
	var left []string
	rows, err := database.Pool.Query(ctx, `select artifact_id::text from reference_excerpt_backoff`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	rows.Close()
	if len(left) != 1 || left[0] != owed {
		t.Fatalf("backoff rows left = %v; want only the owed document %s", left, owed)
	}
	if err := RecordExcerpts(ctx, database.Pool, owed, 1, nil); err != nil {
		t.Fatalf("record: %v", err)
	}
	if next, err := NextExcerptRetry(ctx, database.Pool); err != nil || !next.IsZero() {
		t.Fatalf("next retry with nothing owed = %v, %v; want none", next, err)
	}
}

// Every artifact mention left owed an excerpt announces itself on dispatch_excerpts_owed at
// commit, however it got there - inserted with none, or cleared by ClearExcerpts or a direct
// update as the previous release's code does - and nothing else does: recording an excerpt, or a
// mention another kind of node writes, which holds no excerpt.
func TestAMentionLeftOwedAnExcerptNotifies(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	listener, err := database.Pool.Listener(ctx)
	if err != nil {
		t.Fatalf("open a listener: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close(context.Background()) })
	if _, err := listener.Exec(ctx, `listen dispatch_excerpts_owed`); err != nil {
		t.Fatalf("listen: %v", err)
	}
	notified := func() bool {
		t.Helper()
		waitCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		_, err := listener.WaitForNotification(waitCtx)
		return err == nil
	}

	document := owedDocument(t, database, "design")
	if !notified() {
		t.Fatal("an inserted mention owed an excerpt sent no notification")
	}
	for name, write := range map[string]func() error{
		"recording its excerpt": func() error { return RecordExcerpts(ctx, database.Pool, document, 1, nil) },
		"a comment's mention": func() error {
			_, err := database.Pool.Exec(ctx, `insert into refs (from_kind, from_id, to_kind, to_id) values ('comment', gen_random_uuid()::text, 'issue', 'CORE-1')`)
			return err
		},
	} {
		if err := write(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if notified() {
			t.Fatalf("%s sent a notification; want none", name)
		}
	}
	for name, clear := range map[string]func() error{
		"ClearExcerpts": func() error { return ClearExcerpts(ctx, database.Pool, document) },
		"a direct update": func() error {
			_, err := database.Pool.Exec(ctx, `update refs set excerpt_version = null where from_id = $1`, document)
			return err
		},
	} {
		if err := RecordExcerpts(ctx, database.Pool, document, 1, nil); err != nil {
			t.Fatalf("record before %s: %v", name, err)
		}
		if err := clear(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !notified() {
			t.Fatalf("%s left a mention owed and sent no notification", name)
		}
	}
}
