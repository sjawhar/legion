package refs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// IndexItemIfOwed indexes comment or message itemID in the caller's transaction when its blocks
// are owed: its state row is missing (an older build wrote it before the state existed), owed
// (owed_at set by an older build's write, or by rebuild-refs), or indexed from another body than
// the stored one. It indexes as ReindexOwedItem does: the blocks and the mentions each holds, and
// for a comment or an issue message its own refs, at the time its body was written; a direct
// message writes no mention. A reply that names a block of the item calls it first, so the block
// it names is one of the item's current body; the block-index reconciliation drains owed items
// through it (IndexOwedItemBlocks). The caller holds the item's owner row (the reconciliation
// holds none) and the item row; IndexItem takes the state row after them. It reports whether the
// item was owed and so indexed.
func IndexItemIfOwed(ctx context.Context, tx pgx.Tx, kind ItemKind, itemID, serverURL string) (bool, error) {
	var query string
	switch kind {
	case ItemComment:
		query = `
			select c.body, coalesce(c.edited_at, c.created_at), true,
			       s.comment_id is not null, s.owed_at is not null, s.body_sha256
			from comments c left join comment_block_state s on s.comment_id = c.id
			where c.id = $1::uuid`
	case ItemMessage:
		query = `
			select m.body, m.created_at, m.issue_key is not null,
			       s.message_id is not null, s.owed_at is not null, s.body_sha256
			from messages m left join message_block_state s on s.message_id = m.id
			where m.id = $1::uuid`
	default:
		return false, fmt.Errorf("index the blocks of %s %s: not an item kind", kind, itemID)
	}
	var body string
	var writtenAt time.Time
	var mentions, stated, owed bool
	var sum *string
	if err := tx.QueryRow(ctx, query, itemID).Scan(&body, &writtenAt, &mentions, &stated, &owed, &sum); err != nil {
		return false, fmt.Errorf("read the block state of %s %s: %w", kind, itemID, err)
	}
	if stated && !owed && sum != nil && *sum == itemblocks.BodySHA256(body) {
		return false, nil
	}
	var located []text.Located
	if mentions {
		located = text.ExtractAt(body, serverURL)
	}
	if _, err := IndexItem(ctx, tx, kind, itemID, body, writtenAt, located, nil); err != nil {
		return false, fmt.Errorf("index the blocks of %s %s: %w", kind, itemID, err)
	}
	if !mentions {
		return true, nil
	}
	if _, err := replaceRefs(ctx, tx, string(kind), itemID, text.Refs(located), &writtenAt); err != nil {
		return false, fmt.Errorf("index %s %s: %w", kind, itemID, err)
	}
	return true, nil
}

// CurrentItemBlock is block blockID of comment or message itemID while a current revision of the
// item holds it: its node id and stored excerpt. found is false when the item holds no such block
// now, a removed one included.
func CurrentItemBlock(ctx context.Context, q Queryer, kind ItemKind, itemID, blockID string) (nodeID, excerpt string, found bool, err error) {
	tables, err := kind.tables()
	if err != nil {
		return "", "", false, err
	}
	err = q.QueryRow(ctx, `
		select node_id, excerpt from `+tables.blocks+`
		where `+tables.item+` = $1::uuid and block_id = $2 and position is not null
	`, itemID, blockID).Scan(&nodeID, &excerpt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("read block %s of %s %s: %w", blockID, kind, itemID, err)
	}
	return nodeID, excerpt, true, nil
}

// ReplyBlocks reads the block each of the replies replyIDs (comments or messages, by kind) names
// in replies_to_block, keyed by reply id, in one query; a reply that names no block has no entry.
// A block belongs to an item in its reply's own thread, so its reference is addressed under the
// reply's owner (as itemBlockParent addresses the reply's parent); a direct message's block has
// none. Text is the block's stored excerpt, which a removal keeps, so a removed block reads the
// last text it held.
func ReplyBlocks(ctx context.Context, q Queryer, kind ItemKind, replyIDs []string) (map[string]*model.ReplyBlock, error) {
	blocks := make(map[string]*model.ReplyBlock)
	if len(replyIDs) == 0 {
		return blocks, nil
	}
	var query string
	switch kind {
	case ItemComment:
		query = `
			select r.id::text, b.node_id, b.block_id, b.excerpt, b.position is null, b.comment_id::text,
			       r.issue_key, coalesce(i.project_key, a.project_key, ''), coalesce(a.slug, '')
			from comments r
			join comment_blocks b on b.node_id = r.replies_to_block
			left join issues i on i.key = r.issue_key
			left join artifacts a on a.id = r.artifact_id
			where r.id = any($1::uuid[]) and r.replies_to_block is not null`
	case ItemMessage:
		// A message has an address only under its issue, so it needs neither project nor slug.
		query = `
			select r.id::text, b.node_id, b.block_id, b.excerpt, b.position is null, b.message_id::text,
			       r.issue_key, '', ''
			from messages r
			join message_blocks b on b.node_id = r.replies_to_block
			where r.id = any($1::uuid[]) and r.replies_to_block is not null`
	default:
		return nil, fmt.Errorf("read reply blocks of %s: not an item kind", kind)
	}
	rows, err := q.Query(ctx, query, replyIDs)
	if err != nil {
		return nil, fmt.Errorf("read the blocks %s replies name: %w", kind, err)
	}
	defer rows.Close()
	for rows.Next() {
		var replyID, itemID, project, slug string
		var issueKey *string
		block := model.ReplyBlock{}
		if err := rows.Scan(&replyID, &block.Node, &block.BlockID, &block.Text, &block.Removed, &itemID, &issueKey, &project, &slug); err != nil {
			return nil, fmt.Errorf("scan the block a %s reply names: %w", kind, err)
		}
		block.Ref = BlockRef(itemRef(string(kind), issueKey, project, slug, itemID), block.BlockID)
		blocks[replyID] = &block
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the blocks %s replies name: %w", kind, err)
	}
	return blocks, nil
}
