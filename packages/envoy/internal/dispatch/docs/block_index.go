package docs

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pmezard/go-difflib/difflib"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
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
	// LiveAhead is how many owed documents, none of a closed issue, it skipped because their live
	// text is ahead of their latest version and differs from it: owed a settlement that has not run.
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
// owed a block index (document_block_versions.version null) - each one at migration 0121, each
// version any build writes (migration 0122's trigger) that its own writer did not index, and each
// rebuild-refs marks - and every comment, ask and issue message whose text holds `#b-`, owed an
// index with the block grammar (block_grammar_owed, migrations 0123-0126), which a build older
// than the block grammar indexes without. One tick per lease, at start, then every blockIndexStep
// while a batch is full and every BlockIndexReconcileInterval once none is, until ctx ends.
func (s *Service) RunBlockIndexReconciliation(ctx context.Context, leases duty.Leases) {
	ctx = store.WithTransactionTracking(ctx)
	var cursor string
	// logged is the last tick this loop logged: a tick that indexed nothing and leaves the same
	// counts (an open issue's document whose live text stays ahead, skipped every tick) logs nothing.
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
	case markedAt != nil && markdown != stored && open:
		return false, true, nil
	case markedAt != nil && markdown != stored && !markedAt.Equal(document.markedAt):
		return false, false, nil
	case markedAt != nil && markdown != stored:
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

// indexedBlock is a block row of a document's index: its id, type and text's sha256 (hex).
type indexedBlock struct {
	id, typ, sum string
}

// indexedBlocks is each block row the index holds for document artifactID, by id, read in the
// caller's transaction, which holds the document's owner row.
func indexedBlocks(ctx context.Context, tx pgx.Tx, artifactID string) ([]indexedBlock, error) {
	rows, err := tx.Query(ctx, `
		select block_id, type, text_sha256 from document_blocks where artifact_id = $1 order by block_id
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the indexed blocks of %s: %w", artifactID, err)
	}
	indexed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (indexedBlock, error) {
		var block indexedBlock
		err := row.Scan(&block.id, &block.typ, &block.sum)
		return block, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the indexed blocks of %s: %w", artifactID, err)
	}
	return indexed, nil
}

// closedVersionBlocks is the rendering of the version whose stored markdown is stored, with each
// block of it that a row of the document's index (indexed, by id) or a block of the live document
// - live, with liveBlocks (TextWithBlocks) - accounts for, under that row's or live block's id. A
// block of the version that neither accounts for, and a live block that matches no block of it,
// are left out, so each block indexed at the version is text that version holds.
//
// The version's markdown names the id of a typed block alone; its parse mints the others, which no
// reader knows, so those ids are never indexed. A typed block of the version is the live block of
// its id and type, whose text may differ from the version's in what the close wrote into it.
//
// Every row of the index that holds the text of a block of the version claims that block before
// any live text is compared. A row was indexed from a version's text of its block under that
// block's id, so where the version holds the row's type and text (the sha256 of markdown[From:To],
// as refs.IndexBlocks hashes it), the block keeps its row, its changed version and its text under
// its own id rather than being recorded deleted at a version that holds it, and a live block an
// edit typed with the same text takes none of it. The rows claim in two steps. First the
// version's untyped blocks are aligned in document order with the live untyped blocks that have a
// row, keyed by that row's type and text sha256 against each version block's, by difflib's
// matching blocks with no junk heuristic: unlike a greedy first match, a block repeated further
// on, or a block an edit moved, cannot pull the alignment past the unchanged run that follows it,
// and the longest runs of unchanged blocks, which an edit leaves around itself, are kept whole, so
// each of several identical blocks keeps its own row. Then each block of the version still
// unmatched takes the row of its type and text whose id no other block took: a typed block only
// the row of its own id, an untyped one the first such row whose id is a live block's, in live
// document order, else the first of the others by id. A block an edit moved across a first-step
// match keeps its row so, and a block an unsettled edit deleted keeps its row under its own id,
// which no live block holds.
//
// Only then is live text compared, between the version's untyped blocks no row claimed and the
// live untyped blocks whose id no row claimed for a block of the version, keyed by type and live
// text, each typed block inside the live one read as the version's block of that id, so a block
// holding an ask whose state the close wrote still matches. They are aligned by difflib in the
// same way within each gap between two consecutive first-step matches, so no match crosses one.
// Those matches are the only bounds that keep document order on both sides: difflib finds a match
// in every span that holds an equal pair, so a block of the version that the second step gave a
// live block's row is one an edit moved across a first-step match, and a block it gave the row of
// an id no live block holds has no place in the live document to cross. The text alignment
// matches every untyped block of a document none of whose blocks the index holds yet, and a block
// whose row holds an older version's text that this version changed. A version is compared and
// indexed as it renders today, as the live text is. A text either side still holds more than once
// takes no part in that alignment: with no row to name its copies, the version's copies and the
// live ones are paired in order only where both hold equally many, and otherwise none is matched
// until a settlement versions them, so a copy an edit typed never takes an original's id and
// history (an original left unmatched is dated to the version the reopen's settlement writes).
// Last, a block of the version and a live block both still unmatched take each other where each
// is the only one left on its side holding that text: a block an edit moved across others, in a
// document with no row to name it, matches no block within its gap.
//
// A row whose text the version does not hold - indexed at an older version whose text of that
// block this version changed - and whose block's live text the version does not hold either
// matches no block, and is still recorded deleted at this version, although this version holds
// the block.
func closedVersionBlocks(stored, live string, liveBlocks []model.ArtifactBlock, indexed []indexedBlock) (string, []model.ArtifactBlock, error) {
	tree, err := pmdoc.ParseRendering(stored)
	if err != nil {
		return "", nil, err
	}
	markdown, offsets, err := documentMarkdownWithOffsets(tree)
	if err != nil {
		return "", nil, err
	}
	firstHolder := make(map[string]int, len(liveBlocks))
	for index, block := range liveBlocks {
		if _, held := firstHolder[block.ID]; !held {
			firstHolder[block.ID] = index
		}
	}
	rows := make(map[string]indexedBlock, len(indexed))
	for _, row := range indexed {
		rows[row.id] = row
	}
	ids := make([]string, len(offsets))
	sums := make([]string, len(offsets))
	typedTexts := map[string]string{}
	var versionSums, versionKeys []string
	var versionUntyped []int
	for index, offset := range offsets {
		sum := sha256.Sum256([]byte(markdown[offset.From:offset.To]))
		sums[index] = offset.Type + "\x00" + hex.EncodeToString(sum[:])
		if pmdoc.IsTypedBlock(offset.Type) {
			if at, held := firstHolder[offset.ID]; held && liveBlocks[at].Type == offset.Type {
				ids[index] = offset.ID
				typedTexts[offset.ID] = markdown[offset.From:offset.To]
			}
			continue
		}
		versionSums = append(versionSums, sums[index])
		versionKeys = append(versionKeys, offset.Type+"\x00"+markdown[offset.From:offset.To])
		versionUntyped = append(versionUntyped, index)
	}
	var liveKeys, liveSums []string
	var liveUntyped, liveIndexed []int
	for index, block := range liveBlocks {
		if pmdoc.IsTypedBlock(block.Type) {
			continue
		}
		if row, held := rows[block.ID]; held {
			liveSums = append(liveSums, row.typ+"\x00"+row.sum)
			liveIndexed = append(liveIndexed, len(liveUntyped))
		}
		liveKeys = append(liveKeys, block.Type+"\x00"+versionText(live, liveBlocks, index, typedTexts))
		liveUntyped = append(liveUntyped, index)
	}
	var anchors [][2]int
	for _, match := range difflib.NewMatcherWithJunk(versionSums, liveSums, false, nil).GetMatchingBlocks() {
		for step := range match.Size {
			anchor := [2]int{match.A + step, liveIndexed[match.B+step]}
			ids[versionUntyped[anchor[0]]] = liveBlocks[liveUntyped[anchor[1]]].ID
			anchors = append(anchors, anchor)
		}
	}
	used := make(map[string]bool, len(offsets))
	for _, id := range ids {
		if id != "" {
			used[id] = true
		}
	}
	rank := func(id string) int {
		if at, held := firstHolder[id]; held {
			return at
		}
		return len(liveBlocks)
	}
	slices.SortStableFunc(indexed, func(a, b indexedBlock) int { return cmp.Compare(rank(a.id), rank(b.id)) })
	holders := make(map[string][]string, len(indexed))
	for _, row := range indexed {
		key := row.typ + "\x00" + row.sum
		holders[key] = append(holders[key], row.id)
	}
	for index, offset := range offsets {
		if ids[index] != "" {
			continue
		}
		for _, id := range holders[sums[index]] {
			if !used[id] && (!pmdoc.IsTypedBlock(offset.Type) || id == offset.ID) {
				ids[index], used[id] = id, true
				break
			}
		}
	}
	// Text names a block only where it is not repeated. A text the version or the live document
	// still holds more than once, with no row to name its copies, says which copy is which only when
	// both hold the same number of copies, in order; otherwise a copy an edit typed would take an
	// original's id and history, so no copy is matched until a settlement versions them.
	versionCopies := map[string]int{}
	for at, index := range versionUntyped {
		if ids[index] == "" {
			versionCopies[versionKeys[at]]++
		}
	}
	liveCopies := map[string]int{}
	for at, index := range liveUntyped {
		if !used[liveBlocks[index].ID] {
			liveCopies[liveKeys[at]]++
		}
	}
	repeated := func(key string) bool { return versionCopies[key] > 1 || liveCopies[key] > 1 }
	var gapVersion, gapLive []string
	var gapVersionAt, gapLiveAt []int
	fromVersion, fromLive := 0, 0
	for _, anchor := range append(anchors, [2]int{len(versionUntyped), len(liveUntyped)}) {
		gapVersion, gapVersionAt = gapVersion[:0], gapVersionAt[:0]
		for at := fromVersion; at < anchor[0]; at++ {
			if ids[versionUntyped[at]] == "" && !repeated(versionKeys[at]) {
				gapVersion, gapVersionAt = append(gapVersion, versionKeys[at]), append(gapVersionAt, versionUntyped[at])
			}
		}
		gapLive, gapLiveAt = gapLive[:0], gapLiveAt[:0]
		for at := fromLive; at < anchor[1]; at++ {
			if !used[liveBlocks[liveUntyped[at]].ID] && !repeated(liveKeys[at]) {
				gapLive, gapLiveAt = append(gapLive, liveKeys[at]), append(gapLiveAt, liveUntyped[at])
			}
		}
		if len(gapVersion) > 0 && len(gapLive) > 0 {
			for _, match := range difflib.NewMatcherWithJunk(gapVersion, gapLive, false, nil).GetMatchingBlocks() {
				for step := range match.Size {
					ids[gapVersionAt[match.A+step]] = liveBlocks[gapLiveAt[match.B+step]].ID
					used[liveBlocks[gapLiveAt[match.B+step]].ID] = true
				}
			}
		}
		fromVersion, fromLive = anchor[0]+1, anchor[1]+1
	}
	// A repeated text whose copies the version and the live document hold equally many of pairs
	// them in order: the first copy with the first, and so on.
	copiesLeft := map[string][]int{}
	for at, index := range liveUntyped {
		if key := liveKeys[at]; !used[liveBlocks[index].ID] && repeated(key) && versionCopies[key] == liveCopies[key] {
			copiesLeft[key] = append(copiesLeft[key], index)
		}
	}
	for at, index := range versionUntyped {
		if key := versionKeys[at]; ids[index] == "" && len(copiesLeft[key]) > 0 {
			id := liveBlocks[copiesLeft[key][0]].ID
			ids[index], used[id] = id, true
			copiesLeft[key] = copiesLeft[key][1:]
		}
	}
	// A block an edit moved across others, with no row to claim it (a document none of whose blocks
	// the index holds yet), matches no block in its gap. Its text names it where exactly one block
	// of the version and one live block still unmatched hold it.
	versionHolding := map[string][]int{}
	for at, index := range versionUntyped {
		if ids[index] == "" {
			versionHolding[versionKeys[at]] = append(versionHolding[versionKeys[at]], index)
		}
	}
	liveHolding := map[string][]int{}
	for at, index := range liveUntyped {
		if !used[liveBlocks[index].ID] {
			liveHolding[liveKeys[at]] = append(liveHolding[liveKeys[at]], index)
		}
	}
	for key, held := range versionHolding {
		if holding := liveHolding[key]; len(held) == 1 && len(holding) == 1 {
			ids[held[0]] = liveBlocks[holding[0]].ID
		}
	}
	blocks := make([]model.ArtifactBlock, 0, len(offsets))
	for index, offset := range offsets {
		if ids[index] != "" {
			blocks = append(blocks, model.ArtifactBlock{ID: ids[index], Type: offset.Type, From: offset.From, To: offset.To})
		}
	}
	return markdown, blocks, nil
}

// versionText is the text of blocks[index] in live, with each typed block inside it that the
// version holds (typedTexts, by id) read as the version's text of that block. blocks are in
// document order, each parent before its children, so the blocks inside one follow it up to the
// first that starts at or past its end.
func versionText(live string, blocks []model.ArtifactBlock, index int, typedTexts map[string]string) string {
	outer := blocks[index]
	var text strings.Builder
	at := outer.From
	for _, inner := range blocks[index+1:] {
		if inner.From >= outer.To {
			break
		}
		held, typed := typedTexts[inner.ID]
		if inner.From < at || !typed || !pmdoc.IsTypedBlock(inner.Type) {
			continue
		}
		text.WriteString(live[at:inner.From])
		text.WriteString(held)
		at = inner.To
	}
	if at == outer.From {
		return live[outer.From:outer.To]
	}
	text.WriteString(live[at:outer.To])
	return text.String()
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
