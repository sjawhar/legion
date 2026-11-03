package main

import (
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// --- edits -----------------------------------------------------------------------------------------
//
// What POST /api/v1/artifacts/{id}/edits accepts at Dispatch 35efd326 (internal/dispatch/docs/edits.go
// applyOperation, findEditQuote; pmdoc/find.go FindQuote, TargetSpansBlocks), and what an edit planned
// here therefore holds to:
//
//   - A replace's or a delete's find is matched against the document's text as rendered: the text of
//     every text node, concatenated, a space between two that do not touch. Markdown syntax is not in
//     that text: a find holding a list marker, a heading marker, an escape (`\_`) or a table pipe
//     matches nothing (404 TARGET_NOT_FOUND) unless its markdown reading holds inline markup, and one
//     reaching across a line break between blocks spans blocks (400 TARGET_SPANS_BLOCKS).
//   - The find must match once (409 TARGET_AMBIGUOUS) and inside one textblock (400
//     TARGET_SPANS_BLOCKS).
//   - A replace's with is inline markdown: one forming two paragraphs, or opening with the matched
//     block's own marker, is refused (400 INVALID_OP). Text written over a marked span (code,
//     emphasis, a link) loses the mark, and a find covering a whole textblock with an empty with
//     deletes the block.
//   - What the document then holds is its canonical rendering.
//
// So every op planned here replaces a plain run of one line, a run Dispatch renders exactly as the
// markdown writes it, outside every inline mark and inside one textblock (plainBytes), by plain
// text that is never empty: Dispatch then holds the client's text with the find replaced, byte for
// byte (replay_edit_bundle_test.go checks it over a bundle).

// replayEditOp is one op of POST /api/v1/artifacts/{id}/edits: a replace of find by with, or an
// insert of markdown after the top-level block holding the quote after.
type replayEditOp struct {
	Op       string  `json:"op"`
	Find     string  `json:"find,omitempty"`
	With     *string `json:"with,omitempty"`
	After    string  `json:"after,omitempty"`
	Markdown string  `json:"markdown,omitempty"`
}

// planEdit is a version's edit of text: the logged diff's changes as ops when the diff is complete
// (not cut at production's 4,094 characters) and every change can be one (diffOps), else
// substitute replaces whose length change is delta, the logged size change (substituteEdit). It
// returns the ops, the text after them, whether they came from the diff, and false when no edit can
// be made.
func planEdit(text, diff string, delta int, rngSeed uint64, docID, itemID string) ([]replayEditOp, string, bool, bool) {
	if diff != "" && !diffCut(diff) {
		if ops, next, ok := diffOps(text, diff); ok {
			return ops, next, true, true
		}
	}
	ops, next, ok := substituteEdit(text, delta, rngSeed, docID, itemID)
	if !ok {
		return nil, text, false, false
	}
	return ops, next, false, true
}

// diffCut says whether production cut the diff: it ends in the "… (truncated)" line or reaches the
// cap.
func diffCut(diff string) bool {
	if len(diff) >= replayDiffCap {
		return true
	}
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	return strings.HasPrefix(lines[len(lines)-1], "… (")
}

// diffRun is one run of a hunk's changed lines: the lines it removes and the lines it adds, at the
// old text's line at.
type diffRun struct {
	at             int
	removed, added []string
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@`)

// diffRuns reads a unified diff's runs of changed lines, each placed by its hunk's old line numbers
// in lines, the text it applies to; false when the diff is not one, or when a context or removed
// line is not the line lines holds there (the text is not the version the diff was taken from).
func diffRuns(lines []string, diff string) ([]diffRun, bool) {
	var runs []diffRun
	open := false // whether the last run takes the next changed line
	old := -1     // the old line the next context or removed line is; -1 before the first hunk
	for _, l := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			m := hunkHeader.FindStringSubmatch(l)
			if m == nil {
				return nil, false
			}
			start, _ := strconv.Atoi(m[1])
			old, open = max(0, start-1), false
			if m[2] == "0" {
				old = start
			}
		case old < 0, strings.HasPrefix(l, `\`):
		case l == "" || l[0] == ' ' || l[0] == '-':
			text := ""
			if l != "" {
				text = l[1:]
			}
			if old >= len(lines) || lines[old] != text {
				return nil, false
			}
			switch {
			case l == "" || l[0] == ' ':
				open = false
			case !open || len(runs[len(runs)-1].added) > 0:
				runs, open = append(runs, diffRun{at: old, removed: []string{text}}), true
			default:
				runs[len(runs)-1].removed = append(runs[len(runs)-1].removed, text)
			}
			old++
		case l[0] == '+':
			if !open {
				runs, open = append(runs, diffRun{at: old}), true
			}
			runs[len(runs)-1].added = append(runs[len(runs)-1].added, l[1:])
		default:
			return nil, false
		}
	}
	return runs, len(runs) > 0
}

// diffOps turns a unified diff into ops: a run of changed lines removing as many lines as it adds is
// one replace for each line it changes, the k-th removed line becoming the k-th added one (lineOp),
// and a run adding lines alone is one insert (insertOp). A diff with any other change, or a change
// neither can make, is not one diffOps can make.
func diffOps(text, diff string) ([]replayEditOp, string, bool) {
	lines := strings.Split(text, "\n")
	runs, ok := diffRuns(lines, diff)
	if !ok {
		return nil, "", false
	}
	var ops []replayEditOp
	shift := 0 // the lines the runs before added, less those they removed
	for _, run := range runs {
		at := run.at + shift
		switch {
		case len(run.removed) == len(run.added):
			for k, a := range run.added {
				if run.removed[k] == a {
					continue
				}
				find, with, ok := lineOp(scanMarkdown(strings.Join(lines, "\n")), at+k, a)
				if !ok {
					return nil, "", false
				}
				lines[at+k] = a
				ops = append(ops, replayEditOp{Op: "replace", Find: find, With: &with})
			}
		case len(run.removed) == 0:
			op, ok := insertOp(scanMarkdown(strings.Join(lines, "\n")), at, run.added)
			if !ok {
				return nil, "", false
			}
			lines = slices.Insert(lines, at, run.added...)
			ops = append(ops, op)
		default:
			return nil, "", false
		}
		shift += len(run.added) - len(run.removed)
	}
	return ops, strings.Join(lines, "\n"), true
}

// lineOp is the replace turning line li of d into a: the run of the line from the first to the
// last byte where it and a differ, widened to whole words, then a word at a time while its find is
// not unique, its with empty or its find blank. Both runs must be plain (the line's in d, a's read
// as a line of its own), and a with opening the block's text must open with a letter or a digit
// that starts no list item (opensText).
func lineOp(d *mdDoc, li int, a string) (find, with string, ok bool) {
	l := d.lines[li]
	r := l.text
	lPlain := d.plain(li)
	aContent, aPlain := plainBytes(a)
	if lPlain == nil || aPlain == nil {
		return "", "", false
	}
	p := 0
	for p < len(r) && p < len(a) && r[p] == a[p] {
		p++
	}
	q := 0
	for q < len(r)-p && q < len(a)-p && r[len(r)-1-q] == a[len(a)-1-q] {
		q++
	}
	shift := len(a) - len(r)
	s, e := wordStart(r, p), wordEnd(r, len(r)-q)
	for {
		if !allPlain(lPlain, s, e) || !allPlain(aPlain, s, e+shift) {
			return "", "", false
		}
		find, with = r[s:e], a[s:e+shift]
		if strings.TrimSpace(find) != "" && with != "" && (s > aContent || opensText(with)) && d.unique(find, l.start+s) {
			return find, with, true
		}
		// Widen right by a word, else left; a run that can widen neither way is not unique.
		if ne := wordEnd(r, skipSpaces(r, e)); ne > e && allPlain(lPlain, s, ne) && allPlain(aPlain, s, ne+shift) {
			e = ne
		} else if ns := wordStart(r, skipSpacesBack(r, s)); ns < s && allPlain(lPlain, ns, e) && allPlain(aPlain, ns, e+shift) {
			s = ns
		} else {
			return "", "", false
		}
	}
}

// insertOp is the insert of a run adding lines before d's line at: the lines less the blank line at
// one end that parts them from a top-level paragraph or heading of one line between blank lines,
// inserted after it and anchored on a plain run of it that is unique (anchorRun). Dispatch writes
// an insert after the top-level block holding its anchor, a blank line apart, so the text it then
// holds is the text the diff gives. The lines hold no directive (an ask block is the asks route's),
// and a list they end with must not meet a list after them, which it would join.
func insertOp(d *mdDoc, at int, added []string) (replayEditOp, bool) {
	n, lines := len(added), d.lines
	blank := func(i int) bool { return i >= len(lines) || lines[i].text == "" }
	var j int
	var frag []string
	switch {
	case n >= 2 && added[0] == "" && added[n-1] != "" && at > 0 && !blank(at-1) && blank(at):
		j, frag = at-1, added[1:]
	case n >= 2 && added[n-1] == "" && added[0] != "" && at >= 2 && blank(at-1) && !blank(at):
		j, frag = at-2, added[:n-1]
	default:
		return replayEditOp{}, false
	}
	if frag[0] == "" || frag[len(frag)-1] == "" || slices.ContainsFunc(frag, func(f string) bool { return strings.HasPrefix(strings.TrimSpace(f), ":::") }) {
		return replayEditOp{}, false
	}
	l := lines[j].text
	if !lines[j].editable || (j > 0 && !blank(j-1)) || !blank(j+1) || strings.HasPrefix(l, "|") || (blockStart(l) > 0 && !strings.HasPrefix(l, "#")) {
		return replayEditOp{}, false
	}
	if listItem.MatchString(frag[len(frag)-1]) && j+2 < len(lines) && listItem.MatchString(lines[j+2].text) {
		return replayEditOp{}, false
	}
	quote, ok := d.anchorRun(j)
	if !ok {
		return replayEditOp{}, false
	}
	return replayEditOp{Op: "insert", After: quote, Markdown: strings.Join(frag, "\n")}, true
}

var listItem = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])(?:\s|$)`)

// anchorRun is the longest plain run of line li, its edge spaces trimmed, that is unique.
func (d *mdDoc) anchorRun(li int) (string, bool) {
	plain, text := d.plain(li), d.lines[li].text
	type run struct{ s, e int }
	var runs []run
	for i := 0; i < len(plain); {
		if !plain[i] {
			i++
			continue
		}
		s := i
		for i < len(plain) && plain[i] {
			i++
		}
		s, e := skipSpaces(text[:i], s), skipSpacesBack(text, i)
		if e > s {
			runs = append(runs, run{s, e})
		}
	}
	slices.SortStableFunc(runs, func(a, b run) int { return (b.e - b.s) - (a.e - a.s) })
	for _, r := range runs {
		if d.unique(text[r.s:r.e], d.lines[li].start+r.s) {
			return text[r.s:r.e], true
		}
	}
	return "", false
}

// opensText says whether with can open a block's text: it starts with a letter or a digit, and not
// with a number and a period or parenthesis, which Dispatch writes escaped.
func opensText(with string) bool {
	r, _ := utf8.DecodeRuneInString(with)
	return (unicode.IsLetter(r) || unicode.IsDigit(r)) && !orderedStart.MatchString(with)
}

var orderedStart = regexp.MustCompile(`^\d+[.)]`)

func wordStart(s string, i int) int {
	for i > 0 && s[i-1] != ' ' {
		i--
	}
	return i
}

func wordEnd(s string, i int) int {
	for i < len(s) && s[i] != ' ' {
		i++
	}
	return i
}

func skipSpaces(s string, i int) int {
	for i < len(s) && s[i] == ' ' {
		i++
	}
	return i
}

func skipSpacesBack(s string, i int) int {
	for i > 0 && s[i-1] == ' ' {
		i--
	}
	return i
}

func allPlain(plain []bool, s, e int) bool {
	if s < 0 || e > len(plain) || s >= e {
		return s == e && s >= 0 && e <= len(plain)
	}
	for _, p := range plain[s:e] {
		if !p {
			return false
		}
	}
	return true
}

// substituteEdit replaces unique sentences of text (fallbackCandidates, fallbackOrder's first, then
// the rest in text order), each one whose occurrence is a plain run, by synthetic words: one
// sentence by words of its length plus delta, or, for a shrink past what a sentence can give while
// keeping a byte, as many sentences as the shrink takes. A shrink larger than all of them shrinks
// what they hold.
func substituteEdit(text string, delta int, rngSeed uint64, docID, itemID string) ([]replayEditOp, string, bool) {
	cands := fallbackCandidates(text)
	order := fallbackOrder(rngSeed, docID, itemID, cands)
	tried := map[string]bool{}
	for _, c := range order {
		tried[c] = true
	}
	for _, c := range cands {
		if !tried[c] {
			order = append(order, c)
		}
	}
	salt := int(replayHash(rngSeed, "edit", itemID) % 1000)
	// The sentences are distinct and the synthetic words hold no syntax, so a sentence's plain run
	// in text stays one in the text the ops before it left; it must still occur there once.
	d := scanMarkdown(text)
	cur, rest := text, delta
	var ops []replayEditOp
	for _, c := range order {
		if at := strings.Index(text, c); !d.plainRun(at, at+len(c)) || !d.unique(c, at) || (len(ops) > 0 && strings.Count(cur, c) != 1) {
			continue
		}
		at := strings.Index(cur, c)
		n := max(1, len(c)+rest)
		with := padText(n, salt+len(ops))
		ops = append(ops, replayEditOp{Op: "replace", Find: c, With: &with})
		cur, rest = cur[:at]+with+cur[at+len(c):], rest-(n-len(c))
		if rest >= 0 {
			break
		}
	}
	return ops, cur, len(ops) > 0
}

// --- the markdown as Dispatch's find reads it -----------------------------------------------------

// mdLine is one line of a document: its offset in the text, the line, and whether it can hold a
// plain run at all: no line in a code fence, front matter or a directive block, nor a directive
// line, can.
type mdLine struct {
	start    int
	text     string
	editable bool
}

// mdDoc is a document's lines and its rendered text as far as a find can match it (rendered): the
// lines' text with block markers, inline syntax and link destinations dropped and a table's cells
// trimmed, every line a space apart. It holds every occurrence of a plain run the rendered text
// holds, and possibly more. masks caches plainBytes of the lines asked about.
type mdDoc struct {
	text     string
	lines    []mdLine
	rendered string
	masks    map[int][]bool
}

func scanMarkdown(text string) *mdDoc {
	d := &mdDoc{text: text, masks: map[int][]bool{}}
	var rendered strings.Builder
	inFence, inFront := false, false
	directive, start := 0, 0
	for i, line := range strings.Split(text, "\n") {
		l := mdLine{start: start, text: line}
		start += len(line) + 1
		trimmed := strings.TrimSpace(line)
		switch {
		case i == 0 && trimmed == "---":
			inFront = true
		case inFront:
			inFront = trimmed != "---"
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			inFence = !inFence
		case inFence:
			rendered.WriteString(line + " ")
		case strings.HasPrefix(trimmed, ":::"):
			if trimmed == ":::" {
				directive = max(0, directive-1)
			} else {
				directive++
			}
		default:
			l.editable = directive == 0
			if t := renderedLine(line[blockStart(line):]); t != "" {
				rendered.WriteString(t + " ")
			}
		}
		d.lines = append(d.lines, l)
	}
	d.rendered = rendered.String()
	return d
}

// plain is line i's plain bytes (plainBytes), nil when it has none.
func (d *mdDoc) plain(i int) []bool {
	if !d.lines[i].editable {
		return nil
	}
	m, ok := d.masks[i]
	if !ok {
		_, m = plainBytes(d.lines[i].text)
		d.masks[i] = m
	}
	return m
}

// unique says whether find occurs once in the text, at at, and once in the rendered text.
func (d *mdDoc) unique(find string, at int) bool {
	return strings.Count(d.text, find) == 1 && strings.Index(d.text, find) == at && strings.Count(d.rendered, find) == 1
}

// plainRun says whether the text's bytes [from, to) are a plain run of one line.
func (d *mdDoc) plainRun(from, to int) bool {
	i := sort.Search(len(d.lines), func(i int) bool { return d.lines[i].start > from }) - 1
	if i < 0 || to > d.lines[i].start+len(d.lines[i].text) {
		return false
	}
	return allPlain(d.plain(i), from-d.lines[i].start, to-d.lines[i].start)
}

// inlineSyntax are the characters that open or close an inline mark, an autolink, inline HTML or
// an entity, or escape, or part a table's cells: never plain. A "!" opening an image is not either.
const inlineSyntax = "`*_~[]<>\\|&@"

var (
	// blockPrefix is one block marker at the start of a line's remaining text: a task or list item,
	// a block quote, or an ATX heading, with the space after it.
	blockPrefix = regexp.MustCompile(`^(?:[-*+][ \t]+\[[ xX]\]|[-*+]|\d+[.)]|>|#{1,6})(?:[ \t]+|$)`)
	// linkDefinition is a link reference definition's opening.
	linkDefinition = regexp.MustCompile(`^ {0,3}\[[^\]]+\]:`)
)

// blockStart is the offset of a line's block text: past its indentation and its block markers.
func blockStart(line string) int {
	c := len(line) - len(strings.TrimLeft(line, " "))
	for {
		loc := blockPrefix.FindStringIndex(line[c:])
		if loc == nil || loc[1] == 0 {
			return c
		}
		c += loc[1]
	}
}

// plainBytes is the offset of a line's block text (blockStart) and which of its bytes are plain:
// rendered as written, outside every inline mark, inside one textblock. It is nil when no byte is:
// indented code, a thematic break, a blank line, a table's delimiter row or a link reference
// definition. It reads the line on its own, outside any fence or directive.
func plainBytes(line string) (int, []bool) {
	if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") || strings.Trim(line, "-*_= \t") == "" ||
		(strings.HasPrefix(strings.TrimLeft(line, " "), "[") && linkDefinition.MatchString(line)) {
		return 0, nil
	}
	c := blockStart(line)
	// A table's delimiter row.
	if strings.Trim(line[c:], "|:- \t") == "" {
		return c, nil
	}
	plain := make([]bool, len(line))
	for i := c; i < len(line); i++ {
		plain[i] = line[i] != '\t' && strings.IndexByte(inlineSyntax, line[i]) < 0 && !(line[i] == '!' && i+1 < len(line) && line[i+1] == '[')
	}
	// An escape and the character it escapes.
	for i := c; i < len(line)-1; i++ {
		if line[i] == '\\' {
			plain[i+1] = false
			i++
		}
	}
	// A code span: from a backtick run to the next run of its length.
	for i := c; i < len(line); {
		if line[i] != '`' || (i > 0 && line[i-1] == '\\') {
			i++
			continue
		}
		n := runLen(line, i, '`')
		end := closingRun(line, i+n, '`', n)
		if end < 0 {
			i += n
			continue
		}
		clear(plain[i:end])
		i = end
	}
	// Emphasis and strikethrough: everything from the first unescaped delimiter of a kind to its last.
	for _, delim := range []byte{'*', '_', '~'} {
		first, last := -1, -1
		for i := c; i < len(line); i++ {
			if line[i] == delim && (i == 0 || line[i-1] != '\\') {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		if first >= 0 {
			clear(plain[first : last+1])
		}
	}
	// A link's or an image's text and destination, an autolink or inline HTML.
	for i := c; i < len(line); i++ {
		switch line[i] {
		case '[':
			if end := matching(line, i, '[', ']'); end > 0 {
				if end+1 < len(line) && line[end+1] == '(' {
					if close := matching(line, end+1, '(', ')'); close > 0 {
						end = close
					}
				}
				clear(plain[i : end+1])
				i = end
			}
		case '<':
			if end := strings.IndexByte(line[i:], '>'); end > 0 {
				clear(plain[i : i+end+1])
				i += end
			}
		}
	}
	// A bare URL or email address, which Dispatch links: a word holding "://" or "@", or starting
	// "www.".
	for i := c; i < len(line); {
		j := wordEnd(line, i)
		if w := line[i:j]; strings.Contains(w, "://") || strings.Contains(w, "@") || strings.HasPrefix(w, "www.") {
			clear(plain[i:j])
		}
		i = skipSpaces(line, j)
	}
	// Whitespace at the block text's edges and at a table cell's is no text of the block.
	for i := c; i < len(line); i++ {
		if line[i] != ' ' {
			continue
		}
		j := skipSpaces(line, i)
		if i == c || j == len(line) || line[i-1] == '|' || line[j] == '|' {
			clear(plain[i:j])
		}
		i = j - 1
	}
	return c, plain
}

func runLen(s string, i int, b byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == b {
		n++
	}
	return n
}

// closingRun is the end of the first run of exactly n bs at or after i, or -1.
func closingRun(s string, i int, b byte, n int) int {
	for i < len(s) {
		if s[i] != b {
			i++
			continue
		}
		m := runLen(s, i, b)
		if m == n {
			return i + m
		}
		i += m
	}
	return -1
}

// matching is the offset of the close matching the open at i, or -1.
func matching(s string, i int, open, close byte) int {
	depth := 0
	for j := i; j < len(s); j++ {
		switch {
		case s[j] == '\\':
			j++
		case s[j] == open:
			depth++
		case s[j] == close:
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// renderedLine is a line's block text as the rendered text holds it, roughly: a table row's cells
// trimmed and a space apart, link destinations and inline HTML dropped, every other syntax
// character and escape dropped.
func renderedLine(s string) string {
	if strings.HasPrefix(s, "|") {
		cells := strings.Split(strings.Trim(s, "| "), "|")
		for i := range cells {
			cells[i] = renderedLine(strings.TrimSpace(cells[i]))
		}
		return strings.Join(cells, " ")
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; {
		case ch == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case ch == ']' && i+1 < len(s) && s[i+1] == '(':
			if close := matching(s, i+1, '(', ')'); close > 0 {
				i = close
			}
		case ch == '<':
			if end := strings.IndexByte(s[i:], '>'); end > 0 {
				i += end
			}
		case strings.IndexByte("`*_~[]|", ch) >= 0:
		default:
			b.WriteByte(ch)
		}
	}
	return strings.TrimSpace(b.String())
}
