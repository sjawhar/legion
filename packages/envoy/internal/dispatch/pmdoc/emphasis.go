package pmdoc

import (
	"reflect"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// inlineParsers is goldmark's default inline parsers with its emphasis parser, found by its type
// as blockParsers finds the block parsers it wraps, replaced by emphasis.
func inlineParsers(emphasis emphasisParser) []util.PrioritizedValue {
	parsers := parser.DefaultInlineParsers()
	emphasisType := reflect.TypeOf(parser.NewEmphasisParser())
	for index, prioritized := range parsers {
		if reflect.TypeOf(prioritized.Value) == emphasisType {
			parsers[index].Value = emphasis
		}
	}
	return parsers
}

// emphasisParser reads `*` and `_` delimiter runs as the browser editor's parser (micromark) does:
// it judges whether a run can open or close by CommonMark's flanking rules, except that a run can
// also open where another `*` or `_` follows it and close where one precedes it (micromark's
// attentionMarkers), which goldmark's ScanDelimiter does not allow. In `b_*a**` the `*` can
// therefore both open and close, so the rule of three keeps it from pairing with the `**`, and the
// engine reads the line as text where goldmark read italic `a`. It pairs the runs as the browser
// editor's parser does (emphasisDelimiters).
//
// micromark counts GFM's `~` as such a marker too, and that half is not applied: a run beside a
// `~` meets micromark's strikethrough resolver, which pairs its runs apart from and, in a link
// label or where a `~` run comes first, before the `*` and `_` runs, where goldmark pairs every
// run from one stack in order. With the `~` half alone, `[**~~**d**~~**](u)`, which both parsers
// read as strong struck `d`, reads to goldmark as two strong `~~` around `d`.
//
// flankingOnly reads by CommonMark's flanking rules alone, as goldmark does; the renderer prefers
// a spelling that reads back under both (runReadsBack).
type emphasisParser struct {
	flankingOnly bool
}

func (emphasisParser) Trigger() []byte {
	return []byte{'*', '_'}
}

func (p emphasisParser) Parse(_ ast.Node, block text.Reader, pc parser.Context) ast.Node {
	before := block.PrecendingCharacter()
	line, segment := block.PeekLine()
	char := line[0]
	length := 1
	for length < len(line) && line[length] == char {
		length++
	}
	after := ' '
	if length < len(line) {
		after = util.ToRune(line, length)
	}
	beforeIsSpace, beforeIsPunct := util.IsSpaceRune(before), util.IsPunctRune(before)
	afterIsSpace, afterIsPunct := util.IsSpaceRune(after), util.IsPunctRune(after)
	canOpen := !afterIsSpace && (!afterIsPunct || beforeIsSpace || beforeIsPunct)
	canClose := !beforeIsSpace && (!beforeIsPunct || afterIsSpace || afterIsPunct)
	if !p.flankingOnly {
		canOpen = canOpen || after == '*' || after == '_'
		canClose = canClose || before == '*' || before == '_'
	}
	if char == '_' {
		canOpen, canClose = canOpen && (beforeIsSpace || beforeIsPunct || !canClose), canClose && (afterIsSpace || afterIsPunct || !canOpen)
	}
	if !canOpen && !canClose {
		// A run that can neither open nor close pairs with nothing. goldmark's ProcessDelimiters
		// walks every delimiter still on its list when a closer looks for its opener, and never
		// takes such a run off it, so a paragraph of `a_b*a_b*…` cost time quadratic in its length
		// (LEGION-465). It is read as the text it would have become; cmark pushes no such run.
		text := ast.NewTextSegment(segment.WithStop(segment.Start + length))
		block.Advance(length)
		return text
	}
	node := parser.NewDelimiter(canOpen, canClose, length, char, emphasisDelimiters{})
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
