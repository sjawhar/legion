package pmdoc

import (
	"fmt"
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

// RepeatedBlockID reports the first block id, in next's document order, that next carries on two
// or more blocks and on more blocks than live does, or nil. next is the document a write would
// store over live; live is nil for a document with none before it. A repeat live already carries
// is the document's own - a browser write can leave one until settlement repairs it - and not the
// write's to answer for. Any other is refused rather than left to EnsureBlockIDs, which keeps the
// id for the first holder in document order: the write would move the id, and the ask row or
// anchor keyed on it, onto whichever of the blocks comes first. Only a typed block's markdown can
// name its id, so what this finds is markdown naming one id twice, or naming an id live holds.
func RepeatedBlockID(live, next *Node) error {
	held := blockIDCounts(live)
	written := blockIDCounts(next)
	var repeated error
	walkBlockIDs(next, func(id string) bool {
		if written[id] > 1 && written[id] > held[id] {
			repeated = fmt.Errorf("block id %q would name two blocks; give one of them another id, or omit {#%s} to have one minted", id, id)
			return false
		}
		return true
	})
	return repeated
}

func blockIDCounts(tree *Node) map[string]int {
	counts := make(map[string]int)
	walkBlockIDs(tree, func(id string) bool {
		counts[id]++
		return true
	})
	return counts
}

// walkBlockIDs visits the id of every block in tree that carries one, in document order, until
// visit returns false.
func walkBlockIDs(tree *Node, visit func(id string) bool) {
	Walk(tree, func(node *Node) bool {
		if node.Type == "doc" || isInlineNodeType(node.Type) {
			return true
		}
		if id, _ := node.Attrs[BlockIDAttr].(string); id != "" {
			return visit(id)
		}
		return true
	})
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

// BlockIDForRange returns the lowest block that contains all of r. A range
// spanning top-level siblings has only the document root in common, which has
// no usable block identity, so it returns an empty ID without an error.
func BlockIDForRange(tree *Node, r Range) (string, error) {
	if tree == nil || tree.Type != "doc" {
		return "", ErrTargetNotFound
	}
	var blockID string
	walk(tree, func(node *Node, _ []int, pos, end int) bool {
		if node.Type == "doc" || isInlineNodeType(node.Type) || r.From < pos || r.To > end {
			return true
		}
		if id, ok := node.Attrs[BlockIDAttr].(string); ok && id != "" {
			blockID = id
		}
		return true
	})
	return blockID, nil
}
