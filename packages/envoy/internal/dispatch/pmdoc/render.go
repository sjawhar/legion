package pmdoc

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
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
	r := &renderer{}
	r.blocks(doc.Children, "")
	if r.err != nil {
		return nil, r.err
	}
	// A rule opening the document is written `---`, which the front-matter reader takes as an
	// opener when any later line is also `---` - a second rule, a code line - and reads everything
	// up to that line as front matter. It is written `***` there, the same length, and `---`
	// everywhere else, as it has always been written.
	if doc.Children[0].Type == "hr" && parseFrontmatterBlock(r.b.Bytes()) != nil {
		copy(r.b.Bytes(), "***")
	}
	return r, nil
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
		r.writeSyntax("---")
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
		r.blocksNoTrailing(n.Children, prefix+"    ")
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
		r.writeSyntax(":::" + n.Type + "{" + attrs + "}\n" + prefix)
		outer := r.typedPrefix
		r.typedPrefix = &prefix
		r.blocksNoTrailing(n.Children, prefix)
		r.typedPrefix = outer
		r.writeSyntax("\n" + prefix + ":::")
	}
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
			if childIndex > 0 {
				if item.Attrs["spread"] == true {
					r.writeSyntax("\n" + strings.TrimRight(prefix, " ") + "\n" + indent)
				} else {
					r.writeSyntax("\n" + indent)
				}
			}
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

// inlineContext is where one run of inline nodes is written.
type inlineContext struct {
	// tableCell reports whether the run is a table cell, whose pipes are syntax.
	tableCell bool
	// startsLine reports whether the run begins a markdown line of its own, so that a marker at
	// the start of its text would open a block.
	startsLine bool
	// heading reports whether the run is a heading's text, whose trailing run of `#` would read
	// as the heading's closing sequence.
	heading bool
}

// inline writes a paragraph's children, which begin their own markdown line.
func (r *renderer) inline(nodes []*Node, prefix string) {
	r.inlineWithEscapes(nodes, prefix, inlineContext{startsLine: true})
}

// headingInline writes a heading's children. The `## ` is already on the line, so nothing in the
// text can open a block of its own.
func (r *renderer) headingInline(nodes []*Node, prefix string) {
	r.inlineWithEscapes(nodes, prefix, inlineContext{heading: true})
}

// tableCellInline writes one cell, which sits after a `|` on a line the table owns.
func (r *renderer) tableCellInline(nodes []*Node, prefix string) {
	r.inlineWithEscapes(nodes, prefix, inlineContext{tableCell: true})
}

// inlinePosition is where the inline writer stands inside one textblock.
type inlinePosition struct {
	// atLineStart reports whether the next text character is the first on a markdown line of its
	// own, after any mark's marker, so that the line may read as a block. A heading's or a table
	// cell's text does not begin one.
	atLineStart bool
	// atTextStart reports whether the next character starts an inline run's text or a new line in
	// it, where the writer has always escaped a list, heading, quote or ordered-list marker - in a
	// heading, a cell and inside a mark too. Those escapes read back, and dropping them would
	// rewrite every stored document that carries one, so they stay exactly where they were.
	atTextStart bool
	// afterLine reports whether any line precedes this one in the textblock. A setext underline
	// needs a line to underline, and it may have ended at a newline in an earlier text node or at
	// a hard break, neither of which the current node's own offsets can show.
	afterLine bool
	// afterMarker reports whether a mark's marker is written on this line before its text.
	afterMarker bool
}

// lineCandidate is a line's first text character, when it is one that can begin a block form
// only the whole line decides: the writer writes it unescaped, finishes the line, and then asks
// the parser (endLine).
type lineCandidate struct {
	// at is where the character is written in the markdown.
	at int
	// char is the character.
	char rune
	// afterLine reports whether the line continues its textblock, so the line before is read
	// with it.
	afterLine bool
	// atTypedPrefix reports whether the textblock is written at the prefix of the typed block
	// around it, where a lone `:::` line closes that block.
	atTypedPrefix bool
	// prefix is the prefix the textblock's lines are written at.
	prefix string
}

// inlineWithEscapes writes one textblock's inline nodes. Delimiters in text the escape rules leave
// alone can still pair into a mark the text never had, or break one it has: GFM reads a single
// tilde as a strikethrough delimiter and refuses a run of three, and asterisks and underscores pair
// across a hard break, around a reference or inside a link label. A run whose text holds one is
// read back after it is written, and when it does not come back as written it is written again
// with its text's tildes escaped, then with its asterisks and underscores escaped as well - each
// kept only if it reads back, so a run that fails for another reason keeps the bytes it had.
func (r *renderer) inlineWithEscapes(nodes []*Node, prefix string, context inlineContext) {
	from := r.b.Len()
	r.writeInlineRun(nodes, prefix, context, delimitersAsRuled)
	held := delimitersInText(nodes)
	if r.err != nil || held == delimitersAsRuled || r.runReadsBack(from, prefix, nodes) {
		return
	}
	written := string(r.b.Bytes()[from:])
	for escapes := held; escapes <= delimitersAll; escapes++ {
		r.b.Truncate(from)
		r.writeInlineRun(nodes, prefix, context, escapes)
		if r.err == nil && r.runReadsBack(from, prefix, nodes) {
			return
		}
	}
	r.b.Truncate(from)
	r.b.WriteString(written)
}

// runReadsBack reports whether the inline markdown written since from reads back as nodes: the
// run's own lines, with the prefix its later lines are written behind taken off.
func (r *renderer) runReadsBack(from int, prefix string, nodes []*Node) bool {
	source := string(r.b.Bytes()[from:])
	if prefix != "" {
		source = strings.ReplaceAll(source, "\n"+prefix, "\n")
	}
	parsed, err := ParseInline(source)
	return err == nil && slices.Equal(inlineSignature(parsed), inlineSignature(nodes))
}

func (r *renderer) writeInlineRun(nodes []*Node, prefix string, context inlineContext, escapes delimiterEscapes) {
	escapePipes := context.tableCell
	var active []Mark
	position := inlinePosition{atLineStart: context.startsLine, atTextStart: true}
	for index, n := range nodes {
		if r.err != nil {
			return
		}
		switch n.Type {
		case "text":
			next := writtenMarks(n, escapePipes)
			hasLink := containsMark(visibleMarks(n.Marks), "link")
			var following []Mark
			if index+1 < len(nodes) {
				following = writtenMarks(nodes[index+1], escapePipes)
			}
			common := sharedMarks(active, next)
			for i := len(active) - 1; i >= common; i-- {
				r.closeInlineMark(active[i], escapePipes)
			}
			for _, mark := range next[common:] {
				r.openInlineMark(mark, nodes, index)
			}
			if position.atLineStart && (len(active) != common || len(next) != common) {
				position.afterMarker = true
			}
			active = next
			// The brackets rule follows the marks actually written, not hasLink: a bare URL's
			// link mark is stripped above, and its text must keep its own brackets so linkify
			// reads the whole href. The verdict itself was taken when the link opened.
			label := labelBracketsNone
			if containsMark(next, "link") {
				label = r.labelBrackets
			}
			r.writeInlineText(n, &position, prefix, escapeContext{
				tableCell:  escapePipes,
				urlSchemes: !hasLink,
				label:      label,
				followed:   index+1 < len(nodes) || len(next) > 0,
				delimiters: escapes,
				heading:    context.heading,
				marked:     len(next) > 0,
				opener:     adjacentDelimiter(next[common:]),
				closer:     adjacentDelimiter(next[sharedMarks(next, following):]),
			})
		case "hardbreak":
			r.closeMarks(active, escapePipes)
			active = nil
			// A hard break is a backslash or two spaces before the newline. After text that
			// already ends in a backslash the first form reads as one escaped backslash and a
			// soft break, losing the line break, so the break is written as two spaces there.
			if r.endsWith('\\') {
				r.writeSyntax("  ")
			} else {
				r.writeSyntax("\\")
			}
			r.endLine()
			r.writeSyntax("\n" + prefix)
			position = inlinePosition{atLineStart: true, atTextStart: true, afterLine: true}
		case "image":
			r.closeMarks(active, escapePipes)
			active = nil
			src, _ := n.Attrs["src"].(string)
			alt, _ := n.Attrs["alt"].(string)
			title, _ := n.Attrs["title"].(string)
			r.writeSyntax("![" + escapeTablePipes(strings.ReplaceAll(alt, "]", "\\]"), escapePipes) + "](" + escapeLinkDestination(src, escapePipes) + titleSuffix(title, escapePipes) + ")")
			position.atLineStart, position.atTextStart = false, false
		case "html":
			r.closeMarks(active, escapePipes)
			active = nil
			value, _ := n.Attrs["value"].(string)
			r.writeSyntax(escapeTableHTMLPipes(value, escapePipes))
			position.atLineStart, position.atTextStart = false, false
		case "footnote_reference":
			r.closeMarks(active, escapePipes)
			active = nil
			label, _ := n.Attrs["label"].(string)
			r.writeSyntax("[^" + escapeFootnoteLabel(label) + "]")
			position.atLineStart, position.atTextStart = false, false
		default:
			r.err = fmt.Errorf("%w: cannot render inline %q", ErrSchema, n.Type)
		}
	}
	r.closeMarks(active, escapePipes)
	r.endLine()
}

// endLine judges the line just written when its text began with a lineCandidate: the character is
// escaped when the parser reads the line, markers and hard break included, as something other than
// it reads with the character escaped (lineReadsAsText). A lone `:::` at the prefix of the typed
// block around it closes that block, which the line read on its own cannot show.
func (r *renderer) endLine() {
	candidate := r.heldLineStart
	if candidate == nil {
		return
	}
	r.heldLineStart = nil
	written := r.b.Bytes()
	lineFrom := bytes.LastIndexByte(written[:candidate.at], '\n') + 1
	line := string(written[lineFrom:])
	width := utf8.RuneLen(candidate.char)
	escape := escaped(candidate.char)
	if !candidate.atTypedPrefix || !closesTypedBlock(line, candidate.prefix) {
		readFrom, before := lineFrom, ""
		if candidate.afterLine && lineFrom > 0 {
			readFrom = bytes.LastIndexByte(written[:lineFrom-1], '\n') + 1
			before = string(written[readFrom:lineFrom])
		}
		var footnote string
		if readFrom == r.footnoteLineAt && r.footnoteLabel != "" {
			footnote = r.footnoteLabel
		}
		offset := candidate.at - lineFrom
		if lineReadsAsText(before, line, line[:offset]+escape+line[offset+width:], candidate.prefix, footnote) {
			return
		}
	}
	tail := line[candidate.at-lineFrom+width:]
	r.b.Truncate(candidate.at)
	r.b.WriteString(escape)
	r.b.WriteString(tail)
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
		r.writeSyntax(prefix)
		offset = end
	}
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

func (r *renderer) closeMarks(marks []Mark, escapePipes bool) {
	for i := len(marks) - 1; i >= 0; i-- {
		r.closeInlineMark(marks[i], escapePipes)
	}
}

func (r *renderer) openInlineMark(mark Mark, nodes []*Node, index int) {
	if mark.Type == "link" {
		r.labelBrackets = linkLabelBrackets(linkLabel(nodes, index, mark))
	}
	if mark.Type != "inlineCode" {
		r.writeSyntax(openMark(mark))
		return
	}
	r.inlineCodeFence, r.inlineCodePadded = inlineCodeFence(nodes, index)
	r.writeSyntax(r.inlineCodeFence)
	if r.inlineCodePadded {
		r.writeSyntax(" ")
	}
}

func (r *renderer) closeInlineMark(mark Mark, escapePipes bool) {
	if mark.Type != "inlineCode" {
		r.writeSyntax(closeMark(mark, escapePipes))
		return
	}
	if r.inlineCodePadded {
		r.writeSyntax(" ")
	}
	r.writeSyntax(r.inlineCodeFence)
	r.inlineCodeFence = ""
	r.inlineCodePadded = false
}

func inlineCodeFence(nodes []*Node, index int) (string, bool) {
	var content strings.Builder
	for _, node := range nodes[index:] {
		if node.Type != "text" || !nodeHasMark(node, "inlineCode") {
			break
		}
		content.WriteString(node.Text)
	}
	value := content.String()
	longest, run := 0, 0
	for _, char := range value {
		if char == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", longest+1), inlineCodePadding(value)
}

func inlineCodePadding(value string) bool {
	if strings.HasPrefix(value, "`") || strings.HasSuffix(value, "`") {
		return true
	}
	return strings.HasPrefix(value, " ") && strings.HasSuffix(value, " ") && strings.Trim(value, " ") != ""
}

func (r *renderer) writeSyntax(value string) {
	r.b.WriteString(value)
}

// endsWith reports whether the markdown written so far ends with char.
func (r *renderer) endsWith(char byte) bool {
	rendered := r.b.Bytes()
	return len(rendered) > 0 && rendered[len(rendered)-1] == char
}

func (r *renderer) writeText(value string) {
	r.b.WriteString(value)
}

func (r *renderer) writeInlineText(node *Node, position *inlinePosition, prefix string, context escapeContext) {
	if nodeHasMark(node, "inlineCode") {
		// A code span's text is written as it is; a newline in it still ends a line.
		value := escapeTablePipes(node.Text, context.tableCell)
		for {
			newline := strings.IndexByte(value, '\n')
			if newline < 0 {
				break
			}
			r.writeText(value[:newline])
			r.endLine()
			r.writeText("\n")
			value = value[newline+1:]
		}
		r.writeText(value)
		position.atLineStart = strings.HasSuffix(node.Text, "\n")
		position.atTextStart = position.atLineStart
		position.afterLine = position.afterLine || strings.Contains(node.Text, "\n")
		return
	}

	value := node.Text
	segmentStart := 0
	// Where the current line's text begins inside this node, or -1 when it began in an earlier
	// one: lineStart for a line of its own whose first character is yet to be judged, and
	// textLineStart for the writer's long-standing escapes.
	lineStart, textLineStart := -1, -1
	if position.atLineStart {
		lineStart = 0
	}
	if position.atTextStart {
		textLineStart = 0
	}
	for byteOffset, char := range value {
		width := utf8.RuneLen(char)
		context.textLineStart = textLineStart
		context.afterMarker = position.afterMarker
		escape := needsInlineEscape(value, byteOffset, char, context)
		if lineStart >= 0 && r.heldLineStart == nil {
			switch lineStartOf(value, lineStart, byteOffset, char, escape) {
			case lineStartEscaped:
				escape = true
				lineStart = -1
			case lineStartHeld:
				r.holdLineStart(value[segmentStart:byteOffset], char, position, prefix)
				segmentStart = byteOffset
				lineStart = -1
			case lineStartAsIs:
				lineStart = -1
			}
		}
		if escape {
			r.writeText(value[segmentStart:byteOffset])
			r.writeSyntax(textEscape(char))
			segmentStart = byteOffset + width
		}
		if char == '\n' {
			r.writeText(value[segmentStart:byteOffset])
			segmentStart = byteOffset
			r.endLine()
			lineStart, textLineStart = byteOffset+width, byteOffset+width
			position.afterLine = true
		}
		position.atLineStart = char == '\n'
		position.atTextStart = char == '\n'
		position.afterMarker = false
	}
	r.writeText(value[segmentStart:])
}

// holdLineStart writes the text before a line's first character and holds that character for
// endLine to judge once the line is written.
func (r *renderer) holdLineStart(before string, char rune, position *inlinePosition, prefix string) {
	r.writeText(before)
	r.heldLineStart = &lineCandidate{
		at:            r.b.Len(),
		char:          char,
		afterLine:     position.afterLine,
		atTypedPrefix: r.typedPrefix != nil && *r.typedPrefix == prefix,
		prefix:        prefix,
	}
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
