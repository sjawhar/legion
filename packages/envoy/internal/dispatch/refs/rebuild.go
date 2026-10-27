package refs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Pool is the part of the Dispatch connection pool a rebuild uses.
type Pool interface {
	Queryer
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Rebuild counts what RebuildAll reconciled.
type Rebuild struct {
	Documents int
	Asks      int
	Comments  int
	Messages  int
	// Orphans is the number of edges deleted because no live source writes them.
	Orphans int
	// Edges is the number of mention edges after the rebuild.
	Edges int
	// BlocksOwed is the number of documents marked owed a block index (document_block_versions).
	BlocksOwed int
}

// RebuildAll reparses every reference source and reconciles refs with it, one transaction per
// source, so the index converges on what the live write paths produce: the latest version of
// every document, every ask question, every comment body, and every issue message (issue-less
// messages are never indexed), each read by sourceText. An edge that survives keeps its
// created_at and source_seq; one the rebuild introduces has no source_seq, since no event
// introduced it, and no current excerpt: a document mention's excerpt is cut from the document's
// tree, which only the document service reads, so its reconciliation
// (docs.Service.RunReferenceExcerptReconciliation) stores it. An edge that survives keeps its
// payload, but StoredExcerpts hides it until the row has a current excerpt_version.
//
// A document's blocks and the mentions they hold are cut from its tree too, so the rebuild marks
// every document owed a block index (document_block_versions.version null), which the block-index
// reconciliation (docs.Service.RunBlockIndexReconciliation) indexes. It leaves block_grammar_owed
// alone: it has just re-read every comment, ask and issue message with this build's grammar itself.
func RebuildAll(ctx context.Context, pool Pool, serverURL string) (Rebuild, error) {
	var report Rebuild
	sources := []struct {
		kind  string
		query string
		count *int
	}{
		{"artifact", `
			select a.id::text from artifacts a
			where exists (select 1 from artifact_versions v where v.artifact_id = a.id and v.markdown is not null)
			order by a.id`, &report.Documents},
		{"ask", `select id::text from asks order by id`, &report.Asks},
		{"comment", `select id::text from comments order by id`, &report.Comments},
		{"message", `select id::text from messages where issue_key is not null order by id`, &report.Messages},
	}
	for _, source := range sources {
		ids, err := loadSourceIDs(ctx, pool, source.query)
		if err != nil {
			return report, fmt.Errorf("load %s sources: %w", source.kind, err)
		}
		for _, id := range ids {
			rebuilt, err := replaceInTx(ctx, pool, source.kind, id, serverURL)
			if err != nil {
				return report, fmt.Errorf("rebuild %s %s: %w", source.kind, id, err)
			}
			if rebuilt {
				*source.count++
			}
		}
	}
	orphans, err := pool.Exec(ctx, `
		delete from refs r
		where not exists (
			select 1 from artifacts a where r.from_kind = 'artifact' and a.id::text = r.from_id
			union all
			select 1 from asks k where r.from_kind = 'ask' and k.id::text = r.from_id
			union all
			select 1 from comments c where r.from_kind = 'comment' and c.id::text = r.from_id
			union all
			select 1 from messages m where r.from_kind = 'message' and m.issue_key is not null and m.id::text = r.from_id
		)
	`)
	if err != nil {
		return report, fmt.Errorf("delete orphan references: %w", err)
	}
	report.Orphans = int(orphans.RowsAffected())
	owed, err := pool.Exec(ctx, `
		insert into document_block_versions (artifact_id) select id from artifacts where kind = 'doc'
		on conflict (artifact_id) do update set version = null
	`)
	if err != nil {
		return report, fmt.Errorf("mark every document owed a block index: %w", err)
	}
	report.BlocksOwed = int(owed.RowsAffected())
	if err := pool.QueryRow(ctx, `select count(*) from refs`).Scan(&report.Edges); err != nil {
		return report, fmt.Errorf("count references: %w", err)
	}
	return report, nil
}

func loadSourceIDs(ctx context.Context, pool Pool, query string) ([]string, error) {
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// replaceInTx reconciles one source's refs with its text in a transaction of its own, and
// reports false for a source gone since it was listed, whose edges the orphan sweep deletes.
func replaceInTx(ctx context.Context, pool Pool, fromKind, fromID, serverURL string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	body, _, found, err := sourceText(ctx, tx, fromKind, fromID, false)
	if err != nil || !found {
		return false, err
	}
	if _, err := replaceRefs(ctx, tx, fromKind, fromID, text.Extract(body, serverURL), nil); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// sourceText reads the text one reference source writes, with the time that text was written: a
// document's latest version's markdown (fromID its uuid) and that version's created_at, an ask's
// question, a comment's body, or an issue message's body, with the row's edited_at where its table
// has one (asks, comments) and its created_at otherwise. found is false when no such source
// exists: a deleted row, a document with no markdown version, a direct message, which nothing
// indexes. forUpdate takes the row read `for no key update`, the lock an edit of that text takes,
// so the caller's index of the text and a concurrent edit of it are ordered.
func sourceText(ctx context.Context, q Queryer, kind, id string, forUpdate bool) (body string, writtenAt time.Time, found bool, err error) {
	var query string
	switch kind {
	case "artifact":
		query = `select markdown, created_at from artifact_versions
			where artifact_id = $1::uuid and markdown is not null order by number desc limit 1`
	case "ask":
		query = `select question, coalesce(edited_at, created_at) from asks where id = $1::uuid`
	case "comment":
		query = `select body, coalesce(edited_at, created_at) from comments where id = $1::uuid`
	case "message":
		query = `select body, created_at from messages where id = $1::uuid and issue_key is not null`
	default:
		return "", time.Time{}, false, fmt.Errorf("read the text of %s %s: not a reference source", kind, id)
	}
	if forUpdate {
		query += " for no key update"
	}
	switch err := q.QueryRow(ctx, query, id).Scan(&body, &writtenAt); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", time.Time{}, false, nil
	case err != nil:
		return "", time.Time{}, false, fmt.Errorf("read the text of %s %s: %w", kind, id, err)
	}
	return body, writtenAt, true, nil
}

// OwedItem is one comment, ask or issue message owed an index with the block grammar
// (block_grammar_owed), as of the write that last owed it (EnqueuedAt).
type OwedItem struct {
	Kind       string
	ID         string
	EnqueuedAt time.Time
}

// OwedItems lists at most limit items owed an index with the block grammar, oldest first, and
// locks nothing.
func OwedItems(ctx context.Context, q Queryer, limit int) ([]OwedItem, error) {
	rows, err := q.Query(ctx, `
		select kind, id::text, enqueued_at from block_grammar_owed order by enqueued_at, kind, id limit $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list the items owed a block index: %w", err)
	}
	defer rows.Close()
	owed := []OwedItem{}
	for rows.Next() {
		var item OwedItem
		if err := rows.Scan(&item.Kind, &item.ID, &item.EnqueuedAt); err != nil {
			return nil, fmt.Errorf("scan an item owed a block index: %w", err)
		}
		owed = append(owed, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the items owed a block index: %w", err)
	}
	return owed, nil
}

// ReindexOwedItem reconciles one owed item's refs with its text, read with this build's grammar,
// and settles its owed row, in one transaction. It takes the source row first (sourceText,
// forUpdate) and the owed row after, the order an edit takes them in (the row's update, then its
// trigger's upsert), so the two never deadlock. The owed row is deleted only while it holds the
// enqueued_at item was listed with: an edit that lands meanwhile moves it, and the item stays
// owed for the next pass. A new edge takes the time the text was written (sourceText) and no
// source_seq, since no event introduced it, as a rebuild's does; a re-index of text no one
// rewrote appends no event, as rebuild-refs appends none. An item whose source is gone has its
// edges deleted with its owed row.
func ReindexOwedItem(ctx context.Context, pool Pool, item OwedItem, serverURL string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	body, writtenAt, found, err := sourceText(ctx, tx, item.Kind, item.ID, true)
	if err != nil {
		return err
	}
	if found {
		if _, err := replaceRefs(ctx, tx, item.Kind, item.ID, text.Extract(body, serverURL), &writtenAt); err != nil {
			return fmt.Errorf("index %s %s: %w", item.Kind, item.ID, err)
		}
	} else if _, err := tx.Exec(ctx, `delete from refs where from_kind = $1 and from_id = $2`, item.Kind, item.ID); err != nil {
		return fmt.Errorf("delete the references of %s %s, which is gone: %w", item.Kind, item.ID, err)
	}
	if _, err := tx.Exec(ctx, `
		delete from block_grammar_owed where kind = $1 and id = $2::uuid and enqueued_at = $3
	`, item.Kind, item.ID, item.EnqueuedAt); err != nil {
		return fmt.Errorf("settle the block index owed by %s %s: %w", item.Kind, item.ID, err)
	}
	return tx.Commit(ctx)
}
