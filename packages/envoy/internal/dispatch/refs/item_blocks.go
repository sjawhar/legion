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

// ItemKind is the kind of item whose blocks IndexItem writes, a comment or a message. It names the
// tables a call writes and is never caller input.
type ItemKind string

const (
	ItemComment ItemKind = "comment"
	ItemMessage ItemKind = "message"
)

// itemTables is the storage of one item kind and its blocks: the items table, the blocks table,
// the column naming the item in every block table, the state table and the block mentions table.
type itemTables struct {
	items, blocks, item, state, refs string
}

func (kind ItemKind) tables() (itemTables, error) {
	switch kind {
	case ItemComment:
		return itemTables{items: "comments", blocks: "comment_blocks", item: "comment_id", state: "comment_block_state", refs: "comment_block_refs"}, nil
	case ItemMessage:
		return itemTables{items: "messages", blocks: "message_blocks", item: "message_id", state: "message_block_state", refs: "message_block_refs"}, nil
	}
	return itemTables{}, fmt.Errorf("index the blocks of a %q: not an item kind", string(kind))
}

// BlockKind is the storage kind of a block of an item of this kind, in graph_edges and refs:
// comment_block or message_block. The API names every block node kind block; the storage kind
// keeps a build that predates item blocks from counting or resolving their rows (TargetOf).
func (kind ItemKind) BlockKind() string {
	return string(kind) + "_block"
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
// transaction. The caller holds the item's owner row and the item row (its insert or update, or a
// read `for no key update`); IndexItem takes the item's state row after them (inserting it when
// missing, then `for no key update`), so every writer takes owner, item, state in that order and
// the reconciliation, which takes no owner, takes item then state.
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
	tables, err := kind.tables()
	if err != nil {
		return nil, err
	}
	state, err := lockItemState(ctx, tx, kind, tables, itemID)
	if err != nil {
		return nil, err
	}
	// An item no block-aware build indexed holds no block and no block mention: IndexItem writes
	// them in the transaction that marks the state indexed, and a state row goes only with its
	// item.
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

	next := itemblocks.Split(body)
	var ids []string
	if carried != nil && kind == ItemComment && carried.BaseRevision == state.revision {
		spans, err := itemblocks.ReadCarried(stored, body, carried.Blocks)
		if err != nil {
			return nil, err
		}
		ids, counter = itemblocks.Carry(stored, next, spans, counter)
	} else {
		ids, counter = itemblocks.Align(stored, next, counter)
	}

	byID := make(map[string]itemblocks.Stored, len(stored))
	for _, block := range stored {
		byID[block.ID] = block
	}
	bodySum := itemblocks.BodySHA256(body)
	changed := !state.indexed || state.bodySHA256 != bodySum
	var written, kept []itemBlockRow
	for index, block := range next {
		row := itemBlockRow{
			id: ids[index], position: index, nodeType: block.Type, sum: block.Hash,
			excerpt: text.HeadRunes(block.Text, BlockExcerptRunes), text: block.Text,
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
		if !state.indexed && kind == ItemComment && state.edited {
			madeAt = nil
		}
		if err := writeItemBlocks(ctx, tx, kind, tables, itemID, revision, at, madeAt, written, kept, removed); err != nil {
			return nil, err
		}
	}
	pairs := itemBlockPairs(next, ids, located)
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
	if err := storeItemState(ctx, tx, kind, itemID, revision, counter, bodySum); err != nil {
		return nil, err
	}
	return moved, nil
}

// itemState is an item's state row as IndexItem found it: whether a block-aware build indexed
// it before (a comment's revision above 0, a message's body_sha256 set), its revision and next
// block number (a comment's; a message's are 0 and 1), the body it was last indexed from, and
// whether the comment was edited (edited_at set).
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
	if kind == ItemComment {
		err = tx.QueryRow(ctx, `
			select s.revision, s.next_block, s.body_sha256, c.edited_at is not null
			from comment_block_state s join comments c on c.id = s.comment_id
			where s.comment_id = $1::uuid
			for no key update of s
		`, itemID).Scan(&state.revision, &state.nextBlock, &sum, &state.edited)
		state.indexed = state.revision > 0
	} else {
		err = tx.QueryRow(ctx, `
			select body_sha256 from message_block_state where message_id = $1::uuid for no key update
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

// itemBlockRow is one current block IndexItem writes: its id, position, type, identity hash,
// excerpt and text.
type itemBlockRow struct {
	id       string
	position int
	nodeType string
	sum      string
	excerpt  string
	text     string
}

// writeItemBlocks stores one new revision of the item's blocks: written (new, changed or restored
// blocks) take the revision and `at` as their changed facts, and a new one madeAt; kept blocks
// move their position, type and excerpt; removed ones lose their position at `at`. A comment
// records each written block's text, and null for each removed one, at the revision.
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
		ids, positions, types, sums, excerpts := itemBlockColumns(written)
		var err error
		if kind == ItemComment {
			_, err = tx.Exec(ctx, `
				insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt, made_at, changed_revision, changed_at)
				select $1::uuid, b.id, 'comment/' || $1::uuid::text || '#' || b.id, b.position, b.type, b.sum, b.excerpt, $2, $3, $4
				from unnest($5::text[], $6::int[], $7::text[], $8::text[], $9::text[]) as b (id, position, type, sum, excerpt)
				on conflict (comment_id, block_id) do update set
					position = excluded.position,
					type = excluded.type,
					text_sha256 = excluded.text_sha256,
					excerpt = excluded.excerpt,
					changed_revision = excluded.changed_revision,
					changed_at = excluded.changed_at,
					removed_at = null
			`, itemID, madeAt, revision, at, ids, positions, types, sums, excerpts)
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
		ids, positions, types, _, excerpts := itemBlockColumns(kept)
		if _, err := tx.Exec(ctx, `
			update `+tables.blocks+` b set position = k.position, type = k.type, excerpt = k.excerpt
			from unnest($2::text[], $3::int[], $4::text[], $5::text[]) as k (id, position, type, excerpt)
			where b.`+tables.item+` = $1::uuid and b.block_id = k.id
		`, itemID, ids, positions, types, excerpts); err != nil {
			return fmt.Errorf("move the unchanged blocks of %s %s: %w", kind, itemID, err)
		}
	}
	if kind != ItemComment || len(written)+len(removed) == 0 {
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
		insert into comment_block_texts (comment_id, block_id, revision, text)
		select $1::uuid, t.id, $2, t.text from unnest($3::text[], $4::text[]) as t (id, text)
		on conflict (comment_id, block_id, revision) do update set text = excluded.text
	`, itemID, revision, textIDs, texts); err != nil {
		return fmt.Errorf("record the block texts of revision %d of comment %s: %w", revision, itemID, err)
	}
	return nil
}

// itemBlockColumns is rows as the parallel arrays an unnest reads.
func itemBlockColumns(rows []itemBlockRow) (ids []string, positions []int32, types, sums, excerpts []string) {
	for _, row := range rows {
		ids = append(ids, row.id)
		positions = append(positions, int32(row.position))
		types = append(types, row.nodeType)
		sums = append(sums, row.sum)
		excerpts = append(excerpts, row.excerpt)
	}
	return ids, positions, types, sums, excerpts
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

// storeItemState records the item indexed from the body whose itemblocks.BodySHA256 is bodySum, at
// revision with next as the next block number for a comment, and settles its owed_at.
func storeItemState(ctx context.Context, tx pgx.Tx, kind ItemKind, itemID string, revision, next int, bodySum string) error {
	var err error
	if kind == ItemComment {
		_, err = tx.Exec(ctx, `
			update comment_block_state set revision = $2, next_block = $3, body_sha256 = $4, owed_at = null
			where comment_id = $1::uuid
		`, itemID, revision, next, bodySum)
	} else {
		_, err = tx.Exec(ctx, `
			update message_block_state set body_sha256 = $2, owed_at = null where message_id = $1::uuid
		`, itemID, bodySum)
	}
	if err != nil {
		return fmt.Errorf("record the blocks of %s %s indexed: %w", kind, itemID, err)
	}
	return nil
}

// OwedItemBlock is one comment or message owed a block index: its state row's owed_at is set,
// by the trigger on a write no block-aware build indexed (a build without the index wrote it,
// during the roll or after a rollback), or by rebuild-refs.
type OwedItemBlock struct {
	Kind ItemKind
	ID   string
}

// OwedItemBlocks lists at most limit comments and messages owed a block index, oldest owed_at
// first, and locks nothing. Each table's read goes through its partial index on owed_at.
func OwedItemBlocks(ctx context.Context, q Queryer, limit int) ([]OwedItemBlock, error) {
	rows, err := q.Query(ctx, `
		select kind, id from (
			(select 'comment' as kind, comment_id::text as id, owed_at from comment_block_state
			 where owed_at is not null order by owed_at limit $1)
			union all
			(select 'message', message_id::text, owed_at from message_block_state
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

// IndexOwedItemBlocks indexes one owed comment or message in a transaction of its own and reports
// whether it indexed it. It takes the item row `for no key update`, the lock an edit of the
// comment takes, then indexes it as a reply naming one of its blocks does (IndexItemIfOwed), which
// takes the state row after it; it never takes an owner row. A new block and a new mention take
// the time the item's body was written: a comment's coalesce(edited_at, created_at), a message's
// created_at. An item a write site indexed since it was listed is no longer owed and is left
// alone; one deleted since took its state row with it (on delete cascade), so nothing is owed.
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
