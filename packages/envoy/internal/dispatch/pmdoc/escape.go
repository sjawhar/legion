package pmdoc

import (
	"fmt"
	"slices"
	"strings"

	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
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
	// delimiters is which of the text's delimiter characters are escaped beyond the rules, because
	// the run they are in does not read back with them as written (inlineWithEscapes).
	delimiters delimiterEscapes
	// heading reports whether the text is a heading's.
	heading bool
	// marked reports whether the text is written inside a mark's syntax, where the parser keeps
	// whitespace at its edges.
	marked bool
	// afterMarker reports whether a mark's marker is written on the character's line before it,
	// so that the character does not begin the line's text as the parser trims it.
	afterMarker bool
	// opener and closer are the delimiter character (`*` or `~`) of the mark written right
	// before the text and of the one written right after it, or 0 when that is no delimiter.
	opener, closer byte
}

// needsInlineEscape decides one character from the text alone.
func needsInlineEscape(value string, offset int, char rune, context escapeContext) bool {
	textLineStart := context.textLineStart
	switch char {
	case '\\':
		// Before whitespace written as a reference, a backslash would escape its `&`.
		return offset+1 < len(value) && isASCIIPunctuation(value[offset+1]) ||
			offset+1 == len(value) && context.followed ||
			offset+1 < len(value) && (value[offset+1] == ' ' || value[offset+1] == '\t') &&
				needsInlineEscape(value, offset+1, rune(value[offset+1]), context)
	case '*':
		return (blockStart(value, textLineStart, offset) && markerTerminator(value, offset+1)) ||
			emphasisDelimiter(value, offset, '*') || besideDelimiter(value, offset, '*', context) ||
			context.delimiters == delimitersAll
	case '_':
		return emphasisDelimiter(value, offset, '_') || context.delimiters == delimitersAll
	case '`':
		return true
	case '~':
		return context.delimiters >= delimitersTildes
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
		return blockStart(value, textLineStart, offset) && atxHeadingRun(value, offset) ||
			context.heading && !context.followed && closingSequence(value, offset)
	case '>':
		return blockStart(value, textLineStart, offset)
	case '-', '+':
		return blockStart(value, textLineStart, offset) && markerTerminator(value, offset+1)
	case '|':
		return context.tableCell
	case ':':
		return context.urlSchemes && urlSchemeColon(value, offset)
	case ' ', '\t':
		// The parser trims whitespace that begins a line of a textblock's text or ends the
		// textblock, and a delimiter beside whitespace opens or closes no mark, so the first of
		// a leading run and the last of a trailing one are written as the references it keeps,
		// which the delimiter rule does not read as whitespace.
		return offset == textLineStart && !context.marked && !context.afterMarker ||
			offset+1 == len(value) && !context.followed ||
			offset == 0 && context.opener != 0 ||
			offset+1 == len(value) && context.closer != 0
	case '.', ')':
		return orderedListMarkerPunctuation(value, offset, textLineStart)
	default:
		return false
	}
}

// escaped is how an escaped character is written: `&` as the `&amp;` reference; whitespace, which
// takes no backslash, as the numeric reference the parser decodes back to it, which it neither
// trims nor reads as the indentation that opens indented code; and anything else behind a
// backslash.
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

// textEscape is how the text writer spells an escaped character: a tilde as its numeric
// reference, since the strikethrough parser refuses a delimiter run right after a tilde even when
// a backslash escapes it, and anything else as escaped spells it.
func textEscape(char rune) string {
	if char == '~' {
		return numericEntity(char)
	}
	return escaped(char)
}

// delimiterEscapes is which delimiter characters in text the writer escapes beyond its rules.
type delimiterEscapes int

const (
	// delimitersAsRuled: only where the rules escape them.
	delimitersAsRuled delimiterEscapes = iota
	// delimitersTildes: every tilde as well.
	delimitersTildes
	// delimitersAll: every tilde, asterisk and underscore.
	delimitersAll
)

// delimitersInText is the first escape a run's text could need: delimitersTildes when a text node
// outside a code span holds a tilde; delimitersAll when asterisks or underscores could pair - two
// free runs of one of them, or one free run of asterisks beside bold or italic text, whose markers
// are asterisks too; and delimitersAsRuled otherwise, when nothing is read back.
func delimitersInText(nodes []*Node) delimiterEscapes {
	stars, underscores := 0, 0
	for _, node := range nodes {
		if node.Type != "text" || nodeHasMark(node, "inlineCode") {
			continue
		}
		if strings.ContainsRune(node.Text, '~') {
			return delimitersTildes
		}
		stars += freeDelimiterRuns(node.Text, '*')
		underscores += freeDelimiterRuns(node.Text, '_')
		if nodeHasMark(node, "strong") || nodeHasMark(node, "emphasis") {
			stars++
		}
	}
	if stars >= 2 || underscores >= 2 {
		return delimitersAll
	}
	return delimitersAsRuled
}

// freeDelimiterRuns counts the runs of delimiter in text that the rules leave unescaped and that
// could still open or close emphasis: no letter or digit beside them, which the rules escape,
// and not whitespace on both sides, which can do neither. A text's edge may be anything.
func freeDelimiterRuns(text string, delimiter byte) int {
	runs := 0
	for start := 0; start < len(text); start++ {
		if text[start] != delimiter {
			continue
		}
		end := start + 1
		for end < len(text) && text[end] == delimiter {
			end++
		}
		before, after := byte('!'), byte('!')
		if start > 0 {
			before = text[start-1]
		}
		if end < len(text) {
			after = text[end]
		}
		if !isASCIIAlphaNumeric(before) && !isASCIIAlphaNumeric(after) && !(isLineWhitespace(before) && isLineWhitespace(after)) {
			runs++
		}
		start = end - 1
	}
	return runs
}

func isLineWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n'
}

// inlineSignature describes inline nodes as the reader sees them: each run of text with the marks
// it renders, merged across nodes that carry the same marks, and every other node by its type and
// what it writes.
func inlineSignature(nodes []*Node) []string {
	var signature []string
	lastMarks := ""
	for _, node := range nodes {
		if node.Type != "text" {
			value, _ := node.Attrs["value"].(string)
			src, _ := node.Attrs["src"].(string)
			label, _ := node.Attrs["label"].(string)
			signature = append(signature, node.Type+"|"+value+"|"+src+"|"+label)
			lastMarks = "\x00"
			continue
		}
		var marks []string
		for _, mark := range visibleMarks(node.Marks) {
			href, _ := mark.Attrs["href"].(string)
			title, _ := mark.Attrs["title"].(string)
			marks = append(marks, mark.Type+"|"+href+"|"+title)
		}
		key := strings.Join(marks, ",")
		if len(signature) > 0 && key == lastMarks {
			signature[len(signature)-1] += node.Text
			continue
		}
		signature = append(signature, key+"\x00"+node.Text)
		lastMarks = key
	}
	return signature
}

// lineStartVerdict is what the writer does with a character at the start of a line of its own.
type lineStartVerdict int

const (
	// lineStartUndecided: the character is indentation the parser skips, and a later one decides.
	lineStartUndecided lineStartVerdict = iota
	// lineStartAsIs: the line's text begins with a character no line-wide block form begins with,
	// or one the rules that read the text alone already escape.
	lineStartAsIs
	// lineStartEscaped: whitespace that would open indented code, which the rules that read the
	// text alone already escape where the parser would trim it.
	lineStartEscaped
	// lineStartHeld: a character only the whole line decides, held until the line is written.
	lineStartHeld
)

// lineStartOf decides the character at offset, on a line of its own whose text begins at
// lineStart; escape is what the rules that read the text alone decided for it.
func lineStartOf(value string, lineStart, offset int, char rune, escape bool) lineStartVerdict {
	switch {
	case offset == lineStart && (char == ' ' || char == '\t') && indentedCodeRun(value, offset):
		if escape {
			return lineStartEscaped
		}
		return lineStartHeld
	case char != ' ' && char != '\t' && blockStart(value, lineStart, offset):
		if !escape && lineStartMarker(value, offset, char) {
			return lineStartHeld
		}
		return lineStartAsIs
	case !blockStart(value, lineStart, offset):
		return lineStartAsIs
	}
	return lineStartUndecided
}

// lineStartMarker reports whether char, first in a line's text, can begin a form that only the
// whole line decides: a block form, or, for `[`, the task checkbox a list item's text would open
// with `[ ]` or `[x]`.
func lineStartMarker(value string, offset int, char rune) bool {
	if char == '[' {
		return taskCheckboxText(value, offset)
	}
	return strings.ContainsRune("-*_=~:<|", char)
}

// taskCheckboxText reports whether the text at offset is `[ ]`, `[x]` or `[X]` followed by a
// space, a tab or its end.
func taskCheckboxText(value string, offset int) bool {
	rest := value[offset:]
	return len(rest) >= 3 && rest[0] == '[' && strings.ContainsRune(" xX", rune(rest[1])) && rest[2] == ']' &&
		(len(rest) == 3 || rest[3] == ' ' || rest[3] == '\t')
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
	source := []byte(markdown)
	root := parseLined(unfrontmatteredParser, lineEnds(source), source)
	var kinds []ast.NodeKind
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		// A task checkbox is inline, but it is the list item's syntax, not its text.
		if entering && (node.Type() == ast.TypeBlock || node.Kind() == extensionast.KindTaskCheckBox) {
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

// closingSequence reports whether offset starts a heading's closing sequence: a run of `#` at the
// start of the heading's text or after a space or a tab, with nothing after it but spaces and
// tabs. The parser drops it from the heading's text; escaping its first `#` keeps it as text.
func closingSequence(value string, offset int) bool {
	if offset > 0 && value[offset-1] != ' ' && value[offset-1] != '\t' {
		return false
	}
	return strings.Trim(value[offset+markerRun(value, offset):], " \t") == ""
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

// besideDelimiter reports whether the run of delimiter holding offset touches a mark's run of the
// same character, which it would join.
func besideDelimiter(value string, offset int, delimiter byte, context escapeContext) bool {
	start, end := offset, offset+1
	for start > 0 && value[start-1] == delimiter {
		start--
	}
	for end < len(value) && value[end] == delimiter {
		end++
	}
	return start == 0 && context.opener == delimiter || end == len(value) && context.closer == delimiter
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
