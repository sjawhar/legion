package pmdoc

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// The first screen is the opening of a document, drawn as HTML by the server so a reader meets
// their document in the page's own bytes rather than after the editor has been downloaded, built
// and connected (dispatch://INFRA-341, R11).
//
// It is the same draw the page makes for itself before its editor exists
// (`web/src/features/doc/DocumentPreview.tsx`): the document's tree through ProseMirror's
// DOMSerializer over the Proof schema, with each block's `data-block-id` on it, and the two
// passes that painter runs afterwards (`web/src/features/refs/paint-block.ts`) - a task item's
// checkbox drawn, and a tight list item's paragraph unwrapped. The page takes these blocks as its
// own preview and paints nothing over them (`features/doc/first-screen.ts`), so the two renderers
// must agree block for block; `first_screen_test.go` and
// `web/src/features/doc/first-screen.test.ts` hold both to one set of fixtures.
const (
	// FirstScreenBlocks is the most top-level blocks the first screen carries.
	FirstScreenBlocks = 30
	// FirstScreenBytes is the most HTML it carries. A document whose opening block alone is
	// longer is still drawn, because a reader with no first block has no first screen at all.
	FirstScreenBytes = 16 << 10
)

// FirstScreenHTML is the HTML of doc's opening blocks: up to FirstScreenBlocks of them and no
// more than FirstScreenBytes, cut at whichever comes first, and the whole document when it fits.
// complete says the HTML holds every top-level block the document has, which is what lets the
// page know it is reading the whole of it.
//
// It takes a document node (a tree the server already holds) and writes no error: a node outside
// the Proof schema is skipped rather than refused, since a page drawing most of a document is
// worth more to its reader than one drawing none of it, and the editor arriving behind this draw
// is the document's real renderer.
func FirstScreenHTML(doc *Node) (html string, complete bool) {
	if doc == nil {
		return "", false
	}
	var out strings.Builder
	var block strings.Builder
	drawn := 0
	for index, child := range doc.Children {
		if drawn == FirstScreenBlocks {
			return out.String(), false
		}
		block.Reset()
		writeNodeHTML(&block, child)
		if block.Len() == 0 {
			continue
		}
		if out.Len() > 0 && out.Len()+block.Len() > FirstScreenBytes {
			return out.String(), false
		}
		out.WriteString(block.String())
		drawn++
		if out.Len() >= FirstScreenBytes && index < len(doc.Children)-1 {
			return out.String(), false
		}
	}
	return out.String(), true
}

// writeNodeHTML writes one node as the page's own serializer draws it. An unknown node writes
// nothing, as an unknown node draws nothing on the page either.
func writeNodeHTML(out *strings.Builder, n *Node) {
	if n == nil {
		return
	}
	switch n.Type {
	case "text":
		writeTextHTML(out, n)
	case "paragraph":
		writeElement(out, "p", nil, n, writeInlineChildren)
	case "heading":
		writeHeadingHTML(out, n)
	case "blockquote":
		writeElement(out, "blockquote", nil, n, writeBlockChildren)
	case "bullet_list":
		writeElement(out, "ul", attrs{{"data-spread", boolText(n.Attrs["spread"])}}, n, writeBlockChildren)
	case "ordered_list":
		writeOrderedListHTML(out, n)
	case "list_item":
		writeListItemHTML(out, n)
	case "code_block":
		writeCodeBlockHTML(out, n)
	case "frontmatter":
		writeFencedHTML(out, attrs{{"data-frontmatter", "true"}}, n)
	case "hr":
		writeVoidElement(out, "hr", nil, n)
	case "table":
		writeTableHTML(out, n)
	case "table_header_row":
		writeElement(out, "tr", attrs{{"data-is-header", "true"}}, n, writeBlockChildren)
	case "table_row":
		writeElement(out, "tr", nil, n, writeBlockChildren)
	case "table_header":
		writeElement(out, "th", cellAttributes(n), n, writeBlockChildren)
	case "table_cell":
		writeElement(out, "td", cellAttributes(n), n, writeBlockChildren)
	case "footnote_definition":
		writeFootnoteDefinitionHTML(out, n)
	case "footnote_reference":
		label := stringAttr(n.Attrs["label"])
		writeOpenTag(out, "sup", attrs{{"data-label", label}, {"data-type", "footnote_reference"}}, n)
		writeEscapedText(out, label)
		out.WriteString("</sup>")
	case "image":
		writeImageHTML(out, n)
	case "hardbreak":
		writeHardbreakHTML(out, n)
	case "html":
		value := stringAttr(n.Attrs["value"])
		writeOpenTag(out, "span", attrs{{"data-value", value}, {"data-type", "html"}}, n)
		writeEscapedText(out, value)
		out.WriteString("</span>")
	default:
		if typ, ok := typedBlock(n.Type); ok {
			writeTypedBlockHTML(out, n, typ)
		}
	}
}

// htmlAttr is one attribute of one element: its name and the value the page writes.
type htmlAttr struct{ name, value string }

// attrs is one element's attributes, in the order the page's serializer writes them.
type attrs []htmlAttr

func writeBlockChildren(out *strings.Builder, n *Node) {
	for _, child := range n.Children {
		writeNodeHTML(out, child)
	}
}

func writeInlineChildren(out *strings.Builder, n *Node) {
	for _, child := range n.Children {
		writeNodeHTML(out, child)
	}
}

// writeElement writes one element with its block id and its children.
func writeElement(out *strings.Builder, tag string, extra attrs, n *Node, children func(*strings.Builder, *Node)) {
	writeOpenTag(out, tag, extra, n)
	children(out, n)
	out.WriteString("</")
	out.WriteString(tag)
	out.WriteByte('>')
}

// writeVoidElement writes an element HTML has no closing tag for, as innerHTML spells one.
func writeVoidElement(out *strings.Builder, tag string, extra attrs, n *Node) {
	writeOpenTag(out, tag, extra, n)
}

// writeOpenTag writes an element's start tag: the node type's own attributes, then the block id
// the schema's wrapper adds last (`editor/schema/block-ids.ts`).
func writeOpenTag(out *strings.Builder, tag string, extra attrs, n *Node) {
	out.WriteByte('<')
	out.WriteString(tag)
	for _, attribute := range extra {
		writeAttribute(out, attribute.name, attribute.value)
	}
	if n != nil {
		if id, ok := n.Attrs[BlockIDAttr].(string); ok && id != "" {
			writeAttribute(out, "data-block-id", id)
		}
	}
	out.WriteByte('>')
}

func writeAttribute(out *strings.Builder, name, value string) {
	out.WriteByte(' ')
	out.WriteString(name)
	out.WriteString(`="`)
	writeEscapedAttribute(out, value)
	out.WriteByte('"')
}

func writeHeadingHTML(out *strings.Builder, n *Node) {
	level := 1
	if value, ok := numberAttr(n.Attrs["level"]); ok && value >= 1 && value <= 6 {
		level = value
	}
	id := stringAttr(n.Attrs["id"])
	if id == "" {
		id = headingID(n)
	}
	tag := "h" + strconv.Itoa(level)
	writeElement(out, tag, attrs{{"id", id}}, n, writeInlineChildren)
}

// headingID is the id milkdown's heading gives a heading that carries none: its text, lowercased,
// trimmed, with each run of whitespace a hyphen (`defaultHeadingIdGenerator`).
func headingID(n *Node) string {
	return strings.Join(strings.Fields(strings.ToLower(nodeText(n))), "-")
}

func writeOrderedListHTML(out *strings.Builder, n *Node) {
	list := attrs{}
	if order, ok := numberAttr(n.Attrs["order"]); ok && order != 1 {
		list = append(list, htmlAttr{"start", strconv.Itoa(order)})
	}
	list = append(list, htmlAttr{"data-spread", boolText(n.Attrs["spread"])})
	writeElement(out, "ol", list, n, writeBlockChildren)
}

// writeListItemHTML writes one list item, with the two passes the page runs over a painted body
// (`refs/paint-block.ts`): a task item carries a drawn checkbox inside its first child, and a
// tight item's one paragraph is unwrapped, so the item reads as a line rather than a paragraph.
func writeListItemHTML(out *strings.Builder, n *Node) {
	item := attrs{}
	checked, isTask := n.Attrs["checked"].(bool)
	if isTask {
		item = append(item, htmlAttr{"data-item-type", "task"})
	}
	item = append(item,
		htmlAttr{"data-label", stringAttr(n.Attrs["label"])},
		htmlAttr{"data-list-type", stringAttr(n.Attrs["listType"])},
		htmlAttr{"data-spread", boolText(n.Attrs["spread"])},
	)
	if isTask {
		item = append(item, htmlAttr{"data-checked", strconv.FormatBool(checked)})
	}
	writeOpenTag(out, "li", item, n)
	tight := n.Attrs["spread"] == false && len(n.Children) == 1 && n.Children[0] != nil &&
		n.Children[0].Type == "paragraph"
	for index, child := range n.Children {
		if index == 0 && isTask {
			writeTaskCheckbox(out, checked)
		}
		if index == 0 && tight {
			writeInlineChildren(out, child)
			continue
		}
		writeNodeHTML(out, child)
	}
	out.WriteString("</li>")
}

// writeTaskCheckbox writes the box `drawTaskCheckboxes` draws: disabled, because that is the only
// native checkbox that is truly read-only, and unlabelled, because the item's own words follow it.
func writeTaskCheckbox(out *strings.Builder, checked bool) {
	out.WriteString(`<input type="checkbox" disabled="" data-markdown-task-checkbox=""`)
	if checked {
		out.WriteString(` checked=""`)
	}
	out.WriteByte('>')
}

func writeCodeBlockHTML(out *strings.Builder, n *Node) {
	fence := attrs{}
	if language := stringAttr(n.Attrs["language"]); language != "" {
		fence = append(fence, htmlAttr{"data-language", language})
	}
	writeFencedHTML(out, fence, n)
}

// writeFencedHTML writes a code block or the frontmatter block: a `pre` carrying the block id and
// a `code` holding the text. A fenced `mermaid` block is its source here, as the page's own draw
// has it until the diagram is drawn over it.
func writeFencedHTML(out *strings.Builder, extra attrs, n *Node) {
	writeOpenTag(out, "pre", extra, n)
	out.WriteString("<code>")
	writeEscapedText(out, nodeText(n))
	out.WriteString("</code></pre>")
}

func writeTableHTML(out *strings.Builder, n *Node) {
	writeOpenTag(out, "table", nil, n)
	out.WriteString("<tbody>")
	writeBlockChildren(out, n)
	out.WriteString("</tbody></table>")
}

// cellAttributes is what prosemirror-tables writes on a cell: the spans it carries beyond one,
// its stored widths, and the column's alignment, which is `left` where the column aligns nowhere.
func cellAttributes(n *Node) attrs {
	cell := attrs{}
	if span, ok := numberAttr(n.Attrs["colspan"]); ok && span != 1 {
		cell = append(cell, htmlAttr{"colspan", strconv.Itoa(span)})
	}
	if span, ok := numberAttr(n.Attrs["rowspan"]); ok && span != 1 {
		cell = append(cell, htmlAttr{"rowspan", strconv.Itoa(span)})
	}
	if widths, ok := n.Attrs["colwidth"].([]any); ok && len(widths) > 0 {
		parts := make([]string, 0, len(widths))
		for _, width := range widths {
			if value, ok := numberAttr(width); ok {
				parts = append(parts, strconv.Itoa(value))
			}
		}
		if len(parts) > 0 {
			cell = append(cell, htmlAttr{"data-colwidth", strings.Join(parts, ",")})
		}
	}
	alignment := stringAttr(n.Attrs["alignment"])
	if alignment == "" {
		alignment = "left"
	}
	return append(cell, htmlAttr{"data-proof-text-align", alignment})
}

func writeFootnoteDefinitionHTML(out *strings.Builder, n *Node) {
	label := stringAttr(n.Attrs["label"])
	writeOpenTag(out, "dl", attrs{{"data-label", label}, {"data-type", "footnote_definition"}}, n)
	out.WriteString("<dt>")
	writeEscapedText(out, label)
	out.WriteString("</dt><dd>")
	writeBlockChildren(out, n)
	out.WriteString("</dd></dl>")
}

// writeImageHTML writes a picture as the page's own serializer draws it: a `dispatch://` source
// names an artifact rather than a file, and draws as the empty anchor the picture serializer
// puts in its place (`refs/markdown-engine.ts`), which the page fills with the picture once it
// has read the artifact; every other source draws as an `img`, under the same scheme rule a link
// is held to, so a `javascript:` picture reaches the page with no source at all.
func writeImageHTML(out *strings.Builder, n *Node) {
	source := stringAttr(n.Attrs["src"])
	if dispatchHref(source) != "" {
		writeOpenTag(out, "a", attrs{{"href", strings.TrimSpace(source)}}, nil)
		out.WriteString("</a>")
		return
	}
	writeVoidElement(out, "img", attrs{
		{"src", safeHref(source)},
		{"alt", stringAttr(n.Attrs["alt"])},
		{"title", stringAttr(n.Attrs["title"])},
	}, nil)
}

// safeHref is the address the page lets an anchor or a picture carry: milkdown's own
// `sanitizeLinkHref`. An address with no scheme is kept as written; one whose scheme is not a
// scheme the page trusts is dropped, so nothing a document's author wrote can run as script.
// The characters a browser ignores inside a scheme are stripped before it is read, so
// `java\nscript:` is read as `javascript:` here as it is there.
func safeHref(href string) string {
	trimmed := strings.TrimSpace(href)
	if trimmed == "" {
		return ""
	}
	normalized := strings.Map(func(r rune) rune {
		if r <= 32 || (r >= 127 && r <= 160) || (r >= 8203 && r <= 8205) ||
			r == 8232 || r == 8233 || r == 65279 {
			return -1
		}
		return r
	}, trimmed)
	colon := strings.IndexByte(normalized, ':')
	if colon <= 0 || !isScheme(normalized[:colon]) {
		return trimmed
	}
	switch strings.ToLower(normalized[:colon]) {
	case "http", "https", "mailto", "tel", "ftp":
		return trimmed
	}
	return ""
}

// isScheme reports whether text spells a URL scheme, `[a-z][a-z0-9+.-]*` case-insensitively: an
// address whose first colon is not a scheme's (a path holding one) carries no scheme at all.
func isScheme(text string) bool {
	for index := range len(text) {
		letter := text[index] | 0x20
		switch {
		case letter >= 'a' && letter <= 'z':
		case index > 0 && (text[index] >= '0' && text[index] <= '9' ||
			text[index] == '+' || text[index] == '.' || text[index] == '-'):
		default:
			return false
		}
	}
	return len(text) > 0
}

// dispatchHref is the `dispatch://` address the page carries beside a sanitized one
// (`dispatch-links.ts`), so its own router can rewrite the anchor to its route; "" for any other
// address.
func dispatchHref(href string) string {
	trimmed := strings.TrimSpace(href)
	if len(trimmed) < len("dispatch://") || !strings.EqualFold(trimmed[:len("dispatch://")], "dispatch://") {
		return ""
	}
	return trimmed
}

func writeHardbreakHTML(out *strings.Builder, n *Node) {
	if inline, ok := n.Attrs["isInline"].(bool); ok && inline {
		writeOpenTag(out, "span", attrs{{"data-is-inline", "true"}}, nil)
		out.WriteString(" </span>")
		return
	}
	writeVoidElement(out, "br", attrs{{"data-is-inline", "false"}}, nil)
}

// writeTypedBlockHTML writes a typed block as the page draws it: a decision block is the shell
// its card mounts into, whose header and footer hold no document text, and every other typed
// block is the summary header and content hole `renderTypedBlock` draws
// (`web/src/features/doc/typed-block-drawing.ts`). Both carry the block's client-owned attributes
// as `data-proof-block-attr-*`, which is the schema wrapper's own stamp.
func writeTypedBlockHTML(out *strings.Builder, n *Node, typ BlockTypeSchema) {
	frame := attrs{
		{"class", "proof-typed-block proof-typed-block-" + n.Type},
		{"data-proof-block-type", n.Type},
	}
	if n.Type == "ask" {
		frame = append(frame,
			htmlAttr{"data-dispatch-ask-urgency", attributeChoice(n, typ, "urgency")},
			htmlAttr{"data-dispatch-ask-state", attributeChoice(n, typ, "state")},
		)
	}
	for _, name := range sortedAttributeNames(typ) {
		definition := typ.Attributes[name]
		value, present := n.Attrs[name]
		if definition.Server || !present || value == nil {
			continue
		}
		frame = append(frame, htmlAttr{
			"data-proof-block-attr-" + name, attributeText(definition, value),
		})
	}
	writeOpenTag(out, "section", frame, n)
	if n.Type == "ask" {
		out.WriteString(`<div class="not-prose" contenteditable="false" data-dispatch-ask-header=""></div>`)
		out.WriteString(`<div data-proof-block-content="">`)
		writeBlockChildren(out, n)
		out.WriteString(`</div><div class="not-prose" contenteditable="false" data-dispatch-ask-footer=""></div></section>`)
		return
	}
	writeTypedBlockHeader(out, n, typ)
	out.WriteString(`<div data-proof-block-content="">`)
	writeBlockChildren(out, n)
	out.WriteString(`</div></section>`)
}

// writeTypedBlockHeader writes the label a reader reads above a typed block's content: the block's
// name, a `kind` as a badge, a `title` as the header's own text, and any other attribute its
// author wrote as a quiet name/value pair. What the author did not write never appears, and nor
// does an attribute the schema marks `server`.
func writeTypedBlockHeader(out *strings.Builder, n *Node, typ BlockTypeSchema) {
	out.WriteString(`<header contenteditable="false" data-proof-block-summary=""><span data-proof-block-name="">`)
	writeEscapedText(out, n.Type)
	out.WriteString("</span>")
	var rest []string
	for _, name := range sortedAttributeNames(typ) {
		if typ.Attributes[name].Server || name == BlockIDAttr {
			continue
		}
		value := attributeDisplay(n.Attrs[name])
		if value == "" {
			continue
		}
		switch name {
		case "kind":
			out.WriteString(`<span data-proof-block-attribute="kind" data-proof-block-badge="">`)
			writeEscapedText(out, value)
			out.WriteString("</span>")
		case "title":
			out.WriteString(`<span data-proof-block-attribute="title" data-proof-block-title="">`)
			writeEscapedText(out, value)
			out.WriteString("</span>")
		default:
			rest = append(rest, name)
		}
	}
	if len(rest) > 0 {
		out.WriteString(`<dl data-proof-block-attributes="">`)
		for _, name := range rest {
			out.WriteString("<dt>")
			writeEscapedText(out, name)
			out.WriteString(`</dt><dd data-proof-block-attribute="`)
			writeEscapedAttribute(out, name)
			out.WriteString(`">`)
			writeEscapedText(out, attributeDisplay(n.Attrs[name]))
			out.WriteString("</dd>")
		}
		out.WriteString("</dl>")
	}
	out.WriteString("</header>")
}

// sortedAttributeNames orders a typed block's attributes by name, so one tree writes one page
// whichever order Go's map hands them back in.
func sortedAttributeNames(typ BlockTypeSchema) []string {
	names := make([]string, 0, len(typ.Attributes))
	for name := range typ.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// attributeChoice is an enum attribute's value, or its schema default where the node carries none.
func attributeChoice(n *Node, typ BlockTypeSchema, name string) string {
	if value := stringAttr(n.Attrs[name]); value != "" {
		return value
	}
	return stringAttr(typ.Attributes[name].Default)
}

// attributeText is a typed block's attribute as its DOM attribute carries it
// (`block-schema.ts`'s `attributeText`).
func attributeText(definition BlockAttributeSchema, value any) string {
	switch definition.Kind {
	case "string[]":
		encoded, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		return string(encoded)
	case "block[]":
		items, _ := stringSlice(value)
		return strings.Join(items, " ")
	default:
		return attributeDisplay(value)
	}
}

// attributeDisplay is `String(value)` as the page spells it, and a list as its JSON
// (`typed-block-drawing.ts`'s `attributeValue`).
func attributeDisplay(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

// writeTextHTML writes one text node inside the marks it carries, innermost last, as
// DOMSerializer nests them.
func writeTextHTML(out *strings.Builder, n *Node) {
	for _, mark := range n.Marks {
		writeMarkOpenTag(out, mark)
	}
	writeEscapedText(out, n.Text)
	for index := len(n.Marks) - 1; index >= 0; index-- {
		out.WriteString("</")
		out.WriteString(markTag(n.Marks[index].Type))
		out.WriteByte('>')
	}
}

func markTag(markType string) string {
	switch markType {
	case "strong":
		return "strong"
	case "emphasis":
		return "em"
	case "inlineCode":
		return "code"
	case "strike_through":
		return "del"
	case "link":
		return "a"
	default:
		return "span"
	}
}

// writeMarkOpenTag writes one mark's start tag. A record mark - a comment, a suggestion, a
// flagged or approved span, an authorship, a decision's own text - is the `span` its stylesheet
// finds by `data-proof` or `data-dispatch` (`proof-marks.ts`, `dispatch-marks.ts`).
func writeMarkOpenTag(out *strings.Builder, mark Mark) {
	switch mark.Type {
	case "strong", "emphasis", "inlineCode", "strike_through":
		out.WriteByte('<')
		out.WriteString(markTag(mark.Type))
		out.WriteByte('>')
	case "link":
		href := stringAttr(mark.Attrs["href"])
		out.WriteString("<a")
		// A `dispatch://` target rides beside the sanitized address, which is where the page's
		// own router reads it from (`dispatch-links.ts`).
		if target := dispatchHref(href); target != "" {
			writeAttribute(out, "data-dispatch-href", target)
		}
		writeAttribute(out, "href", safeHref(href))
		if title := stringAttr(mark.Attrs["title"]); title != "" {
			writeAttribute(out, "title", title)
		}
		out.WriteByte('>')
	case "dispatchAsk":
		writeRecordMarkOpenTag(out, "data-dispatch", "ask", mark)
	default:
		writeRecordMarkOpenTag(out, "data-proof", strings.TrimPrefix(strings.ToLower(mark.Type), "proof"), mark)
	}
}

func writeRecordMarkOpenTag(out *strings.Builder, kindAttr, kind string, mark Mark) {
	out.WriteString("<span")
	writeAttribute(out, kindAttr, kind)
	if id := stringAttr(mark.Attrs["id"]); id != "" {
		writeAttribute(out, "data-id", id)
	}
	if by := stringAttr(mark.Attrs["by"]); by != "" {
		writeAttribute(out, "data-by", by)
	}
	out.WriteByte('>')
}

// nodeText is the text a node and its descendants hold, which is what `textContent` answers for
// the element it draws as.
func nodeText(n *Node) string {
	if n == nil {
		return ""
	}
	if n.Type == "text" {
		return n.Text
	}
	var out strings.Builder
	writeNodeText(&out, n)
	return out.String()
}

func writeNodeText(out *strings.Builder, n *Node) {
	if n == nil {
		return
	}
	if n.Type == "text" {
		out.WriteString(n.Text)
		return
	}
	for _, child := range n.Children {
		writeNodeText(out, child)
	}
}

// writeEscapedText writes text as an element's content, as the browser spells it back.
func writeEscapedText(out *strings.Builder, value string) {
	for index := range len(value) {
		switch value[index] {
		case '&':
			out.WriteString("&amp;")
		case '<':
			out.WriteString("&lt;")
		case '>':
			out.WriteString("&gt;")
		default:
			out.WriteByte(value[index])
		}
	}
}

// writeEscapedAttribute writes an attribute's value, as the browser spells it back.
func writeEscapedAttribute(out *strings.Builder, value string) {
	for index := range len(value) {
		switch value[index] {
		case '&':
			out.WriteString("&amp;")
		case '"':
			out.WriteString("&quot;")
		case '<':
			out.WriteString("&lt;")
		case '>':
			out.WriteString("&gt;")
		default:
			out.WriteByte(value[index])
		}
	}
}

func stringAttr(value any) string {
	text, _ := value.(string)
	return text
}

func boolText(value any) string {
	flag, _ := value.(bool)
	return strconv.FormatBool(flag)
}

// numberAttr reads an attribute a tree carries as a JSON number or a Go int.
func numberAttr(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}
