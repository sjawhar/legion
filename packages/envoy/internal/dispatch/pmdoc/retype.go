package pmdoc

import "fmt"

// RetypeBlock turns a paragraph into a typed block in place. The typed wrapper
// keeps the paragraph's identity; its paragraph body receives a fresh identity
// from the document closer.
func RetypeBlock(doc *Node, blockID, typeName string, attributes Attrs) (*Node, error) {
	if doc == nil || doc.Type != "doc" {
		return nil, fmt.Errorf("%w: RetypeBlock wants a doc", ErrSchema)
	}
	if blockID == "" {
		return nil, fmt.Errorf("%w: retype block id is required", ErrSchema)
	}
	typ, known := typedBlock(typeName)
	if !known {
		return nil, fmt.Errorf("%w: unknown typed block %q", ErrSchema, typeName)
	}

	var targetPath []int
	walk(doc, func(node *Node, path []int, _, _ int) bool {
		if node.Type != "paragraph" || node.Attrs[BlockIDAttr] != blockID {
			return true
		}
		targetPath = append([]int(nil), path...)
		return false
	})
	if targetPath == nil {
		return nil, fmt.Errorf("%w: block %q", ErrTargetNotFound, blockID)
	}

	out := cloneNode(doc)
	body := nodeAtPath(out, targetPath)
	body.Attrs = cloneAttrs(body.Attrs)
	delete(body.Attrs, BlockIDAttr)
	attrs := defaultAttributes(typ)
	attrs[BlockIDAttr] = blockID
	for name, value := range attributes {
		if name == BlockIDAttr || typ.Attributes[name].Server {
			continue
		}
		attrs[name] = value
	}
	retyped := &Node{Type: typeName, Attrs: attrs, Children: []*Node{body}}
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
