package pmdoc

import (
	"fmt"
	"strings"
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

// RepeatedBlockID reports the first block id named carries, in named's document order, that next
// carries on two or more blocks and on more blocks than live does, or nil. next is the document a
// write would store over live (live is nil for a document with none before it), and named is what
// the write itself wrote: the markdown's tree, or the fragment it splices in. Only named's ids are
// judged, because a splice that splits a block gives both halves the block's id, which is the
// document's to repair, not the caller's. A repeat live already carries is the document's own too
// - a browser write can leave one until settlement repairs it. Any other is refused rather than
// left to EnsureBlockIDs, which keeps the id for the first holder in document order: the write
// would move the id, and the ask row or anchor keyed on it, onto whichever of the blocks comes
// first. Only a typed block's markdown can name its id, so what this finds is markdown naming one
// id twice, or naming an id live holds outside the text the write replaces.
func RepeatedBlockID(live, next, named *Node) error {
	held := blockIDCounts(live)
	written := blockIDCounts(next)
	var repeated error
	walkBlockIDs(named, func(id string) bool {
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

// walkBlockIDs visits the id of every block in tree that carries one (storedBlockID), in document
// order, until visit returns false.
func walkBlockIDs(tree *Node, visit func(id string) bool) {
	Walk(tree, func(node *Node) bool {
		if node.Type == "doc" || isInlineNodeType(node.Type) {
			return true
		}
		if id, ok := storedBlockID(node); ok {
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
		if id, ok := storedBlockID(node); ok {
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

// EnsureBlockIDsCount gives every block in tree a unique blockId and reports how many missing
// (storedBlockID) or duplicate IDs it replaced.
func EnsureBlockIDsCount(tree *Node) (stamped int) {
	seen := make(map[string]struct{})
	walk(tree, func(node *Node, _ []int, _, _ int) bool {
		if node.Type == "doc" || isInlineNodeType(node.Type) {
			return true
		}
		if id, ok := storedBlockID(node); ok {
			if _, duplicate := seen[id]; !duplicate {
				seen[id] = struct{}{}
				return true
			}
		}
		if node.Attrs == nil {
			node.Attrs = Attrs{}
		}
		var id string
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

// storedBlockID is node's block id, and whether it has one an ask row or an anchor can store: any
// text but the empty string and one holding U+0000, which PostgreSQL's text cannot hold. A
// browser's update can set any string, so such an id is minted again as a missing one is. Every
// other id stays: the browser editor keeps whatever a pasted typed block's `#id` names up to a
// quote, `#`, `.`, `<`, `=`, `>`, a backtick, `}` or whitespace (`q:1`, `décision`), and an ask row
// or anchor stored under such an id is found by it.
func storedBlockID(node *Node) (string, bool) {
	id, _ := node.Attrs[BlockIDAttr].(string)
	return id, id != "" && strings.IndexByte(id, 0) < 0
}

// BlockIDForRange returns the lowest block that contains all of r and carries an id
// (storedBlockID). A range spanning top-level siblings has only the document root in common,
// which has no usable block identity, so it returns an empty ID without an error, as it does for a
// range whose blocks carry none.
func BlockIDForRange(tree *Node, r Range) (string, error) {
	if tree == nil || tree.Type != "doc" {
		return "", ErrTargetNotFound
	}
	var blockID string
	walk(tree, func(node *Node, _ []int, pos, end int) bool {
		if node.Type == "doc" || isInlineNodeType(node.Type) || r.From < pos || r.To > end {
			return true
		}
		if id, ok := storedBlockID(node); ok {
			blockID = id
		}
		return true
	})
	return blockID, nil
}
