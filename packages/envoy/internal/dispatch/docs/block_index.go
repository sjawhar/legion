package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// blockIndexDocumentBatch is how many owed documents one block-index tick reads,
// blockIndexItemBatch how many items owed an index with the block grammar and, separately, how
// many comments, messages and asks owed a block index of their own blocks;
// blockIndexWindowBatch how many owners with blocks and no embedding window it gives windows
// to and blockIndexRetireBatch the most window owners one kind's pass of the whole-text
// retirement reads, and the most rows the retirement deletes in one tick across the kinds;
// blockIndexStep is the wait before the next tick while any batch was full,
// BlockIndexReconcileInterval the wait
// once none is. The owed reads go through partial indexes (document_block_versions_owed,
// block_grammar_owed_enqueued, comment_block_state_owed, message_block_state_owed,
// ask_block_state_owed), so a tick that finds nothing owed reads nothing else.
const (
	blockIndexDocumentBatch     = 20
	blockIndexItemBatch         = 200
	blockIndexWindowBatch       = 200
	blockIndexRetireBatch       = 500
	blockIndexStep              = 2 * time.Second
	BlockIndexReconcileInterval = 30 * time.Second
)

// BlockIndexLease names the lease one block-index tick runs under. The index it writes is the
// same whichever process writes it, so two processes indexing the same batch is work done twice
// for one result. It is exported for the memory harness, which takes this lease itself for the
// length of each measured window (cmd/dispatch's memoryServer.peakAboveIdle): a pass that ran
// inside such a window would read whole documents of its own, and the peak measured there would
// be that pass's memory and not the request's.
const BlockIndexLease = "block-index"

// BlockIndexCursors is where a tick's keyset reads start, as the tick before it left them
// (BlockIndexTick.Cursor): the owed documents, and, for each owner kind the window arm walks
// ("document", "comment", "message", "ask"), the owner its batch of that kind starts after. A
// zero value starts every read from the beginning.
//
// Retire is the same for the retirement arm, keyed by the whole-text kind the arm retires
// ("document", "comment", "message", "ask") and holding the owner key of a window - the id up
// to the first #, so an item's carries its "<kind>/" prefix - that kind's next pass starts
// after.
type BlockIndexCursors struct {
	Documents string
	Windows   map[string]string
	Retire    map[string]string
}

// BlockIndexTick is what one tick of the block-index reconciliation did (ReconcileBlockIndex).
type BlockIndexTick struct {
	// Documents is how many owed documents it indexed, Items how many owed comments, asks and
	// issue messages it indexed with the block grammar, and ItemBlocks how many owed comments and
	// messages (direct messages included) it indexed the blocks of.
	Documents  int
	Items      int
	ItemBlocks int
	// Texts is how many rows of the re-index queue (refs_grammar_owed) it drained: a comment, an
	// ask or an issue message re-indexed with this build's whole grammar, a document owed that
	// index for the documents batch to write, or one document's whole scope re-indexed, which is
	// the one row a `scope` row is counted as.
	Texts int
	// Windows is how many owners with blocks and no embedding window it gave windows to, and
	// Retired how many whole-text embeddings rows it deleted behind a current window.
	Windows int
	Retired int
	// LiveAhead is how many owed documents, none of a closed issue, it skipped because their live
	// text is ahead of their latest version and differs from it: owed a settlement that has not run.
	LiveAhead int
	// OwedDocuments, OwedTexts, OwedItems and OwedItemBlocks are what is still owed after the tick.
	OwedDocuments  int
	OwedTexts      int
	OwedItems      int
	OwedItemBlocks int
	// Cursor is where the next tick's batches start: the last owner each batch listed while that
	// batch was full, and the start again once a batch lists fewer.
	Cursor BlockIndexCursors
	// Full reports a batch of documents, of texts, of items, of item blocks or of owners with
	// no window that was full, or a retirement pass that deleted a row, so more may be
	// waiting. The retirement term counts rows deleted rather than a full batch because that
	// arm reads window owners and retires only the few that still hold a whole-text row: a
	// pass that read its whole slice and deleted twenty rows is not full by any count of its
	// own, and the walk would then advance one slice every BlockIndexReconcileInterval instead
	// of every blockIndexStep.
	Full bool
}

// RunBlockIndexReconciliation indexes what no block-aware version write indexed: every document
// owed a block index (document_block_versions.version null) - each one at migration 0121, each
// version any build writes (migration 0122's trigger) that its own writer did not index, and each
// rebuild-refs marks - every comment, ask and issue message whose text holds `#b-`, owed an
// index with the block grammar (block_grammar_owed, migrations 0123-0126), which a build older
// than the block grammar indexes without, every source owed an index with this build's whole
// grammar (refs_grammar_owed, migrations 20261103205037-20261103205042), which a build older than
// the bare issue keys, `#id`s and short ids indexes without, and every comment, message and ask
// owed a block index
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
	var cursors BlockIndexCursors
	// logged is the last tick this loop logged: a tick that indexed nothing and leaves the same
	// counts (an open issue's document whose live text stays ahead, skipped every tick) logs nothing.
	var logged BlockIndexTick
	liveAhead := liveAheadDocuments{}
	duty.RunLeasedAfterEachPass(ctx, leases, BlockIndexLease, BlockIndexReconcileInterval, func(ctx context.Context) time.Duration {
		var wait time.Duration
		wait, cursors = s.blockIndexTick(ctx, cursors, liveAhead, &logged)
		return wait
	})
}

// blockIndexTick runs one tick from cursors, logs what it did unless it indexed nothing and left
// the counts *logged holds, and returns how long to wait before the next tick and the cursors it
// starts from.
func (s *Service) blockIndexTick(ctx context.Context, cursors BlockIndexCursors, liveAhead liveAheadDocuments, logged *BlockIndexTick) (time.Duration, BlockIndexCursors) {
	tick, err := s.ReconcileBlockIndex(ctx, cursors, liveAhead)
	if ctx.Err() != nil {
		return BlockIndexReconcileInterval, cursors
	}
	if err != nil {
		slog.Error("dispatch: block index reconciliation", "error", err)
	}
	if tick.Documents > 0 || tick.Items > 0 || tick.ItemBlocks > 0 || tick.Texts > 0 ||
		tick.Windows > 0 || tick.Retired > 0 || tick.LiveAhead != logged.LiveAhead ||
		tick.OwedDocuments != logged.OwedDocuments || tick.OwedItems != logged.OwedItems ||
		tick.OwedItemBlocks != logged.OwedItemBlocks || tick.OwedTexts != logged.OwedTexts {
		slog.Info("dispatch: block index", "documents", tick.Documents, "items", tick.Items,
			"item_blocks", tick.ItemBlocks, "texts", tick.Texts, "windows", tick.Windows,
			"retired", tick.Retired, "owed_documents", tick.OwedDocuments,
			"owed_items", tick.OwedItems, "owed_item_blocks", tick.OwedItemBlocks,
			"owed_texts", tick.OwedTexts, "live_ahead", tick.LiveAhead)
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

// ReconcileBlockIndex runs one tick: one batch of the documents owed a block index past
// cursors.Documents (a document id; empty is the start), one batch of the items owed an index
// with the block grammar, oldest first (refs.ReindexOwed), one batch of the comments, messages
// and asks owed a block index of their own blocks, oldest owed_at first
// (refs.IndexOwedItemBlocks), one batch of the sources owed an index with the whole grammar,
// oldest first (reindexOwedText), each in its own transaction taking the item row then its
// state row, never an owner row, then one batch of the owners whose embedding windows do not
// stand for their blocks (windowDrift) and one slice of the window owners whose whole-text
// embeddings rows the windows have replaced (retireWholeText). The item blocks run after the
// block grammar's items, so an item that batch indexed, blocks included, is no longer owed, and
// before the windows, so an item the grammar batch indexed, windows included, is no longer
// listed; the whole grammar's sources run after them, so an item it would index the blocks of
// again is indexed once by each queue that holds it rather than twice by one. A document that
// batch owes an index is indexed by the documents batch, which owns that index, in this tick
// when it was already owed and in the next otherwise. The documents are listed by keyset, so a
// document it skips never holds back the ones past it: the tick's Cursor moves past every
// document its batch lists, indexed or not, and goes back to the start once a batch lists fewer
// than a full one, and the window and retirement arms walk their owners the same way. A
// document, item or source it cannot index for a reason of its own is logged and left owed, so
// one never holds back the rest of its batch. liveAhead is the documents the calling loop's
// earlier ticks found live ahead, which this tick counts again without reading and keeps up to
// date.
func (s *Service) ReconcileBlockIndex(ctx context.Context, cursors BlockIndexCursors, liveAhead liveAheadDocuments) (BlockIndexTick, error) {
	// A caller outside this package can only pass nil, since liveAheadDocuments is unexported:
	// the api package's tests and cmd/dispatch's memory harness, which
	// drains what is owed before it measures a server, both do. The map is a cache the calling
	// loop keeps across its own ticks, so a throwaway one costs no more than reading again the
	// documents this one tick finds live ahead.
	if liveAhead == nil {
		liveAhead = liveAheadDocuments{}
	}
	after := cursors.Documents
	tick := BlockIndexTick{Cursor: BlockIndexCursors{Documents: after}}
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
			tick.Cursor.Documents = document.id
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
		tick.Cursor.Documents = document.id
	}
	if !documentsFull {
		tick.Cursor.Documents = ""
	}

	items, err := refs.ListOwed(ctx, s.store.Pool, refs.BlockGrammarOwed, blockIndexItemBatch)
	if err != nil {
		return tick, err
	}
	for _, item := range items {
		indexed, err := refs.ReindexOwed(ctx, s.store.Pool, item, s.serverURL)
		if err != nil {
			if ctx.Err() != nil {
				return tick, ctx.Err()
			}
			slog.Error("dispatch: block index skipped an item it could not index",
				"kind", item.Kind, "id", item.ID, "error", err)
			continue
		}
		if indexed {
			tick.Items++
		}
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

	texts, err := refs.ListOwed(ctx, s.store.Pool, refs.RefsGrammarOwed, blockIndexItemBatch)
	if err != nil {
		return tick, err
	}
	for _, source := range texts {
		indexed, err := s.reindexOwedText(ctx, source)
		if err != nil {
			if ctx.Err() != nil {
				return tick, ctx.Err()
			}
			slog.Error("dispatch: block index skipped text it could not index with the whole grammar",
				"kind", source.Kind, "id", source.ID, "error", err)
			continue
		}
		if indexed {
			tick.Texts++
		}
	}

	windowsFull, err := s.windowDrift(ctx, cursors.Windows, &tick)
	if err != nil {
		return tick, err
	}
	if err := s.retireWholeText(ctx, cursors.Retire, &tick); err != nil {
		return tick, err
	}
	tick.Full = documentsFull || len(items) == blockIndexItemBatch ||
		len(itemBlocks) == blockIndexItemBatch || len(texts) == blockIndexItemBatch ||
		windowsFull || tick.Retired > 0

	if err := s.store.Pool.QueryRow(ctx, `
		select (select count(*) from document_block_versions where version is null),
		       (select count(*) from block_grammar_owed),
		       (select count(*) from refs_grammar_owed),
		       (select count(*) from comment_block_state where owed_at is not null) +
		       (select count(*) from message_block_state where owed_at is not null) +
		       (select count(*) from ask_block_state where owed_at is not null)
	`).Scan(&tick.OwedDocuments, &tick.OwedItems, &tick.OwedTexts, &tick.OwedItemBlocks); err != nil {
		return tick, fmt.Errorf("count what is owed a block index: %w", err)
	}
	return tick, nil
}

// windowOwner is one kind of owner the window arm walks: name keys its cursor and names it in a
// log line, batch is the most of one tick's shared batch its kind may take, list lists at most
// limit owners of the kind whose windows do not stand for their blocks, in id order past after,
// and window writes one owner's windows and reports whether it wrote them. One loop serves them
// all, so the reset, the fullness rule and the cursor are written once and every key is this
// name.
type windowOwner struct {
	name   string
	batch  int
	list   func(ctx context.Context, after string, limit int) ([]string, error)
	window func(ctx context.Context, id string) (bool, error)
}

// windowItemKinds is the item kinds whose blocks carry embedding windows, in the order the
// window arm serves them after the documents.
var windowItemKinds = []refs.ItemKind{refs.ItemComment, refs.ItemMessage, refs.ItemAsk}

// windowOwners is the owners the window arm serves, in order: the documents, then the comments,
// the messages and the asks. The documents are held to blockIndexDocumentBatch of the shared
// batch, the bound the owed-document arm reads under, because a document's windows cost the
// same read of the document that arm makes; an item's windows are one transaction of its own,
// so its kind may take the whole batch.
func (s *Service) windowOwners() []windowOwner {
	owners := []windowOwner{{
		name:  "document",
		batch: blockIndexDocumentBatch,
		list: func(ctx context.Context, after string, limit int) ([]string, error) {
			return refs.DocumentsOwedWindows(ctx, s.store.Pool, after, limit)
		},
		window: s.windowDocument,
	}}
	for _, kind := range windowItemKinds {
		owners = append(owners, windowOwner{
			name:  string(kind),
			batch: blockIndexWindowBatch,
			list: func(ctx context.Context, after string, limit int) ([]string, error) {
				return refs.ItemsOwedWindows(ctx, s.store.Pool, kind, after, limit)
			},
			window: func(ctx context.Context, id string) (bool, error) {
				return s.windowItem(ctx, kind, id)
			},
		})
	}
	return owners
}

// windowDrift gives windows to at most blockIndexWindowBatch owners whose windows do not stand
// for their blocks: every document, comment, message and ask written before meaning search
// moved to windows, any whose write wrote its blocks and failed before its windows, and any a
// build without this change re-indexed while the deploy rolled, which left the windows of its
// old text behind (refs.DocumentsOwedWindows, refs.ItemsOwedWindows). It counts them in
// tick.Windows, leaves each kind's cursor in tick.Cursor.Windows and reports whether a batch
// was full.
//
// Each owner is windowed as its own indexer windows it, and nothing else is written: a document
// is read as the server serves it and windowed under its owner row (windowDocument), an item is
// read as the blocks routes read it and windowed under its own row (windowItem). Either way the
// write stamps the state the owner's blocks are in, so an owner this arm has served is no
// longer listed.
func (s *Service) windowDrift(ctx context.Context, cursors map[string]string, tick *BlockIndexTick) (bool, error) {
	owners := s.windowOwners()
	next := make(map[string]string, len(owners))
	tick.Cursor.Windows = next
	budget, full := blockIndexWindowBatch, false
	for _, owner := range owners {
		if budget <= 0 {
			break
		}
		asked := min(budget, owner.batch)
		ids, err := owner.list(ctx, cursors[owner.name], asked)
		if err != nil {
			return false, err
		}
		for _, id := range ids {
			windowed, err := owner.window(ctx, id)
			switch {
			case ctx.Err() != nil:
				return false, ctx.Err()
			case err != nil:
				slog.Error("dispatch: block index skipped an owner it could not window",
					"owner", owner.name, "id", id, "error", err)
			case windowed:
				tick.Windows++
			}
			next[owner.name] = id
		}
		budget -= len(ids)
		full = full || len(ids) == asked
		if len(ids) < asked {
			next[owner.name] = ""
		}
	}
	return full, nil
}

// windowItem writes the windows of one comment, message or ask whose windows do not stand for
// its blocks, in a transaction of its own: it takes the item row, reads the text its blocks
// were split from and writes the windows of those blocks (refs.WriteItemWindows). It writes no
// block row and no reference: the index of this item's blocks stands, and only its windows are
// missing. An item whose stored text no longer splits into the blocks the index holds is marked
// owed a block index instead, which the transaction commits for the owed arm to act on.
func (s *Service) windowItem(ctx context.Context, kind refs.ItemKind, id string) (bool, error) {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	windowed, err := refs.WriteItemWindows(ctx, tx, kind, id)
	if err != nil {
		return false, err
	}
	return windowed, tx.Commit(ctx)
}

// windowDocument writes the windows of one document whose blocks hold none, in a transaction of
// its own: it reads the document the server serves (TextWithBlocks) with no transaction held,
// then, under the document's owner row, which every version write holds first, writes the
// windows of those blocks (refs.WriteDocumentWindows). It writes no block row and no reference:
// the index of this document's blocks stands, and only its windows are missing.
//
// The live text is windowed only where it is the latest version's text - the document owes no
// settlement, or its live markdown equals that version's stored markdown byte for byte - as
// indexOwedDocument indexes it only there: a window of a text no version holds would cover
// blocks the index does not hold. A document whose live text is ahead keeps no window until its
// settlement writes the next version, which writes them. A document the server cannot read has
// no block to window; its blocks came from its version's own markdown and the next version write
// windows them.
func (s *Service) windowDocument(ctx context.Context, artifactID string) (bool, error) {
	markdown, blocks, err := s.TextWithBlocks(ctx, artifactID)
	switch {
	case ctx.Err() != nil:
		return false, ctx.Err()
	case errors.Is(err, ErrDocumentUnloadable), errors.Is(err, ErrDocOutsideSchema):
		return false, nil
	case err != nil:
		return false, err
	}
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	switch _, _, err := lockArtifactOwner(ctx, tx, artifactID); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	var stored string
	var pending bool
	err = tx.QueryRow(ctx, `
		select v.markdown, exists (select 1 from doc_settlements_pending p where p.artifact_id = $1)
		from artifact_versions v
		where v.artifact_id = $1 and v.markdown is not null
		order by v.number desc limit 1
	`, artifactID).Scan(&stored, &pending)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("re-read the latest version of %s for its windows: %w", artifactID, err)
	case pending && markdown != stored:
		return false, nil
	}
	if err := refs.WriteDocumentWindows(ctx, tx, artifactID, markdown, blocks); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// wholeTextOwner is one kind of whole-text embeddings row the windows replace, with the kind of
// window that stands for it and the prefix every owner key of that window kind carries before
// the owner's own id: a document's windows are under its artifact id, so its prefix is empty,
// and an item's are under "<kind>/<uuid>" (refs.ItemKind.WindowOwnerKey), so its prefix is
// "comment/", "message/" or "ask/".
type wholeTextOwner struct{ kind, windowKind, ownerPrefix string }

// wholeTextOwners is every kind of row the release before this one's meaning search reads,
// which this one replaces with windows. The issue rows are not among them: an issue's whole text
// is its title and the head of its spec, which no block indexer writes, and its trigger stands.
var wholeTextOwners = func() []wholeTextOwner {
	owners := []wholeTextOwner{{kind: "document", windowKind: refs.DocumentWindowKind}}
	for _, kind := range windowItemKinds {
		owners = append(owners, wholeTextOwner{
			kind:        string(kind),
			windowKind:  kind.WindowKind(),
			ownerPrefix: kind.WindowOwnerKey(""),
		})
	}
	return owners
}()

// owners lists, in the order embeddings_window_owner holds them, at most $2 of the owner keys
// of this kind's windows past the key $1 (empty: from the start), current or not, since retire
// tests currency. A pass walks the window owners rather than the whole-text rows behind them,
// and its limit bounds it, the cursor it leaves naming where the next pass starts; no choice
// of the planner's does.
//
// The window kind is written as a literal, as refs.windowKinds explains for every read and
// write of a window (migration 20261102233625 lines 48-49): embeddings_window_owner is partial
// over the four window kinds, and the planner can prove a literal kind satisfies that
// predicate where it can prove nothing of a parameter, so only a literal reaches the owners
// through this index.
//
// It tests neither the window's vector nor its hashes. Those columns are not in
// embeddings_window_owner, so testing them here costs a heap read for every index entry, and
// the planner answered that cost by reading all 400,000 windows in a parallel sequential scan
// and sorting them: 195 ms a pass, and growing with the windows. The currency test sits in
// retire instead, where it is one indexed probe for each owner this read. Against 400,000
// current comment_block windows this reads 500 owners in 0.885 ms, through an index scan with
// no sort and no sequential scan.
func (owner wholeTextOwner) owners() string {
	return fmt.Sprintf(`
		select distinct split_part(w.id, '#', 1) from embeddings w
		where w.kind = '%[1]s' and split_part(w.id, '#', 1) > $1
		order by 1
		limit $2`, owner.windowKind)
}

// retire deletes the whole-text rows of this kind whose id $1 holds and whose owner already
// holds a window of the matching kind carrying a vector of its current text. The release before
// this one's meaning search reads exactly these rows, so an item leaves that search only once
// this one's windows can find it, and the two builds overlap for one deploy. The vector is read
// as well as the hash: the queue settles a window whose text has nothing to embed with no
// vector and its embedded_hash set (embedqueue's settledNothingToEmbed), and such a window
// stands for nothing in a meaning search, so a row behind it would be deleted with no vector
// left for the item at all.
//
// It has no limit of its own: one pass passes the ids of one slice of owners, so that slice
// bounds it. Every read is through an index - the rows by embeddings_pkey under
// id = any($1), each owner's windows by embeddings_window_owner under the owner key the prefix
// and the row's id glue back together, with the kinds as literals for the reason owners gives.
// Against 400,000 current comment_block windows and 2,000 whole-text comment rows none of
// which can retire, 500 ids cost 2.435 ms.
//
// Both ways of writing this as one statement are unbounded. Driving the delete from the window
// rows reads every current window of the kind: 999 ms a pass on that shape, with two external
// merge sorts holding 35 MB of temp files, and production always keeps a residue of whole-text
// rows that cannot retire - one whose window settled with nothing to embed, one whose window is
// pending or stale, one of an item refs.ItemsOwedWindows never lists - so that pass pays the
// cost again on every tick for ever. Driving it from the whole-text rows reads every row of the
// kind: one pass took 25 minutes against the 482,073 comments the load-test deployment then had
// unwindowed (INFRA-988). The walk over the owners is what bounds it.
func (owner wholeTextOwner) retire() string {
	return fmt.Sprintf(`
		with picked as (
			select e.kind, e.id from embeddings e
			where e.kind = '%[1]s' and e.id = any($1::text[])
			  and exists (
				select 1 from embeddings w
				where w.kind = '%[2]s' and split_part(w.id, '#', 1) = '%[3]s' || e.id
				  and w.embedding is not null
				  and w.embedded_hash is not null and w.embedded_hash = w.content_hash
			  )
			for update skip locked
		)
		delete from embeddings using picked
		where embeddings.kind = picked.kind and embeddings.id = picked.id`,
		owner.kind, owner.windowKind, owner.ownerPrefix)
}

// pass retires one slice of this kind's whole-text rows: it reads at most limit owner keys of
// the kind's windows past after (owners), turns each key into the id of the whole-text
// row behind it, deletes the rows of those ids whose window is current (retire), and reports
// how many it deleted and the key the next pass starts after.
//
// next is the last key read while the slice came back full, and the empty string otherwise, so
// a slice shorter than asked sends the walk back to the start of the kind's owners. That is
// windowDrift's rule, held to here for the same reason.
//
// It takes a pool rather than hanging off Service so a test can run the pass itself against a
// pool of its own.
func (owner wholeTextOwner) pass(ctx context.Context, pool *store.Pool, after string, limit int) (retired int, next string, err error) {
	rows, err := pool.Query(ctx, owner.owners(), after, limit)
	if err != nil {
		return 0, "", fmt.Errorf("list the owners of the %s windows: %w", owner.windowKind, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, "", fmt.Errorf("list the owners of the %s windows: %w", owner.windowKind, err)
	}
	if len(keys) == limit {
		next = keys[len(keys)-1]
	}
	ids := make([]string, 0, len(keys))
	for _, key := range keys {
		// A key of another shape belongs to no row of this kind. The delete's exists says the
		// same, since it compares a window's owner key against this prefix glued onto the row's
		// id, and it is the one that holds whatever this hands it, for any id and any key; this
		// line only keeps such a key out of the array.
		if id, ok := strings.CutPrefix(key, owner.ownerPrefix); ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return 0, next, nil
	}
	tag, err := pool.Exec(ctx, owner.retire(), ids)
	if err != nil {
		return 0, next, fmt.Errorf("retire the whole-text embeddings of the %ss behind their windows: %w", owner.kind, err)
	}
	return int(tag.RowsAffected()), next, nil
}

// retireWholeText walks one slice of each kind's window owners, the kinds in order, and deletes
// the whole-text rows the windows have replaced. The kinds share blockIndexRetireBatch as a
// budget of rows deleted, not of keys read as the window arm's batch is: each kind's pass reads
// at most what the kinds before it left of the budget, so one tick deletes at most
// blockIndexRetireBatch rows and reads at most len(wholeTextOwners) times as many owner keys.
// It adds what it deleted to tick.Retired and leaves each kind's cursor in tick.Cursor.Retire.
// Every kind's cursor goes there before the first pass, so a kind the budget leaves nothing for,
// and a kind after one whose pass returned an error, keep the cursor they had. That matters
// because this walk reads every window owner of a kind, not only the owners that owe a delete:
// a cursor dropped back to the empty string costs the kind a whole lap over its owners, one
// slice every BlockIndexReconcileInterval for as long as the slices retire nothing.
func (s *Service) retireWholeText(ctx context.Context, cursors map[string]string, tick *BlockIndexTick) error {
	next := make(map[string]string, len(wholeTextOwners))
	for _, owner := range wholeTextOwners {
		next[owner.kind] = cursors[owner.kind]
	}
	tick.Cursor.Retire = next
	budget := blockIndexRetireBatch
	for _, owner := range wholeTextOwners {
		if budget <= 0 {
			break
		}
		retired, after, err := owner.pass(ctx, s.store.Pool, cursors[owner.kind], budget)
		if err != nil {
			return err
		}
		tick.Retired += retired
		budget -= retired
		next[owner.kind] = after
	}
	return nil
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

// reindexOwedText drains one row of the re-index queue (refs.RefsGrammarOwed), the text an older
// build indexed without the whole grammar, and reports whether it did: a comment, an ask or an
// issue message through the queue's own re-index (refs.ReindexOwed), a document by owing it a
// block index (drainOwedDocument), which is the whole of its re-index, and a `scope` row, which
// names a document whose question blocks moved, through a re-index of every source whose text
// belongs to that document (reindexScope). The last two name no text of their own, which is why
// refs.ReindexOwed answers an error for either rather than settling such a row. Each row is
// settled only while it still holds the enqueued_at it was listed with, so a write that lands
// meanwhile leaves it owed for the next tick, which reads its newer text.
func (s *Service) reindexOwedText(ctx context.Context, source refs.Owed) (bool, error) {
	switch source.Kind {
	case "comment", "ask", "message":
		return refs.ReindexOwed(ctx, s.store.Pool, source, s.serverURL)
	case "artifact":
		return s.drainOwedDocument(ctx, source)
	case "scope":
		if err := s.reindexScope(ctx, source.ID); err != nil {
			return false, err
		}
		return refs.SettleOwed(ctx, s.store.Pool, source)
	}
	return false, fmt.Errorf("re-index %s %s: not a kind the queue holds", source.Kind, source.ID)
}

// oweDocumentIndex owes a document a block index (document_block_versions.version null), which is
// what a document's re-index with the whole grammar is: the batch of owed documents indexes its
// latest version whole (indexOwedDocument, indexRenderedVersion), so its own mentions, the
// mentions its blocks hold and the excerpt each shows are all read with this build's grammar. The
// drain owes the work there rather than indexing the document itself because that batch is the one
// writer of a document's index and holds the rules a document needs - its keyset cursor, the owner
// row it indexes under, and the live text it must not date to the version that text is ahead of -
// and because a document an older build's version write left owed is indexed by that batch in this
// same tick.
//
// A document that is gone owes no index: the row is written from the artifacts table, so nothing
// is inserted for one. exec is the pool for a caller that owes the index on its own
// (reindexScope), and the transaction that settles a queue row for the drain
// (drainOwedDocument), which must owe the index and settle the row together or do neither.
func oweDocumentIndex(ctx context.Context, exec refs.Execer, artifactID string) error {
	if _, err := exec.Exec(ctx, `
		insert into document_block_versions (artifact_id) select id from artifacts where id = $1::uuid
		on conflict (artifact_id) do update set version = null
	`, artifactID); err != nil {
		return fmt.Errorf("owe document %s a block index: %w", artifactID, err)
	}
	return nil
}

// drainOwedDocument drains one `artifact` row of the re-index queue by owing its document a block
// index (oweDocumentIndex), which is the whole of that document's re-index, and reports whether it
// drained the row. The row is settled first and only while it holds the enqueued_at it was listed
// with, in the transaction that owes the index, so a write that lands meanwhile leaves the row
// owed and owes nothing. A document that is gone has its row settled all the same.
func (s *Service) drainOwedDocument(ctx context.Context, source refs.Owed) (bool, error) {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	settled, err := refs.SettleOwed(ctx, tx, source)
	if err != nil || !settled {
		return false, err
	}
	if err := oweDocumentIndex(ctx, tx, source.ID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// reindexScope re-indexes every source whose text belongs to document doc (refs.ScopeSources) and
// writes a `#id`: what a question block's settlement, or a version that moved the document's
// question blocks, changed for text written before it, since a `#id` written before its question
// settles resolves to nothing at its own write and to that ask once the ask exists. A source whose
// text holds no such candidate is read and left alone, which is most of a document's scope, and a
// source that fails leaves the row owed, so the next tick re-reads the whole scope; every write it
// makes is the same write a second pass would make.
func (s *Service) reindexScope(ctx context.Context, doc string) error {
	sources, err := refs.ScopeSources(ctx, s.store.Pool, doc)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if source.Kind != "artifact" {
			if _, err := refs.ReindexScopeSource(ctx, s.store.Pool, source.Kind, source.ID, s.serverURL); err != nil {
				return err
			}
			continue
		}
		// The document's own text, whose `#id`s name its own question blocks: owing it an index
		// costs a re-index of every block of its latest version, so its text is read first and
		// the document is left alone unless it writes a `#id` at all. A version write owes the
		// document's own row beside this scope's (migration 20261103205041), so what this covers
		// is a question that settles with no version written after it.
		body, _, found, err := refs.SourceText(ctx, s.store.Pool, source.Kind, source.ID)
		if err != nil {
			return err
		}
		if !found || !refs.HoldsQuestionCandidate(body, s.serverURL) {
			continue
		}
		if err := oweDocumentIndex(ctx, s.store.Pool, source.ID); err != nil {
			return err
		}
	}
	return nil
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
	if _, err := s.indexRenderedVersion(ctx, tx, document.id, version, versionAt, markdown, blocks, nil); err != nil {
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
