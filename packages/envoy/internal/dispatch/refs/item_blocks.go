package refs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// ItemKind is the kind of item whose blocks a call of this file writes: a comment, a message or
// an ask, whose blocks are the parts of its question, options and answer (IndexAskParts). It
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
// alone with no located mentions.
func ReplaceCountedItem(
	ctx context.Context,
	tx pgx.Tx,
	kind ItemKind,
	itemID, body, serverURL string,
	at time.Time,
) (model.ReferenceChanges, error) {
	located := text.ExtractAt(body, serverURL)
	changed, err := replaceItemRefs(ctx, tx, kind, itemID, body, at, located, nil)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return CountedChanges(ctx, tx, changed)
}

// ReplaceCountedItemEdit is ReplaceCountedItem for an edit of a comment from previous to body,
// written at `at`, with the block ids the editor carried (carried; nil: the ids follow the text,
// IndexItem). The counted targets it resolves are every kind the edit moves: the targets whose
// edges or block mentions it inserted or deleted - so a link that moved from one block of the
// comment to another is named as well as one the comment gained or lost - and, when the edit
// moves the head of the comment's text, every target the comment still cites
// (editedHeadTargets). A carried id IndexItem refuses returns its *itemblocks.CarriedError.
func ReplaceCountedItemEdit(
	ctx context.Context,
	tx pgx.Tx,
	kind ItemKind,
	itemID, previous, body, serverURL string,
	at time.Time,
	carried *CarriedEdit,
) (model.ReferenceChanges, error) {
	located := text.ExtractAt(body, serverURL)
	changed, err := replaceItemRefs(ctx, tx, kind, itemID, body, at, located, carried)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return CountedChanges(ctx, tx, append(changed, editedHeadTargets(previous, body, text.Refs(located))...))
}

// replaceItemRefs indexes the item's blocks and their mentions (IndexItem) and reconciles its own
// references (replaceRefs) to located, the mentions of body, and returns the targets either
// inserted or deleted.
func replaceItemRefs(
	ctx context.Context,
	tx pgx.Tx,
	kind ItemKind,
	itemID, body string,
	at time.Time,
	located []text.Located,
	carried *CarriedEdit,
) ([]Target, error) {
	moved, err := IndexItem(ctx, tx, kind, itemID, body, at, located, carried)
	if err != nil {
		return nil, err
	}
	changed, err := replaceRefs(ctx, tx, string(kind), itemID, text.Refs(located), nil)
	if err != nil {
		return nil, err
	}
	return append(changed, moved...), nil
}

// IndexItem records body, written at `at` (the created_at of an insert, the edited_at of an
// edit), as the current revision of the blocks of comment or message itemID, in the caller's
// transaction, through indexItemBlocks: the body splits into blocks (itemblocks.Split) and each
// takes an id from the carried ids or from the text, the mentions of located are paired to the
// block whose [From, To) holds each one, and the state records
// itemblocks.BodySHA256(body). An ask indexes its parts through the same core (IndexAskParts).
//
// When carried is given, its base revision is the comment's stored revision and the item is a
// comment, each block takes the id of the carried span starting in it (itemblocks.Carry, after
// itemblocks.ReadCarried refuses an id that is not a current block, an id named twice, or a From
// outside the body or out of order, with a *itemblocks.CarriedError naming the field); otherwise,
// and for a carried edit made against another revision, the ids follow the text
// (itemblocks.Align).
//
// It returns the targets of the block mentions it inserted or deleted, which the caller's event
// counts (ReplaceCountedItem, ReplaceCountedItemEdit).
func IndexItem(
	ctx context.Context,
	tx pgx.Tx,
	kind ItemKind,
	itemID, body string,
	at time.Time,
	located []text.Located,
	carried *CarriedEdit,
) ([]Target, error) {
	return indexItemBlocks(ctx, tx, kind, itemID, at, itemBlockSource{
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
	})
}

// itemBlockSource is one reading of an item's blocks, which indexItemBlocks writes: the blocks in
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

// indexItemBlocks records source as the current revision of the blocks of item itemID, written at
// `at`, in the caller's transaction. The caller holds the item's owner row and the item row (its
// insert or update, or a read `for no key update`); indexItemBlocks takes the item's state row
// after them (inserting it when missing, then `for no key update`), so every writer takes owner,
// item, state in that order and the reconciliation, which takes no owner, takes item then state.
//
// A new block takes made_at = at, except at the first indexing of a comment or an ask already
// edited, whose blocks' making time was never recorded (null); a new, changed (identity hash) or
// restored block takes the new revision as changed_revision and `at` as changed_at; an unchanged
// one moves only its position, type and excerpt; a current block the source no longer holds is
// removed (position null, removed_at = at). A kind with revisions records the text of each new,
// changed or restored block, and null for each removed one, in its block texts table at the new
// revision.
//
// Revisions: a comment's and an ask's revision counts its indexed revisions, 1 the first
// indexing. A call bumps it by one when it is the first indexing, when the source's sum differs
// from the one last indexed (body_sha256), or when the blocks changed (a parser that reads the
// same body otherwise); a call that finds none of those writes no block and no text row and keeps
// the revision. A message has no revisions.
//
// Every call reconciles the item's block mentions from the source's pairs, one row per (block,
// target), so a reindex with a new grammar re-reads them even when the text is unchanged. A new
// row takes `at`, or at the item's first indexing its own refs row's created_at and source_seq
// for that target; Stamp gives the others their event. A direct message holds no mention. It
// stores the revision, the next block number and the source's sum and clears owed_at.
//
// It returns the targets of the block mentions it inserted or deleted, and of the links a block
// holds whose excerpt it rewrote.
func indexItemBlocks(
	ctx context.Context,
	tx pgx.Tx,
	kind ItemKind,
	itemID string,
	at time.Time,
	source itemBlockSource,
) ([]Target, error) {
	tables, err := kind.tables()
	if err != nil {
		return nil, err
	}
	state, err := lockItemState(ctx, tx, kind, tables, itemID)
	if err != nil {
		return nil, err
	}
	// An item no block-aware build indexed holds no block and no block mention: indexItemBlocks
	// writes them in the transaction that marks the state indexed, and a state row goes only with
	// its item.
	stored := []itemblocks.Stored{}
	var excerpts map[string]string // each current block's stored excerpt, by id
	if state.indexed {
		if stored, excerpts, err = storedItemBlocks(ctx, tx, kind, tables, itemID); err != nil {
			return nil, err
		}
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
		return nil, err
	}

	byID := make(map[string]itemblocks.Stored, len(stored))
	for _, block := range stored {
		byID[block.ID] = block
	}
	changed := !state.indexed || state.bodySHA256 != source.bodySum
	var written, kept []itemBlockRow
	for index, block := range next {
		row := itemBlockRow{
			id: ids[index], position: index, nodeType: block.Type, sum: block.Hash,
			excerpt: text.HeadRunes(block.Text, BlockExcerptRunes), text: block.Text,
		}
		if index < len(source.madeBy) {
			row.madeBy = source.madeBy[index]
		}
		previous, held := byID[row.id]
		delete(byID, row.id)
		if held && previous.Position >= 0 && previous.Hash == row.sum {
			kept = append(kept, row)
			if previous.Position != index || previous.Type != block.Type {
				changed = true
			}
			continue
		}
		written = append(written, row)
		changed = true
	}
	var removed []string
	for _, block := range stored {
		if _, untaken := byID[block.ID]; untaken && block.Position >= 0 {
			removed = append(removed, block.ID)
			changed = true
		}
	}

	revision := state.revision
	if changed {
		revision++
		madeAt := &at
		if !state.indexed && tables.revisions() && state.edited {
			madeAt = nil
		}
		if err := writeItemBlocks(ctx, tx, kind, tables, itemID, revision, at, madeAt, written, kept, removed); err != nil {
			return nil, err
		}
	}
	pairs := source.pairs(next, ids)
	moved, err := replaceItemBlockRefs(ctx, tx, kind, tables, itemID, at, !state.indexed, pairs)
	if err != nil {
		return nil, err
	}
	// A link a block keeps shows that block's excerpt in its target's referenced-by panel, so a
	// block that keeps its id while its excerpt changes names the targets of the links it holds,
	// as a document's reworded block does; a change past the excerpt's runes names none.
	newExcerpts := make(map[string]string, len(written))
	for _, row := range written {
		newExcerpts[row.id] = row.excerpt
	}
	for _, pair := range pairs {
		if previous, held := excerpts[pair.blockID]; held {
			if excerpt, rewritten := newExcerpts[pair.blockID]; rewritten && excerpt != previous {
				moved = append(moved, pair.target)
			}
		}
	}
	if err := storeItemState(ctx, tx, kind, tables, itemID, revision, counter, source.bodySum); err != nil {
		return nil, err
	}
	return moved, nil
}

// itemState is an item's state row as indexItemBlocks found it: whether a block-aware build
// indexed it before (a revision above 0 where the kind counts revisions, a message's body_sha256
// set), its revision and next block number (a message's are 0 and 1), the sum of the text it was
// last indexed from, and whether the item was edited (edited_at set, which a message has not).
type itemState struct {
	indexed    bool
	revision   int
	nextBlock  int
	bodySHA256 string
	edited     bool
}

// lockItemState inserts the item's state row when it has none and locks it `for no key update`.
func lockItemState(ctx context.Context, tx pgx.Tx, kind ItemKind, tables itemTables, itemID string) (itemState, error) {
	if _, err := tx.Exec(ctx, `
		insert into `+tables.state+` (`+tables.item+`) values ($1::uuid) on conflict do nothing
	`, itemID); err != nil {
		return itemState{}, fmt.Errorf("create the block state of %s %s: %w", kind, itemID, err)
	}
	state := itemState{nextBlock: 1}
	var sum *string
	var err error
	if tables.revisions() {
		err = tx.QueryRow(ctx, `
			select s.revision, s.next_block, s.body_sha256, i.edited_at is not null
			from `+tables.state+` s join `+tables.items+` i on i.id = s.`+tables.item+`
			where s.`+tables.item+` = $1::uuid
			for no key update of s
		`, itemID).Scan(&state.revision, &state.nextBlock, &sum, &state.edited)
		state.indexed = state.revision > 0
	} else {
		err = tx.QueryRow(ctx, `
			select body_sha256 from `+tables.state+` where `+tables.item+` = $1::uuid for no key update
		`, itemID).Scan(&sum)
		state.indexed = sum != nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return itemState{}, fmt.Errorf("lock the block state of %s %s: the %s is gone", kind, itemID, kind)
	}
	if err != nil {
		return itemState{}, fmt.Errorf("lock the block state of %s %s: %w", kind, itemID, err)
	}
	if sum != nil {
		state.bodySHA256 = *sum
	}
	return state, nil
}

// storedItemBlocks is every block the item ever held, current ones first in body order, then
// removed ones, the most recently removed first, so Align's first untaken match prefers the block
// in place and, among removed blocks of equal text, the last removed.
func storedItemBlocks(ctx context.Context, tx pgx.Tx, kind ItemKind, tables itemTables, itemID string) ([]itemblocks.Stored, map[string]string, error) {
	rows, err := tx.Query(ctx, `
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

// itemBlockRow is one current block indexItemBlocks writes: its id, position, type, identity
// hash, excerpt, text and, for a kind whose blocks record one, its maker as the json its made_by
// column holds (nil: the item's own author made it).
type itemBlockRow struct {
	id       string
	position int
	nodeType string
	sum      string
	excerpt  string
	text     string
	madeBy   *string
}

// writeItemBlocks stores one new revision of the item's blocks: written (new, changed or restored
// blocks) take the revision and `at` as their changed facts, and a new one madeAt; kept blocks
// move their position, type and excerpt; removed ones lose their position at `at`. A kind with
// revisions records each written block's text, and null for each removed one, at the revision. A
// kind whose blocks record a maker writes each written block's; a kept block keeps the maker it
// was written with, as it keeps its text.
func writeItemBlocks(
	ctx context.Context,
	tx pgx.Tx,
	kind ItemKind,
	tables itemTables,
	itemID string,
	revision int,
	at time.Time,
	madeAt *time.Time,
	written, kept []itemBlockRow,
	removed []string,
) error {
	if len(removed) > 0 {
		if _, err := tx.Exec(ctx, `
			update `+tables.blocks+` set position = null, removed_at = $2
			where `+tables.item+` = $1::uuid and block_id = any($3)
		`, itemID, at, removed); err != nil {
			return fmt.Errorf("remove the blocks %s %s no longer holds: %w", kind, itemID, err)
		}
	}
	if len(written) > 0 {
		ids, positions, types, sums, excerpts, makers := itemBlockColumns(written)
		var err error
		if tables.revisions() {
			// Only a kind whose blocks record a maker has the column, so the three fragments are
			// empty for the others; kind is this package's own constant, never caller input.
			maker, makerValue, makerUpdate := "", "", ""
			if tables.makers {
				maker, makerValue, makerUpdate = ", made_by", ", b.made_by::jsonb", ", made_by = excluded.made_by"
			}
			_, err = tx.Exec(ctx, `
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
			`, itemID, madeAt, revision, at, ids, positions, types, sums, excerpts, makers)
		} else {
			// A message is never edited, so its blocks are made and last changed when it was
			// written; a reindex under another parser moves only those of new text.
			_, err = tx.Exec(ctx, `
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
			`, itemID, at, ids, positions, types, sums, excerpts)
		}
		if err != nil {
			return fmt.Errorf("record the blocks of %s %s: %w", kind, itemID, err)
		}
	}
	if len(kept) > 0 {
		ids, positions, types, _, excerpts, _ := itemBlockColumns(kept)
		if _, err := tx.Exec(ctx, `
			update `+tables.blocks+` b set position = k.position, type = k.type, excerpt = k.excerpt
			from unnest($2::text[], $3::int[], $4::text[], $5::text[]) as k (id, position, type, excerpt)
			where b.`+tables.item+` = $1::uuid and b.block_id = k.id
		`, itemID, ids, positions, types, excerpts); err != nil {
			return fmt.Errorf("move the unchanged blocks of %s %s: %w", kind, itemID, err)
		}
	}
	if !tables.revisions() || len(written)+len(removed) == 0 {
		return nil
	}
	textIDs := make([]string, 0, len(written)+len(removed))
	texts := make([]*string, 0, len(written)+len(removed))
	for i := range written {
		textIDs, texts = append(textIDs, written[i].id), append(texts, &written[i].text)
	}
	for _, id := range removed {
		textIDs, texts = append(textIDs, id), append(texts, nil)
	}
	if _, err := tx.Exec(ctx, `
		insert into `+tables.texts+` (`+tables.item+`, block_id, revision, text)
		select $1::uuid, t.id, $2, t.text from unnest($3::text[], $4::text[]) as t (id, text)
		on conflict (`+tables.item+`, block_id, revision) do update set text = excluded.text
	`, itemID, revision, textIDs, texts); err != nil {
		return fmt.Errorf("record the block texts of revision %d of %s %s: %w", revision, kind, itemID, err)
	}
	return nil
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
	return blockPairs(nodes, located)
}

// replaceItemBlockRefs reconciles the item's block mentions with pairs, matching on the columns
// of their key, and returns the targets of the pairs it deleted and inserted. A pair that survives
// keeps its created_at and source_seq; a new one takes `at`, or at the item's first indexing the
// item's own refs row for that target's created_at and source_seq, when one exists. At the first
// indexing the item holds no block mention, so nothing is deleted.
func replaceItemBlockRefs(
	ctx context.Context,
	tx pgx.Tx,
	kind ItemKind,
	tables itemTables,
	itemID string,
	at time.Time,
	firstIndexing bool,
	pairs []blockPair,
) ([]Target, error) {
	blockIDs := make([]string, len(pairs))
	kinds := make([]string, len(pairs))
	toKinds := make([]string, len(pairs))
	toIDs := make([]string, len(pairs))
	for i, pair := range pairs {
		blockIDs[i], kinds[i], toKinds[i], toIDs[i] = pair.blockID, pair.kind, pair.target.Kind, pair.target.ID
	}
	var moved []Target
	if !firstIndexing {
		var err error
		moved, err = scanTargets(tx.Query(ctx, `
			delete from `+tables.refs+` r
			where r.`+tables.item+` = $1::uuid and not exists (
				select 1 from unnest($2::text[], $3::text[], $4::text[], $5::text[]) as p (block_id, kind, to_kind, to_id)
				where p.block_id = r.block_id and p.kind = r.kind and p.to_kind = r.to_kind and p.to_id = r.to_id)
			returning r.to_kind, r.to_id
		`, itemID, blockIDs, kinds, toKinds, toIDs))
		if err != nil {
			return nil, fmt.Errorf("delete the block mentions %s %s dropped: %w", kind, itemID, err)
		}
	}
	if len(pairs) == 0 {
		return moved, nil
	}
	added, err := scanTargets(tx.Query(ctx, `
		insert into `+tables.refs+` (`+tables.item+`, block_id, from_node, to_kind, to_id, kind, created_at, source_seq)
		select $1::uuid, p.block_id, $6::text || '/' || $1::uuid::text || '#' || p.block_id, p.to_kind, p.to_id, p.kind,
		       coalesce(case when $8::boolean then r.created_at end, $7::timestamptz), case when $8::boolean then r.source_seq end
		from unnest($2::text[], $3::text[], $4::text[], $5::text[]) as p (block_id, kind, to_kind, to_id)
		left join refs r on r.from_kind = $6 and r.from_id = $1::uuid::text
		                and r.to_kind = p.to_kind and r.to_id = p.to_id
		on conflict (`+tables.item+`, block_id, to_kind, to_id, kind) do nothing
		returning to_kind, to_id
	`, itemID, blockIDs, kinds, toKinds, toIDs, string(kind), at, firstIndexing))
	if err != nil {
		return nil, fmt.Errorf("record the block mentions of %s %s: %w", kind, itemID, err)
	}
	return append(moved, added...), nil
}

// storeItemState records the item indexed from the text whose sum is bodySum, at revision with
// next as the next block number for a kind that counts revisions, and settles its owed_at.
func storeItemState(ctx context.Context, tx pgx.Tx, kind ItemKind, tables itemTables, itemID string, revision, next int, bodySum string) error {
	var err error
	if tables.revisions() {
		_, err = tx.Exec(ctx, `
			update `+tables.state+` set revision = $2, next_block = $3, body_sha256 = $4, owed_at = null
			where `+tables.item+` = $1::uuid
		`, itemID, revision, next, bodySum)
	} else {
		_, err = tx.Exec(ctx, `
			update `+tables.state+` set body_sha256 = $2, owed_at = null where `+tables.item+` = $1::uuid
		`, itemID, bodySum)
	}
	if err != nil {
		return fmt.Errorf("record the blocks of %s %s indexed: %w", kind, itemID, err)
	}
	return nil
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
// reports whether it indexed it. It takes the item row `for no key update`, the lock an edit of
// the item takes, then indexes it as a reply naming one of its blocks does (IndexItemIfOwed),
// which takes the state row after it; it never takes an owner row. A new block and a new mention
// take the time the item's text was written: a comment's and an ask's coalesce(edited_at,
// created_at), a message's created_at. An item a write site indexed since it was listed is no
// longer owed and is left alone; one deleted since took its state row with it (on delete
// cascade), so nothing is owed.
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
	var found bool
	switch err := tx.QueryRow(ctx, `select true from `+tables.items+` where id = $1::uuid for no key update`, item.ID).Scan(&found); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("lock %s %s: %w", item.Kind, item.ID, err)
	}
	indexed, err := IndexItemIfOwed(ctx, tx, item.Kind, item.ID, serverURL)
	if err != nil {
		return false, err
	}
	return indexed, tx.Commit(ctx)
}
