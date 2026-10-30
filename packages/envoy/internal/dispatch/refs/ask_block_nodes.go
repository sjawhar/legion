package refs

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// loadAskBlockNodes resolves the part nodes of asks, stored under kind ask_block and addressed
// 'ask/<ask uuid>#<part id>', as loadItemBlockNodes resolves a comment's blocks: a part no current
// revision holds resolves to nothing, the node's kind is block and its parent is its ask, at the
// revision the part's text last changed in. A question and an option are the ask's author's; the
// answer part is the answerer's, who is not the ask's author (ask_blocks.made_by). It carries no
// Text: only the node a reference read names does (CurrentItemBlockText).
func loadAskBlockNodes(ctx context.Context, q Queryer, kind string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	return loadItemBlockNodes(ctx, q, ItemAsk, kind, ids, nodes)
}

// AskPartAt reads part blockID of ask askID, an ask's uuid, as revision of its parts records it
// (itemBlockAt over ask_block_state and ask_block_texts, where the text at revision N is the
// latest row with revision <= N). A revision past Current is one the ask has not reached.
func AskPartAt(ctx context.Context, q Queryer, askID, blockID string, revision int) (ItemBlockRevision, error) {
	return itemBlockAt(ctx, q, ItemAsk, askID, blockID, revision)
}

// RemovedAskBlockNode is the node of ask part nodeID, which no current revision holds since
// revision removedRevision removed it (removedItemBlockNode). found is false when the ask has no
// such part. The caller adds its pin.
func RemovedAskBlockNode(ctx context.Context, q Queryer, nodeID string, removedRevision int) (model.GraphNode, bool, error) {
	return removedItemBlockNode(ctx, q, ItemAsk, nodeID, removedRevision)
}
