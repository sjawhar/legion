package pmdoc

import (
	"errors"
	"fmt"
)

// ErrUnknownBlockType reports a retype to a type the block schema does not declare.
var ErrUnknownBlockType = errors.New("pmdoc: unknown typed block")

// ErrBlockNotRetypable reports a retype of a block that is neither a paragraph nor a typed block.
var ErrBlockNotRetypable = errors.New("pmdoc: block cannot be retyped")

// RetypeBlock turns the block carrying blockID into a typed block in place. A
// paragraph becomes the new block's body and receives a fresh identity from the
// document closer; a typed block keeps its body and takes the new type's
// attributes. The typed wrapper keeps blockID.
func RetypeBlock(doc *Node, blockID, typeName string, attributes Attrs) (*Node, error) {
	if doc == nil || doc.Type != "doc" {
		return nil, fmt.Errorf("%w: RetypeBlock wants a doc", ErrSchema)
	}
	if blockID == "" {
		return nil, fmt.Errorf("%w: retype block id is required", ErrSchema)
	}
	typ, known := typedBlock(typeName)
	if !known {
		return nil, fmt.Errorf("%w: %w %q", ErrSchema, ErrUnknownBlockType, typeName)
	}

	targetPath, _, found := findBlock(doc, blockID)
	if !found {
		return nil, fmt.Errorf("%w: block %q", ErrTargetNotFound, blockID)
	}
	target := nodeAtPath(doc, targetPath)
	_, typed := typedBlock(target.Type)
	if target.Type != "paragraph" && !typed {
		return nil, fmt.Errorf("%w: %w: block %q is a %s; retype accepts a paragraph or a typed block", ErrSchema, ErrBlockNotRetypable, blockID, target.Type)
	}

	out := cloneNode(doc)
	body := nodeAtPath(out, targetPath)
	children := body.Children
	if !typed {
		body.Attrs = cloneAttrs(body.Attrs)
		delete(body.Attrs, BlockIDAttr)
		children = []*Node{body}
	}
	attrs := defaultAttributes(typ)
	attrs[BlockIDAttr] = blockID
	for name, value := range attributes {
		if name == BlockIDAttr || typ.Attributes[name].Server {
			continue
		}
		attrs[name] = value
	}
	retyped := &Node{Type: typeName, Attrs: attrs, Children: children}
	replaceNodeAtPath(out, targetPath, retyped)
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// StripServerOwnedAttrs resets typed-block server attributes at an agent markdown boundary.
// Parsed historical versions retain their recorded values; agent operations never supply them.
func StripServerOwnedAttrs(doc *Node) {
	walk(doc, func(node *Node, _ []int, _, _ int) bool {
		typ, typed := typedBlock(node.Type)
		if !typed {
			return true
		}
		for name, attribute := range typ.Attributes {
			if !attribute.Server {
				continue
			}
			if attribute.Default == nil {
				delete(node.Attrs, name)
				continue
			}
			node.Attrs[name] = cloneSchemaValue(attribute.Default)
		}
		return true
	})
}

// SetBlockAttributes replaces named attributes on one typed block. A nil value
// removes an optional attribute so the canonical directive omits it.
func SetBlockAttributes(doc *Node, blockID string, attributes Attrs) (*Node, error) {
	if doc == nil || doc.Type != "doc" {
		return nil, fmt.Errorf("%w: SetBlockAttributes wants a doc", ErrSchema)
	}
	if blockID == "" {
		return nil, fmt.Errorf("%w: block id is required", ErrSchema)
	}

	var targetPath []int
	walk(doc, func(node *Node, path []int, _, _ int) bool {
		if node.Attrs[BlockIDAttr] != blockID {
			return true
		}
		if _, typed := typedBlock(node.Type); !typed {
			return true
		}
		targetPath = append([]int(nil), path...)
		return false
	})
	if targetPath == nil {
		return nil, fmt.Errorf("%w: typed block %q", ErrTargetNotFound, blockID)
	}

	out := cloneNode(doc)
	target := nodeAtPath(out, targetPath)
	target.Attrs = cloneAttrs(target.Attrs)
	for name, value := range attributes {
		if name == BlockIDAttr {
			continue
		}
		if value == nil {
			delete(target.Attrs, name)
			continue
		}
		target.Attrs[name] = value
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}
