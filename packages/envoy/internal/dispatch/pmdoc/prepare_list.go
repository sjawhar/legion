package pmdoc

import (
	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
)

// This file ports the two upstream functions that decide a list's and a list item's spread in the
// browser editor's parser, run over this package's own line positions and goldmark tree. The
// browser editor runs the versions the repository's root package.json pins,
// micromark-core-commonmark@2.0.3 and mdast-util-from-markdown@2.0.3, each with this
// repository's own patch applied (patches/micromark-core-commonmark@2.0.3.patch,
// patches/mdast-util-from-markdown@2.0.3.patch), so this port is of the patched source:
//
//   - micromark-core-commonmark/dev/lib/list.js - tokenizeListStart, tokenizeListContinuation,
//     tokenizeIndent and tokenizeListItemPrefixWhitespace, with micromark's block-quote.js,
//     micromark-extension-gfm-footnote's definition continuation and this repository's own
//     packages/proof-editor/src/directive-container.ts, decide which lines each container holds
//     and how much of each line it strips (lineMachine below). The patch adds a fast path to
//     tokenizeListContinuation for a line already over at its call (a line ending or the end of
//     the document), which sets furtherBlankLines exactly as onBlank does and takes no space,
//     so the lines it holds and strips are the stock function's;
//   - mdast-util-from-markdown/dev/lib/index.js - prepareList, with the container-exit move
//     micromark-util-subtokenize does before it, decide the spread itself (prepareList and
//     moveContainerExits below). The patch collects that function's listItem events and inserts
//     them once at the end of the walk instead of splicing each one in place, which changes
//     where they are written and not which events are written or what _spread each list gets;
//     this port writes the events in their final order, as the patched compiler leaves them.
//
// The machine writes the event stream those two read, holding only the token kinds they tell
// apart, and then runs them over it. Columns are counted the way micromark's preprocessor and
// factorySpace count them: a tab spans as many columns as it takes to reach the next multiple of
// four, and a container strips part of one where its own columns end inside it.

// spreadToken is the kinds of token the walks over the event stream tell apart. micromark writes
// many more; each one that neither walk names, that clears no list item's atMarker and ends no
// walk, is left out of the stream.
type spreadToken uint8

const (
	// tokenContent is any token neither walk names: it ends both walks and clears atMarker.
	tokenContent spreadToken = iota
	// tokenLineEnding is the line ending of a line the flow tokenizer did not read as blank.
	tokenLineEnding
	// tokenLineEndingBlank is the line ending of a blank line (initialize/flow.js, atBlankEnding).
	tokenLineEndingBlank
	// tokenQuotePrefix is blockQuotePrefix, blockQuoteMarker and blockQuotePrefixWhitespace:
	// prepareList's tail walk steps over them, subtokenize's walk stops at them.
	tokenQuotePrefix
	// tokenItemPrefix is listItemPrefix and the marker, value and whitespace tokens inside it:
	// prepareList's switch leaves atMarker alone for them, and both walks stop at them.
	tokenItemPrefix
	// tokenList is listUnordered and listOrdered, which move prepareList's containerBalance.
	tokenList
	// tokenQuote is blockQuote, which moves it too.
	tokenQuote
	// tokenContainer is a container that is neither - a typed block or a footnote definition -
	// which moves no balance but is a container to subtokenize all the same.
	tokenContainer
)

// container reports whether a token of this kind carries micromark's `_container` flag, which is
// what subtokenize moves back over the line endings before it.
func (t spreadToken) container() bool {
	return t == tokenList || t == tokenQuote || t == tokenContainer
}

// spreadEvent is one event of the stream the two ported functions read. Only a container writes
// both an enter and an exit; every other token is one event, since neither walk reads its exit as
// anything but one more event to step over.
type spreadEvent struct {
	token spreadToken
	enter bool
	// node is the list a tokenList event opens or closes, and the list item a tokenItemPrefix
	// event opens.
	node ast.Node
	// settled marks a container exit resolveContentExits settled where it stands
	// (directive-container.ts, placeExits), which subtokenize does not move back.
	settled bool
}

// spreads is the spread of every list and list item in a document, as the browser editor's parser
// reads it.
type spreads map[ast.Node]bool

// readSpreads reads every list's and every list item's spread from a document's lines.
func readSpreads(root ast.Node, lines sourceLines) spreads {
	events := newLineMachine(root, lines).run()
	moveContainerExits(events)
	return prepareLists(events)
}

// containerKind is the four containers the browser editor's parser keeps open across lines by
// their prefix. A list item is no container of its own there: micromark's container is the list,
// whose state names the item it stands in (list.js, containerState).
type containerKind uint8

const (
	kindQuote containerKind = iota
	kindList
	kindDirective
	kindDefinition
)

// openContainer is one container the line machine holds open, with the state its continuation
// reads and writes.
type openContainer struct {
	kind containerKind
	node ast.Node
	// size is how many columns a list's continuation strips from every line it continues
	// (list.js, containerState.size).
	size int
	// initialBlank and further are list.js's containerState.initialBlankLine and
	// furtherBlankLines: an item whose marker line held nothing ends at the second blank line.
	initialBlank bool
	further      bool
	// indent is how many columns a typed block strips (directive-container.ts, unclosed).
	indent int
}

// opening is a container the goldmark tree opens on a line: a quote, a typed block, a footnote
// definition, a list with its first item, or a later item of a list already open.
type opening struct {
	kind containerKind
	node ast.Node
	// item is the list item a kindList opening opens, and newList whether its list opens with it.
	item    *ast.ListItem
	newList bool
	// depth is how many containers stand around the one that opens.
	depth int
}

// lineMachine reads a document's lines the way micromark's document tokenizer reads them: per
// line, which containers hold it once their prefixes are stripped, and whether what is left of it
// is blank.
type lineMachine struct {
	lines  sourceLines
	source []byte
	// openings is, by line, the containers the tree opens there, outermost first.
	openings map[int][]opening
	// lazy is, by line, whether a leaf block the line continues started before it: micromark
	// leaves the containers such a line does not reach open (document.js, writeToChild).
	lazy map[int]bool
	// flowHeld is, by line, whether a flow construct holds the line, so that its line ending is
	// no blank line's however blank the line looks: a code or HTML block's lines, and the lines
	// indented code keeps past where goldmark ends it (codeHeldAttr).
	flowHeld map[int]bool
	// fenceClosing is, by line, the typed block whose closing fence stands there.
	fenceClosing map[int]*typedDirective
	// openFence is, by the line after a fenced code block no fence closed, how many containers
	// stood around it: it takes every line after it for as long as they go on.
	openFence map[int]int
	// endsInLineEnding is whether the source ends with a line ending, so that micromark reads one
	// more line past its last.
	endsInLineEnding bool
	// lineFirst and lineLast are, by line, where its first event and its line ending stand in the
	// stream, which the exits of a typed block's bare last line are put between.
	lineFirst []int
	lineLast  []int
	// bare is, by line, whether the line held nothing but its line ending once the containers
	// out to the innermost typed block had stripped it, so that the block read it as no line of
	// its own (directive-container.ts, exitsOfAnUnclosedDirectiveContainer, `bare`).
	bare []bool
	// typedLeft is, by line, whether a container read the line's whitespace from where that typed
	// block's content starts, so that contentEnd stops at its prefix rather than walking back
	// past the line (contentEnd).
	typedLeft []bool
	// lastStrip is, by line, the position the last prefix token of the line starts at, or -1.
	lastStrip []int
	// fenced is, by line, whether a typed block's opening or closing fence stands on it, whose
	// line ending resolveFenceLineEndings types a lineEnding however blank the rest of the line
	// is (directive-container.ts, resolveFenceLineEndings).
	fenced []bool

	stack  []*openContainer
	events []spreadEvent
}

// newLineMachine reads from the tree what the machine needs of it: where each container opens,
// which lines a leaf block continues across, and which lines a flow construct holds.
func newLineMachine(root ast.Node, lines sourceLines) *lineMachine {
	machine := &lineMachine{
		lines:        lines,
		source:       lines.source,
		openings:     map[int][]opening{},
		lazy:         map[int]bool{},
		flowHeld:     map[int]bool{},
		fenceClosing: map[int]*typedDirective{},
		openFence:    map[int]int{},
	}
	machine.endsInLineEnding = len(lines.source) > 0 && lines.source[len(lines.source)-1] == '\n'
	machine.lineFirst = make([]int, len(lines.starts))
	machine.lineLast = make([]int, len(lines.starts))
	machine.bare = make([]bool, len(lines.starts))
	machine.typedLeft = make([]bool, len(lines.starts))
	machine.fenced = make([]bool, len(lines.starts))
	machine.lastStrip = make([]int, len(lines.starts))
	for line := range machine.lastStrip {
		machine.lastStrip[line] = -1
	}

	for line := range lines.held {
		machine.flowHeld[line] = true
	}
	depth := 0
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		switch node := node.(type) {
		case *ast.Blockquote:
			if entering {
				machine.openAt(lines.lineOf(startOf(node)), opening{kind: kindQuote, node: node, depth: depth})
				depth++
			} else {
				depth--
			}
		case *typedDirective:
			if entering {
				machine.openAt(lines.lineOf(startOf(node)), opening{kind: kindDirective, node: node, depth: depth})
				if node.Closed {
					machine.fenceClosing[lines.lineOf(node.closer)] = node
				}
				depth++
			} else {
				depth--
			}
		case *extensionast.Footnote:
			if entering {
				machine.openAt(lines.lineOf(node.Pos()), opening{kind: kindDefinition, node: node, depth: depth})
				depth++
			} else {
				depth--
			}
		case *ast.List:
			if entering {
				for child := node.FirstChild(); child != nil; child = child.NextSibling() {
					item, ok := child.(*ast.ListItem)
					if !ok {
						continue
					}
					machine.openAt(lines.lineOf(startOf(item)), opening{
						kind:    kindList,
						node:    node,
						item:    item,
						newList: child == node.FirstChild(),
						depth:   depth,
					})
				}
				depth++
			} else {
				depth--
			}
		case *ast.Paragraph, *ast.TextBlock, *ast.Heading, *extensionast.Table:
			if entering {
				machine.markLazy(node)
			}
			return ast.WalkSkipChildren, nil
		case *ast.FencedCodeBlock:
			if entering {
				machine.markFlow(node)
				if _, closed := node.Attribute(fenceClosedAttr); !closed {
					machine.openFence[machine.lastLine(node)+1] = depth
				}
			}
			return ast.WalkSkipChildren, nil
		case *ast.CodeBlock, *ast.HTMLBlock:
			if entering {
				machine.markFlow(node)
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return machine
}

// openAt records that the tree opens a container on line.
func (m *lineMachine) openAt(line int, entry opening) {
	m.openings[line] = append(m.openings[line], entry)
}

// lastLine is the last line of the source node's own lines reach.
func (m *lineMachine) lastLine(node ast.Node) int {
	segments := node.Lines()
	if segments.Len() == 0 {
		return m.lines.lineOf(startOf(node))
	}
	return m.lines.lineOf(segments.At(segments.Len() - 1).Start)
}

// markLazy records the lines a leaf block continues across: micromark reads such a line past the
// containers it does not reach rather than ending them (document.js, writeToChild). Only the
// content construct and a table take a line that way; a code or HTML block ends before one
// (micromark-core-commonmark, tokenizeNonLazyContinuation).
func (m *lineMachine) markLazy(node ast.Node) {
	segments := node.Lines()
	if segments.Len() == 0 {
		return
	}
	first := m.lines.lineOf(segments.At(0).Start)
	for line := first + 1; line <= m.lastLine(node); line++ {
		m.lazy[line] = true
	}
}

// markFlow records the lines a code or HTML block holds, whose line endings are no blank line's
// however blank the line is: the flow tokenizer stands inside the construct rather than at the
// start of a line, which is where it reads a blank line (initialize/flow.js).
func (m *lineMachine) markFlow(node ast.Node) {
	segments := node.Lines()
	for index := range segments.Len() {
		m.flowHeld[m.lines.lineOf(segments.At(index).Start)] = true
	}
}

// run reads the document line by line and writes the event stream.
func (m *lineMachine) run() []spreadEvent {
	// fenceDepth is how many containers stood around a fenced code block no fence closed that is
	// still taking lines, or -1 where none is.
	fenceDepth := -1
	for line := range m.lines.starts {
		if depth, ok := m.openFence[line]; ok {
			fenceDepth = depth
		}
		fenceDepth = m.readLine(line, fenceDepth)
	}
	// At the source's end micromark runs every continuation once more over the end of the
	// document, where a quote fails and a list, a typed block and a footnote definition go on,
	// and then exits every container (document.js, start and flowStart). Where the source ends
	// with no line ending the flow takes that end instead and no continuation runs there.
	continued, lazyEnd := len(m.stack), false
	if m.endsInLineEnding {
		continued = 0
		for _, entry := range m.stack {
			if entry.kind == kindQuote {
				break
			}
			continued++
		}
		lazyEnd = continued != len(m.stack)
	}
	last := len(m.lines.starts) - 1
	if m.exitsATypedBlock(0) && lazyEnd && m.typedIsLazy(0, continued) && last >= 0 && m.bare[last] {
		m.exitBare(0, last)
	} else {
		m.exit(0, lazyEnd, continued)
	}
	return m.events
}

// readLine writes one line's events and answers whether a fenced code block no fence closed still
// takes the lines after it, as how many containers stood around it or -1.
func (m *lineMachine) readLine(line, fenceDepth int) int {
	cursor := m.cursorAt(line)
	m.lineFirst[line] = len(m.events)
	opens := m.openings[line]
	var newItem ast.Node
	if len(opens) > 0 && opens[0].kind == kindList && !opens[0].newList {
		newItem = opens[0].node
	}
	var prefix []spreadEvent
	continued, closeFlow, closedFence, typed := 0, false, false, false
	typedOffset := 0
	// How much of the prefix the containers around the innermost typed block read: a container
	// inside the block ends where the block's content starts on the line, which is past those and
	// before its own (directive-container.ts, exitsOfAContainerEndedByALine).
	around := 0
	for _, entry := range m.stack {
		state, events := m.continueContainer(entry, &cursor, line, newItem)
		if state == contFail {
			break
		}
		prefix = append(prefix, events...)
		continued++
		if entry.kind == kindDirective {
			// Where its content starts, which the line is bare only where the line's own
			// content starts at as well (directive-container.ts, `bare`).
			typed, typedOffset = true, cursor.offset()
			around = len(prefix)
		}
		if state == contCloseFlow {
			closeFlow = true
			closedFence = entry.kind == kindDirective
			break
		}
	}
	m.bare[line] = typed && cursor.i >= cursor.end && typedOffset == cursor.offset()
	m.typedLeft[line] = typed && m.lastStrip[line] >= typedOffset
	keep := continued
	if len(opens) > 0 && !closeFlow {
		keep = min(opens[0].depth, continued)
	}
	exits := len(m.stack) - keep
	// A typed block opening on a line some container did not go on, before any other container
	// opens there, marks that line lazy, as it was for micromark-extension-directive's flow
	// construct (directive-container.ts, someContainerEnded): the exits of the containers around
	// it go right after the line before, where a lazy line's go (exitsOfALazyLine).
	lazyDirective := len(opens) > 0 && opens[0].kind == kindDirective && continued < len(m.stack)
	// A line micromark marked lazy for the flow construct: one some container did not go on
	// where no new container opened (document.js, thereIsNoNewContainer), and one a typed block
	// opened on (someContainerEnded).
	lazyLine := lazyDirective || len(opens) == 0 && !closeFlow
	switch {
	case exits > 0 && len(opens) == 0 && !closeFlow && m.lazy[line]:
		// The containers the line does not reach stay open: a leaf block inside them takes it.
		m.events = append(m.events, prefix...)
	case exits > 0 && m.exitsATypedBlock(keep) && !closedFence &&
		lazyLine && m.typedIsLazy(keep, continued) && line > 0 && m.bare[line-1]:
		// The block's last line held nothing but its line ending once the containers out to it
		// had stripped it, so it was no line of the block at all: the containers inside the
		// block end before that line, and the block itself before its line ending
		// (directive-container.ts, exitsOfAnUnclosedDirectiveContainer, `bare`).
		m.exitBare(keep, line-1)
		m.events = append(m.events, prefix...)
	case exits > 0 && m.exitsATypedBlock(keep) && !closedFence:
		// A typed block that no fence closed ends right after its last content, with the
		// containers inside it, before the prefixes of the containers around it on this line
		// (directive-container.ts, exitsOfAnUnclosedDirectiveContainer).
		m.exit(keep, lazyLine, continued)
		m.events = append(m.events, prefix...)
	case exits > 0 && m.keepsAnOpenDirective(keep) && !closedFence &&
		(closeFlow || len(opens) == 0 || lazyDirective):
		// A container inside a typed block that goes on ends where the block's content starts on
		// the line, past the prefixes of the containers around the block and before those of the
		// containers inside it, rather than right after the line before
		// (directive-container.ts, exitsOfAContainerEndedByALine).
		m.events = append(m.events, prefix[:around]...)
		m.exit(keep, lazyLine, continued)
		m.events = append(m.events, prefix[around:]...)
	case exits > 0 && len(opens) > 0 && !closeFlow && !lazyDirective:
		// A new container opens where one did not go on, so micromark exits past the prefixes it
		// has read (document.js, thereIsANewContainer).
		m.events = append(m.events, prefix...)
		m.exit(keep, lazyLine, continued)
	case exits > 0:
		// Closing the flow, and a line nothing holds open, put the exits before the line's own
		// prefixes (document.js, documentContinue and writeToChild).
		m.exit(keep, lazyLine, continued)
		m.events = append(m.events, prefix...)
	default:
		m.events = append(m.events, prefix...)
	}
	if closedFence {
		// A closed typed block's own exit goes right after its fence, where its content document
		// ended (directive-container.ts, exitsOfAClosedDirectiveContainer).
		m.exit(len(m.stack)-1, false, continued)
	}
	for _, entry := range opens {
		m.openContainer(entry, &cursor, line)
	}
	// A typed block's fence is read as document syntax, so what is left of its line for the flow
	// tokenizer is its line ending alone, which flow reads as a blank line; resolveFenceLineEndings
	// types it a lineEnding again (directive-container.ts). A footnote definition's label is read
	// there too, and the line is blank where nothing follows it.
	blank := !m.fenced[line] && !m.flowHeld[line] && m.blankFrom(cursor)
	switch {
	case blank && fenceDepth >= 0 && continued >= fenceDepth:
		// A fenced code block no fence closed takes every line after it while the containers
		// around it go on, so the line is its text and no blank line (keepsBlankLinesAfter).
		blank = false
	case fenceDepth >= 0:
		fenceDepth = -1
	}
	if !blank {
		m.events = append(m.events, spreadEvent{token: tokenContent, enter: true})
	}
	ending := tokenLineEnding
	if blank {
		ending = tokenLineEndingBlank
	}
	m.lineLast[line] = len(m.events)
	// The last line of a source that ends with no line ending ends none: the flow tokenizer
	// consumes the end of the document and writes no token for it (initialize/flow.js,
	// atBlankEnding and afterConstruct).
	if m.endsInLineEnding || line+1 < len(m.lines.starts) {
		m.events = append(m.events, spreadEvent{token: ending, enter: true})
	}
	return fenceDepth
}

// typedIsLazy reports whether the typed block the line ends ran no continuation on it, which is
// what resolveContentExits reads as a lazy end of its content document.
func (m *lineMachine) typedIsLazy(keep, continued int) bool {
	for index := len(m.stack) - 1; index >= keep; index-- {
		if m.stack[index].kind == kindDirective {
			return index >= continued
		}
	}
	return false
}

// exitBare writes the exits of the containers past keep where a typed block's last line, line,
// held nothing but its line ending once the containers out to it had stripped it: the block read
// that line as no line of its own, so its own exit goes before the line's ending, settled, and
// the containers inside it end where the block's content would have started on the line - before
// the line's own prefixes where nothing was left for them to strip there, and beside the block's
// exit where they stripped what the block left. The containers around the block stay where
// micromark put them (directive-container.ts, exitsOfAnUnclosedDirectiveContainer with `bare`,
// and contentEnd).
func (m *lineMachine) exitBare(keep, line int) {
	block := keep
	for index := keep; index < len(m.stack); index++ {
		if m.stack[index].kind == kindDirective {
			block = index
			break
		}
	}
	var inner, own, outer []spreadEvent
	for index := len(m.stack) - 1; index >= keep; index-- {
		entry := m.stack[index]
		event := spreadEvent{token: tokenContainer, node: entry.node}
		switch entry.kind {
		case kindDirective:
			event.settled = true
			own = append(own, event)
			continue
		case kindQuote:
			event.token = tokenQuote
		case kindList:
			event.token = tokenList
		}
		if index > block {
			inner = append(inner, event)
		} else {
			outer = append(outer, event)
		}
	}
	first, last := m.lineFirst[line], m.lineLast[line]
	m.insertEvents(last, own)
	if m.typedLeft[line] {
		m.insertEvents(last, inner)
	} else {
		m.insertEvents(first, inner)
	}
	m.events = append(m.events, outer...)
	m.stack = m.stack[:keep]
}

// insertEvents puts added into the stream before the event at index, and moves the line marks
// that stand at or after it along with the events they name.
func (m *lineMachine) insertEvents(index int, added []spreadEvent) {
	if len(added) == 0 {
		return
	}
	m.events = append(m.events, added...)
	copy(m.events[index+len(added):], m.events[index:len(m.events)-len(added)])
	copy(m.events[index:], added)
	for line := range m.lineFirst {
		if m.lineFirst[line] >= index {
			m.lineFirst[line] += len(added)
		}
		if m.lineLast[line] >= index {
			m.lineLast[line] += len(added)
		}
	}
}

// exitsATypedBlock reports whether a typed block stands among the containers past keep, which
// are the ones a line ends.
func (m *lineMachine) exitsATypedBlock(keep int) bool {
	for _, entry := range m.stack[keep:] {
		if entry.kind == kindDirective {
			return true
		}
	}
	return false
}

// keepsAnOpenDirective reports whether a typed block stands among the first keep containers of
// the stack, so that every container past them is inside one that goes on.
func (m *lineMachine) keepsAnOpenDirective(keep int) bool {
	for _, entry := range m.stack[:keep] {
		if entry.kind == kindDirective {
			return true
		}
	}
	return false
}

// contState is what a container's continuation answered for a line.
type contState uint8

const (
	contOK contState = iota
	// contCloseFlow is a continuation that goes on but closes the containers inside it: a typed
	// block's closing fence (directive-container.ts, closed), and a list going on with a new
	// item (list.js, notInCurrentItem).
	contCloseFlow
	contFail
)

// continueContainer runs one container's continuation over the line, as micromark's document
// tokenizer runs it, and answers the prefix tokens it read.
func (m *lineMachine) continueContainer(entry *openContainer, cursor *lineCursor, line int, newItem ast.Node) (contState, []spreadEvent) {
	switch entry.kind {
	case kindQuote:
		// block-quote.js, tokenizeBlockQuoteContinuation: at most three columns of indentation,
		// the marker, and one column of the space or tab after it.
		probe := *cursor
		for count := 0; count < 3 && probe.space(); count++ {
			probe.take()
		}
		if probe.at() != '>' {
			return contFail, nil
		}
		probe.step()
		if probe.space() {
			probe.take()
		}
		*cursor = probe
		return contOK, []spreadEvent{{token: tokenQuotePrefix, enter: true}}
	case kindList:
		if cursor.space() {
			m.lastStrip[line] = cursor.offset()
		}
		state := m.continueList(entry, cursor)
		if state == contFail && entry.node == newItem {
			// notInCurrentItem goes on with a new item of the same list, closing the flow and the
			// containers inside the item that ends.
			return contCloseFlow, nil
		}
		return state, nil
	case kindDirective:
		if entry.indent > 0 && cursor.space() {
			m.lastStrip[line] = cursor.offset()
		}
		if m.fenceClosing[line] == entry.node {
			m.fenced[line] = true
			cursor.toEnd()
			return contCloseFlow, []spreadEvent{{token: tokenContent, enter: true}}
		}
		for count := 0; count < entry.indent && cursor.space(); count++ {
			cursor.take()
		}
		return contOK, nil
	default:
		// micromark-extension-gfm-footnote, tokenizeDefinitionContinuation: a blank line, or
		// exactly four columns of indentation.
		if m.blankFrom(*cursor) {
			return contOK, nil
		}
		if cursor.space() {
			m.lastStrip[line] = cursor.offset()
		}
		probe := *cursor
		count := 0
		for count < tabWidth && probe.space() {
			probe.take()
			count++
		}
		if count < tabWidth {
			return contFail, nil
		}
		*cursor = probe
		// gfmFootnoteDefinitionIndent is a token neither walk steps over.
		return contOK, []spreadEvent{{token: tokenContent, enter: true}}
	}
}

// continueList runs micromark's list continuation (list.js, tokenizeListContinuation).
func (m *lineMachine) continueList(entry *openContainer, cursor *lineCursor) contState {
	if m.blankFrom(*cursor) {
		entry.further = entry.further || entry.initialBlank
		for count := 0; count < entry.size && cursor.space(); count++ {
			cursor.take()
		}
		return contOK
	}
	further := entry.further
	entry.further, entry.initialBlank = false, false
	if further || !cursor.space() {
		return contFail
	}
	probe := *cursor
	count := 0
	for count < entry.size && probe.space() {
		probe.take()
		count++
	}
	if count < entry.size {
		return contFail
	}
	*cursor = probe
	return contOK
}

// openContainer pushes a container the tree opens on the line, writing its enter and the prefix
// tokens micromark reads for it.
func (m *lineMachine) openContainer(entry opening, cursor *lineCursor, line int) {
	switch entry.kind {
	case kindQuote:
		for count := 0; count < 3 && cursor.space(); count++ {
			cursor.take()
		}
		if cursor.at() == '>' {
			cursor.step()
			if cursor.space() {
				cursor.take()
			}
		}
		m.events = append(m.events,
			spreadEvent{token: tokenQuote, enter: true, node: entry.node},
			spreadEvent{token: tokenQuotePrefix, enter: true},
		)
		m.stack = append(m.stack, &openContainer{kind: kindQuote, node: entry.node})
	case kindList:
		size, initialBlank := m.itemPrefix(cursor)
		if entry.newList || len(m.stack) == 0 || m.stack[len(m.stack)-1].node != entry.node {
			m.events = append(m.events, spreadEvent{token: tokenList, enter: true, node: entry.node})
			m.stack = append(m.stack, &openContainer{kind: kindList, node: entry.node})
		}
		held := m.stack[len(m.stack)-1]
		held.size, held.initialBlank, held.further = size, initialBlank, false
		m.events = append(m.events, spreadEvent{token: tokenItemPrefix, enter: true, node: entry.item})
	case kindDirective:
		directive, _ := entry.node.(*typedDirective)
		indent := 0
		if directive != nil {
			indent = directive.indent
		}
		m.events = append(m.events,
			spreadEvent{token: tokenContainer, enter: true, node: entry.node},
			spreadEvent{token: tokenContent, enter: true},
		)
		m.fenced[line] = true
		cursor.toEnd()
		m.stack = append(m.stack, &openContainer{kind: kindDirective, node: entry.node, indent: indent})
	default:
		m.events = append(m.events,
			spreadEvent{token: tokenContainer, enter: true, node: entry.node},
			spreadEvent{token: tokenContent, enter: true},
		)
		m.skipDefinitionLabel(cursor)
		m.stack = append(m.stack, &openContainer{kind: kindDefinition, node: entry.node})
	}
}

// itemPrefix reads a list item's marker the way micromark reads it (list.js, tokenizeListStart),
// and answers how many columns its continuation strips from the lines it continues
// (containerState.size) and whether the rest of the marker's line is blank (initialBlankLine).
func (m *lineMachine) itemPrefix(cursor *lineCursor) (size int, initialBlank bool) {
	// The linePrefix the document tokenizer read before the container start (document.js,
	// tokenizeContainer) opens the item's size.
	initial := 0
	for initial < 3 && cursor.space() {
		cursor.take()
		initial++
	}
	marker := listMarker.Find(m.source[cursor.i:cursor.end])
	cursor.i += len(marker)
	cursor.col += len(marker)
	if m.blankFrom(*cursor) {
		// onBlank counts one more column for the line the marker ends.
		return initial + len(marker) + 1, true
	}
	probe := *cursor
	count := 0
	for count < tabWidth && probe.space() {
		probe.take()
		count++
	}
	if count > 0 && !probe.space() {
		*cursor = probe
		return initial + len(marker) + count, false
	}
	// Five columns or more leave the rest indented code: only one column is the prefix's
	// (otherPrefix).
	if cursor.space() {
		cursor.take()
	}
	return initial + len(marker) + 1, false
}

// skipDefinitionLabel steps the cursor past a footnote definition's `[^label]:` and the
// whitespace after it, which its own line's content starts past.
func (m *lineMachine) skipDefinitionLabel(cursor *lineCursor) {
	for cursor.i < cursor.end && m.source[cursor.i] != ':' {
		cursor.step()
	}
	if cursor.i < cursor.end {
		cursor.step()
	}
	for cursor.space() {
		cursor.take()
	}
}

// exit writes the exits of every container past keep, innermost first, and pops them. lazyLine is
// whether the line the exits stand on was lazy for micromark's flow construct, and continued how
// many continuations ran on it.
func (m *lineMachine) exit(keep int, lazyLine bool, continued int) {
	for index := len(m.stack) - 1; index >= keep; index-- {
		entry := m.stack[index]
		token := tokenContainer
		switch entry.kind {
		case kindQuote:
			token = tokenQuote
		case kindList:
			token = tokenList
		}
		// resolveContentExits settles a typed block's own exit where micromark's flow construct
		// put it, so subtokenize does not move it back over the line endings before it - unless
		// the line was lazy for that construct and its own continuation did not reach the line,
		// where the construct left the exit to micromark (directive-container.ts,
		// exitsOfAnUnclosedDirectiveContainer, `settled`).
		m.events = append(m.events, spreadEvent{
			token:   token,
			node:    entry.node,
			settled: entry.kind == kindDirective && !(lazyLine && index >= continued),
		})
	}
	m.stack = m.stack[:keep]
}

// prepareLists runs prepareList over every list in the stream, a list inside an item before the
// list that holds it, as mdast-util-from-markdown's compiler does.
func prepareLists(events []spreadEvent) spreads {
	found := spreads{}
	var stack []int
	for index := range events {
		if events[index].token != tokenList {
			continue
		}
		if events[index].enter {
			stack = append(stack, index)
			continue
		}
		if len(stack) == 0 {
			continue
		}
		start := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		prepareList(events, start, index, found)
	}
	return found
}

// prepareList is mdast-util-from-markdown's prepareList (dev/lib/index.js) over the events of one
// list: an item is spread by a blank line standing in the item itself - at no nested container,
// at no item's own marker - before the line endings that end it, and the list is spread where
// more than one line ending ends one of its items.
func prepareList(events []spreadEvent, start, length int, found spreads) {
	containerBalance := -1
	listSpread := false
	var listItem ast.Node
	lineIndex, firstBlankLineIndex := 0, 0
	atMarker := false

	for index := start; index <= length; index++ {
		event := events[index]
		switch event.token {
		case tokenList, tokenQuote:
			if event.enter {
				containerBalance++
			} else {
				containerBalance--
			}
			atMarker = false
		case tokenLineEndingBlank:
			if listItem != nil && !atMarker && containerBalance == 0 && firstBlankLineIndex == 0 {
				firstBlankLineIndex = index
			}
			atMarker = false
		case tokenItemPrefix:
			// Empty: an item's own marker tokens leave atMarker alone.
		default:
			atMarker = false
		}

		opensItem := containerBalance == 0 && event.enter && event.token == tokenItemPrefix
		closesList := containerBalance == -1 && !event.enter && event.token == tokenList
		if !opensItem && !closesList {
			continue
		}
		if listItem != nil {
			lineIndex = 0
			for tail := index - 1; tail >= 0; tail-- {
				token := events[tail].token
				if token == tokenLineEnding || token == tokenLineEndingBlank {
					if lineIndex > 0 {
						events[lineIndex].token = tokenLineEndingBlank
						listSpread = true
					}
					events[tail].token = tokenLineEnding
					lineIndex = tail
					continue
				}
				if token == tokenQuotePrefix {
					continue
				}
				break
			}
			if firstBlankLineIndex > 0 && (lineIndex == 0 || firstBlankLineIndex < lineIndex) {
				found[listItem] = true
			}
		}
		if opensItem {
			listItem = event.node
			if _, seen := found[listItem]; !seen {
				found[listItem] = false
			}
			firstBlankLineIndex = 0
			atMarker = true
		}
	}
	found[events[start].node] = listSpread
}
