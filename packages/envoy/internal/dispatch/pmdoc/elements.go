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
// makes 786,432 of them and held a gigabyte (LEGION-481). At this limit a write of any shape holds
// under 130 MiB above the server's idle memory. Hand-written markdown weighs 30 to 45 elements a
// kibibyte, so a document at the 1 MiB cap weighs at most about 46,000.
const maxWriteElements = 65_536

// nodeGuard is how many times maxWriteElements goldmark may make nodes, lines and table cells,
// counted while it parses, before the parse stops. Markdown counts there at most about twice what
// it weighs - a line of text counts as its line and its text, and weighs one element - so the guard
// stops only a write at or past the limit; and goldmark's tree for this many costs at most about
// 90 MiB, so such a write is refused before goldmark allocates the rest of it.
const nodeGuard = 2

// ErrTooManyElements is the refusal of markdown that makes more elements than one write may
// (maxWriteElements). It is not ErrSchema: the markdown is well formed, and too large to store.
var ErrTooManyElements = errors.New("document too large to store")

// elementCount is what the markdown one caller write sends makes. A write's TablePaddingBudget
// carries it, so every parse of that write's markdown spends it; a limit of zero counts nothing, as
// a read-back's does.
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
	// at is where in the parsed source the count passed its limit, and -1 while it has not; last
	// is where the latest inline node was counted, which a mark made while a textblock's
	// delimiters pair is charged at.
	at, last int
	// parent and children are the textblock the inline count last read and its child count then.
	parent   ast.Node
	children int
}

func newElementCount(limit int) elementCount {
	return elementCount{limit: limit, at: -1}
}

// charge counts n nodes goldmark made at offset in the parsed source and reports whether the
// count has passed the guard.
func (c *elementCount) charge(n, offset int) bool {
	if c.passed() {
		return true
	}
	c.nodes += n
	if c.nodes > nodeGuard*c.limit {
		c.at = offset
		return true
	}
	return false
}

// passed reports whether a counting parse has passed a limit; one that counts nothing never has.
func (c *elementCount) passed() bool {
	return c.limit > 0 && c.at >= 0
}

// countedElements is the element count of the write a parse belongs to, or nil for a parse that
// counts none.
func countedElements(pc parser.Context) *elementCount {
	budget, _ := pc.Get(tablePaddingBudgetKey).(*TablePaddingBudget)
	if budget == nil || budget.elements.limit == 0 {
		return nil
	}
	return &budget.elements
}

// elementWeight is what one node of goldmark's tree weighs: a table cell four, as the cell, the
// paragraph and the text the Proof tree makes of it; any other block three, as the Proof block
// and the attributes it carries in the live document; and an inline node one. Each weight is the
// memory the node costs at its worst, measured through the upload route, in units of an inline
// node's.
func elementWeight(node ast.Node) int {
	switch {
	case node.Kind() == ast.KindDocument:
		return 0
	case node.Kind() == extensionast.KindTableCell:
		return 4
	case node.Type() == ast.TypeBlock:
		return 3
	}
	return 1
}

// refusal is the refusal of a parse of the write's markdown, which made root from source whose
// first line is firstLine of what the caller wrote, or nil: goldmark passed the guard, or root's
// elements take the write past its limit. root is weighed without recursion, so a tree nested as
// deep as the guard allows costs no stack.
func (c *elementCount) refusal(root ast.Node, source []byte, firstLine int) error {
	if c.limit == 0 {
		return nil
	}
	if !c.passed() && root != nil {
		for node := root; node != nil; node = nextInTree(root, node) {
			c.made += elementWeight(node)
			if c.made > c.limit {
				c.at = nodeOffset(node)
				break
			}
		}
	}
	if !c.passed() {
		return nil
	}
	line := firstLine + bytes.Count(source[:min(c.at, len(source))], []byte("\n"))
	return fmt.Errorf("%w: this write's markdown makes more than %d elements, passing that at line %d; a block weighs 3 elements, a table cell 4, and each piece of inline syntax, mark and line of text 1. Split the document, or upload data as a file of another content type",
		ErrTooManyElements, c.limit, line)
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
// reads its children as, so in a list the raw block opens inside the item goldmark opens next.
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
