package pmdoc

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

type renderer struct {
	b                bytes.Buffer
	inlineCodeFence  string
	inlineCodePadded bool
	// labelBrackets is decided when a link opens, over the whole label it opens, and read by every
	// text node of that label. One node cannot judge it: what pairs with a bracket may sit in a
	// sibling node of the same link.
	labelBrackets labelBrackets
	// heldLineStart is the current line's first text character, held until the line is written.
	heldLineStart *lineCandidate
	// footnoteLineAt is where the last footnote definition's first line begins in the markdown,
	// and footnoteLabel is its label as written, so that the line is read after a reference to it.
	footnoteLineAt int
	footnoteLabel  string
	// scopes is a frame for each container the blocks being written stand in, outermost first
	// (scope).
	scopes []scope
	// asteriskRule makes the next rule written `***` rather than `---` (list).
	asteriskRule bool
	// otherListMarker makes the next list written with its kind's other marker (otherListMarkers).
	otherListMarker bool
	// footnoteLabels is every footnote label the document defines (footnoteLabelSet).
	footnoteLabels footnoteLabelSet
	// bareEmptyCode writes an empty code block in a typed block in a list item as its fences alone,
	// and wroteBlankEmptyCode records one written with a line between them (emptyCodeWrittenBare).
	bareEmptyCode, wroteBlankEmptyCode bool
	err                                error
	blockOffsets                       []BlockOffset
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

func render(doc *Node) (r *renderer, err error) {
	// The renderer reads back what it writes (blockKinds, parseInlineWithDefinitions).
	defer recoverPanic(&r, &err, "rendering a document")
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
	// the runs, so a mark never changes the markdown (`snake_case`, never `snake\_case`). The line
	// breaks the browser editor holds that markdown has no form for are written as it draws them.
	doc = asWritten(doc)
	if holdsOnlyAnEmptyParagraph(doc) {
		return &renderer{}, nil
	}
	labels := definedFootnoteLabels(doc)
	r = renderBlocks(doc, labels, false)
	// An empty code block in a typed block in a spread list item is written with a line between its
	// fences, as main wrote it, except in a document holding a shape this parser reads only as the
	// browser editor does, where that line is refused (emptyCodeWrittenBare): there the document is
	// written again with the fences alone.
	if r.err == nil && r.wroteBlankEmptyCode && holdsBrowserOnlyShape(r.b.Bytes()) {
		r = renderBlocks(doc, labels, true)
	}
	if r.err != nil {
		return nil, r.err
	}
	return r, nil
}

// renderBlocks writes doc's blocks, an empty code block in a typed block in a spread list item as
// its fences alone when bareEmptyCode says so.
func renderBlocks(doc *Node, labels footnoteLabelSet, bareEmptyCode bool) *renderer {
	r := &renderer{footnoteLabels: labels, bareEmptyCode: bareEmptyCode}
	r.blocks(doc.Children, "")
	if r.err != nil {
		return r
	}
	// A rule opening the document is written `---`, as it always was, except where that is
	// misread; there it is `***`, the same length. A `---` there opens front matter: a later line
	// that is `---` - a second rule, a code line - closes it and everything up to there reads as
	// front matter, and with no such line the browser editor's parser, having tried the front
	// matter to the document's end, reads no list, quote or footnote definition in the rest.
	if doc.Children[0].Type == "hr" {
		if front, _, _ := parseFrontmatterBlock(r.b.Bytes()); front != nil || holdsAContainerTheBrowserDrops(doc) {
			copy(r.b.Bytes(), "***")
		}
	}
	return r
}

// holdsBrowserOnlyShape reports whether markdown, read as Parse reads it, holds a shape this
// parser reads only as the browser editor's parser does (browserOnlyShape).
func holdsBrowserOnlyShape(markdown []byte) bool {
	_, rest, unclosed := parseFrontmatterBlock(markdown)
	return browserOnlyShape(blockReader.parse(markdown[rest:], unclosed)) != ""
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

// footnoteLabelSet is every label a document's footnote definitions carry, wherever they stand,
// kept two ways: as the browser editor's parser compares labels (footnoteLabelKey), since it
// matches a reference to its definition whatever the case, and lowercased, as this renderer
// compared them before it did, since text escaped then must be escaped still.
type footnoteLabelSet struct{ keys, lowered map[string]bool }

// refersTo reports whether text shaped like a reference with label is escaped: it would read as a
// reference to a defined label, or this renderer escaped it when it lowercased labels. An escape
// the parser does not need reads back as the same text.
func (labels footnoteLabelSet) refersTo(label string) bool {
	return labels.keys[footnoteLabelKey(label)] || labels.lowered[strings.ToLower(label)]
}

func definedFootnoteLabels(doc *Node) footnoteLabelSet {
	labels := footnoteLabelSet{keys: make(map[string]bool), lowered: make(map[string]bool)}
	var walk func(*Node)
	walk = func(node *Node) {
		if node.Type == "footnote_definition" {
			if label, ok := node.Attrs["label"].(string); ok {
				labels.keys[footnoteLabelKey(label)] = true
				labels.lowered[strings.ToLower(label)] = true
			}
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(doc)
	return labels
}

func (r *renderer) blocks(nodes []*Node, prefix string) {
	otherMarkers := otherListMarkers(nodes)
	for i, n := range nodes {
		if i > 0 {
			r.writeSyntax("\n" + strings.TrimRight(prefix, " ") + "\n")
		}
		r.otherListMarker = otherMarkers[i]
		r.block(n, prefix)
	}
	if len(nodes) > 0 {
		r.writeSyntax("\n")
	}
}

func (r *renderer) blocksNoTrailing(nodes []*Node, prefix string) {
	otherMarkers := otherListMarkers(nodes)
	for i, n := range nodes {
		if i > 0 {
			blanks := 1
			if previous := nodes[i-1]; isList(previous) {
				blanks, _ = r.blanksAfterList(previous, n)
			}
			r.writeSyntax("\n" + strings.Repeat(strings.TrimRight(prefix, " ")+"\n", blanks) + prefix)
		}
		r.otherListMarker = otherMarkers[i]
		r.block(n, prefix)
	}
}

// otherListMarkers reports, for each of a container's blocks, whether it is a list written with
// its kind's other marker (`*` for `-`, `)` for `.`): a list of the same kind as the block before
// it, past any empty paragraph, which is written as nothing, takes the marker that list did not,
// so the two read back as two lists, as the browser editor writes them. Written with one marker,
// two lists side by side read back as one.
func otherListMarkers(blocks []*Node) []bool {
	other := make([]bool, len(blocks))
	previous := -1
	for index, block := range blocks {
		if block.Type == "paragraph" && len(block.Children) == 0 {
			continue
		}
		if isList(block) && previous >= 0 && blocks[previous].Type == block.Type {
			other[index] = !other[previous]
		}
		previous = index
	}
	return other
}

// listItemMarker is the marker a list item is written with: `- `, or `N. ` in an ordered list, or,
// in a list written with its kind's other marker (otherListMarkers), `* ` or `N) `.
func listItemMarker(ordered bool, number int, other bool) string {
	switch {
	case ordered && other:
		return strconv.Itoa(number) + ") "
	case ordered:
		return strconv.Itoa(number) + ". "
	case other:
		return "* "
	default:
		return "- "
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
		r.heading(n, prefix)
	case "blockquote":
		r.writeSyntax("> ")
		r.enter(n, prefix+"> ")
		r.blocksNoTrailing(n.Children, prefix+"> ")
		r.blanksEndingQuotedList(n.Children, prefix+"> ", false)
		r.leave()
	case "bullet_list", "ordered_list":
		r.list(n, prefix)
	case "code_block":
		language, _ := n.Attrs["language"].(string)
		fence := codeBlockFence(n)
		if emptyCode(n) && r.emptyCodeWrittenBare() {
			r.writeSyntax(fence + language + "\n" + prefix + fence)
			return
		}
		r.writeSyntax(fence + language + "\n")
		var text strings.Builder
		for _, child := range n.Children {
			text.WriteString(child.Text)
		}
		r.writeCodeLinePrefix(text.String(), prefix)
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
		if r.definitionOpensOnALaterLine() {
			r.writeSyntax("[^" + escapeFootnoteLabel(label) + "]:\n" + prefix + definitionIndent)
		} else {
			r.writeSyntax("[^" + escapeFootnoteLabel(label) + "]: ")
		}
		// The browser editor's parser reads the lines of a definition a list item holds as the
		// item's, so in a tight item a blank line between two of its blocks would spread the item.
		around := r.scope().node
		tightItem := around != nil && around.Type == "list_item" && around.Attrs["spread"] != true
		r.enter(n, prefix+definitionIndent)
		if tightItem {
			r.itemBlocks(n.Children, false, prefix, prefix+definitionIndent, false)
		} else {
			r.blocksNoTrailing(n.Children, prefix+definitionIndent)
		}
		r.leave()
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
		colons := typedFence(n, len(prefix))
		fence := strings.Repeat(":", colons)
		if holdsOnlyAnEmptyParagraph(n) && r.inItemBelowQuotes() {
			// Its empty paragraph written as a line would be a blank line in a typed block in a
			// list item, which the browser editor reads as spacing the item; written as nothing,
			// both parsers read the typed block back holding it.
			r.writeSyntax(fence + n.Type + "{" + attrs + "}\n" + prefix + fence)
			return
		}
		r.writeSyntax(fence + n.Type + "{" + attrs + "}\n" + prefix)
		r.enterTyped(n, prefix, colons)
		r.blocksNoTrailing(n.Children, prefix)
		r.blanksEndingQuotedList(n.Children, prefix, true)
		r.leave()
		r.writeSyntax("\n" + prefix + fence)
	}
}

// loosenessReadsSpread reports whether goldmark's looseness reads list's and its items' spread as
// list holds them (parseList): an item is spread exactly where it writes more than one block, an
// empty paragraph beside other blocks writing nothing, and the list only where no item is.
func loosenessReadsSpread(list *Node) bool {
	anySpread := false
	for _, item := range list.Children {
		written := 0
		for _, child := range item.Children {
			if child.Type != "paragraph" || len(child.Children) > 0 {
				written++
			}
		}
		spread := item.Attrs["spread"] == true
		if spread != (written > 1) {
			return false
		}
		anySpread = anySpread || spread
	}
	return !anySpread || list.Attrs["spread"] != true
}

// holdsOnlyAnEmptyParagraph reports whether container n holds nothing but one empty paragraph,
// which both parsers read an empty container as holding (emptyParagraphFirst).
func holdsOnlyAnEmptyParagraph(n *Node) bool {
	return len(n.Children) == 1 && n.Children[0].Type == "paragraph" && len(n.Children[0].Children) == 0
}

// typedFence is the number of colons a typed block whose lines start at column is written with:
// three, or one more than the longest line of colons inside it that could close it
// (closingColons). A renderer prefix is spaces and `> `, so its length is that column.
func typedFence(n *Node, column int) int {
	return max(3, closingColons(n, column)+1)
}

// heading writes heading n. An ATX heading is one line, so a heading holding a line break is
// written setext, its lines as a paragraph's and an underline after them, as the browser editor
// writes it. Only levels one and two have that form; past them the break is written as a space, as
// the browser editor also writes it.
func (r *renderer) heading(n *Node, prefix string) {
	level := int(num(n.Attrs["level"], 1))
	if !holdsHardBreak(n.Children) {
		r.writeSyntax(strings.Repeat("#", level) + " ")
		r.headingInline(n.Children, prefix)
		return
	}
	switch level {
	case 1, 2:
		underline := "---"
		if level == 1 {
			underline = "==="
		}
		r.inline(n.Children, prefix)
		r.writeSyntax("\n" + prefix + underline)
	default:
		r.writeSyntax(strings.Repeat("#", level) + " ")
		r.headingInline(hardBreaksAsSpaces(n.Children), prefix)
	}
}

func (r *renderer) list(n *Node, prefix string) {
	other := r.otherListMarker
	start := 1
	if n.Type == "ordered_list" {
		start = int(num(n.Attrs["order"], 1))
	}
	readsLoose := loosenessReadsSpread(n)
	for index, item := range n.Children {
		if item.Type != "list_item" {
			r.err = fmt.Errorf("%w: list contains %q", ErrSchema, item.Type)
			return
		}
		if index > 0 {
			r.writeSyntax("\n" + prefix)
			blanks := 0
			if previous := n.Children[index-1]; r.definitionEndsItem(previous) {
				blanks = r.blanksAfterDefinitionItem(previous, item)
			} else if r.writesBlankAfterItem(n, previous) {
				blanks = 1
			}
			for range blanks {
				r.writeSyntax("\n" + strings.TrimRight(prefix, " ") + "\n" + prefix)
			}
		}
		marker := listItemMarker(n.Type == "ordered_list", start+index, other)
		r.writeSyntax(marker)
		// A task item holding only an empty paragraph is written as an empty item, as the browser
		// editor writes it: no form of the marker alone reads back as a task, and `- [ ]` reads back
		// as an item holding the text `[ ]`.
		emptyTask := holdsOnlyAnEmptyParagraph(item)
		if checked, ok := item.Attrs["checked"].(bool); ok && !emptyTask {
			if checked {
				r.writeSyntax("[x] ")
			} else {
				r.writeSyntax("[ ] ")
			}
		}
		indent := prefix + strings.Repeat(" ", len(marker))
		// An empty first paragraph is written as nothing, with the next block on the marker's line:
		// a blank line after an empty marker line would end the item, and both parsers read an item
		// that opens with another block as holding an empty paragraph first (`- # h`). A rule there
		// is written with the other character from the marker's, since `- ---` and `* ***` are
		// thematic breaks at the list's level: `***`, and `---` after `* `. A task item cannot
		// be written so: its marker's line would carry the next block as the task's text, and the
		// browser editor reads no other form of it as a task.
		children := item.Children
		skipped := opensWithUnwrittenParagraph(item)
		if skipped {
			if _, task := item.Attrs["checked"].(bool); task {
				r.err = fmt.Errorf("%w: a task item whose first paragraph is empty cannot hold another block after it; the browser editor reads no such item as a task", ErrSchema)
				return
			}
			children = children[1:]
		}
		r.enterItem(item, indent, readsLoose)
		r.itemBlocks(children, item.Attrs["spread"] == true, prefix, indent, skipped && marker != "* ")
		r.leave()
	}
}

// spreadElsewhere reports whether a blank line a spread item writes between two of blocks, its
// own, spreads it wherever it stands: one after a block that is neither a list nor a footnote
// definition, whose blank lines after them can be theirs.
func spreadElsewhere(blocks []*Node) bool {
	for index := 1; index < len(blocks); index++ {
		if previous := blocks[index-1]; previous.Type != "footnote_definition" && !isList(previous) {
			return true
		}
	}
	return false
}

// opensWithUnwrittenParagraph reports whether item's first block is an empty paragraph with another
// block after it, which the renderer writes as nothing, the next block on the marker's line.
func opensWithUnwrittenParagraph(item *Node) bool {
	children := item.Children
	return len(children) > 1 && children[0].Type == "paragraph" && len(children[0].Children) == 0
}

// itemBlocks writes blocks the browser editor's parser reads as a list item's lines - the item's
// own, or those of a footnote definition the item holds - at indent, their lines' prefix, where
// prefix is the list's. A tight item writes its blocks on consecutive lines, where a paragraph
// would run on into a paragraph after it and underline itself with a rule's `---`, and a block
// whose first line would be a row of a table before it (continuesTable) would be one. The browser
// editor's writer puts a blank line between two paragraphs (the item then reads back spread) and
// writes a rule `***`, and so does this renderer, which writes a blank line after such a table too;
// firstRule reports whether a rule opening the blocks is written `***` too.
func (r *renderer) itemBlocks(blocks []*Node, spread bool, prefix, indent string, firstRule bool) {
	otherMarkers := otherListMarkers(blocks)
	for index, child := range blocks {
		afterParagraph := index > 0 && blocks[index-1].Type == "paragraph"
		if index > 0 {
			blanks := 0
			if spread || afterParagraph && child.Type == "paragraph" || blocks[index-1].Type == "table" && continuesTable(child) {
				blanks = 1
			}
			if previous := blocks[index-1]; isList(previous) {
				after, exact := r.blanksAfterList(previous, child)
				switch {
				case exact:
					blanks = after
				// One blank line keeps a block that cannot open on the line after a paragraph off
				// the paragraph the list ends in.
				case !isList(child) && !opensAfterParagraph(child) && endsInParagraph(previous):
					blanks = 1
				}
			} else if previous.Type == "footnote_definition" && r.scope().quotes > 0 && !quoteListOrDefinition(child) {
				// In a quote the blank lines after a footnote definition are the definition's, an
				// empty definition's own line being one of them (definitionSpreadBlanks). A spread
				// item writes what spreads it where no blank line between two of its other blocks
				// does, and a tight one writes one only to keep a block that cannot open on the
				// line after a paragraph off the definition's, and none after a definition ending
				// in a list no line continues (endsInClosedList) or an empty one.
				own := 0
				if holdsOnlyAnEmptyParagraph(previous) {
					own = 1
				}
				switch {
				case spread && !spreadElsewhere(blocks):
					blanks = definitionSpreadBlanks(child) - own
				case spread || own == 0 && !opensAfterParagraph(child) && !endsInClosedList(previous):
					blanks = 1
				}
			}
			r.writeSyntax("\n" + strings.Repeat(strings.TrimRight(prefix, " ")+"\n", blanks) + indent)
			// A quote on the line after a paragraph opens there, and a list opening it whose first
			// item cannot interrupt a paragraph would not: a quote line first leaves the list its
			// own line.
			if blanks == 0 && afterParagraph && opensWithListThatCannotInterrupt(child) {
				r.writeSyntax(">\n" + indent)
			}
		}
		r.asteriskRule = child.Type == "hr" && (afterParagraph && !spread || firstRule && index == 0)
		r.otherListMarker = otherMarkers[index]
		r.block(child, indent)
	}
}

func (r *renderer) table(table *Node, prefix string) {
	if len(table.Children) == 0 || table.Children[0].Type != "table_header_row" {
		r.err = fmt.Errorf("%w: table requires a header row", ErrSchema)
		return
	}
	grid := tableGrid(table)
	r.tableRow(table.Children[0], grid[0], true, prefix)
	r.writeSyntax("\n" + prefix + "| ")
	for i, cell := range grid[0] {
		if i > 0 {
			r.writeSyntax(" | ")
		}
		r.writeSyntax(tableAlignment(cell.Attrs["alignment"]))
	}
	r.writeSyntax(" |")
	for index, row := range table.Children[1:] {
		// A row with no cells is written as nothing: a table with only such rows reads back
		// holding one, as the browser editor's parser reads a table with no body row.
		if len(row.Children) == 0 {
			continue
		}
		r.writeSyntax("\n" + prefix)
		r.tableRow(row, grid[index+1], false, prefix)
	}
}

// tableRow writes row as cells, its cells in the columns tableGrid places them in. A cell is one
// line, so a hard break in it is written as a space, as the browser editor writes it: written as a
// break, it would end the row, and `<br>` reads back as inline HTML the editor shows as that text.
func (r *renderer) tableRow(row *Node, cells []*Node, header bool, prefix string) {
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
	for i, cell := range cells {
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
		r.tableCellInline(hardBreaksAsSpaces(cell.Children[0].Children), prefix)
	}
	r.writeSyntax(" |")
}

// tableGrid is each of table's rows as its markdown writes them, cell by cell. GFM carries no span,
// so a cell spanning columns or rows (colspan, rowspan: a pasted HTML table can hold them) is
// written in the first position it covers and each other one it covers as an empty cell of its
// alignment, as the browser editor writes a spanning cell. The header row, whose width is the
// table's to Go's parser, is as wide as the widest row, its added cells taking the alignment of the
// first cell under them, so that no cell past it is lost. A body row keeps its own width, since
// Parse pads a short row. A table with no span and no row wider than its header is its own rows.
// A span comes from the live tree unchecked, so the empty cells spans add are bounded per table
// (maxSpanCells): each column a colspan adds, each position a rowspan covers below and each gap
// filled up to one is charged, and a span past the budget adds no more cells. A span also covers
// no column past the wider of maxColspan and the table's widest row as it holds cells. Every cell
// the table holds is still written with its text.
func tableGrid(table *Node) [][]*Node {
	covered := map[[2]int]*Node{}
	// lastCovered is, for each row, the last column a span from a row above covers, or -1.
	lastCovered := make([]int, len(table.Children))
	limit := maxColspan
	for index, row := range table.Children {
		lastCovered[index] = -1
		limit = max(limit, len(row.Children))
	}
	budget := maxSpanCells
	grid := make([][]*Node, len(table.Children))
	width := 0
	for rowIndex, row := range table.Children {
		kind := "table_cell"
		if row.Type == "table_header_row" {
			kind = "table_header"
		}
		var cells []*Node
		// fill adds the empty cells a span from a row above covers at the next position, and past
		// it every position up to until, while the budget lasts.
		fill := func(until int) {
			for {
				spanning, spanned := covered[[2]int{rowIndex, len(cells)}]
				if !spanned && (len(cells) >= until || budget == 0) {
					return
				}
				if !spanned {
					budget--
				}
				cells = append(cells, emptyTableCell(kind, spanning))
			}
		}
		for _, cell := range row.Children {
			fill(0)
			column := len(cells)
			columns := max(1, min(tableSpan(cell.Attrs["colspan"]), maxColspan, limit-column))
			rows := min(tableSpan(cell.Attrs["rowspan"]), len(table.Children)-rowIndex)
			cells = append(cells, cell)
			tail := min(columns-1, budget)
			budget -= tail
			for range tail {
				cells = append(cells, emptyTableCell(kind, cell))
			}
			for below := 1; below < rows && budget > 0; below++ {
				covers := min(1+tail, budget)
				budget -= covers
				for offset := range covers {
					covered[[2]int{rowIndex + below, column + offset}] = cell
				}
				lastCovered[rowIndex+below] = max(lastCovered[rowIndex+below], column+covers-1)
			}
		}
		fill(lastCovered[rowIndex] + 1)
		grid[rowIndex] = cells
		width = max(width, len(cells))
	}
	for column := len(grid[0]); column < width; column++ {
		var under *Node
		for _, row := range grid[1:] {
			if column < len(row) {
				under = row[column]
				break
			}
		}
		grid[0] = append(grid[0], emptyTableCell("table_header", under))
	}
	return grid
}

// tableSpan is how many columns or rows a cell's colspan or rowspan covers: at least one.
func tableSpan(value any) int {
	return max(1, int(num(value, 1)))
}

// maxColspan is the most columns a cell is written across, as HTML caps colspan, so the markdown
// holds no more of a span than the browser editor draws.
const maxColspan = 1000

// maxSpanCells is how many empty cells the spans of one table add in all (tableGrid): a render
// reached from a peer's update writes at most that many cells no one wrote.
const maxSpanCells = 100_000

// emptyTableCell is an empty cell of kind with like's alignment, or none when like is nil.
func emptyTableCell(kind string, like *Node) *Node {
	var alignment any
	if like != nil {
		alignment = like.Attrs["alignment"]
	}
	return &Node{
		Type:     kind,
		Attrs:    Attrs{"alignment": alignment, "colspan": 1, "colwidth": nil, "rowspan": 1},
		Children: []*Node{{Type: "paragraph"}},
	}
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
		r.writeCodeLinePrefix(value[end:], prefix)
		offset = end
	}
}

// writeCodeLinePrefix writes prefix ahead of a code line, the first of rest. A blank line does not
// take a footnote definition's indentation, and both parsers keep what it holds as the code's, while
// every other container around the code takes its own columns from it, a list item no more than its
// width. So a blank code line there, where no quote stands inside the definition to carry its
// marker after that indentation, is written without the definition's indentation, and one holding
// nothing with no indentation after the prefix's last quote marker (`> `) - the code's first line
// as well as a later one.
func (r *renderer) writeCodeLinePrefix(rest, prefix string) {
	if frame := r.scope(); frame.footnote != nil && frame.quotes == frame.footnote.quotes && blankLineAhead(rest) {
		switch marker := strings.LastIndex(prefix, "> "); {
		case rest != "" && rest[0] != '\n':
			prefix = prefix[:frame.footnote.indentAt] + prefix[frame.footnote.indentAt+len(definitionIndent):]
		case marker >= 0:
			prefix = prefix[:marker+len("> ")]
		default:
			prefix = ""
		}
	}
	r.writeSyntax(prefix)
}

// blankLineAhead reports whether text's first line, its last included, holds only spaces and tabs.
func blankLineAhead(text string) bool {
	end := strings.IndexByte(text, '\n')
	if end < 0 {
		end = len(text)
	}
	return strings.Trim(text[:end], " \t") == ""
}

// emptyCode reports whether code block node holds no text.
func emptyCode(node *Node) bool {
	for _, child := range node.Children {
		if child.Text != "" {
			return false
		}
	}
	return true
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

// isBareAutolink reports whether a link whose text is its href can be written as that bare text for
// linkify to link again. A backtick in it would be written escaped, which linkify stops at, so such
// a link is written as an autolink (isAngleURLLink).
func isBareAutolink(value string) bool {
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return false
	}
	if strings.ContainsAny(value, " \t\n()<>`") {
		return false
	}
	return !strings.ContainsAny(value[len(value)-1:], ".,!?;:")
}

// isAngleURLLink reports whether node is a link whose text is its href, holding a backtick, which
// is written `<href>`, as the browser editor writes it: both parsers read an autolink's text as
// written, escapes and references included, where bare text would be escaped and linkify would stop
// at the escape. One an autolink cannot hold (whitespace, `<`, `>`, a pipe in a table cell) is
// written as an explicit link.
func isAngleURLLink(node *Node, marks []Mark, escapePipes bool) bool {
	for _, mark := range marks {
		if mark.Type != "link" {
			continue
		}
		href, _ := mark.Attrs["href"].(string)
		title, _ := mark.Attrs["title"].(string)
		value := node.Text
		return href == value && title == "" && strings.Contains(value, "`") &&
			(strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")) &&
			!strings.ContainsAny(value, " \t\n\r<>") && !(escapePipes && strings.Contains(value, "|"))
	}
	return false
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

// escapeFootnoteLabel writes label so that the browser editor's parser, which decodes a label's
// escapes and character references, reads it back: a bracket, a pipe, and a backslash before ASCII
// punctuation or at the label's end are escaped, an ampersand only where it would open a character
// reference, and white space, which no label holds as written, is a numeric character reference.
// A label that reads back as written stays as it is.
func escapeFootnoteLabel(label string) string {
	var written strings.Builder
	written.Grow(len(label))
	for index, char := range label {
		switch {
		case char == '[' || char == ']' || char == '|':
			written.WriteByte('\\')
		case char == '\\' && (index+1 == len(label) || isASCIIPunctuation(label[index+1])):
			written.WriteByte('\\')
		case char == '&' && characterReference.MatchString(label[index:]):
			written.WriteByte('\\')
		case unicode.IsSpace(char):
			written.WriteString("&#" + strconv.Itoa(int(char)) + ";")
			continue
		}
		written.WriteRune(char)
	}
	return written.String()
}

// characterReference is a character reference opening text: a named one, or a decimal or
// hexadecimal numeric one.
var characterReference = regexp.MustCompile(`^&(?:[A-Za-z][A-Za-z0-9]*|#[0-9]{1,7}|#[xX][0-9A-Fa-f]{1,6});`)

func holdsHardBreak(nodes []*Node) bool {
	for _, n := range nodes {
		if n.Type == "hardbreak" {
			return true
		}
	}
	return false
}

// hardBreaksAsSpaces is nodes with each hard break a space carrying no marks.
func hardBreaksAsSpaces(nodes []*Node) []*Node {
	out := make([]*Node, len(nodes))
	for index, n := range nodes {
		if n.Type == "hardbreak" {
			n = &Node{Type: "text", Text: " "}
		}
		out[index] = n
	}
	return out
}

// asWritten is doc as its markdown writes it: without the marks the rendering does not write
// (StripAnchorMarks), and with each line break its textblocks hold in the one form markdown
// carries. The browser editor holds two more. A soft line break it keeps from a paste (a hard break
// with isInline) it draws as a space, and one is written: both parsers read a soft break as one too.
// A line feed in text outside code it draws as a line break (white-space: break-spaces), and a
// hard break is written, which both parsers read back as the editor draws it, except where nothing
// but whitespace follows it in its textblock: a hard break there reads back as a backslash, and a
// line feed as nothing, as trailing whitespace does.
func asWritten(doc *Node) *Node {
	out := StripAnchorMarks(doc)
	writeLineBreaksIn(out)
	return out
}

// writeLineBreaksIn gives every textblock under node its line breaks as asWritten writes them.
func writeLineBreaksIn(node *Node) {
	switch node.Type {
	case "paragraph", "heading":
		node.Children = writtenLineBreaks(node.Children)
	case "code_block":
	default:
		for _, child := range node.Children {
			writeLineBreaksIn(child)
		}
	}
}

// writtenLineBreaks is a textblock's inline nodes with each soft break a space and each line feed
// in text outside code followed by more than whitespace a hard break (asWritten), adjacent text
// with the same marks merged, as the parser reads it.
func writtenLineBreaks(nodes []*Node) []*Node {
	out := make([]*Node, 0, len(nodes))
	appendText := func(text string, marks []Mark) {
		if text == "" {
			return
		}
		if last := len(out) - 1; last >= 0 && out[last].Type == "text" && marksEqual(out[last].Marks, marks) {
			out[last] = &Node{Type: "text", Text: out[last].Text + text, Marks: marks}
			return
		}
		out = append(out, &Node{Type: "text", Text: text, Marks: marks})
	}
	for index, node := range nodes {
		switch {
		case node.Type == "hardbreak" && node.Attrs["isInline"] == true:
			appendText(" ", nil)
		case node.Type == "text" && !nodeHasMark(node, "inlineCode") && strings.Contains(node.Text, "\n"):
			lines := strings.Split(node.Text, "\n")
			for line, text := range lines {
				if line > 0 {
					if onlyWhitespaceFollows(strings.Join(lines[line:], "\n"), nodes[index+1:]) {
						appendText("\n", node.Marks)
					} else {
						out = append(out, &Node{Type: "hardbreak", Attrs: Attrs{"isInline": false}})
					}
				}
				appendText(text, node.Marks)
			}
		case node.Type == "text":
			appendText(node.Text, node.Marks)
		default:
			out = append(out, node)
		}
	}
	return out
}

// onlyWhitespaceFollows reports whether text, and after it every one of rest, holds nothing but
// whitespace.
func onlyWhitespaceFollows(text string, rest []*Node) bool {
	if strings.TrimSpace(text) != "" {
		return false
	}
	for _, node := range rest {
		if node.Type != "text" || strings.TrimSpace(node.Text) != "" {
			return false
		}
	}
	return true
}

// tableAlignment is the delimiter row's cell for a column's alignment. A column with none - null,
// or the "none" this parser once stored - is written `---`, which both parsers read as no
// alignment; `:---` reads as left.
func tableAlignment(value any) string {
	alignment, _ := value.(string)
	switch alignment {
	case "left":
		return ":---"
	case "center":
		return ":---:"
	case "right":
		return "---:"
	default:
		return "---"
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
