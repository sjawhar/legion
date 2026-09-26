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

// RepeatedBlockIDError is a write that would put one block id on two blocks.
type RepeatedBlockIDError struct{ ID string }

func (e *RepeatedBlockIDError) Error() string {
	return fmt.Sprintf("block id %q would name two blocks; give one of them another id, or omit {#%s} to have one minted", e.ID, e.ID)
}

// RepeatedBlockID reports the first block id that two blocks across trees carry, read in order,
// or nil. Only a typed block's markdown can name its id, so this is how a write finds one it is
// about to repeat: an inserted fragment against the document it goes into, or markdown against
// itself (ParseForWrite). A repeated id is not a repair to leave to EnsureBlockIDs, which keeps
// the id for the first holder in document order: the write would move the id, and the ask row or
// anchor keyed on it, onto whichever of the two blocks comes first.
func RepeatedBlockID(trees ...*Node) error {
	seen := make(map[string]struct{})
	var repeated error
	for _, tree := range trees {
		Walk(tree, func(node *Node) bool {
			if node.Type == "doc" || isInlineNodeType(node.Type) {
				return true
			}
			if id, _ := node.Attrs[BlockIDAttr].(string); id != "" {
				if _, held := seen[id]; held {
					repeated = &RepeatedBlockIDError{ID: id}
					return false
				}
				seen[id] = struct{}{}
			}
			return true
		})
		if repeated != nil {
			return repeated
		}
	}
	return nil
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
