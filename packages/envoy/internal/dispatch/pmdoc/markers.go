package pmdoc

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MarkerKind names the markdown marker a textblock renders before its own text.
type MarkerKind string

const (
	// MarkerNone is a block the renderer writes no marker for, such as a paragraph outside a list.
	MarkerNone    MarkerKind = ""
	MarkerHeading MarkerKind = "heading"
	MarkerOrdered MarkerKind = "ordered list"
	MarkerBullet  MarkerKind = "bullet list"
)

// BlockMarker is one block marker. Level carries a heading's level; Number carries an ordered
// item's own number, the one the renderer writes before its text.
type BlockMarker struct {
	Kind   MarkerKind
	Level  int
	Number int
}

// Markdown renders the marker the way the document renderer writes it, for error text.
func (m BlockMarker) Markdown() string {
	switch m.Kind {
	case MarkerHeading:
		return strings.Repeat("#", max(m.Level, 1)) + " "
	case MarkerOrdered:
		return strconv.Itoa(max(m.Number, 1)) + ". "
	case MarkerBullet:
		return "- "
	default:
		return ""
	}
}

var (
	leadingHeadingMarker = regexp.MustCompile(`^[ \t]*(#{1,6})[ \t]+`)
	leadingOrderedMarker = regexp.MustCompile(`^[ \t]*[0-9]{1,9}[.)][ \t]+`)
	leadingBulletMarker  = regexp.MustCompile(`^[ \t]*[-*+][ \t]+`)
)

// LeadingBlockMarker reports the block marker markdown opens with and how many bytes of markdown
// it spans, so a caller can tell the marker apart from the text that follows it. Leading spaces
// belong to the marker: a nested item's replacement carries the indent its level renders with.
func LeadingBlockMarker(markdown string) (BlockMarker, int) {
	if match := leadingHeadingMarker.FindStringSubmatch(markdown); match != nil {
		return BlockMarker{Kind: MarkerHeading, Level: len(match[1])}, len(match[0])
	}
	if match := leadingOrderedMarker.FindString(markdown); match != "" {
		return BlockMarker{Kind: MarkerOrdered}, len(match)
	}
	if match := leadingBulletMarker.FindString(markdown); match != "" {
		return BlockMarker{Kind: MarkerBullet}, len(match)
	}
	return BlockMarker{}, 0
}

// MarkerAt returns the marker of the textblock whose own text begins at position, and reports
// false anywhere else. Only a replacement landing where the renderer has just written a marker
// can repeat it; one landing mid-block is ordinary text.
func MarkerAt(doc *Node, position int) (BlockMarker, bool) {
	var marker BlockMarker
	found := false
	walk(doc, func(node *Node, path []int, pos, _ int) bool {
		if !isTextblock(node.Type) || pos+1 != position {
			return true
		}
		found = true
		marker = textblockMarker(doc, node, path)
		return false
	})
	return marker, found
}

func textblockMarker(doc, node *Node, path []int) BlockMarker {
	if node.Type == "heading" {
		return BlockMarker{Kind: MarkerHeading, Level: int(num(node.Attrs["level"], 1))}
	}
	// Only a paragraph can be carrying a list item's marker: a code block's first line follows a
	// fence, and its content is literal.
	if node.Type != "paragraph" {
		return BlockMarker{}
	}
	// A list item renders its marker before its first child only; every later child of the same
	// item is indented under it.
	if len(path) < 2 || path[len(path)-1] != 0 {
		return BlockMarker{}
	}
	if nodeAtPath(doc, path[:len(path)-1]).Type != "list_item" {
		return BlockMarker{}
	}
	item := path[len(path)-2]
	list := nodeAtPath(doc, path[:len(path)-2])
	switch list.Type {
	case "ordered_list":
		return BlockMarker{Kind: MarkerOrdered, Number: int(num(list.Attrs["order"], 1)) + item}
	case "bullet_list":
		return BlockMarker{Kind: MarkerBullet}
	}
	return BlockMarker{}
}

// TextblockAt is the textblock whose content holds a position.
type TextblockAt struct {
	Node *Node
	// Ancestors are the nodes around it, the nearest first and the document last.
	Ancestors []*Node
	// Content is the range its content covers.
	Content Range
}

// ContainingTextblock returns the textblock whose content holds position, and reports false when
// position is in none.
func ContainingTextblock(doc *Node, position int) (TextblockAt, bool) {
	var at TextblockAt
	found := false
	walk(doc, func(node *Node, path []int, pos, end int) bool {
		if !isTextblock(node.Type) || position < pos+1 || position > end-1 {
			return true
		}
		at = TextblockAt{Node: node, Content: Range{From: pos + 1, To: end - 1}}
		for depth := len(path) - 1; depth >= 0; depth-- {
			at.Ancestors = append(at.Ancestors, nodeAtPath(doc, path[:depth]))
		}
		found = true
		return false
	})
	return at, found
}

// OneLine reports whether the textblock is written on one markdown line - a heading, or a table
// cell's paragraph - where a hard break would end the block.
func (at TextblockAt) OneLine() bool {
	parent := at.Ancestors[0]
	return at.Node.Type == "heading" || parent.Type == "table_cell" || parent.Type == "table_header"
}

// SetHeadingLevel returns a copy of doc with the level of the heading whose own text begins at
// position set to level. A `replace` that carried a heading marker through `find` is renaming the
// heading, so a different level in its replacement is how the caller says "and make it that one".
func SetHeadingLevel(doc *Node, position, level int) (*Node, error) {
	var target []int
	walk(doc, func(node *Node, path []int, pos, _ int) bool {
		if node.Type != "heading" || pos+1 != position {
			return true
		}
		target = append([]int(nil), path...)
		return false
	})
	if target == nil {
		return nil, fmt.Errorf("%w: heading at position %d", ErrTargetNotFound, position)
	}
	out := cloneNode(doc)
	heading := nodeAtPath(out, target)
	attrs := make(Attrs, len(heading.Attrs)+1)
	for name, value := range heading.Attrs {
		attrs[name] = value
	}
	attrs["level"] = float64(level)
	heading.Attrs = attrs
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// HeadingMarker returns the ATX marker a quote carries to select a heading by its text, or the
// empty string when it carries none. The level is not matched, only the heading-ness.
func HeadingMarker(quote string) string {
	marker := atxHeadingMarker.FindString(quote)
	if marker == quote {
		return ""
	}
	return marker
}

// unbalancedInlineDelimiters are the inline marks a quote is matched through (renderedMarkdownQuote),
// each written as a pair.
var unbalancedInlineDelimiters = []string{"**", "`"}

// UnbalancedInlineMark reports the inline delimiter a quote opens and never closes. Such a quote
// can never match the text as rendered, so the miss is about the quote, not the document.
func UnbalancedInlineMark(quote string) (string, bool) {
	for _, delimiter := range unbalancedInlineDelimiters {
		if countUnescaped(quote, delimiter)%2 == 1 {
			return delimiter, true
		}
	}
	return "", false
}

func countUnescaped(text, delimiter string) int {
	count := 0
	for index := 0; index < len(text); {
		if text[index] == '\\' {
			index += 2
			continue
		}
		if strings.HasPrefix(text[index:], delimiter) {
			count++
			index += len(delimiter)
			continue
		}
		index++
	}
	return count
}
