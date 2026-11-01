package pmdoc

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// ErrBlockPointer is a write refused because a `block[]` attribute it introduces points at a block
// of a type the attribute's targets do not allow (CheckBlockPointers). It is the caller's to fix,
// never a schema refusal (ErrSchema): the tree it names is inside the schema.
var ErrBlockPointer = errors.New("block pointer refused")

// BlockPointer is one reference a typed block's `block[]` attribute holds: the typed block's id
// and type, the attribute, the canonical block reference as written and as read, and the
// attribute's targets (none: any block).
type BlockPointer struct {
	Block     string
	BlockType string
	Attribute string
	Ref       string
	Target    text.Ref
	Targets   []string
}

// BlockPointers lists, in document order, each reference each typed block's `block[]` attributes
// hold, a block's attributes by name. A schema declaring no `block[]` attribute (every shipped
// schema) lists none without walking tree. A tree inside the schema holds only canonical
// references (ParseBlockPointerValue); any other value is skipped.
//
// A typed block repeating the id of a typed block before it in document order lists none: the
// block index takes the first holder of an id for its node (refs.IndexBlocks). Only typed blocks
// count here, because only their ids are written in markdown, and so are the same in the tree a
// version write indexes and in the markdown the reconciliation reads back: the rendering writes a
// typed block's id so that it reads back as the tree holds it (TypedBlockIDAttribute), each U+0000
// aside, which it writes U+FFFD (withNulsWritten), so each block is listed under its id as
// written. A paragraph's id is not written in markdown, and a list item or table cell has no
// offset, so it is never the index's node. An earlier holder that is not typed but has an offset
// (a paragraph) is caught by the index itself, which writes a pointer only from a node of the
// pointer's own block type (refs.IndexBlocks).
func BlockPointers(tree *Node) []BlockPointer {
	if len(active.linkKinds) == 0 {
		return nil
	}
	var pointers []BlockPointer
	held := make(map[string]struct{})
	Walk(tree, func(node *Node) bool {
		typ, typed := typedBlock(node.Type)
		if !typed {
			return true
		}
		id, _ := node.Attrs[BlockIDAttr].(string)
		id = nulAsReplacement(id)
		if id != "" {
			if _, repeated := held[id]; repeated {
				return true
			}
			held[id] = struct{}{}
		}
		for _, name := range active.pointerAttributes[node.Type] {
			refs, _ := stringSlice(node.Attrs[name])
			for _, ref := range refs {
				target, canonical := text.CanonicalBlockRef(ref)
				if !canonical {
					continue
				}
				pointers = append(pointers, BlockPointer{Block: id, BlockType: node.Type, Attribute: name, Ref: ref, Target: target, Targets: typ.Attributes[name].Targets})
			}
		}
		return true
	})
	return pointers
}

// CheckBlockPointers refuses tree, the tree a write leaves, when a pointer it introduces names a
// block of a type its attribute's targets do not allow, naming the first such pointer in document
// order. A pointer is introduced when prior, the tree before the write, holds no pointer with the
// same typed block id, typed block type, attribute and reference: a new pointer, a changed value,
// or a block retyped into a type declaring the attribute, whatever the type before declared under
// that name. A nil prior (a new document, or one outside the schema) holds none. A pointer the
// write leaves unchanged is never refused, so a target retyped elsewhere blocks no unrelated edit.
// Its pointers are BlockPointers', so a typed block repeating the id of a typed block before it is
// never checked. The block index writes no edge for it while it repeats the id, but settlement's
// EnsureBlockIDs then gives it a fresh id, and its unchecked pointer becomes an edge, which the
// readers hide while its target has a type the attribute does not allow (block_ref_listed). A
// typed block repeating the id of an earlier paragraph is checked, though the index writes no edge
// for it either.
//
// resolve is called at most once, with the target of each introduced pointer whose attribute has
// targets, and returns the current types of the blocks they name by node id (text.TargetID); none
// for an id no block has: such a pointer is accepted, as an ordinary reference to an absent node
// is. More than one type is refused when any is outside the targets. Its error is returned as it
// is.
func CheckBlockPointers(tree, prior *Node, resolve func(targets []text.Ref) (map[string][]string, error)) error {
	pointers := BlockPointers(tree)
	if len(pointers) == 0 {
		return nil
	}
	held := make(map[[4]string]struct{})
	for _, pointer := range BlockPointers(prior) {
		held[[4]string{pointer.Block, pointer.BlockType, pointer.Attribute, pointer.Ref}] = struct{}{}
	}
	var introduced []BlockPointer
	var targets []text.Ref
	for _, pointer := range pointers {
		if len(pointer.Targets) == 0 {
			continue
		}
		if _, unchanged := held[[4]string{pointer.Block, pointer.BlockType, pointer.Attribute, pointer.Ref}]; unchanged {
			continue
		}
		introduced = append(introduced, pointer)
		targets = append(targets, pointer.Target)
	}
	if len(introduced) == 0 {
		return nil
	}
	types, err := resolve(targets)
	if err != nil {
		return err
	}
	for _, pointer := range introduced {
		for _, typ := range types[text.TargetID(pointer.Target)] {
			if !slices.Contains(pointer.Targets, typ) {
				return fmt.Errorf("%w: typed block %q %s attribute %q points at %s, a %q block; it points only at %s",
					ErrBlockPointer, pointer.BlockType, TypedBlockIDAttribute(pointer.Block), pointer.Attribute, pointer.Ref, typ, strings.Join(pointer.Targets, ", "))
			}
		}
	}
	return nil
}
