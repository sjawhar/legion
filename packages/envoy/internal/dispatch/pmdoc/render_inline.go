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
// across a hard break, around a reference or inside a link label. A bare URL's text, escapes
// included, can read back as another URL, and linkify continues one into the text written right
// after it unless that text opens with trailing punctuation and then an end, or, for a URL the
// tokenizer refuses and goldmark's parser reads, an escape. A mark's
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
// It reads a run back under readBackReaders: inlineMarkdown, which reads as every document is read
// now (editorRules), and the two readers Dispatch read stored markdown under before, storedMarkdown
// (storedRules: a `*` or `_` run may open or close beside a `*` or `_` but not a `~`, a `~` run is
// read by goldmark's strikethrough, which pairs runs of either length and refuses one after any
// `~`, and every run is paired in goldmark's one loop over them in order) and flankingMarkdown
// (flankingRules: those, with a `*` or `_` run judged by CommonMark's flanking rules alone). A
// writing's depth is how many of readBackReaders, from inlineMarkdown, it reads back under
// (readBackDepth), and the writer keeps the earliest writing of the greatest depth, the first
// writing counting first. Each depth asks more than the writer before INFRA-534 asked at that
// step, so wherever its choice reads back under inlineMarkdown this writer makes it too.
//
// Only where no writing so far reads back under inlineMarkdown is a run written again in each of
// lastResorts' spellings, so that none changes the bytes of a run another writing stores. A run
// none of the cases above reads back, but whose free asterisk run stands beside a bold or italic
// code span (delimitersInText with codeMarks), is read back as well, and written again only there.
func (r *renderer) inlineWithEscapes(nodes []*Node, prefix string, context inlineContext) {
	from := r.b.Len()
	fused := r.writeInlineRun(nodes, prefix, context, runSpelling{})
	held := delimitersInText(nodes, false)
	lastHeld := delimitersInText(nodes, true)
	bareURL := writesBareURL(nodes, context.tableCell)
	flanking := delimiterBesidePunctuation(nodes, context.tableCell)
	misread := held != delimitersAsRuled || bareURL || fused || flanking
	if r.err != nil || !misread && lastHeld == delimitersAsRuled {
		return
	}
	depth := r.readBackDepth(from, prefix, nodes)
	if depth == len(readBackReaders) || !misread && depth > 0 {
		return
	}
	kept := string(r.b.Bytes()[from:])
	var spellings, flanked []runSpelling
	if misread {
		spellings = respellings(held, bareURL, marksAsWritten)
		if fused {
			spellings = append(spellings, respellings(held, bareURL, marksKeptOpen)...)
		}
		if flanking {
			flanked = []runSpelling{{flanking: true}}
			for _, spelling := range spellings {
				spelling.flanking = true
				flanked = append(flanked, spelling)
			}
			spellings = append(spellings, flanked...)
		}
	}
	// Every writing leaves the renderer in the same state but for the bytes: writeInlineRun closes
	// its marks, which clears the open code fence, and ends its line, which clears the held line
	// start, and the link label verdict it leaves is read only inside a link, which a later run
	// opens again first. So the kept bytes written back after other spellings write what writing
	// them last would.
	readsBack := func(spellings []runSpelling) bool {
		for _, spelling := range spellings {
			r.b.Truncate(from)
			fusedAgain := r.writeInlineRun(nodes, prefix, context, spelling)
			if r.err != nil || spelling.order != marksAsWritten && fusedAgain {
				continue
			}
			reads := r.readBackDepth(from, prefix, nodes)
			if reads == len(readBackReaders) {
				return true
			}
			if reads > depth {
				depth, kept = reads, string(r.b.Bytes()[from:])
			}
		}
		return false
	}
	if readsBack(spellings) || depth == 0 && readsBack(lastResorts(held, lastHeld, flanked)) {
		return
	}
	r.b.Truncate(from)
	r.b.WriteString(kept)
}

// readBackReaders is the readers inlineWithEscapes reads a run back under, in the order a
// writing's depth counts them (readBackDepth).
var readBackReaders = [...]inlineReader{inlineMarkdown, storedMarkdown, flankingMarkdown}

// runSpelling is how writeInlineRun writes a run beyond the escape rules: the delimiters in its
// text it escapes, whether it escapes the character after each bare URL goldmark's parser reads
// (endsBareURL), whether it writes each bare URL the tokenizer reads (literalLength) as an autolink
// (`<href>`, whose text both parsers read as written and whose `>` ends it), the order it opens
// each text's marks in (markOrder), whether a letter beside a delimiter run that punctuation stands
// inside is a character reference (flanking), whether a literal backslash right before such a
// reference is written `\\` (pairBackslash), and whether an escaped tilde is written `\~`
// (tildeBackslash) rather than as its reference.
type runSpelling struct {
	escapes        delimiterEscapes
	afterBareURL   bool
	angleURLs      bool
	order          markOrder
	flanking       bool
	pairBackslash  bool
	tildeBackslash bool
}

// markOrder is the order writeInlineRun opens a text's marks in, after those written open before it
// that the text shares from the outermost.
type markOrder int

const (
	// marksAsWritten: in their written order (visibleMarks).
	marksAsWritten markOrder = iota
	// marksKeptOpen: keepingOpen.
	marksKeptOpen
	// marksByReach: byReach.
	marksByReach
	// marksReopened: reopening.
	marksReopened
)

// respellings is every spelling, its marks opened in order, that inlineWithEscapes writes a run in
// after its first writing, in the order it tries them: the delimiter escapes from the fewest, then
// each again with the character after a bare URL escaped, then each with every bare URL the
// tokenizer reads written as an autolink.
func respellings(held delimiterEscapes, bareURL bool, order markOrder) []runSpelling {
	var spellings []runSpelling
	for _, url := range []runSpelling{{}, {afterBareURL: true}, {angleURLs: true}} {
		if url != (runSpelling{}) && !bareURL {
			continue
		}
		for _, delimiters := range escapesFrom(held) {
			spelling := runSpelling{escapes: delimiters, afterBareURL: url.afterBareURL, angleURLs: url.angleURLs, order: order}
			if spelling != (runSpelling{}) {
				spellings = append(spellings, spelling)
			}
		}
	}
	return spellings
}

// escapesFrom is the delimiter escapes a run whose text first needs held is written in, from the
// fewest: the rules alone, then held and each after it.
func escapesFrom(held delimiterEscapes) []delimiterEscapes {
	escapes := []delimiterEscapes{delimitersAsRuled}
	if held != delimitersAsRuled {
		for next := held; next <= delimitersAll; next++ {
			escapes = append(escapes, next)
		}
	}
	return escapes
}

// lastResorts is every spelling inlineWithEscapes writes a run in only where no writing before
// reads back under inlineMarkdown, so that no run another writing stores changes its bytes, in the
// order it tries them: flanked, the flanking spellings tried before, again with a literal
// backslash right before a character reference written `\\`, where it would otherwise escape the
// reference's `&`; the delimiter escapes respellings did not try, with an asterisk run beside a bold
// or italic code span counted as one beside a bold or italic text (delimitersInText, lastHeld for
// held); then the escapes from the fewest with each escaped tilde written `\~`, which the browser
// editor reads as a tilde beside which a `*` run may open or close, where its reference ends in
// `;`; then with the marks ordered by reach (byReach); then reopened (reopening).
func lastResorts(held, lastHeld delimiterEscapes, flanked []runSpelling) []runSpelling {
	var spellings []runSpelling
	for _, spelling := range flanked {
		spelling.pairBackslash = true
		spellings = append(spellings, spelling)
	}
	if held == delimitersAsRuled && lastHeld != delimitersAsRuled {
		for _, delimiters := range escapesFrom(lastHeld)[1:] {
			spellings = append(spellings, runSpelling{escapes: delimiters})
		}
	}
	for _, delimiters := range escapesFrom(lastHeld) {
		if delimiters >= delimitersTildes {
			spellings = append(spellings, runSpelling{escapes: delimiters, tildeBackslash: true})
		}
	}
	for _, order := range []markOrder{marksByReach, marksReopened} {
		for _, delimiters := range escapesFrom(lastHeld) {
			spellings = append(spellings, runSpelling{escapes: delimiters, order: order})
		}
	}
	return spellings
}

// keepingOpen orders marks, the marks a text is written under, so that no mark it or the text
// after it still carries is closed and opened again between them: the longest run of active, the
// marks written open before it, from the outermost, whose every mark it carries comes first, as
// active holds them; then its other marks that following, the next text's marks, carries; then
// the rest, each group in its written order, opened inside the one before; and a code span's mark,
// whose span holds no syntax, innermost (codeInnermost).
func keepingOpen(active, marks, following []Mark) []Mark {
	kept := sharedOpenMarks(active, marks)
	ordered := append(make([]Mark, 0, len(marks)), active[:kept]...)
	for _, carried := range []bool{true, false} {
		for _, mark := range marks {
			if !containsSameMark(active[:kept], mark) && containsSameMark(following, mark) == carried {
				ordered = append(ordered, mark)
			}
		}
	}
	return codeInnermost(ordered)
}

// sharedOpenMarks is how long a run of active, the marks written open before a text, from the
// outermost, marks, the text's marks, carries every mark of.
func sharedOpenMarks(active, marks []Mark) int {
	kept := 0
	for kept < len(active) && containsSameMark(marks, active[kept]) {
		kept++
	}
	return kept
}

// byReach orders marks, the marks nodes[index] is written under, so that a mark the texts after it
// carry further is opened outside one they carry less far, and is not closed and opened again
// where the nearer one ends (`***a**b*`, never `***a****b*`): the longest run of active, the
// marks written open before it, from the outermost, whose every mark it carries comes first, as
// active holds them; then its other marks, the one carried over the most texts in a row outermost,
// ties in their written order; and a code span's mark innermost (codeInnermost).
func byReach(active, marks []Mark, nodes []*Node, index int, escapePipes, wwwBare bool) []Mark {
	kept := sharedOpenMarks(active, marks)
	ordered := append(make([]Mark, 0, len(marks)), active[:kept]...)
	rest := make([]Mark, 0, len(marks))
	reach := make([]int, 0, len(marks))
	for _, mark := range marks {
		if containsSameMark(active[:kept], mark) {
			continue
		}
		texts := 1
		for texts+index < len(nodes) && containsSameMark(writtenMarks(nodes[index+texts], escapePipes, wwwBare), mark) {
			texts++
		}
		at := len(rest)
		for at > 0 && reach[at-1] < texts {
			at--
		}
		rest, reach = slices.Insert(rest, at, mark), slices.Insert(reach, at, texts)
	}
	return codeInnermost(append(ordered, rest...))
}

// reopening orders marks, the marks a text is written under, so that none of active, the marks
// written open before it, stays open around a mark the text opens: the marks active does not hold
// come first, then those it holds, each group in its written order, and a code span's mark
// innermost (codeInnermost). A mark both texts carry is then closed and opened again inside the
// new ones (`*a*~~*b*~~`), so that its delimiter run stands beside the new one's rather than a
// letter.
func reopening(active, marks []Mark) []Mark {
	ordered := make([]Mark, 0, len(marks))
	for _, held := range []bool{false, true} {
		for _, mark := range marks {
			if containsSameMark(active, mark) == held {
				ordered = append(ordered, mark)
			}
		}
	}
	return codeInnermost(ordered)
}

// codeInnermost is marks with a code span's mark, if any, moved innermost: a code span holds no
// syntax, so a mark opened inside it would be written as its text.
func codeInnermost(marks []Mark) []Mark {
	for index, mark := range marks {
		if mark.Type == "inlineCode" && index < len(marks)-1 {
			return append(append(marks[:index:index], marks[index+1:]...), mark)
		}
	}
	return marks
}

// writesBareURL reports whether nodes write a link as its bare URL (isBareURLLink, isBareWWWLink),
// which linkify reads again from the text alone.
func writesBareURL(nodes []*Node, escapePipes bool) bool {
	for _, node := range nodes {
		if node.Type != "text" {
			continue
		}
		if marks := visibleMarks(node.Marks); isBareURLLink(node, marks, escapePipes) || isBareWWWLink(node, marks, escapePipes) {
			return true
		}
	}
	return false
}

// followsBareURL reports whether nodes[index] is text outside any link written right after a bare
// URL (isBareURLLink) that goldmark's parser reads, since the tokenizer refuses it
// (literalLength): goldmark ends a URL at a backslash, where the tokenizer reads one into it.
func followsBareURL(nodes []*Node, index int, escapePipes bool) bool {
	if index == 0 || nodes[index].Type != "text" || nodeHasMark(nodes[index], "link") {
		return false
	}
	previous := nodes[index-1]
	return previous.Type == "text" && nodeHasMark(previous, "link") && isBareURLLink(previous, visibleMarks(previous.Marks), escapePipes) &&
		literalLength(previous.Text) == 0
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

// readBackDepth is how many of readBackReaders, from the first, the inline markdown written since
// from reads back as nodes under (runReadsBack), stopping at the first it does not: a writing
// that does not beat a depth is read under at most one reader more than that depth.
func (r *renderer) readBackDepth(from int, prefix string, nodes []*Node) int {
	depth := 0
	for depth < len(readBackReaders) && r.runReadsBack(from, prefix, nodes, readBackReaders[depth]) {
		depth++
	}
	return depth
}

// delimiterBesidePunctuation reports whether nodes write a delimiter run (a strong, emphasis or
// strikethrough mark's) with punctuation or whitespace just inside it and text just outside it,
// where the flanking rules can keep the run from opening or closing.
func delimiterBesidePunctuation(nodes []*Node, escapePipes bool) bool {
	for index, node := range nodes {
		if node.Type != "text" || !hasDelimiterMark(writtenMarks(node, escapePipes, true)) {
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

// wwwMayFollow reports whether the markdown written so far ends where the browser editor's
// tokenizer begins a www URL, and Go's guard reads one (literalURLFollows): at a line's head, or
// after a space, a tab, `(`, `*`, `_`, `[`, `]` or `~`. After any other character the editor reads
// a www URL by another rule than Go's, so a www link is written as an explicit link there.
func (r *renderer) wwwMayFollow() bool {
	written := r.b.Bytes()
	return len(written) == 0 || strings.IndexByte("\n \t(*_[]~", written[len(written)-1]) >= 0
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
			wwwBare := !spelling.angleURLs
			// A www link is written bare only where the character written before it is one a www
			// URL may follow (wwwMayFollow); elsewhere its marks are written again with the link.
			// The rollback restores everything closing and opening marks changes: the bytes, the
			// open marks, the position, and the renderer's open code fence and link label verdict.
			before, beforeActive, beforePosition, beforeFused := r.b.Len(), active, position, fused
			beforeFence, beforePadded, beforeLabel := r.inlineCodeFence, r.inlineCodePadded, r.labelBrackets
			bareHere := wwwBare
			var next, following []Mark
			var common int
			var flankFirst bool
			for {
				next = writtenMarks(n, escapePipes, bareHere)
				following = nil
				if index+1 < len(nodes) {
					following = writtenMarks(nodes[index+1], escapePipes, wwwBare)
				}
				switch spelling.order {
				case marksKeptOpen:
					next = keepingOpen(active, next, following)
					if index+1 < len(nodes) {
						var after []Mark
						if index+2 < len(nodes) {
							after = writtenMarks(nodes[index+2], escapePipes, wwwBare)
						}
						following = keepingOpen(next, following, after)
					}
				case marksByReach:
					next = byReach(active, next, nodes, index, escapePipes, wwwBare)
					if index+1 < len(nodes) {
						following = byReach(next, following, nodes, index+1, escapePipes, wwwBare)
					}
				case marksReopened:
					next = reopening(active, next)
					following = reopening(next, following)
				}
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
				r.b.Truncate(before)
				active, position, fused, bareHere = beforeActive, beforePosition, beforeFused, false
				r.inlineCodeFence, r.inlineCodePadded, r.labelBrackets = beforeFence, beforePadded, beforeLabel
			}
			hasLink := containsMark(visibleMarks(n.Marks), "link")
			active = next
			marks := visibleMarks(n.Marks)
			if isAngleURLLink(n, marks, escapePipes) || spelling.angleURLs && isBareURLLink(n, marks, escapePipes) && literalLength(n.Text) != 0 {
				// An autolink's text is literal to both parsers, so it takes no escape.
				r.writeSyntax("<" + n.Text + ">")
				position.atLineStart, position.atTextStart, position.afterMarker = false, false, false
				continue
			}
			if isBareURLLink(n, marks, escapePipes) && literalLength(n.Text) != 0 || bareHere && isBareWWWLink(n, marks, escapePipes) {
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
		// A text `!` right before the link's `[` would open an image, so it is written `\!`.
		if r.endsWith('!') && !escapedByBackslashes(r.b.Bytes(), r.b.Len()-1) {
			r.b.Truncate(r.b.Len() - 1)
			r.writeSyntax(`\!`)
		}
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
			r.writeSyntax(textEscape(char, endsBareURL(byteOffset, char, context) || char == '~' && context.tildeBackslash))
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
