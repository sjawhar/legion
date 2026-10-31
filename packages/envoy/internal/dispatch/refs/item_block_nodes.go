package refs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// loadCommentBlockNodes resolves the block nodes of comments, stored under kind comment_block and
// addressed 'comment/<comment uuid>#<block id>', as loadItemBlockNodes resolves the blocks of any
// item whose revisions are recorded.
func loadCommentBlockNodes(ctx context.Context, q Queryer, kind string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	return loadItemBlockNodes(ctx, q, ItemComment, kind, ids, nodes)
}

// loadItemBlockNodes resolves the block nodes of items of kind item, stored under that kind's
// storage kind (ItemKind.BlockKind) and addressed '<item kind>/<item uuid>#<block id>', from the
// current blocks of their items. A block no current revision holds resolves to nothing, so every
// edge at it is omitted until a revision holds its id again. The node's kind is block, as a
// document block's is; its parent is its item. It carries no Text: only the node a reference read
// names does (CurrentItemBlockText), as a document block's.
func loadItemBlockNodes(ctx context.Context, q Queryer, item ItemKind, kind string, ids []string, nodes map[[2]string]resolvedNode) error {
	blocks, err := itemBlockNodes(ctx, q, item, ids, false)
	if err != nil {
		return err
	}
	for _, block := range blocks {
		nodes[[2]string{kind, block.node.ID}] = resolvedNode{GraphNode: block.node, text: block.excerpt}
	}
	return nil
}

// itemBlockNode is one block of an item, its stored excerpt, and when a revision removed it
// (nil while it is current).
type itemBlockNode struct {
	node      model.GraphNode
	excerpt   string
	removedAt *time.Time
}

// itemBlockNodes reads the blocks of the item kind whose node ids are ids: current ones, and
// removed ones too when includeRemoved. Only a comment's author edits it, so each of its blocks
// is made and last changed by that author; a block that records its own maker (ask_blocks.made_by,
// which an answer part carries, since whoever answered is not the ask's author) is made and last
// changed by that one, and every other part of an ask by the ask's author. A block is made at the
// time of the revision that first held it (null for one found at the first indexing of an item
// edited before blocks were recorded) and changed at its changed revision's time. It belongs to
// its item at that revision.
//
// Only a kind that records its revisions is read here: a message is never edited and has no
// document of its own, and its blocks are resolved by loadMessageBlockNodes.
func itemBlockNodes(ctx context.Context, q Queryer, kind ItemKind, ids []string, includeRemoved bool) ([]itemBlockNode, error) {
	tables, err := kind.tables()
	if err != nil {
		return nil, err
	}
	if !tables.revisions() {
		return nil, fmt.Errorf("read the block nodes of a %q: its revisions are not recorded", string(kind))
	}
	maker := "t.author"
	if tables.makers {
		maker = "coalesce(b.made_by, t.author)"
	}
	rows, err := q.Query(ctx, `
		select b.node_id, b.block_id, b.excerpt, b.made_at, b.changed_revision, b.changed_at, b.removed_at,
		       t.id::text, t.issue_key, coalesce(i.project_key, a.project_key, ''), coalesce(a.slug, ''), `+maker+`
		from `+tables.blocks+` b
		join `+tables.items+` t on t.id = b.`+tables.item+`
		left join issues i on i.key = t.issue_key
		left join artifacts a on a.id = t.artifact_id
		where b.node_id = any($1) and ($2 or b.position is not null)`, ids, includeRemoved)
	if err != nil {
		return nil, fmt.Errorf("load %s block nodes: %w", string(kind), err)
	}
	defer rows.Close()
	blocks := []itemBlockNode{}
	for rows.Next() {
		var block itemBlockNode
		var nodeID, blockID, itemID, project, slug string
		var issueKey *string
		var madeAt *time.Time
		var changedAt time.Time
		var changedRevision int
		var author []byte
		if err := rows.Scan(&nodeID, &blockID, &block.excerpt, &madeAt, &changedRevision, &changedAt, &block.removedAt,
			&itemID, &issueKey, &project, &slug, &author); err != nil {
			return nil, fmt.Errorf("scan %s block node: %w", string(kind), err)
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
			Ref:            BlockRef(itemRef(string(kind), issueKey, project, slug, itemID), blockID),
			ChangedVersion: changedRevision,
			MadeBy:         authors,
			MadeAt:         madeAt,
			ChangedBy:      authors,
			ChangedAt:      &changedAt,
			Parent:         itemParent(string(kind), issueKey, project, slug, itemID),
			ParentVersion:  &changedRevision,
		}
		blocks = append(blocks, block)
	}
	return blocks, rows.Err()
}

// loadMessageBlockNodes resolves the block nodes of messages, stored under kind message_block and
// addressed 'message/<message uuid>#<block id>', from the current blocks of their messages, as
// loadItemBlockNodes resolves a comment's. A message is never edited, so a block is made by the
// message's author at its created_at and has no other change (as loadMessageNodes); it belongs to
// its message, which has no versions, and carries no Text (CurrentItemBlockText). A direct
// message has no address, so its blocks resolve to no node: no reference names one, and an edge
// at one is omitted as at any node that does not exist; GET /api/v1/messages/{id}/blocks reads
// them under the conversation's read rule.
func loadMessageBlockNodes(ctx context.Context, q Queryer, kind string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select b.node_id, b.block_id, b.excerpt, m.id::text, m.issue_key, coalesce(i.project_key, ''), m.author, m.created_at
		from message_blocks b
		join messages m on m.id = b.message_id
		left join issues i on i.key = m.issue_key
		where b.node_id = any($1) and b.position is not null and m.issue_key is not null`, ids)
	if err != nil {
		return fmt.Errorf("load message block nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID, blockID, excerpt, messageID, project string
		var issueKey *string
		var author []byte
		var createdAt time.Time
		if err := rows.Scan(&nodeID, &blockID, &excerpt, &messageID, &issueKey, &project, &author, &createdAt); err != nil {
			return fmt.Errorf("scan message block node: %w", err)
		}
		node := model.GraphNode{
			Kind:     "block",
			ID:       nodeID,
			BlockID:  blockID,
			IssueKey: issueKey,
			Project:  project,
			Ref:      BlockRef(itemRef("message", issueKey, project, "", messageID), blockID),
			Parent:   itemParent("message", issueKey, project, "", messageID),
		}
		if err := setMadeAndChanged(&node, author, createdAt, nil, nil, storedActor); err != nil {
			return err
		}
		nodes[[2]string{kind, nodeID}] = resolvedNode{GraphNode: node, text: excerpt}
	}
	return rows.Err()
}

// CurrentItemBlockText is the whole current text of block nodeID of an item of kind, the text a
// reference read's node carries. A comment block's and an ask part's is the latest text the
// kind's texts table records for it, "" once removed. A message block's is re-read from the
// message body: a message never changes, so the body split as the index split it
// (itemblocks.Split) holds the block at its position, which its identity hash confirms; were the
// parser to read the body otherwise since, the block is the first of equal hash, else its stored
// excerpt stands in. A block the item does not hold has "".
func CurrentItemBlockText(ctx context.Context, q Queryer, kind ItemKind, nodeID string) (string, error) {
	tables, err := kind.tables()
	if err != nil {
		return "", err
	}
	if tables.revisions() {
		var current string
		if err := q.QueryRow(ctx, `
			select coalesce((select t.text from `+tables.blocks+` b
			                 join `+tables.texts+` t on t.`+tables.item+` = b.`+tables.item+` and t.block_id = b.block_id
			                 where b.node_id = $1 order by t.revision desc limit 1), '')`, nodeID).Scan(&current); err != nil {
			return "", fmt.Errorf("read the text of %s block %s: %w", string(kind), nodeID, err)
		}
		return current, nil
	}
	var position int
	var sum, excerpt, body string
	err = q.QueryRow(ctx, `
		select b.position, b.text_sha256, b.excerpt, m.body
		from message_blocks b join messages m on m.id = b.message_id
		where b.node_id = $1 and b.position is not null`, nodeID).Scan(&position, &sum, &excerpt, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the text of message block %s: %w", nodeID, err)
	}
	return messageBlockText(itemblocks.Split(body), position, sum, excerpt), nil
}

// messageBlockText is the whole text of the message block at position whose identity hash is sum,
// from blocks, the message body's split: the block at position when its hash is sum, else the
// first block whose hash is, else excerpt.
func messageBlockText(blocks []itemblocks.Block, position int, sum, excerpt string) string {
	if position >= 0 && position < len(blocks) && blocks[position].Hash == sum {
		return blocks[position].Text
	}
	for _, block := range blocks {
		if block.Hash == sum {
			return block.Text
		}
	}
	return excerpt
}

// itemBlockParent is the block node nodeID, a reply's replies_to_block ('<item kind>/<item
// id>#<block id>', read by ParseItemBlockNode), as the reply's parent: a block of an item in the
// reply's own thread, so addressed under the reply's owner.
func itemBlockParent(issueKey *string, project, slug, nodeID string) *model.NodeParent {
	kind, itemID, blockID, _ := ParseItemBlockNode(nodeID)
	return &model.NodeParent{Kind: "block", ID: nodeID, Ref: BlockRef(itemRef(string(kind), issueKey, project, slug, itemID), blockID)}
}

// ItemBlockRevision is what the index recorded of one block of an item at one revision: a
// comment's block (IndexItem writes comment_block_state and comment_block_texts) or a part of an
// ask (IndexAskParts writes ask_block_state and ask_block_texts).
type ItemBlockRevision struct {
	// Current is the item's current revision, 0 while no block-aware build indexed it.
	Current int
	// Text is the block's whole text at the revision: the latest text recorded at or before it.
	// It is nil when the revision does not hold the block.
	Text *string
	// RemovedRevision is the latest revision that recorded the block removed, or 0.
	RemovedRevision int
}

// ItemBlockAt reads block blockID of the item of kind whose uuid is itemID as revision records
// it: a block of a comment as that revision of the comment records it, a part of an ask as that
// revision of the ask's parts does, over the kind's state and texts tables, where the text at
// revision N is the latest row with revision <= N. A revision past Current is one the item has
// not reached; its Text is still the block's latest, which the caller does not read.
func ItemBlockAt(ctx context.Context, q Queryer, kind ItemKind, itemID, blockID string, revision int) (ItemBlockRevision, error) {
	tables, err := kind.tables()
	if err != nil {
		return ItemBlockRevision{}, err
	}
	var read ItemBlockRevision
	var removed *int
	if err := q.QueryRow(ctx, `
		select coalesce((select s.revision from `+tables.state+` s where s.`+tables.item+` = $1::uuid), 0),
		       (select t.text from `+tables.texts+` t where t.`+tables.item+` = $1::uuid and t.block_id = $2 and t.revision <= $3::bigint
		        order by t.revision desc limit 1),
		       (select max(t.revision) from `+tables.texts+` t where t.`+tables.item+` = $1::uuid and t.block_id = $2 and t.text is null)
	`, itemID, blockID, revision).Scan(&read.Current, &read.Text, &removed); err != nil {
		return ItemBlockRevision{}, fmt.Errorf("read block %q of %s %s at revision %d: %w", blockID, string(kind), itemID, revision, err)
	}
	if removed != nil {
		read.RemovedRevision = *removed
	}
	return read, nil
}

// RemovedItemBlockNode is the node of block nodeID of an item of kind — a block of a comment, a
// part of an ask — which no current revision holds since revision removedRevision removed it,
// built as a current block's but with no changed revision: made as it was made, last changed by
// that removal (by the block's maker, at the block's removed_at), and part of the item at the
// revision before it, the last one to hold it. found is false when the item has no such block.
// The caller adds its pin.
func RemovedItemBlockNode(ctx context.Context, q Queryer, kind ItemKind, nodeID string, removedRevision int) (model.GraphNode, bool, error) {
	blocks, err := itemBlockNodes(ctx, q, kind, []string{nodeID}, true)
	if err != nil || len(blocks) == 0 {
		return model.GraphNode{}, false, err
	}
	node := blocks[0].node
	node.ChangedVersion, node.ChangedAt, node.ParentVersion = 0, blocks[0].removedAt, nil
	if held := removedRevision - 1; held > 0 {
		node.ParentVersion = &held
	}
	return node, true, nil
}
