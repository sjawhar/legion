package pmdoc

import (
	"bytes"
	"sort"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// linkify is goldmark's Linkify extension with its inline parser behind linkifyGuard, at the
// priority extension.Linkify gives it.
type linkify struct{}

func (linkify) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithInlineParsers(util.Prioritized(newLinkifyGuard(), 999)))
}

// linkifyGuard runs goldmark's linkify parser only where it can match. goldmark's parser, triggered
// at every space, `*`, `_`, `~` and `(`, scans forward over email local-part characters to the
// next `@` before it knows there is no email there: on a line of `a_a_a_…` every `_` scans to the
// line's end, and the line costs time quadratic in its length (LEGION-465). The guard reads each
// line once, recording for each `@` where its run of local-part characters begins, and answers a
// trigger with no `@` reachable through local-part characters, or one whose `@` the parser already
// refused, without calling it. What the parser links is unchanged: it is called exactly where it
// could link, and whether it links an email ending at an `@` does not depend on which trigger
// before that `@` asked. A line whose segment carries padding (the columns of a tab its container
// took part of) goes to the parser as it is, since its bytes are not the source's from the
// segment's start; that is one trigger, at the line's head.
type linkifyGuard struct {
	inner parser.InlineParser
}

func newLinkifyGuard() *linkifyGuard {
	return &linkifyGuard{inner: extension.NewLinkifyParser()}
}

var linkifyLineKey = parser.NewContextKey()

// linkifyLine is what the guard knows about the line ending at stop in the source, from the first
// offset a trigger asked about (from) to its end.
type linkifyLine struct {
	stop   int
	from   int
	ats    []int  // offsets of each `@`, ascending
	starts []int  // for each `@`, the lowest offset ≥ from from which only local-part characters lead to it
	failed []bool // for each `@`, whether the parser refused an email ending at it
}

var (
	linkifyProtocols = [][]byte{[]byte("http:"), []byte("https:"), []byte("ftp:"), []byte("www.")}
	// emailLocal marks the bytes goldmark's util.FindEmailIndex reads as an email's local part
	// (its emailTable); linkify_test.go holds the two to the same set.
	emailLocal = func() (table [256]bool) {
		for _, c := range []byte("!#$%&'*+-./0123456789=?ABCDEFGHIJKLMNOPQRSTUVWXYZ^_`abcdefghijklmnopqrstuvwxyz{|}~") {
			table[c] = true
		}
		return table
	}()
)

func (g *linkifyGuard) Trigger() []byte { return g.inner.Trigger() }

func (g *linkifyGuard) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	if pc.IsInLinkLabel() {
		return nil
	}
	line, segment := block.PeekLine()
	if segment.Padding != 0 {
		return g.inner.Parse(parent, block, pc)
	}
	start := segment.Start
	rest := line
	// The parser skips the trigger where it is not a line head, as its Parse does.
	if c := line[0]; c == ' ' || c == '*' || c == '_' || c == '~' || c == '(' {
		start++
		rest = rest[1:]
	}
	for _, protocol := range linkifyProtocols {
		if bytes.HasPrefix(rest, protocol) {
			return g.inner.Parse(parent, block, pc)
		}
	}
	if len(rest) == 0 || util.IsPunct(rest[0]) {
		return nil
	}
	state := g.line(block.Source(), start, segment.Stop, pc)
	index := sort.SearchInts(state.ats, start)
	if index == len(state.ats) || state.starts[index] > start || state.failed[index] {
		return nil
	}
	node := g.inner.Parse(parent, block, pc)
	if node == nil {
		state.failed[index] = true
	}
	return node
}

// line is the guard's record of the line ending at stop, read from `from` on first sight. Triggers
// on one line ask in offset order, so a record read from an earlier offset answers every later one.
func (g *linkifyGuard) line(source []byte, from, stop int, pc parser.Context) *linkifyLine {
	if state, ok := pc.Get(linkifyLineKey).(*linkifyLine); ok && state.stop == stop && state.from <= from {
		return state
	}
	state := &linkifyLine{stop: stop, from: from}
	runStart := from
	for offset := from; offset < stop && offset < len(source); offset++ {
		c := source[offset]
		if c == '@' {
			state.ats = append(state.ats, offset)
			state.starts = append(state.starts, runStart)
			state.failed = append(state.failed, false)
			runStart = offset + 1
			continue
		}
		if !emailLocal[c] {
			runStart = offset + 1
		}
	}
	pc.Set(linkifyLineKey, state)
	return state
}

func (g *linkifyGuard) CloseBlock(parent ast.Node, pc parser.Context) {}
