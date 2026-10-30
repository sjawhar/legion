package pmdoc

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// htmlFlowLinesKey holds, while a document parses, where each line starts that goldmark gave to a
// paragraph and the browser editor's parser reads as opening a block of HTML (htmlFlowKind), and
// htmlFlowLinesAttr holds the same on the parsed document root afterwards (parseSource). Both
// parsers read the seven kinds of block CommonMark names, but that parser's grammar is micromark's
// own tokenizer where goldmark's is a handful of patterns (parser/html_block.go), and the two
// disagree at the edges: micromark takes a tab where those patterns take only a space, in the
// line's indentation, between a tag's parts and after the tag, and it reads a declaration opening
// with a lower-case letter, and a closing tag of a raw text name, as a block as well. So
// `> </a\t>` and `> a\n> \t<div>` open a block of HTML in the browser and are the text of a
// paragraph to goldmark. The Proof schema holds no block of HTML, and the browser editor drops the
// block it reads together with the container around it, so such a document cannot read back:
// blockRefusal refuses the paragraph, text block or setext heading holding such a line
// (htmlFlowRefusal). Where goldmark's own patterns open a block of HTML it refuses that block
// already (ErrBlockHTML) and the line belongs to no paragraph, so nothing is recorded for it;
// the lines recorded here are the ones those patterns miss.
var (
	htmlFlowLinesKey  = parser.NewContextKey()
	htmlFlowLinesAttr = []byte("pmdoc-html-flow-lines")
)

// recordHTMLFlowLine records a paragraph's line, which starts at start in the source and whose
// kind the browser editor's parser reads it as opening a block of HTML of (htmlFlowKind, 0 for
// none), under htmlFlowLinesKey. continued is whether the line continues the paragraph rather than
// opening it: a block of kind 7, one complete tag and nothing else, interrupts a paragraph in
// neither parser, since the browser editor's copy of the construct drops micromark's exception for
// a lazy line (packages/proof-editor/src/html-flow.ts), so a continuation line opens one nowhere
// and is recorded nowhere. Kinds 1 to 6 interrupt a paragraph in both parsers.
func recordHTMLFlowLine(kind int, continued bool, start int, pc parser.Context) {
	if kind == 0 || kind == 7 && continued {
		return
	}
	recorded, _ := pc.Get(htmlFlowLinesKey).(map[int]struct{})
	if recorded == nil {
		recorded = make(map[int]struct{})
		pc.Set(htmlFlowLinesKey, recorded)
	}
	recorded[start] = struct{}{}
}

// htmlFlowLines is where each paragraph line the browser editor's parser reads as opening a block
// of HTML starts, recorded on the document node stands in (htmlFlowLinesAttr), or nil where the
// document holds none.
func htmlFlowLines(node ast.Node) map[int]struct{} {
	for node.Parent() != nil {
		node = node.Parent()
	}
	recorded, _ := node.Attribute(htmlFlowLinesAttr)
	lines, _ := recorded.(map[int]struct{})
	return lines
}

// dropHTMLFlowLines takes the lines a parse recorded on root off it (htmlFlowLinesAttr), so that
// nothing is refused for opening a block of HTML the browser editor's parser reads. The reader of
// one textblock's inline markdown calls it (readInline): that refusal is about a document's block
// structure - a line that opens a block of HTML where it lands - and inline markdown lands inside
// a textblock the document already holds, where a line opens no block at all, since the renderer
// escapes a marker that would open one there (escape.go, htmlBlockStart). The readers of a
// document's markdown keep the recording, so a caller who writes such a line as a document's own
// is refused as before. Goldmark's node only sets attributes, so the lines are set to none rather
// than removed, which htmlFlowLines reads as a document that holds none.
func dropHTMLFlowLines(root ast.Node) {
	root.SetAttribute(htmlFlowLinesAttr, map[int]struct{}(nil))
}

// htmlFlowRefusal is why node, a paragraph, text block or setext heading holding lines, cannot be
// stored when the browser editor's parser reads one of them as opening a block of HTML (recorded,
// from htmlFlowLines), naming the first such line as source's markdown numbers it from firstLine,
// or nil where it reads none of them so. It wraps ErrBlockHTML, the refusal of the block of HTML
// goldmark opens itself, because the document is unreadable for the same reason and a caller
// telling the two apart would learn nothing (docs/edits.go, unreadableReason).
//
// A node that is read alike is not refused: that parser's block of HTML runs to the first blank
// line, and the editor draws such a block as a paragraph holding its text as inline HTML
// (packages/proof-editor, remarkHtmlBlockParagraphs), which is goldmark's own reading of the one
// line. So the two agree where the block holds that line and nothing else: the node holds no
// other line, and nothing stands between it and the blank line or the container's end - no block
// opening right under it (a heading, a list, a fence, a rule, a table), which goldmark reads as
// its own and that parser swallows, and no setext underline, which goldmark takes into the
// heading and that parser swallows the same way.
func htmlFlowRefusal(node ast.Node, lines *gmtext.Segments, recorded map[int]struct{}, source []byte, firstLine int) error {
	if recorded == nil {
		return nil
	}
	if lines.Len() < 2 && readAloneBeforeABlankLine(node) {
		return nil
	}
	for index := range lines.Len() {
		start := lineStart(source, lines.At(index).Start)
		if _, ok := recorded[start]; !ok {
			continue
		}
		end := bytes.IndexByte(source[start:], '\n')
		if end < 0 {
			end = len(source) - start
		}
		number := firstLine + bytes.Count(source[:start], []byte("\n"))
		written := strings.TrimRight(string(source[start:start+end]), "\r")
		return fmt.Errorf("%w: line %d, %q, opens with HTML the browser editor's parser reads as a block of HTML where goldmark reads the text of a paragraph: that parser takes a tab in the line's indentation, inside the tag or after it as goldmark takes spaces, and reads a declaration opening with a lower-case letter, or a closing tag of script, pre or style, as a block as well; escape the < to write it as text", ErrBlockHTML, number, written)
	}
	return nil
}

// readAloneBeforeABlankLine reports whether node is the last block the browser editor's parser's
// block of HTML could hold: a blank line, or the end of the container around it, stands right
// after it, so that parser's block ends where goldmark's node does. A setext heading never is:
// its underline stands on the line after the lines it holds, and that parser takes the underline
// into the block. A sibling under it with no blank line between is the block's next line too.
func readAloneBeforeABlankLine(node ast.Node) bool {
	if _, heading := node.(*ast.Heading); heading {
		return false
	}
	next := node.NextSibling()
	return next == nil || next.HasBlankPreviousLines()
}

// htmlFlowKind is the kind of block of HTML the browser editor's parser (micromark's html-flow,
// copied into packages/proof-editor/src/html-flow.ts) opens on line, what is left of a line from
// column on, where nothing holds it off - 1 to 7 as CommonMark numbers them - or 0 where it opens
// none. Four columns of indentation stop it, and then, read from the `<`: a raw text opening
// (`<script`, `<pre`, `<style`, `<textarea`, the name ended by a space, a tab, `/`, `>` or the
// line's end) is kind 1, `<!--` kind 2, `<?` kind 3, `<!` and a letter of either case kind 4,
// `<![CDATA[` kind 5, an opening or closing tag of a block-level name (htmlBlockNames) ended so,
// or by `/>`, kind 6, and one complete opening or closing tag of any other name with nothing after
// it but spaces and tabs kind 7. Whether a kind opens a block while it interrupts a paragraph is
// the caller's (recordHTMLFlowLine): this answers only what the line holds.
//
// It is a port of micromark's tokenizer, state for state, not of goldmark's patterns, and the two
// differ where it matters: micromark reads a tab as the whitespace it allows in the indentation,
// between a tag's parts and after the tag, takes no space between a closing tag's slash and its
// name, and lets a declaration open with a letter of either case.
func htmlFlowKind(line []byte, column int) int {
	indent, position := util.IndentWidth(line, column)
	if indent >= 4 {
		return 0
	}
	line = bytes.TrimRight(line[position:], "\r\n")
	if len(line) < 2 || line[0] != '<' {
		return 0
	}
	switch {
	case bytes.HasPrefix(line, []byte("<!--")):
		return 2
	case line[1] == '?':
		return 3
	case bytes.HasPrefix(line, []byte("<![CDATA[")):
		return 5
	case line[1] == '!':
		// A declaration, which micromark opens on a letter of either case where goldmark's
		// pattern takes an upper-case one alone.
		if len(line) > 2 && isASCIILetter(line[2]) {
			return 4
		}
		return 0
	}
	index := 1
	next := func() byte {
		if index < len(line) {
			return line[index]
		}
		return 0
	}
	closing := next() == '/'
	if closing {
		index++
	}
	if !isASCIILetter(next()) {
		return 0
	}
	nameStart := index
	for char := next(); isASCIILetter(char) || char >= '0' && char <= '9' || char == '-'; char = next() {
		index++
	}
	// The name ends at the line's end, a slash, a `>`, a space or a tab, and ends the tag
	// nowhere else.
	if char := next(); char != 0 && char != '/' && char != '>' && !htmlSpace(char) {
		return 0
	}
	raw, block := htmlNamed(line[nameStart:index])
	slash := next() == '/'
	switch {
	case raw && !slash && !closing:
		return 1
	case block && slash:
		if index+1 < len(line) && line[index+1] == '>' {
			return 6
		}
		return 0
	case block:
		return 6
	}
	if closing {
		for htmlSpace(next()) {
			index++
		}
	} else if !completeAttributes(line, &index) {
		return 0
	}
	if next() != '>' {
		return 0
	}
	index++
	for htmlSpace(next()) {
		index++
	}
	if index != len(line) {
		return 0
	}
	return 7
}

// completeAttributes reads a complete opening tag's attributes from line[*at] on, state by state
// as micromark's html-flow does (completeAttributeNameBefore and the states after it), up to the
// tag's `>`: it leaves *at there, or at a character that is not `>`, which no complete tag holds,
// or reports false where an attribute's value cannot stand.
func completeAttributes(line []byte, at *int) bool {
	char := func() byte {
		if *at < len(line) {
			return line[*at]
		}
		return 0
	}
	nameStart := func(char byte) bool { return char == ':' || char == '_' || isASCIILetter(char) }
	nameChar := func(char byte) bool {
		return nameStart(char) || char >= '0' && char <= '9' || char == '-' || char == '.'
	}
	const (
		nameBefore = iota
		nameAfter
		valueBefore
	)
	for state := nameBefore; ; {
		switch current := char(); state {
		case nameBefore:
			switch {
			case current == '/':
				// The tag's own closing slash: only a `>` may follow it.
				*at++
				return true
			case nameStart(current):
				for nameChar(char()) {
					*at++
				}
				state = nameAfter
			case htmlSpace(current):
				*at++
			default:
				return true
			}
		case nameAfter:
			switch {
			case current == '=':
				*at++
				state = valueBefore
			case htmlSpace(current):
				*at++
			default:
				state = nameBefore
			}
		case valueBefore:
			switch {
			case current == 0 || current == '<' || current == '=' || current == '>' || current == '`':
				return false
			case current == '"' || current == '\'':
				*at++
				end := bytes.IndexByte(line[*at:], current)
				if end < 0 {
					return false
				}
				*at += end + 1
				if after := char(); after != '/' && after != '>' && !htmlSpace(after) {
					return false
				}
				state = nameBefore
			case htmlSpace(current):
				*at++
			default:
				for value := char(); value != 0 && !htmlSpace(value) && !htmlValueEnd(value); value = char() {
					*at++
				}
				state = nameAfter
			}
		}
	}
}

// htmlSpace and htmlValueEnd are micromark's markdownSpace and the characters that end an
// unquoted attribute value, over the bytes of a line that holds no line ending: a tab stands for
// the columns it spans, which micromark reads as spaces of its own. Its asciiAlpha is this
// package's isASCIILetter.
func htmlSpace(char byte) bool { return char == ' ' || char == '\t' }

func htmlValueEnd(char byte) bool {
	switch char {
	case '"', '\'', '/', '<', '=', '>', '`':
		return true
	}
	return false
}

// htmlNamed reports whether name, a tag name read as written, is one micromark opens a block of
// kind 1 or of kind 6 on: a name of htmlRawNames or of htmlBlockNames, which both it and the
// writer's own reading of a line (htmlBlockStart) match in any case. Goldmark's list
// (parser/html_block.go, allowedBlockTags) holds one name more, `meta`, which it opens a block of
// kind 6 on and refuses there.
func htmlNamed(name []byte) (raw, block bool) {
	return htmlNameIn(htmlRawNames, name), htmlNameIn(htmlBlockNames, name)
}

// htmlNameIn reports whether name, read as written, is one of names, which are written in lower
// case: it folds name's own case as it compares, taking no buffer and no string of its own.
func htmlNameIn(names []string, name []byte) bool {
	for _, known := range names {
		if len(known) != len(name) {
			continue
		}
		folded := true
		for position, char := range name {
			if char >= 'A' && char <= 'Z' {
				char += 'a' - 'A'
			}
			if char != known[position] {
				folded = false
				break
			}
		}
		if folded {
			return true
		}
	}
	return false
}
