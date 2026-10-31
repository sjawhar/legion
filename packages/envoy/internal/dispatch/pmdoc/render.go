package pmdoc

import (
	"bytes"
	"fmt"
	"slices"
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
	// openers is each text `<` the inline run being written wrote as it is (angleOpener), and
	// escapedOpeners those of them its writing again escapes (writeInlineRun).
	openers, escapedOpeners []angleOpener
	// footnoteLineAt is where the last footnote definition's first line begins in the markdown,
	// and footnoteLabel is its label as written, so that the line is read after a reference to it.
	footnoteLineAt int
	footnoteLabel  string
	// checkboxEnd is where the last task item's checkbox (`[ ] `, `[x] `) ends in the markdown, or 0
	// before any, so that the text written after it on its line is not read as the line's first
	// (holdLineStart).
	checkboxEnd int
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
	// spanBudget is how many more empty cells the tables' spans may add: maxSpanCells, or none in
	// a spanless render (renderSpanless).
	spanBudget   int
	err          error
	blockOffsets []BlockOffset
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
	r, err := render(doc, maxSpanCells)
	if err != nil {
		return "", err
	}
	return r.b.String(), nil
}

// renderSpanless is Render writing no cell for a table's spans: each cell once, and the header as
// wide as the widest row as it holds cells (tableGrid). The read-back of one document-level block
// at a time (ReadBackBlock) reads it so. It runs once for each block a write changed, where a
// budget for each render would let a write across many tables add maxSpanCells cells for every one
// of them; a render of the whole document, the one a write stores or reads back (NewMisread),
// spends one budget in all.
func renderSpanless(doc *Node) (string, error) {
	r, err := render(doc, 0)
	if err != nil {
		return "", err
	}
	return r.b.String(), nil
}

// SpanBudget is one budget of the empty cells tables' spans add (maxSpanCells) that several renders
// share, where together they write what one render of a document writes: each spends what its
// tables' spans add, and the next has what is left.
type SpanBudget struct{ cells int }

// NewSpanBudget is a whole budget.
func NewSpanBudget() *SpanBudget {
	return &SpanBudget{cells: maxSpanCells}
}

// Render is Render spending b.
func (b *SpanBudget) Render(doc *Node) (string, error) {
	r, err := render(doc, b.cells)
	if err != nil {
		return "", err
	}
	b.cells = r.spanBudget
	return r.b.String(), nil
}

// RenderWithBlockOffsets renders a document and records each identified block's
// byte range in the returned markdown. It validates doc first.
func RenderWithBlockOffsets(doc *Node) (string, []BlockOffset, error) {
	return renderWithBlockOffsets(doc, true)
}

// RenderWithBlockOffsets is RenderWithBlockOffsets of d's tree without validating it again: the
// renderer's own refusals still run, and a panic in it is still an ErrPanic. The zero
// ValidDocument is RenderWithBlockOffsets' error for a nil tree.
func (d ValidDocument) RenderWithBlockOffsets() (string, []BlockOffset, error) {
	return renderWithBlockOffsets(d.doc, false)
}

// renderWithBlockOffsets is the markdown of doc and its blocks' byte ranges in it, validating doc
// first only when validate says so (renderDocument).
func renderWithBlockOffsets(doc *Node, validate bool) (string, []BlockOffset, error) {
	r, err := renderDocument(doc, maxSpanCells, validate)
	if err != nil {
		return "", nil, err
	}
	return r.b.String(), r.blockOffsets, nil
}

// render writes doc, its tables' spans adding at most spanCells empty cells (tableGrid), after
// validating doc.
func render(doc *Node, spanCells int) (*renderer, error) {
	return renderDocument(doc, spanCells, true)
}

// renderDocument is render, validating doc first only when validate says so: ValidDocument's
// rendering passes false for the tree it validated once already. A nil or non-doc tree is refused
// either way, every refusal of the renderer's own runs either way, and a panic is an ErrPanic
// either way.
func renderDocument(doc *Node, spanCells int, validate bool) (r *renderer, err error) {
	// The renderer reads back what it writes (blockKinds, parseInlineWithDefinitions).
	defer recoverPanic(&r, &err, "rendering a document")
	if doc == nil || doc.Type != "doc" {
		if doc == nil {
			return nil, fmt.Errorf("%w: Render wants a doc, got nil", ErrSchema)
		}
		return nil, fmt.Errorf("%w: Render wants a doc, got %q", ErrSchema, doc.Type)
	}
	if validate {
		if err := doc.Validate(); err != nil {
			return nil, err
		}
	}
	// Anchor marks render nothing, but one that starts or ends inside a word splits its text into
	// runs, and an escape is decided within one run: rendering the document without them merges
	// the runs, so a mark never changes the markdown (`snake_case`, never `snake\_case`). The line
	// breaks the browser editor holds that markdown has no form for are written as it draws them.
	doc = asWritten(doc)
	if holdsOnlyAnEmptyParagraph(doc) {
		return &renderer{spanBudget: spanCells}, nil
	}
	labels := definedFootnoteLabels(doc)
	r = renderBlocks(doc, labels, false, spanCells)
	// An empty code block in a typed block in a spread list item is written with a line between its
	// fences, as the writer before sjawhar/legion#1464 wrote it, except in a document holding a
	// shape this parser reads only as the browser editor does, where that line is refused
	// (emptyCodeWrittenBare): there the document is written again with the fences alone.
	if r.err == nil && r.wroteBlankEmptyCode && holdsBrowserOnlyShape(r.b.Bytes()) {
		r = renderBlocks(doc, labels, true, spanCells)
	}
	if r.err != nil {
		return nil, r.err
	}
	return r, nil
}

// renderBlocks writes doc's blocks, an empty code block in a typed block in a spread list item as
// its fences alone when bareEmptyCode says so, its tables' spans adding at most spanCells cells.
func renderBlocks(doc *Node, labels footnoteLabelSet, bareEmptyCode bool, spanCells int) *renderer {
	r := &renderer{footnoteLabels: labels, bareEmptyCode: bareEmptyCode, spanBudget: spanCells}
	r.blocks(doc.Children, "")
	if r.err != nil {
		return r
	}
	// A rule opening the document is written `---`, as it always was, except where that is
	// misread; there it is `***`, the same length. A `---` there opens front matter: a later line
	// that is `---` - a second rule, a code line - closes it and everything up to there reads as
	// front matter, and with no such line the browser editor's parser, having tried the front
	// matter to the document's end, reads no list, quote, footnote definition or typed block in the
	// rest.
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
	root, err := blockReader.parse(markdown[rest:], unclosed, readBackBudget(), fromStart)
	return err == nil && browserOnlyShape(root) != ""
}

// holdsAContainerTheBrowserDrops reports whether doc holds, at its top level, a block the browser
// editor's parser reads no more after an unclosed `---` opener: a list, a quote, a footnote
// definition or a typed block (frontmatterAttempt).
func holdsAContainerTheBrowserDrops(doc *Node) bool {
	for _, child := range doc.Children {
		switch child.Type {
		case "bullet_list", "ordered_list", "blockquote", "footnote_definition":
			return true
		}
		if IsTypedBlock(child.Type) {
			return true
		}
	}
	return false
}

// footnoteLabelSet is every label a document's footnote definitions carry, wherever they stand,
// kept two ways: as the browser editor's parser compares labels (footnoteLabelKey), since it
// matches a reference to its definition whatever the case, and lowercased, as this renderer
// compared them before it did, since text escaped then must be escaped still.
type footnoteLabelSet struct {
	keys, lowered    map[string]bool
	longestRuneCount int
	hasBracket       bool
}

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
				runeCount := 0
				for range label {
					runeCount++
				}
				labels.longestRuneCount = max(labels.longestRuneCount, runeCount)
				labels.hasBracket = labels.hasBracket || strings.Contains(label, "[")
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
			r.checkboxEnd = r.b.Len()
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

// codeBlockFence is a code block's fence: backticks, one more than the longest run of them in its
// text and at least three - or, where its language holds a backtick, which a backtick fence's info
// string cannot, tildes counted the same way.
func codeBlockFence(node *Node) string {
	fence := byte('`')
	if language, _ := node.Attrs["language"].(string); strings.Contains(language, "`") {
		fence = '~'
	}
	longest := 0
	for _, child := range node.Children {
		longest = max(longest, longestRun(child.Text, fence))
	}
	return strings.Repeat(string(fence), max(longest, 2)+1)
}

// longestRun is the length of the longest run of char in text, zero where text holds none.
func longestRun(text string, char byte) int {
	longest, run := 0, 0
	for index := range len(text) {
		if text[index] != char {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	return longest
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
// the readers to link again. A backtick in it would be written escaped, and the tokenizer reads the
// escape's backslash into the URL, so such a link is written as an autolink (isAngleURLLink). A URL
// the tokenizer reads at all (literalLength), its scheme in any case, must be read whole: one ending
// in punctuation the tokenizer leaves out of a URL before an end (literalTrail) is never read back
// whole. One the tokenizer refuses, such as a host opening with `-`, the transform reads from the
// text's decoding, and must be read whole there (findURLs).
func isBareAutolink(value string) bool {
	if value == "" || strings.ContainsAny(value, " \t\n<>`") || len(value) >= len("www.") && strings.EqualFold(value[:len("www.")], "www.") {
		return false
	}
	if length := literalLength(value); length != 0 {
		return length == len(value)
	}
	urls := findURLs(value)
	return len(urls) == 1 && urls[0].start == 0 && urls[0].end == len(value)
}

// isBareWWWLink reports whether node is a www link, its href `http://` and its text, that the
// tokenizer reads whole from that text (literalLength), which is written as the bare text where the
// character before it is one the readers read a www URL after (renderer.wwwMayFollow) and its run
// reads back so (inlineWithEscapes), and as an explicit link otherwise (runSpelling.angleURLs). Text
// the tokenizer reads an email from is not one: it tries an email first.
func isBareWWWLink(node *Node, marks []Mark, escapePipes bool) bool {
	for _, mark := range marks {
		if mark.Type != "link" {
			continue
		}
		href, _ := mark.Attrs["href"].(string)
		title, _ := mark.Attrs["title"].(string)
		value := node.Text
		return title == "" && href == "http://"+value && len(value) > len("www.") && strings.EqualFold(value[:len("www.")], "www.") &&
			!strings.ContainsAny(value, " \t\n<>`") && !(escapePipes && strings.Contains(value, "|")) && literalEmail([]byte(value)) == 0 &&
			literalLength(value) == len(value)
	}
	return false
}

// isBareEmailLink reports whether node is an email link: its text an address the transform's email
// pattern matches whole and links standing alone (findEmails), its href that address's (emailHref).
// Such a link is written explicit, and as its text, escaped as text is, only where its run reads
// back in no other spelling (runSpelling.bareEmails): after an explicit link the engine begins a
// text node, which it searches for an address of its own, so text after the link that holds one
// at its start reads back linked; written bare, the link and that text are one text node, searched
// once.
func isBareEmailLink(node *Node, marks []Mark) bool {
	for _, mark := range marks {
		if mark.Type != "link" {
			continue
		}
		href, _ := mark.Attrs["href"].(string)
		title, _ := mark.Attrs["title"].(string)
		value := node.Text
		if title != "" || href != emailHref(value) {
			return false
		}
		found := findEmails(nil, value, 0, len(value))
		return len(found) == 1 && found[0].start == 0 && found[0].end == len(value)
	}
	return false
}

// literalLength is the length of the URL the browser editor's tokenizer reads from value, or 0. A
// bare URL it reads is written as its bytes are: the tokenizer reads a URL's escapes and
// references as written. The transform, which reads one the tokenizer refuses, reads it from the
// decoded text, so its text is escaped as other text is.
func literalLength(value string) int {
	length, _ := literalURL([]byte(value), &literalDomains{from: -1, end: -1})
	return length
}

// isAngleURLLink reports whether node is a link whose text is its href, holding a backtick, which
// is written `<href>`, as the browser editor writes it: both parsers read an autolink's text as
// written, escapes and references included, where bare text would be escaped and the tokenizer
// would read the escape's backslash into the URL. One an autolink cannot hold (whitespace, `<`,
// `>`, a pipe in a table cell) is written as an explicit link.
func isAngleURLLink(node *Node, marks []Mark, escapePipes bool) bool {
	for _, mark := range marks {
		if mark.Type != "link" {
			continue
		}
		href, _ := mark.Attrs["href"].(string)
		title, _ := mark.Attrs["title"].(string)
		value := node.Text
		return href == value && title == "" && strings.Contains(value, "`") &&
			len(value) > len("https://") && (strings.EqualFold(value[:len("http://")], "http://") || strings.EqualFold(value[:len("https://")], "https://")) &&
			!strings.ContainsAny(value, " \t\n\r<>") && !(escapePipes && strings.Contains(value, "|"))
	}
	return false
}

// escapeLinkDestination writes a link's href as its destination (escapeDestination), an ampersand
// that would open a character reference written as `&amp;`, so a stored destination such as
// `x?a=1&amp;amp;b=2` keeps its bytes.
func escapeLinkDestination(href string, escapePipes bool) string {
	return escapeDestination(href, escapePipes, "&amp;")
}

// escapeImageSource writes an image's address as its destination (escapeDestination), an
// ampersand that would open a character reference written as `\&`.
func escapeImageSource(src string, escapePipes bool) string {
	return escapeDestination(src, escapePipes, `\&`)
}

// escapeDestination writes href as a destination that both readers, which decode a destination's
// escapes and character references, read back as href: a backslash before ASCII punctuation or
// ending href, which would escape what follows it, is escaped, an ampersand where it would open a
// character reference (characterReferenceAt) is written as ampersand, an ASCII control character
// (asciiControl), which ends a destination or is none to the engine's parser, is written as its
// numeric reference, and a pipe in a table cell is escaped. An href holding a space or a
// parenthesis is written between angle brackets, a `<` or `>` in it escaped; any other is written
// bare, a `<` opening it escaped.
func escapeDestination(href string, escapePipes bool, ampersand string) string {
	angle := strings.ContainsAny(href, " ()")
	var written strings.Builder
	written.Grow(len(href) + 4)
	if angle {
		written.WriteByte('<')
	}
	for index := range len(href) {
		char := href[index]
		switch {
		case char == '\\' && (index+1 == len(href) || isASCIIPunctuation(href[index+1])),
			char == '|' && escapePipes,
			char == '<' && (angle || index == 0),
			char == '>' && angle:
			written.WriteByte('\\')
		case char == '&':
			if _, _, ok := characterReferenceAt(href, index); ok {
				written.WriteString(ampersand)
				continue
			}
		case asciiControl(rune(char)):
			written.WriteString(numericEntity(rune(char)))
			continue
		}
		written.WriteByte(char)
	}
	if angle {
		written.WriteByte('>')
	}
	return written.String()
}

// asciiControl reports whether char is an ASCII control character, U+0000 to U+001F or U+007F.
func asciiControl(char rune) bool {
	return char < ' ' || char == 0x7f
}

// titleSuffix writes a link or image title, quoted, so that the browser editor's parser, which
// decodes a title's escapes and character references, reads it back: a backslash and a quote are
// escaped, an ampersand only where it would open a character reference (characterReferenceAt),
// and in a table a pipe as well.
func titleSuffix(title string, escapePipes bool) string {
	if title == "" {
		return ""
	}
	var written strings.Builder
	written.Grow(len(title) + len(` ""`))
	written.WriteString(` "`)
	for index := range len(title) {
		char := title[index]
		switch {
		case char == '\\' || char == '"' || char == '|' && escapePipes:
			written.WriteByte('\\')
		case char == '&':
			if _, _, ok := characterReferenceAt(title, index); ok {
				written.WriteByte('\\')
			}
		}
		written.WriteByte(char)
	}
	written.WriteByte('"')
	return written.String()
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

// escapeFootnoteLabel writes label so that the browser editor's parser, which decodes a label's
// escapes and character references, reads it back: a bracket, a pipe, and a backslash before ASCII
// punctuation or at the label's end are escaped, an ampersand only where it opens a run shaped as a
// character reference (characterReferenceLength), and white space, which no label holds as written,
// is a numeric character reference.
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
		case char == '&' && characterReferenceLength(label, index) > 0:
			written.WriteByte('\\')
		case unicode.IsSpace(char):
			written.WriteString("&#" + strconv.Itoa(int(char)) + ";")
			continue
		}
		written.WriteRune(char)
	}
	return written.String()
}

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
// (renderedMarkTypes) and with adjacent text runs under equal marks merged, as StripAnchorMarks
// gives it, with each line break its textblocks hold in the one form markdown carries, and with
// each U+0000 as U+FFFD (withNulsWritten). The browser editor holds two more line breaks. A soft
// line break it keeps from a paste (a hard break with isInline) it draws as a space, and one is
// written: both parsers read a soft break as one too. A line feed in text outside code it draws as
// a line break (white-space: break-spaces), and a hard break is written, which both parsers read
// back as the editor draws it, except where nothing but whitespace follows it in its textblock: a
// hard break there reads back as a backslash, and a line feed as nothing, as trailing whitespace
// does.
//
// It never writes doc, and shares doc's nodes, their attributes and their marks wherever they are
// written as they are: only a node written otherwise, and the nodes above it, is a copy, so a
// document with no anchor mark, no line break to rewrite and no U+0000 is doc itself. What it
// returns is only read.
func asWritten(doc *Node) *Node {
	out := writtenTree(doc, true)
	if holdsNul(out) {
		out = withNulsWritten(out)
	}
	return out
}

// writtenTree is node as asWritten writes it but for a U+0000 in its own text and attributes,
// which its parent writes once it has merged its children (childrenAsWritten); node itself where
// nothing under it is written otherwise. lineBreaks is whether a paragraph or heading at or under
// node has its line breaks written (writtenLineBreaks): not under a textblock or a code block.
func writtenTree(node *Node, lineBreaks bool) *Node {
	marks, marksKept := renderedMarks(node.Marks)
	children, childrenKept := childrenAsWritten(node, lineBreaks)
	if marksKept && childrenKept {
		return node
	}
	return &Node{Type: node.Type, Attrs: node.Attrs, Text: node.Text, Marks: marks, Children: children}
}

// renderedMarks is marks without those the rendering does not write, in their order, and whether
// that is marks itself: nil when none is written, a slice of its own when some are not.
func renderedMarks(marks []Mark) ([]Mark, bool) {
	rendered := 0
	for _, mark := range marks {
		if renderedMarkTypes[mark.Type] {
			rendered++
		}
	}
	switch rendered {
	case len(marks):
		return marks, true
	case 0:
		return nil, false
	}
	out := make([]Mark, 0, rendered)
	for _, mark := range marks {
		if renderedMarkTypes[mark.Type] {
			out = append(out, mark)
		}
	}
	return out, false
}

// childrenAsWritten is node's children as asWritten writes them, and whether that is node's own
// slice. Each child is written (writtenTree), and adjacent text runs whose written marks are equal
// merge into one that keeps the first's attributes. A paragraph or heading's children then have
// their line breaks written where they hold one to rewrite (holdsLineBreakToWrite). Only after
// those decisions is each child's own U+0000 written, so they are taken on the values as they are:
// two runs whose links differ only in a U+0000 and a U+FFFD stay two runs.
func childrenAsWritten(node *Node, lineBreaks bool) ([]*Node, bool) {
	textblock := lineBreaks && (node.Type == "paragraph" || node.Type == "heading")
	childLineBreaks := lineBreaks && !textblock && node.Type != "code_block"
	children := node.Children
	// out is nil while the children written so far are node's own; merged is whether its last
	// node is a merge of runs this pass made, whose text a later run may extend.
	var out []*Node
	merged := false
	for index, child := range children {
		clean := writtenTree(child, childLineBreaks)
		var previous *Node
		if out != nil {
			previous = out[len(out)-1]
		} else if index > 0 {
			previous = children[index-1]
		}
		mergeable := previous != nil && previous.Type == "text" && clean.Type == "text" && marksEqual(previous.Marks, clean.Marks)
		if mergeable && merged {
			previous.Text += clean.Text
			continue
		}
		if out == nil && (mergeable || clean != child) {
			out = make([]*Node, index, len(children))
			copy(out, children[:index])
		}
		if mergeable {
			out[len(out)-1] = &Node{Type: "text", Attrs: previous.Attrs, Text: previous.Text + clean.Text, Marks: previous.Marks, Children: previous.Children}
			merged = true
			continue
		}
		merged = false
		if out != nil {
			out = append(out, clean)
		}
	}
	kept := out == nil
	if kept {
		out = children
	}
	if textblock && holdsLineBreakToWrite(out) {
		out = writtenLineBreaks(out)
		kept = false
	}
	for index, child := range out {
		if !holdsNul(child) {
			continue
		}
		if kept {
			out = slices.Clone(out)
			kept = false
		}
		out[index] = withNulsWritten(child)
	}
	return out, kept
}

// holdsLineBreakToWrite reports whether writtenLineBreaks gives nodes, a textblock's inline nodes
// with no two adjacent text runs under equal marks (childrenAsWritten), back otherwise: a hard
// break with isInline, a line feed in text outside code, or a text it does not keep as it is (an
// empty one, or one with attributes or children).
func holdsLineBreakToWrite(nodes []*Node) bool {
	for _, node := range nodes {
		switch node.Type {
		case "hardbreak":
			if node.Attrs["isInline"] == true {
				return true
			}
		case "text":
			if node.Text == "" || len(node.Attrs) > 0 || len(node.Children) > 0 ||
				strings.IndexByte(node.Text, '\n') >= 0 && !nodeHasMark(node, "inlineCode") {
				return true
			}
		}
	}
	return false
}

// nulAsReplacement is text with each U+0000 written as U+FFFD, which is what CommonMark reads the
// character as (§2.3 Insecure characters), so no markdown carries one back; PostgreSQL's text and
// jsonb cannot store one either. A browser's edit can still put one in a live document, past every
// check a route makes, so text Dispatch takes from a document's tree passes through this: its
// rendering (asWritten), an anchor's quote (FindMark), an ask block's question and options and a
// heading's text (TextContent), and the text a quote is matched against (buildFlattenedText). A
// document's settlement and every write that versions it store that text, so one U+0000 left in it
// as it is would fail all of them, and text copied from what a read serves would match nothing.
func nulAsReplacement(text string) string {
	if strings.IndexByte(text, 0) < 0 {
		return text
	}
	return strings.ReplaceAll(text, "\x00", "\uFFFD")
}

// holdsNul reports whether node holds, in its own text or attributes, a U+0000 that
// withNulsWritten writes.
func holdsNul(node *Node) bool {
	if strings.IndexByte(node.Text, 0) >= 0 {
		return true
	}
	if attrsHoldNul(node.Attrs) {
		if !IsTypedBlock(node.Type) {
			return true
		}
		if id, _ := node.Attrs[BlockIDAttr].(string); strings.IndexByte(id, 0) >= 0 {
			return true
		}
	}
	for _, mark := range node.Marks {
		if attrsHoldNul(mark.Attrs) {
			return true
		}
	}
	return false
}

func attrsHoldNul(attrs Attrs) bool {
	for _, value := range attrs {
		if text, ok := value.(string); ok && strings.IndexByte(text, 0) >= 0 {
			return true
		}
	}
	return false
}

// withNulsWritten is a copy of node, which holds a U+0000 (holdsNul), with each U+0000 the
// rendering would write as it is - in its text, in a string attribute a node or mark writes (a
// link's href, an image's alt, a code block's language), or in a block's id - as U+FFFD
// (nulAsReplacement). node may be asWritten's input, so the copy's attributes and marks are its
// own before any is written. A typed block's other attributes are written quoted
// (renderTypedAttributes), U+0000 as the escape \x00 that its parser unquotes back, so they keep
// it; its id is written bare, `#` and the id.
func withNulsWritten(node *Node) *Node {
	out := &Node{Type: node.Type, Attrs: cloneAttrs(node.Attrs), Text: nulAsReplacement(node.Text), Marks: cloneMarks(node.Marks), Children: node.Children}
	if !IsTypedBlock(out.Type) {
		writeNulsInAttrs(out.Attrs)
	} else if id, ok := out.Attrs[BlockIDAttr].(string); ok && strings.IndexByte(id, 0) >= 0 {
		out.Attrs[BlockIDAttr] = nulAsReplacement(id)
	}
	for _, mark := range out.Marks {
		writeNulsInAttrs(mark.Attrs)
	}
	return out
}

func writeNulsInAttrs(attrs Attrs) {
	for name, value := range attrs {
		if text, ok := value.(string); ok && strings.IndexByte(text, 0) >= 0 {
			attrs[name] = nulAsReplacement(text)
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
	onlyWhitespaceAfter := make([]bool, len(nodes))
	whitespace := true
	for index := len(nodes) - 1; index >= 0; index-- {
		onlyWhitespaceAfter[index] = whitespace
		node := nodes[index]
		whitespace = whitespace && node.Type == "text" && strings.TrimSpace(node.Text) == ""
	}
	for index, node := range nodes {
		switch {
		case node.Type == "hardbreak" && node.Attrs["isInline"] == true:
			appendText(" ", nil)
		case node.Type == "text" && !nodeHasMark(node, "inlineCode") && strings.Contains(node.Text, "\n"):
			value := node.Text
			lastNonWhitespace := len(strings.TrimRightFunc(value, unicode.IsSpace))
			lineStart := 0
			for {
				lineEnd := strings.IndexByte(value[lineStart:], '\n')
				if lineEnd < 0 {
					appendText(value[lineStart:], node.Marks)
					break
				}
				lineEnd += lineStart
				appendText(value[lineStart:lineEnd], node.Marks)
				if lastNonWhitespace <= lineEnd+1 && onlyWhitespaceAfter[index] {
					appendText("\n", node.Marks)
				} else {
					out = append(out, &Node{Type: "hardbreak", Attrs: Attrs{"isInline": false}})
				}
				lineStart = lineEnd + 1
			}
		case node.Type == "text":
			appendText(node.Text, node.Marks)
		default:
			out = append(out, node)
		}
	}
	return out
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
