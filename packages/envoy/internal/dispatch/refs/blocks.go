package refs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Pointer is one reference a typed block's `block[]` attribute holds (pmdoc.BlockPointer): the
// typed block's id and type, the link's kind (the attribute's name), the block reference it names,
// and the block types the attribute points at (none: any block).
type Pointer struct {
	Block, BlockType, Kind string
	Target                 text.Ref
	Targets                []string
}

// IndexBlocks records version `version` (written at versionAt) of document artifactID's blocks as
// graph nodes, and each mention written inside a block as an edge out of the innermost block
// holding its start, in the caller's transaction, which holds the document's owner row. blocks
// are the version's rendered blocks in document order, each parent before its children; the first
// holder of a repeated id is the node. located must be text.ExtractAt(markdown, …). A block whose
// id is longer than text.MaxBlockIDBytes, which no key of the index can hold, is no node: a
// mention inside it starts at the innermost node holding it, if any, and the indexing logs a
// warning naming the document, the longest such id's length and the bound.
//
// pointers are the version's `block[]` values (pmdoc.BlockPointers, which lists none of a typed
// block repeating the id of a typed block before it): each whose block is a node is an edge of its
// attribute's kind from that block, carrying the attribute's targets as to_types, which the
// readers list only while its target has an allowed type (block_ref_listed). A value is written in
// its typed block's opening line, so the extraction also finds it as a mention held by that block:
// the block's mention of a target is kept only while the block itself holds more references to it
// than its pointers do (a string attribute citing it too, say).
//
// A block whose text (markdown[From:To]) is unchanged keeps its changed version; a new, changed
// or restored block takes this version, and its text is recorded in document_block_texts; a
// block the version no longer holds is deleted, recorded there with no text, and the edges at it
// stop resolving until a version holds its id again. A block's stored excerpt is its text cut to
// BlockExcerptRunes, the excerpt a document's mention has shown since migration 0092.
//
// A pair (block, kind, target) that survives keeps its created version, time and source event,
// and takes the to_types this version gives it. A new pair takes this version and versionAt, and
// the source event Stamp gives it. On the document's first indexing (no block rows yet), a new
// pair dates instead from the document's own refs row for that target, when one exists. The
// version is recorded as indexed, and as the version the document's blocks stand at
// (document_block_versions). It returns the targets of the pairs it inserted or deleted, or whose
// to_types it changed, whose counts moved: a node is counted once for each block citing it. It
// also returns what else a Referenced by panel or a link shows that the version moved: the target
// of each pair it kept in a block whose text it changed (the block's text is that link's excerpt),
// and each block it changed or deleted that some source cites (citedBlocks), whose text every
// link to it and its own panel show.
func IndexBlocks(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	version int,
	versionAt time.Time,
	markdown string,
	blocks []model.ArtifactBlock,
	located []text.Located,
	pointers []Pointer,
) ([]Target, error) {
	stored, err := storedBlockSums(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	firstIndexing := len(stored) == 0
	nodes, long, longest := uniqueBlocks(blocks)
	if long > 0 {
		slog.Warn("dispatch: block ids past the bound are not graph nodes; a mention inside one of their blocks starts at the block holding it",
			"artifact", artifactID, "version", version, "blocks", long, "longest_bytes", longest, "bound_bytes", text.MaxBlockIDBytes)
	}

	// The blocks whose text this version changed (new, changed or restored), and those it deleted.
	var ids, types, sums, excerpts []string
	var textIDs []string
	var texts []*string
	for _, block := range nodes {
		body := markdown[block.From:block.To]
		hexSum := BlockTextSum(body)
		previous, held := stored[block.ID]
		delete(stored, block.ID)
		if held && previous == hexSum {
			continue
		}
		ids, types, sums = append(ids, block.ID), append(types, block.Type), append(sums, hexSum)
		excerpts = append(excerpts, text.HeadRunes(body, BlockExcerptRunes))
		textIDs, texts = append(textIDs, block.ID), append(texts, &body)
	}
	deleted := make([]string, 0, len(stored))
	for id := range stored {
		deleted = append(deleted, id)
		textIDs, texts = append(textIDs, id), append(texts, nil)
	}

	if len(deleted) > 0 {
		if _, err := tx.Exec(ctx, `
			delete from document_blocks where artifact_id = $1 and block_id = any($2)
		`, artifactID, deleted); err != nil {
			return nil, fmt.Errorf("delete the blocks version %d of %s no longer holds: %w", version, artifactID, err)
		}
	}
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			select $1::uuid, b.id, a.ref_key || '#' || b.id, b.type, b.sum, b.excerpt, $2, $3
			from unnest($4::text[], $5::text[], $6::text[], $7::text[]) as b (id, type, sum, excerpt)
			cross join (select ref_key from artifacts where id = $1::uuid) a
			on conflict (artifact_id, block_id) do update set
				type = excluded.type,
				text_sha256 = excluded.text_sha256,
				excerpt = excluded.excerpt,
				changed_version = excluded.changed_version,
				changed_at = excluded.changed_at
		`, artifactID, version, versionAt, ids, types, sums, excerpts); err != nil {
			return nil, fmt.Errorf("record the blocks version %d of %s changed: %w", version, artifactID, err)
		}
	}
	if len(textIDs) > 0 {
		if _, err := tx.Exec(ctx, `
			insert into document_block_texts (artifact_id, block_id, version, text)
			select $1::uuid, t.id, $2, t.text from unnest($3::text[], $4::text[]) as t (id, text)
			on conflict (artifact_id, block_id, version) do update set text = excluded.text
		`, artifactID, version, textIDs, texts); err != nil {
			return nil, fmt.Errorf("record the block texts of version %d of %s: %w", version, artifactID, err)
		}
	}

	pairs := blockPairs(nodes, located, pointers)
	moved, err := replaceBlockRefs(ctx, tx, artifactID, version, versionAt, firstIndexing, pairs)
	if err != nil {
		return nil, err
	}
	changedBlocks := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		changedBlocks[id] = struct{}{}
	}
	for _, pair := range pairs {
		if _, changed := changedBlocks[pair.blockID]; changed {
			moved = append(moved, pair.target)
		}
	}
	// textIDs is every block this version changed, restored or deleted.
	cited, err := citedBlocks(ctx, tx, artifactID, textIDs)
	if err != nil {
		return nil, err
	}
	moved = append(moved, cited...)
	if _, err := tx.Exec(ctx, `
		insert into document_indexed_versions (artifact_id, version) values ($1, $2)
		on conflict (artifact_id, version) do nothing
	`, artifactID, version); err != nil {
		return nil, fmt.Errorf("record version %d of %s indexed: %w", version, artifactID, err)
	}
	// Migration 0122's trigger marked the document owed when this version's row was inserted;
	// this transaction's own index settles it.
	if _, err := tx.Exec(ctx, `
		insert into document_block_versions (artifact_id, version) values ($1, $2)
		on conflict (artifact_id) do update set version = excluded.version
	`, artifactID, version); err != nil {
		return nil, fmt.Errorf("record the blocks of %s indexed at version %d: %w", artifactID, version, err)
	}
	if err := writeDocumentWindows(ctx, tx, artifactID, markdown, nodes); err != nil {
		return nil, err
	}
	return moved, nil
}

// DocumentWindowKind is the embeddings kind of a window of a document's blocks, as
// ItemKind.WindowKind is of a comment's, a message's or an ask's.
const DocumentWindowKind = "document_block"

// windowKinds is every kind an embeddings window row is written under. A statement naming them
// as literals is one the planner can prove embeddings_window_owner's partial predicate for,
// whatever a parameter holds (writeWindows).
const windowKinds = `('document_block', 'comment_block', 'message_block', 'ask_part')`

// BlockEmbeddingStates is where meaning search stands on each block of the owner whose windows
// are kind kind under ownerKey: model.BlockEmbedded for a block every window covering it holds
// a vector of its current text for, model.BlockEmbeddingPending while one of them waits for its
// vector. A block no window covers is in no entry of the map, which a reader of one block reads
// as model.BlockNotEmbedded - a block the indexer puts in no window, and every block of an owner
// whose windows are not written yet. The read names the window kinds as literals beside the
// parameter, as every read and write of a window does (writeWindows), so the planner can prove
// embeddings_window_owner's partial predicate and reach the owner's rows through that index.
//
// It lives here, with the writes of the rows it reads, so the list of window kinds is held in
// one place: this package's windowKinds, migration 20261102233625's index predicate and the
// column the two of them name.
func BlockEmbeddingStates(ctx context.Context, q Queryer, kind, ownerKey string) (map[string]string, error) {
	rows, err := q.Query(ctx, `
		select covered.block_id, bool_and(e.embedded_hash is not distinct from e.content_hash)
		from embeddings e cross join unnest(e.covers) as covered(block_id)
		where e.kind = $1 and e.kind in `+windowKinds+`
		  and split_part(e.id, '#', 1) = $2
		group by covered.block_id
	`, kind, ownerKey)
	if err != nil {
		return nil, fmt.Errorf("read the embedding state of the blocks of %s %s: %w", kind, ownerKey, err)
	}
	defer rows.Close()
	states := map[string]string{}
	for rows.Next() {
		var blockID string
		var current bool
		if err := rows.Scan(&blockID, &current); err != nil {
			return nil, fmt.Errorf("scan the embedding state of a block of %s %s: %w", kind, ownerKey, err)
		}
		states[blockID] = model.BlockEmbeddingPending
		if current {
			states[blockID] = model.BlockEmbedded
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the embedding state of the blocks of %s %s: %w", kind, ownerKey, err)
	}
	return states, nil
}

// WriteDocumentWindows writes the windows of document artifactID's blocks, as the version write
// writes them (IndexBlocks), for the block-index tick's window arm: blocks are the document's
// blocks in document order, each parent before its children, and markdown the text they index
// into.
func WriteDocumentWindows(ctx context.Context, tx pgx.Tx, artifactID, markdown string, blocks []model.ArtifactBlock) error {
	nodes, _, _ := uniqueBlocks(blocks)
	return writeDocumentWindows(ctx, tx, artifactID, markdown, nodes)
}

// writeDocumentWindows writes the windows of the document's blocks (nodes, as uniqueBlocks keeps
// them) in the caller's transaction, which holds the document's owner row, and stamps the state
// those blocks are in beside them, so the window arm can tell these windows from ones an older
// build left behind. The owner key of a document's windows is its artifact id, and the context
// of each is the document's own title and the heading above the window
// (itemblocks.DocumentWindows).
func writeDocumentWindows(ctx context.Context, tx pgx.Tx, artifactID, markdown string, nodes []model.ArtifactBlock) error {
	var title string
	if err := tx.QueryRow(ctx, `select name from artifacts where id = $1::uuid`, artifactID).Scan(&title); err != nil {
		return fmt.Errorf("read the title of %s for its windows: %w", artifactID, err)
	}
	return writeWindows(ctx, tx, DocumentWindowKind, artifactID,
		itemblocks.DocumentWindows(title, documentBlockTexts(markdown, nodes)), blockState{
			query: `select ` + documentBlockState + ` as block_state
			          from document_block_versions v where v.artifact_id = $5::uuid`,
			args: []any{artifactID},
		})
}

// documentBlockTexts is each block of nodes holding text of its own, in document order, with
// that text (markdown[From:To]): a block inside another is left out, since the window of its
// parent already holds its text and no window should embed one passage twice. nodes are in
// document order, each parent before its children, so a block starting before the last kept one
// ended is inside it.
func documentBlockTexts(markdown string, nodes []model.ArtifactBlock) []itemblocks.WindowBlock {
	blocks := make([]itemblocks.WindowBlock, 0, len(nodes))
	end := 0
	for _, node := range nodes {
		if node.To <= node.From || node.From < end {
			continue
		}
		end = node.To
		blocks = append(blocks, itemblocks.WindowBlock{ID: node.ID, Type: node.Type, Text: markdown[node.From:node.To]})
	}
	return blocks
}

// blockState is how one write of an owner's windows (writeWindows) tells that write what state
// the blocks those windows cover are in: one query returning a single block_state column, which
// the write runs as a common table expression and stamps beside the windows. An index that
// settles the owner's own state row writes that row here, returning the state it has just
// written (indexedItemState), so it pays no round trip for the stamp; a write that settles no
// such row reads the state the row already holds (storedItemState, and the document's own query
// above). The query numbers its parameters from $5 and passes their values in args, since
// writeWindows holds $1 to $4.
type blockState struct {
	query string
	args  []any
}

// writeWindows writes windows as the windows of owner ownerKey, of kind kind, in the caller's
// transaction: each takes the id "<owner key>#<its id suffix>" through
// embeddings_enqueue_window, which rewrites a row only where the hash of its text moved, and the
// owner's rows of that kind the new set does not name are deleted. Rewriting an owner's windows
// when none of them changed therefore leaves every row's vector, attempts and wait as they
// stand, and an owner left with no block loses every window it had.
//
// The whole write is one statement, so a document of fifty windows costs one round trip and an
// index whose state query settles its owner's state row (indexedItemState) pays nothing beyond
// the round trip it was already sending for that. The windows go through jsonb_to_recordset in
// the top-level select; the state query, the stamp of what it returns and the delete are common
// table expressions, and Postgres runs a data-modifying one exactly once and to completion
// whether or not the select reads it. The delete cannot race the enqueue: it names only ids the
// new set does not hold, and the enqueue writes only ids it does.
//
// The delete names the kind twice, as the parameter and in the literal list of every window
// kind, because a generic plan knows nothing of what the parameter holds: the literal list is
// what lets the planner prove embeddings_window_owner's partial predicate and read the owner's
// rows through that index.
func writeWindows(ctx context.Context, tx pgx.Tx, kind, ownerKey string, windows []itemblocks.Window, state blockState) error {
	type row struct {
		Suffix string   `json:"suffix"`
		Text   string   `json:"text"`
		Covers []string `json:"covers"`
	}
	rows := make([]row, len(windows))
	ids := make([]string, len(windows))
	for i, window := range windows {
		rows[i] = row{Suffix: window.IDSuffix, Text: window.Text, Covers: window.Covers}
		ids[i] = ownerKey + "#" + window.IDSuffix
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("encode the windows of %s %s: %w", kind, ownerKey, err)
	}
	if _, err := tx.Exec(ctx, `
		with state as (
			`+state.query+`
		),
		stamped as (
			insert into embedding_window_states (kind, owner_key, block_state, written_at)
			select $1::text, $2::text, state.block_state, now() from state
			on conflict (kind, owner_key) do update
			  set block_state = excluded.block_state, written_at = excluded.written_at
		),
		dropped as (
			delete from embeddings
			where kind = $1 and kind in `+windowKinds+`
			  and split_part(id, '#', 1) = $2 and id <> all($4::text[])
		)
		select embeddings_enqueue_window($1, $2 || '#' || w.suffix, w.text, w.covers)
		from jsonb_to_recordset($3::jsonb) as w (suffix text, text text, covers text[])
	`, append([]any{kind, ownerKey, payload, ids}, state.args...)...); err != nil {
		return fmt.Errorf("write the windows of %s %s: %w", kind, ownerKey, err)
	}
	return nil
}

// documentBlockState is the state of a document's blocks that its windows are stamped with
// (writeDocumentWindows) and read back against (DocumentsOwedWindows): the version they are
// indexed at, over document_block_versions v. A document owed its first block index holds a
// null version, which stamps and compares as the empty string, so windowing it once settles it
// until the index that gives it a version writes its windows again. The state is read from
// document_block_versions in the statement that writes the windows, so what is stamped is the
// version the blocks those windows cover are indexed at.
const documentBlockState = `coalesce(v.version::text, '')`

// DocumentsOwedWindows lists at most limit documents whose blocks the index holds and whose
// windows do not stand for those blocks, in artifact id order past after (empty: from the
// start), for the block-index tick's window arm. A document written before meaning search moved
// to windows has blocks and no window, and one whose version write failed after its blocks were
// written has the same. One a build without this change re-indexed during the deploy is the
// harder case: it keeps the windows of its old text, so holding a window says nothing. The read
// therefore asks both whether the state its windows were written from
// (embedding_window_states, migration 20261104010400) is the state its blocks are in now, and
// whether it holds a window at all, so a window row lost by any other route is written again
// too. The read walks document_block_versions, one row per document, and probes the other
// three tables through their keys.
func DocumentsOwedWindows(ctx context.Context, q Queryer, after string, limit int) ([]string, error) {
	rows, err := q.Query(ctx, `
		select v.artifact_id::text from document_block_versions v
		where v.artifact_id > $1::uuid
		  and exists (select 1 from document_blocks b where b.artifact_id = v.artifact_id)
		  and (not exists (
			select 1 from embedding_window_states w
			where w.kind = '`+DocumentWindowKind+`' and w.owner_key = v.artifact_id::text
			  and w.block_state = `+documentBlockState+`
		  ) or not exists (
			select 1 from embeddings e
			where e.kind = '`+DocumentWindowKind+`' and split_part(e.id, '#', 1) = v.artifact_id::text
		  ))
		order by v.artifact_id limit $2
	`, uuidAfter(after), limit)
	if err != nil {
		return nil, fmt.Errorf("list the documents whose windows do not stand for their blocks: %w", err)
	}
	documents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the documents whose windows do not stand for their blocks: %w", err)
	}
	return documents, nil
}

// uuidAfter is a keyset cursor over uuids: the id a batch starts after, or the nil uuid, which
// is below every uuid a row holds, when a batch starts from the beginning.
func uuidAfter(after string) string {
	if after == "" {
		return "00000000-0000-0000-0000-000000000000"
	}
	return after
}

// citedBlocks is, of the document's blocks named by ids, each that a source cites (a refs or a
// block_refs row points at its node), as a block target.
func citedBlocks(ctx context.Context, tx pgx.Tx, artifactID string, ids []string) ([]Target, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	cited, err := scanTargets(tx.Query(ctx, `
		select 'block', b.node_id
		from (
			select a.ref_key || '#' || t.id as node_id
			from unnest($2::text[]) as t (id)
			cross join (select ref_key from artifacts where id = $1::uuid) a
		) b
		where exists (select 1 from refs r where r.to_kind = 'block' and r.to_id = b.node_id)
		   or exists (select 1 from block_refs r where r.to_kind = 'block' and r.to_id = b.node_id)
	`, artifactID, ids))
	if err != nil {
		return nil, fmt.Errorf("read the cited blocks of %s: %w", artifactID, err)
	}
	return cited, nil
}

// BlockTextSum is the sha256 (hex) of a block's text (markdown[From:To]), as document_blocks
// stores it in text_sha256.
func BlockTextSum(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// storedBlockSums is each block the document's index holds, by id, with its text's sha256.
func storedBlockSums(ctx context.Context, tx pgx.Tx, artifactID string) (map[string]string, error) {
	rows, err := tx.Query(ctx, `select block_id, text_sha256 from document_blocks where artifact_id = $1`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the blocks of %s: %w", artifactID, err)
	}
	defer rows.Close()
	stored := make(map[string]string)
	for rows.Next() {
		var id, sum string
		if err := rows.Scan(&id, &sum); err != nil {
			return nil, fmt.Errorf("scan a block of %s: %w", artifactID, err)
		}
		stored[id] = sum
	}
	return stored, rows.Err()
}

// blockPair is one link a block holds: the block's id, the edge's kind and its target, and the
// block types it points at (nil: any).
type blockPair struct {
	blockID string
	kind    string
	target  Target
	toTypes []string
}

// heldTarget is a target as one block holds it.
type heldTarget struct {
	blockID string
	target  Target
}

// blockPairs is each distinct link the pointers of nodes and the mentions in located write: a
// pointer from its typed block (pointerPairs), a mention from the innermost of nodes holding its
// start. A mention no block holds, and an external URL, write none; nor does a block's mention of
// a target its own pointers account for (IndexBlocks).
func blockPairs(nodes []model.ArtifactBlock, located []text.Located, pointers []Pointer) []blockPair {
	pairs, pointed := pointerPairs(nodes, pointers)
	pairs = slices.Grow(pairs, len(located))

	starts := make([]int, len(located))
	for i, mention := range located {
		starts[i] = mention.Offset
	}
	holders := innermostBlocks(nodes, starts)
	mentions := make([]heldTarget, 0, len(located))
	counts := make(map[heldTarget]int, len(located))
	for i, mention := range located {
		if holders[i] < 0 {
			continue
		}
		target, ok := TargetOf(mention.Ref)
		if !ok {
			continue
		}
		held := heldTarget{blockID: nodes[holders[i]].ID, target: target}
		if counts[held] == 0 {
			mentions = append(mentions, held)
		}
		counts[held]++
	}
	for _, held := range mentions {
		if counts[held] > pointed[held] {
			pairs = append(pairs, blockPair{blockID: held.blockID, kind: "mentions", target: held.target})
		}
	}
	return pairs
}

// pointerPairs is each distinct link the pointers of nodes write, from its typed block, and the
// number of each block's pointer values naming each target. A pinned and an unpinned value of one
// attribute name one target, and write one pair. No attribute is named mentions (pmdoc), so no
// pointer's pair is a mention's. A pointer writes a pair only from the node uniqueBlocks keeps for
// its block id, the first holder of the id with an offset, and only when that node is the typed
// block holding it (its type is the pointer's BlockType). pmdoc.BlockPointers already lists none
// of a typed block repeating the id of a typed block before it. The type check keeps a typed
// block's pointers off a first holder of its id that is not typed (a paragraph, a heading). It is
// not what makes the two paths agree: a typed block's id reads back from the markdown as the tree
// holds it (pmdoc.TypedBlockIDAttribute), so the version write, indexing the tree, and the
// reconciliation, reading the markdown back, list the same pointers under that id and resolve it
// to the same node of the same render, and without the check both would write the same wrong edge
// from that holder. A holder with no offset (a list item, a table cell) is never a node, so a typed
// block after it is the id's node and keeps its pointers. One whose block is no node (an id past
// the bound, say) or not its own writes nothing and is charged against no mention.
func pointerPairs(nodes []model.ArtifactBlock, pointers []Pointer) ([]blockPair, map[heldTarget]int) {
	if len(pointers) == 0 {
		return nil, nil
	}
	type pointerKey struct {
		held heldTarget
		kind string
	}
	written := make(map[pointerKey]struct{}, len(pointers))
	nodeTypes := make(map[string]string, len(nodes))
	for _, node := range nodes {
		nodeTypes[node.ID] = node.Type
	}
	pairs := make([]blockPair, 0, len(pointers))
	pointed := make(map[heldTarget]int, len(pointers))
	for _, pointer := range pointers {
		if typ, ok := nodeTypes[pointer.Block]; !ok || typ != pointer.BlockType {
			continue
		}
		target, _ := TargetOf(pointer.Target)
		held := heldTarget{blockID: pointer.Block, target: target}
		pointed[held]++
		key := pointerKey{held: held, kind: pointer.Kind}
		if _, dup := written[key]; dup {
			continue
		}
		written[key] = struct{}{}
		pairs = append(pairs, blockPair{blockID: pointer.Block, kind: pointer.Kind, target: target, toTypes: pointer.Targets})
	}
	return pairs, pointed
}

// replaceBlockRefs reconciles the document's block_refs with pairs, matching on the five columns
// of its key, and returns the targets of the pairs it deleted and inserted, and of those whose
// to_types it changed.
func replaceBlockRefs(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	version int,
	versionAt time.Time,
	firstIndexing bool,
	pairs []blockPair,
) ([]Target, error) {
	blockIDs := make([]string, len(pairs))
	kinds := make([]string, len(pairs))
	toKinds := make([]string, len(pairs))
	toIDs := make([]string, len(pairs))
	// A pair's to_types travel as one space-joined text, null for no limit: a block type's name is
	// a directive name (pmdoc), which holds no space.
	toTypes := make([]*string, len(pairs))
	for i, pair := range pairs {
		blockIDs[i], kinds[i], toKinds[i], toIDs[i] = pair.blockID, pair.kind, pair.target.Kind, pair.target.ID
		if len(pair.toTypes) > 0 {
			joined := strings.Join(pair.toTypes, " ")
			toTypes[i] = &joined
		}
	}
	moved, err := scanTargets(tx.Query(ctx, `
		delete from block_refs r
		where r.artifact_id = $1 and not exists (
			select 1 from unnest($2::text[], $3::text[], $4::text[], $5::text[]) as p (block_id, kind, to_kind, to_id)
			where p.block_id = r.block_id and p.kind = r.kind and p.to_kind = r.to_kind and p.to_id = r.to_id)
		returning r.to_kind, r.to_id
	`, artifactID, blockIDs, kinds, toKinds, toIDs))
	if err != nil {
		return nil, fmt.Errorf("delete the block mentions version %d of %s dropped: %w", version, artifactID, err)
	}
	if len(pairs) == 0 {
		return moved, nil
	}
	// A surviving pair keeps its dates and source event and takes this version's to_types, so a
	// change of an attribute's targets reaches it here; it is returned only when that changed.
	added, err := scanTargets(tx.Query(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, to_types, created_version, created_at, source_seq)
		select $1::uuid, p.block_id, a.ref_key || '#' || p.block_id, p.to_kind, p.to_id, p.kind, string_to_array(p.to_types, ' '), $6,
		       coalesce(case when $8 then r.created_at end, $7), case when $8 then r.source_seq end
		from unnest($2::text[], $3::text[], $4::text[], $5::text[], $9::text[]) as p (block_id, kind, to_kind, to_id, to_types)
		cross join (select ref_key from artifacts where id = $1::uuid) a
		left join refs r on r.from_kind = 'artifact' and r.from_id = $1::uuid::text
		                and r.to_kind = p.to_kind and r.to_id = p.to_id
		on conflict (artifact_id, block_id, to_kind, to_id, kind) do update set to_types = excluded.to_types
		where block_refs.to_types is distinct from excluded.to_types
		returning to_kind, to_id
	`, artifactID, blockIDs, kinds, toKinds, toIDs, version, versionAt, firstIndexing, toTypes))
	if err != nil {
		return nil, fmt.Errorf("record the block mentions of version %d of %s: %w", version, artifactID, err)
	}
	return append(moved, added...), nil
}

// BlockLinks lists each link from a block of document fromArtifact to a block of document
// toArtifact (artifact uuids; the two may be one document), newest first, then by the source
// block's node id, then by the target's. A link counts only while both of its blocks are current
// blocks of their documents: a deleted block's links drop out, and its stored mentions bring them
// back when a version holds its id again. A pointer's link counts only while its target has a
// type the pointer allows (block_ref_listed).
func BlockLinks(ctx context.Context, q Queryer, fromArtifact, toArtifact string) ([]model.BlockLink, error) {
	rows, err := q.Query(ctx, `
		select r.kind, r.from_node, r.to_id, r.created_at, r.created_version
		from block_refs r
		join document_blocks f on f.artifact_id = r.artifact_id and f.block_id = r.block_id
		join document_blocks t on t.node_id = r.to_id and t.artifact_id = $2::uuid
		where r.artifact_id = $1::uuid and r.to_kind = 'block' and (r.to_types is null or block_ref_listed(r.to_types, r.to_id))
		order by r.created_at desc, r.from_node, r.to_id`, fromArtifact, toArtifact)
	if err != nil {
		return nil, fmt.Errorf("list the block links from %s to %s: %w", fromArtifact, toArtifact, err)
	}
	type linkRow struct {
		kind, from, to string
		createdAt      time.Time
		createdVersion int
	}
	var links []linkRow
	var ends []edgeRow
	for rows.Next() {
		var link linkRow
		if err := rows.Scan(&link.kind, &link.from, &link.to, &link.createdAt, &link.createdVersion); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan a block link: %w", err)
		}
		links = append(links, link)
		ends = append(ends, edgeRow{otherKind: "block", otherID: link.from}, edgeRow{otherKind: "block", otherID: link.to})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the block links from %s to %s: %w", fromArtifact, toArtifact, err)
	}
	nodes, err := resolveNodes(ctx, q, true, ends)
	if err != nil {
		return nil, err
	}
	result := make([]model.BlockLink, 0, len(links))
	for _, link := range links {
		from, fromFound := nodes[[2]string{"block", link.from}]
		to, toFound := nodes[[2]string{"block", link.to}]
		if !fromFound || !toFound {
			// A block deleted between the two reads: its links drop out as they would have.
			continue
		}
		result = append(result, model.BlockLink{
			Kind:           link.kind,
			From:           from.GraphNode,
			To:             to.GraphNode,
			CreatedAt:      link.createdAt,
			CreatedVersion: link.createdVersion,
		})
	}
	return result, nil
}

// BlockChange is when a block of a document last changed: the version its text last changed in
// and that version's time, as document_blocks records them (IndexBlocks).
type BlockChange struct {
	Version int
	At      time.Time
}

// BlockChanges is each current block of document artifactID, by block id, with when its text last
// changed. It is the document's own half of the out-of-date rule a decision record is read by: a
// decision's changed version and time are the greatest of its blocks', and BlockLinks gives the
// same facts for the blocks it links to. A document the block index has not reached has no rows
// and answers an empty map.
func BlockChanges(ctx context.Context, q Queryer, artifactID string) (map[string]BlockChange, error) {
	rows, err := q.Query(ctx, `
		select block_id, changed_version, changed_at from document_blocks where artifact_id = $1::uuid
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("list the block changes of %s: %w", artifactID, err)
	}
	defer rows.Close()
	changes := map[string]BlockChange{}
	for rows.Next() {
		var blockID string
		var change BlockChange
		if err := rows.Scan(&blockID, &change.Version, &change.At); err != nil {
			return nil, fmt.Errorf("scan a block change of %s: %w", artifactID, err)
		}
		changes[blockID] = change
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the block changes of %s: %w", artifactID, err)
	}
	return changes, nil
}

// BlockTargets is where a write to document artifactID lands and the block each of targets names,
// by node id (text.TargetID), as the block index would hold it once that write, leaving the blocks
// written (rendered, in document order, each parent before its children), is indexed, read through
// q: a block of that document as written's node for its id (uniqueBlocks: the first holder of a
// repeated id, and no id the index cannot key), any other as its document's current blocks
// (document_blocks) hold it, with one query for every such target. A target no block has is
// absent. The write lands in the document's own issue, or in none when a project holds it; that
// issue's ancestors are read only once a target turns out to sit in another document
// (IssueAncestors), since a pointer inside the writing document - a plan task's `needs`, the
// common case - is held to nothing above it (pmdoc.CheckBlockPointers), and this runs inside
// the write's transaction.
func BlockTargets(ctx context.Context, q Queryer, artifactID string, written []model.ArtifactBlock, targets []text.Ref) (pmdoc.BlockWriting, map[string]pmdoc.BlockTarget, error) {
	var writing pmdoc.BlockWriting
	if err := q.QueryRow(ctx, `
		select coalesce(ref_key, ''), coalesce(issue_key, '') from artifacts where id = $1
	`, artifactID).Scan(&writing.Document, &writing.Issue); err != nil {
		return pmdoc.BlockWriting{}, nil, fmt.Errorf("read the reference of document %s: %w", artifactID, err)
	}
	nodes, _, _ := uniqueBlocks(written)
	own := make(map[string]string, len(nodes))
	for _, node := range nodes {
		own[node.ID] = node.Type
	}
	found := make(map[string]pmdoc.BlockTarget, len(targets))
	var elsewhere []string
	for _, target := range targets {
		nodeID := text.TargetID(target)
		// A ref_key holds no '#', so the node id's document part ends at its first one.
		if document, _, _ := strings.Cut(nodeID, "#"); document != writing.Document {
			elsewhere = append(elsewhere, nodeID)
		} else if typ, ok := own[target.Block]; ok {
			found[nodeID] = pmdoc.BlockTarget{Types: []string{typ}, Document: writing.Document, Issue: writing.Issue}
		}
	}
	if len(elsewhere) == 0 {
		return writing, found, nil
	}
	rows, err := q.Query(ctx, `
		select d.node_id, d.type, a.ref_key, coalesce(a.issue_key, '')
		from document_blocks d join artifacts a on a.id = d.artifact_id
		where d.node_id = any($1)
	`, elsewhere)
	if err != nil {
		return pmdoc.BlockWriting{}, nil, fmt.Errorf("read the blocks %s points at: %w", artifactID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID, typ, document, issue string
		if err := rows.Scan(&nodeID, &typ, &document, &issue); err != nil {
			return pmdoc.BlockWriting{}, nil, fmt.Errorf("scan a block %s points at: %w", artifactID, err)
		}
		target := found[nodeID]
		target.Types = append(target.Types, typ)
		target.Document, target.Issue = document, issue
		found[nodeID] = target
	}
	if err := rows.Err(); err != nil {
		return pmdoc.BlockWriting{}, nil, err
	}
	// Only a target in another document can sit in another issue, so the ancestor walk is paid
	// for only here.
	if writing.Ancestors, err = IssueAncestors(ctx, q, writing.Issue); err != nil {
		return pmdoc.BlockWriting{}, nil, err
	}
	return writing, found, nil
}

// blockFirstVersion is a left join lateral, aliased alias, over the first version that held block
// block of document artifact (SQL expressions of the outer row): F, the lowest version
// document_block_texts records a text of the block in, as alias.version, and F's authors and
// time as alias.authors and alias.created_at. alias.known is whether Dispatch knows the block was
// made in F: F is 1, or a block-aware build indexed version F-1, which did not hold it. A block
// first seen when its document was first indexed, or after a version no block-aware build
// indexed, may be older than F. A block with no recorded text leaves every column null.
func blockFirstVersion(alias, artifact, block string) string {
	return `left join lateral (
		select t.version,
		       t.version = 1 or exists (
		         select 1 from document_indexed_versions i where i.artifact_id = t.artifact_id and i.version = t.version - 1
		       ) as known,
		       v.authors, v.created_at
		from document_block_texts t
		left join artifact_versions v on v.artifact_id = t.artifact_id and v.number = t.version
		where t.artifact_id = ` + artifact + ` and t.block_id = ` + block + ` and t.text is not null
		order by t.version limit 1
	) ` + alias + ` on true`
}

// blockFirstColumns selects blockFirstVersion's columns, aliased alias, in blockFirst.targets'
// order.
func blockFirstColumns(alias string) string {
	return alias + `.version, ` + alias + `.known, ` + alias + `.authors, ` + alias + `.created_at`
}

// blockFirst is the first version that held a block, as blockFirstColumns selects it.
type blockFirst struct {
	version   *int
	known     *bool
	authors   []byte
	createdAt *time.Time
}

func (f *blockFirst) targets() []any {
	return []any{&f.version, &f.known, &f.authors, &f.createdAt}
}

// maker is who made the block and when: the authors and time of the version that first held it
// when Dispatch knows the block was made there, else nobody recorded at no time. Authors that do
// not decode (storedActors) fail.
func (f blockFirst) maker() ([]model.Actor, *time.Time, error) {
	if f.known == nil || !*f.known {
		return []model.Actor{}, nil, nil
	}
	authors, err := storedActors(f.authors)
	if err != nil {
		return nil, nil, err
	}
	return authors, f.createdAt, nil
}

// RemovedBlockNode is the node nodeID of block blockID of document (its resolved artifact node),
// which no current version holds since version removedVersion deleted it, built as loadBlockNodes
// builds a current block's but with no text or changed version: made as a current block is
// (blockFirstVersion), last changed by that deletion, with its version's authors and time, and
// part of the document at the latest version a block-aware build indexed below removedVersion, the
// last one known to hold it (null when none is). The caller adds its pin. Authors that do not
// decode (storedActors) fail the read.
func RemovedBlockNode(ctx context.Context, q Queryer, document model.GraphNode, nodeID, blockID string, removedVersion int) (model.GraphNode, error) {
	var first blockFirst
	var removedBy []byte
	var removedAt *time.Time
	var held *int
	documentID := document.ID
	if err := q.QueryRow(ctx, `
		select `+blockFirstColumns("first")+`, removed.authors, removed.created_at,
		       (select max(i.version) from document_indexed_versions i where i.artifact_id = b.artifact_id and i.version < $3)
		from (select $1::uuid as artifact_id, $2::text as block_id) b
		`+blockFirstVersion("first", "b.artifact_id", "b.block_id")+`
		left join artifact_versions removed on removed.artifact_id = b.artifact_id and removed.number = $3
	`, documentID, blockID, removedVersion).Scan(append(first.targets(), &removedBy, &removedAt, &held)...); err != nil {
		return model.GraphNode{}, fmt.Errorf("read the facts of removed block %q of %s: %w", blockID, documentID, err)
	}
	madeBy, madeAt, err := first.maker()
	if err != nil {
		return model.GraphNode{}, fmt.Errorf("decode the maker of removed block %q of %s: %w", blockID, documentID, err)
	}
	changedBy, err := storedActors(removedBy)
	if err != nil {
		return model.GraphNode{}, fmt.Errorf("decode the last change of removed block %q of %s: %w", blockID, documentID, err)
	}
	return model.GraphNode{
		Kind:          "block",
		ID:            nodeID,
		BlockID:       blockID,
		IssueKey:      document.IssueKey,
		Project:       document.Project,
		Ref:           BlockRef(document.Ref, blockID),
		Document:      document.Ref,
		MadeBy:        madeBy,
		MadeAt:        madeAt,
		ChangedBy:     changedBy,
		ChangedAt:     removedAt,
		Parent:        &model.NodeParent{Kind: "artifact", ID: documentID, Ref: document.Ref},
		ParentVersion: held,
	}, nil
}

// uniqueBlocks keeps the first block of each id, in order, as the node GET …/blocks addresses by
// that id: a rendered version can repeat an id (pmdoc/block_ids.go), and document_blocks holds one
// row per id. It skips an empty id, and one that is not valid UTF-8 or holds a NUL, which a
// Postgres text[] cannot hold. It skips an id longer than text.MaxBlockIDBytes too, which a
// B-tree key of the index cannot hold, and reports how many blocks it skipped for that and the
// longest such id's bytes.
func uniqueBlocks(blocks []model.ArtifactBlock) (nodes []model.ArtifactBlock, long, longest int) {
	nodes = make([]model.ArtifactBlock, 0, len(blocks))
	seen := make(map[string]struct{}, len(blocks))
	for _, block := range blocks {
		if block.ID == "" || !text.Storable(block.ID) {
			continue
		}
		if len(block.ID) > text.MaxBlockIDBytes {
			long, longest = long+1, max(longest, len(block.ID))
			continue
		}
		if _, dup := seen[block.ID]; dup {
			continue
		}
		seen[block.ID] = struct{}{}
		nodes = append(nodes, block)
	}
	return nodes, long, longest
}

// innermostBlocks names, for each start (ascending), the index of the tightest block whose
// [From, To) holds it, or -1. blocks are in document order, each parent before its children.
func innermostBlocks(blocks []model.ArtifactBlock, starts []int) []int {
	holders := make([]int, len(starts))
	stack := make([]int, 0, 8)
	next := 0
	for i, start := range starts {
		for next < len(blocks) && blocks[next].From <= start {
			for len(stack) > 0 && blocks[stack[len(stack)-1]].To <= blocks[next].From {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, next)
			next++
		}
		for len(stack) > 0 && blocks[stack[len(stack)-1]].To <= start {
			stack = stack[:len(stack)-1]
		}
		holders[i] = -1
		if len(stack) > 0 {
			holders[i] = stack[len(stack)-1]
		}
	}
	return holders
}
