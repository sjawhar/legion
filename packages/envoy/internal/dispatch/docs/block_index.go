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

// blockIndexDocumentBatch is how many owed documents one block-index tick reads and
// blockIndexItemBatch how many owed items; blockIndexStep is the wait before the next tick while
// either batch was full, BlockIndexReconcileInterval the wait once neither is. The owed reads go
// through partial indexes (document_block_versions_owed, block_grammar_owed_enqueued), so a tick
// that finds nothing owed reads nothing else.
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
	// Documents is how many owed documents it indexed, and Items how many owed comments, asks and
	// issue messages it indexed with the block grammar.
	Documents int
	Items     int
	// LiveAhead is how many owed documents it skipped because their live text is ahead of their
	// latest version and differs from it: owed a settlement that has not run.
	LiveAhead int
	// OwedDocuments and OwedItems are what is still owed after the tick.
	OwedDocuments int
	OwedItems     int
	// Cursor is the document the next tick's batch starts after: the last one this batch listed
	// while the batch was full, empty once a batch lists fewer and the next starts over.
	Cursor string
	// Full reports a batch of documents or of items that was full, so more may be owed.
	Full bool
}

// RunBlockIndexReconciliation indexes what no block-aware version write indexed: every document
// owed a block index (document_block_versions.version null) - each one at migration 0120, each
// version any build writes (migration 0121's trigger) that its own writer did not index, and each
// rebuild-refs marks - and every comment, ask and issue message whose text holds `#b-`, owed an
// index with the block grammar (block_grammar_owed, migrations 0122-0125), which a build older
// than the block grammar indexes without. One tick per lease, at start, then every blockIndexStep
// while a batch is full and every BlockIndexReconcileInterval once none is, until ctx ends.
func (s *Service) RunBlockIndexReconciliation(ctx context.Context, leases duty.Leases) {
	ctx = store.WithTransactionTracking(ctx)
	var cursor string
	// logged is the last tick this loop logged: a tick that indexed nothing and leaves the same
	// counts (a closed document whose live text stays ahead, skipped every tick) logs nothing.
	var logged BlockIndexTick
	liveAhead := liveAheadDocuments{}
	// period is this loop's own interval at this moment, the wait the last tick asked for, as
	// in RunReferenceExcerptReconciliation: a tick inside it is one some process already made.
	period := BlockIndexReconcileInterval
	for {
		// A tick whose lease another process holds, or whose period that process has already
		// served, waits the full interval: that process is doing this work.
		wait := BlockIndexReconcileInterval
		duty.Exclusive(ctx, leases, blockIndexLease, period, func(ctx context.Context) {
			wait, cursor = s.blockIndexTick(ctx, cursor, liveAhead, &logged)
		})
		period = wait
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
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
	if tick.Documents > 0 || tick.Items > 0 || tick.LiveAhead != logged.LiveAhead ||
		tick.OwedDocuments != logged.OwedDocuments || tick.OwedItems != logged.OwedItems {
		slog.Info("dispatch: block index", "documents", tick.Documents, "items", tick.Items,
			"owed_documents", tick.OwedDocuments, "owed_items", tick.OwedItems, "live_ahead", tick.LiveAhead)
		*logged = tick
	}
	if tick.Full {
		return blockIndexStep, tick.Cursor
	}
	return BlockIndexReconcileInterval, tick.Cursor
}

// owedDocument is a document owed a block index as it was listed: its latest version, and the
// marked_at of its pending settlement, zero when it owes none.
type owedDocument struct {
	id       string
	version  int
	markedAt time.Time
}

// liveAheadDocuments is, by id, each owed document a tick found live ahead (indexOwedDocument), as
// it was listed then. Every update appended to a document's store moves its pending settlement's
// marked_at (markSettlementPending), and every version write moves its version, so a document
// listed again with both unchanged still holds the stored live text a tick found ahead: it is
// counted live ahead without being read or locked. Only the store's text is remembered: a room
// this server holds can be ahead of the store (an edit not yet appended) or behind it (another
// process's append it has not yet been evicted for), so a document is neither remembered nor
// taken from here while one is resident.
type liveAheadDocuments map[string]owedDocument

// ReconcileBlockIndex runs one tick: one batch of the documents owed a block index past after (a
// document id; empty is the start), and one batch of the items owed an index with the block
// grammar, oldest first (refs.ReindexOwedItem). The documents are listed by keyset, so a document
// it skips never holds back the ones past it: the tick's Cursor moves past every document its
// batch lists, indexed or not, and goes back to the start once a batch lists fewer than a full
// one. A document or item it cannot index for a reason of its own is logged and left owed, so one
// never holds back the rest of its batch. liveAhead is the documents the calling loop's earlier
// ticks found live ahead, which this tick counts again without reading and keeps up to date.
func (s *Service) ReconcileBlockIndex(ctx context.Context, after string, liveAhead liveAheadDocuments) (BlockIndexTick, error) {
	tick := BlockIndexTick{Cursor: after}
	owed, err := s.owedDocuments(ctx, after)
	if err != nil {
		return tick, err
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
			if cold && !document.markedAt.IsZero() && s.srv.GetDoc(document.id) == nil {
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
	tick.Full = documentsFull || len(items) == blockIndexItemBatch

	if err := s.store.Pool.QueryRow(ctx, `
		select (select count(*) from document_block_versions where version is null),
		       (select count(*) from block_grammar_owed)
	`).Scan(&tick.OwedDocuments, &tick.OwedItems); err != nil {
		return tick, fmt.Errorf("count what is owed a block index: %w", err)
	}
	return tick, nil
}

// stillLiveAhead reports whether document, as listed now, is one liveAhead holds at the same
// version and settlement mark, with no room of this server resident for it.
func (s *Service) stillLiveAhead(liveAhead liveAheadDocuments, document owedDocument) bool {
	found, ok := liveAhead[document.id]
	return ok && found.version == document.version && found.markedAt.Equal(document.markedAt) &&
		s.srv.GetDoc(document.id) == nil
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
// past after, each with its latest markdown version and its pending settlement's marked_at. A
// document with no markdown version has nothing to index and is listed with version 0, which
// indexOwedDocument passes over.
func (s *Service) owedDocuments(ctx context.Context, after string) ([]owedDocument, error) {
	var cursor any
	if after != "" {
		cursor = after
	}
	rows, err := s.store.Pool.Query(ctx, `
		select b.artifact_id::text,
		       coalesce((select max(v.number) from artifact_versions v
		                 where v.artifact_id = b.artifact_id and v.markdown is not null), 0),
		       (select p.marked_at from doc_settlements_pending p where p.artifact_id = b.artifact_id)
		from document_block_versions b
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
		if err := rows.Scan(&document.id, &document.version, &markedAt); err != nil {
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
// byte. A document whose live text is ahead of its latest version, and differs from it, is
// skipped and reported liveAhead, and stays owed: indexing it at that version would date a later
// edit's blocks to it. Its settlement indexes it when it writes the next version - which, on a
// closed issue, waits for the issue to reopen (RunSettlementResumption resumes open issues only).
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
	owner, _, err := lockArtifactOwner(ctx, tx, document.id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var version int
	var stored string
	var versionAt time.Time
	var owed, pending bool
	err = tx.QueryRow(ctx, `
		select v.number, v.markdown, v.created_at, b.version is null,
		       exists (select 1 from doc_settlements_pending p where p.artifact_id = b.artifact_id)
		from document_block_versions b
		join lateral (
			select number, markdown, created_at from artifact_versions
			where artifact_id = b.artifact_id and markdown is not null
			order by number desc limit 1
		) v on true
		where b.artifact_id = $1
		for no key update of b
	`, document.id).Scan(&version, &stored, &versionAt, &owed, &pending)
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
	case pending && markdown != stored:
		return false, true, nil
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
