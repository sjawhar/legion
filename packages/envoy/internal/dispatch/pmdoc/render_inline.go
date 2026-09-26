package pmdoc

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
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
	// atTypedPrefix reports whether the textblock is written at the prefix of the typed block
	// around it, where a lone `:::` line closes that block.
	atTypedPrefix bool
	// prefix is the prefix the textblock's lines are written at.
	prefix string
}

// inlineWithEscapes writes one textblock's inline nodes. Delimiters in text the escape rules leave
// alone can still pair into a mark the text never had, or break one it has: GFM reads a single
// tilde as a strikethrough delimiter and refuses a run of three, and asterisks and underscores pair
// across a hard break, around a reference or inside a link label. A run whose text holds one is
// read back after it is written, and when it does not come back as written it is written again
// with its text's tildes escaped, then with its asterisks and underscores escaped as well - each
// kept only if it reads back, so a run that fails for another reason keeps the bytes it had.
func (r *renderer) inlineWithEscapes(nodes []*Node, prefix string, context inlineContext) {
	from := r.b.Len()
	r.writeInlineRun(nodes, prefix, context, delimitersAsRuled)
	held := delimitersInText(nodes)
	if r.err != nil || held == delimitersAsRuled || r.runReadsBack(from, prefix, nodes) {
		return
	}
	written := string(r.b.Bytes()[from:])
	for escapes := held; escapes <= delimitersAll; escapes++ {
		r.b.Truncate(from)
		r.writeInlineRun(nodes, prefix, context, escapes)
		if r.err == nil && r.runReadsBack(from, prefix, nodes) {
			return
		}
	}
	r.b.Truncate(from)
	r.b.WriteString(written)
}

// runReadsBack reports whether the inline markdown written since from reads back as nodes: the
// run's own lines, with the prefix its later lines are written behind taken off.
func (r *renderer) runReadsBack(from int, prefix string, nodes []*Node) bool {
	source := string(r.b.Bytes()[from:])
	if prefix != "" {
		source = strings.ReplaceAll(source, "\n"+prefix, "\n")
	}
	parsed, err := ParseInline(source)
	return err == nil && slices.Equal(inlineSignature(parsed), inlineSignature(nodes))
}

func (r *renderer) writeInlineRun(nodes []*Node, prefix string, context inlineContext, escapes delimiterEscapes) {
	escapePipes := context.tableCell
	var active []Mark
	position := inlinePosition{atLineStart: context.startsLine, atTextStart: true}
	for index, n := range nodes {
		if r.err != nil {
			return
		}
		switch n.Type {
		case "text":
			next := writtenMarks(n, escapePipes)
			hasLink := containsMark(visibleMarks(n.Marks), "link")
			var following []Mark
			if index+1 < len(nodes) {
				following = writtenMarks(nodes[index+1], escapePipes)
			}
			common := sharedMarks(active, next)
			for i := len(active) - 1; i >= common; i-- {
				r.closeInlineMark(active[i], escapePipes)
			}
			for _, mark := range next[common:] {
				r.openInlineMark(mark, nodes, index)
			}
			if position.atLineStart && (len(active) != common || len(next) != common) {
				position.afterMarker = true
			}
			active = next
			// The brackets rule follows the marks actually written, not hasLink: a bare URL's
			// link mark is stripped above, and its text must keep its own brackets so linkify
			// reads the whole href. The verdict itself was taken when the link opened.
			label := labelBracketsNone
			if containsMark(next, "link") {
				label = r.labelBrackets
			}
			r.writeInlineText(n, &position, prefix, escapeContext{
				tableCell:  escapePipes,
				urlSchemes: !hasLink,
				label:      label,
				followed:   index+1 < len(nodes) || len(next) > 0,
				delimiters: escapes,
				heading:    context.heading,
				marked:     len(next) > 0,
				opener:     adjacentDelimiter(next[common:]),
				closer:     adjacentDelimiter(next[sharedMarks(next, following):]),
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
			r.endLine()
			r.writeSyntax("\n" + prefix)
			position = inlinePosition{atLineStart: true, atTextStart: true, afterLine: true}
		case "image":
			r.closeMarks(active, escapePipes)
			active = nil
			src, _ := n.Attrs["src"].(string)
			alt, _ := n.Attrs["alt"].(string)
			title, _ := n.Attrs["title"].(string)
			r.writeSyntax("![" + imageAlt(alt, escapePipes) + "](" + escapeLinkDestination(src, escapePipes) + titleSuffix(title, escapePipes) + ")")
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
	r.endLine()
}

// endLine judges the line just written when its text began with a lineCandidate: the character is
// escaped when the parser reads the line, markers and hard break included, as something other than
// it reads with the character escaped (lineReadsAsText). A lone `:::` at the prefix of the typed
// block around it closes that block, which the line read on its own cannot show.
func (r *renderer) endLine() {
	candidate := r.heldLineStart
	if candidate == nil {
		return
	}
	r.heldLineStart = nil
	written := r.b.Bytes()
	lineFrom := bytes.LastIndexByte(written[:candidate.at], '\n') + 1
	line := string(written[lineFrom:])
	width := utf8.RuneLen(candidate.char)
	escape := escaped(candidate.char)
	if !candidate.atTypedPrefix || !closesTypedBlock(line, candidate.prefix) {
		readFrom, before := lineFrom, ""
		if candidate.afterLine && lineFrom > 0 {
			readFrom = bytes.LastIndexByte(written[:lineFrom-1], '\n') + 1
			before = string(written[readFrom:lineFrom])
		}
		var footnote string
		if readFrom == r.footnoteLineAt && r.footnoteLabel != "" {
			footnote = r.footnoteLabel
		}
		offset := candidate.at - lineFrom
		if lineReadsAsText(before, line, line[:offset]+escape+line[offset+width:], candidate.prefix, footnote) {
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

func inlineCodePadding(value string) bool {
	if strings.HasPrefix(value, "`") || strings.HasSuffix(value, "`") {
		return true
	}
	return strings.HasPrefix(value, " ") && strings.HasSuffix(value, " ") && strings.Trim(value, " ") != ""
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
		// A code span's text is written as it is; a newline in it still ends a line.
		value := escapeTablePipes(node.Text, context.tableCell)
		for {
			newline := strings.IndexByte(value, '\n')
			if newline < 0 {
				break
			}
			r.writeText(value[:newline])
			r.endLine()
			r.writeText("\n")
			value = value[newline+1:]
		}
		r.writeText(value)
		position.atLineStart = strings.HasSuffix(node.Text, "\n")
		position.atTextStart = position.atLineStart
		position.afterLine = position.afterLine || strings.Contains(node.Text, "\n")
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
		if escape {
			r.writeText(value[segmentStart:byteOffset])
			r.writeSyntax(textEscape(char))
			segmentStart = byteOffset + width
		}
		if char == '\n' {
			r.writeText(value[segmentStart:byteOffset])
			segmentStart = byteOffset
			r.endLine()
			lineStart, textLineStart = byteOffset+width, byteOffset+width
			position.afterLine = true
		}
		position.atLineStart = char == '\n'
		position.atTextStart = char == '\n'
		position.afterMarker = false
	}
	r.writeText(value[segmentStart:])
}

// holdLineStart writes the text before a line's first character and holds that character for
// endLine to judge once the line is written.
func (r *renderer) holdLineStart(before string, char rune, position *inlinePosition, prefix string) {
	r.writeText(before)
	r.heldLineStart = &lineCandidate{
		at:            r.b.Len(),
		char:          char,
		afterLine:     position.afterLine,
		atTypedPrefix: r.typedPrefix != nil && *r.typedPrefix == prefix,
		prefix:        prefix,
	}
}
