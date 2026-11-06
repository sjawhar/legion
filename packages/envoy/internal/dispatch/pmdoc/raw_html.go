package pmdoc

import (
	"bytes"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// opensDeclaration reports whether line, read from its own `<`, opens an inline HTML declaration
// as CommonMark 0.31 and the browser editor's engine (micromark's html-text) read one: `<!` and an
// ASCII letter. goldmark's raw HTML parser opens one only after an uppercase letter
// (parser/raw_html.go:41 at v1.8.6), so `p <!a> q` was text to the reader where the engine reads
// the html node `<!a>` between two texts, and `p <!A> q` raw HTML to both. `<!>`, `<!1>` and
// `<! a>` open none for either.
//
// This is the rule's one definition, read from a string or from bytes as its caller holds the
// line: rawHTMLCloser asks it of a line the inline reader (inlineRawHTMLParser) or the writer's
// scan (rawHTMLScan) is at a `<` of, the reader asks it for a block (declarationBlockParser), and
// the writer asks it for the HTML block the engine opens at a `<` a line begins with
// (htmlBlockStart), so the three cannot drift.
func opensDeclaration[T string | []byte](line T) bool {
	return len(line) > 2 && line[0] == '<' && line[1] == '!' && isASCIILetter(line[2])
}

// The kinds of inline raw HTML rawHTMLCloser tells apart, each ending at the first of its own
// closer, and how many of them there are (unclosedRawHTML.from, rawHTMLScan.ends).
const (
	htmlComment = iota
	htmlInstruction
	htmlDeclaration
	htmlCDATA
	htmlCloserKinds
)

// htmlNoCloser is rawHTMLCloser's answer for a tag or anything else: one the parser reading it
// ends, not a closer of its own.
const htmlNoCloser = -1

// rawHTMLCloser is which kind of construct ending at the first of its closer a line opens: a
// comment, a processing instruction, a declaration (opensDeclaration, the rule the reader opens
// one by: `<!` and an ASCII letter, as CommonMark and the engine read one), a CDATA section, or
// htmlNoCloser for a tag or nothing. The inline reader asks it of each `<` it is given, and the
// writer's scan of each it writes.
func rawHTMLCloser(line []byte) int {
	switch {
	case bytes.HasPrefix(line, []byte("<!--")):
		return htmlComment
	case bytes.HasPrefix(line, []byte("<?")):
		return htmlInstruction
	case opensDeclaration(line):
		return htmlDeclaration
	case bytes.HasPrefix(line, []byte("<![CDATA[")):
		return htmlCDATA
	}
	return htmlNoCloser
}

// inlineRawHTMLParser is the package's inline raw HTML parser: goldmark's own, wrapped and asked
// for every kind but an inline HTML declaration (opensDeclaration), which it reads itself
// (parseDeclaration) as one ast.RawHTML, the node every other inline raw HTML is read as, whose
// segments are the lines it spans, so the element counter and the weigher charge it as the raw
// HTML it is (elementWeight) and parse reads its value with segmentsText, the line endings
// between those lines kept: the engine reads `p <!a\nb> q` as one html node whose value holds the
// newline.
//
// Every reader configuration swaps it in for goldmark's own parser where that parser stands, by
// its type (inlineParsers), rather than asking a parser of its own at a `<` before it, so every
// kind of inline raw HTML a reader reads is read by this one parser; the writer's scan asks it too
// (rawHTMLParser) for every kind but an open tag, which it matches against openTagPattern.
// goldmark's own reads a declaration opening at an uppercase letter by that same walk, to
// the first `>` over as many lines of the block as it takes (parseUntil over closeDecl,
// parser/raw_html.go:101-120 at v1.8.6), so `<!A` is read as it was read before, and `<!a`, which
// that parser declines, as the engine reads it.
//
// Everything else a `<` opens is the wrapped parser's, which is asked for it: a comment (`<!--`),
// a CDATA section (`<![CDATA[`), a processing instruction, an open or closing tag, and `<!`
// before anything that is no ASCII letter.
//
// A construct of a kind that ends at a closer of its own (rawHTMLCloser) is read by walking the
// lines of its block to that closer, so one whose block holds no such closer is text, as it is to
// the engine, and the walk that learns so reaches the block's end. The block's record of where it
// holds no closer of each kind (unclosedRawHTML) refuses every later opener of that kind in that
// block without walking again, so each block is walked to its end at most once per kind, however
// many openers of it the block holds: that is the invariant linear_test.go holds the reader and
// the writer to.
type inlineRawHTMLParser struct{ parser.InlineParser }

func (p inlineRawHTMLParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	line, _ := block.PeekLine()
	kind := rawHTMLCloser(line)
	if kind == htmlNoCloser {
		// A tag is read by a pattern anchored at this `<` which reads past another only inside a
		// quoted attribute value, and everything else by nothing at all, so neither walk crosses
		// the block and neither is worth a record.
		return p.InlineParser.Parse(parent, block, pc)
	}
	_, segment := block.Position()
	record := unclosedRawHTMLOf(pc, parent)
	if record.holdsNoCloser(kind, segment.Start) {
		return nil
	}
	var html ast.Node
	if kind == htmlDeclaration {
		html = parseDeclaration(block)
	} else {
		html = p.InlineParser.Parse(parent, block, pc)
	}
	if html == nil {
		// Both walks, this parser's over a declaration and the wrapped parser's over the three
		// kinds it is asked for, take each line of the block whole from this opener until one
		// holds the kind's closer, so either gives up only where the block holds no such closer
		// from this offset on, and this is the first offset the block holds none of this kind
		// from: every later opener of the kind is refused above without walking again.
		record.noCloserFrom(kind, segment.Start)
	}
	return html
}

// parseDeclaration reads the declaration the block reader is at, which goldmark's parser reads
// only after an uppercase letter, by the shape of its parseUntil, which is unexported there: each
// line is taken whole until one holds the `>` the declaration ends at, which the node's last
// segment stops after. Where no line of the block holds it the reader is put back where it stood
// and nothing is read.
func parseDeclaration(block text.Reader) ast.Node {
	savedLine, savedSegment := block.Position()
	node := ast.NewRawHTML()
	for {
		line, segment := block.PeekLine()
		if line == nil {
			break
		}
		if index := bytes.IndexByte(line, '>'); index >= 0 {
			node.Segments.Append(segment.WithStop(segment.Start + index + 1))
			block.Advance(index + 1)
			return node
		}
		node.Segments.Append(segment)
		block.AdvanceLine()
	}
	block.SetPosition(savedLine, savedSegment)
	return nil
}

// unclosedRawHTMLKey holds, while a block's inline markdown is read, that block's record of where
// it holds no closer of each kind (unclosedRawHTML).
var unclosedRawHTMLKey = parser.NewContextKey()

// htmlUnclosedUnwalked is what a kind's entry in unclosedRawHTML.from holds before any walk of
// that kind reached the block's end, no offset being known to hold no closer of it. What is
// recorded there is the offset of an opener, which in the first block of a document can be 0, so
// the empty state cannot be 0 as the writer's own record spells its own (htmlEndUnasked).
const htmlUnclosedUnwalked = -1

// unclosedRawHTML is what the inline raw HTML parser knows of the block it is reading. It is kept
// in the parse's context, where the bare-address reader keeps what it knows of a block
// (linkifyBlock) and of a line (literalDomains), and starts afresh at the first opener of the
// next block, since a closer in another block closes nothing in this one.
type unclosedRawHTML struct {
	parent ast.Node
	// from is, for each kind rawHTMLCloser tells apart - a comment, closing at `-->`, a
	// processing instruction, at `?>`, a declaration, at `>`, and a CDATA section, at `]]>` - the
	// source offset of the first opener of that kind whose walk reached the block's end with no
	// closer of its own on the way, so the block holds none at or after it, and
	// htmlUnclosedUnwalked before any such walk. One entry per kind, since a `-->` the block does
	// hold says nothing of where its `?>` is.
	from [htmlCloserKinds]int
}

// unclosedRawHTMLOf is the parse's record of parent, the block whose inline markdown it is being
// asked about, made at its first opener of a kind that ends at a closer of its own. A parse keeps
// one; the writer's scan reads the lines of one run as one block and keeps its own record of what
// it asked of them (rawHTMLScan.ends), so it gives the parser no context and is given no record.
func unclosedRawHTMLOf(pc parser.Context, parent ast.Node) *unclosedRawHTML {
	if pc == nil {
		return nil
	}
	record, _ := pc.Get(unclosedRawHTMLKey).(*unclosedRawHTML)
	if record == nil || record.parent != parent {
		record = &unclosedRawHTML{parent: parent}
		for kind := range record.from {
			record.from[kind] = htmlUnclosedUnwalked
		}
		pc.Set(unclosedRawHTMLKey, record)
	}
	return record
}

// holdsNoCloser reports whether the block is known to hold no closer of kind at or after at, the
// offset of an opener of it; a caller the parser keeps no record for knows of none.
func (u *unclosedRawHTML) holdsNoCloser(kind, at int) bool {
	if u == nil {
		return false
	}
	from := u.from[kind]
	return from != htmlUnclosedUnwalked && at >= from
}

// noCloserFrom records that the block holds no closer of kind at or after at.
func (u *unclosedRawHTML) noCloserFrom(kind, at int) {
	if u == nil {
		return
	}
	u.from[kind] = at
}

// declarationBlockParser is goldmark's HTML block parser opening its type 4, a declaration block,
// wherever CommonMark 0.31 and the browser editor's engine (micromark) open one: `<!` and an ASCII
// letter, after up to three spaces of indentation. goldmark opens that type only after an
// uppercase letter (parser/html_block.go:88 at v1.8.6), so a line holding a lone `<!a>` was a
// paragraph of inline raw HTML to the reader, stored unescaped, where the engine reads an HTML
// block, a reading the Proof schema has no node for. Opening the block here has such a document
// refused on the write path (ErrBlockHTML), which is already what a line holding a lone `<b>`
// does: the two readers then agree instead of disagreeing silently.
//
// It asks opensDeclaration, the one definition of `<!` and an ASCII letter, which the inline
// reader (inlineRawHTMLParser) and the writer ask too, so block and inline cannot drift.
//
// The real parser answers first, so every line it already opened - `<!DOCTYPE html>`, `<!A>`, a
// comment, a CDATA section, a processing instruction, a tag - opens exactly the block it opened
// before, and only a line goldmark declines is looked at again. Everything else is the embedded
// parser's: Trigger, Continue and Close read a type 4 block from the node alone
// (parser/html_block.go:157-218), closing it at the first `>` however many lines that takes, and
// CanInterruptParagraph stays true, as CommonMark has a type 4 block interrupt a paragraph.
type declarationBlockParser struct{ parser.BlockParser }

func (p declarationBlockParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	if node, state := p.BlockParser.Open(parent, reader, pc); node != nil {
		return node, state
	}
	line, segment := reader.PeekLine()
	// goldmark's own opening patterns allow three spaces of indentation and no tab
	// (parser/html_block.go:79-96), a fourth space being indented code; this reads the same
	// indentation before asking the rule.
	indent := 0
	for indent < 3 && indent < len(line) && line[indent] == ' ' {
		indent++
	}
	if !opensDeclaration(line[indent:]) {
		return nil, parser.NoChildren
	}
	node := ast.NewHTMLBlock(ast.HTMLBlockType4)
	reader.AdvanceToEOL()
	node.Lines().Append(segment)
	return node, parser.NoChildren
}
