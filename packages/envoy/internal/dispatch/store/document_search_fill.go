package store

import (
	"context"
	"fmt"
	"log/slog"
)

// DocumentSearchFillBatch is how many documents one transaction of FillDocumentSearch takes.
//
// The single statement this fill replaces, `insert … select distinct on (artifact_id) … from
// artifact_versions` (migration 0164 before this change), measured 35.5 s over 40,000 documents
// on a production-shaped database (artifact_versions 3337 MB, 200,000 versions): about 0.9 ms a
// document. A thousand documents is therefore a transaction of about a second and the whole pass
// forty of them, so a writer that meets a key the fill holds - 0163's trigger's upsert on a
// document the fill has reached - waits about a second at worst instead of 35.5 s, while the
// cost a batch pays whatever its size, its commit, its cursor update and one descent of
// artifact_versions_artifact_id_number_key, is paid forty times rather than four hundred.
const DocumentSearchFillBatch = 1000

// fillDocumentSearchBatchQuery is one batch: it takes the next DocumentSearchFillBatch documents
// by keyset from the cursor, writes each one's latest version into document_search, moves the
// cursor to the last document it took, and answers the rows it wrote and whether the pass is
// finished. $1 is the batch size.
//
// The cursor is read `for update`, which is what makes two processes filling at once take
// different batches rather than the same one: the second waits for the first's batch to commit
// and then reads the cursor it moved.
//
// `batch` is the keyset: the next distinct artifact_ids above the cursor, in id order, which is
// the order artifact_versions_artifact_id_number_key holds, so the scan starts where the last
// batch stopped instead of counting past it as an `offset` would. `markdown is not null` is
// 0163's trigger's own guard: a version with no markdown has a null search vector and is no
// document to search.
//
// `latest` is 0164's old write guard, unchanged: the latest version of each document in the
// batch, and `filled` writes them under `on conflict (artifact_id) do update … where
// excluded.number >= document_search.number`, so a row 0163's trigger wrote while the pass runs
// is never replaced by an older one. That guard is also what makes a batch idempotent, so the
// pass is safe to run twice and from two processes at once.
//
// The cursor comes from `batch` and never from `filled`'s `returning`: a row the guard skips
// returns nothing, so a cursor taken from the write would stall on the first document whose row
// the trigger had already written. `done` is set when the batch took no further id, which is the
// pass finished; the cursor then stays where it is.
const fillDocumentSearchBatchQuery = `
	with progress as (
		select artifact_id from document_search_backfill for update
	), batch as (
		select distinct v.artifact_id
		  from artifact_versions v
		 where v.markdown is not null
		   and v.artifact_id > (select artifact_id from progress)
		 order by v.artifact_id
		 limit $1
	), latest as (
		select distinct on (v.artifact_id) v.artifact_id, v.number, v.created_at, v.search
		  from artifact_versions v
		 where v.markdown is not null
		   and v.artifact_id in (select artifact_id from batch)
		 order by v.artifact_id, v.number desc
	), filled as (
		insert into document_search (artifact_id, number, created_at, search)
		select artifact_id, number, created_at, search from latest
		on conflict (artifact_id) do update
		   set number = excluded.number, created_at = excluded.created_at, search = excluded.search
		 where excluded.number >= document_search.number
		returning 1
	), moved as (
		update document_search_backfill
		   set artifact_id = coalesce(
		           (select b.artifact_id from batch b order by b.artifact_id desc limit 1),
		           document_search_backfill.artifact_id),
		       done = not exists (select 1 from batch)
		returning done
	)
	select (select count(*) from filled), (select done from moved)
`

// FillDocumentSearch fills 0162's document_search for the documents stored before 0163's trigger
// went live, and answers how many rows it wrote. The boot runs it after the migrations and
// before the server serves (cmd/dispatch/main.go): search's document arm reads document_search
// alone, so a process serving with the table unfilled answers a search with documents missing.
//
// A pass already finished costs one row: the progress row 0164 creates says so, and this reads
// nothing else. Otherwise it loops, each iteration one short transaction that takes the next
// batch of documents by keyset, writes them and commits, so no row it writes stays uncommitted
// for longer than its own batch and a version write that meets one of its keys waits for that
// batch alone. 0164's comment holds the measurement that says why.
func (s *Store) FillDocumentSearch(ctx context.Context) (int64, error) {
	var done bool
	if err := s.Pool.QueryRow(ctx, "select done from document_search_backfill").Scan(&done); err != nil {
		return 0, fmt.Errorf("read the document_search fill's progress: %w", err)
	}
	if done {
		return 0, nil
	}
	var written, batches int64
	for {
		rows, finished, err := s.fillDocumentSearchBatch(ctx)
		if err != nil {
			return written, err
		}
		written += rows
		batches++
		slog.Info("dispatch: document_search fill committed a batch", "rows", rows, "written", written, "batches", batches)
		if finished {
			slog.Info("dispatch: document_search fill finished", "rows", written, "batches", batches)
			return written, nil
		}
	}
}

// fillDocumentSearchBatch runs one batch in a transaction of its own and commits it, answering
// the rows it wrote and whether the batch took no further document, which is the pass finished.
func (s *Store) fillDocumentSearchBatch(ctx context.Context) (int64, bool, error) {
	// A batch is a transaction like any other, so it is marked like any other: nothing it runs
	// may take a second pooled connection while it is open.
	ctx = WithTransactionTracking(ctx)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("begin a document_search fill batch: %w", err)
	}
	defer tx.Rollback(ctx)
	var written int64
	var finished bool
	if err := tx.QueryRow(ctx, fillDocumentSearchBatchQuery, DocumentSearchFillBatch).Scan(&written, &finished); err != nil {
		return 0, false, fmt.Errorf("fill a batch of document_search: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("commit a document_search fill batch: %w", err)
	}
	return written, finished, nil
}
