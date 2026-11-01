package docs

import (
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// applyOperations applies ops to a plain tree, each operation to its predecessor's output, as
// applyBatch applies them. The edit paths hold a batch tree and the token their precondition check
// encoded (applyBatch's own arguments); a test holds a tree alone, so this is where that shape
// lives now.
func applyOperations(tree *pmdoc.Node, ops []model.EditOp) (editBatch, error) {
	return applyBatch(newBatchTree(tree), ops, nil, "")
}
