package pmdoc

import (
	"bytes"
	"fmt"
	"strconv"
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
	// typed is the innermost typed block the blocks being written stand in, or nil outside one
	// (typedScope). A lone `:::` closes a three-colon block as a line less than four columns past
	// the prefix its lines are written at (typedFenceReach), which a paragraph written there can
	// produce.
	typed *typedScope
	// heldLineStart is the current line's first text character, held until the line is written.
	heldLineStart *lineCandidate
	// footnoteLineAt is where the last footnote definition's first line begins in the markdown,
	// and footnoteLabel is its label as written, so that the line is read after a reference to it.
	footnoteLineAt int
	footnoteLabel  string
	// inFootnote reports whether the blocks being written are inside a footnote definition,
	// footnoteQuotes is how many quotes stand around that definition, and footnoteIndentAt is
	// where its indentation starts in the prefix of every line written inside it.
	inFootnote       bool
	footnoteQuotes   int
	footnoteIndentAt int
	// itemDepth is how many list items the blocks being written stand in, and quoteDepth how many
	// quotes, each a quote marker on their lines' prefix.
	itemDepth  int
	quoteDepth int
	// containers is the list items, quotes, footnote definitions and typed blocks the blocks being
	// written stand in, outermost first.
	containers []*Node
	// asteriskRule makes the next rule written `***` rather than `---` (list).
	asteriskRule bool
	// otherListMarker makes the next list written with its kind's other marker (otherListMarkers).
	otherListMarker bool
	// footnoteLabels is every footnote label the document defines (footnoteLabelSet).
	footnoteLabels footnoteLabelSet
	// listsReadLoose is, for each list the blocks being written stand in, outermost first, whether
	// goldmark's looseness reads its spread as it is (loosenessReadsSpread); bareEmptyCode writes an
	// empty code block in a typed block in a list item as its fences alone, and wroteBlankEmptyCode
	// records one written with a line between them (emptyCodeWrittenBare).
	listsReadLoose                     []bool
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
	// the runs, so a mark never changes the markdown (`snake_case`, never `snake\_case`).
	doc = StripAnchorMarks(doc)
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
		if front, _ := parseFrontmatterBlock(r.b.Bytes()); front != nil || holdsAContainerTheBrowserDrops(doc) {
			copy(r.b.Bytes(), "***")
		}
	}
	return r
}

// holdsBrowserOnlyShape reports whether markdown, read as Parse reads it, holds a shape this
// parser reads only as the browser editor's parser does (browserOnlyShape).
func holdsBrowserOnlyShape(markdown []byte) bool {
	_, rest := parseFrontmatterBlock(markdown)
	return browserOnlyShape(blockReader.parse(markdown[rest:])) != ""
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
		r.enter(n)
		r.quoteDepth++
		r.blocksNoTrailing(n.Children, prefix+"> ")
		r.blanksEndingQuotedList(n.Children, prefix+"> ", false)
		r.quoteDepth--
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
		outer, outerQuotes, outerIndentAt := r.inFootnote, r.footnoteQuotes, r.footnoteIndentAt
		r.inFootnote, r.footnoteQuotes, r.footnoteIndentAt = true, r.quoteDepth, len(prefix)
		// The browser editor's parser reads the lines of a definition a list item holds as the
		// item's, so in a tight item a blank line between two of its blocks would spread the item.
		tightItem := len(r.containers) > 0 && r.containers[len(r.containers)-1].Type == "list_item" && r.containers[len(r.containers)-1].Attrs["spread"] != true
		r.enter(n)
		if tightItem {
			r.itemBlocks(n.Children, false, prefix, prefix+definitionIndent, false)
		} else {
			r.blocksNoTrailing(n.Children, prefix+definitionIndent)
		}
		r.leave()
		r.inFootnote, r.footnoteQuotes, r.footnoteIndentAt = outer, outerQuotes, outerIndentAt
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
		outer := r.typed
		r.typed = &typedScope{prefix: prefix, colons: colons, quotes: r.quoteDepth}
		r.enter(n)
		r.blocksNoTrailing(n.Children, prefix)
		r.blanksEndingQuotedList(n.Children, prefix, true)
		r.leave()
		r.typed = outer
		r.writeSyntax("\n" + prefix + fence)
	}
}

// typedScope is a typed block the renderer is writing the blocks of: the prefix its lines are written
// at, how many colons its fence has, and how many quotes stand around it.
type typedScope struct {
	prefix string
	colons int
	quotes int
}

// enter and leave bracket the writing of a container's blocks (containers).
func (r *renderer) enter(container *Node) { r.containers = append(r.containers, container) }

func (r *renderer) leave() { r.containers = r.containers[:len(r.containers)-1] }

// definitionOpensOnALaterLine reports whether the footnote definition being written opens with
// nothing past its `]:` and its first block on the next line, which the browser editor's parser
// reads as a blank line of the list item holding the definition, spreading it. It is written so
// where the definition is the only block a spread item writes and nothing else spreads the item:
// no blank line between two of the definition's own blocks (spreadByItsOwnLines), and no quote
// around the item, where the blank lines after it are the definition's (blanksAfterDefinitionItem).
func (r *renderer) definitionOpensOnALaterLine() bool {
	if len(r.containers) == 0 || r.quoteDepth > 0 || r.inFootnote {
		return false
	}
	item := r.containers[len(r.containers)-1]
	return item.Type == "list_item" && item.Attrs["spread"] == true && !spreadByItsOwnLines(item, false, false)
}

// inItemBelowQuotes reports whether the blocks being written stand in a list item with no quote
// between: a line holding only their prefix is a blank line in the item, which the browser editor
// reads as spacing it, where with a quote between it is the quote's.
func (r *renderer) inItemBelowQuotes() bool {
	for index := len(r.containers) - 1; index >= 0; index-- {
		switch r.containers[index].Type {
		case "blockquote":
			return false
		case "list_item":
			return true
		}
	}
	return false
}

// emptyCodeWrittenBare reports whether an empty code block is written as its fences alone, with no
// line between them, where no quote between takes a line holding only the prefix as its own: in a
// footnote definition, where both parsers keep that line as the code's text, and in a typed block in
// a list item, where it is a blank line the browser editor reads as spacing the nearest item. That
// line is written there, as main wrote it, where the nearest item is spread and goldmark's
// looseness, which this parser leaves that item's list to, reads the list's spread as it is
// (loosenessReadsSpread), unless the document holds a shape this parser cannot decide the spacing
// of (browserOnlyShape), where it is refused: render writes such a document again with the fences
// alone (bareEmptyCode).
func (r *renderer) emptyCodeWrittenBare() bool {
	typed := false
	items := 0
	for index := len(r.containers) - 1; index >= 0; index-- {
		switch r.containers[index].Type {
		case "blockquote":
			return false
		case "footnote_definition":
			return true
		case "list_item":
			if !typed {
				items++
				continue
			}
			if r.bareEmptyCode || r.containers[index].Attrs["spread"] != true || !r.listsReadLoose[len(r.listsReadLoose)-1-items] {
				return true
			}
			r.wroteBlankEmptyCode = true
			return false
		default:
			typed = true
		}
	}
	return false
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
	r.listsReadLoose = append(r.listsReadLoose, loosenessReadsSpread(n))
	defer func() { r.listsReadLoose = r.listsReadLoose[:len(r.listsReadLoose)-1] }()
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
		r.itemDepth++
		r.enter(item)
		r.itemBlocks(children, item.Attrs["spread"] == true, prefix, indent, skipped && marker != "* ")
		r.leave()
		r.itemDepth--
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
// would run on into a paragraph after it and underline itself with a rule's `---`. The browser
// editor's writer puts a blank line between two paragraphs (the item then reads back spread) and
// writes a rule `***`, and so does this renderer; firstRule reports whether a rule opening the
// blocks is written `***` too.
func (r *renderer) itemBlocks(blocks []*Node, spread bool, prefix, indent string, firstRule bool) {
	otherMarkers := otherListMarkers(blocks)
	for index, child := range blocks {
		afterParagraph := index > 0 && blocks[index-1].Type == "paragraph"
		if index > 0 {
			blanks := 0
			if spread || afterParagraph && child.Type == "paragraph" {
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
			} else if previous.Type == "footnote_definition" && r.quoteDepth > 0 && child.Type != "blockquote" && child.Type != "footnote_definition" && !isList(child) {
				// In a quote the blank lines after a footnote definition are the definition's: one
				// spreads the item only before a quote, a list or a definition, and two spread it
				// before anything else (definitionBlanksInQuote), an empty definition's own line
				// being one of them. A spread item writes two where no blank line between two of
				// its other blocks spreads it, and a tight one writes one only to keep a block that
				// cannot open on the line after a paragraph off the definition's, and none after a
				// definition ending in a list no line continues (endsInClosedList) or an empty one.
				own := 0
				if holdsOnlyAnEmptyParagraph(previous) {
					own = 1
				}
				switch {
				case spread && !spreadElsewhere(blocks):
					blanks = 2 - own
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
		// A row with no cells is written as nothing: a table with only such rows reads back
		// holding one, as the browser editor's parser reads a table with no body row.
		if len(row.Children) == 0 {
			continue
		}
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
	if r.inFootnote && r.quoteDepth == r.footnoteQuotes && blankLineAhead(rest) {
		switch marker := strings.LastIndex(prefix, "> "); {
		case rest != "" && rest[0] != '\n':
			prefix = prefix[:r.footnoteIndentAt] + prefix[r.footnoteIndentAt+len(definitionIndent):]
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

func isBareAutolink(value string) bool {
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return false
	}
	if strings.ContainsAny(value, " \t\n()<>") {
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
