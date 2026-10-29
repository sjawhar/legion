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
// This is the rule's one definition: the reader (declarationParser) and the writer
// (rawHTMLCloser's declaration) both ask it, so the two cannot drift.
func opensDeclaration(line []byte) bool {
	return len(line) > 2 && line[0] == '<' && line[1] == '!' && isASCIILetter(line[2])
}

// declarationClose is where a declaration ends in line, one line of its block from the
// declaration's `<` on: after the first `>`, and -1 where the line holds none, since a declaration
// ends at the first `>` after it however many lines of the block that takes. The reader
// (declarationParser) and the writer (rawHTMLScan.declarationEnd) both ask it, so one closer
// answers for both.
func declarationClose(line []byte) int {
	if index := bytes.IndexByte(line, '>'); index >= 0 {
		return index + 1
	}
	return -1
}

// declarationParser reads an inline HTML declaration (opensDeclaration) as one ast.RawHTML, the
// node every other inline HTML is read as, whose segments are the lines it spans, so the element
// counter and the weigher charge it as the raw HTML it is (elementWeight) and parse reads its
// value with segmentsText, the line endings between those lines kept: the engine reads
// `p <!a\nb> q` as one html node whose value holds the newline.
//
// Every reader configuration asks it before goldmark's own raw HTML parser (inlineParsers:
// goldmark registers that parser at 400 and its autolink parser at 300), so a declaration opening
// at an uppercase letter, which goldmark reads itself, goes through this parser too and both cases
// are read by the one rule.
//
// It refuses everything else a `<` opens, leaving each to the parsers after it: a comment
// (`<!--`), a CDATA section (`<![CDATA[`), a processing instruction, an open or closing tag, and
// `<!` before anything that is no ASCII letter. It refuses a declaration no `>` closes before the
// end of its block as well, which is text to the engine too.
type declarationParser struct{}

func (declarationParser) Trigger() []byte {
	return []byte{'<'}
}

func (declarationParser) Parse(_ ast.Node, block text.Reader, _ parser.Context) ast.Node {
	line, _ := block.PeekLine()
	if !opensDeclaration(line) {
		return nil
	}
	// This is the shape of goldmark's parseUntil (parser/raw_html.go:101-120 at v1.8.6), which is
	// unexported there: each line is taken whole until one holds the closer, which the node's last
	// segment stops after, and where no line of the block holds it the reader is put back where it
	// stood and nothing is read.
	savedLine, savedSegment := block.Position()
	node := ast.NewRawHTML()
	for {
		line, segment := block.PeekLine()
		if line == nil {
			break
		}
		if end := declarationClose(line); end >= 0 {
			node.Segments.Append(segment.WithStop(segment.Start + end))
			block.Advance(end)
			return node
		}
		node.Segments.Append(segment)
		block.AdvanceLine()
	}
	block.SetPosition(savedLine, savedSegment)
	return nil
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
