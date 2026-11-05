package main

// Two of the replay seed's decisions, pure like replay_seed_plan.go: where an anchor lands when its
// logged quote cannot hold it (chooseAnchor, also what the replay source calls for the window's
// anchors), and the size every synthetic history version must have so that each version changes
// as many bytes as the production version it stands for (planHistory).

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// The branches chooseAnchor takes, by name, as replay-fetch counts them, the seed records them
// (loadtest.replay_anchors) and replay-check-seed reports them.
const (
	ruleLoggedQuote  = "logged_quote"
	ruleInBlock      = "in_logged_block"
	ruleNearest      = "nearest_surviving_block"
	ruleNoBlockMap   = "no_block_map"
	ruleUnanchorable = "unanchorable"
)

// anchorChoice is where an anchor lands: the branch, the block chosen (its id; empty without a
// block map), its span [Start, End) in the document's text at At, and the quotes to try in order.
type anchorChoice struct {
	Branch     string
	BlockID    string
	Start, End int
	Candidates []string
	// Occurrence, when set, is the zero-based occurrence of the single candidate (the logged
	// quote, repeated in the text) that lies inside the span: the anchor input's occurrence.
	// QuoteAt is that occurrence's byte offset in the text.
	Occurrence *int
	QuoteAt    int
}

// find is the top-level index of a block id and the text it stands for: the block's own, or a
// table row's (the table's) or cell's.
func (bm *replayBlockMap) find(id string) (int, string, bool) {
	if bm == nil || id == "" {
		return -1, "", false
	}
	for i, b := range bm.Blocks {
		if b.ID == id {
			return i, b.Text, true
		}
		for _, in := range b.Inner {
			if in.ID == id {
				if in.Text == "" {
					return i, b.Text, true
				}
				return i, in.Text, true
			}
		}
	}
	return -1, "", false
}

// spanCandidates are the fallback sentences inside text[start:end) that occur once in the whole
// text.
func spanCandidates(text string, start, end int) []string {
	var out []string
	for _, c := range fallbackCandidates(text[start:end]) {
		if strings.Count(text, c) == 1 {
			out = append(out, c)
		}
	}
	return out
}

// locate is the span of block text in the document's text when it occurs there exactly once.
func locate(text, block string) (int, int, bool) {
	if strings.TrimSpace(block) == "" || strings.Count(text, block) != 1 {
		return 0, 0, false
	}
	i := strings.Index(text, block)
	return i, i + len(block), true
}

// chooseAnchor decides where an anchored comment or ask lands in text, its document's markdown
// at At, given the logged anchor (its block id, quote and orphaned flag) and the document's
// current blocks bm. The rule, in order:
//
//  1. logged_quote: the logged block's current text occurs once in text, the logged quote is not
//     orphaned, occurs once in text and lies inside that block's span: the logged quote.
//  2. in_logged_block: the logged block's current text occurs once in text: a sentence inside
//     that span that is unique in the whole text (fallbackCandidates), drawn by fallbackOrder.
//  3. nearest_surviving_block: otherwise (the block's text is not in text, holds no usable
//     sentence, or its id is not among the current blocks): the nearest block in current order
//     (ties: the earlier) whose text occurs once in text and holds a usable unique sentence,
//     measured from the logged block; when its id is absent, from the block whose text holds the
//     logged quote, else from the block at the logged quote's relative position in text (the
//     document's middle when the quote is not in text either).
//
// Without a block map the whole text is the span (no_block_map); with no usable sentence anywhere
// the anchor is unanchorable. skipLogged drops branch 1, for a logged quote Dispatch refused.
func chooseAnchor(text string, bm *replayBlockMap, blockID, quote string, orphaned, skipLogged bool, rngSeed uint64, docID, itemID string) anchorChoice {
	pick := func(branch, id string, start, end int, cands []string) anchorChoice {
		return anchorChoice{Branch: branch, BlockID: id, Start: start, End: end, Candidates: fallbackOrder(rngSeed, docID, itemID, cands)}
	}
	useLogged := !skipLogged && !orphaned && quote != ""
	if bm == nil || len(bm.Blocks) == 0 {
		if useLogged && strings.Count(text, quote) == 1 {
			return anchorChoice{Branch: ruleNoBlockMap, Start: 0, End: len(text), Candidates: []string{quote}}
		}
		// fallbackCandidates already keeps only sentences unique in the whole text.
		if c := fallbackCandidates(text); len(c) > 0 {
			return pick(ruleNoBlockMap, "", 0, len(text), c)
		}
		return anchorChoice{Branch: ruleUnanchorable}
	}
	top, btext, found := bm.find(blockID)
	start, end, located := 0, 0, false
	if found {
		start, end, located = locate(text, btext)
	}
	// Rule 1: a logged quote that is not orphaned and occurs once in the text is no substitute, so
	// it is used whether or not its logged block survives. Its span is its block's when the quote
	// lies inside it, else the quote itself.
	if useLogged && strings.Count(text, quote) == 1 {
		q := strings.Index(text, quote)
		if located && q >= start && q+len(quote) <= end {
			return anchorChoice{Branch: ruleLoggedQuote, BlockID: blockID, Start: start, End: end, Candidates: []string{quote}}
		}
		return anchorChoice{Branch: ruleLoggedQuote, Start: q, End: q + len(quote), Candidates: []string{quote}}
	}
	// A logged quote repeated in the text but once inside its located block: that occurrence,
	// counted under in_logged_block.
	if located && useLogged {
		k, at, inside := 0, -1, 0
		for i, off := 0, 0; ; i++ {
			j := strings.Index(text[off:], quote)
			if j < 0 {
				break
			}
			if p := off + j; p >= start && p+len(quote) <= end {
				k, at = i, p
				inside++
			}
			off += j + 1
		}
		if inside == 1 {
			return anchorChoice{Branch: ruleInBlock, BlockID: blockID, Start: start, End: end, Candidates: []string{quote}, Occurrence: &k, QuoteAt: at}
		}
	}
	if located {
		if c := spanCandidates(text, start, end); len(c) > 0 {
			return pick(ruleInBlock, blockID, start, end, c)
		}
	}
	ref := top
	if !found {
		ref = refByQuote(text, bm, quote)
	}
	for d := 0; d < len(bm.Blocks); d++ {
		for _, i := range []int{ref - d, ref + d} {
			if i < 0 || i >= len(bm.Blocks) || (found && i == top && located) {
				continue
			}
			if s, e, ok := locate(text, bm.Blocks[i].Text); ok {
				if c := spanCandidates(text, s, e); len(c) > 0 {
					return pick(ruleNearest, bm.Blocks[i].ID, s, e, c)
				}
			}
			if d == 0 {
				break
			}
		}
	}
	return anchorChoice{Branch: ruleUnanchorable}
}

// refByQuote is where the search for the nearest surviving block starts when the logged block id
// is not among the current blocks: the first block whose text holds the logged quote, else the
// block at the quote's relative position in text (the middle when the quote is not in text).
func refByQuote(text string, bm *replayBlockMap, quote string) int {
	if quote != "" {
		for i, b := range bm.Blocks {
			if strings.Contains(b.Text, quote) {
				return i
			}
		}
	}
	rel := 0.5
	if i := strings.Index(text, quote); quote != "" && i >= 0 {
		rel = float64(i) / float64(len(text))
	}
	return min(len(bm.Blocks)-1, int(rel*float64(len(bm.Blocks))))
}

// substituteAnchor is chooseAnchor for the replay source's window anchors: given a document's
// text, the logged block id and quote, the first of the choice's candidates that the document's
// rendered text holds once (mdDoc.rendered: a sentence unique in the markdown recurs in a code
// block or where markup splits another copy of it, and Dispatch, matching the rendered text,
// answers 409 TARGET_AMBIGUOUS), else the first such sentence of the whole text in fallbackOrder,
// and the branch that chose it.
func substituteAnchor(text string, bm *replayBlockMap, blockID, quote string, rngSeed uint64, docID, itemID string) (string, string, error) {
	c := chooseAnchor(text, bm, blockID, quote, false, false, rngSeed, docID, itemID)
	rendered := scanMarkdown(text).rendered
	once := func(q string) bool { return strings.Count(rendered, q) == 1 }
	for _, q := range c.Candidates {
		if once(q) {
			return q, c.Branch, nil
		}
	}
	if all := fallbackOrder(rngSeed, docID, itemID, slices.DeleteFunc(fallbackCandidates(text), func(q string) bool { return !once(q) })); len(all) > 0 {
		return all[0], ruleNoBlockMap, nil
	}
	return "", c.Branch, fmt.Errorf("anchor %s on document %s has no usable quote", itemID, docID)
}

// --- history sizes -------------------------------------------------------------------------------

// historyPlan is how a document's synthetic history reproduces production's version sizes. Every
// version below Restore is the base (the text at At with the run [RunStart, RunEnd) removed and the
// typed ask blocks still to be answered or resolved stripped) plus a padding paragraph of synthetic
// words at the end (padAt), sized so the version has Sizes[n-1] bytes. Version Restore removes the
// padding and inserts the run back (before the heading at RunEnd, or at the end) in one edit, giving
// the text at At less the ask steps still to come. AskAt maps a version to the ask steps (a typed ask
// block's answer or resolve, a change Dispatch makes) it carries.
type historyPlan struct {
	Final            int
	Sizes            []int
	Restore          int
	RunStart, RunEnd int
	AskAt            map[int][]askStep
	// Flipped counts versions whose size change is production's with the sign flipped, because
	// the base cannot be made small enough; Missing the versions production lists no size for
	// (taken as unchanged).
	Flipped, Missing int
	// Unreachable counts versions no sign can reach: raised to the smallest size the base and a
	// padding paragraph take, so their change differs from production's.
	Unreachable int
}

// askStep is one write of a typed ask block that Dispatch versions: the answer of the issue's ask
// at index Ask, or its resolve.
type askStep struct {
	Ask     int
	Resolve bool
}

// padMin is the shortest padding paragraph a version carries.
const padMin = 8

// maxJSONBody is the largest JSON request body Dispatch takes (maxJSONRequestBytes at 35efd326;
// a larger one is refused 413 REQUEST_TOO_LARGE) and the largest document it stores
// (pmdoc.MaxDocumentBytes, 413 CAP_EXCEEDED).
const maxJSONBody = 1 << 20

// restoreEnvelope bounds what the restore edit's body holds beside its run's encoded markdown: the
// padding block's delete, the insert's anchor and the actor.
const restoreEnvelope = 1024

// padLen is the length of padded version n's padding paragraph: version n-1's (prevPad bytes)
// changed by the planned size change, so version n changes size by exactly the plan's change
// whatever size Dispatch stored version n-1 at: version 1's base canonicalised to other bytes than
// sent, or an ask step version whose change differs from production's. A padding paragraph is plain
// words, which Dispatch stores as sent. The stored padding, not the seeder's memory, is prevPad,
// so a resumed seed continues alike.
func (p historyPlan) padLen(n, prevPad int) int { return prevPad + p.Sizes[n-1] - p.Sizes[n-2] }

// padOverhead is what a padding paragraph adds to a document beyond its own bytes: the blank line
// before it and its line feed; one byte when the base is empty (no blank line).
func padOverhead(baseLen int) int {
	if baseLen == 0 {
		return 1
	}
	return 2
}

// atxHeading matches an ATX heading line.
var atxHeading = regexp.MustCompile(`^#{1,6} `)

// sectionStarts are the offsets in text where a removable run may start or end: 0 and each ATX
// heading line that follows a blank line outside code fences and directive blocks.
func sectionStarts(text string) []int {
	starts := []int{0}
	fence, directive, pos, prevBlank := false, 0, 0, true
	for _, line := range strings.Split(text, "\n") {
		s := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(s, "```") || strings.HasPrefix(s, "~~~"):
			fence = !fence
		case !fence && strings.HasPrefix(s, ":::"):
			if s == ":::" {
				directive = max(0, directive-1)
			} else {
				directive++
			}
		case !fence && directive == 0 && pos > 0 && prevBlank && atxHeading.MatchString(line):
			starts = append(starts, pos)
		}
		prevBlank = s == ""
		pos += len(line) + 1
	}
	return starts
}

// chooseRun picks the run of whole sections to remove. It considers the runs that hold no typed ask
// block and whose markdown, JSON-encoded, takes at most maxJSON bytes (the restore edit re-inserts
// the run whole, in one body): the shortest one of at least need bytes, else (none is long enough)
// the longest one. Both ends are section starts (the end may be the text's end). It returns 0, 0
// when need <= 0 or no run qualifies.
func chooseRun(text string, need, maxJSON int) (int, int) {
	if need <= 0 {
		return 0, 0
	}
	starts := append(sectionStarts(text), len(text))
	enc := jsonPrefix(text)
	bestS, bestE, longS, longE := 0, 0, 0, 0
	for i := 0; i < len(starts)-1; i++ {
		for j := i + 1; j < len(starts); j++ {
			if strings.Contains(text[starts[i]:starts[j]], ":::ask") || enc[starts[j]]-enc[starts[i]] > maxJSON {
				break
			}
			n := starts[j] - starts[i]
			if n >= need && (bestE == 0 || n < bestE-bestS) {
				bestS, bestE = starts[i], starts[j]
			}
			if n > longE-longS {
				longS, longE = starts[i], starts[j]
			}
		}
	}
	if bestE > 0 {
		return bestS, bestE
	}
	return longS, longE
}

// jsonCost is how many bytes encoding/json's Marshal writes, inside a string's quotes, for the
// character starting at s[i], and that character's length in s: an invalid byte is written as
// U+FFFD (3 bytes), U+2028 and U+2029 escaped (TestJSONPrefix holds it to Marshal).
func jsonCost(s string, i int) (cost, size int) {
	if b := s[i]; b < utf8.RuneSelf {
		switch {
		case b == '\\' || b == '"' || b == '\b' || b == '\f' || b == '\n' || b == '\r' || b == '\t':
			return 2, 1
		case b < 0x20 || b == '<' || b == '>' || b == '&':
			return 6, 1
		}
		return 1, 1
	}
	c, size := utf8.DecodeRuneInString(s[i:])
	switch {
	case c == utf8.RuneError && size == 1:
		return 3, 1
	case c == '\u2028' || c == '\u2029':
		return 6, size
	}
	return size, size
}

// jsonPrefix is, for each k in [0, len(text)] that starts a character (or ends text), the length of
// text[:k] JSON-encoded without its quotes.
func jsonPrefix(text string) []int {
	enc := make([]int, len(text)+1)
	for i := 0; i < len(text); {
		cost, size := jsonCost(text, i)
		for k := i + 1; k <= i+size; k++ {
			enc[k] = enc[i]
		}
		enc[i+size] += cost
		i += size
	}
	return enc
}

// baseText is the text with the run removed. A run reaching the end leaves the blank line before
// it, which canonical markdown drops.
func baseText(text string, runStart, runEnd int) string {
	if runEnd <= runStart {
		return text
	}
	base := text[:runStart] + text[runEnd:]
	if runEnd == len(text) {
		base = strings.TrimRight(base, "\n")
		if base != "" {
			base += "\n"
		}
	}
	return base
}

// planHistory plans a document's history from production's version sizes (sizes[n-1] for version
// n, -1 when unlisted), its text at At (whose size Dispatch keeps, so local size n is sizes[n-1] -
// sizes[final-1] + len(text)), and the version each typed ask block's answer or resolve lands at
// (versions[i] for steps[i]; one under 2 or past final takes the last free version).
func planHistory(text string, sizes []int, final int, steps []askStep, versions []int) (historyPlan, error) {
	p := historyPlan{Final: final, AskAt: map[int][]askStep{}}
	real := make([]int, final)
	for n := range final {
		switch {
		case n < len(sizes) && sizes[n] >= 0:
			real[n] = sizes[n]
		case n > 0:
			real[n] = real[n-1]
			fallthrough
		default:
			p.Missing++
		}
	}
	free := final
	for i, st := range steps {
		v := versions[i]
		if v < 2 || v > final {
			for free > 1 && len(p.AskAt[free]) > 0 {
				free--
			}
			v = free
		}
		p.AskAt[v] = append(p.AskAt[v], st)
	}
	p.Restore = final
	for p.Restore > 1 && len(p.AskAt[p.Restore]) > 0 {
		p.Restore--
	}
	for v := range p.AskAt {
		if v <= 1 {
			return p, fmt.Errorf("an answer or resolve cannot land at version %d", v)
		}
	}
	local := func(n int) int { return real[n-1] - real[final-1] + len(text) }
	p.Sizes = make([]int, final)
	for n := p.Restore; n <= final; n++ {
		p.Sizes[n-1] = local(n)
	}
	if p.Restore == 1 {
		if len(p.AskAt) > 0 {
			return p, fmt.Errorf("every version is an answer or resolve: no version is left to create the document")
		}
		return p, nil
	}
	// The run: long enough that the smallest padded version still holds padMin bytes of padding
	// over the base. The floor is taken over the unstripped base, the largest any version's base
	// gets as ask steps land.
	smallest := len(text)
	for n := 1; n < p.Restore; n++ {
		smallest = min(smallest, local(n))
	}
	p.RunStart, p.RunEnd = chooseRun(text, len(text)+padOverhead(len(text))+padMin-smallest, maxJSONBody-restoreEnvelope)
	base := baseText(text, p.RunStart, p.RunEnd)
	floor := len(base) + padOverhead(len(base)) + padMin
	// Walk back from Restore: each padded version is the next one less production's change; one
	// that would fall under the floor takes the change with its sign flipped, and one under it
	// still is raised to the floor (Unreachable).
	for n := p.Restore - 1; n >= 1; n-- {
		d := real[n] - real[n-1]
		size := p.Sizes[n] - d
		if len(p.AskAt[n+1]) == 0 && size < floor && d != 0 {
			size = p.Sizes[n] + d
			p.Flipped++
		}
		if size < floor {
			size = floor
			p.Unreachable++
		}
		p.Sizes[n-1] = size
	}
	return p, nil
}

var (
	askDirective = regexp.MustCompile(`^:::ask\{#([^ }]+)([^}]*)\}`)
	answerAttr   = regexp.MustCompile(` (answered_by|answered_at|selected|answer)="[^"]*"`)
	stateAttr    = regexp.MustCompile(` state="[^"]*"`)
)

// stripAnswers is text with the typed ask blocks named in ids written as Dispatch stores an open
// block: no answered_by, answered_at, selected or answer, and state "open" (an answered or a
// resolved block alike).
func stripAnswers(text string, ids map[string]bool) string {
	if len(ids) == 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		m := askDirective.FindStringSubmatch(line)
		if m == nil || !ids[m[1]] {
			continue
		}
		attrs := answerAttr.ReplaceAllString(m[2], "")
		attrs = stateAttr.ReplaceAllString(attrs, ` state="open"`)
		lines[i] = ":::ask{#" + m[1] + attrs + "}" + line[len(m[0]):]
	}
	return strings.Join(lines, "\n")
}

// padText is n bytes of synthetic lowercase words, starting with a token no document holds, and
// different for each salt.
func padText(n int, salt int) string {
	var b strings.Builder
	b.WriteString(padToken)
	for i := 0; b.Len() < n; i++ {
		b.WriteByte(' ')
		b.WriteString(words[(salt*7+i*13)%len(words)])
	}
	s := b.String()[:n]
	if strings.HasSuffix(s, " ") {
		s = s[:n-1] + "x"
	}
	return s
}

// The padding paragraph of n bytes is padAt(n): the first n bytes of the pad stream, the words
// padWord(0), padWord(1), ... joined by single spaces (a last byte that would be a space is an x).
// Each word is padToken and four lowercase letters, so a whole word occurs once in a document (no
// document holds padToken, docText). Growing the paragraph by d appends the stream's next d bytes
// and shrinking it by d drops its last d, each sent as a replace of only the paragraph's tail
// (padEdit), and a version whose size does not change swaps its last whole word for the word's
// digit form or back (padSwap): every padding edit's body holds the change and under 40 bytes of
// padding besides, however long the paragraph.
const padWordLen = len(padToken) + 4

// padStride is the distance between two words' starts in the pad stream.
const padStride = padWordLen + 1

// padWord is the pad stream's word i (i < 26^4, so a stream of up to 4.5 MB, past any document).
func padWord(i int) string {
	b := []byte(padToken + "aaaa")
	for k := len(b) - 1; k >= len(padToken); k-- {
		b[k] = 'a' + byte(i%26)
		i /= 26
	}
	return string(b)
}

// padAt is the padding paragraph of n bytes.
func padAt(n int) string {
	var b strings.Builder
	b.Grow(n + padStride)
	for i := 0; b.Len() < n; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(padWord(i))
	}
	s := b.String()[:n]
	if strings.HasSuffix(s, " ") {
		s = s[:n-1] + "x"
	}
	return s
}

// padSwap is pad with its last whole word swapped between the stream's word and its digit form
// (padToken and four digits, a word no stream holds), the same length and other bytes; a paragraph
// too short for a whole word swaps its last byte between y and z.
func padSwap(pad string) string {
	if len(pad) < padWordLen {
		last := byte('y')
		if pad[len(pad)-1] == 'y' {
			last = 'z'
		}
		return pad[:len(pad)-1] + string(last)
	}
	j := (len(pad) - padWordLen) / padStride
	at := j * padStride
	word := padWord(j)
	if pad[at:at+padWordLen] == word {
		word = fmt.Sprintf("%s%04d", padToken, j%10000)
	}
	return pad[:at] + word + pad[at+padWordLen:]
}

// padEdit is the replace that turns padding paragraph cur into next: find is cur from the start of
// the last whole word at or before the first byte they differ in (from cur's start when it holds no
// whole word), with is next from there. The find starts with a whole word, which occurs once in the
// document (padSwap's digit form too, which only the last word ever takes), or is the whole
// paragraph, the document's only text holding padToken: so it occurs once.
func padEdit(cur, next string) (find, with string) {
	k := 0
	for k < len(cur) && k < len(next) && cur[k] == next[k] {
		k++
	}
	at := 0
	if len(cur) >= padWordLen {
		at = min(k, len(cur)-padWordLen) / padStride * padStride
	}
	return cur[at:], next[at:]
}

// padNext is the padding paragraph of n bytes that follows cur: padAt(n), or cur swapped (padSwap)
// when that is cur itself, so a version whose size does not change still changes its bytes.
func padNext(cur string, n int) string {
	if next := padAt(n); next != cur {
		return next
	}
	return padSwap(cur)
}
