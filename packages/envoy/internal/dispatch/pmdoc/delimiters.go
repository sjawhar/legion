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
// Each run is linked to the runs before and after it three ways: among every run, among its group's
// (runGroup) and among its kind's (runKind). A closer looks back only at runs of its kind, the only
// ones it could pair with, down to the start of the scope it is paired in; a pair takes every run
// between its opener and closer off the list, so each run is taken off once and pairing a textblock
// takes time linear in its runs, apart from the openers the rule of three makes a closer pass.
type delimiterList struct {
	last        *listedRun
	lastInGroup [runGroups]*listedRun
	lastOfKind  [runKinds]*listedRun
	// pushed is the seq of the last run pushed, so a run pushed after a link label opens has a seq
	// above the one pushed when it opened.
	pushed int
	// bottoms is, for each link or image label open, innermost last, the pushed count when it opened,
	// as goldmark's link parser keeps the last delimiter on its list (pushLinkBottom).
	bottoms []int
	// first is the group whose run the textblock tokenized first, or runGroups before any.
	first runGroup
}

// listedRun is a run on delimiterList: the delimiter goldmark's inline parser placed among the
// textblock's nodes, the order it was pushed in (seq, from 1), and its neighbours on the list.
type listedRun struct {
	delimiter                *parser.Delimiter
	seq                      int
	group                    runGroup
	kind                     runKind
	prev, next               *listedRun
	prevInGroup, nextInGroup *listedRun
	prevOfKind, nextOfKind   *listedRun
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
	run := &listedRun{delimiter: d, group: emphasisRuns}
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
	run.prevOfKind = l.lastOfKind[run.kind]
	if run.prevOfKind != nil {
		run.prevOfKind.nextOfKind = run
	}
	l.lastOfKind[run.kind] = run
}

// pairTextblock pairs the runs a textblock leaves once its inline markdown is all read. Under
// micromark's rules the group whose run the textblock tokenized first is paired first, then the
// other.
func (l *delimiterList) pairTextblock(goldmarkRules bool) {
	switch {
	case goldmarkRules:
		l.pair(scopePass(emphasisRuns, 0))
	case l.first == tildeRuns:
		l.pair(scopePass(emphasisRuns, 0), scopePass(tildeRuns, 0))
	default:
		l.pair(scopePass(tildeRuns, 0), scopePass(emphasisRuns, 0))
	}
}

// pairLabel pairs the runs of a link or image label that formed: those pushed after bottom, now the
// link's children. Under micromark's rules the `~` runs go first, since micromark resolves a label
// with the extensions' resolvers before its own attention (resolveToLabelEnd, insideSpan).
func (l *delimiterList) pairLabel(bottom int, goldmarkRules bool) {
	if goldmarkRules {
		l.pair(scopePass(emphasisRuns, bottom))
		return
	}
	l.pair(scopePass(emphasisRuns, bottom), scopePass(tildeRuns, bottom))
}

// pairingPass is one pass pairing the runs of group whose seq lies above bottom and below top.
// closer is the next run it tries as a closer, once started.
type pairingPass struct {
	group       runGroup
	bottom, top int
	closer      *listedRun
	started     bool
}

// scopePass is the pass over every run of group pushed after bottom.
func scopePass(group runGroup, bottom int) pairingPass {
	return pairingPass{group: group, bottom: bottom, top: math.MaxInt}
}

// pair makes passes, the last first, and every pass a pair they make calls for. micromark resolves
// what a pair encloses as soon as it makes the pair, with every resolver over the runs between its
// opener and closer (resolveAll over insideSpan), so no run pairs out of a pair: a pass that pairs
// an opener and a closer enclosing runs of the other group pairs those at once, in a pass of their
// own, before it goes on. The passes are a stack rather than calls, so how deeply pairs call for
// passes is bounded by memory, not by the goroutine's stack.
func (l *delimiterList) pair(passes ...pairingPass) {
	stack := passes
	for len(stack) > 0 {
		pass := stack[len(stack)-1]
		if !pass.started {
			pass.closer, pass.started = l.firstAfter(pass.group, pass.bottom), true
		}
		if inner, found := l.advance(&pass); found {
			stack[len(stack)-1] = pass
			stack = append(stack, inner)
			continue
		}
		l.clear(pass)
		stack = stack[:len(stack)-1]
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
// processor accepts and the rule of three lets it use, the runs between them move into the mark,
// the runs of the group between them leave the list as text, and a run used up leaves it. A closer
// that pairs with nothing and cannot open leaves the list unless the rule of three alone stopped it.
// The walk back looks only at runs of the closer's kind: each processor refuses a run of another
// character (and micromark's `~` one of another length) before it changes anything, so that walk
// pairs as goldmark's walk over every run does. It stops at a pair that encloses runs of the other
// group and returns the pass over those, which pair makes before this one goes on.
func (l *delimiterList) advance(pass *pairingPass) (pairingPass, bool) {
	for pass.closer != nil && pass.closer.seq < pass.top {
		closer := pass.closer
		end := closer.delimiter
		if !end.CanClose {
			pass.closer = closer.nextInGroup
			continue
		}
		var opener *listedRun
		consume, maybeOpener := 0, false
		for candidate := closer.prevOfKind; candidate != nil && candidate.seq > pass.bottom; candidate = candidate.prevOfKind {
			start := candidate.delimiter
			if start.CanOpen && start.Processor.CanOpenCloser(start, end) {
				maybeOpener = true
				if consume = start.CalcComsumption(end); consume > 0 {
					opener = candidate
					break
				}
			}
		}
		if opener == nil {
			pass.closer = closer.nextInGroup
			if !maybeOpener && !end.CanOpen {
				l.remove(closer)
			}
			continue
		}
		start := opener.delimiter
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
		}
		if end.Length == 0 {
			pass.closer = closer.nextInGroup
			l.remove(closer)
		}
		if enclosed != closer {
			return pairingPass{group: enclosed.group, bottom: opener.seq, top: closer.seq, closer: enclosed, started: true}, true
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
	if run.prevOfKind != nil {
		run.prevOfKind.nextOfKind = run.nextOfKind
	}
	if run.nextOfKind != nil {
		run.nextOfKind.prevOfKind = run.prevOfKind
	} else {
		l.lastOfKind[run.kind] = run.prevOfKind
	}
	d := run.delimiter
	parent := d.Parent()
	if d.Length != 0 {
		ast.MergeOrReplaceTextSegment(parent, d, d.Segment)
	} else {
		parent.RemoveChild(parent, d)
	}
}
