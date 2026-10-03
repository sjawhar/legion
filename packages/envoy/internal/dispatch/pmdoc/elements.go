package pmdoc

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// MaxDocumentElements is how many elements one document's markdown may make, and the markdown one
// caller write sends, weighed as nodeWeight weighs them and as an upload's parse counts them.
// What a byte of markdown makes ranges from nothing to an element per byte, and every element costs
// the server memory in each stage a write runs - goldmark's tree, the Proof tree, the renderer's
// read-back of each run, the read-back of the whole document, the live document - and in every read
// and settlement of the document it stores: a mebibyte of `)_` makes 786,432 of them and held a
// gigabyte (LEGION-481). The heaviest document of each shape measured at this limit holds at most
// about 190 MiB above the server's idle memory to store and settle (a four-column table), and
// 150 MiB to read (cmd/dispatch's memory tests). Over this repository's 618 markdown files
// (MeasureDocument), every one of 4 KiB or more weighs 1.5 to 379 elements a kibibyte, so prose
// passes to the 1 MiB cap and the densest, a comparison matrix, to about 173 KiB; smaller files run
// denser, up to 1,296 a kibibyte for a 147-byte test fixture of empty list items.
const MaxDocumentElements = 65_536

// nodeGuard is how many times MaxDocumentElements goldmark may make nodes, lines and table cells,
// counted while it parses, before the parse stops. Markdown counts there at most about twice what
// it weighs - a line of text counts as its line and its text, and weighs one element - so the guard
// stops only a write at or past the limit, and goldmark builds no more of the tree than that:
// refusing a mebibyte of any shape measured allocates at most about 130 MiB (hard breaks, which
// count two and weigh four), where reading a mebibyte of `)_` whole allocated 2.6 GB.
const nodeGuard = 2

// ErrTooManyElements is the refusal of markdown that makes more elements than one write may
// (MaxDocumentElements). It is not ErrSchema: the markdown is well formed, and too large to store.
var ErrTooManyElements = errors.New("document too large to store")

// elementCount is what the markdown one caller write sends makes, across every parse of it. A
// write's WriteBudget carries it, so every parse of that write's markdown spends it; a limit of
// zero counts nothing, as a read-back's does. It holds only the write's totals: what one parse
// tracks while it runs is that parse's parseCount, which goes with the parse.
type elementCount struct {
	// limit is the most elements the write may make, and made the elements its parses have made
	// (weigh).
	limit, made int
	// nodes is what goldmark has made while parsing the write's markdown, counted as it goes:
	// every block it opens and every line a block reads (elementBlockCounter), every node a
	// textblock's inline syntax leaves (elementInlineCounter), every mark a delimiter pair makes
	// (emphasisDelimiters.OnMatch), and every table cell (lazyTableRows). Past nodeGuard*limit the
	// parse stops making them.
	nodes int
}

func newElementCount(limit int) elementCount {
	return elementCount{limit: limit}
}

// parseCount is one parse's share of its write's elementCount: the state the counters keep while
// goldmark reads one source, kept in that parse's context (elementCountKey) so that none of it, the
// goldmark tree parent points into least of all, outlives the parse on the write's budget.
type parseCount struct {
	write *elementCount
	// at is where in the parsed source the count passed its limit, and -1 while it has not;
	// stopped says the guard passed there, so goldmark stopped building the tree. last is where the
	// latest inline node was counted, which a mark made while a textblock's delimiters pair is
	// charged at.
	at, last int
	stopped  bool
	// server is how many bytes the parsed source opens with that the server wrote ahead of the
	// caller's markdown - parseTableRows's header - which charge nothing.
	server int
	// parent and children are the textblock the inline count last read and its child count then.
	parent   ast.Node
	children int
}

// elementCountKey is the context key of a counting parse's parseCount.
var elementCountKey = parser.NewContextKey()

// countParse starts the count of one parse of the write's markdown in pc, whose source opens with
// server bytes the server wrote, or returns nil for a budget that counts no elements.
func (c *elementCount) countParse(pc parser.Context, server int) *parseCount {
	if c.limit == 0 {
		return nil
	}
	count := &parseCount{write: c, at: -1, server: server}
	pc.Set(elementCountKey, count)
	return count
}

// charge counts n nodes goldmark made at offset in the parsed source and reports whether the
// count has passed the guard. Nodes in the server's own prefix charge nothing.
func (p *parseCount) charge(n, offset int) bool {
	if p.passed() || offset < p.server {
		return p.passed()
	}
	p.write.nodes += n
	if p.write.nodes > nodeGuard*p.write.limit {
		p.at, p.stopped = offset, true
		return true
	}
	return false
}

// passed reports whether the parse has passed its write's limit.
func (p *parseCount) passed() bool {
	return p.at >= 0
}

// countedElements is the count of the parse pc belongs to, or nil for a parse that counts none.
func countedElements(pc parser.Context) *parseCount {
	count, _ := pc.Get(elementCountKey).(*parseCount)
	return count
}

// What elementWeight weighs each node of goldmark's tree, in units of an inline node's: the memory
// the node costs at its worst, measured through the upload route.
const (
	// inlineWeight is a piece of inline syntax, a mark or a line of text.
	inlineWeight = 1
	// autolinkWeight is an autolink: the text and the link mark the Proof tree makes of it, where
	// goldmark keeps no text node.
	autolinkWeight = 2
	// blockWeight is a block other than a table cell: the Proof block and the attributes it
	// carries in the live document.
	blockWeight = 3
	// tableCellWeight is a table cell: the cell, the paragraph and the text the Proof tree makes
	// of it.
	tableCellWeight = 4
	// hardbreakWeight is a hard line break, beside the line of text it ends. The Proof tree makes
	// it a node of its own between two texts, and the live document an element splitting its
	// textblock's text in two, so it costs what a block does: weighed as one inline node, the
	// heaviest document of hard breaks the limit admitted held 350 MiB stored, and four cold reads
	// of it at once 1,190 MiB.
	hardbreakWeight = 3
	// rawHTMLWeight is a piece of inline HTML, a tag or a comment: the Proof tree makes it a node
	// of its own carrying the HTML as an attribute, and the live document an element with that
	// attribute, splitting its textblock's text. Weighed as one inline node, the heaviest document
	// of `<b>a</b>` spans the limit admitted held 213 to 257 MiB to store, the most of any shape and
	// past the 256 MiB a request may hold.
	rawHTMLWeight = 2
	// escapeWeight is a backslash escape or a character reference in a text: goldmark keeps it in
	// its text node, so it makes no node there, but it spells a character - `_`, `~`, `[` - that
	// the Proof tree holds bare and every rendering writes again, and the renderer reads each run
	// back in a spelling that leaves such characters bare (inlineWithEscapes), where they make the
	// delimiters and link openers they would unescaped: a mebibyte of `\~a`, `)\_`, `\)\_` or
	// `\[a` weighed four elements and allocated 330 to 720 MiB to parse and 260 to 620 MiB to
	// render, and the stored `\)\_` held 256 MiB to read cold and 973 MiB for four reads at once.
	escapeWeight = 1
)

// nodeWeight is what one node of goldmark's tree, read from source, weighs: elementWeight, and
// escapeWeight for each escape a text node outside a code span carries.
func nodeWeight(node ast.Node, source []byte) int {
	weight := elementWeight(node)
	if text, ok := node.(*ast.Text); ok && node.Parent().Kind() != ast.KindCodeSpan {
		weight += escapeWeight * escapes(text.Segment.Value(source))
	}
	return weight
}

// escapes is how many backslash escapes of punctuation and character references (characterReference)
// value holds, the spellings the parser reads as the one character each names.
func escapes(value []byte) int {
	count := 0
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '\\':
			if index+1 < len(value) && util.IsPunct(value[index+1]) {
				count++
				index++
			}
		case '&':
			if reference := characterReference.Find(value[index:]); reference != nil {
				count++
				index += len(reference) - 1
			}
		}
	}
	return count
}

// elementWeight is what one node of goldmark's tree weighs.
func elementWeight(node ast.Node) int {
	switch {
	case node.Kind() == ast.KindDocument:
		return 0
	case node.Kind() == extensionast.KindTableCell:
		return tableCellWeight
	case node.Type() == ast.TypeBlock:
		return blockWeight
	case node.Kind() == ast.KindText && node.(*ast.Text).HardLineBreak():
		return inlineWeight + hardbreakWeight
	case node.Kind() == ast.KindAutoLink:
		return autolinkWeight
	case node.Kind() == ast.KindRawHTML:
		return rawHTMLWeight
	}
	return inlineWeight
}

// weights is how a refusal of a write's elements names the weights.
var weights = fmt.Sprintf("a block weighs %d elements, a table cell %d, a hard line break %d, a piece of inline HTML %d, an autolink %d, and each piece of inline syntax, escape, mark and line of text %d",
	blockWeight, tableCellWeight, hardbreakWeight, rawHTMLWeight, autolinkWeight, inlineWeight)

// MaxDocumentBytes is the most bytes of markdown one document may hold: what one upload of a
// markdown document may send, and what a write may grow a stored document's rendering to.
const MaxDocumentBytes = 1 << 20

// DocumentSize is what markdown measures as an upload of it is measured (ParseForWrite): its bytes,
// against MaxDocumentBytes, and the elements the upload's parse makes of it - elementWeight over the
// tree goldmark builds, front matter apart - against MaxDocumentElements.
type DocumentSize struct {
	Bytes int
	// Elements is the markdown's weight when Counted. Goldmark stops building the tree once it has
	// made nodeGuard times the limit, and then the markdown is not Counted: it makes more elements
	// than the limit, by how many is unknown, and Elements is zero.
	Elements int
	Counted  bool
}

// TooLong reports whether the markdown is past MaxDocumentBytes.
func (s DocumentSize) TooLong() bool { return s.Bytes > MaxDocumentBytes }

// TooHeavy reports whether the markdown makes more than MaxDocumentElements.
func (s DocumentSize) TooHeavy() bool { return !s.Counted || s.Elements > MaxDocumentElements }

// MeasureDocument is the DocumentSize of markdown, a whole document. Its elements are counted
// exactly as an upload's parse counts them, its guard included, so markdown MeasureDocument finds
// within both limits is what one upload may send, as far as its size goes. Markdown an upload's
// parse refuses for its shape - nested past the parse's bounds, or tables whose short rows would
// take more padding cells than one write may add, which the parse leaves as a paragraph that weighs
// next to nothing - measures as markdown past the element limit: a 101-column header over 200
// one-cell rows measured 407 elements where it weighs 82,111 written whole. The renderer refuses a
// tree nested that deep, and writes such short rows only where it leaves browser-made spans
// unwritten.
func MeasureDocument(markdown string) DocumentSize {
	size := DocumentSize{Bytes: len(markdown)}
	source := []byte(markdown)
	_, rest, unclosedFrontmatter := parseFrontmatterBlock(source)
	root, count, pc, err := blockReader.read(source[rest:], unclosedFrontmatter, NewWriteBudget(), 0)
	if err != nil || count.passed() {
		return size
	}
	if refused, _ := pc.Get(tablePaddingErrorKey).(error); refused != nil {
		return size
	}
	for node := root; node != nil; node = nextInTree(root, node) {
		size.Elements += nodeWeight(node, source[rest:])
	}
	size.Counted = true
	return size
}

// weigh charges the write the elements root, read from source, makes, which the parse p counts
// made, and records where they take it past its limit. root is weighed without recursion, so a tree
// nested as deep as the guard allows costs no stack. A tree the guard stopped is not weighed, and a
// parse that counts nothing (a nil p) weighs nothing.
func (p *parseCount) weigh(root ast.Node, source []byte) {
	if p == nil || p.passed() {
		return
	}
	for node := root; node != nil; node = nextInTree(root, node) {
		if p.server > 0 && nodeOffset(node) < p.server {
			continue
		}
		p.write.made += nodeWeight(node, source)
		if p.write.made > p.write.limit {
			p.at = nodeOffset(node)
			return
		}
	}
}

// refusal is the refusal of the weighed parse p counts, read from source whose first line is
// firstLine of what the caller wrote, or nil: goldmark passed the guard, or the elements its tree
// makes take the write past its limit.
//
// A refusal of the tree's weight names the line where it passed the limit. One of the guard names
// the line goldmark stopped reading at, and says only that: the guard counts nodes rather than
// weight, in both of goldmark's phases - every line's blocks first, then each textblock's inline
// syntax - so where it stops can lie well before or well after where the markdown's weight passed
// the limit, and the tree it leaves unbuilt cannot say where that was.
func (p *parseCount) refusal(source []byte, firstLine int) error {
	if p == nil || !p.passed() {
		return nil
	}
	passing := "passing that at line"
	if p.stopped {
		passing = "too many to read past line"
	}
	line := firstLine + bytes.Count(source[:min(p.at, len(source))], []byte("\n"))
	return fmt.Errorf("%w: this write's markdown makes more than %d elements, %s %d; %s. Shorten the change, or split the document; an upload of data rather than prose can go up as a file of another content type",
		ErrTooManyElements, p.write.limit, passing, line, weights)
}

// plainTextSyntax is the characters of plain text that its markdown writes escaped, or bare as the
// inline syntax they open: a delimiter, a link's bracket, a code span, raw HTML. Each weighs at
// least an element in the rendering, an escape or the node it makes.
const plainTextSyntax = "\\*_~`[]<"

// lineFeedWeight is the least a line feed in plain text weighs in its rendering: it is written as a
// hard break, beside the text after it, or, two at once, as a paragraph and its text.
const lineFeedWeight = (blockWeight + inlineWeight) / 2

// RefusePlainText is the refusal of texts, which a caller writes into a document as plain text -
// an ask's question and its options - when what their rendering weighs at least, an element for
// each syntax character (plainTextSyntax) and lineFeedWeight for each line feed, would take the
// write b budgets past its element limit, or nil. Plain text is made into nodes, rendered escaped
// and read back run by run (inlineWithEscapes) before any parse of its rendering counts what it
// makes: a 920 KB option description of `)_` held 417 MiB to render, and one of 330,000 lines
// did not finish its write in twenty minutes. Nothing is charged here; the parse of the rendering
// charges the write what it makes.
func (b *WriteBudget) RefusePlainText(texts ...string) error {
	weight := 0
	for _, text := range texts {
		for index := range len(text) {
			switch {
			case text[index] == '\n':
				weight += lineFeedWeight
			case strings.IndexByte(plainTextSyntax, text[index]) >= 0:
				weight += escapeWeight
			}
		}
	}
	if b.elements.made+weight <= b.elements.limit {
		return nil
	}
	return fmt.Errorf("%w: this write's text weighs at least %d elements as markdown - its line feeds and the characters its markdown writes escaped or reads as syntax (%s) - past the %d one write may make (%s); shorten it, or split the document",
		ErrTooManyElements, weight, plainTextSyntax, b.elements.limit, weights)
}

// nextInTree is the node after node in a walk of root's tree in document order.
func nextInTree(root, node ast.Node) ast.Node {
	if child := node.FirstChild(); child != nil {
		return child
	}
	for node != root {
		if next := node.NextSibling(); next != nil {
			return next
		}
		node = node.Parent()
	}
	return nil
}

// nodeOffset is where node starts in the parsed source, or where the nearest ancestor that records
// one starts.
func nodeOffset(node ast.Node) int {
	for ; node != nil; node = node.Parent() {
		if text, ok := node.(*ast.Text); ok {
			return text.Segment.Start
		}
		if position := node.Pos(); position >= 0 {
			return position
		}
		if node.Type() == ast.TypeBlock && node.Lines().Len() > 0 {
			return node.Lines().At(0).Start
		}
	}
	return 0
}

// elementBlockCounter is a block parser that opens nothing but counts what goldmark's block phase
// makes: tried first wherever goldmark tries to open a block, every container level of every line
// it reads, so each block and each line counts once. Once the write's markdown passes the guard it
// opens a raw block that takes every line after it unread, so a refused document costs no more of
// goldmark's tree than the guard allows. A list holds only list items, which goldmark's list parser
// reads its children as, so no raw block opens in a list itself: the list closes instead
// (elementListGuard), and the raw block opens where the list was.
type elementBlockCounter struct{}

// everyByte is every byte a line can open with: the counter is tried for each line whatever opens
// it, ahead of the parsers that line's first byte triggers.
var everyByte = func() []byte {
	all := make([]byte, 0, 256)
	for b := range 256 {
		all = append(all, byte(b))
	}
	return all
}()

func (elementBlockCounter) Trigger() []byte { return everyByte }

func (elementBlockCounter) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	count := countedElements(pc)
	if count == nil {
		return nil, parser.NoChildren
	}
	_, segment := reader.Position()
	if !count.charge(1, segment.Start) || parent.Kind() == ast.KindList {
		return nil, parser.NoChildren
	}
	return ast.NewCodeBlock(), parser.NoChildren
}

// Continue keeps the raw block a passed limit opens to the end of what it is in, reading nothing.
func (elementBlockCounter) Continue(ast.Node, gmtext.Reader, parser.Context) parser.State {
	return parser.Continue | parser.NoChildren
}

func (elementBlockCounter) Close(ast.Node, gmtext.Reader, parser.Context) {}

func (elementBlockCounter) CanInterruptParagraph() bool { return true }

func (elementBlockCounter) CanAcceptIndentedLine() bool { return true }

// elementListGuard is goldmark's list parser closing its list on the first line after the write's
// markdown passed the guard, so that line opens the raw block that takes the rest
// (elementBlockCounter) where the list stood, rather than one more list item and the raw block in
// it, as every later line would.
type elementListGuard struct{ parser.BlockParser }

func (p elementListGuard) Continue(node ast.Node, reader gmtext.Reader, pc parser.Context) parser.State {
	if count := countedElements(pc); count != nil && count.passed() {
		return parser.Close
	}
	return p.BlockParser.Continue(node, reader, pc)
}

// elementInlineCounter is an inline parser that parses nothing but counts what goldmark's inline
// phase makes: tried first at every character an inline parser triggers on, a line's start among
// them, it charges the nodes the textblock has gained since it last looked - the text goldmark ends
// there and the node the trigger before made. Once the write's markdown passes its limit it takes
// the rest of each line as one text, so no further node is made.
type elementInlineCounter struct{}

// Trigger is every character the readers' inline parsers trigger on: goldmark's code span, link,
// autolink, raw HTML and emphasis parsers, and the linkify (a space stands for every space and a
// line's start), strikethrough, footnote and task list extensions.
func (elementInlineCounter) Trigger() []byte {
	return []byte{' ', '*', '_', '~', '(', '`', '[', ']', '!', '<'}
}

func (elementInlineCounter) Parse(parent ast.Node, block gmtext.Reader, pc parser.Context) ast.Node {
	count := countedElements(pc)
	if count == nil {
		return nil
	}
	line, segment := block.PeekLine()
	count.last = segment.Start
	if count.parent != parent {
		count.parent, count.children = parent, 0
	}
	if grown := parent.ChildCount() - count.children; grown > 0 {
		count.charge(grown, segment.Start)
	}
	count.children = parent.ChildCount()
	if !count.passed() {
		return nil
	}
	rest := len(line)
	if rest > 0 && line[rest-1] == '\n' {
		rest--
	}
	if rest == 0 {
		return nil
	}
	block.Advance(rest)
	return ast.NewTextSegment(segment.WithStop(segment.Start + rest))
}
