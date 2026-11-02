package pmdoc

import (
	"bytes"
	"regexp"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// emptyItemGuard is a list parser that opens no list whose first item could not interrupt a
// paragraph - an empty item, a marker with nothing after it on its line, or an ordered item
// numbered anything but a lone `1` (orderedCannotInterrupt) - where it would, as the browser
// editor's parser reads it. Goldmark refuses one only while the paragraph is the block last opened,
// and there lets an item numbered `01.` interrupt it; that parser also refuses one opening a
// container on a line that already interrupted the paragraph, so `- a\n  - -` is an item holding
// the text `-`, `- a\n  > -` a quote holding it, and `a\n> 2. b` a quote holding the paragraph
// `2. b`, and refuses one where indented code is still the open construct (interruptsOpenBlock),
// so `    code\n> 2. b` is a quote holding that paragraph too, the quote opening on the item's own
// line, while in `    code\n>\n> 2. b` the quote opened a line earlier, which ends the code, and
// the item opens the ordered list that quote holds.
type emptyItemGuard struct{ parser.BlockParser }

func (p emptyItemGuard) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, segment := reader.PeekLine()
	if !bareMarkerLine.Match(bytes.TrimRight(line, "\n")) && !orderedCannotInterrupt(line) {
		return p.BlockParser.Open(parent, reader, pc)
	}
	if last, paragraph := pc.LastOpenedBlock().Node.(*ast.Paragraph); paragraph && last.Parent() == parent {
		return nil, parser.NoChildren
	}
	if interruptsOpenBlock(parent, reader.Source(), lineStart(reader.Source(), segment.Start), pc) {
		return nil, parser.NoChildren
	}
	return p.BlockParser.Open(parent, reader, pc)
}

// Continue keeps the list open for a line holding its empty last item's content, as the browser
// editor's parser reads it: an item whose marker line holds nothing takes its content from the
// next line when that line, with no blank line between, reaches the item's content column, a list
// marker there included. Goldmark closes the list when that marker cannot continue it - another
// bullet character, or an ordered marker under a bullet - so `-\n  1.` read as two lists and
// `> 1.\n>    2) a` as two quoted lists.
func (p emptyItemGuard) Continue(node ast.Node, reader gmtext.Reader, pc parser.Context) parser.State {
	state := p.BlockParser.Continue(node, reader, pc)
	item, ok := node.LastChild().(*ast.ListItem)
	if state != parser.Close || !ok || item.ChildCount() != 0 {
		return state
	}
	line, segment := reader.PeekLine()
	source := reader.Source()
	start := lineStart(source, segment.Start)
	if util.IsBlank(line) || start == 0 || markersOrWhitespace(source[lineStart(source, start-1):start]) {
		return state
	}
	if indent, _ := util.IndentWidth(line, reader.LineOffset()); indent < item.Offset {
		return state
	}
	return parser.Continue | parser.HasChildren
}

// orderedMarkerStart is an ordered list marker opening a line, its number captured.
var orderedMarkerStart = regexp.MustCompile(`^ {0,3}([0-9]{1,9})[.)](?:[ \t]|\n|$)`)

// orderedCannotInterrupt reports whether line opens an ordered list item the browser editor's
// parser lets interrupt no paragraph: one numbered anything but a lone `1`, so `01.`, `001)` and
// `10.` as well as `2.`, where goldmark takes the number's value and lets `01.` interrupt one.
func orderedCannotInterrupt(line []byte) bool {
	match := orderedMarkerStart.FindSubmatch(line)
	return match != nil && string(match[1]) != "1"
}

// interruptsOpenBlock reports whether a list opening in parent on the line starting at start
// interrupts a block the browser editor's parser holds open there, other than a paragraph parent
// holds, which goldmark names: a paragraph ending on the line before, reached across the containers
// the line opens first, that forms no table, or an indented code block the flow the line belongs to
// still holds open. That parser decides once per line whether it interrupts, from the construct it
// holds open, before opening the containers the line starts with, and keeps indented code open to
// the line - except code right after a list, or right after a quote with no blank line between
// them. The list stays open across the blank lines before the code, and the quote across the code's
// first line; neither continues the line after it, and that parser ends indented code at a line no
// open container continues, so the code has ended and the line interrupts nothing (which is also
// why it reads a second code line there as a second code block). A container opened on a line after
// the code, such as a typed block's fence, starts a flow of its own that holds nothing open: where
// the code is reached only by walking up out of the containers the line opens (opensContainers),
// it is the open construct only while no line between it and this one carries a container's marker,
// since a line carrying one is a line that container was already open on, and by this line its own
// flow holds nothing. So `    code\n> -` is a quote holding the paragraph `-` and `    code\n\n> -`
// is too, the quote opening on the item's own line and the blank line between opening nothing,
// while `    code\n>\n> -` is a quote holding a list with one empty item, its quote having opened a
// line earlier. Where the code is parent's own last child instead, no container opened after it and
// lines of markers and whitespace leave it open, so `>     code\n>  \n> -` is a quoted paragraph.
func interruptsOpenBlock(parent ast.Node, source []byte, start int, pc parser.Context) bool {
	previous := parent.LastChild()
	opensContainers := previous == nil
	for node := parent; previous == nil && node != nil && node.Kind() != ast.KindDocument; node = node.Parent() {
		previous = node.PreviousSibling()
	}
	switch previous := previous.(type) {
	case *ast.Paragraph:
		// A table goldmark makes of the paragraph is no paragraph to that parser, and holds
		// nothing open a line could interrupt.
		if !opensContainers || previous.Lines().Len() == 0 {
			return false
		}
		if table, _ := formsTable(previous, source, pc); table {
			return false
		}
		last := previous.Lines().At(previous.Lines().Len() - 1)
		return bytes.Count(source[last.Start:start], []byte("\n")) <= 1
	case *ast.CodeBlock:
		if heldOpenBy(previous) != nil {
			return false
		}
		if previous.Lines().Len() == 0 {
			return false
		}
		last := previous.Lines().At(previous.Lines().Len() - 1)
		between := source[last.Start:start]
		between = between[bytes.IndexByte(between, '\n')+1:]
		for _, line := range bytes.Split(bytes.TrimSuffix(between, []byte("\n")), []byte("\n")) {
			if util.IsBlank(line) {
				continue
			}
			if opensContainers || !markersOrWhitespace(line) {
				return false
			}
		}
		return true
	}
	return false
}
