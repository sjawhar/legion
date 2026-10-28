package refs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// IndexBlocks records version `version` (written at versionAt) of document artifactID's blocks as
// graph nodes, and each mention written inside a block as an edge out of the innermost block
// holding its start, in the caller's transaction, which holds the document's owner row. blocks
// are the version's rendered blocks in document order, each parent before its children; the first
// holder of a repeated id is the node. located must be text.ExtractAt(markdown, …). A block whose
// id is longer than text.MaxBlockIDBytes, which no key of the index can hold, is no node: a
// mention inside it starts at the innermost node holding it, if any, and the indexing logs a
// warning naming the document, the longest such id's length and the bound.
//
// A block whose text (markdown[From:To]) is unchanged keeps its changed version; a new, changed
// or restored block takes this version, and its text is recorded in document_block_texts; a
// block the version no longer holds is deleted, recorded there with no text, and the edges at it
// stop resolving until a version holds its id again. A block's stored excerpt is its text cut to
// BlockExcerptRunes, the excerpt a document's mention has shown since migration 0092.
//
// A pair (block, target) that survives keeps its created version, time and source event. A new
// pair takes this version and versionAt, and the source event Stamp gives it. On the document's
// first indexing (no block rows yet), a new pair dates instead from the document's own refs row
// for that target, when one exists. The version is recorded as indexed, and as the version the
// document's blocks stand at (document_block_versions). It returns the targets of the pairs it
// inserted or deleted, whose counts moved: a node is counted once for each block citing it. It is
// ReadBlockIndex, PlanBlocks and Queue back to back, the queued statements run at once as a batch
// of their own.
func IndexBlocks(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	version int,
	versionAt time.Time,
	markdown string,
	blocks []model.ArtifactBlock,
	located []text.Located,
) ([]Target, error) {
	index, err := ReadBlockIndex(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	indexing := PlanBlocks(index, artifactID, version, versionAt, markdown, blocks, located)
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		indexing.Queue(b)
		return nil
	}); err != nil {
		return nil, err
	}
	return indexing.Moved(), nil
}

// BlockIndex is what a document's block index holds: the sha256 of each block's text, by block
// id, and the links its blocks hold.
type BlockIndex struct {
	sums  map[string]string
	pairs []blockPair
}

// ReadBlockIndex reads what q's block index holds for artifactID, its links read with the predicate
// the reconciling delete visits them by.
func ReadBlockIndex(ctx context.Context, q Queryer, artifactID string) (BlockIndex, error) {
	sums, err := storedBlockSums(ctx, q, artifactID)
	if err != nil {
		return BlockIndex{}, err
	}
	rows, err := q.Query(ctx, `select block_id, kind, to_kind, to_id from block_refs where artifact_id = $1`, artifactID)
	if err != nil {
		return BlockIndex{}, fmt.Errorf("read the block mentions of %s: %w", artifactID, err)
	}
	defer rows.Close()
	var pairs []blockPair
	for rows.Next() {
		var pair blockPair
		if err := rows.Scan(&pair.blockID, &pair.kind, &pair.target.Kind, &pair.target.ID); err != nil {
			return BlockIndex{}, fmt.Errorf("scan a block mention of %s: %w", artifactID, err)
		}
		pairs = append(pairs, pair)
	}
	if err := rows.Err(); err != nil {
		return BlockIndex{}, fmt.Errorf("read the block mentions of %s: %w", artifactID, err)
	}
	return BlockIndex{sums: sums, pairs: pairs}, nil
}

// BlockIndexing is one version's block index (IndexBlocks), planned against what the index holds
// (PlanBlocks), so the targets it moves are known before anything is written, and written by
// Queue, which puts its statements on a write phase's batch and does nothing else.
type BlockIndexing struct {
	artifactID    string
	version       int
	versionAt     time.Time
	firstIndexing bool
	// deleted are the blocks the version no longer holds; ids, types, sums and excerpts the
	// blocks whose text it changed (new, changed or restored); textIDs and texts the block texts
	// it records, nil for a deleted block.
	deleted, ids, types, sums, excerpts, textIDs []string
	texts                                        []*string
	pairs                                        []blockPair
	moved                                        []Target
	held                                         BlockIndex
}

// PlanBlocks plans version's block index of artifactID against index, what the document's block
// index holds: one ReadBlockIndex read, or the Held of the caller's own earlier indexing of the
// document that is not written yet. It does the indexing's whole work - each block's text sum, its
// excerpt, the links its mentions write - and reads nothing.
func PlanBlocks(
	index BlockIndex,
	artifactID string,
	version int,
	versionAt time.Time,
	markdown string,
	blocks []model.ArtifactBlock,
	located []text.Located,
) BlockIndexing {
	stored := maps.Clone(index.sums)
	indexing := BlockIndexing{artifactID: artifactID, version: version, versionAt: versionAt, firstIndexing: len(stored) == 0}
	nodes, long, longest := uniqueBlocks(blocks)
	if long > 0 {
		slog.Warn("dispatch: block ids past the bound are not graph nodes; a mention inside one of their blocks starts at the block holding it",
			"artifact", artifactID, "version", version, "blocks", long, "longest_bytes", longest, "bound_bytes", text.MaxBlockIDBytes)
	}

	// The blocks whose text this version changed (new, changed or restored), and those it deleted.
	held := make(map[string]string, len(nodes))
	for _, block := range nodes {
		body := markdown[block.From:block.To]
		sum := sha256.Sum256([]byte(body))
		hexSum := hex.EncodeToString(sum[:])
		held[block.ID] = hexSum
		previous, kept := stored[block.ID]
		delete(stored, block.ID)
		if kept && previous == hexSum {
			continue
		}
		indexing.ids, indexing.types, indexing.sums = append(indexing.ids, block.ID), append(indexing.types, block.Type), append(indexing.sums, hexSum)
		indexing.excerpts = append(indexing.excerpts, text.HeadRunes(body, BlockExcerptRunes))
		indexing.textIDs, indexing.texts = append(indexing.textIDs, block.ID), append(indexing.texts, &body)
	}
	indexing.deleted = make([]string, 0, len(stored))
	for id := range stored {
		indexing.deleted = append(indexing.deleted, id)
		indexing.textIDs, indexing.texts = append(indexing.textIDs, id), append(indexing.texts, nil)
	}

	// The links the version drops, in the order the index listed them, then those it adds, in
	// the order the insert writes them: the targets the two statements return.
	indexing.pairs = blockPairs(nodes, located)
	written := make(map[blockPair]struct{}, len(indexing.pairs))
	for _, pair := range indexing.pairs {
		written[pair] = struct{}{}
	}
	kept := make(map[blockPair]struct{}, len(index.pairs))
	for _, pair := range index.pairs {
		kept[pair] = struct{}{}
		if _, still := written[pair]; !still {
			indexing.moved = append(indexing.moved, pair.target)
		}
	}
	for _, pair := range indexing.pairs {
		if _, already := kept[pair]; !already {
			indexing.moved = append(indexing.moved, pair.target)
		}
	}
	indexing.held = BlockIndex{sums: held, pairs: indexing.pairs}
	return indexing
}

// Moved is the targets of the links the indexing inserts or deletes, whose counts move.
func (b BlockIndexing) Moved() []Target {
	return b.moved
}

// Held is what the document's block index holds once the indexing is written.
func (b BlockIndexing) Held() BlockIndex {
	return b.held
}

// Queue queues the indexing's writes on a write phase's batch: the blocks the version deleted and
// changed, their texts, the links its blocks hold, and the version recorded as indexed and as the
// one the blocks stand at.
func (b BlockIndexing) Queue(batch *store.WriteBatch) {
	artifactID, version := b.artifactID, b.version
	if len(b.deleted) > 0 {
		batch.Queue(fmt.Sprintf("delete the blocks version %d of %s no longer holds", version, artifactID), `
			delete from document_blocks where artifact_id = $1 and block_id = any($2)
		`, artifactID, b.deleted)
	}
	if len(b.ids) > 0 {
		batch.Queue(fmt.Sprintf("record the blocks version %d of %s changed", version, artifactID), `
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
		`, artifactID, version, b.versionAt, b.ids, b.types, b.sums, b.excerpts)
	}
	if len(b.textIDs) > 0 {
		batch.Queue(fmt.Sprintf("record the block texts of version %d of %s", version, artifactID), `
			insert into document_block_texts (artifact_id, block_id, version, text)
			select $1::uuid, t.id, $2, t.text from unnest($3::text[], $4::text[]) as t (id, text)
			on conflict (artifact_id, block_id, version) do update set text = excluded.text
		`, artifactID, version, b.textIDs, b.texts)
	}
	b.queueBlockRefs(batch)
	batch.Queue(fmt.Sprintf("record version %d of %s indexed", version, artifactID), `
		insert into document_indexed_versions (artifact_id, version) values ($1, $2)
		on conflict (artifact_id, version) do nothing
	`, artifactID, version)
	// Migration 0122's trigger marked the document owed when this version's row was inserted;
	// this transaction's own index settles it.
	batch.Queue(fmt.Sprintf("record the blocks of %s indexed at version %d", artifactID, version), `
		insert into document_block_versions (artifact_id, version) values ($1, $2)
		on conflict (artifact_id) do update set version = excluded.version
	`, artifactID, version)
}

// storedBlockSums is each block the document's index holds, by id, with its text's sha256.
func storedBlockSums(ctx context.Context, q Queryer, artifactID string) (map[string]string, error) {
	rows, err := q.Query(ctx, `select block_id, text_sha256 from document_blocks where artifact_id = $1`, artifactID)
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

// blockPair is one link a block holds: the block's id, the edge's kind and its target.
type blockPair struct {
	blockID string
	kind    string
	target  Target
}

// blockPairs is each distinct link the mentions in located write from the innermost of nodes
// holding its start. A mention no block holds, and an external URL, write none.
func blockPairs(nodes []model.ArtifactBlock, located []text.Located) []blockPair {
	starts := make([]int, len(located))
	for i, mention := range located {
		starts[i] = mention.Offset
	}
	holders := innermostBlocks(nodes, starts)
	pairs := make([]blockPair, 0, len(located))
	seen := make(map[blockPair]struct{}, len(located))
	for i, mention := range located {
		if holders[i] < 0 {
			continue
		}
		target, ok := targetOf(mention.Ref)
		if !ok {
			continue
		}
		pair := blockPair{blockID: nodes[holders[i]].ID, kind: "mentions", target: target}
		if _, dup := seen[pair]; dup {
			continue
		}
		seen[pair] = struct{}{}
		pairs = append(pairs, pair)
	}
	return pairs
}

// queueBlockRefs queues the reconciliation of the document's block_refs with the indexing's pairs,
// matching on the five columns of its key.
func (b BlockIndexing) queueBlockRefs(batch *store.WriteBatch) {
	pairs := b.pairs
	blockIDs := make([]string, len(pairs))
	kinds := make([]string, len(pairs))
	toKinds := make([]string, len(pairs))
	toIDs := make([]string, len(pairs))
	for i, pair := range pairs {
		blockIDs[i], kinds[i], toKinds[i], toIDs[i] = pair.blockID, pair.kind, pair.target.Kind, pair.target.ID
	}
	batch.Queue(fmt.Sprintf("delete the block mentions version %d of %s dropped", b.version, b.artifactID), `
		delete from block_refs r
		where r.artifact_id = $1 and not exists (
			select 1 from unnest($2::text[], $3::text[], $4::text[], $5::text[]) as p (block_id, kind, to_kind, to_id)
			where p.block_id = r.block_id and p.kind = r.kind and p.to_kind = r.to_kind and p.to_id = r.to_id)
	`, b.artifactID, blockIDs, kinds, toKinds, toIDs)
	if len(pairs) == 0 {
		return
	}
	batch.Queue(fmt.Sprintf("record the block mentions of version %d of %s", b.version, b.artifactID), `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version, created_at, source_seq)
		select $1::uuid, p.block_id, a.ref_key || '#' || p.block_id, p.to_kind, p.to_id, p.kind, $6,
		       coalesce(case when $8 then r.created_at end, $7), case when $8 then r.source_seq end
		from unnest($2::text[], $3::text[], $4::text[], $5::text[]) as p (block_id, kind, to_kind, to_id)
		cross join (select ref_key from artifacts where id = $1::uuid) a
		left join refs r on r.from_kind = 'artifact' and r.from_id = $1::uuid::text
		                and r.to_kind = p.to_kind and r.to_id = p.to_id
		on conflict (artifact_id, block_id, to_kind, to_id, kind) do nothing
	`, b.artifactID, blockIDs, kinds, toKinds, toIDs, b.version, b.versionAt, b.firstIndexing)
}

// BlockLinks lists each link from a block of document fromArtifact to a block of document
// toArtifact (artifact uuids; the two may be one document), newest first, then by the source
// block's node id, then by the target's. A link counts only while both of its blocks are current
// blocks of their documents: a deleted block's links drop out, and its stored mentions bring them
// back when a version holds its id again.
func BlockLinks(ctx context.Context, q Queryer, fromArtifact, toArtifact string) ([]model.BlockLink, error) {
	rows, err := q.Query(ctx, `
		select r.kind, r.from_node, r.to_id, r.created_at, r.created_version
		from block_refs r
		join document_blocks f on f.artifact_id = r.artifact_id and f.block_id = r.block_id
		join document_blocks t on t.node_id = r.to_id and t.artifact_id = $2::uuid
		where r.artifact_id = $1::uuid and r.to_kind = 'block'
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
