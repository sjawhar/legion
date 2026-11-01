package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// replyBlockInAskThread is the refusal of a reply in an ask's thread that names a block: an ask's
// thread has no blocks to answer until INFRA-369 gives an ask's replies blocks of their own.
const replyBlockInAskThread = "a reply in an ask's thread names no block yet"

// namedBlock is the block of its parent a reply names (reply_to_block, in_reply_to_block): the
// node id replies_to_block stores, the item holding the block, the block's own id and its
// stored excerpt.
type namedBlock struct {
	node    string
	item    string
	id      string
	excerpt string
}

// replyBlock is the block as a reply carries it (model.ReplyBlock), for a write whose reply row
// is queued and so has nothing for the read of a stored reply to join (refs.ReplyBlocks). The
// block is one the item holds now, so it is not removed; issueKey, project and slug address the
// item as that read addresses it, under the reply's owner (ownerAddress).
func (block *namedBlock) replyBlock(kind refs.ItemKind, issueKey *string, project, slug string) *model.ReplyBlock {
	if block == nil {
		return nil
	}
	return &model.ReplyBlock{
		Node:    block.node,
		Ref:     refs.ItemBlockRef(kind, issueKey, project, slug, block.item, block.id),
		BlockID: block.id,
		Text:    block.excerpt,
	}
}

// ownerAddress is how refs addresses an item under owner: an issue's comments and messages by
// its key, a document's comments by its project and slug (refs.ItemBlockRef, refs.ReplyBlocks).
func (s *server) ownerAddress(ctx context.Context, tx pgx.Tx, owner owner) (issueKey *string, project, slug string, err error) {
	if owner.ArtifactID == nil {
		return owner.IssueKey, "", "", nil
	}
	if err := tx.QueryRow(ctx, `
		select coalesce(project_key, ''), slug from artifacts where id = $1
	`, *owner.ArtifactID).Scan(&project, &slug); err != nil {
		return nil, "", "", fmt.Errorf("load the address of document %s: %w", *owner.ArtifactID, err)
	}
	return owner.IssueKey, project, slug, nil
}

// planReplyBlock checks that blockID is a current block of item itemID, the comment or message a
// reply answers, and returns it with the index the item owes, which the caller queues in its
// write phase. The caller holds the item's owner (when it has one) and, for a message, the item
// row; an item an older build wrote, or wrote after its last index, is indexed by the same
// transaction (refs.PlanItemIfOwed), so the block named is one of the item's current body: the
// repaired index is read from its plan, the stored one from the block rows. A block the item
// does not hold now answers 400 with code, naming field and the block.
func (s *server) planReplyBlock(
	ctx context.Context, tx pgx.Tx, kind refs.ItemKind, itemID, blockID, field, code string,
) (refs.ItemRepair, *namedBlock, error) {
	repair, err := refs.PlanItemIfOwed(ctx, tx, kind, itemID, s.deps.ServerURL)
	if err != nil {
		return refs.ItemRepair{}, nil, err
	}
	node, excerpt, found := "", "", false
	if repair.Owed() {
		node, excerpt, found = repair.Block(blockID)
	} else if node, excerpt, found, err = refs.CurrentItemBlock(ctx, tx, kind, itemID, blockID); err != nil {
		return refs.ItemRepair{}, nil, err
	}
	if !found {
		return refs.ItemRepair{}, nil, errorf(http.StatusBadRequest, code, "%s %s is not a block of %s %s", field, blockID, kind, itemID)
	}
	return repair, &namedBlock{node: node, item: itemID, id: blockID, excerpt: excerpt}, nil
}

// replyBlockOf is planReplyBlock with the item's owed index queued on ledger, at the point the
// reply reads the block, or run at once where the transaction queues no write phase (a nil
// ledger: a route that writes as it goes, the delivery replies).
func (s *server) replyBlockOf(
	ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, kind refs.ItemKind, itemID, blockID, field, code string,
) (*namedBlock, error) {
	repair, block, err := s.planReplyBlock(ctx, tx, kind, itemID, blockID, field, code)
	if err != nil {
		return nil, err
	}
	if !repair.Owed() {
		return block, nil
	}
	return block, ledger.Queue(func(b *store.WriteBatch) error {
		repair.Queue(b)
		return nil
	})
}

// nodeID is the block's node id for replies_to_block, null for a reply naming no block.
func (block *namedBlock) nodeID() *string {
	if block == nil {
		return nil
	}
	return &block.node
}

// queueReplyParentMessageLock queues the lock of message id, the parent a reply names a block of,
// `for no key update` as the first write of the reply's write phase, after its issue's owner
// lock, the order the accept route takes them in, so the index of that parent the transaction
// may queue (planReplyBlock) and the reply's own writes run under it. It is not FOR UPDATE: the
// reply's insert then takes the row FOR KEY SHARE through its foreign key, which this lock does
// not block.
func queueReplyParentMessageLock(b *store.WriteBatch, id string) {
	b.Queue("", `select 1 from messages where id = $1 for no key update`, id)
}

// lockReplyParentMessage takes queueReplyParentMessageLock's lock at once, in a transaction that
// writes as it goes (the accept route).
func lockReplyParentMessage(ctx context.Context, tx pgx.Tx, id string) error {
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		queueReplyParentMessageLock(b, id)
		return nil
	})
}

// hydrateCommentReplyBlocks sets each comment's ReplyBlock, the block of its parent it answers
// (null when it names none), in one query over every comment that names one (RepliesToBlock),
// however many there are, and none when no comment does. A comment that already carries its
// block is left alone: its writer read that block before its row was written, and its insert may
// still be queued, where this read would find nothing (namedBlock.replyBlock).
func hydrateCommentReplyBlocks(ctx context.Context, q queryer, comments ...*model.Comment) error {
	ids := make([]string, 0, len(comments))
	for _, comment := range comments {
		if comment.RepliesToBlock != nil && comment.ReplyBlock == nil {
			ids = append(ids, comment.ID)
		}
	}
	blocks, err := refs.ReplyBlocks(ctx, q, refs.ItemComment, ids)
	if err != nil {
		return err
	}
	for _, comment := range comments {
		if block, read := blocks[comment.ID]; read {
			comment.ReplyBlock = block
		}
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
