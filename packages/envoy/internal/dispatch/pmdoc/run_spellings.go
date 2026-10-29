package pmdoc

import (
	"cmp"
	"crypto/sha256"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark/util"
)

// inlineWithEscapes writes one textblock's inline nodes. Delimiters in text the escape rules leave
// alone can still pair into a mark the text never had, or break one it has: GFM reads a single
// tilde as a strikethrough delimiter and refuses a run of three, and asterisks and underscores pair
// across a hard break, around a reference or inside a link label. A bare URL's text can read back
// as another URL, or run on into the text written right after it, which the browser editor's
// engine continues a URL it reads in decoded text into up to whitespace; plain text can read back
// as an address. A mark's delimiter run beside punctuation inside it and a letter outside it opens
// or closes nothing (`**Which here?**Careful.`). A run whose text holds any of these (runNeeds) is
// read back after it is written, and when it does not come back as written it is written again in
// each spelling runSpellings lists, keeping the first that reads back, so a run that fails for
// another reason keeps the bytes it had.
//
// And the marks are written in one order, so a mark the next text still carries is closed and
// opened again inside that text's other marks wherever the order puts it after them: bold inside
// italic comes out as `*`, `**` and `*` side by side, one run of four asterisks. The parser splits
// such a fused run as written where its flanking allows, so a fused first writing that reads back
// is kept, as the writer before sjawhar/legion#2360 keeps it, and one that does not is written
// again in the plain respellings, as that writer writes it again. Only where none of those reads
// back is it written with open marks kept open (keepingOpen), each such spelling kept only where
// it reads back with no delimiter run fused.
// Then come those spellings again with the letter beside each such delimiter run written as a
// character reference, which is punctuation to the flanking rules, as the browser editor writes it
// (flanking).
//
// An email link is written explicit, and a run holding one is read back too: the text before it and
// the text after it, which the engine searches for an address each as a text of its own, can read
// back linked where the run's markdown read them once with the address. `a\_b@c.dd\.@y.zz` reads
// as the address `a_b@c.dd` and the text `.@y.zz`, but the engine links `.@y.zz` after
// `[a_b@c.dd](mailto:a_b@c.dd)`, and `x /www.` before an explicit link reads as a www URL. Last,
// where the run holds an email link, come all these spellings again with each email link written
// as its address (isBareEmailLink), its `@` escaped where the ats spellings escape them, so the
// address and the text beside it are one text again.
//
// It reads a run back under readBackReaders: inlineMarkdown, which reads as every document is
// read now (editorRules), and the two readers Dispatch read stored markdown under before,
// storedMarkdown (storedRules: a `*` or `_` run may open or close beside a `*` or `_` but not a
// `~`, a `~` run is read by goldmark's strikethrough, which pairs runs of either length and
// refuses one after any `~`, and every run is paired in goldmark's one loop over them in order)
// and flankingMarkdown (flankingRules: those, with a `*` or `_` run judged by CommonMark's
// flanking rules alone). A writing's depth is how many of readBackReaders, from inlineMarkdown, it
// reads back under (readsBackUnder), and the writer keeps the earliest writing of the greatest
// depth, the first writing counting first. Each depth asks more than the writer before the tilde
// pairing (sjawhar/legion#2360) asked at that step, so wherever its choice reads back under
// inlineMarkdown this writer makes it too. Stored markdown keeps its bytes wherever they read back
// as the browser editor reads them.
//
// Only where no writing so far reads back under inlineMarkdown is a run written again in each of
// lastResorts' spellings, so that none changes the bytes of a run another writing stores. A run
// none of the cases above reads back, but whose free asterisk run stands beside a bold or italic
// code span (delimitersInText's lastHeld), is read back as well, and written again only there.
//
// What a writing reads back as is decided by its bytes alone, and many spellings write the same
// bytes: a flag the run gives nothing to change. So each spelling is written once and only bytes
// no writing before it wrote are read back, each under the readers in order until one refuses it,
// which is how many from the first it reads back under (readsBackUnder); the writing kept is then
// written again.
//
// A read that is provably the same parse as one already made is not made again (readsBackUnder):
// storedMarkdown reads markdown holding no `~` as inlineMarkdown does, since the rules differ only
// beside a `~`, and flankingMarkdown reads markdown as storedMarkdown does where no `*` or `_` run
// could open or close otherwise under flankingRules (flankingDivergenceKey).
func (r *renderer) inlineWithEscapes(nodes []*Node, prefix string, context inlineContext) {
	from := r.b.Len()
	fused := r.writeInlineRun(nodes, prefix, context, runSpelling{})
	needs := runNeedsOf(nodes, context, fused)
	if r.err != nil || needs == (runNeeds{}) {
		return
	}
	labels, signature := referencedLabels(nodes), inlineSignature(nodes)
	// firstReads is how many of readBackReaders, from the first, the first writing reads back under.
	firstReads := r.readsBackUnder(from, prefix, labels, signature)
	if firstReads == len(readBackReaders) {
		return
	}
	written := string(r.b.Bytes()[from:])
	// readsByBytes is how many of readBackReaders, from the first, each writing read back so far
	// reads back under, by the digest of its bytes.
	readsByBytes := map[[sha256.Size]byte]int{sha256.Sum256(r.b.Bytes()[from:]): firstReads}
	// keepsDeeper writes each of spellings once and reports whether one of them reads back deeper
	// than the first writing, leaving the earliest writing of the greatest depth written.
	keepsDeeper := func(spellings []runSpelling) bool {
		// reads is each spelling's depth, or -1 for one that orders marks otherwise and still
		// fuses a delimiter run, which is never kept.
		reads := make([]int, len(spellings))
		for index, spelling := range spellings {
			r.b.Truncate(from)
			if r.writeInlineRun(nodes, prefix, context, spelling) && spelling.order != marksAsWritten {
				reads[index] = -1
				continue
			}
			digest := sha256.Sum256(r.b.Bytes()[from:])
			count, read := readsByBytes[digest]
			if !read {
				count = r.readsBackUnder(from, prefix, labels, signature)
				if count == len(readBackReaders) {
					return true
				}
				readsByBytes[digest] = count
			}
			reads[index] = count
		}
		for under := len(readBackReaders) - 1; under > firstReads; under-- {
			for index, spelling := range spellings {
				if reads[index] >= under {
					r.b.Truncate(from)
					r.writeInlineRun(nodes, prefix, context, spelling)
					return true
				}
			}
		}
		return false
	}
	spellings := runSpellings(needs)
	if keepsDeeper(spellings) || firstReads == 0 && keepsDeeper(lastResorts(needs, spellings)) {
		return
	}
	r.b.Truncate(from)
	r.b.WriteString(written)
}

// runNeeds is what a run's first writing and its text call for beyond the escape rules, each a
// reason to read the run back and a family of spellings to write it again in (runSpellings): the
// first delimiter escape its text could need (delimitersInText), the first it could need with a
// free asterisk run beside a bold or italic code span counted as one beside bold or italic text
// (lastHeld, which only lastResorts writes), whether it writes a bare URL (writesBareURL), whether
// its text outside a link holds a URL scheme's colon, a dot after `www`
// or an `@` (writesAddressSyntax), whether the first writing fused two delimiter runs where a text
// is under more than one mark, which keepingOpen can then order otherwise (nestsMarks), whether a
// delimiter run has punctuation just inside it (delimiterBesidePunctuation) and a letter just
// outside it the flanking spellings can write as a reference (letterBesideDelimiter), and whether
// it holds an email link it can write as its address (writesBareEmail). A run that needs none of
// these is not read back.
type runNeeds struct {
	held, lastHeld                                      delimiterEscapes
	bareURL, colons, dots, ats, fused, flanking, emails bool
}

// runNeedsOf is the runNeeds of nodes, written in context, whose first writing fused two delimiter
// runs where fused.
func runNeedsOf(nodes []*Node, context inlineContext, fused bool) runNeeds {
	colons, dots, ats := writesAddressSyntax(nodes)
	held, lastHeld := delimitersInText(nodes)
	return runNeeds{
		held:     held,
		lastHeld: lastHeld,
		bareURL:  writesBareURL(nodes, context.tableCell),
		colons:   colons,
		dots:     dots,
		ats:      ats,
		fused:    fused && nestsMarks(nodes),
		flanking: delimiterBesidePunctuation(nodes, context.tableCell) && letterBesideDelimiter(nodes),
		emails:   writesBareEmail(nodes),
	}
}

// readBackReaders is the readers inlineWithEscapes reads a run back under, in the order it drops
// them from the last: inlineMarkdown, storedMarkdown and flankingMarkdown (readsBackUnder).
var readBackReaders = [...]inlineReader{inlineMarkdown, storedMarkdown, flankingMarkdown}

// runSpelling is how writeInlineRun writes a run beyond the escape rules: the delimiters in its
// text it escapes, whether it writes each bare URL (isBareURLLink) as an autolink (`<href>`, whose
// text both readers read as written and whose `>` ends it), whether it escapes the colons of URL
// schemes, the dots after `www` and the `@`s in text outside a link (escapeContext.schemeColons,
// wwwDots, ats), whether it writes each bare URL as text with that colon escaped as well
// (bareURLColons), the order it opens each text's marks in (markOrder), whether a letter beside a
// delimiter run that punctuation stands inside is a character reference (flanking), whether it
// writes each email link as its address (isBareEmailLink), text outside a link to the escapes,
// whether a literal backslash right before such a reference is written `\\` (pairBackslash), and
// whether an escaped tilde is written `\~` (tildeBackslash) rather than as its reference.
type runSpelling struct {
	escapes                               delimiterEscapes
	angleURLs, schemeColons, wwwDots, ats bool
	bareURLColons                         bool
	order                                 markOrder
	flanking                              bool
	bareEmails                            bool
	pairBackslash, tildeBackslash         bool
}

// markOrder is the order writeInlineNodes opens a text's marks in, after those written open before
// it that the text shares from the outermost.
type markOrder int

const (
	// marksAsWritten: in their written order (spelledMarks).
	marksAsWritten markOrder = iota
	// marksKeptOpen: keepingOpen.
	marksKeptOpen
	// marksByReach: byReach.
	marksByReach
	// marksReopened: reopening.
	marksReopened
)

// runSpellings is every spelling inlineWithEscapes writes a run in after its first writing, in the
// order it tries them. First the respellings, email links explicit: the delimiter escapes from the
// fewest, then each with every bare URL written as an autolink where the run writes one (bareURL),
// the colons of URL schemes escaped, or the `@`s, where its text holds them (colons, ats), then
// each with two of these and last with all three, so a bare URL before plain text opening with an
// address only the tokenizer reads (`http://localhost`) is written explicit with that colon
// escaped. Where its text holds a `www.` (dots), all of these come again with the dot after each
// `www` escaped as well, after every spelling that leaves it alone. Where the first writing fused
// two delimiter runs, all the respellings come again with open marks kept open; then, where a
// delimiter run has punctuation inside it (flanking), the run with its letters beside one written
// as references, and each spelling before it so. Last, where the run holds an email link (emails),
// the run with each written as its address, then all of these spellings again with them so, whose
// `@` is text the ats spellings escape.
func runSpellings(needs runNeeds) []runSpelling {
	escapes := escapesFrom(needs.held)
	var spellings []runSpelling
	for _, bareEmails := range []bool{false, true} {
		if bareEmails && !needs.emails {
			break
		}
		ats := needs.ats || bareEmails
		// lead is the run written with no flag but bareEmails: the first writing, or the first
		// spelling of the email pass.
		lead := runSpelling{bareEmails: bareEmails}
		if bareEmails {
			spellings = append(spellings, lead)
		}
		respelled := len(spellings)
		for _, order := range []markOrder{marksAsWritten, marksKeptOpen} {
			if order != marksAsWritten && !needs.fused {
				break
			}
			for _, dotted := range []bool{false, true} {
				if dotted && !needs.dots {
					break
				}
				for _, url := range []runSpelling{{}, {angleURLs: true}, {schemeColons: true}, {ats: true},
					{schemeColons: true, ats: true}, {angleURLs: true, schemeColons: true}, {angleURLs: true, ats: true},
					{angleURLs: true, schemeColons: true, ats: true}} {
					if url.angleURLs && !needs.bareURL || url.schemeColons && !needs.colons || url.ats && !ats {
						continue
					}
					url.wwwDots, url.order, url.bareEmails = dotted, order, bareEmails
					for _, delimiters := range escapes {
						spelling := url
						spelling.escapes = delimiters
						if spelling != lead {
							spellings = append(spellings, spelling)
						}
					}
				}
			}
		}
		if needs.flanking {
			flanked := len(spellings)
			spellings = append(spellings, runSpelling{flanking: true, bareEmails: bareEmails})
			for index := respelled; index < flanked; index++ {
				spelling := spellings[index]
				spelling.flanking = true
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
// reads back under inlineMarkdown, so that no run another writing stores changes its bytes, in
// the order it tries them: the flanking spellings of spellings, the list runSpellings gave, again
// with a literal backslash right before a character reference written `\\`, where it would
// otherwise escape the reference's `&`; the delimiter escapes runSpellings did not try, with an
// asterisk run beside a bold or italic code span counted as one beside a bold or italic text
// (delimitersInText's lastHeld for its held); then the escapes from the fewest with each escaped
// tilde written `\~`, which the browser editor reads as a tilde beside which a `*` run may open or
// close, where its reference ends in `;`; then with the marks ordered by reach (byReach); then
// reopened (reopening); last, where the run writes a bare URL (bareURL), the escapes from the
// fewest with each bare URL written as text whose scheme colon is escaped (bareURLColons).
func lastResorts(needs runNeeds, spellings []runSpelling) []runSpelling {
	escapes := escapesFrom(needs.lastHeld)
	var resorts []runSpelling
	for _, spelling := range spellings {
		if spelling.flanking {
			spelling.pairBackslash = true
			resorts = append(resorts, spelling)
		}
	}
	if needs.held == delimitersAsRuled && needs.lastHeld != delimitersAsRuled {
		for _, delimiters := range escapes[1:] {
			resorts = append(resorts, runSpelling{escapes: delimiters})
		}
	}
	for _, delimiters := range escapes {
		if delimiters >= delimitersTildes {
			resorts = append(resorts, runSpelling{escapes: delimiters, tildeBackslash: true})
		}
	}
	for _, order := range []markOrder{marksByReach, marksReopened} {
		for _, delimiters := range escapes {
			resorts = append(resorts, runSpelling{escapes: delimiters, order: order})
		}
	}
	if needs.bareURL {
		for _, delimiters := range escapes {
			resorts = append(resorts, runSpelling{escapes: delimiters, bareURLColons: true})
		}
	}
	return resorts
}

// orderMarks orders next, the marks the text nodes[index] is written under, and returns it with
// following, the marks of the text after it (empty after the last), ordered as spelling.order
// orders a run's marks (markOrder). marksAsWritten leaves both in their written order.
// marksKeptOpen keeps the marks written open before a text open around it, then opens the marks
// the next text still carries (keepingOpen). marksByReach also keeps them open, then opens
// outermost the mark the texts after it carry furthest (byReach). marksReopened closes the marks a
// text shares with the one before and opens them again inside its new ones (reopening). following
// is ordered with next as the marks written open before it, so next is ordered first, and each
// order moves a code span's mark innermost itself (codeInnermost), since the ordering after it
// reads its result that way.
func orderMarks(active, next []Mark, nodes []*Node, index int, escapePipes, wwwBare bool, spelling runSpelling) ([]Mark, []Mark) {
	var following []Mark
	if index+1 < len(nodes) {
		following = spelledMarks(nodes[index+1], escapePipes, wwwBare, spelling)
	}
	switch spelling.order {
	case marksKeptOpen:
		next = keepingOpen(active, next, following)
		if index+1 < len(nodes) {
			var after []Mark
			if index+2 < len(nodes) {
				after = spelledMarks(nodes[index+2], escapePipes, wwwBare, spelling)
			}
			following = keepingOpen(next, following, after)
		}
	case marksByReach:
		next = byReach(active, next, nodes, index, escapePipes, wwwBare, spelling)
		if index+1 < len(nodes) {
			following = byReach(next, following, nodes, index+1, escapePipes, wwwBare, spelling)
		}
	case marksReopened:
		next = reopening(active, next)
		following = reopening(next, following)
	}
	return next, following
}

// keepingOpen orders marks, the marks a text is written under, so that no mark it or the text
// after it still carries is closed and opened again between them: the longest run of active, the
// marks written open before it, from the outermost, whose every mark it carries comes first, as
// active holds them; then its other marks that following, the next text's marks, carries; then
// the rest, each group in its written order, opened inside the one before; and a code span's
// mark, whose span holds no syntax, innermost (codeInnermost).
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

// byReach orders marks, the marks nodes[index] is written under in spelling, so that a mark the
// texts after it carry further is opened outside one they carry less far, and is not closed and
// opened again where the nearer one ends (`***a**b*`, never `***a****b*`): the longest run of
// active, the marks written open before it, from the outermost, whose every mark it carries comes
// first, as active holds them; then its other marks, the one carried over the most texts in a row
// outermost, ties in their written order; and a code span's mark innermost (codeInnermost).
func byReach(active, marks []Mark, nodes []*Node, index int, escapePipes, wwwBare bool, spelling runSpelling) []Mark {
	kept := sharedOpenMarks(active, marks)
	ordered := append(make([]Mark, 0, len(marks)), active[:kept]...)
	// rest is each of the text's other marks with how many texts from its own, that one counted,
	// carry it in a row.
	type reach struct {
		mark  Mark
		texts int
	}
	rest := make([]reach, 0, len(marks))
	for _, mark := range marks {
		if !containsSameMark(active[:kept], mark) {
			rest = append(rest, reach{mark: mark, texts: 1})
		}
	}
	// The texts after are read in order, each once, while a mark still reaches them: a mark whose
	// reach is the texts read so far carries on into the next one or stops there.
	for texts, reaching := 1, len(rest); reaching > 0 && index+texts < len(nodes); texts++ {
		later := spelledMarks(nodes[index+texts], escapePipes, wwwBare, spelling)
		reaching = 0
		for at := range rest {
			if rest[at].texts == texts && containsSameMark(later, rest[at].mark) {
				rest[at].texts++
				reaching++
			}
		}
	}
	slices.SortStableFunc(rest, func(a, b reach) int { return cmp.Compare(b.texts, a.texts) })
	for _, carried := range rest {
		ordered = append(ordered, carried.mark)
	}
	return codeInnermost(ordered)
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

// nestsMarks reports whether a text among nodes is under more than one mark. keepingOpen orders
// each text's marks, so where every text has one at most, a spelling that keeps marks open writes
// what the same spelling without it does.
func nestsMarks(nodes []*Node) bool {
	for _, node := range nodes {
		if node.Type != "text" {
			continue
		}
		rendered := 0
		for _, mark := range node.Marks {
			if renderedMarkTypes[mark.Type] {
				rendered++
			}
		}
		if rendered > 1 {
			return true
		}
	}
	return false
}

// writesBareURL reports whether nodes write a link as its bare URL (isBareURLLink, isBareWWWLink),
// which the readers read again from the text alone.
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

// writesBareEmail reports whether nodes hold an email link the bareEmails spelling writes as its
// address (isBareEmailLink).
func writesBareEmail(nodes []*Node) bool {
	for _, node := range nodes {
		if node.Type == "text" && isBareEmailLink(node, node.Marks) {
			return true
		}
	}
	return false
}

// spelledMarks is the marks spelling writes node's text under: its writtenMarks, less an email
// link's where the spelling writes it as its address (runSpelling.bareEmails, isBareEmailLink).
func spelledMarks(node *Node, escapePipes, wwwBare bool, spelling runSpelling) []Mark {
	marks := writtenMarks(node, escapePipes, wwwBare)
	if spelling.bareEmails && isBareEmailLink(node, marks) {
		marks = withoutMark(marks, "link")
	}
	return marks
}

// writesAddressSyntax reports whether text outside a link or code among nodes holds a URL scheme's
// colon (urlSchemeColon), a dot after `www` (wwwDot) and an `@`, where the readers may read an
// address from plain text.
func writesAddressSyntax(nodes []*Node) (colons, dots, ats bool) {
	for _, node := range nodes {
		if node.Type != "text" || nodeHasMark(node, "inlineCode") || nodeHasMark(node, "link") {
			continue
		}
		for offset := range len(node.Text) {
			colons = colons || node.Text[offset] == ':' && urlSchemeColon(node.Text, offset)
			dots = dots || node.Text[offset] == '.' && wwwDot(node.Text, offset)
			ats = ats || node.Text[offset] == '@'
			if colons && dots && ats {
				return colons, dots, ats
			}
		}
	}
	return colons, dots, ats
}

// readsBackUnder is how many of readBackReaders, from the first, the inline markdown
// written since from reads back under as the nodes whose footnote labels are labels and whose
// inlineSignature is signature: the run's own lines, with the prefix its later lines are written
// behind taken off, read after a definition for each label, since a reference reads as one only
// then. It stops at the first reader the run does not read back under, and makes no read that is
// the same parse as one it made: storedMarkdown reads a source holding no `~`, its labels included,
// as inlineMarkdown does, since editorRules and storedRules differ only beside a `~` (a `*` or `_`
// run's widening by one, tildeParser, tildeDelimiters, and the `~` runs' own group in
// delimiterList), so that parse's nodes, error and divergence are inlineMarkdown's; and
// flankingMarkdown reads a source as storedMarkdown does unless the stored parse reported a `*` or
// `_` run that could open or close otherwise under flankingRules (parseInlineWithDefinitions).
func (r *renderer) readsBackUnder(from int, prefix string, labels, signature []string) int {
	written := r.b.Bytes()[from:]
	var lines strings.Builder
	lines.Grow(len(written))
	for start := 0; start < len(written); {
		stop, next := blockLine(written, start, prefix)
		lines.Write(written[start:stop])
		start = next
	}
	source := lines.String()
	readsBack := func(reader inlineReader) (bool, bool) {
		if readBackObserver != nil {
			readBackObserver(source, reader)
		}
		parsed, diverged, err := parseInlineWithDefinitions(source, labels, reader)
		return err == nil && slices.Equal(inlineSignature(parsed), signature), diverged
	}
	reads, diverged := readsBack(readBackReaders[0])
	if !reads {
		return 0
	}
	if holdsTilde(source, labels) {
		if reads, diverged = readsBack(readBackReaders[1]); !reads {
			return 1
		}
	}
	if diverged {
		if reads, _ = readsBack(readBackReaders[2]); !reads {
			return 2
		}
	}
	return 3
}

// readBackObserver, where a test sets it, is given each markdown readsBackUnder parses and the
// reader it parses it under.
var readBackObserver func(markdown string, reader inlineReader)

// holdsTilde reports whether a run's source or any of the footnote labels whose definitions are
// read after it holds a `~`.
func holdsTilde(source string, labels []string) bool {
	return strings.Contains(source, "~") || slices.ContainsFunc(labels, func(label string) bool { return strings.Contains(label, "~") })
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

// letterBesideDelimiter reports whether a flanking spelling can write a character of nodes as a
// reference (writeInlineNodes' flankFirst and flankLast): a text's first character, not
// flankingNeutral, after a text under a delimiter mark that carries every delimiter and code mark
// it does, so that the marks it closes there are the text's alone; or its last such character
// before a text so, which opens a delimiter run before whitespace, punctuation, a link or code.
// Where there is none, each flanking spelling writes the run as the spelling without flanking does.
func letterBesideDelimiter(nodes []*Node) bool {
	for index, node := range nodes {
		if node.Type != "text" {
			continue
		}
		if index > 0 && closesOrOpensDelimiterBeside(nodes[index-1], node) && !flankingNeutral(firstRune(node.Text)) {
			return true
		}
		if index+1 < len(nodes) && closesOrOpensDelimiterBeside(nodes[index+1], node) && !flankingNeutral(lastRune(node.Text)) &&
			(flankingNeutral(firstRune(nodes[index+1].Text)) || nodeHasMark(nodes[index+1], "link") || nodeHasMark(nodes[index+1], "inlineCode")) {
			return true
		}
	}
	return false
}

// closesOrOpensDelimiterBeside reports whether beside, the text before or after text, is under a
// delimiter mark and under every delimiter and code mark text is: where the writer closes or opens
// only marks text does not carry between them, one of them a delimiter run.
func closesOrOpensDelimiterBeside(beside, text *Node) bool {
	if beside.Type != "text" || !hasDelimiterMark(beside.Marks) {
		return false
	}
	for _, mark := range text.Marks {
		if (markDelimiter(mark) != 0 || mark.Type == "inlineCode") && !containsSameMark(beside.Marks, mark) {
			return false
		}
	}
	return true
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
