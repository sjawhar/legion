package pmdoc

import (
	"fmt"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
)

// MaxNesting is how many blocks a markdown document may open inside one another. Quotes, lists,
// list items, typed blocks, and footnote definitions each count one. The next block is refused
// before any deep tree is built.
const MaxNesting = 100

type nestingGuard struct{ parser.BlockParser }

var nestingRefusalKey = parser.NewContextKey()

func (g nestingGuard) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	if len(pc.OpenedBlocks()) >= MaxNesting {
		if pc.Get(nestingRefusalKey) == nil {
			line, _ := reader.Position()
			pc.Set(nestingRefusalKey, line)
		}
		return nil, parser.NoChildren
	}
	return g.BlockParser.Open(parent, reader, pc)
}

type nestingError struct{ line int }

func (e nestingError) Error() string {
	return fmt.Sprintf("markdown nesting refused at line %d", e.line+1)
}

func (e nestingError) Is(target error) bool { return target == ErrSchema }

func nestingRefusal(pc parser.Context) (line int, refused bool) {
	line, refused = pc.Get(nestingRefusalKey).(int)
	return line, refused
}

// maxTreeDepth is how many ancestors a node may have, with the document root excluded. It bounds
// the recursive walks over browser-authored CRDT trees as well as schema validation and parsing.
const maxTreeDepth = 10_000

func treeDepthError(depth int) error {
	return fmt.Errorf("%w: a node %d levels deep; a document nests at most %d levels", ErrSchema, depth, maxTreeDepth)
}
