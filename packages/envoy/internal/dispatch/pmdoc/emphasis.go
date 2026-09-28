package pmdoc

import (
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// inlineParsers is goldmark's default inline parsers with its emphasis parser, the one instance
// parser.NewEmphasisParser returns, replaced by emphasisParser.
func inlineParsers() []util.PrioritizedValue {
	parsers := parser.DefaultInlineParsers()
	for index, prioritized := range parsers {
		if prioritized.Value == parser.NewEmphasisParser() {
			parsers[index] = util.Prioritized(emphasisParser{}, prioritized.Priority)
		}
	}
	return parsers
}

// emphasisParser reads `*` and `_` delimiter runs as goldmark's emphasis parser does, pairing them
// as the browser editor's parser does (emphasisDelimiters).
type emphasisParser struct{}

func (emphasisParser) Trigger() []byte {
	return []byte{'*', '_'}
}

func (emphasisParser) Parse(_ ast.Node, block text.Reader, pc parser.Context) ast.Node {
	before := block.PrecendingCharacter()
	line, segment := block.PeekLine()
	node := parser.ScanDelimiter(line, before, 1, emphasisDelimiters{})
	if node == nil {
		return nil
	}
	node.Segment = segment.WithStop(segment.Start + node.OriginalLength)
	block.Advance(node.OriginalLength)
	pc.PushDelimiter(node)
	return node
}

// emphasisDelimiters pairs an opener and a closer of one character. CommonMark's rule of three
// keeps a run that can both open and close from pairing where the two runs' lengths sum to a
// multiple of three and the closer's is not one. The browser editor's parser (micromark) judges
// the lengths the runs have left after the pairs already made from them, where goldmark judges
// the lengths they were written with (Delimiter.CalcComsumption): in `***a.****&#32;b*`, once
// the strong pair is made, the engine leaves as text the `**` the closer has left, where goldmark
// pairs one of them with the opener's last `*`. Goldmark asks this before it measures a pair, and
// the rule is the only reader of OriginalLength after a run is scanned, so CanOpenCloser gives
// both runs their remaining length as that.
type emphasisDelimiters struct{}

func (emphasisDelimiters) IsDelimiter(b byte) bool {
	return b == '*' || b == '_'
}

func (emphasisDelimiters) CanOpenCloser(opener, closer *parser.Delimiter) bool {
	if opener.Char != closer.Char {
		return false
	}
	opener.OriginalLength, closer.OriginalLength = opener.Length, closer.Length
	return true
}

func (emphasisDelimiters) OnMatch(consumes int) ast.Node {
	return ast.NewEmphasis(consumes)
}
