package pmdoc

import (
	"sync"

	"github.com/google/uuid"
)

const BlockIDAttr = "blockId"

var blockIDGenerator = struct {
	sync.RWMutex
	mint func() string
}{mint: uuid.NewString}

// SetBlockIDGenerator replaces the block ID generator. Passing nil restores UUID v4 IDs.
// Fixture generators and tests use this to make generated documents deterministic.
func SetBlockIDGenerator(mint func() string) {
	blockIDGenerator.Lock()
	defer blockIDGenerator.Unlock()
	if mint == nil {
		blockIDGenerator.mint = uuid.NewString
		return
	}
	blockIDGenerator.mint = mint
}

func mintBlockID() string {
	blockIDGenerator.RLock()
	mint := blockIDGenerator.mint
	blockIDGenerator.RUnlock()
	return mint()
}

// EnsureBlockIDs gives every block in tree a unique blockId. Existing unique IDs stay
// unchanged; on duplicate IDs, the first document-order occurrence keeps its identity.
func EnsureBlockIDs(tree *Node) bool {
	return EnsureBlockIDsCount(tree) > 0
}

// BlockIDRepairCount reports how many block IDs EnsureBlockIDs would mint.
func BlockIDRepairCount(tree *Node) (repairs int) {
	seen := make(map[string]struct{})
	walk(tree, func(node *Node, _ []int, _, _ int) bool {
		if node.Type == "doc" || isInlineNodeType(node.Type) {
			return true
		}
		id, ok := node.Attrs[BlockIDAttr].(string)
		if ok && id != "" {
			if _, duplicate := seen[id]; !duplicate {
				seen[id] = struct{}{}
				return true
			}
		}
		repairs++
		return true
	})
	return repairs
}

// EnsureBlockIDsCount gives every block in tree a unique blockId and reports
// how many missing or duplicate IDs it replaced.
func EnsureBlockIDsCount(tree *Node) (stamped int) {
	seen := make(map[string]struct{})
	walk(tree, func(node *Node, _ []int, _, _ int) bool {
		if node.Type == "doc" || isInlineNodeType(node.Type) {
			return true
		}
		id, ok := node.Attrs[BlockIDAttr].(string)
		if ok && id != "" {
			if _, duplicate := seen[id]; !duplicate {
				seen[id] = struct{}{}
				return true
			}
		}
		if node.Attrs == nil {
			node.Attrs = Attrs{}
		}
		for {
			id = mintBlockID()
			if _, duplicate := seen[id]; !duplicate && id != "" {
				break
			}
		}
		node.Attrs[BlockIDAttr] = id
		seen[id] = struct{}{}
		stamped++
		return true
	})
	return stamped
}
