package pmdoc

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ErrBlockPointer is a write refused because a `block[]` attribute it introduces points at a block
// of a type the attribute's targets do not allow (CheckBlockPointers). It is the caller's to fix,
// never a schema refusal (ErrSchema): the tree it names is inside the schema.
var ErrBlockPointer = errors.New("block pointer refused")

// BlockPointer is one reference a typed block's `block[]` attribute holds: the typed block's id
// and type, the attribute, the canonical block reference, and the attribute's targets (none: any
// block).
type BlockPointer struct {
	Block     string
	BlockType string
	Attribute string
	Ref       string
	Targets   []string
}

// BlockPointers lists, in document order, each reference each typed block's `block[]` attributes
// hold, a block's attributes by name. A schema declaring no `block[]` attribute (every shipped
// schema) lists none without walking tree.
func BlockPointers(tree *Node) []BlockPointer {
	if len(schemaLinkKinds) == 0 {
		return nil
	}
	var pointers []BlockPointer
	Walk(tree, func(node *Node) bool {
		typ, ok := typedBlock(node.Type)
		if !ok {
			return true
		}
		names := make([]string, 0, len(typ.Attributes))
		for name, attr := range typ.Attributes {
			if attr.Kind == "block[]" {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		id, _ := node.Attrs[BlockIDAttr].(string)
		for _, name := range names {
			refs, _ := stringSlice(node.Attrs[name])
			for _, ref := range refs {
				pointers = append(pointers, BlockPointer{Block: id, BlockType: node.Type, Attribute: name, Ref: ref, Targets: typ.Attributes[name].Targets})
			}
		}
		return true
	})
	return pointers
}

// CheckBlockPointers refuses tree, the tree a write leaves, when a pointer it introduces names a
// block of a type its attribute's targets do not allow, naming the first such pointer in document
// order. A pointer is introduced when prior, the tree before the write, holds no pointer with the
// same typed block id, attribute and reference: a new pointer, a changed value, or a block
// retyped into a type declaring the attribute. A nil prior (a new document, or one outside the
// schema) holds none. A pointer the write leaves unchanged is never refused, so a target retyped
// elsewhere blocks no unrelated edit.
//
// resolve returns the current types of the block a reference names, none when no block has the
// id: such a pointer is accepted, as an ordinary reference to an absent node is. More than one
// type is refused when any is outside the targets. Its error is returned as it is.
func CheckBlockPointers(tree, prior *Node, resolve func(ref string) ([]string, error)) error {
	pointers := BlockPointers(tree)
	if len(pointers) == 0 {
		return nil
	}
	held := make(map[[3]string]struct{})
	for _, pointer := range BlockPointers(prior) {
		held[[3]string{pointer.Block, pointer.Attribute, pointer.Ref}] = struct{}{}
	}
	for _, pointer := range pointers {
		if len(pointer.Targets) == 0 {
			continue
		}
		if _, unchanged := held[[3]string{pointer.Block, pointer.Attribute, pointer.Ref}]; unchanged {
			continue
		}
		types, err := resolve(pointer.Ref)
		if err != nil {
			return err
		}
		for _, typ := range types {
			if !slices.Contains(pointer.Targets, typ) {
				return fmt.Errorf("%w: typed block %q #%s attribute %q points at %s, a %q block; it points only at %s",
					ErrBlockPointer, pointer.BlockType, pointer.Block, pointer.Attribute, pointer.Ref, typ, strings.Join(pointer.Targets, ", "))
			}
		}
	}
	return nil
}
