package pmdoc

import (
	"reflect"

	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// inlineParsers is goldmark's default inline parsers, each found by its type as blockParsers finds
// the block parsers it wraps: its emphasis parser replaced by emphasisParser, its link parser
// measuring the images it makes and pairing their labels' runs (imageNesting), and its raw HTML
// parser wrapped by the package's own (inlineRawHTMLParser), where that parser stands (400), so
// every kind of inline raw HTML is read by one parser and every declaration by the one rule
// (opensDeclaration), which opens one at any ASCII letter where goldmark's parser opens one only
// after an uppercase letter. tildeParser is added at the priority goldmark's strikethrough
// extension gives its own (500), and every delimiter run is read by rules.
func inlineParsers(rules delimiterRules) []util.PrioritizedValue {
	parsers := parser.DefaultInlineParsers()
	emphasisType := reflect.TypeOf(parser.NewEmphasisParser())
	linkType := reflect.TypeOf(parser.NewLinkParser())
	rawHTMLType := reflect.TypeOf(parser.NewRawHTMLParser())
	for index, prioritized := range parsers {
		switch reflect.TypeOf(prioritized.Value) {
		case emphasisType:
			parsers[index].Value = emphasisParser{rules: rules}
		case linkType:
			parsers[index].Value = imageNesting{prioritized.Value.(goldmarkLinkParser)}
		case rawHTMLType:
			parsers[index].Value = inlineRawHTMLParser{prioritized.Value.(parser.InlineParser)}
		}
	}
	return append(parsers, util.Prioritized(tildeParser{rules: rules}, 500))
}

// delimiterRules is which rules a reader reads `*`, `_` and `~` delimiter runs by.
type delimiterRules uint8

const (
	// editorRules reads them as the browser editor's parser (micromark) does: a `*` or `_` run can
	// also open or close beside a `*`, `_` or `~` (emphasisParser), a `~` run pairs only with one of
	// its length and is refused after a `~` no backslash escapes (tildeParser), and the `~` runs
	// pair apart from the others, in micromark's order (delimiterList). Every document is read by
	// these rules.
	editorRules delimiterRules = iota
	// storedRules reads them as Parse did before it read as the browser editor does: a `*` or `_`
	// run can also open or close beside a `*` or `_` but not a `~`, a `~` run is read by goldmark's
	// strikethrough, which pairs runs of either length and refuses one after any `~`, and every run
	// is paired in goldmark's one loop over them in order.
	storedRules
	// flankingRules reads them as goldmark does, and as Parse did before it read a `*` or `_` run
	// beside another as the browser editor does (LEGION-290): as storedRules, except that a `*` or
	// `_` run is judged by CommonMark's flanking rules alone.
	flankingRules
)

// emphasisParser reads `*` and `_` delimiter runs as the browser editor's parser (micromark) does:
// it judges whether a run can open or close by CommonMark's flanking rules, except that a run can
// also open where another `*`, `_` or `~` follows it and close where one precedes it (micromark's
// attentionMarkers, which GFM's strikethrough extends with `~`), which goldmark's ScanDelimiter
// does not allow. In `b_*a**` the `*` can therefore both open and close, so the rule of three keeps
// it from pairing with the `**`, and the engine reads the line as text where goldmark read italic
// `a`. It pairs the runs as the browser editor's parser does (emphasisDelimiters), apart from the
// `~` runs and in micromark's order (delimiterList).
//
// Counting `~` as a marker changes only whether a `*` or `_` run beside it can open or close, never
// what it pairs with: the `~` runs pair in a pass of their own, apart from the `*` and `_` runs, as
// micromark's strikethrough pairs them apart from attention (delimiterList), so a run that opens
// toward a `~` never takes a `~` run as its partner. In `*~~**b~~*` the outer `*` runs open and
// close toward the `~~` and pair around the struck `**b`, as the engine reads it.
//
// That is its editorRules. Under storedRules a run opens or closes beside a `*` or `_` alone, and
// under flankingRules by the flanking rules alone; under both every run is paired in goldmark's one
// loop over them in order (delimiterList.advance). The renderer keeps first a spelling that reads
// back under all three (inlineWithEscapes), so the bytes it wrote while Dispatch read by the stored
// rules stay where they still read back.
//
// storedRules and flankingRules differ only in that widening by a `*` or `_` beside a run: the `~`
// runs, their pairing and every other parser read both alike. So a parse under storedRules or
// editorRules in which every `*` and `_` run can open and close exactly as it could under
// flankingRules is, step for step, the parse flankingRules makes of the same source. Under either
// of those rules the parser sets flankingDivergenceKey in its parse's context at any run whose
// pair under storedRules (no `~` widening, even under editorRules) differs from its pair under
// flankingRules, so the renderer reads a run back under flankingMarkdown only where that read can
// differ (parseInlineWithDefinitions).
type emphasisParser struct {
	rules delimiterRules
}

// flankingDivergenceKey is set, in the context of a parse under editorRules or storedRules, once a
// `*` or `_` run could open or close otherwise under flankingRules than under storedRules
// (emphasisParser).
var flankingDivergenceKey = parser.NewContextKey()

func (emphasisParser) Trigger() []byte {
	return []byte{'*', '_'}
}

func (p emphasisParser) Parse(_ ast.Node, block text.Reader, pc parser.Context) ast.Node {
	// micromark tokenizes every `*` and `_` run as attention, one that pairs with nothing too, and
	// the first run tokenized decides which group pairs first (delimiterList.tokenized).
	delimiters := delimitersOf(pc)
	delimiters.tokenized(emphasisRuns)
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
	beforeIsNeutral, afterIsNeutral := beforeIsSpace || beforeIsPunct, afterIsSpace || afterIsPunct
	canOpen := !afterIsSpace && (!afterIsPunct || beforeIsNeutral)
	canClose := !beforeIsSpace && (!beforeIsPunct || afterIsNeutral)
	if p.rules != flankingRules {
		flankingOpen, flankingClose := underscoreRule(char, canOpen, canClose, beforeIsNeutral, afterIsNeutral)
		canOpen = canOpen || after == '*' || after == '_'
		canClose = canClose || before == '*' || before == '_'
		if storedOpen, storedClose := underscoreRule(char, canOpen, canClose, beforeIsNeutral, afterIsNeutral); storedOpen != flankingOpen || storedClose != flankingClose {
			pc.Set(flankingDivergenceKey, true)
		}
	}
	if p.rules == editorRules {
		canOpen = canOpen || after == '~'
		canClose = canClose || before == '~'
	}
	canOpen, canClose = underscoreRule(char, canOpen, canClose, beforeIsNeutral, afterIsNeutral)
	if !canOpen && !canClose {
		// A run that can neither open nor close pairs with nothing. goldmark's ProcessDelimiters
		// walked every delimiter still on its list when a closer looked for its opener and never
		// took such a run off it, so a paragraph of `a_b*a_b*…` cost time quadratic in its length
		// (LEGION-465). It is read as the text it would have become, so it costs the delimiter list
		// nothing; cmark pushes no such run.
		text := ast.NewTextSegment(segment.WithStop(segment.Start + length))
		block.Advance(length)
		return text
	}
	node := parser.NewDelimiter(canOpen, canClose, length, char, emphasisDelimiters{count: countedElements(pc)})
	node.Segment = segment.WithStop(segment.Start + node.OriginalLength)
	block.Advance(node.OriginalLength)
	delimiters.push(node, p.rules)
	return node
}

// underscoreRule is whether a run of char that its flanking leaves able to open and close as
// canOpen and canClose can: an `_` run opens only where whitespace or punctuation precedes it or it
// cannot close, and closes only where one follows it or it cannot open, so `_` inside a word pairs
// with nothing; a `*` run as its flanking leaves it.
func underscoreRule(char byte, canOpen, canClose, beforeIsNeutral, afterIsNeutral bool) (bool, bool) {
	if char != '_' {
		return canOpen, canClose
	}
	return canOpen && (beforeIsNeutral || !canClose), canClose && (afterIsNeutral || !canOpen)
}

// CloseBlock pairs the runs a textblock leaves once its inline markdown is all read, where
// goldmark's parser pairs its own list (parser/parser.go:1275-1278 at v1.8.6), and drops the list
// so the next textblock starts its own. Goldmark calls every inline parser's CloseBlock as each
// block closes; the link parser's, which turns the labels a block leaves open into text, may run
// before or after it, and neither changes how a run pairs.
func (emphasisParser) CloseBlock(_ ast.Node, _ text.Reader, pc parser.Context) {
	if delimiters, _ := pc.Get(delimitersKey).(*delimiterList); delimiters != nil {
		delimiters.pairTextblock()
		pc.Set(delimitersKey, nil)
	}
}

// tildeParser reads a `~` delimiter run as the browser editor's strikethrough
// (micromark-extension-gfm-strikethrough) does: a run of three or more is text, and so is a run
// directly after a `~`, unless a backslash escapes that `~` (escapedTilde), since micromark refuses
// a run after a `~` only where the event before is no character escape. Its runs pair only with a
// run of their own length (tildeDelimiters).
//
// Under storedRules and flankingRules it reads a run as goldmark's strikethrough does: a run
// directly after any `~`, escaped or not, is text, and runs of either length pair.
//
// Goldmark scans a run after a `~` before it refuses it, so a run of n tildes cost n scans of the
// rest of it; returning before the scan keeps that result and makes the run linear (LEGION-465). A
// run after an escaped `~` is scanned once, and the run after it is refused again before its scan.
type tildeParser struct{ rules delimiterRules }

func (tildeParser) Trigger() []byte {
	return []byte{'~'}
}

func (p tildeParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	before := block.PrecendingCharacter()
	line, segment := block.PeekLine()
	if before == '~' && (p.rules != editorRules || !escapedTilde(parent, block.Source(), segment.Start-1)) {
		return nil
	}
	node := parser.ScanDelimiter(line, before, 1, tildeDelimiters{rules: p.rules})
	if node == nil || node.OriginalLength > 2 {
		return nil
	}
	node.Segment = segment.WithStop(segment.Start + node.OriginalLength)
	block.Advance(node.OriginalLength)
	delimitersOf(pc).push(node, p.rules)
	return node
}

// escapedTilde reports whether a backslash escapes the `~` at index of source: an odd run of
// backslashes stands before it (escapedByBackslashes) in the text goldmark's inline loop read up to
// it. The loop appends the text it read since the last inline node as parent's last child before it
// offers a parser the next character (parser/parser.go:1218-1221 at v1.8.6), so a backslash an
// inline node took is not counted: in `www.a.com/~x\~~~`, the `\` ends the bare URL, and micromark,
// which reads it as the URL's, refuses the runs after the `~` it does not escape.
func escapedTilde(parent ast.Node, source []byte, index int) bool {
	read, ok := parent.LastChild().(*ast.Text)
	if !ok || read.Segment.Stop != index+1 || read.Segment.Start > index {
		return false
	}
	return escapedByBackslashes(source[read.Segment.Start:index+1], index-read.Segment.Start)
}

// tildeDelimiters pairs an opener and a closer of `~`: under editorRules only runs of one length,
// which a pair then uses up whole, and under storedRules and flankingRules, as goldmark's
// strikethrough, any two. A pair is a strikethrough mark, which charges the parse's element count
// nothing, as goldmark's strikethrough charged none.
type tildeDelimiters struct{ rules delimiterRules }

func (tildeDelimiters) IsDelimiter(b byte) bool {
	return b == '~'
}

func (t tildeDelimiters) CanOpenCloser(opener, closer *parser.Delimiter) bool {
	return opener.Char == closer.Char && (t.rules != editorRules || opener.Length == closer.Length)
}

func (tildeDelimiters) OnMatch(int) ast.Node {
	return extensionast.NewStrikethrough()
}

// emphasisDelimiters pairs an opener and a closer of one character. CommonMark's rule of three
// keeps a run that can both open and close from pairing where the two runs' lengths sum to a
// multiple of three and the closer's is not one. The browser editor's parser (micromark) judges
// the lengths the runs have left after the pairs already made from them, where goldmark judges
// the lengths they were written with (Delimiter.CalcComsumption): in `***a.****&#32;b*`, once
// the strong pair is made, the engine leaves as text the `**` the closer has left, where goldmark
// pairs one of them with the opener's last `*`. The pairing asks this before it measures a pair, as
// goldmark's does (delimiterList.advance), and the rule is the only reader of OriginalLength after
// a run is scanned, so CanOpenCloser gives both runs their remaining length as that. Each pair it
// makes is a mark, which count, the parse's share of its write's element count when the parse
// counts them, is charged for. Pairing runs once a textblock's runs are all read, so the pairs it
// makes after the guard has passed are only those of the runs pushed before it, at most the
// guard's count: refusing a mebibyte line of `a_b*` or `*a*` allocates about 30 MiB.
type emphasisDelimiters struct{ count *parseCount }

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

func (d emphasisDelimiters) OnMatch(consumes int) ast.Node {
	if d.count != nil {
		d.count.charge(1, d.count.last)
	}
	return ast.NewEmphasis(consumes)
}
