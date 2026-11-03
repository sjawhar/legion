package refs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// ItemKind is the kind of item whose blocks a call of this file writes: a comment, a message or
// an ask, whose blocks are the parts of its question, options and answer (PlanAskParts). It
// names the tables a call writes and is never caller input.
type ItemKind string

const (
	ItemComment ItemKind = "comment"
	ItemMessage ItemKind = "message"
	ItemAsk     ItemKind = "ask"
)

// itemTables is the storage of one item kind and its blocks: the items table, the blocks table,
// the column naming the item in every block table, the state table, the block mentions table and
// the table of each revision's block texts, which a kind with no revisions has none of. makers is
// whether a block of the kind records its own maker (made_by): an ask's answer part was made by
// whoever answered, where every block of a comment or a message was made by its author.
type itemTables struct {
	items, blocks, item, state, refs, texts string
	makers                                  bool
}

func (kind ItemKind) tables() (itemTables, error) {
	switch kind {
	case ItemComment:
		return itemTables{items: "comments", blocks: "comment_blocks", item: "comment_id", state: "comment_block_state", refs: "comment_block_refs", texts: "comment_block_texts"}, nil
	case ItemMessage:
		return itemTables{items: "messages", blocks: "message_blocks", item: "message_id", state: "message_block_state", refs: "message_block_refs"}, nil
	case ItemAsk:
		return itemTables{items: "asks", blocks: "ask_blocks", item: "ask_id", state: "ask_block_state", refs: "ask_block_refs", texts: "ask_block_texts", makers: true}, nil
	}
	return itemTables{}, fmt.Errorf("index the blocks of a %q: not an item kind", string(kind))
}

// revisions is whether the kind counts its indexed revisions and records the text its blocks held
// at each: a comment's and an ask's, never a message's, which is never edited.
func (tables itemTables) revisions() bool { return tables.texts != "" }

// BlockKind is the storage kind of a block of an item of this kind, in graph_edges and refs:
// comment_block, message_block or ask_block. The API names every block node kind block; the
// storage kind keeps a build that predates item blocks from counting or resolving their rows
// (TargetOf).
func (kind ItemKind) BlockKind() string {
	return string(kind) + "_block"
}

// ParseItemBlockNode reads an item block's node id: the kind of item it is a block of, that
// item's uuid and the block's id. That id is `<item kind>/<item uuid>#<block id>` -
// `comment/<uuid>#b2`, `message/<uuid>#b1`, `ask/<uuid>#b3` for a part of an ask - the one
// shape every block table's node_id column holds, which text.TargetID writes for a reference to
// one and the index writes into the rows. ok is false for any other text — a document block's
// node id, which is its document's ref_key and names no item kind (`KEY/slug#b2`), and a kind
// this build does not know among them.
func ParseItemBlockNode(nodeID string) (kind ItemKind, itemID, blockID string, ok bool) {
	item, blockID, found := strings.Cut(nodeID, "#")
	if !found {
		return "", "", "", false
	}
	name, itemID, found := strings.Cut(item, "/")
	if !found {
		return "", "", "", false
	}
	kind = ItemKind(name)
	if _, err := kind.tables(); err != nil {
		return "", "", "", false
	}
	return kind, itemID, blockID, true
}

// CarriedEdit is the block ids an edit of a comment carries (PATCH /api/v1/comments/{id}'s
// `blocks` and `base_revision`): the comment's revision the editor read its blocks at, and each
// carried block's id with its From in UTF-16 code units into the body the edit writes, in body
// order.
type CarriedEdit struct {
	BaseRevision int
	Blocks       []itemblocks.Carried
}

// ReplaceCountedItem indexes a comment's or an issue message's body, which the calling
// transaction has just inserted: its blocks and the mentions each holds (IndexItem), then the
// item's own references (replaceRefs), from one extraction of body. The counted targets it
// resolves are those either moved. A direct message, which writes no mention, calls IndexItem
// alone with no located mentions. A transaction that decides before it writes plans the same
// index and reconciliation instead (ReadItemIndex, PlanItemIndex, PlanReplace) and queues both
// in its write phase.
func ReplaceCountedItem(
	ctx context.Context,
	tx pgx.Tx,
	kind ItemKind,
	itemID, body, serverURL string,
	at time.Time,
) (model.ReferenceChanges, error) {
	located := text.ExtractAt(body, serverURL)
	moved, err := IndexItem(ctx, tx, kind, itemID, body, at, located, nil)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	changed, err := replaceRefs(ctx, tx, string(kind), itemID, text.Refs(located), nil)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return CountedChanges(ctx, tx, append(changed, moved...))
}

// IndexItem records body, written at `at` (the created_at of an insert, the edited_at of an
// edit), as the current revision of the blocks of comment or message itemID, in the caller's
// transaction. It is ReadItemIndex, planItemBlocks and Queue back to back, the queued statements
// run at once as a batch of their own (store.RunWrite); a transaction that decides everything
// before it writes calls the three apart and queues the writes in its write phase. An ask indexes
// its parts through the same core (PlanAskParts, ItemIndexing.Queue).
//
// The caller holds the item's owner - its advisory lock (store.LockOwner), which the
// reconciliation takes too (IndexOwedItemBlocks) - and the index's first queued statement takes
// the item's state row (inserting it when missing, then `for no key update`), so the index of one
// item is written by one transaction at a time. A direct message has no owner, and no site
// rewrites a message, so every writer of its index plans the same index from the same body.
//
// The body splits into blocks (itemblocks.Split) and each takes an id: when carried is given, its
// base revision is the comment's stored revision and the item is a comment, each block takes the
// id of the carried span starting in it (itemblocks.Carry, after itemblocks.ReadCarried refuses an
// id that is not a current block, an id named twice, or a From outside the body or out of order,
// with a *itemblocks.CarriedError naming the field); otherwise, and for a carried edit made
// against another revision, the ids follow the text (itemblocks.Align over every block the item
// ever held, removed ones included). A new block takes made_at = at, except at the first indexing
// of a comment already edited, whose blocks' making time was never recorded (null); a new, changed
// (identity hash) or restored block takes the new revision as changed_revision and `at` as
// changed_at; an unchanged one moves only its position, type and excerpt; a current block the body
// no longer holds is removed (position null, removed_at = at). A comment records the text of each
// new, changed or restored block, and null for each removed one, in comment_block_texts at the new
// revision.
//
// Revisions: a comment's revision counts its indexed revisions, 1 the first indexing. A call bumps
// it by one when it is the first indexing, when body differs byte for byte from the body last
// indexed (body_sha256), or when the blocks changed (a parser that reads the same body otherwise);
// a call that finds none of those writes no block and no text row and keeps the revision. A
// message has no revisions.
//
// Every call reconciles the item's block mentions from located (text.ExtractAt(body, …)): each
// mention to the block whose [From, To) holds its offset, one row per (block, target), so a
// reindex with a new grammar re-reads them even when the body is unchanged. A new row takes `at`,
// or at the item's first indexing its own refs row's created_at and source_seq for that target;
// Stamp gives the others their event. A direct message passes no located mentions and holds none.
// It stores the revision, the next block number and itemblocks.BodySHA256(body) and clears
// owed_at.
//
// It returns the targets of the block mentions it inserted or deleted, which the caller's event
// counts (ReplaceCountedItem, ItemIndexing.Moved).
func IndexItem(
	ctx context.Context,
	tx pgx.Tx,
	kind ItemKind,
	itemID, body string,
	at time.Time,
	located []text.Located,
	carried *CarriedEdit,
) ([]Target, error) {
	index, err := ReadItemIndex(ctx, tx, kind, itemID)
	if err != nil {
		return nil, err
	}
	indexing, err := planItemBlocks(index, kind, itemID, at, itemBodySource(kind, body, located, carried))
	if err != nil {
		return nil, err
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		indexing.Queue(b)
		return nil
	}); err != nil {
		return nil, err
	}
	return indexing.Moved(), nil
}

// itemBlockSource is one reading of an item's blocks, which the index writes: the blocks in
// order, the sum of the text they were read from (the state row's body_sha256), how each takes
// its id against the blocks the item holds, the maker each records (made_by, for a kind whose
// blocks have one: nil, or an entry per block, null where the item's own author made it), and the
// links the blocks hold once their ids are known.
type itemBlockSource struct {
	blocks  []itemblocks.Block
	bodySum string
	ids     func(stored []itemblocks.Stored, next []itemblocks.Block, revision, counter int) ([]string, int, error)
	madeBy  []*string
	pairs   func(next []itemblocks.Block, ids []string) []blockPair
}

// itemBodySource is the reading of an item's blocks that a body makes, which IndexItem writes and
// PlanItemIndex plans: the blocks body splits into, the sum its state row records, the ids the
// carried spans or the text give them, and the links the mentions of located write. A comment's
// and a message's blocks record no maker of their own, so the source names none.
func itemBodySource(kind ItemKind, body string, located []text.Located, carried *CarriedEdit) itemBlockSource {
	return itemBlockSource{
		blocks:  itemblocks.Split(body),
		bodySum: itemblocks.BodySHA256(body),
		ids: func(stored []itemblocks.Stored, next []itemblocks.Block, revision, counter int) ([]string, int, error) {
			if carried != nil && kind == ItemComment && carried.BaseRevision == revision {
				spans, err := itemblocks.ReadCarried(stored, body, carried.Blocks)
				if err != nil {
					return nil, 0, err
				}
				ids, counter := itemblocks.Carry(stored, next, spans, counter)
				return ids, counter, nil
			}
			ids, counter := itemblocks.Align(stored, next, counter)
			return ids, counter, nil
		},
		pairs: func(next []itemblocks.Block, ids []string) []blockPair {
			return itemBlockPairs(next, ids, located)
		},
	}
}

// ItemIndex is what a comment's, a message's or an ask's block index holds: its state row, every
// block the item ever held with the stored excerpt of each current one, and the block mentions
// those blocks hold.
type ItemIndex struct {
	state    itemState
	stored   []itemblocks.Stored
	excerpts map[string]string
	held     []itemBlockLink
}

// itemBlockLink is one stored block mention of an item: the block holding it, the edge kind and
// the target, the columns the reconciliation matches a link on.
type itemBlockLink struct {
	blockID string
	kind    string
	target  Target
}

// ReadItemIndex reads what q holds of comment, message or ask itemID's block index: its state
// row, the blocks it holds and their mentions, the mentions read with the predicate the
// reconciling delete visits them by. An item no block-aware build indexed holds no block and no
// block mention - the index writes them in the transaction that marks the state indexed, and a
// state row goes only with its item - and so does one whose row q does not hold yet, the item
// whose insert the calling transaction has queued.
func ReadItemIndex(ctx context.Context, q Queryer, kind ItemKind, itemID string) (ItemIndex, error) {
	tables, err := kind.tables()
	if err != nil {
		return ItemIndex{}, err
	}
	state, err := readItemState(ctx, q, kind, tables, itemID)
	if err != nil {
		return ItemIndex{}, err
	}
	index := ItemIndex{state: state, stored: []itemblocks.Stored{}}
	if !state.indexed {
		return index, nil
	}
	if index.stored, index.excerpts, err = storedItemBlocks(ctx, q, kind, tables, itemID); err != nil {
		return ItemIndex{}, err
	}
	if index.held, err = heldItemBlockLinks(ctx, q, kind, tables, itemID); err != nil {
		return ItemIndex{}, err
	}
	return index, nil
}

// Edited is the index with the item marked edited, which an edit's own read of the item does not
// show when the update that sets edited_at is queued and has not run. It matters at a first
// indexing alone: the blocks of a comment or an ask edited before this write were made at a time
// the item never recorded, so they record none (PlanItemIndex, madeAt).
func (i ItemIndex) Edited() ItemIndex {
	i.state.edited = true
	return i
}

// ItemIndexing is one body's index of an item's blocks and their mentions (IndexItem), planned
// against what the index holds (PlanItemIndex), so the targets it moves are known before anything
// is written, and written by Queue, which puts its statements on a write phase's batch and does
// nothing else.
type ItemIndexing struct {
	kind   ItemKind
	tables itemTables
	itemID string
	at     time.Time
	// madeAt is what a new block records as its making time: `at`, or null at the first indexing
	// of a comment or an ask already edited, whose blocks' making time was never recorded.
	madeAt              *time.Time
	revision, nextBlock int
	bodySHA256          string
	// changed reports that this body's blocks differ from the indexed ones, so the index takes a
	// new revision and writes them; an index that finds them unchanged writes no block row.
	changed       bool
	written, kept []itemBlockRow
	removed       []string
	pairs         []blockPair
	firstIndexing bool
	moved         []Target
	// current is the excerpt of each block the body holds, by block id: what a reply naming one
	// of them reads of the index before it is written (Block).
	current map[string]string
	// index is what the item's block index held when this one was planned against it, which Held
	// carries forward into what it leaves.
	index ItemIndex
}

// PlanItemIndex plans the index of body, written at `at`, of comment or message itemID against
// index, what its block index holds (ReadItemIndex): the blocks, their ids, the links their
// mentions write and the targets they move. It reads nothing. A carried id it refuses returns a
// *itemblocks.CarriedError. An ask's parts are planned from their own reading of its blocks
// through the same core (PlanAskParts, planItemBlocks).
func PlanItemIndex(
	index ItemIndex,
	kind ItemKind,
	itemID, body string,
	at time.Time,
	located []text.Located,
	carried *CarriedEdit,
) (ItemIndexing, error) {
	return planItemBlocks(index, kind, itemID, at, itemBodySource(kind, body, located, carried))
}

// planItemBlocks plans the index of source, read at `at`, of item itemID against index, what its
// block index holds: the blocks, their ids, the maker each records, the links their mentions
// write and the targets they move. It reads nothing and writes nothing; what it plans is written
// by Queue.
func planItemBlocks(
	index ItemIndex,
	kind ItemKind,
	itemID string,
	at time.Time,
	source itemBlockSource,
) (ItemIndexing, error) {
	tables, err := kind.tables()
	if err != nil {
		return ItemIndexing{}, err
	}
	state, stored := index.state, index.stored
	indexing := ItemIndexing{
		kind: kind, tables: tables, itemID: itemID, at: at,
		bodySHA256: source.bodySum, firstIndexing: !state.indexed, index: index,
	}
	counter := state.nextBlock
	for _, block := range stored {
		if number, ok := blockNumber(block.ID); ok && number >= counter {
			counter = number + 1
		}
	}

	next := source.blocks
	ids, counter, err := source.ids(stored, next, state.revision, counter)
	if err != nil {
		return ItemIndexing{}, err
	}

	byID := make(map[string]itemblocks.Stored, len(stored))
	for _, block := range stored {
		byID[block.ID] = block
	}
	indexing.changed = !state.indexed || state.bodySHA256 != indexing.bodySHA256
	indexing.current = make(map[string]string, len(next))
	for position, block := range next {
		row := itemBlockRow{
			id: ids[position], position: position, nodeType: block.Type, sum: block.Hash,
			excerpt: text.HeadRunes(block.Text, BlockExcerptRunes), text: block.Text,
		}
		if position < len(source.madeBy) {
			row.madeBy = source.madeBy[position]
		}
		indexing.current[row.id] = row.excerpt
		previous, held := byID[row.id]
		delete(byID, row.id)
		if held && previous.Position >= 0 && previous.Hash == row.sum {
			indexing.kept = append(indexing.kept, row)
			if previous.Position != position || previous.Type != block.Type {
				indexing.changed = true
			}
			continue
		}
		indexing.written = append(indexing.written, row)
		indexing.changed = true
	}
	for _, block := range stored {
		if _, untaken := byID[block.ID]; untaken && block.Position >= 0 {
			indexing.removed = append(indexing.removed, block.ID)
			indexing.changed = true
		}
	}

	indexing.revision, indexing.nextBlock = state.revision, counter
	if indexing.changed {
		indexing.revision++
		indexing.madeAt = &at
		if !state.indexed && tables.revisions() && state.edited {
			indexing.madeAt = nil
		}
	}

	// The links the body drops, in the order the index listed them, then those it adds, in the
	// order the insert writes them: the targets the delete and the insert move.
	indexing.pairs = source.pairs(next, ids)
	// An item's links carry no block types, so a pair is its key (blockPairKey) and no link is
	// kept while its types change, as a document's typed block attribute can be.
	written := make(map[blockPairKey]struct{}, len(indexing.pairs))
	for _, pair := range indexing.pairs {
		written[pair.key()] = struct{}{}
	}
	stillHeld := make(map[blockPairKey]struct{}, len(index.held))
	for _, link := range index.held {
		pair := blockPairKey{blockID: link.blockID, kind: link.kind, target: link.target}
		stillHeld[pair] = struct{}{}
		if _, still := written[pair]; !still {
			indexing.moved = append(indexing.moved, pair.target)
		}
	}
	for _, pair := range indexing.pairs {
		if _, already := stillHeld[pair.key()]; !already {
			indexing.moved = append(indexing.moved, pair.target)
		}
	}
	// A link a block keeps shows that block's excerpt in its target's referenced-by panel, so a
	// block that keeps its id while its excerpt changes names the targets of the links it holds,
	// as a document's reworded block does; a change past the excerpt's runes names none.
	newExcerpts := make(map[string]string, len(indexing.written))
	for _, row := range indexing.written {
		newExcerpts[row.id] = row.excerpt
	}
	for _, pair := range indexing.pairs {
		if previous, held := index.excerpts[pair.blockID]; held {
			if excerpt, rewritten := newExcerpts[pair.blockID]; rewritten && excerpt != previous {
				indexing.moved = append(indexing.moved, pair.target)
			}
		}
	}
	return indexing, nil
}

// Moved is the targets of the block mentions the index inserts or deletes, and of those it keeps
// in a block whose excerpt it changes.
func (i ItemIndexing) Moved() []Target {
	return i.moved
}

// Held is what the item's block index holds once this index is written, which a second index of
// the same item in one transaction plans against, so it sees the rows the first one queued
// (PlanAskParts). It reads the plan alone: the state this index records, the blocks the source
// holds in its order followed by the removed ones, the ones this index removes first, as a read
// after the write orders them (storedItemBlocks), the excerpt of each block the source holds,
// and the links its blocks hold once the reconciliation has run.
func (i ItemIndexing) Held() ItemIndex {
	held := ItemIndex{
		state: itemState{
			indexed: true, revision: i.revision, nextBlock: i.nextBlock,
			bodySHA256: i.bodySHA256, edited: i.index.state.edited,
		},
		stored:   make([]itemblocks.Stored, 0, len(i.index.stored)+len(i.written)),
		excerpts: i.current,
		held:     make([]itemBlockLink, 0, len(i.pairs)),
	}
	for _, rows := range [][]itemBlockRow{i.written, i.kept} {
		for _, row := range rows {
			held.stored = append(held.stored, itemblocks.Stored{
				ID: row.id, Type: row.nodeType, Hash: row.sum, Position: row.position,
			})
		}
	}
	slices.SortFunc(held.stored, func(first, second itemblocks.Stored) int {
		return first.Position - second.Position
	})
	// A block this index removes loses its position at a removed_at later than every earlier
	// removal's, so a read after the write puts it before them, and among themselves by id, the
	// order storedItemBlocks takes. Each keeps the type and the text it was written with.
	var earlier []itemblocks.Stored
	removing := len(held.stored)
	for _, block := range i.index.stored {
		if _, holds := i.current[block.ID]; holds {
			continue
		}
		removed := block.Position < 0
		block.Position = -1
		if removed {
			earlier = append(earlier, block)
			continue
		}
		held.stored = append(held.stored, block)
	}
	slices.SortFunc(held.stored[removing:], func(first, second itemblocks.Stored) int {
		return cmp.Or(cmp.Compare(len(first.ID), len(second.ID)), cmp.Compare(first.ID, second.ID))
	})
	held.stored = append(held.stored, earlier...)
	for _, pair := range i.pairs {
		held.held = append(held.held, itemBlockLink{blockID: pair.blockID, kind: pair.kind, target: pair.target})
	}
	return held
}

// Block is the node id and excerpt of block blockID as the index leaves it, the row its insert
// writes (queueBlocks): what a reply naming a block of an item this transaction is indexing
// reads in place of the stored block (CurrentItemBlock). found is false when the body holds no
// such block.
func (i ItemIndexing) Block(blockID string) (nodeID, excerpt string, found bool) {
	excerpt, found = i.current[blockID]
	if !found {
		return "", "", false
	}
	return string(i.kind) + "/" + i.itemID + "#" + blockID, excerpt, true
}

// Queue queues the index's writes on a write phase's batch: the item's state row taken first, the
// blocks the body changed and removed, the block texts of a kind that records them, the links its
// blocks hold, and the state the index leaves.
func (i ItemIndexing) Queue(b *store.WriteBatch) {
	i.queueStateLock(b)
	if i.changed {
		i.queueBlocks(b)
	}
	i.queueBlockRefs(b)
	i.queueState(b)
}

// itemState is an item's state row as the index found it: whether a block-aware build indexed it
// before (a revision above 0 where the kind counts revisions, a message's body_sha256 set), its
// revision and next block number (a message's are 0 and 1), the sum of the text it was last
// indexed from, and whether the item was edited (edited_at set, which a message has not).
type itemState struct {
	indexed    bool
	revision   int
	nextBlock  int
	bodySHA256 string
	edited     bool
}

// readItemState is the item's state row as the index finds it, read under no lock: the index's
// write phase takes the row (queueStateLock) and the owner's lock the caller holds, the
// reconciliation included, excludes every other writer of the item until then (IndexItem). An
// item with no state row, one no block-aware build indexed, and one whose insert the calling
// transaction has queued, read as never indexed.
func readItemState(ctx context.Context, q Queryer, kind ItemKind, tables itemTables, itemID string) (itemState, error) {
	state := itemState{nextBlock: 1}
	var sum *string
	var err error
	if tables.revisions() {
		err = q.QueryRow(ctx, `
			select coalesce(s.revision, 0), coalesce(s.next_block, 1), s.body_sha256, i.edited_at is not null
			from `+tables.items+` i left join `+tables.state+` s on s.`+tables.item+` = i.id
			where i.id = $1::uuid
		`, itemID).Scan(&state.revision, &state.nextBlock, &sum, &state.edited)
		state.indexed = state.revision > 0
	} else {
		err = q.QueryRow(ctx, `
			select body_sha256 from `+tables.state+` where `+tables.item+` = $1::uuid
		`, itemID).Scan(&sum)
		state.indexed = sum != nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return itemState{nextBlock: 1}, nil
	}
	if err != nil {
		return itemState{}, fmt.Errorf("read the block state of %s %s: %w", kind, itemID, err)
	}
	if sum != nil {
		state.bodySHA256 = *sum
	}
	return state, nil
}

// heldItemBlockLinks is the block mentions the item's blocks hold, read with the predicate the
// reconciling delete visits them by, so the index knows what it moves before it writes.
func heldItemBlockLinks(ctx context.Context, q Queryer, kind ItemKind, tables itemTables, itemID string) ([]itemBlockLink, error) {
	rows, err := q.Query(ctx, `
		select block_id, kind, to_kind, to_id from `+tables.refs+` where `+tables.item+` = $1::uuid
	`, itemID)
	if err != nil {
		return nil, fmt.Errorf("read the block mentions of %s %s: %w", kind, itemID, err)
	}
	defer rows.Close()
	held := []itemBlockLink{}
	for rows.Next() {
		var link itemBlockLink
		if err := rows.Scan(&link.blockID, &link.kind, &link.target.Kind, &link.target.ID); err != nil {
			return nil, fmt.Errorf("scan a block mention of %s %s: %w", kind, itemID, err)
		}
		held = append(held, link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the block mentions of %s %s: %w", kind, itemID, err)
	}
	return held, nil
}

// queueStateLock queues the item's state row, the first statements of the index's write phase:
// its insert when it has none, then the row `for no key update`, which the reconciliation takes
// as well (IndexOwedItemBlocks), so the index of one item is written by one transaction at a
// time. Its row is not read back: the index was planned from the state the read phase found.
func (i ItemIndexing) queueStateLock(b *store.WriteBatch) {
	b.Queue(fmt.Sprintf("create the block state of %s %s", i.kind, i.itemID), `
		insert into `+i.tables.state+` (`+i.tables.item+`) values ($1::uuid) on conflict do nothing
	`, i.itemID)
	b.Queue(fmt.Sprintf("lock the block state of %s %s", i.kind, i.itemID), `
		select 1 from `+i.tables.state+` where `+i.tables.item+` = $1::uuid for no key update
	`, i.itemID)
}

// storedItemBlocks is every block the item ever held, current ones first in body order, then
// removed ones, the most recently removed first, so Align's first untaken match prefers the block
// in place and, among removed blocks of equal text, the last removed.
func storedItemBlocks(ctx context.Context, q Queryer, kind ItemKind, tables itemTables, itemID string) ([]itemblocks.Stored, map[string]string, error) {
	rows, err := q.Query(ctx, `
		select block_id, type, text_sha256, coalesce(position, -1), excerpt from `+tables.blocks+`
		where `+tables.item+` = $1::uuid
		order by position nulls last, removed_at desc, length(block_id), block_id
	`, itemID)
	if err != nil {
		return nil, nil, fmt.Errorf("read the blocks of %s %s: %w", kind, itemID, err)
	}
	defer rows.Close()
	stored := []itemblocks.Stored{}
	excerpts := map[string]string{}
	for rows.Next() {
		var block itemblocks.Stored
		var excerpt string
		if err := rows.Scan(&block.ID, &block.Type, &block.Hash, &block.Position, &excerpt); err != nil {
			return nil, nil, fmt.Errorf("scan a block of %s %s: %w", kind, itemID, err)
		}
		stored = append(stored, block)
		if block.Position >= 0 {
			excerpts[block.ID] = excerpt
		}
	}
	return stored, excerpts, rows.Err()
}

// blockNumber is the number of block id b<N>.
func blockNumber(id string) (int, bool) {
	digits, ok := strings.CutPrefix(id, "b")
	if !ok {
		return 0, false
	}
	number, err := strconv.Atoi(digits)
	return number, err == nil
}

// itemBlockRow is one current block the index writes: its id, position, type, identity hash,
// excerpt, text and, for a kind whose blocks record one, its maker as the json its made_by column
// holds (nil: the item's own author made it).
type itemBlockRow struct {
	id       string
	position int
	nodeType string
	sum      string
	excerpt  string
	text     string
	madeBy   *string
}

// queueBlocks queues one new revision of the item's blocks: written (new, changed or restored
// blocks) take the revision and `at` as their changed facts, and a new one madeAt; kept blocks
// move their position, type and excerpt; removed ones lose their position at `at`. A kind with
// revisions records each written block's text, and null for each removed one, at the revision. A
// kind whose blocks record a maker writes each written block's; a kept block keeps the maker it
// was written with, as it keeps its text.
func (i ItemIndexing) queueBlocks(b *store.WriteBatch) {
	kind, tables, itemID, revision := i.kind, i.tables, i.itemID, i.revision
	if len(i.removed) > 0 {
		b.Queue(fmt.Sprintf("remove the blocks %s %s no longer holds", kind, itemID), `
			update `+tables.blocks+` set position = null, removed_at = $2
			where `+tables.item+` = $1::uuid and block_id = any($3)
		`, itemID, i.at, i.removed)
	}
	if len(i.written) > 0 {
		ids, positions, types, sums, excerpts, makers := itemBlockColumns(i.written)
		what := fmt.Sprintf("record the blocks of %s %s", kind, itemID)
		if tables.revisions() {
			// Only a kind whose blocks record a maker has the column, so the three fragments are
			// empty for the others; kind is this package's own constant, never caller input.
			maker, makerValue, makerUpdate := "", "", ""
			if tables.makers {
				maker, makerValue, makerUpdate = ", made_by", ", b.made_by::jsonb", ", made_by = excluded.made_by"
			}
			b.Queue(what, `
				insert into `+tables.blocks+` (`+tables.item+`, block_id, node_id, position, type, text_sha256, excerpt, made_at, changed_revision, changed_at`+maker+`)
				select $1::uuid, b.id, '`+string(kind)+`/' || $1::uuid::text || '#' || b.id, b.position, b.type, b.sum, b.excerpt, $2, $3, $4`+makerValue+`
				from unnest($5::text[], $6::int[], $7::text[], $8::text[], $9::text[], $10::text[]) as b (id, position, type, sum, excerpt, made_by)
				on conflict (`+tables.item+`, block_id) do update set
					position = excluded.position,
					type = excluded.type,
					text_sha256 = excluded.text_sha256,
					excerpt = excluded.excerpt,
					changed_revision = excluded.changed_revision,
					changed_at = excluded.changed_at,
					removed_at = null`+makerUpdate+`
			`, itemID, i.madeAt, revision, i.at, ids, positions, types, sums, excerpts, makers)
		} else {
			// A message is never edited, so its blocks are made and last changed when it was
			// written; a reindex under another parser moves only those of new text.
			b.Queue(what, `
				insert into message_blocks (message_id, block_id, node_id, position, type, text_sha256, excerpt, made_at, changed_at)
				select $1::uuid, b.id, 'message/' || $1::uuid::text || '#' || b.id, b.position, b.type, b.sum, b.excerpt, $2, $2
				from unnest($3::text[], $4::int[], $5::text[], $6::text[], $7::text[]) as b (id, position, type, sum, excerpt)
				on conflict (message_id, block_id) do update set
					position = excluded.position,
					type = excluded.type,
					text_sha256 = excluded.text_sha256,
					excerpt = excluded.excerpt,
					changed_at = excluded.changed_at,
					removed_at = null
			`, itemID, i.at, ids, positions, types, sums, excerpts)
		}
	}
	if len(i.kept) > 0 {
		ids, positions, types, _, excerpts, _ := itemBlockColumns(i.kept)
		b.Queue(fmt.Sprintf("move the unchanged blocks of %s %s", kind, itemID), `
			update `+tables.blocks+` b set position = k.position, type = k.type, excerpt = k.excerpt
			from unnest($2::text[], $3::int[], $4::text[], $5::text[]) as k (id, position, type, excerpt)
			where b.`+tables.item+` = $1::uuid and b.block_id = k.id
		`, itemID, ids, positions, types, excerpts)
	}
	if !tables.revisions() || len(i.written)+len(i.removed) == 0 {
		return
	}
	textIDs := make([]string, 0, len(i.written)+len(i.removed))
	texts := make([]*string, 0, len(i.written)+len(i.removed))
	for index := range i.written {
		textIDs, texts = append(textIDs, i.written[index].id), append(texts, &i.written[index].text)
	}
	for _, id := range i.removed {
		textIDs, texts = append(textIDs, id), append(texts, nil)
	}
	b.Queue(fmt.Sprintf("record the block texts of revision %d of %s %s", revision, kind, itemID), `
		insert into `+tables.texts+` (`+tables.item+`, block_id, revision, text)
		select $1::uuid, t.id, $2, t.text from unnest($3::text[], $4::text[]) as t (id, text)
		on conflict (`+tables.item+`, block_id, revision) do update set text = excluded.text
	`, itemID, revision, textIDs, texts)
}

// itemBlockColumns is rows as the parallel arrays an unnest reads.
func itemBlockColumns(rows []itemBlockRow) (ids []string, positions []int32, types, sums, excerpts []string, makers []*string) {
	for _, row := range rows {
		ids = append(ids, row.id)
		positions = append(positions, int32(row.position))
		types = append(types, row.nodeType)
		sums = append(sums, row.sum)
		excerpts = append(excerpts, row.excerpt)
		makers = append(makers, row.madeBy)
	}
	return ids, positions, types, sums, excerpts, makers
}

// itemBlockPairs is each distinct link the mentions in located write from the block of next
// (whose ids are ids) whose [From, To) holds the mention's start. A mention no block holds, and
// an external URL, write none. next's blocks are top-level, apart and in body order, and located
// is in offset order, so blockPairs' innermost holder is the one block holding each start.
func itemBlockPairs(next []itemblocks.Block, ids []string, located []text.Located) []blockPair {
	nodes := make([]model.ArtifactBlock, len(next))
	for index, block := range next {
		nodes[index] = model.ArtifactBlock{ID: ids[index], From: block.From, To: block.To}
	}
	return blockPairs(nodes, located, nil)
}

// queueBlockRefs queues the reconciliation of the item's block mentions with the links its blocks
// hold, matching on the columns of their key: the delete of the links the body dropped and the
// insert of the ones it holds. A link that survives keeps its created_at and source_seq; a new
// one takes `at`, or at the item's first indexing the item's own refs row for that target's
// created_at and source_seq, when one exists. At the first indexing the item holds no block
// mention, so nothing is deleted. The targets the two statements move are already known
// (PlanItemIndex), so neither returns its rows.
func (i ItemIndexing) queueBlockRefs(b *store.WriteBatch) {
	kind, tables, itemID, pairs := i.kind, i.tables, i.itemID, i.pairs
	blockIDs := make([]string, len(pairs))
	kinds := make([]string, len(pairs))
	toKinds := make([]string, len(pairs))
	toIDs := make([]string, len(pairs))
	for index, pair := range pairs {
		blockIDs[index], kinds[index], toKinds[index], toIDs[index] = pair.blockID, pair.kind, pair.target.Kind, pair.target.ID
	}
	if !i.firstIndexing {
		b.Queue(fmt.Sprintf("delete the block mentions %s %s dropped", kind, itemID), `
			delete from `+tables.refs+` r
			where r.`+tables.item+` = $1::uuid and not exists (
				select 1 from unnest($2::text[], $3::text[], $4::text[], $5::text[]) as p (block_id, kind, to_kind, to_id)
				where p.block_id = r.block_id and p.kind = r.kind and p.to_kind = r.to_kind and p.to_id = r.to_id)
		`, itemID, blockIDs, kinds, toKinds, toIDs)
	}
	if len(pairs) == 0 {
		return
	}
	b.Queue(fmt.Sprintf("record the block mentions of %s %s", kind, itemID), `
		insert into `+tables.refs+` (`+tables.item+`, block_id, from_node, to_kind, to_id, kind, created_at, source_seq)
		select $1::uuid, p.block_id, $6::text || '/' || $1::uuid::text || '#' || p.block_id, p.to_kind, p.to_id, p.kind,
		       coalesce(case when $8::boolean then r.created_at end, $7::timestamptz), case when $8::boolean then r.source_seq end
		from unnest($2::text[], $3::text[], $4::text[], $5::text[]) as p (block_id, kind, to_kind, to_id)
		left join refs r on r.from_kind = $6 and r.from_id = $1::uuid::text
		                and r.to_kind = p.to_kind and r.to_id = p.to_id
		on conflict (`+tables.item+`, block_id, to_kind, to_id, kind) do nothing
	`, itemID, blockIDs, kinds, toKinds, toIDs, string(kind), i.at, i.firstIndexing)
}

// queueState records the item indexed from the text whose sum the index took, at its revision
// with the next block number for a kind that counts revisions, and settles its owed_at.
func (i ItemIndexing) queueState(b *store.WriteBatch) {
	what := fmt.Sprintf("record the blocks of %s %s indexed", i.kind, i.itemID)
	if i.tables.revisions() {
		b.Queue(what, `
			update `+i.tables.state+` set revision = $2, next_block = $3, body_sha256 = $4, owed_at = null
			where `+i.tables.item+` = $1::uuid
		`, i.itemID, i.revision, i.nextBlock, i.bodySHA256)
		return
	}
	b.Queue(what, `
		update `+i.tables.state+` set body_sha256 = $2, owed_at = null where `+i.tables.item+` = $1::uuid
	`, i.itemID, i.bodySHA256)
}

// OwedItemBlock is one comment, message or ask owed a block index: its state row's owed_at is
// set, by the trigger on a write no block-aware build indexed (a build without the index wrote
// it, during the roll or after a rollback), or by rebuild-refs.
type OwedItemBlock struct {
	Kind ItemKind
	ID   string
}

// OwedItemBlocks lists at most limit comments, messages and asks owed a block index, oldest
// owed_at first, and locks nothing. Each table's read goes through its partial index on owed_at.
func OwedItemBlocks(ctx context.Context, q Queryer, limit int) ([]OwedItemBlock, error) {
	rows, err := q.Query(ctx, `
		select kind, id from (
			(select 'comment' as kind, comment_id::text as id, owed_at from comment_block_state
			 where owed_at is not null order by owed_at limit $1)
			union all
			(select 'message', message_id::text, owed_at from message_block_state
			 where owed_at is not null order by owed_at limit $1)
			union all
			(select 'ask', ask_id::text, owed_at from ask_block_state
			 where owed_at is not null order by owed_at limit $1)
		) owed
		order by owed_at, kind, id limit $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list the items owed a block index of their blocks: %w", err)
	}
	owed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OwedItemBlock, error) {
		var item OwedItemBlock
		err := row.Scan(&item.Kind, &item.ID)
		return item, err
	})
	if err != nil {
		return nil, fmt.Errorf("list the items owed a block index of their blocks: %w", err)
	}
	return owed, nil
}

// IndexOwedItemBlocks indexes one owed comment, message or ask in a transaction of its own and
// reports whether it indexed it. It takes the item's owner lock (readSourceUnderOwner), the lock
// every writer of the item takes, and reads the item in the statement after it, so a write
// committed while it waited is seen; a direct message, whose conversation is no owner, has no
// such lock and needs none, since nothing ever rewrites a message's body. It takes no item row: a
// row lock would assign the transaction a transaction id to hold through everything it reads and
// plans, and under the owner lock it excludes nothing the owner lock does not. Reading and
// planning come first and its writes follow back to back (IndexItemIfOwed). A new block and a new
// mention take the time the item's text was written: a comment's and an ask's
// coalesce(edited_at, created_at), a message's created_at. An item a write site indexed since it
// was listed is no longer owed and is left alone; one deleted since, before the lock or while it
// waited, took its state row with it (on delete cascade), so nothing is owed.
//
// It runs from the block-index reconciliation (docs.(*Service).ReconcileBlockIndex) and indexes
// through IndexItemIfOwed, which indexes nothing unless the item's index is owed, so every
// mention it records is of text an earlier write already appended an event for. A block mention
// takes the source_seq of the item's own refs row for its target at the item's first indexing
// (PlanItemIndex) and none otherwise, and no ?since= read lists a row with none until a later
// write of the item stamps it (Stamp).
func IndexOwedItemBlocks(ctx context.Context, pool Pool, item OwedItemBlock, serverURL string) (bool, error) {
	tables, err := item.Kind.tables()
	if err != nil {
		return false, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	read := `select true from ` + tables.items + ` where id = $1::uuid`
	var present bool
	found, err := readSourceUnderOwner(ctx, tx, string(item.Kind), item.ID, read, &present)
	if err != nil || !found {
		return false, err
	}
	indexed, err := IndexItemIfOwed(ctx, tx, item.Kind, item.ID, serverURL)
	if err != nil {
		return false, err
	}
	return indexed, tx.Commit(ctx)
}
