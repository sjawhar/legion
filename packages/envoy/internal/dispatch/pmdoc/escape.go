package pmdoc

import (
	"fmt"
	"slices"
	"strings"

	"github.com/yuin/goldmark/ast"
	gmtext "github.com/yuin/goldmark/text"
)

// The renderer's escape rules. Paragraph text is written so the parser reads it back as the same
// text: a character is escaped when, unescaped, it would open a block, close a mark, form a link
// or an autolink, or otherwise change what comes back - and never otherwise, because every byte a
// rule changes in stored markdown mints a new version of every document that carries it.
//
// Most rules read the text alone (needsInlineEscape). The block forms a line's first character can
// begin - a thematic break or setext underline, a fence, a directive, an HTML block, a table
// delimiter row, indented code, a rule a list marker completes - depend on the whole line as
// written, markers and hard break included, and a setext underline or delimiter row on the line
// before as well. Those are left to the parser once the line is written (lineReadsAsText).

// escapeContext is what one character's escape depends on beyond the text itself.
type escapeContext struct {
	// textLineStart is where the character's line of text begins inside this node, or -1 when it
	// began in an earlier one: the writer's long-standing list, heading, quote and ordered-list
	// escapes are judged here, in headings, cells and inside marks as well, so that the markdown
	// they have always produced is produced still.
	textLineStart int
	// tableCell reports whether pipes are cell syntax here.
	tableCell bool
	// urlSchemes reports whether a URL scheme's colon would autolink: outside a link, and not in
	// a bare URL, which must stay as written for the parser to linkify it again.
	urlSchemes bool
	// label is the verdict on the brackets of the link label the text belongs to.
	label labelBrackets
	// followed reports whether something follows the text in its block, so that a trailing
	// backslash would escape that instead of standing for itself: the marker closing a mark, a
	// link's `]`, a hard break's backslash, the next node's first character. A block-final
	// backslash reads back as itself and is left as written.
	followed bool
}

// needsInlineEscape decides one character from the text alone.
func needsInlineEscape(value string, offset int, char rune, context escapeContext) bool {
	textLineStart := context.textLineStart
	switch char {
	case '\\':
		return offset+1 < len(value) && isASCIIPunctuation(value[offset+1]) ||
			offset+1 == len(value) && context.followed
	case '*':
		return (blockStart(value, textLineStart, offset) && markerTerminator(value, offset+1)) ||
			emphasisDelimiter(value, offset, '*')
	case '_':
		return emphasisDelimiter(value, offset, '_')
	case '`':
		return true
	case '[':
		// A label whose brackets cannot pair as written escapes them all: a stray `]` closes it
		// early, and a `[` left raw would then pair with its own closer.
		return context.label == labelBracketsEscaped ||
			context.label != labelBracketsWritten && linkOpener(value, offset)
	case '(':
		return offset > 0 && value[offset-1] == ']'
	case ']':
		return context.label == labelBracketsEscaped
	case '<':
		return angleConstruct(value, offset)
	case '&':
		return entityReference(value, offset)
	case '#':
		return blockStart(value, textLineStart, offset) && atxHeadingRun(value, offset)
	case '>':
		return blockStart(value, textLineStart, offset)
	case '-', '+':
		return blockStart(value, textLineStart, offset) && markerTerminator(value, offset+1)
	case '|':
		return context.tableCell
	case ':':
		return context.urlSchemes && urlSchemeColon(value, offset)
	case '.', ')':
		return orderedListMarkerPunctuation(value, offset, textLineStart)
	default:
		return false
	}
}

// escaped is how an escaped character is written: `&` as the `&amp;` reference; leading
// whitespace, which takes no backslash, as the numeric reference the parser decodes back to it,
// which keeps the line a paragraph instead of the indented code block four spaces or a tab would
// open; and anything else behind a backslash.
func escaped(char rune) string {
	switch char {
	case '&':
		return "&amp;"
	case ' ', '\t':
		return numericEntity(char)
	default:
		return "\\" + string(char)
	}
}

// lineStartVerdict is what the writer does with a character at the start of a line of its own.
type lineStartVerdict int

const (
	// lineStartUndecided: the character is indentation the parser skips, and a later one decides.
	lineStartUndecided lineStartVerdict = iota
	// lineStartAsIs: the line's text begins with a character no line-wide block form begins with,
	// or one the rules that read the text alone already escape.
	lineStartAsIs
	// lineStartEscaped: whitespace that would open indented code, on a textblock's first line with
	// no mark's marker before it, where the parser strips it even when no block opens - after a
	// list marker, say - so it is escaped at once.
	lineStartEscaped
	// lineStartHeld: a character only the whole line decides, held until the line is written.
	lineStartHeld
)

// lineStartOf decides the character at offset, on a line of its own whose text begins at
// lineStart; escape is what the rules that read the text alone decided for it.
func lineStartOf(value string, lineStart, offset int, char rune, escape, afterLine, afterMarker bool) lineStartVerdict {
	switch {
	case offset == lineStart && (char == ' ' || char == '\t') && indentedCodeRun(value, offset):
		if !afterLine && !afterMarker {
			return lineStartEscaped
		}
		return lineStartHeld
	case char != ' ' && char != '\t' && blockStart(value, lineStart, offset):
		if !escape && lineStartMarker(char) {
			return lineStartHeld
		}
		return lineStartAsIs
	case !blockStart(value, lineStart, offset):
		return lineStartAsIs
	}
	return lineStartUndecided
}

// lineStartMarker reports whether char, first in a line's text, can begin a block form that only
// the whole line decides.
func lineStartMarker(char rune) bool {
	return strings.ContainsRune("-*_=~:<|", char)
}

// closesTypedBlock reports whether line, written at the prefix of the typed block around it, is
// the lone `:::` that closes the block, which the line read on its own cannot show.
func closesTypedBlock(line, prefix string) bool {
	return strings.TrimSpace(strings.TrimPrefix(line, prefix)) == ":::"
}

// lineReadsAsText reports whether a written line reads the same with its text's first character
// escaped as without it, so that the character needs no escape. before is the line before when
// the line continues the same textblock, where a setext underline or a table delimiter row reads
// differently, and empty otherwise: a line of another block, read without the lines above it, can
// make a block of its own - a previous list item's `---` or `<br>` - that is no reason to escape
// anything here. The lines are read as the content of the blockquotes their prefix opens, without
// the list indentation they share up to the indentation of that prefix, so a deeply nested item
// is not read as indented code while text that is itself indented still is. footnote is the
// label, as written, of the footnote definition the read begins with, or empty: the parser drops a
// definition nothing refers to, so the lines are read after a reference to it.
func lineReadsAsText(before, line, rewrittenLine, prefix, footnote string) bool {
	quote, indentation := splitPrefix(prefix)
	written := dedent(unquote(before+line, quote), indentation)
	rewritten := dedent(unquote(before+rewrittenLine, quote), indentation)
	if footnote != "" {
		reference := "x[^" + footnote + "]\n\n"
		written, rewritten = reference+written, reference+rewritten
	}
	return slices.Equal(blockKinds(written), blockKinds(rewritten))
}

// splitPrefix splits a textblock's line prefix into the blockquote markers it opens with - up to
// its last `>` and the one space after it - and the indentation of the list items inside them.
func splitPrefix(prefix string) (quote string, indentation int) {
	if last := strings.LastIndexByte(prefix, '>'); last >= 0 {
		quote, prefix = prefix[:last+1], prefix[last+1:]
		if strings.HasPrefix(prefix, " ") {
			quote, prefix = quote+" ", prefix[1:]
		}
	}
	return quote, len(prefix) - len(strings.TrimLeft(prefix, " "))
}

// unquote drops quote from the start of each line that carries it, or its trimmed form from a
// blank quoted line.
func unquote(lines, quote string) string {
	if quote == "" {
		return lines
	}
	blank := strings.TrimRight(quote, " ")
	parts := strings.Split(lines, "\n")
	for index, line := range parts {
		if strings.HasPrefix(line, quote) {
			parts[index] = line[len(quote):]
		} else if strings.HasPrefix(line, blank) {
			parts[index] = line[len(blank):]
		}
	}
	return strings.Join(parts, "\n")
}

// blockKinds is the kinds of the blocks the parser reads markdown as, in document order.
func blockKinds(markdown string) []ast.NodeKind {
	root := unfrontmatteredParser.Parser().Parse(gmtext.NewReader([]byte(markdown)))
	var kinds []ast.NodeKind
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && node.Type() == ast.TypeBlock {
			kinds = append(kinds, node.Kind())
		}
		return ast.WalkContinue, nil
	})
	return kinds
}

// dedent drops the leading spaces every non-blank line of lines shares, up to limit.
func dedent(lines string, limit int) string {
	shared := limit
	parts := strings.Split(lines, "\n")
	for _, line := range parts {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if spaces := len(line) - len(strings.TrimLeft(line, " ")); spaces < shared {
			shared = spaces
		}
	}
	if shared <= 0 {
		return lines
	}
	for index, line := range parts {
		if len(line) >= shared {
			parts[index] = line[shared:]
		} else {
			parts[index] = strings.TrimLeft(line, " ")
		}
	}
	return strings.Join(parts, "\n")
}

// blockStart reports whether offset opens its own line: everything back to lineStart is
// indentation the parser skips, up to three spaces or a run of tabs. A marker one space in is
// still the marker — AGENTC-193's own payload was indented — and a line that began in an earlier
// text node (lineStart < 0) is never a block start here.
func blockStart(value string, lineStart, offset int) bool {
	if lineStart < 0 || lineStart > offset {
		return false
	}
	spaces := 0
	for index := lineStart; index < offset; index++ {
		switch value[index] {
		case ' ':
			spaces++
		case '\t':
		default:
			return false
		}
	}
	return spaces <= 3
}

// markerTerminator reports whether offset ends a list marker: the parser opens an item on a
// marker followed by a space, a tab, or the end of the line.
func markerTerminator(value string, offset int) bool {
	return offset >= len(value) || value[offset] == ' ' || value[offset] == '\t' || value[offset] == '\n'
}

// atxHeadingRun reports whether value opens an ATX heading marker at offset: one to six hashes
// ending the line or followed by a space or a tab. Escaping the first hash is enough to keep the
// whole run text, and without it a `## ` a replacement wrote into a paragraph reads back as a
// heading — or, inside a list item, as markdown the Proof schema refuses to import at all.
func atxHeadingRun(value string, offset int) bool {
	hashes := markerRun(value, offset)
	return hashes <= 6 && markerTerminator(value, offset+hashes)
}

// markerRun is how many times the character at offset repeats from there.
func markerRun(value string, offset int) int {
	marker := value[offset]
	end := offset
	for end < len(value) && value[end] == marker {
		end++
	}
	return end - offset
}

// indentedCodeRun reports whether a line opens with the four spaces, or the tab, that make it an
// indented code block. Only a block's own first line can open one: a later line of the same
// paragraph is a continuation, which no indentation turns into a block. Shorter runs are left
// alone - the parser strips them, which loses no block.
func indentedCodeRun(value string, offset int) bool {
	spaces := 0
	for index := offset; index < len(value); index++ {
		switch value[index] {
		case '\t':
			return true
		case ' ':
			spaces++
			if spaces >= 4 {
				return true
			}
		default:
			return false
		}
	}
	return false
}

// numericEntity writes a character as the numeric character reference the parser decodes back to
// it, for the one case where a backslash escape is not available.
func numericEntity(char rune) string {
	return fmt.Sprintf("&#%d;", char)
}

func emphasisDelimiter(value string, offset int, delimiter byte) bool {
	start, end := offset, offset+1
	for start > 0 && value[start-1] == delimiter {
		start--
	}
	for end < len(value) && value[end] == delimiter {
		end++
	}
	before := start > 0 && isASCIIAlphaNumeric(value[start-1])
	after := end < len(value) && isASCIIAlphaNumeric(value[end])
	return (delimiter != '_' || !before || !after) && (before || after)
}

func linkOpener(value string, offset int) bool {
	closing := strings.IndexByte(value[offset+1:], ']')
	if closing < 0 {
		return false
	}
	closing += offset + 1
	return linkCloser(value, closing)
}

func linkCloser(value string, offset int) bool {
	return offset+1 < len(value) && (value[offset+1] == '(' || value[offset+1] == ':')
}

// labelBrackets is how a link label's brackets are written, decided once for the whole label.
type labelBrackets int

const (
	// labelBracketsNone: the text is not a link's label.
	labelBracketsNone labelBrackets = iota
	// labelBracketsWritten: they balance and nest as written, so none is escaped - not even a `[`
	// the paragraph rule would take, whose escape would leave its partner closing the label early.
	// Nothing inside such a label forms another construct: a `(` after a `]` is escaped by the
	// paragraph rule, and `[x]:` cannot open a reference definition behind the label's own `[`,
	// since a definition's label holds no bracket.
	labelBracketsWritten
	// labelBracketsParagraph: they balance once the paragraph rule has escaped its `[`, which is
	// how they were always written, so they are written that way still.
	labelBracketsParagraph
	// labelBracketsEscaped: they balance neither way, so every bracket is escaped.
	labelBracketsEscaped
)

// linkLabelBrackets decides a label's brackets from its text, given as linkLabel builds it. The
// order keeps stored markdown stable: a label that balances as written, or as the paragraph rule
// writes it, reads back and is written with the same bytes again.
func linkLabelBrackets(label string) labelBrackets {
	if bracketsBalance(label, false) {
		return labelBracketsWritten
	}
	if bracketsBalance(label, true) {
		return labelBracketsParagraph
	}
	return labelBracketsEscaped
}

// bracketsBalance reports whether a label's brackets balance and nest, counting a `[` the
// paragraph rule escapes (marked paragraphEscapedBracket by linkLabel) as a bracket only when
// that rule is not applied.
func bracketsBalance(value string, paragraphRule bool) bool {
	depth := 0
	for offset := 0; offset < len(value); offset++ {
		switch value[offset] {
		case '[':
			depth++
		case paragraphEscapedBracket:
			if !paragraphRule {
				depth++
			}
		case ']':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// paragraphEscapedBracket stands, in a label's joined text, for a `[` the paragraph rule would
// escape (linkOpener, judged inside its own node as the writer judges it).
const paragraphEscapedBracket = '\x00'

// linkLabel is the text of the label a link opens at index: every text node from there that keeps
// the same link, less its code spans, which bind tighter than the label's brackets and whose text
// is never escaped anyway.
func linkLabel(nodes []*Node, index int, link Mark) string {
	var label strings.Builder
	for _, node := range nodes[index:] {
		if node.Type != "text" {
			break
		}
		marks := visibleMarks(node.Marks)
		if len(marks) == 0 || marks[0].Type != "link" || !attrsEqual(marks[0].Attrs, link.Attrs) {
			break
		}
		if nodeHasMark(node, "inlineCode") {
			continue
		}
		for offset := 0; offset < len(node.Text); offset++ {
			if node.Text[offset] == '[' && linkOpener(node.Text, offset) {
				label.WriteByte(paragraphEscapedBracket)
				continue
			}
			label.WriteByte(node.Text[offset])
		}
	}
	return label.String()
}

func angleConstruct(value string, offset int) bool {
	if offset+1 >= len(value) || !isASCIIAlphaNumeric(value[offset+1]) && value[offset+1] != '/' && value[offset+1] != '!' && value[offset+1] != '?' {
		return false
	}
	return strings.IndexByte(value[offset+1:], '>') >= 0
}

func entityReference(value string, offset int) bool {
	end := strings.IndexByte(value[offset+1:], ';')
	if end < 0 {
		return false
	}
	for _, char := range value[offset+1 : offset+end+1] {
		if !isASCIIAlphaNumeric(byte(char)) && char != '#' {
			return false
		}
	}
	return true
}

func urlSchemeColon(value string, offset int) bool {
	return strings.HasSuffix(value[:offset], "http") || strings.HasSuffix(value[:offset], "https")
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func isASCIIPunctuation(value byte) bool {
	return value >= '!' && value <= '/' || value >= ':' && value <= '@' || value >= '[' && value <= '`' || value >= '{' && value <= '~'
}

// orderedListMarkerPunctuation reports whether the `.` or `)` at offset closes an ordered-list
// marker: digits back to the start of an indented line, and a marker terminator after it.
func orderedListMarkerPunctuation(value string, offset int, lineStart int) bool {
	if !markerTerminator(value, offset+1) {
		return false
	}
	start := offset
	for start > 0 && value[start-1] >= '0' && value[start-1] <= '9' {
		start--
	}
	return start < offset && blockStart(value, lineStart, start)
}
