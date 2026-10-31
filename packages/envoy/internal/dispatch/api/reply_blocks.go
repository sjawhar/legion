package api

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// replyBlockInAskThread is the refusal of a reply in an ask's thread that names a block: an ask's
// thread has no blocks to answer until INFRA-369 gives an ask's replies blocks of their own.
const replyBlockInAskThread = "a reply in an ask's thread names no block yet"

// namedBlock is the block of its parent a reply names (reply_to_block, in_reply_to_block): the
// node id replies_to_block stores, and the block's stored excerpt.
type namedBlock struct {
	node    string
	excerpt string
}

// replyBlockOf checks that blockID is a current block of item itemID, the comment or message a
// reply answers, and returns it. The caller holds the item's owner row (when it has one) and the
// item row; an item an older build wrote, or wrote after its last index, is indexed first, in the
// caller's transaction (refs.IndexItemIfOwed), so the block named is one of the item's current
// body. A block the item does not hold now answers 400 with code, naming field and the block.
func (s *server) replyBlockOf(
	ctx context.Context, tx pgx.Tx, kind refs.ItemKind, itemID, blockID, field, code string,
) (*namedBlock, error) {
	if _, err := refs.IndexItemIfOwed(ctx, tx, kind, itemID, s.deps.ServerURL); err != nil {
		return nil, err
	}
	node, excerpt, found, err := refs.CurrentItemBlock(ctx, tx, kind, itemID, blockID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errorf(http.StatusBadRequest, code, "%s %s is not a block of %s %s", field, blockID, kind, itemID)
	}
	return &namedBlock{node: node, excerpt: excerpt}, nil
}

// nodeID is the block's node id for replies_to_block, null for a reply naming no block.
func (block *namedBlock) nodeID() *string {
	if block == nil {
		return nil
	}
	return &block.node
}

// lockReplyParentMessage takes message id, the parent a reply names a block of, `for no key update`
// after its issue, the order the accept route takes it in, so replyBlockOf indexes it under the
// lock and IndexItem takes its state row after it. It is not FOR UPDATE: the reply's insert then
// takes the row FOR KEY SHARE through its foreign key, which this lock does not block.
func lockReplyParentMessage(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `select 1 from messages where id = $1 for no key update`, id)
	return err
}

// hydrateCommentReplyBlocks sets each comment's ReplyBlock, the block of its parent it answers
// (null when it names none), in one query over every comment that names one (RepliesToBlock),
// however many there are, and none when no comment does.
func hydrateCommentReplyBlocks(ctx context.Context, q queryer, comments ...*model.Comment) error {
	ids := make([]string, 0, len(comments))
	for _, comment := range comments {
		if comment.RepliesToBlock != nil {
			ids = append(ids, comment.ID)
		}
	}
	blocks, err := refs.ReplyBlocks(ctx, q, refs.ItemComment, ids)
	if err != nil {
		return err
	}
	for _, comment := range comments {
		comment.ReplyBlock = blocks[comment.ID]
	}
	return nil
}

// hydrateMessageReplyBlocks sets each message's ReplyBlock, the block of the message it answers
// (null when it names none), in one query over every message that names one (RepliesToBlock),
// however many there are, and none when no message does.
func hydrateMessageReplyBlocks(ctx context.Context, q queryer, messages ...*model.Message) error {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		if message.RepliesToBlock != nil {
			ids = append(ids, message.ID)
		}
	}
	blocks, err := refs.ReplyBlocks(ctx, q, refs.ItemMessage, ids)
	if err != nil {
		return err
	}
	for _, message := range messages {
		message.ReplyBlock = blocks[message.ID]
	}
	return nil
}

// hydrateThreadReplyBlocks sets ReplyBlock on every message of the threads roots head: each root
// (a message read by id may be a reply itself) and each reply chains holds under it, in one query.
func hydrateThreadReplyBlocks(ctx context.Context, q queryer, roots []model.Message, chains map[string][]model.Message) error {
	messages := make([]*model.Message, 0, len(roots))
	for index := range roots {
		messages = append(messages, &roots[index])
		chain := chains[roots[index].ID]
		for reply := range chain {
			messages = append(messages, &chain[reply])
		}
	}
	return hydrateMessageReplyBlocks(ctx, q, messages...)
}
