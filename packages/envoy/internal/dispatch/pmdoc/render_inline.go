package pmdoc

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

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
	// closesTyped reports whether a lone `:::` line of the textblock is escaped as one that could
	// close the typed block around it: the textblock is written at the typed block's own prefix, or,
	// in a block fenced with three colons, less than four columns past it with no quote marker
	// between (typedFenceReach).
	closesTyped bool
	// prefix is the prefix the textblock's lines are written at.
	prefix string
}

// inlineWithEscapes writes one textblock's inline nodes. Delimiters in text the escape rules leave
// alone can still pair into a mark the text never had, or break one it has: GFM reads a single
// tilde as a strikethrough delimiter and refuses a run of three, and asterisks and underscores pair
// across a hard break, around a reference or inside a link label. Linkify continues a bare URL
// into the text written right after it, which a backslash escape had stopped as written. A mark's
// delimiter run beside punctuation inside it and a letter outside it opens or closes nothing
// (`**Which here?**Careful.`). A run whose text holds any of these is read back after it is
// written, and when it does not come back as written it is written again in each spelling
// respellings lists, keeping the first that reads back, so a run that fails for another reason
// keeps the bytes it had.
//
// And the marks are written in one order, so a mark the next text still carries is closed and
// opened again inside that text's other marks wherever the order puts it after them: bold inside
// italic comes out as `*`, `**` and `*` side by side, one run of four asterisks. The parser splits
// such a fused run as written where its flanking allows, so a fused first writing that reads back
// is kept, as main writes it, and one that does not is written again in the plain respellings, as
// main writes it again. Only where none of those reads back is it written with open marks kept
// open (keepingOpen), each such spelling kept only where it reads back with no delimiter run fused.
// Last come those spellings again with the letter beside each such delimiter run written as a
// character reference, which is punctuation to the flanking rules, as the browser editor writes it
// (flanking).
//
// A spelling is taken first where it reads back by CommonMark's flanking rules as well
// (flankingOnlyMarkdown), the rules goldmark reads by, and only where none does by the browser
// editor's alone, which also let a run beside another `*` or `_` open or close (emphasisParser):
// the writer keeps the bytes it wrote while it read by CommonMark's rules, wherever they read back
// by the editor's too.
func (r *renderer) inlineWithEscapes(nodes []*Node, prefix string, context inlineContext) {
	from := r.b.Len()
	fused := r.writeInlineRun(nodes, prefix, context, runSpelling{})
	held := delimitersInText(nodes)
	bareURL := textAfterBareURL(nodes, context.tableCell)
	flanking := delimiterBesidePunctuation(nodes, context.tableCell)
	if r.err != nil || held == delimitersAsRuled && !bareURL && !fused && !flanking {
		return
	}
	firstReadsBack := r.runReadsBack(from, prefix, nodes, inlineMarkdown)
	if firstReadsBack && r.runReadsBack(from, prefix, nodes, flankingOnlyMarkdown) {
		return
	}
	written := string(r.b.Bytes()[from:])
	spellings := respellings(held, bareURL, false)
	if fused {
		spellings = append(spellings, respellings(held, bareURL, true)...)
	}
	if flanking {
		flanked := []runSpelling{{flanking: true}}
		for _, spelling := range spellings {
			spelling.flanking = true
			flanked = append(flanked, spelling)
		}
		spellings = append(spellings, flanked...)
	}
	for _, flankingToo := range []bool{true, false} {
		if !flankingToo && firstReadsBack {
			break
		}
		for _, spelling := range spellings {
			r.b.Truncate(from)
			fusedAgain := r.writeInlineRun(nodes, prefix, context, spelling)
			if r.err == nil && !(spelling.keepOpen && fusedAgain) && r.runReadsBack(from, prefix, nodes, inlineMarkdown) &&
				(!flankingToo || r.runReadsBack(from, prefix, nodes, flankingOnlyMarkdown)) {
				return
			}
		}
	}
	r.b.Truncate(from)
	r.b.WriteString(written)
}

// runSpelling is how writeInlineRun writes a run beyond the escape rules: the delimiters in its
// text it escapes, whether it escapes the character after each bare URL (endsBareURL), whether
// a mark the next text still carries stays open around that text's other marks (keepingOpen), and
// whether a letter beside a delimiter run that punctuation stands inside is a character reference
// (flanking).
type runSpelling struct {
	escapes      delimiterEscapes
	afterBareURL bool
	keepOpen     bool
	flanking     bool
}

// respellings is every spelling, open marks kept open or not, that inlineWithEscapes writes a run
// in after its first writing, in the order it tries them: the delimiter escapes from the fewest,
// then each again with the character after a bare URL escaped.
func respellings(held delimiterEscapes, bareURL, keepOpen bool) []runSpelling {
	escapes := []delimiterEscapes{delimitersAsRuled}
	if held != delimitersAsRuled {
		for next := held; next <= delimitersAll; next++ {
			escapes = append(escapes, next)
		}
	}
	var spellings []runSpelling
	for _, afterBareURL := range []bool{false, true} {
		if afterBareURL && !bareURL {
			continue
		}
		for _, delimiters := range escapes {
			spelling := runSpelling{escapes: delimiters, afterBareURL: afterBareURL, keepOpen: keepOpen}
			if spelling != (runSpelling{}) {
				spellings = append(spellings, spelling)
			}
		}
	}
	return spellings
}

// keepingOpen orders marks, the marks a text is written under, so that no mark it or the text
// after it still carries is closed and opened again between them: the longest run of active, the
// marks written open before it, from the outermost, whose every mark it carries comes first, as
// active holds them; then its other marks that following, the next text's marks, carries; then
// the rest, each group in its written order, opened inside the one before.
func keepingOpen(active, marks, following []Mark) []Mark {
	kept := 0
	for kept < len(active) && containsSameMark(marks, active[kept]) {
		kept++
	}
	ordered := append(make([]Mark, 0, len(marks)), active[:kept]...)
	for _, carried := range []bool{true, false} {
		for _, mark := range marks {
			if !containsSameMark(active[:kept], mark) && containsSameMark(following, mark) == carried {
				ordered = append(ordered, mark)
			}
		}
	}
	return ordered
}

// textAfterBareURL reports whether nodes write text right after a bare URL, which linkify can
// continue into that text's first character.
func textAfterBareURL(nodes []*Node, escapePipes bool) bool {
	for index := 1; index < len(nodes); index++ {
		if followsBareURL(nodes, index, escapePipes) {
			return true
		}
	}
	return false
}

// followsBareURL reports whether nodes[index] is text outside any link written right after a bare
// URL (isBareURLLink), whose link linkify reads again from the text alone.
func followsBareURL(nodes []*Node, index int, escapePipes bool) bool {
	if index == 0 || nodes[index].Type != "text" || nodeHasMark(nodes[index], "link") {
		return false
	}
	previous := nodes[index-1]
	return previous.Type == "text" && nodeHasMark(previous, "link") && isBareURLLink(previous, visibleMarks(previous.Marks), escapePipes)
}

// runReadsBack reports whether the inline markdown written since from reads back as nodes by
// reader: the run's own lines, with the prefix its later lines are written behind taken off, read
// after a definition for each footnote label it refers to, since a reference reads as one only
// then.
func (r *renderer) runReadsBack(from int, prefix string, nodes []*Node, reader inlineReader) bool {
	source := string(r.b.Bytes()[from:])
	if prefix != "" {
		source = strings.ReplaceAll(source, "\n"+prefix, "\n")
	}
	parsed, err := parseInlineWithDefinitions(source, referencedLabels(nodes), reader)
	return err == nil && slices.Equal(inlineSignature(parsed), inlineSignature(nodes))
}

// delimiterBesidePunctuation reports whether nodes write a delimiter run (a strong, emphasis or
// strikethrough mark's) with punctuation or whitespace just inside it and text just outside it,
// where the flanking rules can keep the run from opening or closing.
func delimiterBesidePunctuation(nodes []*Node, escapePipes bool) bool {
	for index, node := range nodes {
		if node.Type != "text" || !hasDelimiterMark(writtenMarks(node, escapePipes)) {
			continue
		}
		if index > 0 && nodes[index-1].Type == "text" && flankingNeutral(firstRune(node.Text)) ||
			index+1 < len(nodes) && nodes[index+1].Type == "text" && flankingNeutral(lastRune(node.Text)) {
			return true
		}
	}
	return false
}

func hasDelimiterMark(marks []Mark) bool {
	for _, mark := range marks {
		if markDelimiter(mark) != 0 {
			return true
		}
	}
	return false
}

// flankingNeutral reports whether char is whitespace or punctuation to the flanking rules, as the
// parser classifies it: a delimiter run beside it inside needs another such character outside.
func flankingNeutral(char rune) bool {
	return util.IsPunctRune(char) || util.IsSpaceRune(char)
}

func firstRune(text string) rune {
	char, _ := utf8.DecodeRuneInString(text)
	return char
}

func lastRune(text string) rune {
	char, _ := utf8.DecodeLastRuneInString(text)
	return char
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

// writeInlineRun writes nodes in spelling, reporting whether it fused two delimiter runs: closed a
// mark and at once opened one written with the same delimiter character, which the parser reads as
// one run.
func (r *renderer) writeInlineRun(nodes []*Node, prefix string, context inlineContext, spelling runSpelling) (fused bool) {
	escapePipes := context.tableCell
	var active []Mark
	position := inlinePosition{atLineStart: context.startsLine, atTextStart: true}
	for index, n := range nodes {
		if r.err != nil {
			return fused
		}
		switch n.Type {
		case "text":
			next := writtenMarks(n, escapePipes)
			hasLink := containsMark(visibleMarks(n.Marks), "link")
			var following []Mark
			if index+1 < len(nodes) {
				following = writtenMarks(nodes[index+1], escapePipes)
			}
			if spelling.keepOpen {
				next = keepingOpen(active, next, following)
				if index+1 < len(nodes) {
					var after []Mark
					if index+2 < len(nodes) {
						after = writtenMarks(nodes[index+2], escapePipes)
					}
					following = keepingOpen(next, following, after)
				}
			}
			common := sharedMarks(active, next)
			for i := len(active) - 1; i >= common; i-- {
				r.closeInlineMark(active[i], escapePipes)
			}
			if common < len(active) && common < len(next) && markDelimiter(active[common]) != 0 && markDelimiter(active[common]) == markDelimiter(next[common]) {
				fused = true
			}
			// A delimiter run the marks just closed, with nothing opened after it, stands before
			// this text's first character.
			flankFirst := spelling.flanking && len(active) != common && len(next) == common && r.closesAfterPunctuation() && !flankingNeutral(firstRune(n.Text))
			for _, mark := range next[common:] {
				r.openInlineMark(mark, nodes, index)
			}
			if position.atLineStart && (len(active) != common || len(next) != common) {
				position.afterMarker = true
			}
			active = next
			if isAngleURLLink(n, visibleMarks(n.Marks), escapePipes) {
				// An autolink's text is literal to both parsers, so it takes no escape.
				r.writeSyntax("<" + n.Text + ">")
				position.atLineStart, position.atTextStart, position.afterMarker = false, false, false
				continue
			}
			flankLast := spelling.flanking && opensBeforePunctuation(nodes, index, next, following) && !flankingNeutral(lastRune(n.Text))
			// The brackets rule follows the marks actually written, not hasLink: a bare URL's
			// link mark is stripped above, and its text must keep its own brackets so linkify
			// reads the whole href. The verdict itself was taken when the link opened.
			label := labelBracketsNone
			if containsMark(next, "link") {
				label = r.labelBrackets
			}
			r.writeInlineText(n, &position, prefix, escapeContext{
				footnoteLabels: r.footnoteLabels,
				tableCell:      escapePipes,
				urlSchemes:     !hasLink,
				label:          label,
				followed:       index+1 < len(nodes) || len(next) > 0,
				delimiters:     spelling.escapes,
				afterBareURL:   spelling.afterBareURL && followsBareURL(nodes, index, escapePipes),
				heading:        context.heading,
				marked:         len(next) > 0,
				opener:         adjacentDelimiter(next[common:]),
				closer:         adjacentDelimiter(next[sharedMarks(next, following):]),
				flankFirst:     flankFirst,
				flankLast:      flankLast,
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
			r.writeSyntax("![" + imageAlt(alt, context) + "](" + escapeLinkDestination(src, escapePipes) + titleSuffix(title, escapePipes) + ")")
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
// it reads with the character escaped (lineReadsAsText). A lone `:::` at the prefix of the typed
// block around it closes that block, which the line read on its own cannot show. A line the
// textblock goes on past, at a line ending in its text or a hard break (continues), is judged with
// a line of text after it: a task marker opens a list item only where the paragraph goes on.
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
		if lineReadsAsText(before, judged, rewritten, candidate.prefix, footnote) {
			return
		}
	}
	tail := line[candidate.at-lineFrom+width:]
	r.b.Truncate(candidate.at)
	r.b.WriteString(escape)
	r.b.WriteString(tail)
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
	longest, run := 0, 0
	for _, char := range value {
		if char == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", longest+1), inlineCodePadding(value)
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
		// holding an odd run of backslashes before a pipe has no spelling the browser editor's
		// parser reads back: it removes one backslash before a pipe and ends the cell after an even
		// run, so such code is written as Parse reads it back, and that parser splits the cell there.
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
			if r.takesCodeLineIndent(value) {
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
		escape := needsInlineEscape(value, byteOffset, char, context)
		if lineStart >= 0 && r.heldLineStart == nil {
			switch lineStartOf(value, lineStart, byteOffset, char, escape) {
			case lineStartEscaped:
				escape = true
				lineStart = -1
			case lineStartHeld:
				r.holdLineStart(value[segmentStart:byteOffset], char, position, prefix)
				segmentStart = byteOffset
				lineStart = -1
			case lineStartAsIs:
				lineStart = -1
			}
		}
		flank := byteOffset == 0 && context.flankFirst || byteOffset+width == len(value) && context.flankLast
		if flank {
			// A character reference is punctuation to the flanking rules, where the letter or
			// digit it stands for keeps the delimiter run beside it from opening or closing.
			r.writeText(value[segmentStart:byteOffset])
			r.writeSyntax(numericEntity(char))
			segmentStart = byteOffset + width
		} else if escape {
			r.writeText(value[segmentStart:byteOffset])
			r.writeSyntax(textEscape(char, endsBareURL(byteOffset, char, context)))
			segmentStart = byteOffset + width
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
// endLine to judge once the line is written.
func (r *renderer) holdLineStart(before string, char rune, position *inlinePosition, prefix string) {
	r.writeText(before)
	typed := r.scope().typed
	r.heldLineStart = &lineCandidate{
		at:          r.b.Len(),
		char:        char,
		afterLine:   position.afterLine,
		closesTyped: typed != nil && (typed.prefix == prefix || typed.colons == 3 && typedFenceReach(typed.prefix, prefix)),
		prefix:      prefix,
	}
}
