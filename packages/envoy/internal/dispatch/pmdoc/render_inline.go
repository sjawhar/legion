package pmdoc

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// inlineContext is where one run of inline nodes is written.
type inlineContext struct {
	// tableCell reports whether the run is a table cell, whose pipes are syntax.
	tableCell bool
	// startsLine reports whether the run begins a markdown line of its own, so that a marker at
	// the start of its text would open a block.
	startsLine bool
	// heading reports whether the run is a heading's text, whose trailing run of `#` would read
	// as the heading's closing sequence.
	heading bool
}

// inline writes a paragraph's children, which begin their own markdown line.
func (r *renderer) inline(nodes []*Node, prefix string) {
	r.inlineWithEscapes(nodes, prefix, inlineContext{startsLine: true})
}

// headingInline writes a heading's children. The `## ` is already on the line, so nothing in the
// text can open a block of its own.
func (r *renderer) headingInline(nodes []*Node, prefix string) {
	r.inlineWithEscapes(nodes, prefix, inlineContext{heading: true})
}

// tableCellInline writes one cell, which sits after a `|` on a line the table owns.
func (r *renderer) tableCellInline(nodes []*Node, prefix string) {
	r.inlineWithEscapes(nodes, prefix, inlineContext{tableCell: true})
}

// inlinePosition is where the inline writer stands inside one textblock.
type inlinePosition struct {
	// atLineStart reports whether the next text character is the first on a markdown line of its
	// own, after any mark's marker, so that the line may read as a block. A heading's or a table
	// cell's text does not begin one.
	atLineStart bool
	// atTextStart reports whether the next character starts an inline run's text or a new line in
	// it, where the writer has always escaped a list, heading, quote or ordered-list marker - in a
	// heading, a cell and inside a mark too. Those escapes read back, and dropping them would
	// rewrite every stored document that carries one, so they stay exactly where they were.
	atTextStart bool
	// afterLine reports whether any line precedes this one in the textblock. A setext underline
	// needs a line to underline, and it may have ended at a newline in an earlier text node or at
	// a hard break, neither of which the current node's own offsets can show.
	afterLine bool
	// afterMarker reports whether a mark's marker is written on this line before its text.
	afterMarker bool
}

// lineCandidate is a line's first text character, when it is one that can begin a block form
// only the whole line decides: the writer writes it unescaped, finishes the line, and then asks
// the parser (endLine).
type lineCandidate struct {
	// at is where the character is written in the markdown.
	at int
	// char is the character.
	char rune
	// afterLine reports whether the line continues its textblock, so the line before is read
	// with it.
	afterLine bool
	// firstOnLine reports whether the character is the first the line writes after its prefix, no
	// mark's marker, escaped indentation or task item's checkbox before it, where the engine opens
	// an HTML block at a `<` it starts (htmlBlockStart). Both parsers read the checkbox inside the
	// item's paragraph, after its blocks are decided, so no HTML block opens after it on its line.
	firstOnLine bool
	// closesTyped reports whether a lone `:::` line of the textblock is escaped as one that could
	// close the typed block around it: the textblock is written at the typed block's own prefix, or,
	// in a block fenced with three colons, less than four columns past it with no quote marker
	// between (typedFenceReach).
	closesTyped bool
	// prefix is the prefix the textblock's lines are written at.
	prefix string
}

// closesAfterPunctuation reports whether the markdown written so far ends in a delimiter run with
// whitespace or punctuation before it, or nothing: a run that closes only before one of them.
func (r *renderer) closesAfterPunctuation() bool {
	written := r.b.Bytes()
	start := len(written)
	for start > 0 && (written[start-1] == '*' || written[start-1] == '~') {
		start--
	}
	if start == len(written) {
		return false
	}
	before, _ := utf8.DecodeLastRune(written[:start])
	return start == 0 || flankingNeutral(before)
}

// opensBeforePunctuation reports whether the text after nodes[index], written under next, opens a
// delimiter run with nothing closed before it and whitespace or punctuation after it - the text's
// own first character, or a link's or code span's syntax opened inside the run: a run that opens
// only after one of them.
func opensBeforePunctuation(nodes []*Node, index int, next, following []Mark) bool {
	if index+1 >= len(nodes) || nodes[index+1].Type != "text" || sharedMarks(next, following) != len(next) || len(following) == len(next) || markDelimiter(following[len(next)]) == 0 {
		return false
	}
	for _, mark := range following[len(next)+1:] {
		if markDelimiter(mark) == 0 {
			return true
		}
	}
	return flankingNeutral(firstRune(nodes[index+1].Text))
}

// opensLabel reports whether a `[` is written right after a text written under marks, the node
// after it, after, written under following: a footnote reference's after a text no mark closes
// after, or a link's opened before any other mark of following with none of marks closed.
func opensLabel(marks, following []Mark, after *Node) bool {
	if after.Type == "footnote_reference" {
		return len(marks) == 0
	}
	shared := sharedMarks(marks, following)
	return shared == len(marks) && shared < len(following) && following[shared].Type == "link"
}

// wwwMayFollow reports whether the markdown written so far ends where the browser editor's engine,
// and Go with it, reads a www URL from bare text: at a line's head or after whitespace,
// punctuation or a symbol, where its tokenizer begins one (literalURLFollows) or its transform finds
// one in the decoded text (transformBoundary). After a letter or a digit neither reads one, so a www
// link is written as an explicit link there.
func (r *renderer) wwwMayFollow() bool {
	written := r.b.Bytes()
	if len(written) == 0 {
		return true
	}
	char, _ := utf8.DecodeLastRune(written)
	return transformBoundary(char)
}

// writeInlineRun writes nodes in spelling, reporting whether it fused two delimiter runs: closed a
// mark and at once opened one written with the same delimiter character, which the parser reads as
// one run.
//
// A text `<` the escape rules leave as it is (angleConstruct judges it within its own node) can
// still open an autolink or inline raw HTML with what the run writes after it, out to a `>` in a
// later node: the text after an address, which the readers split off from the text before it, or
// a link's syntax (`&lt;javascript:void(a@b.cc)>` written `<javascript:void([a@b.cc](mailto:a@b.cc))>`
// reads as one autolink to a `javascript:` address, `x &lt;!-- a@b.cc --> y` written
// `x <!-- [a@b.cc](mailto:a@b.cc) --> y` as one HTML comment). So the run is written, and where
// the markdown it wrote opens an autolink (autolinkAt) or raw HTML (rawHTMLScan) at such a `<`, it
// is written again with each of those escaped. That markdown reads back as no run that holds the
// `<` as text, so a run that reads back keeps its bytes; and escaping a `<` changes neither at
// another: an autolink stops at any `<`, and the backslash comes before a `<`, which no closer of
// raw HTML holds and a tag holds only inside a quoted value.
//
// A spelling that orders marks otherwise than as written is never kept once it fuses a delimiter
// run, so its writing is thrown away here and in the search (inlineWithEscapes).
func (r *renderer) writeInlineRun(nodes []*Node, prefix string, context inlineContext, spelling runSpelling) (fused bool) {
	from := r.b.Len()
	r.openers = r.openers[:0]
	fused = r.writeInlineNodes(nodes, prefix, context, spelling)
	if r.err != nil || fused && spelling.order != marksAsWritten {
		return fused
	}
	written := r.b.Bytes()
	var escaped []angleOpener
	html := rawHTMLScan{written: written, from: from, prefix: prefix}
	for _, opener := range r.openers {
		if written[opener.at] == '<' && (autolinkAt(written[opener.at+1:]) || html.opensAt(opener.at)) {
			escaped = append(escaped, opener)
		}
	}
	if len(escaped) == 0 {
		return fused
	}
	r.b.Truncate(from)
	r.escapedOpeners = escaped
	fused = r.writeInlineNodes(nodes, prefix, context, spelling)
	r.escapedOpeners = nil
	return fused
}

// angleOpener is a text `<` written as it is: where it stands in the markdown, and the node and
// offset in its text it is written from.
type angleOpener struct {
	at     int
	node   *Node
	offset int
}

// autolinkAt reports whether markdown written after a `<` closes it into an autolink, as goldmark
// reads one and CommonMark defines it: an email address or a scheme and its URL, then the `>`.
// Neither holds whitespace, a control character, `<` or `>`, so only the bytes before the first of
// those are read, which a `<` after this one bounds.
func autolinkAt(after []byte) bool {
	end := 0
	for end < len(after) && after[end] > ' ' && after[end] != '<' && after[end] != '>' && after[end] != 0x7f {
		end++
	}
	if end == 0 || end == len(after) || after[end] != '>' {
		return false
	}
	address := after[:end]
	return util.FindURLIndex(address) == end || util.FindEmailIndex(address) == end
}

// rawHTMLParser is the package's inline raw HTML parser (inlineRawHTMLParser), which every reader
// of Parse's reads raw HTML with (inlineParsers), so the writer's scan and the readers read a
// comment, a processing instruction, a declaration, a CDATA section and a closing tag by the one
// parser; an open tag the scan matches against openTagPattern instead.
var rawHTMLParser = inlineRawHTMLParser{parser.NewRawHTMLParser()}

// openTagPattern is an open tag as CommonMark 0.31 and the browser editor's engine (micromark's
// html-text) read one, which is goldmark's pattern with CommonMark's unquoted attribute value: one
// that holds no space, tab, line ending, `"`, `'`, `=`, `<`, `>` or backtick, though it may hold a
// control character goldmark's does not. Every tag goldmark reads matches it.
var openTagPattern = regexp.MustCompile("^<[A-Za-z][A-Za-z0-9-]*" +
	`(?:[\r\n \t]+[a-zA-Z_:][a-zA-Z0-9:._-]*(?:[\r\n \t]*=[\r\n \t]*(?:[^"'=<>` + "`" + `\t\n\r ]+|'[^']*'|"[^"]*"))?)*` +
	`(?:[ \t]|(?:\r\n|\n){0,1})*/?>`)

// htmlEndUnasked and htmlEndNone are what a kind's entry in rawHTMLScan.ends holds beside the end
// of a construct: before any ask of that kind, and where the last ask found no closer. The end of
// a construct is never 0.
const (
	htmlEndUnasked = 0
	htmlEndNone    = -1
)

// rawHTMLScan reports whether the markdown a run wrote, from its start at from to the end of
// written, opens inline raw HTML at each text `<` (opensAt), in the order they are written, as
// either goldmark (rawHTMLParser) or the browser editor's engine would read it: an escaped `<`
// reads as text in both, so one that only one of them reads as raw HTML is escaped too. goldmark
// reads less as raw HTML than CommonMark 0.31 and the engine do, so it is asked only for a closing
// tag, a comment, a processing instruction and a CDATA section, which it reads as they do or
// more; an open tag is matched against CommonMark's (openTagPattern), and a declaration, `<!` and
// any ASCII letter where goldmark reads one only after an uppercase letter, is read by the parser
// the readers read one with. It reads the run's lines as the readers do, each later line without
// the prefix written after the line feed (as readsBackUnder takes it off), so a `>` of a quote
// marker closes nothing.
//
// An open or closing tag reads past a `<` only inside a quoted attribute value, which ends at the
// next quote of its kind, so no more than one asked tag is read at any byte per kind of quote, and
// one outside both. A comment, processing instruction, declaration or CDATA section ends at the
// first of its closer (`-->`, `?>`, `>`, `]]>`) after its `<`, which is searched for out to the
// run's end, so one is asked only where the last of its kind asked does not answer for it: a
// later `<` of that kind before the end of the construct that one found ends at the same closer,
// and none after one that found no closer finds one. That record of its own asks (ends) is the
// one that answers for a run, so the parser is given no context and keeps none for the scan
// (unclosedRawHTMLOf). Each byte is so read for at most one of each kind, and the run's scan is
// linear.
type rawHTMLScan struct {
	written []byte
	from    int
	prefix  string
	// lines are the run's lines the reader reads, made at the first ask, the run's lines being
	// one block to the parser; line is the one the last `<` asked about is on.
	lines  text.Segments
	reader text.BlockReader
	line   int
	// ends is, for each kind rawHTMLCloser tells apart, where the construct its last ask found
	// ends, htmlEndNone where that ask found none, and htmlEndUnasked before any ask.
	ends [htmlCloserKinds]int
}

// opensAt reports whether the markdown written from the `<` at at reads as inline raw HTML to
// goldmark or to the engine; at is after the `<` asked about before.
func (s *rawHTMLScan) opensAt(at int) bool {
	if at+1 >= len(s.written) || !opensAngleConstruct(s.written[at+1]) {
		return false
	}
	if s.reader == nil {
		s.readLines()
	}
	for s.lines.At(s.line).Stop <= at {
		s.line++
	}
	s.reader.SetPosition(s.line, text.NewSegment(at, s.lines.At(s.line).Stop))
	line, _ := s.reader.PeekLine()
	kind := rawHTMLCloser(line)
	if kind != htmlNoCloser {
		switch end := s.ends[kind]; {
		case end == htmlEndNone:
			return false
		case end != htmlEndUnasked && at < end:
			// The `<` is before that construct's closer, which holds no `<`.
			return true
		}
	}
	if isASCIILetter(s.written[at+1]) {
		return s.reader.Match(openTagPattern)
	}
	// The parser is asked for the lines of this one run, whose asks the scan records itself, so
	// it is given neither a parent nor a context to record them in.
	html := rawHTMLParser.Parse(nil, s.reader, nil)
	if kind != htmlNoCloser {
		s.ends[kind] = htmlEndNone
		if html != nil {
			segments := html.(*ast.RawHTML).Segments
			s.ends[kind] = segments.At(segments.Len() - 1).Stop
		}
	}
	return html != nil
}

// readLines makes the run's lines and a reader over them.
func (s *rawHTMLScan) readLines() {
	for start := s.from; start < len(s.written); {
		stop, next := blockLine(s.written, start, s.prefix)
		s.lines.Append(text.NewSegment(start, stop))
		start = next
	}
	s.reader = text.NewBlockReader(s.written, &s.lines)
}

// blockLine is the line of written that starts at start: where it stops, after its line feed or
// at the end of written, and where the next line's own content begins, past the prefix the
// textblock's later lines are written behind. Both readers of a run's lines take that prefix off
// this way, since the readers read the run without it (readsBackUnder, rawHTMLScan).
func blockLine(written []byte, start int, prefix string) (stop, next int) {
	end := bytes.IndexByte(written[start:], '\n')
	if end < 0 {
		return len(written), len(written)
	}
	stop = start + end + 1
	next = stop
	if len(written)-next >= len(prefix) && string(written[next:next+len(prefix)]) == prefix {
		next += len(prefix)
	}
	return stop, next
}

// escapesOpener reports whether the text `<` at offset in node is the next of the openers the run's
// writing escapes (writeInlineRun), which the writing reaches in the order it recorded them.
func (r *renderer) escapesOpener(node *Node, offset int) bool {
	if len(r.escapedOpeners) == 0 || r.escapedOpeners[0].node != node || r.escapedOpeners[0].offset != offset {
		return false
	}
	r.escapedOpeners = r.escapedOpeners[1:]
	return true
}

// writeInlineNodes writes nodes in spelling for writeInlineRun, recording each text `<` it writes
// as it is (angleOpener).
func (r *renderer) writeInlineNodes(nodes []*Node, prefix string, context inlineContext, spelling runSpelling) (fused bool) {
	escapePipes := context.tableCell
	var active []Mark
	position := inlinePosition{atLineStart: context.startsLine, atTextStart: true}
	for index, n := range nodes {
		if r.err != nil {
			return fused
		}
		switch n.Type {
		case "text":
			wwwBare := !spelling.angleURLs
			// A www link is written bare only where the character written before it is one a www
			// URL may follow (wwwMayFollow); elsewhere its marks are written again with the link.
			// Every delimiter a mark closes or opens with ends in punctuation, a symbol or a space,
			// which a www URL may follow, so the link is written again only where no mark closed or
			// opened before it: the first writing wrote nothing and changed no state to take back.
			bareHere := wwwBare
			var next, following []Mark
			var common int
			var flankFirst bool
			for {
				next, following = orderMarks(active, spelledMarks(n, escapePipes, bareHere, spelling), nodes, index, escapePipes, wwwBare, spelling)
				common = sharedMarks(active, next)
				for i := len(active) - 1; i >= common; i-- {
					r.closeInlineMark(active[i], escapePipes)
				}
				if common < len(active) && common < len(next) && markDelimiter(active[common]) != 0 && markDelimiter(active[common]) == markDelimiter(next[common]) {
					fused = true
				}
				// A delimiter run the marks just closed, with nothing opened after it, stands
				// before this text's first character.
				flankFirst = spelling.flanking && len(active) != common && len(next) == common && r.closesAfterPunctuation() && !flankingNeutral(firstRune(n.Text))
				for _, mark := range next[common:] {
					r.openInlineMark(mark, nodes, index)
				}
				if position.atLineStart && (len(active) != common || len(next) != common) {
					position.afterMarker = true
				}
				if !bareHere || !isBareWWWLink(n, visibleMarks(n.Marks), escapePipes) || r.wwwMayFollow() {
					break
				}
				bareHere = false
			}
			marks := visibleMarks(n.Marks)
			// An email link the spelling writes as its address is text outside a link to the escapes.
			hasLink := containsMark(marks, "link") && !(spelling.bareEmails && isBareEmailLink(n, marks))
			active = next
			if isAngleURLLink(n, marks, escapePipes) || spelling.angleURLs && isBareURLLink(n, marks, escapePipes) {
				// An autolink's text is literal to both parsers, so it takes no escape.
				r.writeSyntax("<" + n.Text + ">")
				position.atLineStart, position.atTextStart, position.afterMarker = false, false, false
				continue
			}
			// bareURL reports whether this text is a URL the tokenizer reads whole, which is
			// written as its bytes unless the spelling writes it as text with its scheme colon
			// escaped (bareURLColons): the tokenizer then reads no URL there, so a delimiter run
			// written after it stays a delimiter run rather than being read on into the URL, and
			// the transform, which reads the decoded text, links the URL again.
			bareURL := isBareURLLink(n, marks, escapePipes) && literalLength(n.Text) != 0
			if bareURL && !spelling.bareURLColons || bareHere && isBareWWWLink(n, marks, escapePipes) {
				// The tokenizer reads a bare URL's bytes as they are written, a backslash among
				// them, so its text takes no escape either. A www URL begins only after some
				// characters; where its run does not read back, the angleURLs spelling writes it as
				// an explicit link.
				r.writeSyntax(n.Text)
				position.atLineStart, position.atTextStart, position.afterMarker = false, false, false
				continue
			}
			flankLast := spelling.flanking && opensBeforePunctuation(nodes, index, next, following) && !flankingNeutral(lastRune(n.Text))
			// The brackets rule follows the marks actually written, not hasLink: an autolink's
			// link mark is stripped above. The verdict itself was taken when the link opened.
			label := labelBracketsNone
			if containsMark(next, "link") {
				label = r.labelBrackets
			}
			r.writeInlineText(n, &position, prefix, escapeContext{
				footnoteLabels: r.footnoteLabels,
				tableCell:      escapePipes,
				schemeColons:   spelling.schemeColons && !hasLink || bareURL,
				wwwDots:        spelling.wwwDots && !hasLink,
				ats:            spelling.ats && !hasLink,
				label:          label,
				followed:       index+1 < len(nodes) || len(next) > 0,
				beforeLabel:    index+1 < len(nodes) && opensLabel(next, following, nodes[index+1]),
				delimiters:     spelling.escapes,
				heading:        context.heading,
				marked:         len(next) > 0,
				opener:         adjacentDelimiter(next[common:]),
				closer:         adjacentDelimiter(next[sharedMarks(next, following):]),
				flankFirst:     flankFirst,
				flankLast:      flankLast,
				pairBackslash:  spelling.pairBackslash,
				tildeBackslash: spelling.tildeBackslash,
			})
		case "hardbreak":
			r.closeMarks(active, escapePipes)
			active = nil
			// A hard break is a backslash or two spaces before the newline. After text that
			// already ends in a backslash the first form reads as one escaped backslash and a
			// soft break, losing the line break, so the break is written as two spaces there.
			if r.endsWith('\\') {
				r.writeSyntax("  ")
			} else {
				r.writeSyntax("\\")
			}
			r.endLine(true)
			r.writeSyntax("\n" + prefix)
			position = inlinePosition{atLineStart: true, atTextStart: true, afterLine: true}
		case "image":
			r.closeMarks(active, escapePipes)
			active = nil
			src, _ := n.Attrs["src"].(string)
			alt, _ := n.Attrs["alt"].(string)
			title, _ := n.Attrs["title"].(string)
			r.writeSyntax("![" + imageAlt(alt, context) + "](" + escapeImageSource(src, escapePipes) + titleSuffix(title, escapePipes) + ")")
			position.atLineStart, position.atTextStart = false, false
		case "html":
			r.closeMarks(active, escapePipes)
			active = nil
			value, _ := n.Attrs["value"].(string)
			r.writeSyntax(escapeTableHTMLPipes(value, escapePipes))
			position.atLineStart, position.atTextStart = false, false
		case "footnote_reference":
			r.closeMarks(active, escapePipes)
			active = nil
			label, _ := n.Attrs["label"].(string)
			r.writeSyntax("[^" + escapeFootnoteLabel(label) + "]")
			position.atLineStart, position.atTextStart = false, false
		default:
			r.err = fmt.Errorf("%w: cannot render inline %q", ErrSchema, n.Type)
		}
	}
	r.closeMarks(active, escapePipes)
	r.endLine(false)
	return fused
}

// endLine judges the line just written when its text began with a lineCandidate: the character is
// escaped when the parser reads the line, markers and hard break included, as something other than
// it reads with the character escaped (lineReadsAsText), or when it is a `<` the engine opens an
// HTML block at (htmlBlockStart), as written out to the line's end across the run's later nodes,
// which goldmark can read as a paragraph. A lone `:::` at the prefix of the typed block around it
// closes that block, which the line read on its own cannot show. A line the textblock goes on past,
// at a line ending in its text or a hard break (continues), is judged with a line of text after
// it: a task marker opens a list item only where the paragraph goes on.
func (r *renderer) endLine(continues bool) {
	candidate := r.heldLineStart
	if candidate == nil {
		return
	}
	r.heldLineStart = nil
	written := r.b.Bytes()
	lineFrom := markdownLineStart(written, candidate.at)
	line := string(written[lineFrom:])
	width := utf8.RuneLen(candidate.char)
	escape := escaped(candidate.char)
	if !candidate.closesTyped || !closesTypedBlock(line, candidate.prefix) {
		readFrom, before := lineFrom, ""
		if candidate.afterLine && lineFrom > 0 {
			readFrom = markdownLineStart(written, lineFrom-1)
			before = string(written[readFrom:lineFrom])
		}
		offset := candidate.at - lineFrom
		judged, rewritten := line, line[:offset]+escape+line[offset+width:]
		if continues {
			judged, rewritten = judged+"\nx", rewritten+"\nx"
		}
		var footnote string
		if readFrom == r.footnoteLineAt && r.footnoteLabel != "" {
			footnote = r.footnoteLabel
		}
		htmlBlock := candidate.char == '<' && candidate.firstOnLine && htmlBlockStart(line[offset:], candidate.afterLine)
		if !htmlBlock && lineReadsAsText(before, judged, rewritten, candidate.prefix, footnote) {
			return
		}
	}
	tail := line[candidate.at-lineFrom+width:]
	r.b.Truncate(candidate.at)
	r.b.WriteString(escape)
	r.b.WriteString(tail)
	// The openers after the escaped character are on its line, the last ones recorded.
	for index := len(r.openers) - 1; index >= 0 && r.openers[index].at > candidate.at; index-- {
		r.openers[index].at += len(escape) - width
	}
}

func (r *renderer) closeMarks(marks []Mark, escapePipes bool) {
	for i := len(marks) - 1; i >= 0; i-- {
		r.closeInlineMark(marks[i], escapePipes)
	}
}

func (r *renderer) openInlineMark(mark Mark, nodes []*Node, index int) {
	if mark.Type == "link" {
		r.labelBrackets = linkLabelBrackets(linkLabel(nodes, index, mark))
	}
	if mark.Type != "inlineCode" {
		r.writeSyntax(openMark(mark))
		return
	}
	r.inlineCodeFence, r.inlineCodePadded = inlineCodeFence(nodes, index)
	r.writeSyntax(r.inlineCodeFence)
	if r.inlineCodePadded {
		r.writeSyntax(" ")
	}
}

func (r *renderer) closeInlineMark(mark Mark, escapePipes bool) {
	if mark.Type != "inlineCode" {
		r.writeSyntax(closeMark(mark, escapePipes))
		return
	}
	if r.inlineCodePadded {
		r.writeSyntax(" ")
	}
	r.writeSyntax(r.inlineCodeFence)
	r.inlineCodeFence = ""
	r.inlineCodePadded = false
}

func inlineCodeFence(nodes []*Node, index int) (string, bool) {
	var content strings.Builder
	for _, node := range nodes[index:] {
		if node.Type != "text" || !nodeHasMark(node, "inlineCode") {
			break
		}
		content.WriteString(node.Text)
	}
	value := content.String()
	return strings.Repeat("`", longestRun(value, '`')+1), inlineCodePadding(value)
}

// inlineCodePadding reports whether a code span's text is written with a space inside each
// backtick fence: text that begins or ends with a backtick, and text the parser would take a
// space or a line ending off each end of (codeSpanPadded).
func inlineCodePadding(value string) bool {
	if strings.HasPrefix(value, "`") || strings.HasSuffix(value, "`") {
		return true
	}
	return codeSpanPadded(value)
}

// takesCodeLineIndent reports whether the containers around a code span would take columns off the
// whitespace its later line, the first of rest, opens with, were the line written as it is, without
// their prefix, as it is everywhere else: outermost first, each list item and footnote definition
// takes its own columns while that whitespace reaches them, until a quote, which the line does not
// continue, so the rest of the line is the paragraph's, whitespace and all. Where one would, the
// line is written behind the prefix, which the containers take instead.
func (r *renderer) takesCodeLineIndent(rest string) bool {
	whitespace := len(rest) - len(strings.TrimLeft(rest, " \t"))
	columns, taken, from := columnOf([]byte(rest[:whitespace])), 0, 0
	for _, container := range r.scopes {
		width := len(container.prefix) - from
		from = len(container.prefix)
		switch container.node.Type {
		case "blockquote":
			return taken > 0
		case "list_item", "footnote_definition":
			if columns-taken < width {
				return taken > 0
			}
			taken += width
		}
	}
	return taken > 0
}

// codeLineOpensHTML reports whether a code span's later line, the first of rest, holds one
// complete HTML tag and nothing else, which written without its containers' prefix would be a
// lazy continuation line holding a block of HTML of kind 7 (htmlFlowKind). CommonMark lets no
// block of that kind interrupt a paragraph, and goldmark and the browser editor's copy of the
// construct both obey that on a lazy line as on any other
// (packages/proof-editor/src/html-flow.ts), so either reader reads the line as the paragraph's
// and the code's with the prefix or without it. Upstream micromark opens a block there, as the
// browser's engine did before this change, and such a reader ends the paragraph, the containers
// around it and the code with it: writing the prefix makes the stored bytes read the same there
// too, at no cost here, since the containers take the prefix off the code's text again. A line
// the span's closing backticks end holds them after the tag, which opens no block.
func codeLineOpensHTML(rest string) bool {
	line, _, whole := strings.Cut(rest, "\n")
	if !whole || !strings.HasPrefix(strings.TrimLeft(line, " \t"), "<") {
		return false
	}
	return htmlFlowKind(line, 0) == 7
}

func (r *renderer) writeSyntax(value string) {
	r.b.WriteString(value)
}

// endsWith reports whether the markdown written so far ends with char.
func (r *renderer) endsWith(char byte) bool {
	rendered := r.b.Bytes()
	return len(rendered) > 0 && rendered[len(rendered)-1] == char
}

func (r *renderer) writeText(value string) {
	r.b.WriteString(value)
}

func (r *renderer) writeInlineText(node *Node, position *inlinePosition, prefix string, context escapeContext) {
	if nodeHasMark(node, "inlineCode") {
		// A code span's text is written as it is; a line feed in it still ends a line. In a table
		// cell each pipe is written `\|`, which both parsers read back as the code's pipe. Code
		// holding an odd run of backslashes before a pipe has no spelling there, and tableRow takes
		// each such pipe out of the code before it reaches here (codePipesOutOfCode).
		value := escapeTablePipes(node.Text, context.tableCell)
		endsLine := false
		for {
			lineEnd := strings.IndexByte(value, '\n')
			if lineEnd < 0 {
				break
			}
			r.writeText(value[:lineEnd])
			r.endLine(true)
			r.writeText(value[lineEnd : lineEnd+1])
			value = value[lineEnd+1:]
			if r.takesCodeLineIndent(value) || codeLineOpensHTML(value) {
				r.writeSyntax(prefix)
			}
			endsLine, position.afterLine = value == "", true
		}
		r.writeText(value)
		position.atLineStart = endsLine
		position.atTextStart = position.atLineStart
		return
	}

	value := node.Text
	context.scan = newForwardScan(value)
	segmentStart := 0
	// Where the current line's text begins inside this node, or -1 when it began in an earlier
	// one: lineStart for a line of its own whose first character is yet to be judged, and
	// textLineStart for the writer's long-standing escapes.
	lineStart, textLineStart := -1, -1
	if position.atLineStart {
		lineStart = 0
	}
	if position.atTextStart {
		textLineStart = 0
	}
	for byteOffset, char := range value {
		width := utf8.RuneLen(char)
		context.textLineStart = textLineStart
		context.afterMarker = position.afterMarker
		escape := needsInlineEscape(value, byteOffset, char, context) || char == '<' && r.escapesOpener(node, byteOffset)
		if lineStart >= 0 && r.heldLineStart == nil {
			switch lineStartOf(value, lineStart, byteOffset, char, escape) {
			case lineStartEscaped:
				escape = true
				lineStart = -1
			case lineStartHeld:
				r.holdLineStart(value[segmentStart:byteOffset], char, position, prefix, byteOffset == lineStart)
				segmentStart = byteOffset
				lineStart = -1
			case lineStartAsIs:
				lineStart = -1
			}
		}
		flank := byteOffset == 0 && context.flankFirst || byteOffset+width == len(value) && context.flankLast
		if flank {
			// A character reference is punctuation to the flanking rules, where the letter or
			// digit it stands for keeps the delimiter run beside it from opening or closing. A
			// literal backslash written right before it escapes its `&`, so that the writing never
			// reads back, unless the spelling writes that backslash `\\` (pairBackslash).
			r.writeText(value[segmentStart:byteOffset])
			if context.pairBackslash && escapedByBackslashes(r.b.Bytes(), r.b.Len()) {
				r.writeSyntax(`\`)
			}
			r.writeSyntax(numericEntity(char))
			segmentStart = byteOffset + width
		} else if escape {
			r.writeText(value[segmentStart:byteOffset])
			r.writeSyntax(textEscape(char, context.tildeBackslash))
			segmentStart = byteOffset + width
		} else if char == '<' {
			r.openers = append(r.openers, angleOpener{at: r.b.Len() + byteOffset - segmentStart, node: node, offset: byteOffset})
		}
		lineEnd := char == '\n'
		if lineEnd {
			r.writeText(value[segmentStart:byteOffset])
			segmentStart = byteOffset
			r.endLine(true)
			lineStart, textLineStart = byteOffset+width, byteOffset+width
			position.afterLine = true
		}
		position.atLineStart = lineEnd
		position.atTextStart = lineEnd
		position.afterMarker = false
	}
	r.writeText(value[segmentStart:])
}

// markdownLineStart is where the markdown line holding offset begins in written: after the last
// line feed before it.
func markdownLineStart(written []byte, offset int) int {
	for index := offset - 1; index >= 0; index-- {
		if written[index] == '\n' {
			return index + 1
		}
	}
	return 0
}

// holdLineStart writes the text before a line's first character and holds that character for
// endLine to judge once the line is written. first reports whether no text of the line comes before
// it.
func (r *renderer) holdLineStart(before string, char rune, position *inlinePosition, prefix string, first bool) {
	r.writeText(before)
	typed := r.scope().typed
	r.heldLineStart = &lineCandidate{
		at:          r.b.Len(),
		char:        char,
		afterLine:   position.afterLine,
		firstOnLine: first && !position.afterMarker && r.checkboxEnd <= markdownLineStart(r.b.Bytes(), r.b.Len()),
		closesTyped: typed != nil && (typed.prefix == prefix || typed.colons == 3 && typedFenceReach(typed.prefix, prefix)),
		prefix:      prefix,
	}
}
