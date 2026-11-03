package pmdoc

import (
	"encoding/json"
)

// The display tree's encoding carries what a reader needs to draw the document and nothing
// else. It is the encoding alone that is cut: ReadForDisplay's tree is untouched, so the
// cross-reader differential, which holds that tree against the browser's engines
// (gen/differential.sh), still compares whole trees.
//
// What is left out is what the browser's schema puts back by itself. The dashboard builds the
// document node by node with `schema.nodes[type].create(attrs, content, marks)`
// (documentFromJSON, packages/dispatch/web/src/features/refs/markdown-engine.ts), and
// ProseMirror's NodeType.create fills every attribute the object does not name from that
// attribute's declared default. An attribute standing at its default is therefore bytes a
// reader pays for and then writes the same value over.
//
// displayNodeDefaults is each core node type's attribute defaults as the dashboard's schema
// declares them: milkdown's commonmark and gfm presets, which proof-editor builds its schema
// from, with `blockId` added to every block node (proof-editor's editor/schema/block-ids.ts,
// `{ default: null }`). An attribute this table does not name is one the encoding always
// writes: a link's `href`, which the schema declares with no default at all, and a table cell's
// `alignment`, whose default is "left" where both readers write null for a column the markdown
// does not align.
//
// A typed block's defaults are not here. They are its own schema's (schema/blocks.json), read
// through typedDefaults, so a block type added there needs no change to this file.
var displayNodeDefaults = map[string]Attrs{
	// heading: `id` is the slug milkdown's own generator would write and both readers leave
	// empty; `level` is 1 for a heading nothing says the level of.
	"heading":             {"id": "", "level": 1.0},
	"code_block":          {"language": ""},
	"image":               {"src": "", "alt": "", "title": ""},
	"hardbreak":           {"isInline": false},
	"html":                {"value": ""},
	"bullet_list":         {"spread": false},
	"ordered_list":        {"order": 1.0, "spread": false},
	"footnote_definition": {"label": ""},
	"footnote_reference":  {"label": ""},
	// list_item: milkdown declares `spread` default true, so the false both readers write for a
	// tight item is no default and stays. `checked` is null for an item that is no task.
	"list_item":    {"label": "•", "listType": "bullet", "checked": nil},
	"table_cell":   {"colspan": 1.0, "rowspan": 1.0, "colwidth": nil},
	"table_header": {"colspan": 1.0, "rowspan": 1.0, "colwidth": nil},
}

// displayMarkDefaults is each mark type's attribute defaults, the same reading of the same
// schema. A link's `title` is null for a link written with none; its `href` has no default
// declared, so it is never left out.
var displayMarkDefaults = map[string]Attrs{
	"link": {"title": nil},
}

// typedDefaults is every typed block's declared attribute defaults, read from the block schema
// once. An attribute the schema gives no default is left out of it: the browser declares such
// an attribute `{ default: undefined }`, which fills nothing in.
var typedDefaults = func() map[string]Attrs {
	out := make(map[string]Attrs, len(schema.Types))
	for _, typ := range schema.Types {
		defaults := make(Attrs, len(typ.Attributes))
		for name, attr := range typ.Attributes {
			if attr.Default != nil {
				defaults[name] = attr.Default
			}
		}
		out[typ.Name] = defaults
	}
	return out
}()

// toDisplayJSON is node as a reader is sent it: its own JSON shape with every attribute the
// browser's schema would supply left out, and nothing written where that leaves an empty object
// or an empty array (jsonNode's omitempty). The children are sized once rather than appended to,
// since a display tree is encoded whole and often.
func (n *Node) toDisplayJSON() *jsonNode {
	j := &jsonNode{Type: n.Type, Attrs: displayNodeAttrs(n.Type, n.Attrs), Text: n.Text}
	if len(n.Marks) > 0 {
		j.Marks = make([]jsonMark, len(n.Marks))
		for index, mark := range n.Marks {
			j.Marks[index] = jsonMark{Type: mark.Type, Attrs: withoutDefaults(mark.Attrs, displayMarkDefaults[mark.Type])}
		}
	}
	if len(n.Children) > 0 {
		j.Content = make([]*jsonNode, len(n.Children))
		for index, child := range n.Children {
			j.Content[index] = child.toDisplayJSON()
		}
	}
	return j
}

// displayNodeAttrs is attrs as the display encoding writes them for a node of this type: a typed
// block's defaults are its own schema's, every other node's are displayNodeDefaults'.
func displayNodeAttrs(nodeType string, attrs Attrs) Attrs {
	if defaults, typed := typedDefaults[nodeType]; typed {
		return withoutDefaults(attrs, defaults)
	}
	return withoutDefaults(attrs, displayNodeDefaults[nodeType])
}

// withoutDefaults is attrs with every attribute standing at its declared default left out, or
// nil where that leaves nothing. attrs itself is never written to, and is handed straight back
// where it holds no default at all, which is what a block the markdown gave no attribute to has.
func withoutDefaults(attrs, defaults Attrs) Attrs {
	if len(attrs) == 0 {
		return nil
	}
	if len(defaults) == 0 {
		return attrs
	}
	kept := len(attrs)
	for name, value := range attrs {
		if def, declared := defaults[name]; declared && atDisplayDefault(value, def) {
			kept--
		}
	}
	if kept == len(attrs) {
		return attrs
	}
	if kept == 0 {
		return nil
	}
	out := make(Attrs, kept)
	for name, value := range attrs {
		if def, declared := defaults[name]; declared && atDisplayDefault(value, def) {
			continue
		}
		out[name] = value
	}
	return out
}

// atDisplayDefault reports whether value is the attribute default def. A number is compared as a
// number, since one attribute reaches this both as the int a parse wrote and as the float64 a
// JSON round trip left; anything that is neither string, bool nor number is compared by its
// encoding, so a `string[]` default written `[]` matches an empty list of either Go type.
func atDisplayDefault(value, def any) bool {
	switch want := def.(type) {
	case nil:
		return value == nil
	case string:
		got, ok := value.(string)
		return ok && got == want
	case bool:
		got, ok := value.(bool)
		return ok && got == want
	case float64:
		got, ok := displayNumber(value)
		return ok && got == want
	}
	if value == nil {
		return false
	}
	gotJSON, err := json.Marshal(value)
	if err != nil {
		return false
	}
	wantJSON, err := json.Marshal(def)
	return err == nil && string(gotJSON) == string(wantJSON)
}

// displayNumber is value as a number, and whether it is one: an attribute reaches the encoding
// as the int a parse wrote, as the float64 a JSON round trip left, or as a json.Number.
func displayNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case float32:
		return float64(number), true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	}
	return 0, false
}
