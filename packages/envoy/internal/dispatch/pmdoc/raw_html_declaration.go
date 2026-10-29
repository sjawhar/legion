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
// line: the reader asks it inline (declarationParser) and for a block (declarationBlockParser),
// and the writer asks it for a declaration's closer (rawHTMLCloser) and for the HTML block the
// engine opens at a `<` a line begins with (htmlBlockStart), so the four cannot drift.
func opensDeclaration[T string | []byte](line T) bool {
	return len(line) > 2 && line[0] == '<' && line[1] == '!' && isASCIILetter(line[2])
}

// declarationParser is goldmark's inline raw HTML parser reading an inline HTML declaration
// (opensDeclaration) itself, as one ast.RawHTML, the node every other inline HTML is read as,
// whose segments are the lines it spans, so the element counter and the weigher charge it as the
// raw HTML it is (elementWeight) and parse reads its value with segmentsText, the line endings
// between those lines kept: the engine reads `p <!a\nb> q` as one html node whose value holds the
// newline.
//
// Every reader configuration swaps it in for goldmark's own parser where that parser stands, by
// its type (inlineParsers), rather than asking a parser of its own at a `<` before it, and the
// writer's scan asks it too (rawHTMLParser): one parser reads every kind of inline raw HTML, so a
// declaration is walked once where it was walked three times. goldmark's parser reads one opening
// at an uppercase letter with the same walk, to the first `>` over as many lines of the block as
// it takes (parseUntil over closeDecl, parser/raw_html.go:101-120 at v1.8.6), so `<!A` is read as
// it was read before, and `<!a`, which that parser declines, as the engine reads it.
//
// Everything else a `<` opens is the wrapped parser's, which is asked for it: a comment (`<!--`),
// a CDATA section (`<![CDATA[`), a processing instruction, an open or closing tag, and `<!`
// before anything that is no ASCII letter.
//
// A declaration no `>` closes before the end of its block is text, as it is to the engine, and
// the block's record of where no `>` remains (declarationBlock) refuses every later opener in
// that block without walking again. Without the record a block of `<!a` repeated walked to its
// end from each of them: on one machine a 1 MiB paragraph of them took 9.97 s to Parse and
// 19.38 s to ParseForWrite, and `<!A`, which goldmark's own parser at 400 walked again after
// this one gave up, 19.24 s and 37.02 s; the multi-line form `x <!a` on each line grew about
// fourfold per doubling, 6.88 s at 64 KiB and 27.60 s at 128 KiB. With the record and the one
// parser the same machine takes 0.21 s and 0.67 s, 0.20 s and 0.51 s, and 0.05 s at 64 KiB and
// 0.13 s at 128 KiB, and linear_test.go holds the growth.
//
// The record keeps one such offset for each kind that ends at a closer of its own, since the
// three the wrapped parser reads - a comment, closing at `-->`, a processing instruction, at
// `?>`, and a CDATA section, at `]]>` - are walked to the block's end from each opener just as a
// declaration was, which is how goldmark has read them since it had a raw HTML parser. Their
// walk is the wrapped parser's, so this one records what that walk answered: the wrapped parser
// declines a construct of these kinds only where the block holds no closer of it from the opener
// on. On one machine a 1 MiB paragraph of `p ` and `<!--` repeated took 24.97 s to Parse and
// 50.17 s to ParseForWrite at this pull request's fork point, `<?` 46.31 s and 93.65 s, and
// `<![CDATA[`, whose parse the element limit refuses past, 1.61 s and 1.66 s; with the record
// they take 0.23 s and 0.55 s, 0.22 s and 0.62 s, and 0.15 s and 0.11 s.
type declarationParser struct{ parser.InlineParser }

func (p declarationParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	line, _ := block.PeekLine()
	kind := rawHTMLCloser(line)
	if kind == htmlNoCloser {
		// A tag is read by a pattern anchored at this `<` which reads past another only inside a
		// quoted attribute value, and everything else by nothing at all, so neither walk crosses
		// the block and neither is worth a record.
		return p.InlineParser.Parse(parent, block, pc)
	}
	savedLine, savedSegment := block.Position()
	state := declarationBlockOf(pc, parent)
	if unclosed := state.unclosed[kind]; unclosed != htmlUnclosedUnwalked && savedSegment.Start >= unclosed {
		return nil
	}
	if kind != htmlDeclaration {
		// The walk is the wrapped parser's, which takes each line of the block whole until one
		// holds this kind's closer at or after this opener, so it declines only where the block
		// holds no such closer from here on, and this offset is the first the block holds none
		// from: every later opener of the kind is refused above without walking again.
		html := p.InlineParser.Parse(parent, block, pc)
		if html == nil {
			state.unclosed[kind] = savedSegment.Start
		}
		return html
	}
	// This is the shape of goldmark's parseUntil, which is unexported there: each line is taken
	// whole until one holds the closer, which the node's last segment stops after. Where no line
	// of the block holds it the reader is put back where it stood, nothing is read, and the block
	// is recorded as holding no `>` from this opener on, which is why that walk runs at most once
	// per block.
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
	state.unclosed[kind] = savedSegment.Start
	return nil
}

// declarationBlockKey holds, while a block's inline markdown is read, that block's record of
// where it holds no closer of each kind (declarationBlock).
var declarationBlockKey = parser.NewContextKey()

// htmlUnclosedUnwalked is what a kind's entry in declarationBlock.unclosed holds before any walk
// of that kind reached the block's end, no offset being known to hold no closer of it. The
// writer's own record spells its two such states htmlEndUnasked and htmlEndNone, 0 and -1, since
// the end of a construct is never 0; what is recorded here is the offset of an opener, which in
// the first block of a document is.
const htmlUnclosedUnwalked = -1

// declarationBlock is what the inline raw HTML reader knows of the block it is reading. It is
// kept in the parse's context, where the bare-address reader keeps what it knows of a block
// (linkifyBlock) and of a line (literalDomains), and starts afresh at the first opener of the
// next block, since a closer in another block closes nothing in this one.
type declarationBlock struct {
	parent ast.Node
	// unclosed is, for each kind rawHTMLCloser tells apart - a comment, closing at `-->`, a
	// processing instruction, at `?>`, a declaration, at `>`, and a CDATA section, at `]]>` - the
	// source offset of the first opener of that kind whose walk reached the block's end with no
	// closer of its own on the way, so the block holds none at or after it, and
	// htmlUnclosedUnwalked before any such walk. It is the reader's mirror of the writer's own
	// record of one end per kind (rawHTMLScan.ends), and one entry per kind for the same reason:
	// a `-->` the block does hold says nothing of where its `?>` is.
	unclosed [htmlCloserKinds]int
}

// declarationBlockOf is the reader's record of parent, the block whose inline markdown it is
// reading. The writer's scan reads the lines of one run as one block and asks the parser with a
// context of its own for them (rawHTMLScan.readLines), parent being nil there.
func declarationBlockOf(pc parser.Context, parent ast.Node) *declarationBlock {
	state, _ := pc.Get(declarationBlockKey).(*declarationBlock)
	if state == nil || state.parent != parent {
		state = &declarationBlock{parent: parent}
		for kind := range state.unclosed {
			state.unclosed[kind] = htmlUnclosedUnwalked
		}
		pc.Set(declarationBlockKey, state)
	}
	return state
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
// reader (declarationParser) and the writer ask too, so block and inline cannot drift.
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
