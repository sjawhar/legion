package pmdoc

import (
	"bytes"
	"fmt"
	"strings"
)

type renderer struct {
	b                bytes.Buffer
	inlineCodeFence  string
	inlineCodePadded bool
	// labelBrackets is decided when a link opens, over the whole label it opens, and read by every
	// text node of that label. One node cannot judge it: what pairs with a bracket may sit in a
	// sibling node of the same link.
	labelBrackets labelBrackets
	// typedPrefix is the prefix the innermost typed block's lines are written at, or nil outside
	// one. A lone `:::` closes the block only as a line at that prefix, which only a paragraph
	// written at the same prefix can produce.
	typedPrefix *string
	// heldLineStart is the current line's first text character, held until the line is written.
	heldLineStart *lineCandidate
	// footnoteLineAt is where the last footnote definition's first line begins in the markdown,
	// and footnoteLabel is its label as written, so that the line is read after a reference to it.
	footnoteLineAt int
	footnoteLabel  string
	// inFootnote reports whether the blocks being written are inside a footnote definition.
	inFootnote bool
	// asteriskRule makes the next rule written `***` rather than `---` (list).
	asteriskRule bool
	// footnoteLabels is every footnote label the document defines, lowercased: text shaped like a
	// reference to one would read as that reference.
	footnoteLabels map[string]bool
	err            error
	blockOffsets   []BlockOffset
}

// BlockOffset identifies one rendered block in byte offsets of the markdown.
type BlockOffset struct {
	ID   string
	Type string
	From int
	To   int
}

// Render returns doc as canonical Markdown. A mark the rendering does not write
// (renderedMarkTypes) cannot change it: the document is rendered with its anchor marks stripped,
// which merges the text runs an anchor split, and an escape is decided over a whole run.
func Render(doc *Node) (string, error) {
	r, err := render(doc)
	if err != nil {
		return "", err
	}
	return r.b.String(), nil
}

// RenderWithBlockOffsets renders a document and records each identified block's
// byte range in the returned markdown.
func RenderWithBlockOffsets(doc *Node) (string, []BlockOffset, error) {
	r, err := render(doc)
	if err != nil {
		return "", nil, err
	}
	return r.b.String(), r.blockOffsets, nil
}

func render(doc *Node) (*renderer, error) {
	if doc == nil || doc.Type != "doc" {
		if doc == nil {
			return nil, fmt.Errorf("%w: Render wants a doc, got nil", ErrSchema)
		}
		return nil, fmt.Errorf("%w: Render wants a doc, got %q", ErrSchema, doc.Type)
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	// Anchor marks render nothing, but one that starts or ends inside a word splits its text into
	// runs, and an escape is decided within one run: rendering the document without them merges
	// the runs, so a mark never changes the markdown (`snake_case`, never `snake\_case`).
	doc = StripAnchorMarks(doc)
	if len(doc.Children) == 1 && doc.Children[0].Type == "paragraph" && len(doc.Children[0].Children) == 0 {
		return &renderer{}, nil
	}
	r := &renderer{footnoteLabels: definedFootnoteLabels(doc)}
	r.blocks(doc.Children, "")
	if r.err != nil {
		return nil, r.err
	}
	// A rule opening the document is written `---`, as it always was, except where that is
	// misread; there it is `***`, the same length. A `---` there opens front matter: a later line
	// that is `---` - a second rule, a code line - closes it and everything up to there reads as
	// front matter, and with no such line the browser editor's parser, having tried the front
	// matter to the document's end, reads no list, quote or footnote definition in the rest.
	if doc.Children[0].Type == "hr" {
		if front, _ := parseFrontmatterBlock(r.b.Bytes()); front != nil || holdsAContainerTheBrowserDrops(doc) {
			copy(r.b.Bytes(), "***")
		}
	}
	return r, nil
}

// holdsAContainerTheBrowserDrops reports whether doc holds, at its top level, a block the browser
// editor's parser reads no more after an unclosed `---` opener: a list, a quote or a footnote
// definition.
func holdsAContainerTheBrowserDrops(doc *Node) bool {
	for _, child := range doc.Children {
		switch child.Type {
		case "bullet_list", "ordered_list", "blockquote", "footnote_definition":
			return true
		}
	}
	return false
}

// definedFootnoteLabels is every label doc's footnote definitions carry, lowercased, since the
// browser editor's parser matches a reference to its definition whatever the case.
func definedFootnoteLabels(doc *Node) map[string]bool {
	labels := make(map[string]bool)
	for _, child := range doc.Children {
		if child.Type == "footnote_definition" {
			if label, ok := child.Attrs["label"].(string); ok {
				labels[strings.ToLower(label)] = true
			}
		}
	}
	return labels
}

func (r *renderer) blocks(nodes []*Node, prefix string) {
	for i, n := range nodes {
		if i > 0 {
			r.writeSyntax("\n" + strings.TrimRight(prefix, " ") + "\n")
		}
		r.block(n, prefix)
	}
	if len(nodes) > 0 {
		r.writeSyntax("\n")
	}
}

func (r *renderer) blocksNoTrailing(nodes []*Node, prefix string) {
	for i, n := range nodes {
		if i > 0 {
			r.writeSyntax("\n" + strings.TrimRight(prefix, " ") + "\n" + prefix)
		}
		r.block(n, prefix)
	}
}

func (r *renderer) block(n *Node, prefix string) {
	start := r.b.Len()
	blockOffset := -1
	if n.Type != "doc" && !isInlineNodeType(n.Type) {
		if id, ok := n.Attrs[BlockIDAttr].(string); ok && id != "" {
			blockOffset = len(r.blockOffsets)
			r.blockOffsets = append(r.blockOffsets, BlockOffset{ID: id, Type: n.Type, From: start})
		}
	}
	defer func() {
		if blockOffset >= 0 {
			r.blockOffsets[blockOffset].To = r.b.Len()
		}
	}()
	if r.err != nil {
		return
	}
	switch n.Type {
	case "paragraph":
		r.inline(n.Children, prefix)
	case "heading":
		level := int(num(n.Attrs["level"], 1))
		r.writeSyntax(strings.Repeat("#", level) + " ")
		r.headingInline(n.Children, prefix)
	case "blockquote":
		r.writeSyntax("> ")
		r.blocksNoTrailing(n.Children, prefix+"> ")
	case "bullet_list", "ordered_list":
		r.list(n, prefix)
	case "code_block":
		language, _ := n.Attrs["language"].(string)
		fence := codeBlockFence(n)
		r.writeSyntax(fence + language + "\n" + prefix)
		for _, child := range n.Children {
			if child.Type != "text" {
				r.err = fmt.Errorf("%w: code block contains %q", ErrSchema, child.Type)
				return
			}
			r.writeCodeText(child, prefix)
		}
		r.writeSyntax("\n" + prefix + fence)
	case "hr":
		if r.asteriskRule {
			r.writeSyntax("***")
			r.asteriskRule = false
		} else {
			r.writeSyntax("---")
		}
	case "frontmatter":
		for _, child := range n.Children {
			if child.Type != "text" {
				r.err = fmt.Errorf("%w: frontmatter contains %q", ErrSchema, child.Type)
				return
			}
			r.writeText(child.Text)
		}
	case "table":
		r.table(n, prefix)
	case "footnote_definition":
		label, _ := n.Attrs["label"].(string)
		r.footnoteLineAt, r.footnoteLabel = r.b.Len(), escapeFootnoteLabel(label)
		r.writeSyntax("[^" + escapeFootnoteLabel(label) + "]: ")
		r.inFootnote = true
		r.blocksNoTrailing(n.Children, prefix+"    ")
		r.inFootnote = false
	default:
		typ, typed := typedBlock(n.Type)
		if !typed {
			r.err = fmt.Errorf("%w: cannot render block %q", ErrSchema, n.Type)
			return
		}
		attrs, err := renderTypedAttributes(n, typ)
		if err != nil {
			r.err = err
			return
		}
		fence := strings.Repeat(":", typedFence(n, len(prefix)))
		r.writeSyntax(fence + n.Type + "{" + attrs + "}\n" + prefix)
		outer := r.typedPrefix
		r.typedPrefix = &prefix
		r.blocksNoTrailing(n.Children, prefix)
		r.typedPrefix = outer
		r.writeSyntax("\n" + prefix + fence)
	}
}

// typedFence is the number of colons a typed block whose lines start at column is written with:
// three, or one more than the longest line of colons inside it that could close it
// (closingColons). A renderer prefix is spaces and `> `, so its length is that column.
func typedFence(n *Node, column int) int {
	return max(3, closingColons(n, column)+1)
}

func (r *renderer) list(n *Node, prefix string) {
	start := 1
	if n.Type == "ordered_list" {
		start = int(num(n.Attrs["order"], 1))
	}
	for index, item := range n.Children {
		if item.Type != "list_item" {
			r.err = fmt.Errorf("%w: list contains %q", ErrSchema, item.Type)
			return
		}
		if index > 0 {
			r.writeSyntax("\n" + prefix)
			if n.Attrs["spread"] == true {
				r.writeSyntax("\n" + strings.TrimRight(prefix, " ") + "\n" + prefix)
			}
		}
		marker := "- "
		if n.Type == "ordered_list" {
			marker = fmt.Sprintf("%d. ", start+index)
		}
		r.writeSyntax(marker)
		if checked, ok := item.Attrs["checked"].(bool); ok {
			if checked {
				r.writeSyntax("[x] ")
			} else {
				r.writeSyntax("[ ] ")
			}
		}
		indent := prefix + strings.Repeat(" ", len(marker))
		for childIndex, child := range item.Children {
			// A tight item writes its blocks on consecutive lines, where a paragraph would run on
			// into a paragraph after it and underline itself with a rule's `---`. The browser
			// editor's writer puts a blank line between two paragraphs (the item then reads back
			// spread) and writes a rule `***`, and so does this renderer.
			afterParagraph := childIndex > 0 && item.Children[childIndex-1].Type == "paragraph"
			if childIndex > 0 {
				if item.Attrs["spread"] == true || afterParagraph && child.Type == "paragraph" {
					r.writeSyntax("\n" + strings.TrimRight(prefix, " ") + "\n" + indent)
				} else {
					r.writeSyntax("\n" + indent)
				}
			}
			r.asteriskRule = child.Type == "hr" && afterParagraph && item.Attrs["spread"] != true
			r.block(child, indent)
		}
	}
}

func (r *renderer) table(table *Node, prefix string) {
	if len(table.Children) == 0 || table.Children[0].Type != "table_header_row" {
		r.err = fmt.Errorf("%w: table requires a header row", ErrSchema)
		return
	}
	header := table.Children[0]
	r.tableRow(header, true, prefix)
	r.writeSyntax("\n" + prefix + "| ")
	for i, cell := range header.Children {
		if i > 0 {
			r.writeSyntax(" | ")
		}
		r.writeSyntax(tableAlignment(cell.Attrs["alignment"]))
	}
	r.writeSyntax(" |")
	for _, row := range table.Children[1:] {
		r.writeSyntax("\n" + prefix)
		r.tableRow(row, false, prefix)
	}
}

func (r *renderer) tableRow(row *Node, header bool, prefix string) {
	want := "table_cell"
	rowType := "table_row"
	if header {
		want = "table_header"
		rowType = "table_header_row"
	}
	if row.Type != rowType {
		r.err = fmt.Errorf("%w: unexpected table row %q", ErrSchema, row.Type)
		return
	}
	r.writeSyntax("| ")
	for i, cell := range row.Children {
		if cell.Type != want {
			r.err = fmt.Errorf("%w: table row contains %q", ErrSchema, cell.Type)
			return
		}
		if i > 0 {
			r.writeSyntax(" | ")
		}
		if len(cell.Children) != 1 || cell.Children[0].Type != "paragraph" {
			r.err = fmt.Errorf("%w: table cell requires one paragraph", ErrSchema)
			return
		}
		r.tableCellInline(cell.Children[0].Children, prefix)
	}
	r.writeSyntax(" |")
}

func (r *renderer) writeCodeText(node *Node, prefix string) {
	value := node.Text
	for offset := 0; offset < len(value); {
		newline := strings.IndexByte(value[offset:], '\n')
		if newline < 0 {
			r.writeText(value[offset:])
			return
		}
		end := offset + newline + 1
		r.writeText(value[offset:end])
		// A blank line does not take a footnote definition's indentation, and both parsers keep
		// what it holds as the code's, so a blank code line there, behind indentation alone, is
		// written without it.
		if !r.inFootnote || strings.TrimSpace(prefix) != "" || !blankLineAhead(value[end:]) {
			r.writeSyntax(prefix)
		}
		offset = end
	}
}

// blankLineAhead reports whether text's first line holds only spaces and tabs before its line
// ending.
func blankLineAhead(text string) bool {
	end := strings.IndexByte(text, '\n')
	return end >= 0 && strings.Trim(text[:end], " \t\r") == ""
}

func codeBlockFence(node *Node) string {
	longest := 0
	for _, child := range node.Children {
		for run, char := 0, 0; ; {
			if char < len(child.Text) && child.Text[char] == '`' {
				run++
				if run > longest {
					longest = run
				}
				char++
				continue
			}
			if char == len(child.Text) {
				break
			}
			run = 0
			char++
		}
	}
	if longest < 2 {
		longest = 2
	}
	return strings.Repeat("`", longest+1)
}

func isBareURLLink(node *Node, marks []Mark, escapePipes bool) bool {
	for _, mark := range marks {
		if mark.Type != "link" {
			continue
		}
		href, _ := mark.Attrs["href"].(string)
		title, _ := mark.Attrs["title"].(string)
		return (!escapePipes || !strings.Contains(node.Text, "|")) && href == node.Text && title == "" && isBareAutolink(node.Text)
	}
	return false
}

func isBareAutolink(value string) bool {
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return false
	}
	if strings.ContainsAny(value, " \t\r\n()<>") {
		return false
	}
	return !strings.ContainsAny(value[len(value)-1:], ".,!?;:")
}

func containsMark(marks []Mark, markType string) bool {
	for _, mark := range marks {
		if mark.Type == markType {
			return true
		}
	}
	return false
}

func withoutMark(marks []Mark, markType string) []Mark {
	out := marks[:0]
	for _, mark := range marks {
		if mark.Type != markType {
			out = append(out, mark)
		}
	}
	return out
}

func nodeHasMark(node *Node, markType string) bool {
	for _, mark := range node.Marks {
		if mark.Type == markType {
			return true
		}
	}
	return false
}

// renderedMarkTypes are the marks canonical Markdown writes. Every other mark the schema allows
// (markTypes) is an anchor, invisible to the rendering, and StripAnchorMarks removes exactly the
// marks that are not here: what renders is one list, not two of opposite polarity that a new
// invisible mark could fall between.
var renderedMarkTypes = map[string]bool{
	"link": true, "strong": true, "emphasis": true, "strike_through": true, "inlineCode": true,
}

// writtenMarks is the marks a node's text is written under: its visible marks, less a bare URL's
// link, which the parser links again on its own. A node that is not text is written under none.
func writtenMarks(node *Node, escapePipes bool) []Mark {
	if node.Type != "text" {
		return nil
	}
	marks := visibleMarks(node.Marks)
	if isBareURLLink(node, marks, escapePipes) {
		marks = withoutMark(marks, "link")
	}
	return marks
}

// adjacentDelimiter is the delimiter character of the innermost of marks opened or closed beside a
// text, the one written next to it, or 0 when that mark is not written with a delimiter run.
func adjacentDelimiter(marks []Mark) byte {
	if len(marks) == 0 {
		return 0
	}
	switch marks[len(marks)-1].Type {
	case "strong", "emphasis":
		return '*'
	case "strike_through":
		return '~'
	}
	return 0
}

func visibleMarks(marks []Mark) []Mark {
	out := make([]Mark, 0, len(marks))
	for _, mark := range marks {
		if renderedMarkTypes[mark.Type] {
			out = append(out, mark)
		}
	}
	sortMarks(out)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if renderMarkRank(out[j].Type) < renderMarkRank(out[i].Type) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func sharedMarks(left, right []Mark) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for i := range limit {
		if left[i].Type != right[i].Type || !attrsEqual(left[i].Attrs, right[i].Attrs) {
			return i
		}
	}
	return limit
}

func renderMarkRank(markType string) int {
	switch markType {
	case "link":
		return 0
	case "strong":
		return 1
	case "emphasis":
		return 2
	case "strike_through":
		return 3
	case "inlineCode":
		return 4
	default:
		return 5
	}
}

func openMark(mark Mark) string {
	switch mark.Type {
	case "link":
		return "["
	case "strong":
		return "**"
	case "emphasis":
		return "*"
	case "strike_through":
		return "~~"
	case "inlineCode":
		return "`"
	default:
		return ""
	}
}

func closeMark(mark Mark, escapePipes bool) string {
	if mark.Type == "link" {
		href, _ := mark.Attrs["href"].(string)
		title, _ := mark.Attrs["title"].(string)
		return "](" + escapeLinkDestination(href, escapePipes) + titleSuffix(title, escapePipes) + ")"
	}
	return openMark(mark)
}

func escapeLinkDestination(href string, escapePipes bool) string {
	if escapePipes {
		href = escapeTableLinkDestination(href)
	}
	if strings.ContainsAny(href, " ()") {
		return "<" + href + ">"
	}
	return href
}

func titleSuffix(title string, escapePipes bool) string {
	if title == "" {
		return ""
	}
	title = strings.ReplaceAll(title, "\\", "\\\\")
	title = strings.ReplaceAll(title, "\"", "\\\"")
	return " \"" + escapeTablePipes(title, escapePipes) + "\""
}

func escapeTablePipes(value string, escapePipes bool) string {
	if !escapePipes {
		return value
	}
	return strings.ReplaceAll(value, "|", "\\|")
}

func escapeTableHTMLPipes(value string, escapePipes bool) string {
	if !escapePipes {
		return value
	}
	return strings.ReplaceAll(value, "|", "&#124;")
}

func escapeTableLinkDestination(value string) string {
	if !strings.ContainsAny(value, "\\|") {
		return value
	}
	var rendered strings.Builder
	rendered.Grow(len(value))
	for index := range len(value) {
		char := value[index]
		if char == '\\' && index+1 < len(value) && isASCIIPunctuation(value[index+1]) {
			rendered.WriteByte('\\')
		}
		if char == '|' {
			rendered.WriteByte('\\')
		}
		rendered.WriteByte(char)
	}
	return rendered.String()
}
func escapeFootnoteLabel(label string) string {
	return escapeTableSyntaxPipes(label)
}

func escapeTableSyntaxPipes(value string) string {
	if !strings.Contains(value, "|") {
		return value
	}
	var escaped bool
	var rendered strings.Builder
	rendered.Grow(len(value))
	for _, char := range value {
		if char == '|' && !escaped {
			rendered.WriteByte('\\')
		}
		rendered.WriteRune(char)
		escaped = char == '\\'
	}
	return rendered.String()
}

func tableAlignment(value any) string {
	alignment, _ := value.(string)
	switch alignment {
	case "center":
		return ":---:"
	case "right":
		return "---:"
	default:
		return ":---"
	}
}

func num(value any, fallback float64) float64 {
	switch number := value.(type) {
	case float64:
		return number
	case float32:
		return float64(number)
	case int:
		return float64(number)
	case int64:
		return float64(number)
	default:
		return fallback
	}
}
