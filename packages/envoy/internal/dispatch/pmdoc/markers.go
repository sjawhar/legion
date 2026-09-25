package pmdoc

import (
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
	switch node.Type {
	case "heading":
		return BlockMarker{Kind: MarkerHeading, Level: int(num(node.Attrs["level"], 1))}
	case "paragraph":
	default:
		// A code block's first line follows a fence, and its content is literal.
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
