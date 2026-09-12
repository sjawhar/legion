package pmdoc

import (
	"bytes"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"go.abhg.dev/goldmark/frontmatter"
)

var anchorAttribute = regexp.MustCompile(`([a-zA-Z0-9_-]+)="([^"]*)"`)

var markdownParser = goldmark.New(
	goldmark.WithExtensions(extension.GFM, extension.Footnote, &frontmatter.Extender{
		Formats: []frontmatter.Format{frontmatter.YAML},
	}),
	goldmark.WithParserOptions(parser.WithBlockParsers(
		util.Prioritized(&typedDirectiveParser{}, 950),
		util.Prioritized(&unsupportedDirectiveParser{}, 900),
	)),
)

// Parse converts markdown into the closed Proof ProseMirror tree.
func Parse(markdown string) (*Node, error) {
	source := []byte(markdown)
	root := markdownParser.Parser().Parse(gmtext.NewReader(source))
	doc, err := parseBlock(root, source, footnoteLabels(root))
	if err != nil {
		return nil, err
	}
	if frontmatter := parseFrontmatterBlock(source); frontmatter != nil {
		doc.Children = append([]*Node{frontmatter}, doc.Children...)
	}
	if len(doc.Children) == 0 {
		doc.Children = []*Node{{Type: "paragraph"}}
	}
	sortNodeMarks(doc)
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	EnsureBlockIDs(doc)
	return doc, nil
}

func parseTableRows(markdown string, width int) ([]*Node, bool, error) {
	if width == 0 {
		return nil, false, nil
	}
	fragment := strings.TrimSpace(markdown)
	if fragment == "" {
		return nil, false, nil
	}

	lines := strings.Split(fragment, "\n")
	for _, line := range lines {
		cells, ok := tableRowCells(line)
		if !ok || tableDelimiterRow(cells) {
			return nil, false, nil
		}
		if len(cells) > width {
			return nil, true, fmt.Errorf("%w: got %d cells, table has %d", ErrTableWidth, len(cells), width)
		}
	}

	parsed, err := Parse(syntheticTableHeader(width) + fragment + "\n")
	if err != nil {
		return nil, false, err
	}
	if len(parsed.Children) != 1 || parsed.Children[0].Type != "table" {
		return nil, false, nil
	}
	rows := parsed.Children[0].Children[1:]
	if len(rows) != len(lines) {
		return nil, false, nil
	}
	return rows, true, nil
}

func syntheticTableHeader(width int) string {
	headers := make([]string, width)
	delimiters := make([]string, width)
	for index := range headers {
		headers[index] = "header"
		delimiters[index] = "---"
	}
	return "| " + strings.Join(headers, " | ") + " |\n| " + strings.Join(delimiters, " | ") + " |\n"
}

func tableRowCells(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, false
	}

	cells := make([]string, 0, 2)
	var cell strings.Builder
	escaped := false
	separatorCount := 0
	endsWithSeparator := false
	for _, char := range line {
		switch {
		case char == '\\':
			cell.WriteRune(char)
			escaped = !escaped
			endsWithSeparator = false
		case char == '|' && !escaped:
			cells = append(cells, cell.String())
			cell.Reset()
			separatorCount++
			endsWithSeparator = true
		default:
			cell.WriteRune(char)
			escaped = false
			endsWithSeparator = false
		}
	}
	cells = append(cells, cell.String())
	if separatorCount == 0 {
		return nil, false
	}
	if line[0] == '|' {
		cells = cells[1:]
	}
	if endsWithSeparator {
		cells = cells[:len(cells)-1]
	}
	if len(cells) == 0 {
		return nil, false
	}
	return cells, true
}

func tableDelimiterRow(cells []string) bool {
	for _, cell := range cells {
		value := strings.TrimSpace(cell)
		value = strings.TrimPrefix(value, ":")
		value = strings.TrimSuffix(value, ":")
		if len(value) < 3 || strings.Trim(value, "-") != "" {
			return false
		}
	}
	return true
}

// parseFrontmatterBlock restores the delimited text the Goldmark extension
// consumes before its completed AST reaches us.
func parseFrontmatterBlock(source []byte) *Node {
	openEnd := bytes.IndexByte(source, '\n')
	if openEnd < 0 || !frontmatterDelimiter(bytes.TrimSuffix(source[:openEnd], []byte("\r"))) {
		return nil
	}
	delimiter := bytes.TrimSuffix(source[:openEnd], []byte("\r"))
	for start := openEnd + 1; start < len(source); {
		end := len(source)
		if next := bytes.IndexByte(source[start:], '\n'); next >= 0 {
			end = start + next
		}
		if bytes.Equal(bytes.TrimSuffix(source[start:end], []byte("\r")), delimiter) {
			return &Node{Type: "frontmatter", Children: []*Node{{Type: "text", Text: string(source[:end])}}}
		}
		if end == len(source) {
			break
		}
		start = end + 1
	}
	return nil
}

func frontmatterDelimiter(line []byte) bool {
	if len(line) < 3 {
		return false
	}
	for _, char := range line {
		if char != '-' {
			return false
		}
	}
	return true
}

func footnoteLabels(root ast.Node) map[int]string {
	labels := make(map[int]string)
	var walk func(ast.Node)
	walk = func(node ast.Node) {
		if definition, ok := node.(*extensionast.Footnote); ok && definition.Index > 0 {
			labels[definition.Index] = string(definition.Ref)
		}
		for child := node.FirstChild(); child != nil; child = child.NextSibling() {
			walk(child)
		}
	}
	walk(root)
	return labels
}

func parseBlock(node ast.Node, source []byte, footnotes map[int]string) (*Node, error) {
	switch current := node.(type) {
	case *ast.Document:
		children, err := parseBlocks(current, source, footnotes)
		if err != nil {
			return nil, err
		}
		return &Node{Type: "doc", Children: children}, nil
	case *ast.Paragraph:
		children, err := parseInline(current, source, nil, footnotes)
		if err != nil {
			return nil, err
		}
		return &Node{Type: "paragraph", Children: children}, nil
	case *ast.TextBlock:
		children, err := parseInline(current, source, nil, footnotes)
		if err != nil {
			return nil, err
		}
		return &Node{Type: "paragraph", Children: children}, nil
	case *ast.Heading:
		children, err := parseInline(current, source, nil, footnotes)
		if err != nil {
			return nil, err
		}
		return &Node{Type: "heading", Attrs: Attrs{"level": current.Level, "id": ""}, Children: children}, nil
	case *ast.Blockquote:
		children, err := parseBlocks(current, source, footnotes)
		if err != nil {
			return nil, err
		}
		return &Node{Type: "blockquote", Children: children}, nil
	case *ast.List:
		return parseList(current, source, footnotes)
	case *ast.ListItem:
		return parseListItem(current, source, footnotes)
	case *ast.FencedCodeBlock:
		language := string(current.Language(source))
		var value any
		if language != "" {
			value = language
		}
		return &Node{
			Type:     "code_block",
			Attrs:    Attrs{"language": value},
			Children: codeBlockText(current.Lines(), source),
		}, nil
	case *ast.CodeBlock:
		return &Node{
			Type:     "code_block",
			Attrs:    Attrs{"language": nil},
			Children: codeBlockText(current.Lines(), source),
		}, nil
	case *ast.ThematicBreak:
		return &Node{Type: "hr"}, nil
	case *ast.HTMLBlock:
		// Proof's doc accepts blocks only, while html is an inline atom.
		return nil, fmt.Errorf("%w: block HTML is not accepted by Proof", ErrSchema)
	case *extensionast.Footnote:
		children, err := parseBlocks(current, source, footnotes)
		if err != nil {
			return nil, err
		}
		if len(children) == 0 {
			children = []*Node{{Type: "paragraph"}}
		}
		return &Node{Type: "footnote_definition", Attrs: Attrs{"label": string(current.Ref)}, Children: children}, nil
	case *typedDirective:
		return parseTypedDirective(current, source, footnotes)
	case *unsupportedDirective:
		return nil, fmt.Errorf("%w: %s", ErrSchema, current.Reason)
	case *extensionast.Table:
		return parseTable(current, source, footnotes)
	default:
		return nil, fmt.Errorf("%w: unsupported markdown block %T", ErrSchema, node)
	}
}

func parseBlocks(parent ast.Node, source []byte, footnotes map[int]string) ([]*Node, error) {
	children := make([]*Node, 0, parent.ChildCount())
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		if footnoteList, ok := child.(*extensionast.FootnoteList); ok {
			for definition := footnoteList.FirstChild(); definition != nil; definition = definition.NextSibling() {
				parsed, err := parseBlock(definition, source, footnotes)
				if err != nil {
					return nil, err
				}
				children = append(children, parsed)
			}
			continue
		}
		parsed, err := parseBlock(child, source, footnotes)
		if err != nil {
			return nil, err
		}

		children = append(children, parsed)
	}
	return children, nil
}
func parseTypedDirective(directive *typedDirective, source []byte, footnotes map[int]string) (*Node, error) {
	if !directive.Closed && directive.Parent().Kind() == ast.KindDocument {
		return nil, fmt.Errorf("%w: typed block %q is unclosed at document level", ErrSchema, directive.Name)
	}
	typ, ok := typedBlock(directive.Name)
	if !ok {
		return nil, fmt.Errorf("%w: unknown typed block %q (known types: %s)", ErrSchema, directive.Name, strings.Join(typedBlockNames(), ", "))
	}
	attrs := defaultAttributes(typ)
	for name, raw := range directive.Attrs {
		if name == BlockIDAttr {
			attrs[name] = raw
			continue
		}
		definition, ok := typ.Attributes[name]
		if !ok {
			return nil, fmt.Errorf("%w: typed block %q does not declare attribute %q", ErrSchema, directive.Name, name)
		}
		value, err := parseTypedAttributeValue(definition, raw)
		if err != nil {
			return nil, fmt.Errorf("%w: typed block %q attribute %q: %v", ErrSchema, directive.Name, name, err)
		}
		attrs[name] = value
	}
	children, err := parseBlocks(directive, source, footnotes)
	if err != nil {
		return nil, err
	}
	return &Node{Type: directive.Name, Attrs: attrs, Children: children}, nil
}

func parseList(list *ast.List, source []byte, footnotes map[int]string) (*Node, error) {
	nodeType := "bullet_list"
	attrs := Attrs{"spread": !list.IsTight}
	if list.IsOrdered() {
		nodeType = "ordered_list"
		attrs["order"] = list.Start
	}
	children := make([]*Node, 0, list.ChildCount())
	for child := list.FirstChild(); child != nil; child = child.NextSibling() {
		item, ok := child.(*ast.ListItem)
		if !ok {
			return nil, fmt.Errorf("%w: list contains %T", ErrSchema, child)
		}
		parsed, err := parseListItem(item, source, footnotes)
		if err != nil {
			return nil, err
		}
		parsed.Attrs["spread"] = !list.IsTight && item.ChildCount() > 1
		if parsed.Attrs["spread"] == true {
			attrs["spread"] = false
		}
		children = append(children, parsed)
	}
	return &Node{Type: nodeType, Attrs: attrs, Children: children}, nil
}

func parseListItem(item *ast.ListItem, source []byte, footnotes map[int]string) (*Node, error) {
	attrs := Attrs{"label": "•", "listType": "bullet", "checked": nil, "spread": false}
	if firstBlock := item.FirstChild(); firstBlock != nil {
		if checkbox, ok := firstBlock.FirstChild().(*extensionast.TaskCheckBox); ok {
			attrs["checked"] = checkbox.IsChecked
		}
	}
	children, err := parseBlocks(item, source, footnotes)
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

func parseTable(table *extensionast.Table, source []byte, footnotes map[int]string) (*Node, error) {
	children := make([]*Node, 0, table.ChildCount())
	for child := table.FirstChild(); child != nil; child = child.NextSibling() {
		switch row := child.(type) {
		case *extensionast.TableHeader:
			parsed, err := parseTableRow(row, true, source, footnotes)
			if err != nil {
				return nil, err
			}
			children = append(children, parsed)
		case *extensionast.TableRow:
			parsed, err := parseTableRow(row, false, source, footnotes)
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

func parseTableRow(row ast.Node, header bool, source []byte, footnotes map[int]string) (*Node, error) {
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
		content, err := parseInline(cell, source, nil, footnotes)
		if err != nil {
			return nil, err
		}
		children = append(children, &Node{
			Type:  cellType,
			Attrs: Attrs{"colspan": 1, "rowspan": 1, "colwidth": nil, "alignment": cell.Alignment.String()},
			Children: []*Node{{
				Type:     "paragraph",
				Children: content,
			}},
		})
	}
	return &Node{Type: nodeType, Children: children}, nil
}

func parseImage(image *ast.Image, source []byte, footnotes map[int]string) (*Node, error) {
	alt, err := parseInline(image, source, nil, footnotes)
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
		"src":   unescapeMarkdownText(image.Destination),
		"alt":   text.String(),
		"title": titleOrNil(image.Title),
	}}, nil
}

func parseInline(parent ast.Node, source []byte, initial []Mark, footnotes map[int]string) ([]*Node, error) {
	active := append([]Mark(nil), initial...)
	var children []*Node
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		switch current := child.(type) {
		case *ast.Text:
			value := parseTextValue(current.Value(source), active)
			if current.HardLineBreak() {
				value = strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "  ")
			}
			appendText(&children, value, active)
			if current.HardLineBreak() {
				children = append(children, &Node{Type: "hardbreak", Attrs: Attrs{"isInline": false}})
			}
			if current.SoftLineBreak() {
				// A soft break is a space, as CommonMark renders it; the browser editor's
				// white-space: break-spaces would show a literal newline as a line break.
				appendText(&children, " ", active)
			}
		case *ast.String:
			appendText(&children, parseTextValue(current.Value, active), active)
		case *ast.Emphasis:
			next := append([]Mark(nil), active...)
			if current.Level >= 2 {
				next = append(next, Mark{Type: "strong", Attrs: Attrs{"marker": "*"}})
			}
			if current.Level%2 == 1 {
				next = append(next, Mark{Type: "emphasis", Attrs: Attrs{"marker": "*"}})
			}
			content, err := parseInline(current, source, next, footnotes)
			if err != nil {
				return nil, err
			}
			appendInline(&children, content)
		case *ast.CodeSpan:
			content, err := parseInline(current, source, append(active, Mark{Type: "inlineCode"}), footnotes)
			if err != nil {
				return nil, err
			}
			appendInline(&children, content)
		case *ast.Link:
			content, err := parseInline(current, source, append(active, Mark{Type: "link", Attrs: Attrs{"href": string(current.Destination), "title": titleOrNil(current.Title)}}), footnotes)
			if err != nil {
				return nil, err
			}
			appendInline(&children, content)
		case *ast.AutoLink:
			appendText(&children, string(current.Label(source)), append(active, Mark{Type: "link", Attrs: Attrs{"href": string(current.URL(source)), "title": nil}}))
		case *extensionast.Strikethrough:
			content, err := parseInline(current, source, append(active, Mark{Type: "strike_through"}), footnotes)
			if err != nil {
				return nil, err
			}
			appendInline(&children, content)
		case *extensionast.TaskCheckBox:
			continue
		case *ast.Image:
			image, err := parseImage(current, source, footnotes)
			if err != nil {
				return nil, err
			}
			children = append(children, image)
		case *extensionast.FootnoteLink:
			label, ok := footnotes[current.Index]
			if !ok {
				return nil, fmt.Errorf("%w: footnote reference %d has no definition", ErrSchema, current.Index)
			}
			children = append(children, &Node{Type: "footnote_reference", Attrs: Attrs{"label": label}})
		case *extensionast.FootnoteBacklink:
			continue
		case *ast.RawHTML:
			value := string(current.Segments.Value(source))
			if strings.EqualFold(strings.TrimSpace(value), "</span>") {
				var removed bool
				active, removed = closeAnchorMark(active)
				if removed {
					continue
				}
			}
			anchor, ok := parseAnchorMark(value)
			if ok {
				active = append(active, anchor)
				continue
			}
			children = append(children, &Node{Type: "html", Attrs: Attrs{"value": value}})
		default:
			return nil, fmt.Errorf("%w: unsupported markdown inline %T", ErrSchema, child)
		}
	}
	return children, nil
}

func unescapeMarkdownText(value []byte) string {
	return html.UnescapeString(string(util.UnescapePunctuations(value)))
}

func parseTextValue(value []byte, marks []Mark) string {
	for _, mark := range marks {
		if mark.Type == "inlineCode" {
			return string(value)
		}
	}
	return unescapeMarkdownText(value)
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
	return unescapeMarkdownText(title)
}

func parseAnchorMark(value string) (Mark, bool) {
	if !strings.HasPrefix(strings.TrimSpace(value), "<span") {
		return Mark{}, false
	}
	attrs := map[string]string{}
	for _, match := range anchorAttribute.FindAllStringSubmatch(value, -1) {
		attrs[match[1]] = match[2]
	}
	if kind := attrs["data-proof"]; kind == "comment" {
		return Mark{Type: "proofComment", Attrs: Attrs{"id": attrs["data-id"], "by": attrs["data-by"]}}, true
	} else if kind == "suggestion" {
		return Mark{Type: "proofSuggestion", Attrs: Attrs{"id": attrs["data-id"], "by": attrs["data-by"], "kind": attrs["data-kind"]}}, true
	}
	if attrs["data-dispatch"] == "ask" {
		return Mark{Type: "dispatchAsk", Attrs: Attrs{"id": attrs["data-id"], "by": attrs["data-by"]}}, true
	}
	return Mark{}, false
}

func closeAnchorMark(marks []Mark) ([]Mark, bool) {
	for index := len(marks) - 1; index >= 0; index-- {
		if strings.HasPrefix(marks[index].Type, "proof") || marks[index].Type == "dispatchAsk" {
			return append(marks[:index], marks[index+1:]...), true
		}
	}
	return marks, false
}
