package pmdoc

import (
	"fmt"

	"github.com/reearth/ygo/crdt"
)

// Read converts the live Yjs tree the browser editor writes into a Node tree.
func Read(frag *crdt.YXmlFragment) (*Node, error) {
	children, err := readChildren(frag)
	if err != nil {
		return nil, err
	}
	doc := &Node{Type: "doc", Children: children}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return doc, nil
}

func readChildren(frag *crdt.YXmlFragment) ([]*Node, error) {
	var out []*Node
	for _, child := range frag.Children() {
		switch c := child.(type) {
		case *crdt.YXmlElement:
			n, err := readElement(c)
			if err != nil {
				return nil, err
			}
			out = append(out, n)
		case *crdt.YXmlText:
			nodes, err := readText(c)
			if err != nil {
				return nil, err
			}
			out = append(out, nodes...)
		default:
			return nil, fmt.Errorf("%w: unexpected yjs child %T", ErrSchema, child)
		}
	}
	return out, nil
}

func readElement(e *crdt.YXmlElement) (*Node, error) {
	n := &Node{Type: e.NodeName}
	if attrs := e.GetAttributeValues(); len(attrs) > 0 {
		n.Attrs = Attrs(attrs)
	}
	children, err := readChildren(&e.YXmlFragment)
	if err != nil {
		return nil, err
	}
	n.Children = children
	return n, nil
}

// readText expands one Y.XmlText into ProseMirror text nodes: one per run of
// identical marks, exactly as y-prosemirror's createTypeFromTextNodes stores them.
func readText(t *crdt.YXmlText) ([]*Node, error) {
	var out []*Node
	for _, d := range t.ToDelta() {
		if d.Op != crdt.DeltaOpInsert {
			return nil, fmt.Errorf("%w: unexpected yjs text delta operation %d", ErrSchema, d.Op)
		}
		s, ok := d.Insert.(string)
		if !ok {
			return nil, fmt.Errorf("%w: unsupported yjs text embed %T", ErrSchema, d.Insert)
		}
		n := &Node{Type: "text", Text: s}
		for name, raw := range d.Attributes {
			attrs, err := attrsFromY(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: mark %q attributes: %v", ErrSchema, name, err)
			}
			n.Marks = append(n.Marks, Mark{Type: yattrToMarkName(name), Attrs: attrs})
		}
		sortMarks(n.Marks)
		out = append(out, n)
	}
	return out, nil
}

func attrsFromY(raw any) (Attrs, error) {
	switch attrs := raw.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return Attrs(attrs), nil
	case crdt.Attributes:
		return Attrs(attrs), nil
	default:
		return nil, fmt.Errorf("want object, got %T", raw)
	}
}
