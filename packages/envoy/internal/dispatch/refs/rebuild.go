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
	// ItemsOwed is the number of comments, messages and asks marked owed a block index of their
	// own blocks (comment_block_state, message_block_state and ask_block_state owed_at).
	ItemsOwed int
}

// RebuildAll reparses every reference source with this build's whole grammar, the bare issue
// keys, `#id`s and short ids it resolves against the database included (Mentions), and
// reconciles refs with it, one transaction per source, so the index converges on what the live
// write paths produce: the latest version of every document, every ask question, every comment
// body, and every issue message (issue-less messages are never indexed), each read by
// sourceText. An edge that survives keeps its created_at and source_seq; one the rebuild
// introduces has no source_seq, since no event introduced it, and no current excerpt: a
// document mention's excerpt is cut from the document's tree, which only the document service
// reads, so its reconciliation
// (docs.Service.RunReferenceExcerptReconciliation) stores it. An edge that survives keeps its
// payload, but StoredExcerpts hides it until the row has a current excerpt_version.
//
// A document's blocks and the mentions they hold are cut from its tree too, so the rebuild marks
// every document owed a block index (document_block_versions.version null), which the block-index
// reconciliation (docs.Service.RunBlockIndexReconciliation) indexes. It marks every comment and
// message, direct messages included, and every ask owed a block index of its own blocks too
// (owed_at, a missing state row inserted owed), so the same reconciliation re-reads each item's
// blocks, an ask's parts included, and the mentions each holds with this build's parser and
// grammar. It leaves block_grammar_owed alone: it has just re-read every comment, ask and issue
// message with this build's grammar itself.
//
// It clears the re-index queue (refs_grammar_owed) for every source it re-indexes: each source's
// own row goes in that source's transaction (replaceRefs), and the `scope` rows, which owe a
// re-index of the sources around a document rather than one source, are deleted at the end for
// every row enqueued before the rebuild started. A row a write enqueues while the rebuild runs
// keeps its place: the rebuild may have read that source already.
func RebuildAll(ctx context.Context, pool Pool, serverURL string) (Rebuild, error) {
	var report Rebuild
	var started time.Time
	if err := pool.QueryRow(ctx, `select now()`).Scan(&started); err != nil {
		return report, fmt.Errorf("read the database's clock: %w", err)
	}
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
	if _, err := pool.Exec(ctx, `delete from refs_grammar_owed where kind = 'scope' and enqueued_at <= $1`, started); err != nil {
		return report, fmt.Errorf("clear the scopes owed an index with the whole grammar: %w", err)
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
	for _, mark := range []struct{ kind, query string }{
		{"comment", `
			insert into comment_block_state (comment_id, owed_at) select id, now() from comments
			on conflict (comment_id) do update set owed_at = excluded.owed_at`},
		{"message", `
			insert into message_block_state (message_id, owed_at) select id, now() from messages
			on conflict (message_id) do update set owed_at = excluded.owed_at`},
		{"ask", `
			insert into ask_block_state (ask_id, owed_at) select id, now() from asks
			on conflict (ask_id) do update set owed_at = excluded.owed_at`},
	} {
		owed, err := pool.Exec(ctx, mark.query)
		if err != nil {
			return report, fmt.Errorf("mark every %s owed a block index: %w", mark.kind, err)
		}
		report.ItemsOwed += int(owed.RowsAffected())
	}
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
	located := NewReader(ctx, tx, fromKind, fromID, serverURL).Mentions(body)
	if _, err := replaceRefs(ctx, tx, fromKind, fromID, text.Refs(located), nil); err != nil {
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

// OwedQueue is one queue of text owed a re-index: a table of (kind, id, enqueued_at) a trigger
// upserts a row into for every source a write left indexed with a grammar older than the one
// this build reads it with, which a drain lists oldest first (ListOwed), settles (SettleOwed)
// and re-indexes (ReindexOwed). Dispatch holds the two below, and the table each names is the
// whole of the difference between them: one drain, one order, for both.
type OwedQueue struct {
	// table is the queue's table, which every statement of the drain names.
	table string
}

// BlockGrammarOwed is the queue of comments, asks and issue messages an older build indexed
// without the block grammar (migration 0121, block_grammar_owed).
var BlockGrammarOwed = OwedQueue{table: "block_grammar_owed"}

// RefsGrammarOwed is the queue of sources an older build indexed without this build's whole
// grammar (migration 20261103205037, refs_grammar_owed). A queue of its own rather than a row
// in the block grammar's: that migration says why.
var RefsGrammarOwed = OwedQueue{table: "refs_grammar_owed"}

// Owed is one source a queue owes an index, as of the write that last owed it (EnqueuedAt).
//
// Kind is the source's own kind - comment, ask or message - or, in the whole grammar's queue,
// `artifact`, a document read at its latest version, or `scope`, whose id is a document whose
// question blocks moved. Those last two name no text this re-indexes: what a `scope` row owes
// is a re-index of every source whose text belongs to that document (ScopeSources) and what an
// `artifact` row owes is a block index of that document, each the drain's own work
// (docs.Service.reindexOwedText). ReindexOwed answers an error for either rather than reading
// a row with no text of its own and settling it, so no caller can drain one by handing it here.
type Owed struct {
	Queue      OwedQueue
	Kind       string
	ID         string
	EnqueuedAt time.Time
}

// reindexable reports whether the row names a text ReindexOwed can read and index: a comment,
// an ask or an issue message, the three kinds sourceText reads as one source's own text.
func (owed Owed) reindexable() bool {
	switch ItemKind(owed.Kind) {
	case ItemComment, ItemAsk, ItemMessage:
		return true
	}
	return false
}

// listQuery is the statement a drain of this queue lists its oldest rows with.
//
// The order columns are written qualified: unqualified, `id` would name the selected `id::text`
// rather than the table's uuid, so every tick would sort the whole tie group the seed enqueues
// under one enqueued_at by a text it casts per row, and on the whole grammar's queue the index
// that carries all three order columns (refs_grammar_owed_enqueued) could not serve the order
// at all. The two orders are the same - a uuid's canonical text is its bytes in lowercase hex,
// hyphens at fixed positions - so the rows come back as they always did
// (TestTheDrainSortsEachQueueByItsTablesID).
func (queue OwedQueue) listQuery() string {
	return `
		select o.kind, o.id::text, o.enqueued_at from ` + queue.table + ` o
		order by o.enqueued_at, o.kind, o.id limit $1
	`
}

// ListOwed lists at most limit sources queue owes an index, oldest first, and locks nothing.
func ListOwed(ctx context.Context, q Queryer, queue OwedQueue, limit int) ([]Owed, error) {
	rows, err := q.Query(ctx, queue.listQuery(), limit)
	if err != nil {
		return nil, fmt.Errorf("list what %s owes an index: %w", queue.table, err)
	}
	defer rows.Close()
	owed := []Owed{}
	for rows.Next() {
		source := Owed{Queue: queue}
		if err := rows.Scan(&source.Kind, &source.ID, &source.EnqueuedAt); err != nil {
			return nil, fmt.Errorf("scan a row of %s: %w", queue.table, err)
		}
		owed = append(owed, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list what %s owes an index: %w", queue.table, err)
	}
	return owed, nil
}

// SettleOwed deletes source's row from its queue while it still holds the enqueued_at source
// was listed with, and reports whether it did: a write that lands meanwhile moves that time,
// and the source stays owed for the next pass, which reads its newer text.
func SettleOwed(ctx context.Context, q Execer, source Owed) (bool, error) {
	settled, err := q.Exec(ctx, `
		delete from `+source.Queue.table+` where kind = $1 and id = $2::uuid and enqueued_at = $3
	`, source.Kind, source.ID, source.EnqueuedAt)
	if err != nil {
		return false, fmt.Errorf("settle the index %s owes for %s %s: %w", source.Queue.table, source.Kind, source.ID, err)
	}
	return settled.RowsAffected() > 0, nil
}

// ReindexOwed reconciles one owed comment's, ask's or issue message's refs with its text, read
// with the grammar this build reads it with, and settles its owed row, in one transaction. It
// takes the source row first (sourceText, forUpdate) and the owed row after, the order a write
// takes them in (the row's update, then its trigger's upsert), so the two never deadlock. The
// owed row is settled only while it holds the enqueued_at source was listed with (SettleOwed),
// and it is settled before the text is written: one a write moved meanwhile is left owed with
// nothing written for it, and the next pass, which reads its newer text, writes it. It reports
// whether it re-indexed the source.
//
// A new edge takes the time the text was written (sourceText) and no source_seq, since no event
// introduced it, as a rebuild's does; a re-index of text no one rewrote appends no event, as
// rebuild-refs appends none. So no ?since= read lists such an edge until a later write of the
// source stamps it with that write's event (Stamp), and then it is listed as that write's. A
// source that is gone has its edges deleted with its owed row, and its blocks and an ask's
// parts are indexed with its text (reindexText).
func ReindexOwed(ctx context.Context, pool Pool, source Owed, serverURL string) (bool, error) {
	if !source.reindexable() {
		return false, fmt.Errorf("re-index %s %s: a %s row of %s names no text of its own",
			source.Kind, source.ID, source.Kind, source.Queue.table)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	body, writtenAt, found, err := sourceText(ctx, tx, source.Kind, source.ID, true)
	if err != nil {
		return false, err
	}
	settled, err := SettleOwed(ctx, tx, source)
	if err != nil || !settled {
		return false, err
	}
	if err := reindexText(ctx, tx, source.Kind, source.ID, body, serverURL, writtenAt, found); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// reindexText reconciles one item's refs, and the blocks its text holds, with body as this build's
// grammar reads it, in tx, which holds the item's row (sourceText, forUpdate). found is what
// sourceText answered: an item whose source is gone has its edges deleted instead. A comment or
// message has its blocks indexed too (IndexItem, which takes the item's block state row after the
// source row), and an ask its parts (IndexAskParts, read from the same row sourceText locked), so
// the mentions its blocks hold are read with the same grammar as its own.
func reindexText(ctx context.Context, tx pgx.Tx, kind, id, body, serverURL string, writtenAt time.Time, found bool) error {
	if !found {
		if _, err := tx.Exec(ctx, `delete from refs where from_kind = $1 and from_id = $2`, kind, id); err != nil {
			return fmt.Errorf("delete the references of %s %s, which is gone: %w", kind, id, err)
		}
		return nil
	}
	resolve := NewReader(ctx, tx, kind, id, serverURL)
	located := resolve.Mentions(body)
	switch itemKind := ItemKind(kind); itemKind {
	case ItemComment, ItemMessage:
		if _, err := IndexItem(ctx, tx, itemKind, id, body, writtenAt, located, nil); err != nil {
			return fmt.Errorf("index the blocks of %s %s: %w", kind, id, err)
		}
	case ItemAsk:
		// An ask's parts are its question, its options and its answer, where sourceText reads
		// the question alone, which is what the ask's own refs are written from.
		ask, _, found, err := askForParts(ctx, tx, id)
		if err != nil {
			return err
		}
		if found {
			if _, err := IndexAskParts(ctx, tx, resolve, ask, writtenAt); err != nil {
				return fmt.Errorf("index the parts of ask %s: %w", id, err)
			}
		}
	}
	if _, err := replaceRefs(ctx, tx, kind, id, text.Refs(located), &writtenAt); err != nil {
		return fmt.Errorf("index %s %s: %w", kind, id, err)
	}
	return nil
}

// ReindexScopeSource re-indexes one comment, ask or issue message whose text belongs to a document
// whose question blocks moved (ScopeSources), so that a `#id` it wrote before its question settled
// is stored as an edge to that ask now. It writes nothing for a text that holds no `#id` candidate
// at all (HoldsQuestionCandidate), which is what most of a document's scope is - for a spec, every
// message, ask and comment of its issue. It reports whether it re-indexed the source, and the
// source's own owed row, if it has one, is settled by that re-index (replaceRefs).
//
// The text is read twice: once with no lock (SourceText), which decides only whether this source
// is worth a transaction, and again inside the transaction under the row's `for no key update`
// (sourceText, forUpdate), which is the text the index is written from. Locking first would take
// the lock an edit of that text takes on every source of the scope before throwing most of them
// away, and a concurrent edit of each such row would wait behind it; the artifact branch of
// docs.Service.reindexScope already reads first and owes the work after.
//
// A text that gains a `#id` between the two reads is indexed by the write that gave it one: a
// write of this build re-indexes the text it wrote in its own transaction (replaceRefs), reading
// the question map as it stands after the settlement this scope row was enqueued by, and a write
// of an older build owes the source its own row (20261103205038-20261103205040), which the drain
// takes in a later tick. One that loses its `#id` between them is left alone by the locked read,
// which is what the write that removed it already indexed.
func ReindexScopeSource(ctx context.Context, pool Pool, kind, id, serverURL string) (bool, error) {
	unlocked, _, found, err := SourceText(ctx, pool, kind, id)
	if err != nil || !found || !HoldsQuestionCandidate(unlocked, serverURL) {
		return false, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	body, writtenAt, found, err := sourceText(ctx, tx, kind, id, true)
	if err != nil || !found {
		return false, err
	}
	if !HoldsQuestionCandidate(body, serverURL) {
		return false, nil
	}
	if err := reindexText(ctx, tx, kind, id, body, serverURL, writtenAt, true); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// SourceText is the text one reference source writes, with the time it was written, taking no
// lock: what a drain reads to decide whether that text has anything a re-index would move. found
// is false for a source that is gone, a document with no markdown version and a direct message.
func SourceText(ctx context.Context, q Queryer, kind, id string) (body string, writtenAt time.Time, found bool, err error) {
	return sourceText(ctx, q, kind, id, false)
}

// HoldsQuestionCandidate reports whether body writes a `#id` the whole grammar would resolve
// against the question map of the document its text belongs to. The grammar itself answers it:
// text.Mentions asks a resolver for that map only when the body holds such a candidate, so a
// resolver that answers nothing and records the question tells which bodies are worth a re-index
// when a document's question blocks move, and no second reading of the grammar can drift from it.
func HoldsQuestionCandidate(body, serverURL string) bool {
	probe := &questionProbe{}
	// A probe that resolves nothing fails nothing: text.Mentions returns an error only from the
	// resolver, and this one answers every call.
	_, _ = text.Mentions(body, serverURL, probe)
	return probe.asked
}

// questionProbe is the text.Resolver HoldsQuestionCandidate reads a body with: it resolves no
// key, no `#id` and no short id, and records that the grammar asked for the question map.
type questionProbe struct {
	asked bool
}

func (p *questionProbe) Issues(keys []string) (map[string]bool, error) { return nil, nil }

func (p *questionProbe) Questions() (map[string]text.Ref, error) {
	p.asked = true
	return nil, nil
}

func (p *questionProbe) Items(prefixes []text.Prefix) (map[int]string, error) { return nil, nil }
