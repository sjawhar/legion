package pmdoc

import (
	"github.com/yuin/goldmark/ast"
)

// lineSuffix is the spaces and tabs ending the line text's segment stands on, and where they start,
// or whether a backslash ends the line instead, a hard break that keeps what stands before it.
func lineSuffix(text *ast.Text, source []byte) (start int, run string, backslash bool) {
	end := text.Segment.Stop
	for end < len(source) && source[end] != '\n' {
		end++
	}
	if end > 0 && source[end-1] == '\\' {
		return end, "", true
	}
	start = end
	for start > 0 && (source[start-1] == ' ' || source[start-1] == '\t') {
		start--
	}
	return start, string(source[start:end]), false
}

// trimLineSuffixes takes the spaces and tabs a line of parent's text ends with, before a break no
// backslash makes, off the text before it, as the browser editor's parser drops them. Goldmark
// keeps all but the last in the text it ends a line's run of it with, and gives the break to an
// empty text after them.
func trimLineSuffixes(parent ast.Node, source []byte) {
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		text, ok := child.(*ast.Text)
		if !ok || !text.SoftLineBreak() && !text.HardLineBreak() {
			continue
		}
		start, _, backslash := lineSuffix(text, source)
		if backslash {
			continue
		}
		for node := ast.Node(text); node != nil; node = node.PreviousSibling() {
			before, ok := node.(*ast.Text)
			if !ok || before.Segment.Stop <= start {
				break
			}
			before.Segment = before.Segment.WithStop(max(before.Segment.Start, start))
		}
	}
}
