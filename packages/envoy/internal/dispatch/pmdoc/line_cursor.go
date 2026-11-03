package pmdoc

import (
	"github.com/yuin/goldmark/ast"
)

// A line read in columns, as micromark reads it: the cursor below is what every container's
// continuation and every blankness test of the port runs on (prepare_list.go).

// lineCursor is a position inside one line, counted in columns: a tab spans as many columns as it
// takes to reach the next multiple of four, and a container takes part of one where its own
// columns end inside it (micromark's preprocess.js turns a tab into a tab code and the virtual
// spaces after it, which factorySpace counts one by one).
type lineCursor struct {
	source []byte
	i      int
	end    int
	col    int
	// tab is how many columns of the tab at i are taken, 0 where no tab is half taken.
	tab int
}

// offset is the position micromark records here: consuming a tab moves it one byte on, and the
// virtual spaces that stand for the tab's remaining columns move it no further
// (micromark/dev/lib/create-tokenizer.js, consume; preprocess.js).
func (c *lineCursor) offset() int {
	if c.tab > 0 {
		return c.i + 1
	}
	return c.i
}

func (m *lineMachine) cursorAt(line int) lineCursor {
	start := m.lines.starts[line]
	end := start + len(m.lines.text(line))
	for end > start && m.source[end-1] == '\n' {
		end--
	}
	return lineCursor{source: m.source, i: start, end: end}
}

// at is the byte under the cursor, or 0 at the line's end.
func (c *lineCursor) at() byte {
	if c.i >= c.end {
		return 0
	}
	return c.source[c.i]
}

// space reports whether the cursor stands on a space or a tab.
func (c *lineCursor) space() bool {
	char := c.at()
	return char == ' ' || char == '\t'
}

// take consumes one column of whitespace, one column of a tab where a tab stands there.
func (c *lineCursor) take() {
	if c.source[c.i] == '\t' {
		c.col++
		c.tab++
		if c.col%tabWidth == 0 {
			c.i++
			c.tab = 0
		}
		return
	}
	c.i++
	c.col++
}

// step consumes one character that is no tab.
func (c *lineCursor) step() {
	c.i++
	c.col++
}

// toEnd takes the cursor to the line's end.
func (c *lineCursor) toEnd() { c.i = c.end }

// blankFrom reports whether the rest of the line is whitespace, which is micromark's blankLine
// check (micromark-core-commonmark/dev/lib/blank-line.js).
func (m *lineMachine) blankFrom(cursor lineCursor) bool {
	for index := cursor.i; index < cursor.end; index++ {
		if m.source[index] != ' ' && m.source[index] != '\t' {
			return false
		}
	}
	return true
}

// moveContainerExits is micromark-util-subtokenize's move of a container's exit back over the line
// endings before it: it steps over line endings, linePrefix and listItemIndent and stops at
// anything else, types the first line ending it reaches a lineEnding and every later one a
// lineEndingBlank, and puts the exit before that first one.
func moveContainerExits(events []spreadEvent) {
	for index := range events {
		event := events[index]
		if event.enter || !event.token.container() || event.settled {
			continue
		}
		lineIndex := 0
		for other := index - 1; other >= 0; other-- {
			if events[other].token != tokenLineEnding && events[other].token != tokenLineEndingBlank {
				break
			}
			if lineIndex > 0 {
				events[lineIndex].token = tokenLineEndingBlank
			}
			events[other].token = tokenLineEnding
			lineIndex = other
		}
		if lineIndex > 0 {
			copy(events[lineIndex+1:index+1], events[lineIndex:index])
			events[lineIndex] = event
		}
	}
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
