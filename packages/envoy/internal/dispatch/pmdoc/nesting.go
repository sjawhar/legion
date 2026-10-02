package pmdoc

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
)

// maxNesting is how many blocks a markdown document may open inside one another. Quotes, lists,
// list items, typed blocks, and footnote definitions each count one. The next block is refused
// before any deep tree is built.
const maxNesting = 100

// nestingGuard is a container's parser refusing the container it would open inside maxNesting
// blocks. withChild says the parser opens the container with a child inside it on the same line,
// as a list opens with its first item: the guard then refuses the container whose child would
// stand past the bound, rather than leave the container without that child, which goldmark's list
// parser does not expect and panics on at the next line. So a list's guard counts its first item.
// Goldmark opens a list item only directly inside a list, so every later item of that list stands
// one level inside it, where the first item did, and list items need no guard of their own.
type nestingGuard struct {
	parser.BlockParser
	withChild bool
}

// nestingRefusalKey holds the nestingError for the first block nestingGuard refused.
var nestingRefusalKey = parser.NewContextKey()

// Open asks the parser first, so only a block it opens is held to the bound: goldmark offers a line
// to every parser its first character can start - the list parser is offered a paragraph's `-x`,
// and each item of a list it opened - and most of them decline it. A refused block puts the line
// back as the parser found it, as a parser that declines leaves it, and goldmark offers the line to
// the parsers after it.
func (g nestingGuard) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, position := reader.Position()
	node, state := g.BlockParser.Open(parent, reader, pc)
	if node == nil {
		return node, state
	}
	opens := 1
	if g.withChild {
		opens = 2
	}
	if !opensPastBound(parent, opens) {
		return node, state
	}
	reader.SetPosition(line, position)
	if pc.Get(nestingRefusalKey) == nil {
		pc.Set(nestingRefusalKey, nestingError{line: line})
	}
	return nil, parser.NoChildren
}

// opensPastBound reports whether opens blocks, opened one inside another under parent, would take
// the innermost inside maxNesting blocks, counting each container from parent up to the document.
func opensPastBound(parent ast.Node, opens int) bool {
	nesting := opens
	for node := parent; node.Kind() != ast.KindDocument; node = node.Parent() {
		if nesting++; nesting > maxNesting {
			return true
		}
	}
	return false
}

// maxInlineNesting is how many inline marks a textblock's markdown may open inside one another.
// Emphasis, strong, strikethrough, links, images and code spans each count one, as goldmark nests
// them: a run of four `*` either side is two strong marks.
const maxInlineNesting = 100

// inlineNesting is the refusal of the first textblock in root whose inline markdown nests past
// maxInlineNesting, or nil. It walks the parsed tree without recursing, and withLineStarts runs it
// before any walk that recurses through inline nodes - goldmark's table transformer
// (tableCodeSpans), footnoteLabels, the conversion's parseInlineMarks - so each of those meets at
// most that many.
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
// from 0 in the source the parser read, and parseUnstamped moves it to count from 0 in what the
// caller wrote.
type nestingError struct {
	line   int
	inline bool
}

func (e nestingError) Error() string {
	if e.inline {
		return fmt.Sprintf("%v: line %d starts text nested inside more than %d inline marks; a document nests at most %d inline marks (emphasis, strong, strikethrough, links, images and code)", ErrSchema, e.line+1, maxInlineNesting, maxInlineNesting)
	}
	return fmt.Sprintf("%v: line %d opens a block inside %d blocks; %s", ErrSchema, e.line+1, maxNesting, blockBound)
}

func (nestingError) Unwrap() error { return ErrSchema }

// blockBound is what maxNesting allows, as a refusal names it.
var blockBound = fmt.Sprintf("a document nests at most %d blocks (quotes, lists and their items, typed blocks and footnote definitions)", maxNesting)

// renderedNesting is err, the parser's refusal of rendered, the markdown of doc, with a refusal of
// blocks nested past maxNesting told by how many blocks doc nests: its line counts lines of the
// rendering, which nobody wrote. A write whose own markdown nests within the bound can land deep
// enough in a document that the result nests past it.
func renderedNesting(doc *Node, err error) error {
	if nesting := (nestingError{}); errors.As(err, &nesting) && !nesting.inline {
		return fmt.Errorf("%w: the result nests %d blocks inside one another; %s", ErrSchema, blockNesting(doc), blockBound)
	}
	return err
}

// blockNesting is how many blocks node's markdown nests one inside another to write its deepest
// block, counted as maxNesting counts them: a quote, a list, a list item, a typed block and a
// footnote definition each count one. It recurses once per level of node, as rendering it does.
func blockNesting(node *Node) int {
	deepest := 0
	for _, child := range node.Children {
		deepest = max(deepest, blockNesting(child))
	}
	switch node.Type {
	case "blockquote", "bullet_list", "ordered_list", "list_item", "footnote_definition":
		deepest++
	default:
		if IsTypedBlock(node.Type) {
			deepest++
		}
	}
	return deepest
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

// MaxTreeDepth is how many levels below the document a node may stand (maxTreeDepth).
func MaxTreeDepth() int {
	return maxTreeDepth
}

// treeDepthError is the refusal of a node depth levels below the document, or nil when it stands
// within maxTreeDepth.
func treeDepthError(depth int) error {
	if depth <= maxTreeDepth {
		return nil
	}
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
