package refs

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// itemOfBlockKind is the item kind whose blocks are stored under kind, comment_block or
// message_block (ItemKind.BlockKind).
func itemOfBlockKind(kind string) (ItemKind, bool) {
	for _, item := range []ItemKind{ItemComment, ItemMessage} {
		if kind == item.BlockKind() {
			return item, true
		}
	}
	return "", false
}

// itemBlocksKnown reports whether this build reads the blocks of items of kind: nodeLoaders has
// their storage kind. Every read that lists or counts a block's edge in place of its item's
// (heldByABlock, an item's own reads in readEdgeRows) asks it, so a build that cannot address an
// item's blocks lists and counts the item's own edges, as a build that predates them does.
func itemBlocksKnown(kind ItemKind) bool {
	_, known := nodeLoaders[kind.BlockKind()]
	return known
}

// loadItemBlockNodes resolves the block nodes of comments and messages, stored under kind
// comment_block or message_block and addressed '<item kind>/<item uuid>#<block id>', from the
// current blocks of their items. A block no current revision holds resolves to nothing, so every
// edge at it is omitted until a revision holds its id again. The node's kind is block, as a
// document block's is; its parent is its item, and its Text the block's whole text.
func loadItemBlockNodes(ctx context.Context, q Queryer, kind string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	item, ok := itemOfBlockKind(kind)
	if !ok {
		return fmt.Errorf("load %s nodes: not an item block kind", kind)
	}
	if item == ItemMessage {
		return loadMessageBlockNodes(ctx, q, kind, ids, nodes)
	}
	blocks, err := commentBlockNodes(ctx, q, ids, false)
	if err != nil {
		return err
	}
	for _, block := range blocks {
		nodes[[2]string{kind, block.node.ID}] = resolvedNode{GraphNode: block.node, text: block.excerpt}
	}
	return nil
}

// commentBlockNode is a comment block's node, its stored excerpt, and when a revision removed it
// (nil while it is current).
type commentBlockNode struct {
	node      model.GraphNode
	excerpt   string
	removedAt *time.Time
}

// commentBlockNodes reads the comment blocks whose node ids are ids: current ones, and removed
// ones too when includeRemoved. Only a comment's author edits it, so a block is made and last
// changed by the author: made at the time of the revision that first held it (null for a block
// found at the first indexing of a comment edited before blocks were recorded), changed at its
// changed revision's time. It belongs to its comment at that revision. Its Text is the latest text
// comment_block_texts records for it, empty once removed.
func commentBlockNodes(ctx context.Context, q Queryer, ids []string, includeRemoved bool) ([]commentBlockNode, error) {
	rows, err := q.Query(ctx, `
		select b.node_id, b.block_id, b.excerpt, b.made_at, b.changed_revision, b.changed_at, b.removed_at,
		       c.id::text, c.issue_key, coalesce(i.project_key, a.project_key, ''), coalesce(a.slug, ''), c.author,
		       coalesce((select t.text from comment_block_texts t where t.comment_id = b.comment_id and t.block_id = b.block_id
		                 order by t.revision desc limit 1), '')
		from comment_blocks b
		join comments c on c.id = b.comment_id
		left join issues i on i.key = c.issue_key
		left join artifacts a on a.id = c.artifact_id
		where b.node_id = any($1) and ($2 or b.position is not null)`, ids, includeRemoved)
	if err != nil {
		return nil, fmt.Errorf("load comment block nodes: %w", err)
	}
	defer rows.Close()
	blocks := []commentBlockNode{}
	for rows.Next() {
		var block commentBlockNode
		var nodeID, blockID, commentID, project, slug, wholeText string
		var issueKey *string
		var madeAt *time.Time
		var changedAt time.Time
		var changedRevision int
		var author []byte
		if err := rows.Scan(&nodeID, &blockID, &block.excerpt, &madeAt, &changedRevision, &changedAt, &block.removedAt,
			&commentID, &issueKey, &project, &slug, &author, &wholeText); err != nil {
			return nil, fmt.Errorf("scan comment block node: %w", err)
		}
		authors, err := storedActor(author)
		if err != nil {
			return nil, fmt.Errorf("decode the author of block %s: %w", nodeID, err)
		}
		block.node = model.GraphNode{
			Kind:           "block",
			ID:             nodeID,
			BlockID:        blockID,
			IssueKey:       issueKey,
			Project:        project,
			Ref:            BlockRef(itemRef("comment", issueKey, project, slug, commentID), blockID),
			ChangedVersion: changedRevision,
			MadeBy:         authors,
			MadeAt:         madeAt,
			ChangedBy:      authors,
			ChangedAt:      &changedAt,
			Parent:         itemParent("comment", issueKey, project, slug, commentID),
			ParentVersion:  &changedRevision,
			Text:           wholeText,
		}
		blocks = append(blocks, block)
	}
	return blocks, rows.Err()
}

// loadMessageBlockNodes resolves message block nodes. A message is never edited, so a block is
// made by the message's author at its created_at and has no other change (as loadMessageNodes);
// it belongs to its message, which has no versions. Its Text is re-read from the message body:
// a message never changes, so the body split as the index split it (itemblocks.Split) holds the
// block at its position, which its identity hash confirms; were the parser to read the body
// otherwise since, the block is the first of equal hash, else its stored excerpt stands in.
func loadMessageBlockNodes(ctx context.Context, q Queryer, kind string, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select b.node_id, b.block_id, b.excerpt, b.position, b.text_sha256,
		       m.id::text, m.issue_key, coalesce(i.project_key, ''), m.body, m.author, m.created_at
		from message_blocks b
		join messages m on m.id = b.message_id
		left join issues i on i.key = m.issue_key
		where b.node_id = any($1) and b.position is not null`, ids)
	if err != nil {
		return fmt.Errorf("load message block nodes: %w", err)
	}
	defer rows.Close()
	splits := make(map[string][]itemblocks.Block)
	for rows.Next() {
		var nodeID, blockID, excerpt, sum, messageID, project, body string
		var position int
		var issueKey *string
		var author []byte
		var createdAt time.Time
		if err := rows.Scan(&nodeID, &blockID, &excerpt, &position, &sum, &messageID, &issueKey, &project, &body, &author, &createdAt); err != nil {
			return fmt.Errorf("scan message block node: %w", err)
		}
		blocks, split := splits[messageID]
		if !split {
			blocks = itemblocks.Split(body)
			splits[messageID] = blocks
		}
		node := model.GraphNode{
			Kind:     "block",
			ID:       nodeID,
			BlockID:  blockID,
			IssueKey: issueKey,
			Project:  project,
			Ref:      BlockRef(itemRef("message", issueKey, project, "", messageID), blockID),
			Parent:   itemParent("message", issueKey, project, "", messageID),
			Text:     messageBlockText(blocks, position, sum, excerpt),
		}
		if err := setMadeAndChanged(&node, author, createdAt, nil, nil, storedActor); err != nil {
			return err
		}
		nodes[[2]string{kind, nodeID}] = resolvedNode{GraphNode: node, text: excerpt}
	}
	return rows.Err()
}

// messageBlockText is the whole text of the message block at position whose identity hash is sum,
// from blocks, the message body's split: the block at position when its hash is sum, else the
// first block whose hash is, else excerpt.
func messageBlockText(blocks []itemblocks.Block, position int, sum, excerpt string) string {
	if position >= 0 && position < len(blocks) && itemblocks.Hash(blocks[position].Text) == sum {
		return blocks[position].Text
	}
	for _, block := range blocks {
		if itemblocks.Hash(block.Text) == sum {
			return block.Text
		}
	}
	return excerpt
}

// itemBlockParent is the block node nodeID, a reply's replies_to_block ('<item kind>/<item
// id>#<block id>'), as the reply's parent: a block of an item in the reply's own thread, so
// addressed under the reply's owner.
func itemBlockParent(issueKey *string, project, slug, nodeID string) *model.NodeParent {
	item, blockID, _ := strings.Cut(nodeID, "#")
	kind, id, _ := strings.Cut(item, "/")
	return &model.NodeParent{Kind: "block", ID: nodeID, Ref: BlockRef(itemRef(kind, issueKey, project, slug, id), blockID)}
}

// CommentBlockRevision is what the index recorded of one block of a comment at one revision
// (IndexItem writes comment_block_state and comment_block_texts).
type CommentBlockRevision struct {
	// Current is the comment's current revision, 0 while no block-aware build indexed it.
	Current int
	// Text is the block's whole text at the revision: the latest text recorded at or before it.
	// It is nil when the revision does not hold the block.
	Text *string
	// RemovedRevision is the latest revision that recorded the block removed, or 0.
	RemovedRevision int
}

// CommentBlockAt reads block blockID of comment commentID, a comment's uuid, as revision records
// it. A revision past Current is one the comment has not reached; its Text is still the block's
// latest, which the caller does not read.
func CommentBlockAt(ctx context.Context, q Queryer, commentID, blockID string, revision int) (CommentBlockRevision, error) {
	var read CommentBlockRevision
	var removed *int
	if err := q.QueryRow(ctx, `
		select coalesce((select s.revision from comment_block_state s where s.comment_id = $1::uuid), 0),
		       (select t.text from comment_block_texts t where t.comment_id = $1::uuid and t.block_id = $2 and t.revision <= $3::bigint
		        order by t.revision desc limit 1),
		       (select max(t.revision) from comment_block_texts t where t.comment_id = $1::uuid and t.block_id = $2 and t.text is null)
	`, commentID, blockID, revision).Scan(&read.Current, &read.Text, &removed); err != nil {
		return CommentBlockRevision{}, fmt.Errorf("read block %q of comment %s at revision %d: %w", blockID, commentID, revision, err)
	}
	if removed != nil {
		read.RemovedRevision = *removed
	}
	return read, nil
}

// RemovedCommentBlockNode is the node of comment block nodeID, which no current revision holds
// since revision removedRevision removed it, built as a current block's but with no text or
// changed revision: made as it was made, last changed by that removal (by the comment's author,
// at the block's removed_at), and part of the comment at the revision before it, the last one to
// hold it. found is false when the comment has no such block. The caller adds its pin.
func RemovedCommentBlockNode(ctx context.Context, q Queryer, nodeID string, removedRevision int) (model.GraphNode, bool, error) {
	blocks, err := commentBlockNodes(ctx, q, []string{nodeID}, true)
	if err != nil || len(blocks) == 0 {
		return model.GraphNode{}, false, err
	}
	node := blocks[0].node
	node.Text, node.ChangedVersion, node.ChangedAt, node.ParentVersion = "", 0, blocks[0].removedAt, nil
	if held := removedRevision - 1; held > 0 {
		node.ParentVersion = &held
	}
	return node, true, nil
}
