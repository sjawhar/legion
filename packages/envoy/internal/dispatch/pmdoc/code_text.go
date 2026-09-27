package pmdoc

import (
	"bytes"
	"strings"

	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
	gmtext "github.com/yuin/goldmark/text"
)

// fencedCodeText is a fenced code block's lines as its text: every line it holds, blank ones
// included, as the browser editor's parser reads them, without the last line's line feed. Of a
// block no fence closed, that parser drops the last line where it is blank and the block ends in a
// quote, or in a list item or footnote definition flow content follows: the blank line is the
// container's there.
func fencedCodeText(code *ast.FencedCodeBlock, source []byte) []*Node {
	value := strings.TrimSuffix(segmentsText(code.Lines(), source), "\n")
	if _, closed := code.Attribute(fenceClosedAttr); !closed && blankTaker(code, source) != nil {
		value = strings.TrimSuffix(value, "\n")
	}
	if value == "" {
		return nil
	}
	return []*Node{{Type: "text", Text: value}}
}

// blankTaker is the container that takes a blank line a fenced code block no fence closed ends in
// from the block's text (fencedCodeText), or nil where the text keeps it: a list item and a
// footnote definition that flow content follows - a paragraph, a heading, a rule, a fence, a typed
// block, a table - on a line that parser reads as ending them take it, and so does a quote, unless
// such a container opens on the line right after the quote's blank line. A line that opens a
// container instead (startsContainer: a quote, a list item of any list, or a definition) leaves
// the blank line the block's, as do a typed block's fence and, but for a quote's, the document's
// end. A last item and a definition nothing follows in its container end with the container around
// them.
func blankTaker(code ast.Node, source []byte) ast.Node {
	for container := code.Parent(); container != nil; container = container.Parent() {
		switch container := container.(type) {
		case *ast.Blockquote:
			// The quote's blank lines are its own lines, so after one of them outside it, or at
			// the end, the quote takes the code's last.
			lines := code.Lines()
			if next := nextBlock(container); next != nil && lines.Len() > 0 && startsContainer(next) &&
				bytes.Count(source[lines.At(lines.Len()-1).Start:startOf(next)], []byte("\n")) == 1 {
				return nil
			}
			return container
		case *ast.ListItem:
			if container.NextSibling() != nil {
				return nil
			}
			if next := nextBlock(container.Parent()); next != nil {
				if startsContainer(next) {
					return nil
				}
				return container
			}
		case *extensionast.Footnote:
			if next := container.NextSibling(); next != nil {
				if startsContainer(next) {
					return nil
				}
				return container
			}
		case *typedDirective:
			return nil
		}
	}
	return nil
}

// codeBlockText is an indented code block's lines as its text; its trailing blank lines are the
// blank lines after it.
func codeBlockText(lines *gmtext.Segments, source []byte) []*Node {
	value := strings.TrimRight(segmentsText(lines, source), "\n")
	if value == "" {
		return nil
	}
	return []*Node{{Type: "text", Text: value}}
}
