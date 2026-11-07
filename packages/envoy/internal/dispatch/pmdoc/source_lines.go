package pmdoc

import (
	"bytes"
	"sort"

	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
)

// itemContentColumn is the column item's content starts at as the browser editor's parser measures
// it: where its first block's text does (browserTextColumn), except where the rest of the item's
// marker line is blank or indented code, five columns or more, where it is one column past the
// marker, however far in the first block then stands.
func (l sourceLines) itemContentColumn(item ast.Node) int {
	start := startOf(item)
	line := l.starts[l.lineOf(start)]
	marker := start
	for marker < len(l.source) && (l.source[marker] == ' ' || l.source[marker] == '\t') {
		marker++
	}
	end := marker + len(listMarker.Find(l.source[marker:]))
	text := end
	for text < len(l.source) && (l.source[text] == ' ' || l.source[text] == '\t') {
		text++
	}
	markerEnd, textStart := columnOf(l.source[line:end]), columnOf(l.source[line:text])
	if text == len(l.source) || l.source[text] == '\n' || textStart-markerEnd >= 5 {
		return markerEnd + 1
	}
	return browserTextColumn(item.FirstChild(), l)
}

// leadingQuoteMarkers is how many quote markers open line, each with the spaces and tabs before it.
func leadingQuoteMarkers(line []byte) int {
	count := 0
	for {
		line = bytes.TrimLeft(line, " \t")
		if len(line) == 0 || line[0] != '>' {
			return count
		}
		count++
		line = line[1:]
	}
}

// quoteContentColumn is the column the content of count quotes starts at on position's line: past
// each quote marker, the spaces and tabs before it, and one column of the space or tab after it -
// the rest of a tab there is the content's indentation.
func (l sourceLines) quoteContentColumn(position, count int) int {
	index, column := l.starts[l.lineOf(position)], 0
	advance := func() {
		if l.source[index] == '\t' {
			column += 4 - column%4
		} else {
			column++
		}
		index++
	}
	for ; count > 0; count-- {
		for index < len(l.source) && (l.source[index] == ' ' || l.source[index] == '\t') {
			advance()
		}
		if index >= len(l.source) || l.source[index] != '>' {
			break
		}
		advance()
		if index < len(l.source) && (l.source[index] == ' ' || l.source[index] == '\t') {
			if count == 1 {
				return column + 1
			}
			advance()
		}
	}
	return column
}

// pastQuoteMarkers is position past count quote markers on its line, each with the spaces and tabs
// before it and the space after it.
func (l sourceLines) pastQuoteMarkers(position, count int) int {
	for ; count > 0; count-- {
		for position < len(l.source) && (l.source[position] == ' ' || l.source[position] == '\t') {
			position++
		}
		if position >= len(l.source) || l.source[position] != '>' {
			break
		}
		position++
		if position < len(l.source) && l.source[position] == ' ' {
			position++
		}
	}
	return position
}

// browserTextColumn is the column block's text starts at as the browser editor's parser measures
// it: on a footnote definition's first line, the definition's text starts where its later lines'
// text does, four columns past where its container's content starts, whatever the spaces before
// its `[^` and after its `]:`.
func browserTextColumn(block ast.Node, lines sourceLines) int {
	column := lines.textColumn(startOf(block))
	definition, footnoted := ancestor[*extensionast.Footnote](block)
	if !footnoted || lines.lineOf(definition.Pos()) != lines.lineOf(startOf(block)) {
		return column
	}
	base := lines.textColumn(definition.Pos())
	switch definition.Parent().(type) {
	case *ast.Document:
		base = 0
	case *ast.Blockquote:
		base = lines.quoteContentColumn(definition.Pos(), quoteDepth(definition))
	}
	return column - lines.textColumn(startOf(definition.FirstChild())) + base + 4
}

// sourceLines is where each line of a source starts, which of its lines indented code holds past
// where goldmark ends the code (held): those are the code's, not blank, wherever a blank line is
// counted (heldCodeLines).
type sourceLines struct {
	source []byte
	starts []int
	held   map[int]bool
}

func newSourceLines(source []byte) sourceLines {
	starts := []int{0}
	for index, char := range source {
		if char == '\n' && index+1 < len(source) {
			starts = append(starts, index+1)
		}
	}
	return sourceLines{source: source, starts: starts}
}

func (l sourceLines) lineOf(position int) int {
	return sort.SearchInts(l.starts, position+1) - 1
}

func (l sourceLines) text(line int) []byte {
	end := len(l.source)
	if line+1 < len(l.starts) {
		end = l.starts[line+1]
	}
	return l.source[l.starts[line]:end]
}

// textColumn is the column the text from position on stands at on its line, past any spaces and
// tabs, a tab advancing to the next multiple of four, as markdown measures indentation. Goldmark
// starts a block indented four columns or more, an indented code block, at its line's start.
func (l sourceLines) textColumn(position int) int {
	for position < len(l.source) && (l.source[position] == ' ' || l.source[position] == '\t') {
		position++
	}
	return columnOf(l.source[l.starts[l.lineOf(position)]:position])
}

// blanksBefore is how many blank lines come right before position's line.
func (l sourceLines) blanksBefore(position int, blank func([]byte) bool) int {
	count := 0
	for line := l.lineOf(position) - 1; line >= 0 && l.blank(line, blank); line-- {
		count++
	}
	return count
}

// tabWidth is how many columns a tab spans at most: every parser here advances to the next
// multiple of four.
const tabWidth = 4

// blank reports whether line is blank as blank reads it and not a line indented code holds.
func (l sourceLines) blank(line int, blank func([]byte) bool) bool {
	return !l.held[line] && blank(l.text(line))
}

// markersOrWhitespace reports whether line holds nothing but whitespace and quote markers.
func markersOrWhitespace(line []byte) bool {
	return len(bytes.Trim(line, " \t>\n")) == 0
}

// quoteDepth is how many quotes node stands in.
func quoteDepth(node ast.Node) int {
	depth := 0
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		if _, quote := parent.(*ast.Blockquote); quote {
			depth++
		}
	}
	return depth
}
