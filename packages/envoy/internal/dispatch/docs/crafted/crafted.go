// Package crafted writes the document updates a crafted client can send that ygo's own writers
// refuse to make, for tests in more than one package and for the server's test hooks
// (docs/testhooks.go). It imports neither package testing, since the server links it, nor any
// Dispatch package, so docs, docstest and pmdoc can all import it.
package crafted

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
)

// NestedLinkUpdate marks every character of the first text in doc's prosemirror fragment with a
// link whose "nested" attribute is nested, and returns the update a crafted client sends for that
// change. ygo's own writers refuse a mark nested past the 100 levels YText takes, so the change is
// made with a placeholder and the update rewritten: a V1 update carries a mark's attributes as JSON
// text, which ygo reads back as deep as encoding/json does. doc keeps the placeholder, so it is a
// copy the caller throws away.
func NestedLinkUpdate(doc *crdt.Doc, nested any) ([]byte, error) {
	text := FirstText(doc.GetXmlFragment("prosemirror"))
	if text == nil {
		return nil, errors.New("the document holds no text to link")
	}
	link := func(nested any) map[string]any {
		return map[string]any{"href": "https://example.com", "nested": nested}
	}
	before := doc.StateVector()
	doc.Transact(func(txn *crdt.Transaction) {
		text.Format(txn, 0, text.Len(), crdt.Attributes{"link": link("placeholder")})
	})
	update := crdt.EncodeStateAsUpdateV1(doc, before)
	// ygo writes the JSON with sorted keys and, depending on its release, with or without escaping
	// <, > and &, none of which the link holds, so json.Marshal spells it as ygo wrote it.
	written := func(v any) ([]byte, error) {
		text, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("encode %T as JSON: %w", v, err)
		}
		return encoding.EncodeBytes(func(encoder *encoding.Encoder) { encoder.WriteVarString(string(text)) }), nil
	}
	placeholder, err := written(link("placeholder"))
	if err != nil {
		return nil, err
	}
	if count := bytes.Count(update, placeholder); count != 1 {
		return nil, fmt.Errorf("the link's update holds its placeholder %d times, want once", count)
	}
	replacement, err := written(link(nested))
	if err != nil {
		return nil, err
	}
	return bytes.Replace(update, placeholder, replacement, 1), nil
}

// FirstText is the first text node in document order under fragment, or nil for a fragment
// without one.
func FirstText(fragment *crdt.YXmlFragment) *crdt.YXmlText {
	for _, child := range fragment.Children() {
		switch node := child.(type) {
		case *crdt.YXmlText:
			return node
		case *crdt.YXmlElement:
			if text := FirstText(&node.YXmlFragment); text != nil {
				return text
			}
		}
	}
	return nil
}

// NestedValue is the string "x" inside depth arrays, each nested inside the next. It builds arrays
// only; pmdoc's attribute bound counts arrays and objects alike, so depth is the value's nesting as
// that bound counts it.
func NestedValue(depth int) any {
	var value any = "x"
	for range depth {
		value = []any{value}
	}
	return value
}
