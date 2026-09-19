package pmdoc

import (
	"fmt"

	"github.com/reearth/ygo/crdt"
)

// Read converts the live Yjs tree the browser editor writes into a Node tree.
func Read(frag *crdt.YXmlFragment) (*Node, error) {
	return readDocument(frag, func(text *crdt.YXmlText) ([]crdt.Delta, error) {
		return text.ToDelta(), nil
	})
}

// ReadInTransaction reads a live tree while the caller's Yjs transaction holds
// the document mutex. It uses the same lock-free text-delta walk as Update.
func ReadInTransaction(txn *crdt.Transaction, frag *crdt.YXmlFragment) (*Node, error) {
	if txn == nil {
		return nil, fmt.Errorf("%w: ReadInTransaction requires transaction", ErrSchema)
	}
	return readDocument(frag, yTextDeltaInTransaction)
}

type textDeltaReader func(*crdt.YXmlText) ([]crdt.Delta, error)

func readDocument(frag *crdt.YXmlFragment, readDelta textDeltaReader) (*Node, error) {
	children, err := readChildren(frag, readDelta)
	if err != nil {
		return nil, err
	}
	doc := &Node{Type: "doc", Children: children}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return doc, nil
}

func readChildren(frag *crdt.YXmlFragment, readDelta textDeltaReader) ([]*Node, error) {
	var out []*Node
	for _, child := range frag.Children() {
		switch c := child.(type) {
		case *crdt.YXmlElement:
			n, err := readElement(c, readDelta)
			if err != nil {
				return nil, err
			}
			out = append(out, n)
		case *crdt.YXmlText:
			nodes, err := readText(c, readDelta)
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

func readElement(e *crdt.YXmlElement, readDelta textDeltaReader) (*Node, error) {
	n := &Node{Type: e.NodeName}
	if attrs := e.GetAttributeValues(); len(attrs) > 0 {
		n.Attrs = Attrs(attrs)
	}
	children, err := readChildren(&e.YXmlFragment, readDelta)
	if err != nil {
		return nil, err
	}
	n.Children = children
	return n, nil
}

// readText expands one Y.XmlText into ProseMirror text nodes: one per run of
// identical marks, exactly as y-prosemirror's createTypeFromTextNodes stores them.
func readText(t *crdt.YXmlText, readDelta textDeltaReader) ([]*Node, error) {
	delta, err := readDelta(t)
	if err != nil {
		return nil, err
	}
	var out []*Node
	for _, d := range delta {
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
