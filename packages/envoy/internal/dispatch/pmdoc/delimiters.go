package pmdoc

import (
	"math"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
)

// delimitersKey holds, while a textblock's inline markdown is read, the delimiter runs no pair has
// used up yet (delimiterList).
var delimitersKey = parser.NewContextKey()

// delimiterList is a textblock's delimiter runs, which pmdoc pairs itself rather than through
// goldmark's ProcessDelimiters. The browser editor's parser (micromark) pairs `~` runs and `*`/`_`
// runs in separate passes, in an order of its own, where goldmark pairs every run from one stack in
// order; and goldmark walks a closer back over every earlier run of any character, so a paragraph
// of `~~a ` and then as many `a* ` took time quadratic in its length. emphasisParser and tildeParser
// push their runs here, never onto goldmark's list, so goldmark's ProcessDelimiters always meets an
// empty list and returns at once (parser/delimiter.go:158-161 at v1.8.6), and the runs are paired at
// a link or image's formation (imageNesting) and at the textblock's end (emphasisParser.CloseBlock).
//
// Each run is linked to the runs before and after it among every run and among its group's
// (runGroup). A pass files each run its closers have gone past that can still open among its
// kind's openers (runKind, openers), in a bucket by what CommonMark's rule of three reads of it.
// The rule rejects a bucket's runs all or none, so a closer finds the nearest opener it could pair
// with among the last runs of at most six buckets (nearestOpener), however many openers the rule
// keeps it from; a pair takes every run between its opener and closer off the list, so each run
// is taken off once and pairing a textblock takes time linear in its runs.
type delimiterList struct {
	last        *listedRun
	lastInGroup [runGroups]*listedRun
	// openers is, for each kind, the last run of each bucket of openers a pass has filed (enlist):
	// by the length the rule of three reads (ruleLength) modulo three, then by whether the run can
	// close, each bucket's runs linked in the order they were pushed. A pass files a run as its
	// closer goes past it, and a pair takes every run between its opener and closer off the list,
	// so a run filed is the latest of its group filed. A pass of a group inside another pass of
	// it pairs runs inside a pair that outer pass made, whose runs of the group are off the list,
	// so the runs filed above its bottom are its own, and it takes them all off (clear) before the
	// outer pass goes on. Outside pair every bucket is empty.
	openers [runKinds][3][2]*listedRun
	// pushed is the seq of the last run pushed, so a run pushed after a link label opens has a seq
	// above the one pushed when it opened.
	pushed int
	// bottoms is, for each link or image label open, innermost last, the pushed count when it opened,
	// as goldmark's link parser keeps the last delimiter on its list (pushLinkBottom). Only openLabel
	// and closeLabel change it.
	bottoms []int
	// first is the group whose run the textblock tokenized first, or runGroups before any.
	first runGroup
	// slab is the chunk push takes its next run from, so a textblock's runs cost an allocation per
	// chunk rather than one each. A full chunk is replaced, never grown, so no run moves from where
	// the list's links point to it.
	slab []listedRun
}

// listedRun is a run on delimiterList: the delimiter goldmark's inline parser placed among the
// textblock's nodes, the order it was pushed in (seq, from 1), its neighbours on the list, and
// those in the bucket of openers it is filed in (bucket, that bucket's last-run slot, nil while it
// is filed in none).
type listedRun struct {
	delimiter                *parser.Delimiter
	seq                      int
	group                    runGroup
	kind                     runKind
	prev, next               *listedRun
	prevInGroup, nextInGroup *listedRun
	prevOpener, nextOpener   *listedRun
	bucket                   **listedRun
}

// runGroup is which pass pairs a run. micromark pairs `*` and `_` runs (its attention resolver) and
// `~` runs (its strikethrough resolver) apart; under goldmark's rules every run is an emphasisRuns
// run, so one pass pairs them all in order, as ProcessDelimiters does.
type runGroup uint8

const (
	emphasisRuns runGroup = iota
	tildeRuns
	runGroups
)

// runKind is which runs a closer may pair with: a run of its own character, and under micromark's
// rules a `~` run only with one of its own length, so a single and a double `~` are two kinds there.
type runKind uint8

const (
	starRun runKind = iota
	underscoreRun
	tildeRun
	doubleTildeRun
	runKinds
)

// delimitersOf is the delimiter list of the textblock pc is reading, made on first use.
func delimitersOf(pc parser.Context) *delimiterList {
	list, _ := pc.Get(delimitersKey).(*delimiterList)
	if list == nil {
		list = &delimiterList{first: runGroups}
		pc.Set(delimitersKey, list)
	}
	return list
}

// tokenized records that the textblock tokenized a run of group. At a textblock's top level
// micromark runs each resolver in the order its construct first tokenized a run (resolveAll in
// micromark's create-tokenizer), so the group whose run came first is paired first.
func (l *delimiterList) tokenized(group runGroup) {
	if l.first == runGroups {
		l.first = group
	}
}

// push adds d, which the inline parser has placed among the textblock's nodes, as the last run,
// grouped and kinded by goldmark's rules or micromark's.
func (l *delimiterList) push(d *parser.Delimiter, goldmarkRules bool) {
	if len(l.slab) == cap(l.slab) {
		l.slab = make([]listedRun, 0, 64)
	}
	l.slab = append(l.slab, listedRun{delimiter: d, group: emphasisRuns})
	run := &l.slab[len(l.slab)-1]
	switch {
	case d.Char == '*':
		run.kind = starRun
	case d.Char == '_':
		run.kind = underscoreRun
	case goldmarkRules:
		run.kind = tildeRun
	case d.OriginalLength == 2:
		run.group, run.kind = tildeRuns, doubleTildeRun
	default:
		run.group, run.kind = tildeRuns, tildeRun
	}
	l.tokenized(run.group)
	l.pushed++
	run.seq = l.pushed
	run.prev = l.last
	if l.last != nil {
		l.last.next = run
	}
	l.last = run
	run.prevInGroup = l.lastInGroup[run.group]
	if run.prevInGroup != nil {
		run.prevInGroup.nextInGroup = run
	}
	l.lastInGroup[run.group] = run
}

// pairTextblock pairs the runs a textblock leaves once its inline markdown is all read. Under
// micromark's rules the group whose run the textblock tokenized first is paired first, then the
// other.
func (l *delimiterList) pairTextblock(goldmarkRules bool) {
	switch {
	case goldmarkRules:
		l.pair(0, emphasisRuns)
	case l.first == tildeRuns:
		l.pair(0, tildeRuns, emphasisRuns)
	default:
		l.pair(0, emphasisRuns, tildeRuns)
	}
}

// openLabel records that a link or image label opened once bottom runs had been pushed.
func (l *delimiterList) openLabel(bottom int) {
	l.bottoms = append(l.bottoms, bottom)
}

// closeLabel takes off the innermost label open and returns how many runs had been pushed when it
// opened, or reports that no label is open, as on a nil list.
func (l *delimiterList) closeLabel() (bottom int, open bool) {
	if l == nil || len(l.bottoms) == 0 {
		return 0, false
	}
	bottom = l.bottoms[len(l.bottoms)-1]
	l.bottoms = l.bottoms[:len(l.bottoms)-1]
	return bottom, true
}

// pairLabel pairs the runs of a link or image label that formed: those pushed after bottom, now the
// link's children. Under micromark's rules the `~` runs go first, since micromark resolves a label
// with the extensions' resolvers before its own attention (resolveToLabelEnd, insideSpan).
func (l *delimiterList) pairLabel(bottom int, goldmarkRules bool) {
	if goldmarkRules {
		l.pair(bottom, emphasisRuns)
		return
	}
	l.pair(bottom, tildeRuns, emphasisRuns)
}

// pairingPass is one pass pairing the runs of group whose seq lies above bottom and below top.
// closer is the next run it tries as a closer.
type pairingPass struct {
	group       runGroup
	bottom, top int
	closer      *listedRun
}

// pair pairs the runs of each of groups pushed after bottom, a pass per group in the order given,
// and makes every pass a pair calls for. micromark resolves what a pair encloses as soon as it
// makes the pair, with every resolver over the runs between its opener and closer (resolveAll over
// insideSpan), so no run pairs out of a pair: a pass that pairs an opener and a closer enclosing
// runs of the other group pairs those at once, in a pass of their own, before it goes on. A group's
// pass starts at its first run left once the groups before it are paired. The passes are a stack
// rather than calls, so how deeply pairs call for passes is bounded by memory, not by the
// goroutine's stack.
func (l *delimiterList) pair(bottom int, groups ...runGroup) {
	var stack []pairingPass
	for _, group := range groups {
		stack = append(stack, pairingPass{group: group, bottom: bottom, top: math.MaxInt, closer: l.firstAfter(group, bottom)})
		for len(stack) > 0 {
			pass := stack[len(stack)-1]
			if inner, found := l.advance(&pass); found {
				stack[len(stack)-1] = pass
				stack = append(stack, inner)
				continue
			}
			l.clear(pass)
			stack = stack[:len(stack)-1]
		}
	}
}

// firstAfter is the first run of group pushed after bottom, or nil.
func (l *delimiterList) firstAfter(group runGroup, bottom int) *listedRun {
	var first *listedRun
	for run := l.lastInGroup[group]; run != nil && run.seq > bottom; run = run.prevInGroup {
		first = run
	}
	return first
}

// advance pairs pass's closers in order as goldmark's ProcessDelimiters pairs a list's
// (parser/delimiter.go:179-237 at v1.8.6): a closer pairs with the nearest earlier opener its
// processor accepts and the rule of three lets it use (nearestOpener), the runs between them move
// into the mark, the runs of the group between them leave the list as text, and a run used up
// leaves it. A closer that pairs with nothing and cannot open leaves the list unless the rule of
// three alone stopped it. A run the pass goes past that can open is filed among the openers. It
// stops at a pair that encloses runs of the other group and returns the pass over those, which
// pair makes before this one goes on.
func (l *delimiterList) advance(pass *pairingPass) (pairingPass, bool) {
	for pass.closer != nil && pass.closer.seq < pass.top {
		closer := pass.closer
		end := closer.delimiter
		if !end.CanClose {
			pass.closer = closer.nextInGroup
			if end.CanOpen {
				l.enlist(closer)
			}
			continue
		}
		opener, maybeOpener := l.nearestOpener(closer, pass.bottom)
		if opener == nil {
			pass.closer = closer.nextInGroup
			switch {
			case end.CanOpen:
				l.enlist(closer)
			case !maybeOpener:
				l.remove(closer)
			}
			continue
		}
		start := opener.delimiter
		// The processor accepts the pair (nearestOpener); asking it still gives an emphasis run's
		// OriginalLength the length CalcComsumption reads, which then measures more than nothing.
		start.Processor.CanOpenCloser(start, end)
		consume := start.CalcComsumption(end)
		start.ConsumeCharacters(consume)
		end.ConsumeCharacters(consume)
		mark := start.Processor.OnMatch(consume)
		mark.(interface{ SetPos(int) }).SetPos(start.Segment.Start)
		parent := start.Parent()
		for child := start.NextSibling(); child != nil && child != end; {
			next := child.NextSibling()
			mark.AppendChild(mark, child)
			child = next
		}
		parent.InsertAfter(parent, start, mark)
		for run := opener.nextInGroup; run != closer; {
			next := run.nextInGroup
			l.remove(run)
			run = next
		}
		// The runs left between the two are the other group's alone.
		enclosed := opener.next
		if start.Length == 0 {
			l.remove(opener)
		} else {
			// What it has left may file it in another bucket, at whose end it belongs: it is the
			// latest run of its group filed, every one after it being off the list.
			l.delist(opener)
			l.enlist(opener)
		}
		if end.Length == 0 {
			pass.closer = closer.nextInGroup
			l.remove(closer)
		}
		if enclosed != closer {
			return pairingPass{group: enclosed.group, bottom: opener.seq, top: closer.seq, closer: enclosed}, true
		}
	}
	return pairingPass{}, false
}

// clear takes off the list, last first, each run of pass's group that pass left, as goldmark's
// ClearDelimiters ends ProcessDelimiters.
func (l *delimiterList) clear(pass pairingPass) {
	run := l.lastInGroup[pass.group]
	if pass.closer != nil {
		run = pass.closer.prevInGroup
	}
	for run != nil && run.seq > pass.bottom {
		prev := run.prevInGroup
		l.remove(run)
		run = prev
	}
}

// remove takes run off the list and leaves what its delimiter has left as text, as goldmark's
// parseContext.RemoveDelimiter does: joined to the text before it where that text ends where the run
// starts, and dropped where the run is used up.
func (l *delimiterList) remove(run *listedRun) {
	if run.prev != nil {
		run.prev.next = run.next
	}
	if run.next != nil {
		run.next.prev = run.prev
	} else {
		l.last = run.prev
	}
	if run.prevInGroup != nil {
		run.prevInGroup.nextInGroup = run.nextInGroup
	}
	if run.nextInGroup != nil {
		run.nextInGroup.prevInGroup = run.prevInGroup
	} else {
		l.lastInGroup[run.group] = run.prevInGroup
	}
	l.delist(run)
	d := run.delimiter
	parent := d.Parent()
	if d.Length != 0 {
		ast.MergeOrReplaceTextSegment(parent, d, d.Segment)
	} else {
		parent.RemoveChild(parent, d)
	}
}

// nearestOpener is the nearest opener pushed after bottom that closer could pair with, and whether
// an opener of closer's kind is left there at all: what goldmark's walk back over every earlier run
// finds, the first that can open, that the processor accepts and that CalcComsumption measures at
// more than nothing. The processor refuses a run of another kind before it changes anything:
// another character, and under micromark's rules a `~` run of another length. It accepts every run
// of closer's kind, since a pair uses up micromark's `~` runs whole, so a run of a kind keeps the
// length its kind is. CalcComsumption measures nothing exactly where the rule of three rejects the
// pair, and the rule reads of the opener only its length modulo three and whether it can close,
// what its bucket is, so it rejects a bucket's runs all or none: the nearest opener is the latest
// of the last runs of the buckets it does not reject. The openers filed are every run of the
// pass's group pushed before closer, after bottom and still on the list, that can open.
func (l *delimiterList) nearestOpener(closer *listedRun, bottom int) (opener *listedRun, maybeOpener bool) {
	end := closer.delimiter
	length := ruleLength(closer) % 3
	for openerLength, buckets := range &l.openers[closer.kind] {
		for canClose, last := range buckets {
			if last == nil || last.seq <= bottom {
				continue
			}
			maybeOpener = true
			if (canClose == 1 || end.CanOpen) && (openerLength+length)%3 == 0 && length != 0 {
				continue
			}
			if opener == nil || last.seq > opener.seq {
				opener = last
			}
		}
	}
	return opener, maybeOpener
}

// ruleLength is the length of run the rule of three reads, where Delimiter.CalcComsumption reads
// OriginalLength: what a `*` or `_` run has left, which emphasisDelimiters.CanOpenCloser makes its
// OriginalLength before every measure, and what a `~` run was written with, which tildeDelimiters
// leaves.
func ruleLength(run *listedRun) int {
	if run.kind == starRun || run.kind == underscoreRun {
		return run.delimiter.Length
	}
	return run.delimiter.OriginalLength
}

// enlist files run, which can open, as the last of its bucket of openers.
func (l *delimiterList) enlist(run *listedRun) {
	canClose := 0
	if run.delimiter.CanClose {
		canClose = 1
	}
	run.bucket = &l.openers[run.kind][ruleLength(run)%3][canClose]
	run.prevOpener = *run.bucket
	if run.prevOpener != nil {
		run.prevOpener.nextOpener = run
	}
	*run.bucket = run
}

// delist takes run out of the bucket of openers it is filed in, if any.
func (l *delimiterList) delist(run *listedRun) {
	if run.bucket == nil {
		return
	}
	if run.prevOpener != nil {
		run.prevOpener.nextOpener = run.nextOpener
	}
	if run.nextOpener != nil {
		run.nextOpener.prevOpener = run.prevOpener
	} else {
		*run.bucket = run.prevOpener
	}
	run.prevOpener, run.nextOpener, run.bucket = nil, nil, nil
}
