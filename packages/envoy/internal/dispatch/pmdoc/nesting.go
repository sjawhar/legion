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

// maxTreeDepth is how many levels below the document a node may stand: the document is level 0 and
// each node one level below its parent. It bounds the recursive walks over browser-authored CRDT
// trees as well as schema validation and parsing, and it sits where every reader of a valid tree
// serves it with room to spare. The tightest is the document token (Node.TokenJSON): encoding/json
// refuses a value nested past 10,000 arrays and objects (from Go 1.27 it will not marshal one, and
// no version decodes one), and a node at level d is nested 2d+1 deep, an object and a content
// array per level, its marks and their attributes three deeper, and an attribute's value at most
// maxAttrNesting more: 2,104 at this bound. The next is GET /blocks, which hashes each block's
// subtree apart from the others, so its work grows with the square of the depth. Markdown, at most
// maxNesting blocks deep, makes trees about a tenth as deep.
const maxTreeDepth = 1_000

func treeDepthError(depth int) error {
	return fmt.Errorf("%w: a node %d levels deep; a document nests at most %d levels", ErrSchema, depth, maxTreeDepth)
}

// maxAttrNesting is how many arrays and objects a node's or a mark's attribute value may nest
// inside one another. The schema's own attributes are scalars or lists of them; ygo decodes an
// element's attributes about this deep at most, but a mark's from JSON as deep as encoding/json
// reads, so without this bound a mark alone could take the document token past what it encodes.
const maxAttrNesting = 100

// attrNestingError is the refusal of an attribute in attrs whose value nests past maxAttrNesting,
// or nil. attrs belong to the node or mark (kind) of type typ.
func attrNestingError(kind, typ string, attrs Attrs) error {
	for name, value := range attrs {
		if nestsPast(value, maxAttrNesting) {
			return fmt.Errorf("%w: %s %q attribute %q nests more than %d arrays and objects", ErrSchema, kind, typ, name, maxAttrNesting)
		}
	}
	return nil
}

// nestsPast reports whether value holds arrays and objects nested more than limit deep. It
// recurses at most limit+1 times.
func nestsPast(value any, limit int) bool {
	switch v := value.(type) {
	case []any:
		if limit < 1 {
			return true
		}
		for _, item := range v {
			if nestsPast(item, limit-1) {
				return true
			}
		}
	case map[string]any:
		if limit < 1 {
			return true
		}
		for _, item := range v {
			if nestsPast(item, limit-1) {
				return true
			}
		}
	case Attrs:
		return nestsPast(map[string]any(v), limit)
	case []string:
		return limit < 1
	}
	return false
}
