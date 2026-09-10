package pmdoc

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	gmtext "github.com/yuin/goldmark/text"
	"go.abhg.dev/goldmark/frontmatter"
)

var anchorAttribute = regexp.MustCompile(`([a-zA-Z0-9_-]+)="([^"]*)"`)

var markdownParser = goldmark.New(
	goldmark.WithExtensions(extension.GFM, extension.Footnote, &frontmatter.Extender{}),
)

// Parse converts markdown into the closed Proof ProseMirror tree.
func Parse(markdown string) (*Node, error) {
	source := []byte(markdown)
	root := markdownParser.Parser().Parse(gmtext.NewReader(source))
	doc, err := parseBlock(root, source)
	if err != nil {
		return nil, err
	}
	sortNodeMarks(doc)
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return doc, nil
}

func parseBlock(node ast.Node, source []byte) (*Node, error) {
	switch current := node.(type) {
	case *ast.Document:
		children, err := parseBlocks(current, source)
		if err != nil {
			return nil, err
		}
		return &Node{Type: "doc", Children: children}, nil
	case *ast.Paragraph:
		if image, ok := current.FirstChild().(*ast.Image); ok && image.NextSibling() == nil {
			return parseImage(image, source)
		}
		children, err := parseInline(current, source, nil)
		if err != nil {
			return nil, err
		}
		return &Node{Type: "paragraph", Children: children}, nil
	case *ast.TextBlock:
		children, err := parseInline(current, source, nil)
		if err != nil {
			return nil, err
		}
		return &Node{Type: "paragraph", Children: children}, nil
	case *ast.Heading:
		children, err := parseInline(current, source, nil)
		if err != nil {
			return nil, err
		}
		return &Node{Type: "heading", Attrs: Attrs{"level": current.Level, "id": ""}, Children: children}, nil
	case *ast.Blockquote:
		children, err := parseBlocks(current, source)
		if err != nil {
			return nil, err
		}
		return &Node{Type: "blockquote", Children: children}, nil
	case *ast.List:
		return parseList(current, source)
	case *ast.ListItem:
		return parseListItem(current, source)
	case *ast.FencedCodeBlock:
		return &Node{
			Type:     "code_block",
			Attrs:    Attrs{"language": string(current.Language(source))},
			Children: codeBlockText(current.Lines(), source),
		}, nil
	case *ast.CodeBlock:
		return &Node{
			Type:     "code_block",
			Attrs:    Attrs{"language": ""},
			Children: codeBlockText(current.Lines(), source),
		}, nil
	case *ast.ThematicBreak:
		return &Node{Type: "hr"}, nil
	case *ast.HTMLBlock:
		return &Node{Type: "html", Attrs: Attrs{"value": string(current.Lines().Value(source))}}, nil
	case *extensionast.Table:
		return parseTable(current, source)
	default:
		return nil, fmt.Errorf("%w: unsupported markdown block %T", ErrSchema, node)
	}
}

func parseBlocks(parent ast.Node, source []byte) ([]*Node, error) {
	children := make([]*Node, 0, parent.ChildCount())
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		parsed, err := parseBlock(child, source)
		if err != nil {
			return nil, err
		}
		children = append(children, parsed)
	}
	return children, nil
}

func parseList(list *ast.List, source []byte) (*Node, error) {
	nodeType := "bullet_list"
	attrs := Attrs{"spread": !list.IsTight}
	if list.IsOrdered() {
		nodeType = "ordered_list"
		attrs["order"] = list.Start
	}
	children, err := parseBlocks(list, source)
	if err != nil {
		return nil, err
	}
	return &Node{Type: nodeType, Attrs: attrs, Children: children}, nil
}

func parseListItem(item *ast.ListItem, source []byte) (*Node, error) {
	attrs := Attrs{"label": "•", "checked": nil, "spread": false}
	if firstBlock := item.FirstChild(); firstBlock != nil {
		if checkbox, ok := firstBlock.FirstChild().(*extensionast.TaskCheckBox); ok {
			attrs["checked"] = checkbox.IsChecked
		}
	}
	children, err := parseBlocks(item, source)
	if err != nil {
		return nil, err
	}
	return &Node{Type: "list_item", Attrs: attrs, Children: children}, nil
}

func codeBlockText(lines *gmtext.Segments, source []byte) []*Node {
	value := strings.TrimRight(string(lines.Value(source)), "\r\n")
	if value == "" {
		return nil
	}
	return []*Node{{Type: "text", Text: value}}
}

func parseTable(table *extensionast.Table, source []byte) (*Node, error) {
	children := make([]*Node, 0, table.ChildCount())
	for child := table.FirstChild(); child != nil; child = child.NextSibling() {
		switch row := child.(type) {
		case *extensionast.TableHeader:
			parsed, err := parseTableRow(row, true, source)
			if err != nil {
				return nil, err
			}
			children = append(children, parsed)
		case *extensionast.TableRow:
			parsed, err := parseTableRow(row, false, source)
			if err != nil {
				return nil, err
			}
			children = append(children, parsed)
		default:
			return nil, fmt.Errorf("%w: unsupported table child %T", ErrSchema, child)
		}
	}
	return &Node{Type: "table", Children: children}, nil
}

func parseTableRow(row ast.Node, header bool, source []byte) (*Node, error) {
	nodeType := "table_row"
	cellType := "table_cell"
	if header {
		nodeType = "table_header_row"
		cellType = "table_header"
	}
	children := make([]*Node, 0, row.ChildCount())
	for child := row.FirstChild(); child != nil; child = child.NextSibling() {
		cell, ok := child.(*extensionast.TableCell)
		if !ok {
			return nil, fmt.Errorf("%w: unsupported table cell %T", ErrSchema, child)
		}
		content, err := parseInline(cell, source, nil)
		if err != nil {
			return nil, err
		}
		children = append(children, &Node{
			Type:     cellType,
			Attrs:    Attrs{"alignment": cell.Alignment.String()},
			Children: content,
		})
	}
	return &Node{Type: nodeType, Children: children}, nil
}

func parseImage(image *ast.Image, source []byte) (*Node, error) {
	alt, err := parseInline(image, source, nil)
	if err != nil {
		return nil, err
	}
	var text strings.Builder
	for _, child := range alt {
		if child.Type == "text" {
			text.WriteString(child.Text)
		}
	}
	return &Node{Type: "image", Attrs: Attrs{
		"src":   string(image.Destination),
		"alt":   text.String(),
		"title": string(image.Title),
	}}, nil
}

func parseInline(parent ast.Node, source []byte, initial []Mark) ([]*Node, error) {
	active := append([]Mark(nil), initial...)
	var children []*Node
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		switch current := child.(type) {
		case *ast.Text:
			value := string(current.Value(source))
			if current.HardLineBreak() {
				value = strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "  ")
			}
			appendText(&children, value, active)
			if current.HardLineBreak() {
				children = append(children, &Node{Type: "hardbreak", Attrs: Attrs{"isInline": false}})
			}
			if current.SoftLineBreak() {
				appendText(&children, "\n", active)
			}
		case *ast.String:
			appendText(&children, string(current.Value), active)
		case *ast.Emphasis:
			next := append([]Mark(nil), active...)
			if current.Level >= 2 {
				next = append(next, Mark{Type: "strong", Attrs: Attrs{"marker": "*"}})
			}
			if current.Level%2 == 1 {
				next = append(next, Mark{Type: "emphasis", Attrs: Attrs{"marker": "*"}})
			}
			content, err := parseInline(current, source, next)
			if err != nil {
				return nil, err
			}
			appendInline(&children, content)
		case *ast.CodeSpan:
			content, err := parseInline(current, source, append(active, Mark{Type: "inlineCode"}))
			if err != nil {
				return nil, err
			}
			appendInline(&children, content)
		case *ast.Link:
			content, err := parseInline(current, source, append(active, Mark{Type: "link", Attrs: Attrs{"href": string(current.Destination), "title": titleOrNil(current.Title)}}))
			if err != nil {
				return nil, err
			}
			appendInline(&children, content)
		case *ast.AutoLink:
			appendText(&children, string(current.Label(source)), append(active, Mark{Type: "link", Attrs: Attrs{"href": string(current.URL(source)), "title": nil}}))
		case *extensionast.Strikethrough:
			content, err := parseInline(current, source, append(active, Mark{Type: "strike_through"}))
			if err != nil {
				return nil, err
			}
			appendInline(&children, content)
		case *extensionast.TaskCheckBox:
			continue
		case *ast.RawHTML:
			value := string(current.Segments.Value(source))
			if strings.EqualFold(strings.TrimSpace(value), "</span>") {
				var removed bool
				active, removed = closeAnchorMark(active)
				if !removed {
					return nil, fmt.Errorf("%w: unmatched inline </span>", ErrSchema)
				}
				continue
			}
			anchor, ok, err := parseAnchorMark(value)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("%w: unsupported inline HTML %q", ErrSchema, value)
			}
			active = append(active, anchor)
		default:
			return nil, fmt.Errorf("%w: unsupported markdown inline %T", ErrSchema, child)
		}
	}
	return children, nil
}

func appendInline(target *[]*Node, nodes []*Node) {
	for _, node := range nodes {
		if node.Type == "text" {
			appendText(target, node.Text, node.Marks)
			continue
		}
		*target = append(*target, node)
	}
}

func appendText(target *[]*Node, value string, marks []Mark) {
	if value == "" {
		return
	}
	marks = append([]Mark(nil), marks...)
	sortMarks(marks)
	if len(*target) > 0 {
		last := (*target)[len(*target)-1]
		if last.Type == "text" && marksEqual(last.Marks, marks) {
			last.Text += value
			return
		}
	}
	*target = append(*target, &Node{Type: "text", Text: value, Marks: marks})
}

func titleOrNil(title []byte) any {
	if len(title) == 0 {
		return nil
	}
	return string(title)
}

func parseAnchorMark(value string) (Mark, bool, error) {
	if !strings.HasPrefix(strings.TrimSpace(value), "<span") {
		return Mark{}, false, nil
	}
	attrs := map[string]string{}
	for _, match := range anchorAttribute.FindAllStringSubmatch(value, -1) {
		attrs[match[1]] = match[2]
	}
	if kind := attrs["data-proof"]; kind == "comment" {
		return Mark{Type: "proofComment", Attrs: Attrs{"id": attrs["data-id"], "by": attrs["data-by"]}}, true, nil
	} else if kind == "suggestion" {
		return Mark{Type: "proofSuggestion", Attrs: Attrs{"id": attrs["data-id"], "by": attrs["data-by"], "kind": attrs["data-kind"]}}, true, nil
	}
	if attrs["data-dispatch"] == "ask" {
		return Mark{Type: "dispatchAsk", Attrs: Attrs{"id": attrs["data-id"], "by": attrs["data-by"]}}, true, nil
	}
	return Mark{}, false, fmt.Errorf("%w: unsupported proof span %q", ErrSchema, value)
}

func closeAnchorMark(marks []Mark) ([]Mark, bool) {
	for index := len(marks) - 1; index >= 0; index-- {
		if strings.HasPrefix(marks[index].Type, "proof") || marks[index].Type == "dispatchAsk" {
			return append(marks[:index], marks[index+1:]...), true
		}
	}
	return marks, false
}
