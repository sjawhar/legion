// Package embedqueue durably retries Bedrock embedding calls for Dispatch's meaning search. A
// write's AFTER INSERT/UPDATE trigger (0072-0076) upserts a pending row into embeddings with the
// latest text and a hash of it in the same transaction as the write, and this package's poller
// embeds whatever that enqueue left pending, with the same scan-then-retry shape as the event
// outbox (internal/dispatch/outbox): a write never fails because the embedder did, since Bedrock
// is never called inside the enqueueing transaction.
package embedqueue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	batchSize          = embed.MaxBatchTexts
	retryInterval      = 5 * time.Second
	retryBaseDelay     = time.Second
	retryMaxDelay      = 5 * time.Minute
	deadLetterAttempts = 10
	backfillPageSize   = 500
	// claimWindow is how long scanPending's claim keeps a row off every other scanner's list.
	// It is a plain UPDATE ... RETURNING, not a transaction held open across the embed call: this
	// package follows the event outbox's own rule (internal/dispatch/outbox: "an open cursor
	// holds its pooled connection until it closes, so publishing ... inside the loop would hold
	// one of the pool's four connections across all of it") - a lock held across a network call
	// is exactly what that rule exists to avoid. Advancing next_attempt_at instead costs one
	// short statement and lets the poller and a concurrent `backfill-embeddings` run claim
	// disjoint batches without either blocking on the other's embed call. Comfortably longer than
	// one real batch of Bedrock calls should ever take; a crashed claimer's rows become eligible
	// again once it passes, not stuck forever.
	claimWindow = 2 * time.Minute
	// batchPause separates one embed call from the next, in both Run's poller and Backfill's
	// drain loop: pending rows arrive in bursts (a bulk import, a backfill's own enqueue pass),
	// and nothing upstream paces how fast this package reads them back out and calls the embedder
	// - without this, draining a large burst would fire batch after batch back to back, which is
	// how a well-behaved client still trips a provider's rate limit. It is not a response to a
	// throttling signal (the AWS SDK's own retry middleware already backs off and retries a
	// Bedrock ThrottlingException), just a floor under how fast this package asks in the first
	// place.
	batchPause = 200 * time.Millisecond
)

// Deps are the durable state and embedding dependencies for the queue.
type Deps struct {
	Store    *store.Store
	Embedder embed.Embedder
}

// Run drains pending embeddings at startup and periodically, so a dropped in-process
// notification cannot strand a row. It returns when ctx is cancelled. Run does nothing when
// deps.Embedder is nil: Dispatch with no Bedrock credentials at boot never starts this poller,
// and every row stays pending until credentials are available and a write or a backfill
// re-enqueues it.
func Run(ctx context.Context, deps Deps) {
	if deps.Embedder == nil {
		return
	}
	ctx = store.WithTransactionTracking(ctx)
	scan(ctx, deps)
	retry := time.NewTicker(retryInterval)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-retry.C:
			scan(ctx, deps)
		}
	}
}

func scan(ctx context.Context, deps Deps) {
	for {
		succeeded, failed, blocked, err := ProcessBatch(ctx, deps)
		if err != nil {
			slog.Error("dispatch embedqueue: scan", "error", err)
			return
		}
		if succeeded+failed < batchSize || blocked {
			return
		}
		if !pause(ctx) {
			return
		}
	}
}

// pause waits batchPause, or returns false without waiting out the rest of it if ctx ends first -
// a cancelled Run should stop promptly, not finish out its pacing delay.
func pause(ctx context.Context) bool {
	timer := time.NewTimer(batchPause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type pendingRow struct {
	kind, id, text, contentHash string
	attempts                    int
}

// ProcessBatch embeds up to one embedder batch of pending rows and reports how many of them it
// actually committed (succeeded) versus sent back for retry (failed) - not the same as how many
// it read, which outbox-style callers used to conflate with "done" - and whether the batch is
// blocked: a row whose retry could not be scheduled stays eligible immediately, and a caller that
// looped on a blocked batch would select the same stuck row forever. Both the poller and Backfill
// drive the queue through this one function; scanPending's claim is what lets the poller and a
// concurrent `backfill-embeddings` run safely at once, each claiming a different set of pending
// rows rather than racing to embed the same ones twice.
func ProcessBatch(ctx context.Context, deps Deps) (succeeded, failed int, blocked bool, err error) {
	rows, err := scanPending(ctx, deps)
	if err != nil {
		return 0, 0, false, err
	}
	if len(rows) == 0 {
		return 0, 0, false, nil
	}
	texts := make([]string, len(rows))
	for i, row := range rows {
		texts[i] = row.text
	}
	vectors, embedErr := deps.Embedder.Embed(ctx, texts, embed.InputDocument)
	if embedErr != nil {
		for _, row := range rows {
			if retryRow(ctx, deps, row, "dispatch embedqueue: embed batch", embedErr) {
				blocked = true
			}
		}
		return 0, len(rows), blocked, nil
	}
	for i, row := range rows {
		if err := commitEmbedding(ctx, deps, row, vectors[i]); err != nil {
			if retryRow(ctx, deps, row, "dispatch embedqueue: commit embedding", err) {
				blocked = true
			}
			failed++
			continue
		}
		succeeded++
	}
	return succeeded, failed, blocked, nil
}

// scanPending claims at most batchSize pending rows, oldest-eligible first, atomically advancing
// their next_attempt_at by claimWindow so a concurrent scanner (the poller and a manually run
// `backfill-embeddings` can run at once) does not also claim them - without holding a lock across
// the embed call that follows; see claimWindow's own comment for why not.
func scanPending(ctx context.Context, deps Deps) ([]pendingRow, error) {
	rows, err := deps.Store.Pool.Query(ctx, `
		update embeddings set next_attempt_at = now() + $2
		where (kind, id) in (
			select kind, id from embeddings
			where embedded_hash is distinct from content_hash
			  and next_attempt_at <= now()
			  and not dead
			order by next_attempt_at, kind, id
			limit $1
			for update skip locked
		)
		returning kind, id, text_snapshot, content_hash, attempt_count
	`, batchSize, claimWindow)
	if err != nil {
		return nil, fmt.Errorf("scan pending embeddings: %w", err)
	}
	defer rows.Close()
	var pending []pendingRow
	for rows.Next() {
		var row pendingRow
		if err := rows.Scan(&row.kind, &row.id, &row.text, &row.contentHash, &row.attempts); err != nil {
			return nil, fmt.Errorf("scan pending embeddings: %w", err)
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan pending embeddings: %w", err)
	}
	return pending, nil
}

// commitEmbedding writes the embedding back only if content_hash still matches what was read at
// scan time: a write that changed the row's text between the scan and this call already reset
// attempt_count and next_attempt_at (embeddings_enqueue), so the guard just means this call
// updates zero rows instead of marking the new text embedded under the old vector - the row stays
// pending and the next scan embeds it at its current text. It refuses a non-finite vector (NaN or
// +/-Inf, which a model should never answer but pgvector's own input parser would otherwise accept
// or reject with a much less specific error) before ever reaching Postgres.
func commitEmbedding(ctx context.Context, deps Deps, row pendingRow, vector []float32) error {
	for _, f := range vector {
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return fmt.Errorf("commit embedding: the model answered a non-finite value (%v)", f)
		}
	}
	_, err := deps.Store.Pool.Exec(ctx, `
		update embeddings
		set embedding = $4::vector, embedded_hash = content_hash, model = $5,
		    embedded_at = now(), attempt_count = 0, next_attempt_at = now()
		where kind = $1 and id = $2 and content_hash = $3
	`, row.kind, row.id, row.contentHash, embed.Literal(vector), embed.Model)
	if err != nil {
		return fmt.Errorf("commit embedding: %w", err)
	}
	return nil
}

// retryRow reports a failed row under what and backs it off, so rows queued behind one that
// cannot be embedded are not stuck behind it. Past deadLetterAttempts it marks the row dead
// instead of scheduling another retry: scanPending's own WHERE excludes dead rows, so a row this
// consistently unembeddable (a vector the model keeps answering as non-finite, an id commitEmbedding
// can never match) stops consuming poller cycles rather than retrying with a five-minute
// ceiling forever. A dead row is still visible (`select * from embeddings where dead`) and still
// recoverable: clearing attempt_count and dead re-admits it to the next scan. It reports whether
// the batch is blocked, exactly as outbox.retryEvent does.
func retryRow(ctx context.Context, deps Deps, row pendingRow, what string, cause error) bool {
	slog.Error(what, "kind", row.kind, "id", row.id, "error", cause)
	attempts := row.attempts + 1
	if attempts >= deadLetterAttempts {
		slog.Error("dispatch embedqueue: row reached the dead-letter threshold, no further automatic retry", "kind", row.kind, "id", row.id, "attempts", attempts)
		if _, err := deps.Store.Pool.Exec(ctx, `
			update embeddings set attempt_count = $3, dead = true where kind = $1 and id = $2 and content_hash = $4
		`, row.kind, row.id, attempts, row.contentHash); err != nil {
			slog.Error("dispatch embedqueue: mark row dead", "kind", row.kind, "id", row.id, "error", err)
			return true
		}
		return false
	}
	_, err := deps.Store.Pool.Exec(ctx, `
		update embeddings set attempt_count = $3, next_attempt_at = $4
		where kind = $1 and id = $2 and content_hash = $5
	`, row.kind, row.id, attempts, time.Now().Add(retryDelay(attempts)), row.contentHash)
	if err != nil {
		slog.Error("dispatch embedqueue: schedule retry", "kind", row.kind, "id", row.id, "error", err)
		return true
	}
	return false
}

func retryDelay(attempts int) time.Duration {
	delay := retryBaseDelay
	for attempt := 1; attempt < attempts && delay < retryMaxDelay; attempt++ {
		delay *= 2
	}
	if delay > retryMaxDelay {
		return retryMaxDelay
	}
	return delay
}

// BackfillReport is what one Backfill call enqueued and embedded, per kind and in total. Embedded
// and Failed count only rows ProcessBatch actually tried to commit, never rows it merely read;
// Pending is whatever is left needing an embedding - still retrying, or dead - once the drain
// loop stops, so a caller (backfill-embeddings) can tell a clean finish from a run that quit with
// work still outstanding.
type BackfillReport struct {
	Enqueued map[string]int64
	Embedded int64
	Failed   int64
	Pending  int64
}

// backfillKinds' SQL selects at most backfillPageSize rows with a primary key greater than the
// kind's checkpoint (embeddings_backfill_progress.last_id, ” before any row has run), ordered by
// that key, in a "page" CTE; a sibling writable CTE inserts each into embeddings exactly as the
// kind's write-time trigger's enqueue would, but ON CONFLICT DO NOTHING rather than DO UPDATE: an
// embeddings row already existing - whether a trigger wrote it after this migration shipped, or
// an earlier Backfill page already did - means Backfill has nothing to add, and must never
// overwrite it. A DO UPDATE here raced a concurrent write: if a trigger's own enqueue (fresher
// text, reset attempt_count) committed between this page's read and this statement's own commit,
// DO UPDATE would have clobbered it with this page's stale snapshot, silently reverting a live
// write back to the text backfill saw moments earlier. DO NOTHING cannot do that - whichever
// inserts first wins, and Backfill is content to have lost that race, since the trigger's version
// is always at least as fresh. Postgres always executes every data-modifying CTE in a WITH list to
// completion, whether or not the main query reads its output (the documented writable-CTE
// behavior), so the insert runs in full even though the final SELECT reads only the plain "page"
// rows - which it must: a conflicting row is absent from a RETURNING list by construction (ON
// CONFLICT DO NOTHING returns nothing for it), but the checkpoint still needs to advance past it.
const (
	backfillIssueSQL = `
		with page as (
			select key as id, title as text from issues where key > $1 order by key limit $2
		), upsert as (
			insert into embeddings (kind, id, text_snapshot, content_hash)
			select 'issue', id, text, encode(sha256(convert_to(text, 'UTF8')), 'hex') from page
			on conflict (kind, id) do nothing
		)
		select id from page order by id`
	backfillDocumentSQL = `
		with page as (
			select a.id::text as id, v.markdown as text
			from artifacts a
			join lateral (
				select markdown from artifact_versions where artifact_id = a.id order by number desc limit 1
			) v on true
			where a.kind = 'doc' and v.markdown is not null and a.id::text > $1
			order by a.id::text limit $2
		), upsert as (
			insert into embeddings (kind, id, text_snapshot, content_hash)
			select 'document', id, text, encode(sha256(convert_to(text, 'UTF8')), 'hex') from page
			on conflict (kind, id) do nothing
		)
		select id from page order by id`
	backfillCommentSQL = `
		with page as (
			select id::text as id, body as text from comments where id::text > $1 order by id::text limit $2
		), upsert as (
			insert into embeddings (kind, id, text_snapshot, content_hash)
			select 'comment', id, text, encode(sha256(convert_to(text, 'UTF8')), 'hex') from page
			on conflict (kind, id) do nothing
		)
		select id from page order by id`
	backfillAskSQL = `
		with page as (
			select id::text as id, question || ' ' || coalesce(answer->>'text', '') as text
			from asks where id::text > $1 order by id::text limit $2
		), upsert as (
			insert into embeddings (kind, id, text_snapshot, content_hash)
			select 'ask', id, text, encode(sha256(convert_to(text, 'UTF8')), 'hex') from page
			on conflict (kind, id) do nothing
		)
		select id from page order by id`
	backfillMessageSQL = `
		with page as (
			select id::text as id, body as text from messages where id::text > $1 order by id::text limit $2
		), upsert as (
			insert into embeddings (kind, id, text_snapshot, content_hash)
			select 'message', id, text, encode(sha256(convert_to(text, 'UTF8')), 'hex') from page
			on conflict (kind, id) do nothing
		)
		select id from page order by id`
)

var backfillKinds = []struct {
	kind string
	sql  string
}{
	{"issue", backfillIssueSQL},
	{"document", backfillDocumentSQL},
	{"comment", backfillCommentSQL},
	{"ask", backfillAskSQL},
	{"message", backfillMessageSQL},
}

// Backfill enqueues every existing row of every searchable kind - content this Dispatch was
// already carrying before meaning search existed, which no write-time trigger ever saw - then
// drains the queue to completion, reporting progress to out as it goes.
//
// It is resumable across restarts: each kind's checkpoint (embeddings_backfill_progress) commits
// after every page, so an interrupted run picks up at the next key rather than rescanning rows it
// already enqueued; enqueueing itself is idempotent (ON CONFLICT DO NOTHING), so running a
// finished kind again does no further work beyond one cheap scan that finds nothing past its
// checkpoint. It runs in small, short transactions (backfillPageSize rows at a time, one
// transaction per page) rather than one long scan, so it does not hold locks a concurrent write
// would wait behind.
func Backfill(ctx context.Context, deps Deps, out io.Writer) (BackfillReport, error) {
	report := BackfillReport{Enqueued: map[string]int64{}}
	for _, source := range backfillKinds {
		count, err := backfillKind(ctx, deps, source.kind, source.sql, out)
		if err != nil {
			return report, fmt.Errorf("backfill %s: %w", source.kind, err)
		}
		report.Enqueued[source.kind] = count
	}
	for {
		succeeded, failed, blocked, err := ProcessBatch(ctx, deps)
		if err != nil {
			return report, fmt.Errorf("backfill: embed queue: %w", err)
		}
		report.Embedded += int64(succeeded)
		report.Failed += int64(failed)
		if succeeded+failed > 0 {
			fmt.Fprintf(out, "backfill-embeddings: embedded %d, failed %d so far\n", report.Embedded, report.Failed)
		}
		if succeeded+failed < batchSize || blocked {
			break
		}
		if !pause(ctx) {
			break
		}
	}
	if err := deps.Store.Pool.QueryRow(ctx, `
		select count(*) from embeddings where embedded_hash is distinct from content_hash
	`).Scan(&report.Pending); err != nil {
		return report, fmt.Errorf("backfill: count pending: %w", err)
	}
	return report, nil
}

func backfillKind(ctx context.Context, deps Deps, kind, sql string, out io.Writer) (int64, error) {
	var checkpoint string
	var done bool
	if err := deps.Store.Pool.QueryRow(ctx, `
		select coalesce(last_id, ''), done from embeddings_backfill_progress where kind = $1
	`, kind).Scan(&checkpoint, &done); err != nil {
		if !isNoRows(err) {
			return 0, fmt.Errorf("read checkpoint: %w", err)
		}
	}
	if done {
		fmt.Fprintf(out, "backfill-embeddings: %s already complete (resume from %q)\n", kind, checkpoint)
		return 0, nil
	}
	var total int64
	for {
		rows, err := deps.Store.Pool.Query(ctx, sql, checkpoint, backfillPageSize)
		if err != nil {
			return total, fmt.Errorf("enqueue page: %w", err)
		}
		var lastID string
		var pageCount int64
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return total, fmt.Errorf("enqueue page: %w", err)
			}
			lastID = id
			pageCount++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return total, fmt.Errorf("enqueue page: %w", err)
		}
		rows.Close()
		total += pageCount
		pageDone := pageCount < backfillPageSize
		if pageCount > 0 {
			checkpoint = lastID
		}
		if _, err := deps.Store.Pool.Exec(ctx, `
			insert into embeddings_backfill_progress (kind, last_id, done, rows_enqueued, updated_at)
			values ($1, $2, $3, $4, now())
			on conflict (kind) do update
				set last_id = excluded.last_id, done = excluded.done,
				    rows_enqueued = embeddings_backfill_progress.rows_enqueued + $4, updated_at = now()
		`, kind, checkpoint, pageDone, pageCount); err != nil {
			return total, fmt.Errorf("write checkpoint: %w", err)
		}
		fmt.Fprintf(out, "backfill-embeddings: %s enqueued %d (checkpoint %q)\n", kind, total, checkpoint)
		if pageDone {
			break
		}
	}
	return total, nil
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
