package pmdoc

import (
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

type renderer struct {
	b     strings.Builder
	md16  int
	pos   int
	spans []Span
	err   error
}

func Render(doc *Node) (string, *PositionMap, error) {
	if doc == nil || doc.Type != "doc" {
		if doc == nil {
			return "", nil, fmt.Errorf("%w: Render wants a doc, got nil", ErrSchema)
		}
		return "", nil, fmt.Errorf("%w: Render wants a doc, got %q", ErrSchema, doc.Type)
	}
	if err := doc.Validate(); err != nil {
		return "", nil, err
	}
	r := &renderer{}
	r.blocks(doc.Children, "")
	if r.err != nil {
		return "", nil, r.err
	}
	return r.b.String(), &PositionMap{spans: r.spans}, nil
}

func RenderInline(nodes []*Node) string {
	r := &renderer{}
	r.inline(nodes, "")
	return r.b.String()
}

func (r *renderer) blocks(nodes []*Node, prefix string) {
	for i, n := range nodes {
		if i > 0 {
			r.writeSyntax("\n" + strings.TrimRight(prefix, " ") + "\n")
		}
		r.block(n, prefix)
	}
	if len(nodes) > 0 {
		r.writeSyntax("\n")
	}
}

func (r *renderer) blocksNoTrailing(nodes []*Node, prefix string) {
	for i, n := range nodes {
		if i > 0 {
			r.writeSyntax("\n" + prefix)
		}
		r.block(n, prefix)
	}
}

func (r *renderer) block(n *Node, prefix string) {
	if r.err != nil {
		return
	}
	switch n.Type {
	case "paragraph":
		r.pos++
		r.inline(n.Children, prefix)
		r.pos++
	case "heading":
		level := int(num(n.Attrs["level"], 1))
		r.writeSyntax(strings.Repeat("#", level) + " ")
		r.pos++
		r.inline(n.Children, prefix)
		r.pos++
	case "blockquote":
		r.pos++
		r.writeSyntax("> ")
		r.blocksNoTrailing(n.Children, prefix+"> ")
		r.pos++
	case "bullet_list", "ordered_list":
		r.list(n, prefix)
	case "code_block":
		language, _ := n.Attrs["language"].(string)
		r.writeSyntax("```" + language + "\n")
		r.pos++
		for _, child := range n.Children {
			if child.Type != "text" {
				r.err = fmt.Errorf("%w: code block contains %q", ErrSchema, child.Type)
				return
			}
			r.writeText(child.Text)
		}
		r.writeSyntax("\n" + prefix + "```")
		r.pos++
	case "hr":
		r.writeSyntax("---")
		r.pos++
	case "image":
		src, _ := n.Attrs["src"].(string)
		alt, _ := n.Attrs["alt"].(string)
		title, _ := n.Attrs["title"].(string)
		r.writeSyntax("![" + alt + "](" + src + titleSuffix(title) + ")")
		r.pos++
	case "html":
		value, _ := n.Attrs["value"].(string)
		r.writeSyntax(value)
		r.pos++
	case "frontmatter":
		r.writeSyntax("---\n")
		r.pos++
		for _, child := range n.Children {
			if child.Type != "text" {
				r.err = fmt.Errorf("%w: frontmatter contains %q", ErrSchema, child.Type)
				return
			}
			r.writeText(child.Text)
		}
		r.writeSyntax("\n---")
		r.pos++
	case "table":
		r.table(n, prefix)
	case "footnote_definition":
		label, _ := n.Attrs["label"].(string)
		r.writeSyntax("[^" + label + "]: ")
		r.pos++
		r.blocksNoTrailing(n.Children, prefix+"    ")
		r.pos++
	default:
		r.err = fmt.Errorf("%w: cannot render block %q", ErrSchema, n.Type)
	}
}

func (r *renderer) list(n *Node, prefix string) {
	r.pos++
	start := 1
	if n.Type == "ordered_list" {
		start = int(num(n.Attrs["order"], 1))
	}
	for index, item := range n.Children {
		if item.Type != "list_item" {
			r.err = fmt.Errorf("%w: list contains %q", ErrSchema, item.Type)
			return
		}
		if index > 0 {
			r.writeSyntax("\n" + prefix)
			if n.Attrs["spread"] == true {
				r.writeSyntax("\n" + strings.TrimRight(prefix, " ") + "\n" + prefix)
			}
		}
		marker := "- "
		if n.Type == "ordered_list" {
			marker = fmt.Sprintf("%d. ", start+index)
		}
		r.writeSyntax(marker)
		r.pos++
		if checked, ok := item.Attrs["checked"].(bool); ok {
			if checked {
				r.writeSyntax("[x] ")
			} else {
				r.writeSyntax("[ ] ")
			}
		}
		indent := prefix + strings.Repeat(" ", len(marker))
		for childIndex, child := range item.Children {
			if childIndex > 0 {
				r.writeSyntax("\n" + indent)
			}
			r.block(child, indent)
		}
		r.pos++
	}
	r.pos++
}

func (r *renderer) table(table *Node, prefix string) {
	if len(table.Children) == 0 || table.Children[0].Type != "table_header_row" {
		r.err = fmt.Errorf("%w: table requires a header row", ErrSchema)
		return
	}
	r.pos++
	header := table.Children[0]
	r.tableRow(header, true, prefix)
	r.writeSyntax("\n" + prefix + "| ")
	for i, cell := range header.Children {
		if i > 0 {
			r.writeSyntax(" | ")
		}
		r.writeSyntax(tableAlignment(cell.Attrs["alignment"]))
	}
	r.writeSyntax(" |")
	for _, row := range table.Children[1:] {
		r.writeSyntax("\n" + prefix)
		r.tableRow(row, false, prefix)
	}
	r.pos++
}

func (r *renderer) tableRow(row *Node, header bool, prefix string) {
	want := "table_cell"
	rowType := "table_row"
	if header {
		want = "table_header"
		rowType = "table_header_row"
	}
	if row.Type != rowType {
		r.err = fmt.Errorf("%w: unexpected table row %q", ErrSchema, row.Type)
		return
	}
	r.pos++
	r.writeSyntax("| ")
	for i, cell := range row.Children {
		if cell.Type != want {
			r.err = fmt.Errorf("%w: table row contains %q", ErrSchema, cell.Type)
			return
		}
		if i > 0 {
			r.writeSyntax(" | ")
		}
		r.pos++
		r.inline(cell.Children, prefix)
		r.pos++
	}
	r.writeSyntax(" |")
	r.pos++
}

func (r *renderer) inline(nodes []*Node, prefix string) {
	var active []Mark
	for _, n := range nodes {
		if r.err != nil {
			return
		}
		switch n.Type {
		case "text":
			next := visibleMarks(n.Marks)
			common := sharedMarks(active, next)
			for i := len(active) - 1; i >= common; i-- {
				r.writeSyntax(closeMark(active[i]))
			}
			for _, mark := range next[common:] {
				r.writeSyntax(openMark(mark))
			}
			active = next
			r.writeText(n.Text)
		case "hardbreak":
			r.closeMarks(active)
			active = nil
			r.writeSyntax("  \n" + prefix)
			r.pos++
		case "footnote_reference":
			r.closeMarks(active)
			active = nil
			label, _ := n.Attrs["label"].(string)
			r.writeSyntax("[^" + label + "]")
			r.pos++
		default:
			r.err = fmt.Errorf("%w: cannot render inline %q", ErrSchema, n.Type)
		}
	}
	r.closeMarks(active)
}

func (r *renderer) closeMarks(marks []Mark) {
	for i := len(marks) - 1; i >= 0; i-- {
		r.writeSyntax(closeMark(marks[i]))
	}
}

func (r *renderer) writeSyntax(value string) {
	r.b.WriteString(value)
	r.md16 += len16(value)
}

func (r *renderer) writeText(value string) {
	from := r.md16
	r.b.WriteString(value)
	length := len16(value)
	r.spans = append(r.spans, Span{MdFrom: from, MdTo: from + length, PmFrom: r.pos})
	r.md16 += length
	r.pos += length
}

func visibleMarks(marks []Mark) []Mark {
	out := make([]Mark, 0, len(marks))
	for _, mark := range marks {
		switch mark.Type {
		case "link", "strong", "emphasis", "strike_through", "inlineCode":
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
		if left[i].Type != right[i].Type || !attrsEqual(left[i].Attrs, right[i].Attrs) {
			return i
		}
	}
	return limit
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

func closeMark(mark Mark) string {
	if mark.Type == "link" {
		href, _ := mark.Attrs["href"].(string)
		title, _ := mark.Attrs["title"].(string)
		return "](" + href + titleSuffix(title) + ")"
	}
	return openMark(mark)
}

func titleSuffix(title string) string {
	if title == "" {
		return ""
	}
	return " \"" + title + "\""
}

func tableAlignment(value any) string {
	alignment, _ := value.(string)
	switch alignment {
	case "center":
		return ":---:"
	case "right":
		return "---:"
	default:
		return ":---"
	}
}

func num(value any, fallback float64) float64 {
	switch number := value.(type) {
	case float64:
		return number
	case float32:
		return float64(number)
	case int:
		return float64(number)
	case int64:
		return float64(number)
	default:
		return fallback
	}
}

func len16(value string) int {
	return text.Len16(value)
}
