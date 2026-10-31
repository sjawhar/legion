package pmdoc

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
)

// htmlFlowLinesKey holds, while a document parses, which kind of block of HTML the browser
// editor's parser opens on each line goldmark gave to a paragraph and that parser reads as opening
// one (htmlFlowKind), by where the line starts, and htmlFlowLinesAttr holds the same on the parsed
// document root afterwards (parseSource). Both parsers read the seven kinds of block CommonMark
// names, but that parser's grammar is micromark's own tokenizer where goldmark's is a handful of
// patterns (parser/html_block.go), and the two disagree at the edges: micromark takes a tab where
// those patterns take only a space, in the line's indentation, between a tag's parts and after the
// tag, and it reads a closing tag of a raw text name as a block as well. So `> </a\t>` and
// `> a\n> \t<div>` open a block of HTML in the browser and are the text of a paragraph to
// goldmark. The Proof schema holds no block of HTML, and the browser editor drops the block it
// reads together with the container around it, so such a document cannot read back: blockRefusal
// refuses the paragraph, text block or setext heading holding such a line (htmlFlowRefusal).
// Where goldmark's own patterns open a block of HTML it refuses that block already (ErrBlockHTML)
// and the line belongs to no paragraph, so nothing is recorded for it; the lines recorded here are
// the ones those patterns miss.
//
// A declaration opening with a lower-case letter (`<!x>`) was one of those lines until PR #2518,
// which has this package's reader open that block itself wherever micromark does
// (declarationBlockParser, opensDeclaration): goldmark's tree holds a block of HTML there now, so
// the line belongs to no paragraph and the two parsers no longer disagree about it, except where
// a tab stands in its indentation, which that parser alone still reads as a block.
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
func recordHTMLFlowLine(kind ast.HTMLBlockType, continued bool, start int, pc parser.Context) {
	if kind == 0 || kind == ast.HTMLBlockType7 && continued {
		return
	}
	recorded, _ := pc.Get(htmlFlowLinesKey).(map[int]ast.HTMLBlockType)
	if recorded == nil {
		recorded = make(map[int]ast.HTMLBlockType)
		pc.Set(htmlFlowLinesKey, recorded)
	}
	recorded[start] = kind
}

// htmlFlowLines is which kind of block of HTML the browser editor's parser opens on each paragraph
// line it reads as opening one, by where that line starts, recorded on the document node stands in
// (htmlFlowLinesAttr), or nil where the document holds none.
func htmlFlowLines(node ast.Node) map[int]ast.HTMLBlockType {
	for node.Parent() != nil {
		node = node.Parent()
	}
	recorded, _ := node.Attribute(htmlFlowLinesAttr)
	lines, _ := recorded.(map[int]ast.HTMLBlockType)
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
	root.SetAttribute(htmlFlowLinesAttr, map[int]ast.HTMLBlockType(nil))
}

// browserBlockHTML is why a line the browser editor's parser alone reads as opening a block of
// HTML cannot be stored: the part of htmlFlowRefusal's reason that follows the line it names,
// which differs from document to document. It is a constant so that what the reader says and what
// a test asserts are one text, as the footnote definition refusals are (typedDefinitionInItem).
const browserBlockHTML = "opens with HTML the browser editor's parser reads as a block of HTML where goldmark reads the text of a paragraph: that parser takes a tab in the line's indentation, inside the tag or after it as goldmark takes spaces, and reads a closing tag of script, pre or style as a block as well; escape the < to write it as text"

// htmlFlowRefusal is why node, a paragraph, text block or setext heading, cannot be stored when
// the browser editor's parser reads one of its lines as opening a block of HTML (recorded, from
// htmlFlowLines), naming the first such line as source's markdown numbers it from firstLine, or
// nil where it reads none of them so. It wraps ErrBlockHTML, the refusal of the block of HTML
// goldmark opens itself, because the document is unreadable for the same reason and a caller
// telling the two apart would learn nothing (docs/edits.go, unreadableReason).
//
// A node that is read alike is not refused: a block of kind 7, one complete tag and nothing else,
// runs to the first blank line, and the editor draws such a block as a paragraph holding its text
// as inline HTML (packages/proof-editor, remarkHtmlBlockParagraphs), which is goldmark's own
// reading of the one line. So the two agree where the block holds that line and nothing else: the
// line opens a block of that kind, the node holds no other line, and nothing stands between it and
// the blank line or the container's end - no block opening right under it (a heading, a list, a
// fence, a rule, a table), which goldmark reads as its own and that parser swallows, and no setext
// underline, which goldmark takes into the heading and that parser swallows the same way.
//
// A block of kind 6 runs to the first blank line the same way, and the exemption covered it until
// PR #2507, which found it letting a divergent reading through: `- a\n\n  \t<div x\n` is one
// line, alone before the container's end, whose block that parser draws as a paragraph holding
// the inline HTML `<div x`, while this reader reads the text `<div x` and the writer stores it as
// `\<div x`, which reads back as text. A tag of a block-level name opens kind 6 however
// incomplete it is, where kind 7 asks for one whole tag and nothing after it, and it is the
// unfinished tag the two read apart; the exemption cannot tell it from the whole one, so kind 6
// is refused like the rest.
//
// A block of kinds 1 to 5 runs to an end marker of its own instead (`</script>`, `-->`, `>`, `?>`,
// `]]>`) and swallows every line up to it, blank lines among them. Where the marker ends the block
// on its own line the two do read it alike - `> \t<!-- c -->` is a paragraph holding the inline
// HTML `<!-- c -->` to both - but goldmark refuses the same block written without the tab, which
// its own patterns open (ErrBlockHTML), so refusing the tab form treats the two writings alike.
func htmlFlowRefusal(node ast.Node, recorded map[int]ast.HTMLBlockType, source []byte, firstLine int) error {
	if recorded == nil {
		return nil
	}
	lines := node.Lines()
	for index := range lines.Len() {
		start := lineStart(source, lines.At(index).Start)
		kind, opens := recorded[start]
		if !opens {
			continue
		}
		if kind == ast.HTMLBlockType7 && lines.Len() < 2 && readAloneBeforeABlankLine(node) {
			return nil
		}
		end := bytes.IndexByte(source[start:], '\n')
		if end < 0 {
			end = len(source) - start
		}
		number := firstLine + bytes.Count(source[:start], []byte("\n"))
		written := strings.TrimRight(string(source[start:start+end]), "\r")
		return fmt.Errorf("%w: line %d, %q, %s", ErrBlockHTML, number, written, browserBlockHTML)
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
// column on, where nothing holds it off - ast.HTMLBlockType1 to ast.HTMLBlockType7, as CommonMark
// numbers the seven kinds - or 0 where it opens none. Four columns of indentation stop it, and
// then, read from the `<`: a raw text opening
// (`<script`, `<pre`, `<style`, `<textarea`, the name ended by a space, a tab, `/`, `>` or the
// line's end) is kind 1, `<!--` kind 2, `<?` kind 3, `<!` and a letter of either case kind 4
// (opensDeclaration), `<![CDATA[` kind 5, an opening or closing tag of a block-level name
// (htmlBlockNames) ended so, or by `/>`, kind 6, and one complete opening or closing tag of any
// other name with nothing after it but spaces and tabs kind 7. Whether a kind opens a block while
// it interrupts a paragraph is the caller's (recordHTMLFlowLine, htmlBlockStart): this answers
// only what the line holds.
//
// It is the package's one reading of that grammar, taken over a string or over the bytes of one as
// its caller holds the line: the reader asks it of each paragraph line it takes, and the writer
// asks it of each line it has written a `<` at the start of (htmlBlockStart). A second port of the
// same grammar is what would let the writer escape a `<` the refusal reads as text, or leave one
// it refuses unescaped.
//
// It is a port of micromark's tokenizer, state for state, not of goldmark's patterns, and the two
// differ where it matters: micromark reads a tab as the whitespace it allows in the indentation,
// between a tag's parts and after the tag, takes no space between a closing tag's slash and its
// name, and lets a declaration open with a letter of either case.
func htmlFlowKind[T string | []byte](line T, column int) ast.HTMLBlockType {
	indent, position := 0, 0
	for ; position < len(line); position++ {
		// A tab spans the columns to the next multiple of four from where it stands, which is
		// goldmark's util.IndentWidth over the bytes a caller holds.
		if line[position] == ' ' {
			indent++
		} else if line[position] == '\t' {
			indent += 4 - (column+indent)%4
		} else {
			break
		}
	}
	if indent >= 4 {
		return 0
	}
	line = line[position:]
	if end := htmlLineEnd(line); end >= 0 {
		line = line[:end]
	}
	if len(line) < 2 || line[0] != '<' {
		return 0
	}
	switch {
	case htmlOpensWith(line, "<!--"):
		return ast.HTMLBlockType2
	case line[1] == '?':
		return ast.HTMLBlockType3
	case htmlOpensWith(line, "<![CDATA["):
		return ast.HTMLBlockType5
	case line[1] == '!':
		// A declaration, which micromark and this package's reader open on a letter of either
		// case where goldmark's own pattern takes an upper-case one alone (opensDeclaration).
		if opensDeclaration(line) {
			return ast.HTMLBlockType4
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
		return ast.HTMLBlockType1
	case block && slash:
		if index+1 < len(line) && line[index+1] == '>' {
			return ast.HTMLBlockType6
		}
		return 0
	case block:
		return ast.HTMLBlockType6
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
	return ast.HTMLBlockType7
}

// htmlBlockStart reports whether the browser editor's parser opens a block of HTML at the `<` line
// begins with, line being the markdown written from that `<` to its line's end or past it, where
// the `<` is the first character after its line's container prefix. interrupt reports whether the
// line would go on a paragraph, which only a complete tag (kind 7) cannot interrupt; every other
// kind opens its block there as well.
//
// It is the one grammar (htmlFlowKind) asked the writer's question: the writer escapes a `<` that
// would open a block of HTML where goldmark reads a paragraph, and the reader refuses a line a
// caller wrote that way (htmlFlowRefusal), so the two read the same line by the same rule.
func htmlBlockStart(line string, interrupt bool) bool {
	kind := htmlFlowKind(line, 0)
	return kind != 0 && !(interrupt && kind == ast.HTMLBlockType7)
}

// completeAttributes reads a complete opening tag's attributes from line[*at] on, state by state
// as micromark's html-flow does (completeAttributeNameBefore and the states after it), up to the
// tag's `>`: it leaves *at there, or at a character that is not `>`, which no complete tag holds,
// or reports false where an attribute's value cannot stand.
func completeAttributes[T string | []byte](line T, at *int) bool {
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
				end := htmlIndexByte(line[*at:], current)
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

// htmlLineEnd is where line's first line ending stands, or -1 where it holds none: the reader
// gives the grammar one line with the ending it was read with, and the writer what it has written
// from a `<` on, which can run past that line.
func htmlLineEnd[T string | []byte](line T) int {
	for index := range len(line) {
		if line[index] == '\n' || line[index] == '\r' {
			return index
		}
	}
	return -1
}

// htmlOpensWith reports whether line opens with prefix.
func htmlOpensWith[T string | []byte](line T, prefix string) bool {
	if len(line) < len(prefix) {
		return false
	}
	for index := range len(prefix) {
		if line[index] != prefix[index] {
			return false
		}
	}
	return true
}

// htmlIndexByte is where char first stands in line, or -1 where it stands nowhere in it.
func htmlIndexByte[T string | []byte](line T, char byte) int {
	for index := range len(line) {
		if line[index] == char {
			return index
		}
	}
	return -1
}

// htmlNamed reports whether name, a tag name read as written, is one micromark opens a block of
// kind 1 or of kind 6 on: a name of htmlRawNames or of htmlBlockNames, which it matches in any
// case. Goldmark's list (parser/html_block.go, allowedBlockTags) holds one name more, `meta`,
// which it opens a block of kind 6 on and refuses there.
func htmlNamed[T string | []byte](name T) (raw, block bool) {
	return htmlNameIn(htmlRawNames, name), htmlNameIn(htmlBlockNames, name)
}

// htmlNameIn reports whether name, read as written, is one of names, which are written in lower
// case: it folds name's own case as it compares, taking no buffer and no string of its own.
func htmlNameIn[T string | []byte](names []string, name T) bool {
	for _, known := range names {
		if len(known) != len(name) {
			continue
		}
		folded := true
		for position := range len(name) {
			char := name[position]
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

// htmlRawNames and htmlBlockNames are the tag names that open a block of HTML of kind 1 and of
// kind 6 to the browser editor's parser (micromark-util-html-tag-name), which reads them in any
// case (htmlNameIn).
var (
	htmlRawNames   = []string{"pre", "script", "style", "textarea"}
	htmlBlockNames = []string{
		"address", "article", "aside", "base", "basefont", "blockquote", "body", "caption", "center",
		"col", "colgroup", "dd", "details", "dialog", "dir", "div", "dl", "dt", "fieldset",
		"figcaption", "figure", "footer", "form", "frame", "frameset", "h1", "h2", "h3", "h4", "h5",
		"h6", "head", "header", "hr", "html", "iframe", "legend", "li", "link", "main", "menu",
		"menuitem", "nav", "noframes", "ol", "optgroup", "option", "p", "param", "search", "section",
		"summary", "table", "tbody", "td", "tfoot", "th", "thead", "title", "tr", "track", "ul",
	}
)
