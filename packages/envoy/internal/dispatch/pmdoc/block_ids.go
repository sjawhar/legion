package pmdoc

import (
	"crypto/rand"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
)

const BlockIDAttr = "blockId"

var blockIDGenerator = struct {
	sync.RWMutex
	mint func() string
}{mint: newBlockID}

// SetBlockIDGenerator replaces the block ID generator. Passing nil restores UUID v4 IDs
// (newBlockID). Fixture generators and tests use this to make generated documents deterministic.
func SetBlockIDGenerator(mint func() string) {
	blockIDGenerator.Lock()
	defer blockIDGenerator.Unlock()
	if mint == nil {
		blockIDGenerator.mint = newBlockID
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

// blockIDEntropy is the random bytes ids are minted from, read from crypto/rand a block at a time.
// The kernel is asked once per read, and a write mints an id for every block that names none: a
// document of 8,000 list items spent about half its parse inside sixteen-byte reads, one per item
// (the plain list of heavy_bodies_test.go). Every id still comes from those bytes alone, 122 of
// them as a version 4 UUID holds.
//
// google/uuid's own EnableRandPool buffers the same way, and this reader exists instead of it
// because that switch is process-wide and, as its doc says, "not thread-safe and should only be
// called when there is no possibility that New or any other UUID Version 4 generation function
// will be called concurrently": the server mints version 4 UUIDs from other goroutines while a
// document is parsed (a process id in dispatch/processes, a suggestion row's in dispatch/api), so
// there is no moment at which it may be turned on. This reader is handed to one call site
// instead, and the buffer it draws from is this package's own.
var blockIDEntropy blockIDBytes

// blockIDEntropyIDs is how many ids one read of crypto/rand fills. Each costs sixteen bytes, so
// the buffer is four kibibytes.
const blockIDEntropyIDs = 256

// blockIDBytes hands out the bytes of blockIDEntropy.
type blockIDBytes struct {
	sync.Mutex
	bytes [blockIDEntropyIDs * len(uuid.UUID{})]byte
	// unread is how many bytes at the end of the buffer no id has taken yet: zero before the
	// first id of the process, and again once a block of them is spent.
	unread int
}

// Read fills p from the block of bytes read last, reading crypto/rand again once they are spent,
// so uuid.NewRandomFromReader mints an id without a read of its own. It is the io.Reader
// uuid.NewRandomFromReader draws from, so what a block id holds of those bytes, and where its
// version and variant go, is the library's own doing rather than this package's.
func (b *blockIDBytes) Read(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	if b.unread == 0 {
		if _, err := rand.Read(b.bytes[:]); err != nil {
			return 0, fmt.Errorf("pmdoc: reading randomness for a block id: %w", err)
		}
		b.unread = len(b.bytes)
	}
	read := copy(p, b.bytes[len(b.bytes)-b.unread:])
	b.unread -= read
	return read, nil
}

// newBlockID is a version 4 UUID, as uuid.NewString writes one, minted from blockIDEntropy.
func newBlockID() string {
	return uuid.Must(uuid.NewRandomFromReader(&blockIDEntropy)).String()
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

// BlockIDRepairCount reports how many block IDs EnsureBlockIDs would mint. Like EnsureBlockIDsCount
// it needs no positions, so it walks the tree without sizing every node (Walk).
func BlockIDRepairCount(tree *Node) (repairs int) {
	seen := make(map[string]struct{})
	Walk(tree, func(node *Node) bool {
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
	Walk(tree, func(node *Node) bool {
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
