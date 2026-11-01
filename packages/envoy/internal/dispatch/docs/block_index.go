package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// blockIndexDocumentBatch is how many owed documents one block-index tick reads,
// blockIndexItemBatch how many items owed an index with the block grammar and, separately, how
// many comments, messages and asks owed a block index of their own blocks; blockIndexStep is the
// wait before the next tick while any batch was full, BlockIndexReconcileInterval the wait once
// none is. The owed reads go through partial indexes (document_block_versions_owed,
// block_grammar_owed_enqueued, comment_block_state_owed, message_block_state_owed,
// ask_block_state_owed), so a tick that finds nothing owed reads nothing else.
const (
	blockIndexDocumentBatch     = 20
	blockIndexItemBatch         = 200
	blockIndexStep              = 2 * time.Second
	BlockIndexReconcileInterval = 30 * time.Second
)

// blockIndexLease names the lease one block-index tick runs under. The index it writes is the
// same whichever process writes it, so two processes indexing the same batch is work done twice
// for one result.
const blockIndexLease = "block-index"

// BlockIndexTick is what one tick of the block-index reconciliation did (ReconcileBlockIndex).
type BlockIndexTick struct {
	// Documents is how many owed documents it indexed, Items how many owed comments, asks and
	// issue messages it indexed with the block grammar, and ItemBlocks how many owed comments and
	// messages (direct messages included) it indexed the blocks of.
	Documents  int
	Items      int
	ItemBlocks int
	// LiveAhead is how many owed documents, none of a closed issue, it skipped because their live
	// text is ahead of their latest version and differs from it: owed a settlement that has not run.
	LiveAhead int
	// OwedDocuments, OwedItems and OwedItemBlocks are what is still owed after the tick.
	OwedDocuments  int
	OwedItems      int
	OwedItemBlocks int
	// Cursor is the document the next tick's batch starts after: the last one this batch listed
	// while the batch was full, empty once a batch lists fewer and the next starts over.
	Cursor string
	// Full reports a batch of documents, of items or of item blocks that was full, so more may be
	// owed.
	Full bool
}

// RunBlockIndexReconciliation indexes what no block-aware version write indexed: every document
// owed a block index (document_block_versions.version null) - each one at migration 0121, each
// version any build writes (migration 0122's trigger) that its own writer did not index, and each
// rebuild-refs marks - every comment, ask and issue message whose text holds `#b-`, owed an
// index with the block grammar (block_grammar_owed, migrations 0123-0126), which a build older
// than the block grammar indexes without, and every comment, message and ask owed a block index
// of its own blocks (comment_block_state, message_block_state and ask_block_state owed_at, set by
// migrations 0143-0144's and 20261101143212's triggers on every write and by rebuild-refs), which
// a build older than item blocks leaves owed.
// One tick per lease, at start, then every blockIndexStep while a batch is full and every
// BlockIndexReconcileInterval once none is, until ctx ends.
//
// The ticks are duty.RunLeasedAfterEachPass's, each one the wait the pass before it asked for after
// that pass returned, and so is the period each runs under: that wait, blockIndexStep while a batch
// is full and the full interval once none is, so a tick inside it is one some process already
// made. A tick whose lease another process holds, or whose period that process has already served,
// waits the full interval: that process is doing this work.
func (s *Service) RunBlockIndexReconciliation(ctx context.Context, leases duty.Leases) {
	ctx = store.WithTransactionTracking(ctx)
	var cursor string
	// logged is the last tick this loop logged: a tick that indexed nothing and leaves the same
	// counts (an open issue's document whose live text stays ahead, skipped every tick) logs nothing.
	var logged BlockIndexTick
	liveAhead := liveAheadDocuments{}
	duty.RunLeasedAfterEachPass(ctx, leases, blockIndexLease, BlockIndexReconcileInterval, func(ctx context.Context) time.Duration {
		var wait time.Duration
		wait, cursor = s.blockIndexTick(ctx, cursor, liveAhead, &logged)
		return wait
	})
}

// blockIndexTick runs one tick from cursor, logs what it did unless it indexed nothing and left the
// counts *logged holds, and returns how long to wait before the next tick and the cursor it starts
// from.
func (s *Service) blockIndexTick(ctx context.Context, cursor string, liveAhead liveAheadDocuments, logged *BlockIndexTick) (time.Duration, string) {
	tick, err := s.ReconcileBlockIndex(ctx, cursor, liveAhead)
	if ctx.Err() != nil {
		return BlockIndexReconcileInterval, cursor
	}
	if err != nil {
		slog.Error("dispatch: block index reconciliation", "error", err)
	}
	if tick.Documents > 0 || tick.Items > 0 || tick.ItemBlocks > 0 || tick.LiveAhead != logged.LiveAhead ||
		tick.OwedDocuments != logged.OwedDocuments || tick.OwedItems != logged.OwedItems ||
		tick.OwedItemBlocks != logged.OwedItemBlocks {
		slog.Info("dispatch: block index", "documents", tick.Documents, "items", tick.Items,
			"item_blocks", tick.ItemBlocks, "owed_documents", tick.OwedDocuments, "owed_items", tick.OwedItems,
			"owed_item_blocks", tick.OwedItemBlocks, "live_ahead", tick.LiveAhead)
		*logged = tick
	}
	if tick.Full {
		return blockIndexStep, tick.Cursor
	}
	return BlockIndexReconcileInterval, tick.Cursor
}

// owedDocument is a document owed a block index as it was listed: its latest version, the
// marked_at of its pending settlement, zero when it owes none, and whether its issue is open (a
// project's document is).
type owedDocument struct {
	id       string
	version  int
	markedAt time.Time
	open     bool
}

// liveAheadDocuments is, by id, each owed document a tick found live ahead (indexOwedDocument), as
// it was listed then. Every update appended to a document's store moves its pending settlement's
// marked_at (markSettlementPending), and every version write moves its version, so a document
// listed again with both unchanged, and its issue still open, still holds the stored live text a
// tick found ahead: it is counted live ahead without being read or locked. Only a listing that
// reads the issue open uses it: the listing reads the issue with no lock, while live ahead is
// decided under the owner row's lock, so an entry can carry a listed close that the lock saw
// undone, and a close can leave the version, the mark and the room as they were. A closed issue's
// document is indexed rather than skipped, so one listed with its issue closed is read again.
// Only the store's text is
// remembered: a room this server holds can be ahead of the store (an edit not yet appended) or
// behind it (another process's append it has not yet been evicted for), so a document is neither
// remembered nor taken from here while one is resident.
type liveAheadDocuments map[string]owedDocument

// ReconcileBlockIndex runs one tick: one batch of the documents owed a block index past after (a
// document id; empty is the start), one batch of the items owed an index with the block grammar,
// oldest first (refs.ReindexOwedItem), and one batch of the comments, messages and asks owed a
// block index of their own blocks, oldest owed_at first (refs.IndexOwedItemBlocks), each in its
// own transaction taking the item row then its state row, never an owner row. The item blocks run
// last, so an item the grammar batch indexed, blocks included, is no longer owed. The documents
// are listed by keyset, so a document it skips never holds back the ones past it: the tick's
// Cursor moves past every document its batch lists, indexed or not, and goes back to the start
// once a batch lists fewer than a full one. A document or item it cannot index for a reason of
// its own is logged and left owed, so one never holds back the rest of its batch. liveAhead is
// the documents the calling loop's earlier ticks found live ahead, which this tick counts again
// without reading and keeps up to date.
func (s *Service) ReconcileBlockIndex(ctx context.Context, after string, liveAhead liveAheadDocuments) (BlockIndexTick, error) {
	tick := BlockIndexTick{Cursor: after}
	owed, err := s.owedDocuments(ctx, after)
	if err != nil {
		return tick, err
	}
	if s.afterBlockIndexListed != nil {
		s.afterBlockIndexListed()
	}
	documentsFull := len(owed) == blockIndexDocumentBatch
	liveAhead.forgetUnlisted(after, owed, documentsFull)
	for _, document := range owed {
		if s.stillLiveAhead(liveAhead, document) {
			tick.LiveAhead++
			tick.Cursor = document.id
			continue
		}
		delete(liveAhead, document.id)
		cold := s.srv.GetDoc(document.id) == nil
		indexed, ahead, err := s.indexOwedDocument(ctx, document)
		switch {
		case ctx.Err() != nil:
			return tick, ctx.Err()
		case err != nil:
			slog.Error("dispatch: block index skipped a document it could not index",
				"artifact", document.id, "error", err)
		case ahead:
			tick.LiveAhead++
			// Every entry is a document its listing read open: the lock can have seen a reopen the
			// listing missed, and a listing that reads the issue closed must read the document again.
			if cold && document.open && !document.markedAt.IsZero() && s.srv.GetDoc(document.id) == nil {
				liveAhead[document.id] = document
			}
		case indexed:
			tick.Documents++
		}
		tick.Cursor = document.id
	}
	if !documentsFull {
		tick.Cursor = ""
	}

	items, err := refs.OwedItems(ctx, s.store.Pool, blockIndexItemBatch)
	if err != nil {
		return tick, err
	}
	for _, item := range items {
		if err := refs.ReindexOwedItem(ctx, s.store.Pool, item, s.serverURL); err != nil {
			if ctx.Err() != nil {
				return tick, ctx.Err()
			}
			slog.Error("dispatch: block index skipped an item it could not index",
				"kind", item.Kind, "id", item.ID, "error", err)
			continue
		}
		tick.Items++
	}

	itemBlocks, err := refs.OwedItemBlocks(ctx, s.store.Pool, blockIndexItemBatch)
	if err != nil {
		return tick, err
	}
	for _, item := range itemBlocks {
		indexed, err := refs.IndexOwedItemBlocks(ctx, s.store.Pool, item, s.serverURL)
		if err != nil {
			if ctx.Err() != nil {
				return tick, ctx.Err()
			}
			slog.Error("dispatch: block index skipped the blocks of an item it could not index",
				"kind", item.Kind, "id", item.ID, "error", err)
			continue
		}
		if indexed {
			tick.ItemBlocks++
		}
	}
	tick.Full = documentsFull || len(items) == blockIndexItemBatch || len(itemBlocks) == blockIndexItemBatch

	if err := s.store.Pool.QueryRow(ctx, `
		select (select count(*) from document_block_versions where version is null),
		       (select count(*) from block_grammar_owed),
		       (select count(*) from comment_block_state where owed_at is not null) +
		       (select count(*) from message_block_state where owed_at is not null) +
		       (select count(*) from ask_block_state where owed_at is not null)
	`).Scan(&tick.OwedDocuments, &tick.OwedItems, &tick.OwedItemBlocks); err != nil {
		return tick, fmt.Errorf("count what is owed a block index: %w", err)
	}
	return tick, nil
}

// stillLiveAhead reports whether document, as listed now with its issue open, is one liveAhead
// holds at the same version and settlement mark, with no room of this server resident for it.
// liveAhead holds only documents listed open (ReconcileBlockIndex); a listing that reads the issue
// closed always reads the document again.
func (s *Service) stillLiveAhead(liveAhead liveAheadDocuments, document owedDocument) bool {
	found, ok := liveAhead[document.id]
	return ok && document.open && found.version == document.version &&
		found.markedAt.Equal(document.markedAt) && s.srv.GetDoc(document.id) == nil
}

// forgetUnlisted drops each document past after, up to the last of owed while the batch is full,
// that owed does not list: it is owed no block index any more.
func (liveAhead liveAheadDocuments) forgetUnlisted(after string, owed []owedDocument, full bool) {
	listed := make(map[string]struct{}, len(owed))
	for _, document := range owed {
		listed[document.id] = struct{}{}
	}
	for id := range liveAhead {
		_, found := listed[id]
		if !found && id > after && (!full || id <= owed[len(owed)-1].id) {
			delete(liveAhead, id)
		}
	}
}

// owedDocuments lists, in id order, at most blockIndexDocumentBatch documents owed a block index
// past after, each with its latest markdown version, its pending settlement's marked_at and
// whether its issue is open. A document with no markdown version has nothing to index and is
// listed with version 0, which indexOwedDocument passes over.
func (s *Service) owedDocuments(ctx context.Context, after string) ([]owedDocument, error) {
	var cursor any
	if after != "" {
		cursor = after
	}
	rows, err := s.store.Pool.Query(ctx, `
		select b.artifact_id::text,
		       coalesce((select max(v.number) from artifact_versions v
		                 where v.artifact_id = b.artifact_id and v.markdown is not null), 0),
		       (select p.marked_at from doc_settlements_pending p where p.artifact_id = b.artifact_id),
		       i.closed_at is null
		from document_block_versions b
		left join artifacts a on a.id = b.artifact_id
		left join issues i on i.key = a.issue_key
		where b.version is null and ($1::uuid is null or b.artifact_id > $1::uuid)
		order by b.artifact_id
		limit $2
	`, cursor, blockIndexDocumentBatch)
	if err != nil {
		return nil, fmt.Errorf("list the documents owed a block index: %w", err)
	}
	defer rows.Close()
	owed := []owedDocument{}
	for rows.Next() {
		var document owedDocument
		var markedAt *time.Time
		if err := rows.Scan(&document.id, &document.version, &markedAt, &document.open); err != nil {
			return nil, fmt.Errorf("scan a document owed a block index: %w", err)
		}
		if markedAt != nil {
			document.markedAt = *markedAt
		}
		owed = append(owed, document)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the documents owed a block index: %w", err)
	}
	return owed, nil
}

// indexOwedDocument indexes one owed document at its latest version with the full version index
// (indexRenderedVersion): references, excerpts, blocks, block texts and the indexed version. It
// reads the document the server serves (TextWithBlocks) with no transaction held, then, under the
// document's owner row, which every version write holds first, re-checks that the latest version
// is still the one listed and the document still owed; otherwise another writer has indexed it or
// will, and it does nothing.
//
// The live text is indexed only where it is the latest version's text: when the document owes no
// settlement, or owes one but its live markdown equals the version's stored markdown byte for
// byte. A document of an open issue or of a project whose live text is ahead of its latest
// version, and differs from it, is skipped and reported liveAhead, and stays owed: indexing it at
// that version would date a later edit's blocks to it. Its settlement indexes it when it writes
// the next version.
//
// A closed issue's document is the exception: no settlement runs while its issue is closed
// (RunSettlementResumption resumes open issues only), so it is indexed at its latest version from
// that version's own text, with each block the version holds under the id of the index's row
// that holds its text, every such row claiming its block before any live text is compared, or
// else of the live block matched to it by text (closedVersionBlocks), read under the owner row.
// The issue's close, its final sweep and the repair write ask states into the live document and
// no version (WriteIssueAskStates), and the close may leave an edit it could not settle. A block
// that only such an edit made is not indexed, since no version holds its text, and a block it
// changed or deleted keeps the version and text its row holds: the reopen's settlement indexes
// the edit at the version it writes, the first that holds it, as block history names only
// versions that hold a block's text. The live blocks were read before the owner row was locked,
// so they are used only while the pending settlement's marked_at, which every appended update
// moves, is still the one listed; otherwise the document is left owed for the next tick to read
// again. A version this build cannot read back is indexed with no blocks.
//
// A document the server cannot read (ErrDocumentUnloadable, ErrDocOutsideSchema) or that has no
// stored state has no block to show, so its latest version's markdown is indexed with no blocks.
//
// A pair the index inserts takes the version's created_at, and the source_seq of the event that
// recorded the version (stampVersionEvent).
func (s *Service) indexOwedDocument(ctx context.Context, document owedDocument) (indexed, liveAhead bool, err error) {
	if document.version == 0 {
		return false, false, nil
	}
	markdown, blocks, err := s.TextWithBlocks(ctx, document.id)
	unreadable := false
	switch {
	case ctx.Err() != nil:
		return false, false, ctx.Err()
	case errors.Is(err, ErrDocumentUnloadable), errors.Is(err, ErrDocOutsideSchema):
		slog.Warn("dispatch: block index cannot read the document; its latest version is indexed with no blocks",
			"artifact", document.id, "error", err)
		unreadable = true
	case err != nil:
		return false, false, err
	}

	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback(ctx)
	owner, open, err := lockArtifactOwner(ctx, tx, document.id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var version int
	var stored string
	var versionAt time.Time
	var owed bool
	var markedAt *time.Time
	err = tx.QueryRow(ctx, `
		select v.number, v.markdown, v.created_at, b.version is null,
		       (select p.marked_at from doc_settlements_pending p where p.artifact_id = b.artifact_id)
		from document_block_versions b
		join lateral (
			select number, markdown, created_at from artifact_versions
			where artifact_id = b.artifact_id and markdown is not null
			order by number desc limit 1
		) v on true
		where b.artifact_id = $1
		for no key update of b
	`, document.id).Scan(&version, &stored, &versionAt, &owed, &markedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("re-read the block index owed by %s: %w", document.id, err)
	}
	if version != document.version || !owed {
		return false, false, nil
	}
	switch {
	case unreadable || markdown == "" && len(blocks) == 0:
		markdown, blocks = stored, nil
	case markedAt != nil && markdown != stored:
		if open {
			return false, true, nil
		}
		if !markedAt.Equal(document.markedAt) {
			return false, false, nil
		}
		indexRows, err := indexedBlocks(ctx, tx, document.id)
		if err != nil {
			return false, false, err
		}
		held, heldBlocks, err := closedVersionBlocks(stored, markdown, blocks, indexRows)
		if err != nil {
			slog.Warn("dispatch: block index cannot read back a closed issue's document version; it is indexed with no blocks",
				"artifact", document.id, "version", version, "error", err)
			held, heldBlocks = stored, nil
		}
		markdown, blocks = held, heldBlocks
	}
	if _, err := s.indexRenderedVersion(ctx, tx, document.id, version, versionAt, markdown, blocks); err != nil {
		return false, false, err
	}
	if err := stampVersionEvent(ctx, tx, owner, document.id, version, versionAt); err != nil {
		return false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, err
	}
	return true, false, nil
}

// stampVersionEvent gives the edges a re-index of version inserted the source_seq of the event
// that recorded that version: its artifact.version, or for version 1 the document's
// artifact.created, or the issue.created of the issue whose spec it seeded. A version write stamps
// its own edges once it appends its event (refs.Stamp);
// this is that stamp for a version a build without the block index wrote. It stamps only what the
// re-index inserted - the block pairs it created at this version, and the document's references
// it dated versionAt - so an edge rebuild-refs inserted keeps no source_seq. With no such event
// the edges keep none.
func stampVersionEvent(ctx context.Context, tx pgx.Tx, owner artifactOwner, artifactID string, version int, versionAt time.Time) error {
	var eventID int64
	err := tx.QueryRow(ctx, `
		select id from events
		where (issue_key = $1 or artifact_id = $2::uuid)
		  and (type = 'artifact.version' and payload->>'artifact_id' = $3 and payload->'version'->>'number' = $4
		       or $5 and type = 'artifact.created' and payload->'artifact'->>'id' = $3
		       or $5 and type = 'issue.created' and issue_key = $1
		          and exists (select 1 from artifacts a where a.id = $2::uuid and a.is_primary))
		order by id desc
		limit 1
	`, owner.IssueKey, artifactID, artifactID, strconv.Itoa(version), version == 1).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the event that recorded version %d of %s: %w", version, artifactID, err)
	}
	if _, err := tx.Exec(ctx, `
		update block_refs set source_seq = $3
		where artifact_id = $1 and created_version = $2 and source_seq is null
	`, artifactID, version, eventID); err != nil {
		return fmt.Errorf("stamp the block mentions of version %d of %s: %w", version, artifactID, err)
	}
	if _, err := tx.Exec(ctx, `
		update refs set source_seq = $3
		where from_kind = 'artifact' and from_id = $1 and source_seq is null and created_at = $2
	`, artifactID, versionAt, eventID); err != nil {
		return fmt.Errorf("stamp the references of version %d of %s: %w", version, artifactID, err)
	}
	return nil
}
