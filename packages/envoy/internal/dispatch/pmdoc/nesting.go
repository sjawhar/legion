package pmdoc

import (
	"bytes"
	"fmt"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
)

// maxNesting is how many blocks a markdown document may open inside one another. Quotes, lists,
// list items, typed blocks, and footnote definitions each count one. The next block is refused
// before any deep tree is built.
const maxNesting = 100

type nestingGuard struct{ parser.BlockParser }

// nestingRefusalKey holds the nestingError for the first block nestingGuard refused.
var nestingRefusalKey = parser.NewContextKey()

func (g nestingGuard) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	if len(pc.OpenedBlocks()) >= maxNesting {
		if pc.Get(nestingRefusalKey) == nil {
			line, _ := reader.Position()
			pc.Set(nestingRefusalKey, nestingError{line: line})
		}
		return nil, parser.NoChildren
	}
	return g.BlockParser.Open(parent, reader, pc)
}

// maxInlineNesting is how many inline marks a textblock's markdown may open inside one another.
// Emphasis, strong, strikethrough, links, images and code spans each count one, as goldmark nests
// them: a run of four `*` either side is two strong marks.
const maxInlineNesting = 100

// inlineNesting is the refusal of the first textblock in root whose inline markdown nests past
// maxInlineNesting, or nil. It walks the parsed tree without recursing, so it runs before any walk
// that recurses through inline nodes - footnoteLabels, the conversion's parseInlineMarks - and each
// of those meets at most that many.
func inlineNesting(root ast.Node, source []byte) error {
	depth := 0 // how many inline nodes currently enclose node
	for node := root; node != nil; {
		if child := node.FirstChild(); child != nil {
			if node.Type() == ast.TypeInline {
				if depth++; depth > maxInlineNesting {
					return nestingError{line: textblockLine(node, source), inline: true}
				}
			}
			node = child
			continue
		}
		for node != root && node.NextSibling() == nil {
			node = node.Parent()
			if node.Type() == ast.TypeInline {
				depth--
			}
		}
		if node == root {
			return nil
		}
		node = node.NextSibling()
	}
	return nil
}

// textblockLine is the line, counted from 0 in source, on which the block holding inline node
// starts.
func textblockLine(node ast.Node, source []byte) int {
	for ; node != nil; node = node.Parent() {
		if node.Type() == ast.TypeBlock && node.Lines().Len() > 0 {
			return bytes.Count(source[:node.Lines().At(0).Start], []byte("\n"))
		}
	}
	return 0
}

// nestingError is markdown nested past a bound: a block opened inside maxNesting blocks
// (nestingGuard), or a textblock's inline marks past maxInlineNesting (inlineNesting). line counts
// from 0 in the source the parser read.
type nestingError struct {
	line   int
	inline bool
}

func (e nestingError) Error() string { return e.at(1).Error() }

func (nestingError) Unwrap() error { return ErrSchema }

// at is the refusal naming its line in what the caller wrote, whose first line is firstLine.
func (e nestingError) at(firstLine int) error {
	if e.inline {
		return fmt.Errorf("%w: line %d starts text nested inside more than %d inline marks; a document nests at most %d inline marks (emphasis, strong, strikethrough, links, images and code)", ErrSchema, firstLine+e.line, maxInlineNesting, maxInlineNesting)
	}
	return fmt.Errorf("%w: line %d opens a block inside %d blocks; a document nests at most %d blocks (quotes, lists and their items, typed blocks and footnote definitions)", ErrSchema, firstLine+e.line, maxNesting, maxNesting)
}

// maxTreeDepth is how many ancestors a node may have, with the document root excluded. It bounds
// the recursive walks over browser-authored CRDT trees as well as schema validation and parsing.
const maxTreeDepth = 10_000

func treeDepthError(depth int) error {
	return fmt.Errorf("%w: a node %d levels deep; a document nests at most %d levels", ErrSchema, depth, maxTreeDepth)
}
