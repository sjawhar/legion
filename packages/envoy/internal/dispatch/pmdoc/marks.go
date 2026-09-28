package pmdoc

// The mark vocabulary the reader and the writer share: which marks a text is written under, in
// what order, with what delimiters, and how two mark sets compare.

func containsMark(marks []Mark, markType string) bool {
	for _, mark := range marks {
		if mark.Type == markType {
			return true
		}
	}
	return false
}

func withoutMark(marks []Mark, markType string) []Mark {
	out := marks[:0]
	for _, mark := range marks {
		if mark.Type != markType {
			out = append(out, mark)
		}
	}
	return out
}

func nodeHasMark(node *Node, markType string) bool {
	for _, mark := range node.Marks {
		if mark.Type == markType {
			return true
		}
	}
	return false
}

// renderedMarkTypes are the marks canonical Markdown writes. Every other mark the schema allows
// (markTypes) is an anchor, invisible to the rendering, and StripAnchorMarks removes exactly the
// marks that are not here: what renders is one list, not two of opposite polarity that a new
// invisible mark could fall between.
var renderedMarkTypes = map[string]bool{
	"link": true, "strong": true, "emphasis": true, "strike_through": true, "inlineCode": true,
}

// writtenMarks is the marks a node's text is written under: its visible marks, less a bare URL's
// link, which the parser links again on its own. A node that is not text is written under none.
func writtenMarks(node *Node, escapePipes bool) []Mark {
	if node.Type != "text" {
		return nil
	}
	marks := visibleMarks(node.Marks)
	if isBareURLLink(node, marks, escapePipes) {
		marks = withoutMark(marks, "link")
	}
	return marks
}

// adjacentDelimiter is the delimiter character of the innermost of marks opened or closed beside a
// text, the one written next to it, or 0 when that mark is not written with a delimiter run.
func adjacentDelimiter(marks []Mark) byte {
	if len(marks) == 0 {
		return 0
	}
	return markDelimiter(marks[len(marks)-1])
}

// markDelimiter is the character a mark's delimiter run is written with, or 0 when the mark is not
// written with one.
func markDelimiter(mark Mark) byte {
	switch mark.Type {
	case "strong", "emphasis":
		return '*'
	case "strike_through":
		return '~'
	}
	return 0
}

func visibleMarks(marks []Mark) []Mark {
	out := make([]Mark, 0, len(marks))
	for _, mark := range marks {
		if renderedMarkTypes[mark.Type] {
			out = append(out, mark)
		}
	}
	sortMarks(out)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if renderMarkRank(out[j].Type) < renderMarkRank(out[i].Type) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func sharedMarks(left, right []Mark) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for i := range limit {
		if !sameMark(left[i], right[i]) {
			return i
		}
	}
	return limit
}

func sameMark(left, right Mark) bool {
	return left.Type == right.Type && attrsEqual(left.Attrs, right.Attrs)
}

func containsSameMark(marks []Mark, mark Mark) bool {
	for _, other := range marks {
		if sameMark(other, mark) {
			return true
		}
	}
	return false
}

func renderMarkRank(markType string) int {
	switch markType {
	case "link":
		return 0
	case "strong":
		return 1
	case "emphasis":
		return 2
	case "strike_through":
		return 3
	case "inlineCode":
		return 4
	default:
		return 5
	}
}

func openMark(mark Mark) string {
	switch mark.Type {
	case "link":
		return "["
	case "strong":
		return "**"
	case "emphasis":
		return "*"
	case "strike_through":
		return "~~"
	case "inlineCode":
		return "`"
	default:
		return ""
	}
}

func closeMark(mark Mark, escapePipes bool) string {
	if mark.Type == "link" {
		href, _ := mark.Attrs["href"].(string)
		title, _ := mark.Attrs["title"].(string)
		return "](" + escapeLinkDestination(href, escapePipes) + titleSuffix(title, escapePipes) + ")"
	}
	return openMark(mark)
}
func withoutSameMark(marks []Mark, mark Mark) []Mark {
	out := make([]Mark, 0, len(marks))
	for _, other := range marks {
		if !sameMark(other, mark) {
			out = append(out, other)
		}
	}
	return out
}
