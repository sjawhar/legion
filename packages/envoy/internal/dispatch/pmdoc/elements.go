package pmdoc

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
)

// maxWriteElements is how many elements the markdown one caller write sends may make, weighed
// as elementWeight weighs them. What a byte of markdown makes ranges from nothing to an element per
// byte, and every element costs the server memory in each stage a write runs - goldmark's tree, the
// Proof tree, the renderer's read-back of each run, the read-back of the whole document, the live
// document - and in every read and settlement of the document it stores: a mebibyte of `)_`
// makes 786,432 of them and held a gigabyte (LEGION-481). The heaviest document of each shape
// measured at this limit holds at most about 220 MiB above the server's idle memory to store and
// settle, and 170 MiB to read (cmd/dispatch's memory tests). This repository's own markdown weighs
// 44 to 100 elements a kibibyte, its route tables the most, so prose passes to the 1 MiB cap and a
// table-dense document to about 650 KiB.
const maxWriteElements = 65_536

// nodeGuard is how many times maxWriteElements goldmark may make nodes, lines and table cells,
// counted while it parses, before the parse stops. Markdown counts there at most about twice what
// it weighs - a line of text counts as its line and its text, and weighs one element - so the guard
// stops only a write at or past the limit, and goldmark builds no more of the tree than that:
// refusing a mebibyte of any shape measured allocates at most about 130 MiB (hard breaks, which
// count two and weigh four), where reading a mebibyte of `)_` whole allocated 2.6 GB.
const nodeGuard = 2

// ErrTooManyElements is the refusal of markdown that makes more elements than one write may
// (maxWriteElements). It is not ErrSchema: the markdown is well formed, and too large to store.
var ErrTooManyElements = errors.New("document too large to store")

// elementCount is what the markdown one caller write sends makes, across every parse of it. A
// write's TablePaddingBudget carries it, so every parse of that write's markdown spends it; a limit
// of zero counts nothing, as a read-back's does. It holds only the write's totals: what one parse
// tracks while it runs is that parse's parseCount, which goes with the parse.
type elementCount struct {
	// limit is the most elements the write may make, and made the elements its parses have made
	// (refusal).
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
	// at is where in the parsed source the count passed its limit, and -1 while it has not; last
	// is where the latest inline node was counted, which a mark made while a textblock's
	// delimiters pair is charged at.
	at, last int
	// parent and children are the textblock the inline count last read and its child count then.
	parent   ast.Node
	children int
}

// elementCountKey is the context key of a counting parse's parseCount.
var elementCountKey = parser.NewContextKey()

// countParse starts the count of one parse of the write's markdown in pc, or returns nil for a
// budget that counts no elements.
func (c *elementCount) countParse(pc parser.Context) *parseCount {
	if c.limit == 0 {
		return nil
	}
	count := &parseCount{write: c, at: -1}
	pc.Set(elementCountKey, count)
	return count
}

// charge counts n nodes goldmark made at offset in the parsed source and reports whether the
// count has passed the guard.
func (p *parseCount) charge(n, offset int) bool {
	if p.passed() {
		return true
	}
	p.write.nodes += n
	if p.write.nodes > nodeGuard*p.write.limit {
		p.at = offset
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

// elementWeight is what one node of goldmark's tree weighs: a table cell four, as the cell, the
// paragraph and the text the Proof tree makes of it; any other block three, as the Proof block
// and the attributes it carries in the live document; a line of text ending in a hard break four,
// its text and the break, which the Proof tree makes a node of its own (hardbreakWeight); an
// autolink two, the text and the link mark the Proof tree makes of it, where goldmark keeps no text
// node; and any other inline node one. Each weight is the memory the node costs at its worst,
// measured through the upload route, in units of an inline node's.
func elementWeight(node ast.Node) int {
	switch {
	case node.Kind() == ast.KindDocument:
		return 0
	case node.Kind() == extensionast.KindTableCell:
		return 4
	case node.Type() == ast.TypeBlock:
		return 3
	case node.Kind() == ast.KindText && node.(*ast.Text).HardLineBreak():
		return 1 + hardbreakWeight
	case node.Kind() == ast.KindAutoLink:
		return 2
	}
	return 1
}

// hardbreakWeight is what a hard line break weighs. The Proof tree makes it a node of its own
// between two texts, and the live document an element splitting its textblock's text in two, so it
// costs what a block does: weighed as one inline node, the heaviest document of hard breaks the
// limit admitted held 350 MiB stored, and four cold reads of it at once 1,190 MiB.
const hardbreakWeight = 3

// MaxDocumentElements is the most a document may weigh (Weight) after a write that makes it
// heavier: what one write's markdown may make, so no run of writes, each within the limit, grows a
// document past what one upload of it may hold. A write that leaves a document as heavy as it was,
// or lighter, is never refused by it, so a document already past it can still be trimmed or split.
const MaxDocumentElements = maxWriteElements

// Weight is what doc weighs in the elements markdown writing it would make, each node weighed as
// elementWeight weighs goldmark's: a table cell four, with the paragraph and the text it holds; any
// other block three; a hard break three; a text one, or none in a code block, whose text goldmark
// keeps as lines; any other inline node one; and each mark one where its run begins, as the inline
// syntax that opens it. Anchor marks count as any mark does. A text's line breaks are not counted,
// and goldmark's text is a line each, so a document weighs at most what its markdown makes.
func Weight(doc *Node) int {
	return proofWeight(doc, "")
}

func proofWeight(node *Node, parent string) int {
	weight := 0
	switch {
	case node.Type == "doc":
	case node.Type == "table_cell" || node.Type == "table_header":
		weight = 4
	case node.Type == "paragraph" && (parent == "table_cell" || parent == "table_header"):
	case node.Type == "hardbreak":
		weight = hardbreakWeight
	case node.Type == "text":
		if parent != "code_block" {
			weight = 1
		}
	case isInlineNodeType(node.Type):
		weight = 1
	default:
		weight = 3
	}
	// A mark run continues across the inline nodes between two texts, which carry none.
	var running []Mark
	for _, child := range node.Children {
		weight += proofWeight(child, node.Type)
		if child.Type != "text" {
			continue
		}
		for _, mark := range child.Marks {
			if !containsSameMark(running, mark) {
				weight++
			}
		}
		running = child.Marks
	}
	return weight
}

// refusal is the refusal of the parse p counts, which made root from source whose first line is
// firstLine of what the caller wrote, or nil: goldmark passed the guard, or root's elements take the
// write past its limit. root is weighed without recursion, so a tree nested as deep as the guard
// allows costs no stack. A parse that counts nothing (a nil p) is never refused.
func (p *parseCount) refusal(root ast.Node, source []byte, firstLine int) error {
	if p == nil {
		return nil
	}
	if !p.passed() && root != nil {
		for node := root; node != nil; node = nextInTree(root, node) {
			p.write.made += elementWeight(node)
			if p.write.made > p.write.limit {
				p.at = nodeOffset(node)
				break
			}
		}
	}
	if !p.passed() {
		return nil
	}
	line := firstLine + bytes.Count(source[:min(p.at, len(source))], []byte("\n"))
	return fmt.Errorf("%w: this write's markdown makes more than %d elements, passing that at line %d; a block weighs 3 elements, a table cell 4, a hard line break 3, and each piece of inline syntax, mark and line of text 1. Split the document, or upload data as a file of another content type",
		ErrTooManyElements, p.write.limit, line)
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
