package pmdoc

import (
	"bytes"
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	gmtext "github.com/yuin/goldmark/text"
)

// rawTitleLinesHold reports whether raw, an image written with its titles' line feeds as they are,
// keeps every line those line feeds create in the paragraph the image stands in, as the line
// before's continuation, in both Go's reader and the browser editor's engine, so that the image
// reads back as held in both. lazy says the paragraph stands in a list item, a quote or a footnote
// definition, whose prefix these lines are written without, so each is a lazy continuation line.
// A typed block behind such a prefix takes no lazy line at all, and ends before it.
//
// Each line is judged by how it starts (opensAsText), never by reading it or the document back, so
// the judgement is linear in the document. It is an allow-list of line starts at which no block
// construct of CommonMark, GFM or the extensions the editor reads (directives, footnotes, math) can
// begin in either reader, not a reading by Go's parser, because the two readers disagree on lines
// Go reads as the paragraph's text: at a lazy line the engine's open container is the list item or
// the quote, not the paragraph, so it opens there blocks that cannot interrupt a paragraph (an HTML
// block of the complete-tag kind, an ordered list item not starting at 1); and its HTML block rules
// are not Go's even in a top-level paragraph (it reads `</script>` as a complete closing tag and
// `<!x>` as a declaration, which Go's leaves out). Judging the lines by Go's reading missed one such
// shape a round (INFRA-190). A line the allow-list does not admit is written `&#10;`, which costs
// its bytes, never a link.
func (r *renderer) rawTitleLinesHold(raw string, lazy bool) bool {
	typed := r.scope().typed
	if typed != nil && typed.prefix != "" && lazy {
		return false
	}
	lines := strings.Split(raw, "\n")
	for index := 1; index < len(lines); index++ {
		line := lines[index]
		if !opensAsText(line, lazy, typed != nil) {
			return false
		}
		if width, ok := delimiterRowWidth(line); ok && !lazy && r.makesTable(lines, index, width) {
			return false
		}
	}
	return true
}

// makesTable reports whether lines[index], a delimiter row width cells wide that raw's lines
// (lines) write in a paragraph, not lazily, can make a table of it under the line before, its
// header: lines[index-1], or for raw's first line feed the written line up to it, holding more than
// a lone `|` and no wider than the delimiter row. Go's table reader (lazyTableRows) takes only the
// paragraph's first delimiter row, but the browser editor's engine takes a later line of the
// paragraph as a header too, a delimiter row before it included (`| - |` over `| - |` is a table
// there), so every delimiter row is judged under its own header. A lazy line makes no table in
// either reader. The written line is read through the renderer's running tally of it
// (writtenLineTally), so a long line is read once however many titles on it are judged.
func (r *renderer) makesTable(lines []string, index, width int) bool {
	var header rowTally
	if index == 1 {
		header = r.writtenLineTally()
	}
	header.add([]byte(lines[index-1]), 0)
	cells, lone := header.width()
	return !lone && cells <= width
}

// rowTally is what tableRowWidth and lonePipe read of a table line, kept as the line is read a
// piece at a time: rowCells drops the line's white space at each end, then its first `|` and its
// last, and splits the rest at each `|` with no backslash right before it.
type rowTally struct {
	// read is how many bytes were added; pipes counts the `|`s added with no backslash before them.
	read, pipes int
	// first and last are the offsets of the first and the last byte that is not a space or a tab,
	// -1 before any; firstByte, lastByte and beforeLast are those bytes and the one before last.
	first, last                     int
	firstByte, lastByte, beforeLast byte
	// previous is the last byte added, which escapes a `|` the next piece opens with.
	previous byte
}

// add reads piece, the next bytes of the line, from offset on.
func (t *rowTally) add(piece []byte, offset int) {
	if t.read == 0 && offset == 0 {
		t.first, t.last = -1, -1
	}
	for index := offset; index < len(piece); index++ {
		char := piece[index]
		if char == '|' && t.previous != '\\' {
			t.pipes++
		}
		if char != ' ' && char != '\t' {
			if t.first < 0 {
				t.first, t.firstByte = t.read, char
			}
			t.last, t.lastByte, t.beforeLast = t.read, char, t.previous
		}
		t.previous = char
		t.read++
	}
}

// width is the line's cell count as tableRowWidth counts it, and whether it is a lone `|`
// (lonePipe).
func (t *rowTally) width() (cells int, lone bool) {
	if t.first < 0 {
		return 0, false
	}
	if t.first == t.last && t.firstByte == '|' {
		return 0, true
	}
	pipes, from, to := t.pipes, t.first, t.last+1
	if t.firstByte == '|' {
		pipes--
		from++
	}
	if t.lastByte == '|' && t.last >= from {
		to--
		if t.beforeLast != '\\' {
			pipes--
		}
	}
	if from >= to {
		return 0, false
	}
	return pipes + 1, false
}

// writtenLineTally is the tally of the line the renderer is writing, from its start to the end of
// what is written. It is kept between calls and read on from where the last call stopped, so each
// byte of the line is read once; a truncation (truncate) back into the read part restarts it from
// the line's start.
func (r *renderer) writtenLineTally() rowTally {
	written := r.b.Bytes()
	line := &r.lineTally
	if r.truncatedTo < line.end || line.end > len(written) {
		line.start = bytes.LastIndexByte(written[:min(r.truncatedTo, len(written))], '\n') + 1
		line.end, line.tally = line.start, rowTally{}
	}
	r.truncatedTo = math.MaxInt
	for line.end < len(written) {
		next := bytes.IndexByte(written[line.end:], '\n')
		if next < 0 {
			line.tally.add(written[line.start:], line.end-line.start)
			line.end = len(written)
			break
		}
		line.start = line.end + next + 1
		line.end, line.tally = line.start, rowTally{}
	}
	return line.tally
}

// delimiterRowWidth is the width of line read as a table's delimiter row (tableDelimiterWidth), or
// false when it is none.
func delimiterRowWidth(line string) (int, bool) {
	source := []byte(line)
	return tableDelimiterWidth(gmtext.NewSegment(0, len(source)), source)
}

// opensAsText reports whether line, a line of an open paragraph, a lazy continuation line where
// lazy, starts where no block can begin in either reader. A blank line ends the paragraph, and one
// opening with a space or a tab loses them to it (or opens indented code), so neither is text. By
// its first character:
//
//   - a letter, a non-ASCII character, `"`, `\`, `&` and any punctuation below that is not named
//     start no block;
//   - digits open an ordered list item only as a marker - at most nine digits, `.` or `)`, then a
//     space, a tab or the line's end - which interrupts a paragraph only starting at 1 with content
//     (orderedMarkerOpens);
//   - `#` opens a heading only as one to six of them before a space, a tab or the end;
//   - `-`, `*`, `+`, `_`, `=` open a bullet item as a marker before a space, a tab or the end -
//     interrupting a paragraph only with content after it, where a lazy line opens an empty one -
//     a thematic break as three or more of one of `-*_` and blanks alone, and outside a lazy line
//     a setext underline as `=`s or `-`s alone; a table's delimiter row (`-`, `:` and `|` lines)
//     is judged under its header (makesTable);
//   - three or more backticks or tildes open a fence, a backtick fence only where its info string
//     holds no backtick (fenceOpens);
//   - `>` opens a quote, and a directive line, past any white space it opens with, is refused or
//     closes a typed block (directiveLineOpens);
//   - `[^label]:` opens a footnote definition; a link reference definition cannot interrupt a
//     paragraph in either reader, nor open at a lazy line;
//   - `<` opens an HTML block only as htmlBlockOpens says.
func opensAsText(line string, lazy, inTyped bool) bool {
	if strings.Trim(line, " \t") == "" || line[0] == ' ' || line[0] == '\t' {
		return false
	}
	first, _ := utf8.DecodeRuneInString(line)
	if jsWhitespace(first) {
		return !directiveLineOpens(line, inTyped)
	}
	if first >= utf8.RuneSelf || unicode.IsLetter(first) {
		return true
	}
	switch first {
	case '>':
		return false
	case '#':
		hashes := len(line) - len(strings.TrimLeft(line, "#"))
		return hashes > 6 || !blankOrEnd(line[hashes:])
	case '-', '*', '+', '_', '=', ':', '|':
		return !markerLine(line, lazy, inTyped)
	case '`', '~':
		return !fenceOpens(line)
	case '[':
		return !footnoteDefinitionOpener.MatchString(line)
	case '<':
		return !htmlBlockOpens(line, lazy)
	}
	if first >= '0' && first <= '9' {
		return !orderedMarkerOpens(line, lazy)
	}
	return true
}

// directiveLineOpens reports whether line, a later line of a paragraph, is a directive both
// readers refuse there, read past the white space it opens with as they read it
// (paragraphDirectiveReason): a line opening with three colons but `:::` alone, a typed block's
// opening included unless white space before it keeps Go's reader from taking it for one, a leaf
// directive (`::name`) or a text directive (`:name{`); or, in a typed block, the `:::` that closes
// it.
func directiveLineOpens(line string, inTyped bool) bool {
	read := strings.TrimLeftFunc(line, jsWhitespace)
	switch {
	case read == ":::":
		return inTyped
	case strings.HasPrefix(read, ":::"):
		return read == line || !paragraphTypedOpening.MatchString(read)
	}
	_, refused := unsupportedDirectiveReason(read)
	return refused
}

// fenceOpens reports whether line, opening with a backtick or a tilde, opens a fenced code block:
// a run of three or more of one, and for backticks no backtick in the rest of the line, which
// CommonMark's info string may not hold: three backticks before "a`b" are text in both readers.
func fenceOpens(line string) bool {
	char := line[0]
	run := len(line) - len(strings.TrimLeft(line, string(char)))
	return run >= 3 && (char == '~' || !strings.Contains(line[run:], "`"))
}

// blankOrEnd reports whether rest, what follows a marker, begins with a space or a tab or is empty.
func blankOrEnd(rest string) bool {
	return rest == "" || rest[0] == ' ' || rest[0] == '\t'
}

// markerLine reports whether line, opening with one of `-*+_=:|`, opens a block: a bullet marker,
// with content outside a lazy line; a thematic break; outside a lazy line, a setext underline, which
// a lazy line cannot be in either reader; or a directive line (directiveLineOpens).
func markerLine(line string, lazy, inTyped bool) bool {
	switch line[0] {
	case '-', '*', '+':
		// A bullet item interrupts a paragraph only with content after its marker; at a lazy line
		// the engine opens an empty one too.
		if blankOrEnd(line[1:]) && (lazy || strings.Trim(line[1:], " \t") != "") {
			return true
		}
	case ':':
		if directiveLineOpens(line, inTyped) {
			return true
		}
	}
	if char := line[0]; char == '-' || char == '*' || char == '_' {
		if run := strings.Count(line, string(char)); run >= 3 && strings.Trim(line, string(char)+" \t") == "" {
			return true
		}
	}
	char := line[0]
	return !lazy && (char == '=' || char == '-') && strings.TrimRight(strings.TrimLeft(line, string(char)), " \t") == ""
}

// orderedMarkerOpens reports whether line, opening with a digit, opens an ordered list item there:
// a marker of at most nine digits and `.` or `)` before a space, a tab or the end, which at a lazy
// line opens one in the engine whatever its number, and in a paragraph interrupts it only starting
// at 1, written `1` (both readers take `01` for another start), with content after the marker.
func orderedMarkerOpens(line string, lazy bool) bool {
	digits := len(line) - len(strings.TrimLeft(line, "0123456789"))
	if digits > 9 || digits == len(line) || line[digits] != '.' && line[digits] != ')' || !blankOrEnd(line[digits+1:]) {
		return false
	}
	if lazy {
		return true
	}
	return line[:digits] == "1" && strings.Trim(line[digits+1:], " \t") != ""
}

// footnoteDefinitionOpener is a footnote definition's opening `[^label]:`.
var footnoteDefinitionOpener = regexp.MustCompile(`^\[\^[^\]]*\]:`)

// htmlBlockOpens reports whether line, opening with `<`, opens an HTML block in either reader at a
// later line of a paragraph, lazy or not: the union of Go's reader (goldmarkHTMLBlockOpens) and the
// browser editor's engine (micromarkHTMLFlowOpens). The two lists of block tag names and the two
// rules for what may follow a tag name differ, so neither reader's rule alone says a line is text.
func htmlBlockOpens(line string, lazy bool) bool {
	return goldmarkHTMLBlockOpens(line) || micromarkHTMLFlowOpens(line, lazy)
}

// goldmarkHTMLBlockOpens reports whether Go's reader opens an HTML block at line, a later line of
// an open paragraph: goldmark v1.8.6 parser/html_block.go, htmlBlockParser.Open, whose patterns
// below are copied from it. Its kinds 1 to 6 interrupt a paragraph, at a lazy line too, since the
// paragraph is still the last block opened there; its kind 7 never does. Kind 1 opens on a raw
// element's opening tag followed by any white space Go's `\s` matches, a form feed included, `>`,
// `/>` or the line's end; kind 6 on a block tag name in goldmark's list (allowedBlockTags, which
// holds `meta`, unlike CommonMark's), opening or closing with spaces after the `/`, followed by a
// space, `>`, `/>` or the end, or as a complete tag whatever white space its attributes hold.
func goldmarkHTMLBlockOpens(line string) bool {
	if goldmarkHTMLType1.MatchString(line) || goldmarkHTMLTypes2To5.MatchString(line) {
		return true
	}
	if match := goldmarkHTMLType7.FindStringSubmatch(line); match != nil && goldmarkBlockTags[strings.ToLower(match[2])] {
		return true
	}
	match := goldmarkHTMLType6.FindStringSubmatch(line)
	return match != nil && goldmarkBlockTags[strings.ToLower(match[1])]
}

// The patterns of goldmark v1.8.6's parser/html_block.go (htmlBlockType1OpenRegexp, the kinds 2 to
// 5 openings, htmlBlockType6Regexp, htmlBlockType7Regexp) and parser/raw_html.go's attributePattern,
// with their leading indentation dropped: a title line opening with a space is never written raw.
var (
	goldmarkHTMLType1     = regexp.MustCompile(`(?i)^<(script|pre|style|textarea)(?:\s.*|>.*|/>.*|)$`)
	goldmarkHTMLTypes2To5 = regexp.MustCompile(`^<(?:!--|\?|![A-Z]+|!\[CDATA\[)`)
	goldmarkHTMLType6     = regexp.MustCompile(`^<(?:/[ ]*)?([a-zA-Z]+[a-zA-Z0-9\-]*)(?:[ ].*|>.*|/>.*|)$`)
	goldmarkHTMLType7     = regexp.MustCompile(`^<(/[ ]*)?([a-zA-Z]+[a-zA-Z0-9\-]*)(` + goldmarkAttribute + `*)[ ]*(?:>|/>)[ ]*$`)
	goldmarkAttribute     = `(?:[\r\n \t]+[a-zA-Z_:][a-zA-Z0-9:._-]*(?:[\r\n \t]*=[\r\n \t]*(?:[^\"'=<>` + "`" + `\x00-\x20]+|'[^']*'|"[^"]*"))?)`
)

// goldmarkBlockTags is goldmark v1.8.6's allowedBlockTags (parser/html_block.go): CommonMark's
// block tag names and `meta`.
var goldmarkBlockTags = withNames(micromarkBlockNames, "meta")

// micromarkHTMLFlowOpens reports whether the browser editor's engine opens an HTML block at line, a
// later line of an open paragraph, lazy where lazy: micromark-core-commonmark 2.0.3
// lib/html-flow.js, tokenizeHtmlFlow. `<!--`, `<?`, `<!` and a letter (any case), `<![CDATA[` open
// one; so does a tag name, opening or closing with nothing between `</` and the name, of letters,
// digits and `-`, that ends at the line's end, a space, a tab, `>` or `/`: a raw element's opening
// tag not followed by `/` (htmlRawNames), any tag of a block element's name (htmlBlockNames), `/`
// then only as `/>`; and otherwise a line that is one complete tag, which interrupts no paragraph
// but opens a block at a lazy line, where the engine's open container is the list item or the quote
// (`self.parser.lazy`), a raw element's closing tag included.
func micromarkHTMLFlowOpens(line string, lazy bool) bool {
	rest := line[1:]
	switch {
	case strings.HasPrefix(rest, "!--"), strings.HasPrefix(rest, "?"), strings.HasPrefix(rest, "![CDATA["):
		return true
	case strings.HasPrefix(rest, "!"):
		return len(rest) > 1 && isASCIIAlpha(rest[1])
	}
	closing := strings.HasPrefix(rest, "/")
	name := strings.TrimPrefix(rest, "/")
	if name == "" || !isASCIIAlpha(name[0]) {
		return false
	}
	end := 1
	for end < len(name) && (isASCIIAlphaNumeric(name[end]) || name[end] == '-') {
		end++
	}
	after := name[end:]
	if after != "" && after[0] != ' ' && after[0] != '\t' && after[0] != '>' && after[0] != '/' {
		return false
	}
	lower := strings.ToLower(name[:end])
	slash := strings.HasPrefix(after, "/")
	switch {
	case !slash && !closing && micromarkRawNames[lower]:
		return true
	case micromarkBlockNames[lower]:
		return !slash || strings.HasPrefix(after, "/>")
	}
	return lazy && completeTagLine.MatchString(line)
}

// isASCIIAlpha reports whether value is an ASCII letter.
func isASCIIAlpha(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

// micromarkRawNames is micromark-util-html-tag-name 2.0.1's htmlRawNames.
var micromarkRawNames = withNames(nil, "pre", "script", "style", "textarea")

// micromarkBlockNames is micromark-util-html-tag-name 2.0.1's htmlBlockNames, CommonMark's list.
var micromarkBlockNames = withNames(nil,
	"address", "article", "aside", "base", "basefont", "blockquote", "body", "caption", "center", "col",
	"colgroup", "dd", "details", "dialog", "dir", "div", "dl", "dt", "fieldset", "figcaption", "figure",
	"footer", "form", "frame", "frameset", "h1", "h2", "h3", "h4", "h5", "h6", "head", "header", "hr",
	"html", "iframe", "legend", "li", "link", "main", "menu", "menuitem", "nav", "noframes", "ol",
	"optgroup", "option", "p", "param", "search", "section", "summary", "table", "tbody", "td",
	"tfoot", "th", "thead", "title", "tr", "track", "ul",
)

// withNames is a set of names: those of base and the rest.
func withNames(base map[string]bool, names ...string) map[string]bool {
	set := make(map[string]bool, len(base)+len(names))
	for name := range base {
		set[name] = true
	}
	for _, name := range names {
		set[name] = true
	}
	return set
}

// completeTagLine is a line holding one complete opening or closing tag, as micromark's html-flow
// reads one (completeAttributeNameBefore through completeAfter), and blanks after it: an HTML block
// of the seventh kind.
var completeTagLine = regexp.MustCompile(`^(?:<[A-Za-z][A-Za-z0-9-]*` +
	`(?:[ \t]+[A-Za-z_:][A-Za-z0-9_.:-]*(?:[ \t]*=[ \t]*(?:[^ \t"'=<>/` + "`" + `]+|'[^']*'|"[^"]*"))?)*` +
	`[ \t]*/?>|</[A-Za-z][A-Za-z0-9-]*[ \t]*>)[ \t]*$`)
