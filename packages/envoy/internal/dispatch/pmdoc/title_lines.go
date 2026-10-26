package pmdoc

import (
	"bytes"
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

// maxTableHeaderRead is how much of the line before a raw title's first line feed
// rawTitleLinesHold reads as the header of a delimiter row the title writes after it. Where that
// line is longer, the delimiter row is taken to make a table, and the title is written `&#10;`.
const maxTableHeaderRead = 4 << 10

// makesTable reports whether lines[index], a delimiter row width cells wide that raw's lines
// (lines) write in a paragraph, not lazily, can make a table of it under the line before, its
// header: lines[index-1], or for raw's first line feed the written line up to it, holding more than
// a lone `|` and no wider than the delimiter row. Go's table reader (lazyTableRows) takes only the
// paragraph's first delimiter row, but the browser editor's engine takes a later line of the
// paragraph as a header too, a delimiter row before it included (`| - |` over `| - |` is a table
// there), so every delimiter row is judged under its own header. A lazy line makes no table in
// either reader.
func (r *renderer) makesTable(lines []string, index, width int) bool {
	header := lines[index-1]
	if index == 1 {
		written := r.b.Bytes()
		from := max(len(written)-maxTableHeaderRead, 0)
		lineFrom := bytes.LastIndexByte(written[from:], '\n') + 1
		if lineFrom == 0 && from > 0 {
			return true
		}
		header = string(written[from+lineFrom:]) + header
	}
	source := []byte(header)
	return !lonePipe(source) && tableRowWidth(gmtext.NewSegment(0, len(source)), source) <= width
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

// rawHTMLNames are the tag names whose opening tag opens an HTML block of CommonMark's first kind,
// raw text up to its closing tag. The engine also reads their closing tag as a complete tag, which
// Go's does not; like any complete tag it interrupts no paragraph, and opens a block at a lazy line.
var rawHTMLNames = map[string]bool{"pre": true, "script": true, "style": true, "textarea": true}

// blockHTMLNames are the tag names whose tag, opening or closing, opens an HTML block of
// CommonMark's sixth kind, which interrupts a paragraph.
var blockHTMLNames = map[string]bool{
	"address": true, "article": true, "aside": true, "base": true, "basefont": true, "blockquote": true,
	"body": true, "caption": true, "center": true, "col": true, "colgroup": true, "dd": true,
	"details": true, "dialog": true, "dir": true, "div": true, "dl": true, "dt": true, "fieldset": true,
	"figcaption": true, "figure": true, "footer": true, "form": true, "frame": true, "frameset": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "head": true,
	"header": true, "hr": true, "html": true, "iframe": true, "legend": true, "li": true, "link": true,
	"main": true, "menu": true, "menuitem": true, "nav": true, "noframes": true, "ol": true,
	"optgroup": true, "option": true, "p": true, "param": true, "search": true, "section": true,
	"summary": true, "table": true, "tbody": true, "td": true, "tfoot": true, "th": true,
	"thead": true, "title": true, "tr": true, "track": true, "ul": true,
}

// htmlTagName is a tag name after `<` or `</`.
var htmlTagName = regexp.MustCompile(`^</?([A-Za-z][A-Za-z0-9-]*)`)

// completeTagLine is a line holding one complete opening or closing tag, as CommonMark defines it,
// and blanks after it: an HTML block of the seventh kind.
var completeTagLine = regexp.MustCompile(`^(?:<[A-Za-z][A-Za-z0-9-]*` +
	`(?:[ \t]+[A-Za-z_:][A-Za-z0-9_.:-]*(?:[ \t]*=[ \t]*(?:[^ \t"'=<>` + "`" + `]+|'[^']*'|"[^"]*"))?)*` +
	`[ \t]*/?>|</[A-Za-z][A-Za-z0-9-]*[ \t]*>)[ \t]*$`)

// htmlBlockOpens reports whether line, opening with `<`, opens an HTML block in either reader: a
// comment, processing instruction, declaration or CDATA (`<!-`, `<?`, `<!` and a letter, `<![`),
// whose open the engine also takes for a lowercase declaration Go's leaves as text; a raw element's
// opening tag; any tag of a block element's name; or, at a lazy line, where the engine opens what
// cannot interrupt a paragraph, a line that is one complete tag, a raw element's closing tag too. A `<` before anything else - a space, a
// digit, `=` - opens none, nor does a tag with text after it on its line.
func htmlBlockOpens(line string, lazy bool) bool {
	rest := line[1:]
	switch {
	case rest == "":
		return false
	case rest[0] == '?':
		return true
	case rest[0] == '!':
		if len(rest) == 1 {
			return false
		}
		next := rest[1]
		return strings.HasPrefix(rest, "!--") || next == '[' || next >= 'a' && next <= 'z' || next >= 'A' && next <= 'Z'
	}
	name := htmlTagName.FindStringSubmatch(line)
	if name == nil {
		return false
	}
	// A block or raw element's name opens its kind of block only where the name ends at a blank,
	// the line's end, `>`, or for a block element `/>`.
	lower, after := strings.ToLower(name[1]), line[len(name[0]):]
	ends := blankOrEnd(after) || strings.HasPrefix(after, ">")
	if blockHTMLNames[lower] && (ends || strings.HasPrefix(after, "/>")) || rawHTMLNames[lower] && rest[0] != '/' && ends {
		return true
	}
	return lazy && completeTagLine.MatchString(line)
}
