package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// replyBlockOwner is the kind of item the block a reply names belongs to, read from the node id
// replies_to_block stores (refs.ParseItemBlockNode): `ask/<id>#<part>` is a part of an ask,
// which a reply naming that ask names, and `comment/<id>#<block>` a block of the comment the
// reply answers. A node id of no shape this build knows is read as a comment's, the kind every
// reply written before ask parts named.
func replyBlockOwner(nodeID string) refs.ItemKind {
	if kind, _, _, ok := refs.ParseItemBlockNode(nodeID); ok {
		return kind
	}
	return refs.ItemComment
}

// namedBlock is the block of its parent a reply names (reply_to_block, in_reply_to_block): the
// kind of item holding it, since a comment's reply may name a block of the comment it answers or
// a part of the ask it was asked under, the node id replies_to_block stores, the item holding the
// block, the block's own id and its stored excerpt.
type namedBlock struct {
	kind    refs.ItemKind
	node    string
	item    string
	id      string
	excerpt string
}

// replyBlock is the block as a reply carries it (model.ReplyBlock), for a write whose reply row
// is queued and so has nothing for the read of a stored reply to join (refs.ReplyBlocks). The
// block is one the item holds now, so it is not removed; the item's own kind addresses it, with
// issueKey, project and slug, as that read addresses it, under the reply's owner (ownerAddress).
func (block *namedBlock) replyBlock(issueKey *string, project, slug string) *model.ReplyBlock {
	if block == nil {
		return nil
	}
	return &model.ReplyBlock{
		Node:    block.node,
		Ref:     refs.ItemBlockRef(block.kind, issueKey, project, slug, block.item, block.id),
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

// planReplyBlock checks that blockID is a current block of item itemID — the comment or the
// message a reply answers, or the ask a reply names, whose blocks are the parts of its question,
// its options and its answer — and returns it with the index the item owes, which the caller
// queues in its write phase. The caller holds the item's owner (when it has one) and, for a
// message, the item row; an item an older build wrote, or wrote after its last index, is indexed
// by the same transaction (refs.PlanItemIfOwed), so the block named is one the item holds now:
// the repaired index is read from its plan, the stored one from the block rows. A block the item
// does not hold now answers 400 with code, naming field and the block.
//
// The live write of this request is the reply, which its own route stamps; the item indexed here
// is the parent, whose text an earlier write already carried an event for, so a block mention
// this records takes the source_seq of the parent's own refs row for its target at the item's
// first indexing (refs.PlanItemIndex) and none after it, and either way it is no new fact of this
// request.
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
	return repair, &namedBlock{kind: kind, node: node, item: itemID, id: blockID, excerpt: excerpt}, nil
}

// replyBlockOf is planReplyBlock with the item's owed index queued on ledger, at the point the
// reply reads the block, or run at once where the transaction queues no write phase (a nil
// ledger: a route that writes as it goes, the delivery replies). A nil ledger has no queue to
// hold the repair until a write phase that never comes, so the repair runs there as every
// unqueued repair runs (refs.ItemRepair.Write), as a batch of its own before the reply's own
// writes.
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
	if ledger != nil {
		return block, ledger.Queue(func(b *store.WriteBatch) error {
			repair.Queue(b)
			return nil
		})
	}
	if err := repair.Write(ctx, tx); err != nil {
		return nil, err
	}
	return block, nil
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
// writes as it goes (replyMessage, a session's reply to a message delivery).
func lockReplyParentMessage(ctx context.Context, tx pgx.Tx, id string) error {
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		queueReplyParentMessageLock(b, id)
		return nil
	})
}

// hydrateCommentReplyBlocks sets each comment's ReplyBlock, the block of its parent it answers
// (null when it names none), in one query per kind of item the named blocks belong to: a reply
// under an ask names a part of that ask, every other reply a block of the comment it answers
// (replyBlockOwner), so a page of either costs one query and a page of both two. A comment that
// already carries its block is left alone: its writer read that block before its row was
// written, and its insert may still be queued, where this read would find nothing
// (namedBlock.replyBlock).
func hydrateCommentReplyBlocks(ctx context.Context, q queryer, comments ...*model.Comment) error {
	ids := map[refs.ItemKind][]string{}
	for _, comment := range comments {
		if comment.RepliesToBlock != nil && comment.ReplyBlock == nil {
			owner := replyBlockOwner(*comment.RepliesToBlock)
			ids[owner] = append(ids[owner], comment.ID)
		}
	}
	blocks := make(map[string]*model.ReplyBlock, len(ids))
	for owner, owned := range ids {
		found, err := refs.ReplyBlocks(ctx, q, owner, owned)
		if err != nil {
			return err
		}
		maps.Copy(blocks, found)
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
