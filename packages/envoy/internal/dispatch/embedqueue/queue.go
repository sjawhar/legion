// Package embedqueue durably retries Cohere embedding calls for Dispatch's meaning search. A
// write's AFTER INSERT/UPDATE trigger (0054_embeddings.up.sql) upserts a pending row into
// embeddings with the latest text and a hash of it in the same transaction as the write, and this
// package's poller embeds whatever that enqueue left pending, with the same scan-then-retry
// shape as the event outbox (internal/dispatch/outbox): a write never fails because Cohere did,
// since Cohere is never called inside the enqueueing transaction.
package embedqueue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
)

// Deps are the durable state and embedding dependencies for the queue.
type Deps struct {
	Store    *store.Store
	Embedder embed.Embedder
}

// Run drains pending embeddings at startup and periodically, so a dropped in-process
// notification cannot strand a row. It returns when ctx is cancelled. Run does nothing when
// deps.Embedder is nil: Dispatch configured without a Cohere key never starts this poller, and
// every row stays pending until one is configured and a write or a backfill re-enqueues it.
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
		count, blocked, err := ProcessBatch(ctx, deps)
		if err != nil {
			slog.Error("dispatch embedqueue: scan", "error", err)
			return
		}
		if count < batchSize || blocked {
			return
		}
	}
}

type pendingRow struct {
	kind, id, text, contentHash string
	attempts                    int
}

// ProcessBatch embeds up to one Cohere batch of pending rows and reports how many it read, and
// whether the batch is blocked: a row whose retry could not be scheduled stays eligible
// immediately, and a caller that looped on a blocked batch would select the same stuck row
// forever. Both the poller and Backfill drive the queue through this one function.
func ProcessBatch(ctx context.Context, deps Deps) (int, bool, error) {
	rows, err := scanPending(ctx, deps)
	if err != nil {
		return 0, false, err
	}
	if len(rows) == 0 {
		return 0, false, nil
	}
	texts := make([]string, len(rows))
	for i, row := range rows {
		texts[i] = row.text
	}
	vectors, err := deps.Embedder.Embed(ctx, texts, embed.InputDocument)
	if err != nil {
		blocked := false
		for _, row := range rows {
			if retryRow(ctx, deps, row, "dispatch embedqueue: embed batch", err) {
				blocked = true
			}
		}
		return len(rows), blocked, nil
	}
	blocked := false
	for i, row := range rows {
		if err := commitEmbedding(ctx, deps, row, vectors[i]); err != nil {
			if retryRow(ctx, deps, row, "dispatch embedqueue: commit embedding", err) {
				blocked = true
			}
		}
	}
	return len(rows), blocked, nil
}

func scanPending(ctx context.Context, deps Deps) ([]pendingRow, error) {
	rows, err := deps.Store.Pool.Query(ctx, `
		select kind, id, text_snapshot, content_hash, attempt_count
		from embeddings
		where embedded_hash is distinct from content_hash
		  and next_attempt_at <= now()
		order by next_attempt_at, kind, id
		limit $1
	`, batchSize)
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
// pending and the next scan embeds it at its current text.
func commitEmbedding(ctx context.Context, deps Deps, row pendingRow, vector []float32) error {
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
// cannot be embedded are not stuck behind it. It reports whether the batch is blocked, exactly as
// outbox.retryEvent does.
func retryRow(ctx context.Context, deps Deps, row pendingRow, what string, cause error) bool {
	slog.Error(what, "kind", row.kind, "id", row.id, "error", cause)
	attempts := row.attempts + 1
	if attempts == deadLetterAttempts {
		slog.Error("dispatch embedqueue: row reached dead-letter threshold", "kind", row.kind, "id", row.id, "attempts", attempts)
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

// BackfillReport is what one Backfill call enqueued and embedded, per kind and in total.
type BackfillReport struct {
	Enqueued map[string]int64
	Embedded int64
	Failed   int64
}

// backfillKinds' SQL selects at most backfillPageSize rows with a primary key greater than the
// kind's checkpoint (embeddings_backfill_progress.last_id, ” before any row has run), ordered by
// that key, in a "page" CTE; a sibling writable CTE upserts each into embeddings exactly as the
// kind's write-time trigger does (embeddings_enqueue's own logic, inlined so the whole page is one
// statement rather than one round trip per row). Postgres always executes every data-modifying CTE
// in a WITH list to completion, whether or not the main query reads its output (the documented
// writable-CTE behavior), so the upsert runs in full even though the final SELECT reads only the
// plain "page" rows - which it must: a conflicting row whose text has not changed satisfies the
// upsert's WHERE-guard as a no-op and would be absent from a RETURNING list, but the checkpoint
// still needs to advance past it.
const (
	backfillIssueSQL = `
		with page as (
			select key as id, title as text from issues where key > $1 order by key limit $2
		), upsert as (
			insert into embeddings (kind, id, text_snapshot, content_hash)
			select 'issue', id, text, encode(sha256(convert_to(text, 'UTF8')), 'hex') from page
			on conflict (kind, id) do update
				set text_snapshot = excluded.text_snapshot, content_hash = excluded.content_hash,
				    attempt_count = 0, next_attempt_at = now()
				where embeddings.content_hash is distinct from excluded.content_hash
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
			on conflict (kind, id) do update
				set text_snapshot = excluded.text_snapshot, content_hash = excluded.content_hash,
				    attempt_count = 0, next_attempt_at = now()
				where embeddings.content_hash is distinct from excluded.content_hash
		)
		select id from page order by id`
	backfillCommentSQL = `
		with page as (
			select id::text as id, body as text from comments where id::text > $1 order by id::text limit $2
		), upsert as (
			insert into embeddings (kind, id, text_snapshot, content_hash)
			select 'comment', id, text, encode(sha256(convert_to(text, 'UTF8')), 'hex') from page
			on conflict (kind, id) do update
				set text_snapshot = excluded.text_snapshot, content_hash = excluded.content_hash,
				    attempt_count = 0, next_attempt_at = now()
				where embeddings.content_hash is distinct from excluded.content_hash
		)
		select id from page order by id`
	backfillAskSQL = `
		with page as (
			select id::text as id, question || ' ' || coalesce(answer->>'text', '') as text
			from asks where id::text > $1 order by id::text limit $2
		), upsert as (
			insert into embeddings (kind, id, text_snapshot, content_hash)
			select 'ask', id, text, encode(sha256(convert_to(text, 'UTF8')), 'hex') from page
			on conflict (kind, id) do update
				set text_snapshot = excluded.text_snapshot, content_hash = excluded.content_hash,
				    attempt_count = 0, next_attempt_at = now()
				where embeddings.content_hash is distinct from excluded.content_hash
		)
		select id from page order by id`
	backfillMessageSQL = `
		with page as (
			select id::text as id, body as text from messages where id::text > $1 order by id::text limit $2
		), upsert as (
			insert into embeddings (kind, id, text_snapshot, content_hash)
			select 'message', id, text, encode(sha256(convert_to(text, 'UTF8')), 'hex') from page
			on conflict (kind, id) do update
				set text_snapshot = excluded.text_snapshot, content_hash = excluded.content_hash,
				    attempt_count = 0, next_attempt_at = now()
				where embeddings.content_hash is distinct from excluded.content_hash
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
// already enqueued; enqueueing itself is also idempotent (embeddings_enqueue is a no-op update
// when the text has not changed), so running a finished kind again does no further work beyond
// one cheap scan that finds nothing past its checkpoint. It runs in small, short transactions
// (backfillPageSize rows at a time, one transaction per page) rather than one long scan, so it
// does not hold locks a concurrent write would wait behind.
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
		count, blocked, err := ProcessBatch(ctx, deps)
		if err != nil {
			return report, fmt.Errorf("backfill: embed queue: %w", err)
		}
		report.Embedded += int64(count)
		if count > 0 {
			fmt.Fprintf(out, "backfill-embeddings: embedded %d so far\n", report.Embedded)
		}
		if count < batchSize || blocked {
			if blocked {
				report.Failed++
			}
			break
		}
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
