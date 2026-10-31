package pmdoc

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/reearth/ygo/crdt"
)

// Read converts the live Yjs tree the browser editor writes into a Node tree. The tree is the
// caller's own: it shares no map or slice with the document it was read from (ownedValue).
func Read(frag *crdt.YXmlFragment) (*Node, error) {
	return readDocument(frag, readLiveText, true)
}

// ReadForRendering is Read without ownedValue's copy: for a caller that renders the tree and
// discards it before releasing whatever lock makes the document's values safe to read, so
// nothing retains the tree afterward to alias a later reader or a write that lands once the lock
// is released.
func ReadForRendering(frag *crdt.YXmlFragment) (*Node, error) {
	return readDocument(frag, readLiveText, false)
}

// ReadUnshared is Read for a document no other goroutine reads or writes while the call runs, and
// which the caller drops afterwards: the read cache's fold of a history it has just decoded. The
// tree is the caller's own, as Read's is (ownedValue). It reads each text by walking its items
// directly (readUnsharedText) rather than through ygo's ToDelta, which takes the document's read
// lock and builds a delta the walk would only turn into nodes, and it returns the tree validated
// once, with the Validate and so the errors Read gives, so its holder never validates it again.
func ReadUnshared(frag *crdt.YXmlFragment) (ValidDocument, error) {
	doc, err := readDocument(frag, readUnsharedText, true)
	if err != nil {
		return ValidDocument{}, err
	}
	return ValidDocument{doc: doc}, nil
}

// ReadInTransaction reads a live tree while the caller's Yjs transaction holds
// the document mutex. It uses the same lock-free text-delta walk as Update.
func ReadInTransaction(txn *crdt.Transaction, frag *crdt.YXmlFragment) (*Node, error) {
	if txn == nil {
		return nil, fmt.Errorf("%w: ReadInTransaction requires transaction", ErrSchema)
	}
	return readDocument(frag, readTextInTransaction, true)
}

// textReader expands one Y.XmlText into ProseMirror text nodes: one per run of identical marks,
// exactly as y-prosemirror's createTypeFromTextNodes stores them. copyAttrs copies each mark's
// attributes (ownedValue).
type textReader func(text *crdt.YXmlText, copyAttrs bool) ([]*Node, error)

// readLiveText reads text through its delta outside any transaction, as Read and ReadForRendering
// walk it.
func readLiveText(text *crdt.YXmlText, copyAttrs bool) ([]*Node, error) {
	return textNodesOfDelta(text.ToDelta(), copyAttrs)
}

// readTextInTransaction reads text through the lock-free delta walk Update makes inside the
// transaction holding the document's lock.
func readTextInTransaction(text *crdt.YXmlText, copyAttrs bool) ([]*Node, error) {
	delta, err := yTextDeltaInTransaction(text)
	if err != nil {
		return nil, err
	}
	return textNodesOfDelta(delta, copyAttrs)
}

func readDocument(frag *crdt.YXmlFragment, readText textReader, copyAttrs bool) (*Node, error) {
	children, err := readChildren(frag, readText, 1, copyAttrs)
	if err != nil {
		return nil, err
	}
	doc := &Node{Type: "doc", Children: children}
	if err := doc.Validate(); err != nil {
		return nil, unknownBeside(doc, err)
	}
	return doc, nil
}

func readChildren(frag *crdt.YXmlFragment, readText textReader, depth int, copyAttrs bool) ([]*Node, error) {
	var out []*Node
	for _, child := range frag.Children() {
		switch c := child.(type) {
		case *crdt.YXmlElement:
			n, err := readElement(c, readText, depth, copyAttrs)
			if err != nil {
				return nil, err
			}
			out = append(out, n)
		case *crdt.YXmlText:
			nodes, err := readText(c, copyAttrs)
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

func readElement(e *crdt.YXmlElement, readText textReader, depth int, copyAttrs bool) (*Node, error) {
	if err := treeDepthError(depth); err != nil {
		return nil, err
	}
	n := &Node{Type: e.NodeName}
	if attrs := e.GetAttributeValues(); len(attrs) > 0 {
		// The map is GetAttributeValues' own; its values are the document's.
		if copyAttrs {
			for name, value := range attrs {
				attrs[name] = ownedValue(value)
			}
		}
		n.Attrs = treeAttrs(e.NodeName, attrs)
	}
	// A typed block an earlier build wrote may lack an attribute added since; it reads with the
	// attribute's default, as the browser editor reads it.
	if typ, typed := typedBlock(e.NodeName); typed {
		n.Attrs = withWrittenDefaults(typ, n.Attrs)
	}
	children, err := readChildren(&e.YXmlFragment, readText, depth+1, copyAttrs)
	if err != nil {
		return nil, err
	}
	n.Children = children
	return n, nil
}

// textNodesOfDelta is the text node of each insert in delta (deltaTextNode).
func textNodesOfDelta(delta []crdt.Delta, copyAttrs bool) ([]*Node, error) {
	var out []*Node
	for _, d := range delta {
		if d.Op != crdt.DeltaOpInsert {
			return nil, fmt.Errorf("%w: unexpected yjs text delta operation %d", ErrSchema, d.Op)
		}
		n, err := deltaTextNode(d.Insert, d.Attributes, copyAttrs)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// deltaTextNode is the text node of insert, a text delta's insert, carrying a mark for each of
// attributes, the formatting it is inserted under; a node without marks has nil Marks. Only a
// string makes a text node: any other embed is outside the schema.
func deltaTextNode(insert any, attributes crdt.Attributes, copyAttrs bool) (*Node, error) {
	s, ok := insert.(string)
	if !ok {
		return nil, fmt.Errorf("%w: unsupported yjs text embed %T", ErrSchema, insert)
	}
	n := &Node{Type: "text", Text: s}
	for name, raw := range attributes {
		if copyAttrs {
			raw = ownedValue(raw)
		}
		attrs, err := attrsFromY(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: mark %q attributes: %v", ErrSchema, name, err)
		}
		n.Marks = append(n.Marks, Mark{Type: yattrToMarkName(name), Attrs: attrs})
	}
	sortMarks(n.Marks)
	return n, nil
}

// readUnsharedText is text's nodes as readLiveText reads them, without ygo's ToDelta: it is
// ToDelta's loop (crdt/ytext.go, YText.ToDelta, sjawhar/ygo v1.54.1-sami.2) with its state, the
// buffered run's text and attribute snapshot, attrsDirty and the live attribute set, emitting each
// delta ToDelta would emit as its node (deltaTextNode) instead, in the same order, so the first
// node refused is the one readLiveText refuses. It takes no lock: its caller holds the only
// reference to the document (ReadUnshared). The rules it keeps from ToDelta:
//
//   - A deleted item is skipped. A format sets its key in the live set, and an untyped nil value
//     deletes the key (crdt's updateAttr); a typed nil sets it.
//   - A string starts a run when none is buffered, the run taking a snapshot of the live set (nil
//     when it is empty). It ends the run and starts another only when a format arrived since the
//     last string and the live set differs from the run's snapshot, comparing each value with
//     reflect.DeepEqual, so a NaN never matches. Every string, an empty one too, clears that a
//     format arrived. A run with no text makes no node, so an empty string never makes one.
//   - An embed ends the run. A string embed is its own node, never joined to a neighbour, carrying
//     the live set rather than a run's snapshot, and an empty node when the string is empty; any
//     other embed is refused, as deltaTextNode refuses it.
//   - Any other content in a text (a type, Any, JSON, binary, deleted content) is ignored.
//
// A run's text is a string of its own, never a string item's Str, which can be a slice of a longer
// string an item split left behind.
func readUnsharedText(text *crdt.YXmlText, copyAttrs bool) ([]*Node, error) {
	var out []*Node
	current := make(crdt.Attributes)
	var run strings.Builder
	var runAttrs crdt.Attributes // the buffered run's attributes; nil when it is unformatted
	attrsDirty := false

	sameAsRun := func() bool {
		if len(current) != len(runAttrs) {
			return false
		}
		for key, value := range current {
			if runValue, ok := runAttrs[key]; !ok || !reflect.DeepEqual(value, runValue) {
				return false
			}
		}
		return true
	}
	flush := func() error {
		if run.Len() == 0 {
			return nil
		}
		n, err := deltaTextNode(run.String(), runAttrs, copyAttrs)
		if err != nil {
			return err
		}
		out = append(out, n)
		run.Reset()
		runAttrs = nil
		return nil
	}

	for item := yTextStart(text); item != nil; item = item.Right {
		if item.Deleted {
			continue
		}
		switch content := item.Content.(type) {
		case *crdt.ContentString:
			switch {
			case run.Len() == 0:
				runAttrs = cloneYAttributes(current)
			case attrsDirty && !sameAsRun():
				if err := flush(); err != nil {
					return nil, err
				}
				runAttrs = cloneYAttributes(current)
			}
			attrsDirty = false
			run.WriteString(content.Str)
		case *crdt.ContentEmbed:
			if err := flush(); err != nil {
				return nil, err
			}
			n, err := deltaTextNode(content.Val, current, copyAttrs)
			if err != nil {
				return nil, err
			}
			out = append(out, n)
		case *crdt.ContentFormat:
			if content.Val == nil {
				delete(current, content.Key)
			} else {
				current[content.Key] = content.Val
			}
			attrsDirty = true
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

// treeAttrs is attrs, a live element's attributes, as the tree holds them: the value liveNulls
// writes for a null attribute is null. It maps attrs in place.
func treeAttrs(nodeName string, attrs map[string]any) Attrs {
	for name, value := range liveNulls[nodeName] {
		if attrs[name] == value {
			attrs[name] = nil
		}
	}
	return Attrs(attrs)
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

// ownedValue is value, an attribute value a Yjs document holds, with every map and slice in it
// copied. ygo hands a reader the document's own values - a mark's attribute map, an element's
// list - and a tree read from a document others read after it, such as a room's replica, would
// otherwise carry its holder's edits to them into every later read.
func ownedValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		owned := make(map[string]any, len(value))
		for name, item := range value {
			owned[name] = ownedValue(item)
		}
		return owned
	case crdt.Attributes:
		owned := make(crdt.Attributes, len(value))
		for name, item := range value {
			owned[name] = ownedValue(item)
		}
		return owned
	case []any:
		owned := make([]any, len(value))
		for index, item := range value {
			owned[index] = ownedValue(item)
		}
		return owned
	default:
		return value
	}
}
