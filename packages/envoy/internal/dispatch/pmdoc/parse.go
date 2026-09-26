package pmdoc

import (
	"fmt"
	"html"
	"regexp"
	"slices"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var anchorAttribute = regexp.MustCompile(`([a-zA-Z0-9_-]+)="([^"]*)"`)

// markdownReader is the one goldmark configuration Dispatch reads markdown with. Its only way in
// is parse. Front matter is read apart from it (parseFrontmatterBlock), so an unclosed opener is
// ordinary markdown. Its list parser opens no empty item that would interrupt a paragraph
// (emptyItemGuard).
type markdownReader struct {
	md goldmark.Markdown
}

var blockReader = markdownReader{md: goldmark.New(
	goldmark.WithParser(parser.NewParser(
		parser.WithBlockParsers(
			util.Prioritized(parser.NewSetextHeadingParser(), 100),
			util.Prioritized(parser.NewThematicBreakParser(), 200),
			util.Prioritized(emptyItemGuard{parser.NewListParser()}, 300),
			util.Prioritized(parser.NewListItemParser(), 400),
			util.Prioritized(parser.NewCodeBlockParser(), 500),
			util.Prioritized(parser.NewATXHeadingParser(), 600),
			util.Prioritized(parser.NewFencedCodeBlockParser(), 700),
			util.Prioritized(parser.NewBlockquoteParser(), 800),
			util.Prioritized(parser.NewHTMLBlockParser(), 900),
		),
		parser.WithInlineParsers(parser.DefaultInlineParsers()...),
		parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...),
	)),
	goldmark.WithExtensions(extension.Linkify, lazyAwareTable{}, extension.Strikethrough, taskList{}, footnotes{}),
	goldmark.WithParserOptions(
		parser.WithBlockParsers(
			util.Prioritized(&typedDirectiveParser{}, 950),
			util.Prioritized(&unsupportedDirectiveParser{}, 900),
			util.Prioritized(lineRecordingParagraph{parser.NewParagraphParser()}, 999),
		),
	),
)}

// Parse converts markdown into the closed Proof ProseMirror tree.
func Parse(markdown string) (*Node, error) {
	doc, err := parseUnstamped(markdown)
	if err != nil {
		return nil, err
	}
	EnsureBlockIDs(doc)
	return doc, nil
}

// ParseForWrite parses markdown a caller is writing as Parse does, except that a block id the
// markdown names on two blocks is refused (ErrSchema) instead of repaired, since the repair would
// silently give the id to whichever block comes first. live is the document the markdown replaces
// whole, or nil for a fragment or a new document: a repeat live already carries is not refused
// (RepeatedBlockID), and the repair keeps it for its first block, as settlement would.
func ParseForWrite(markdown string, live *Node) (*Node, error) {
	doc, err := parseUnstamped(LineFeeds(markdown))
	if err != nil {
		return nil, err
	}
	if err := RepeatedBlockID(live, doc, doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSchema, err)
	}
	EnsureBlockIDs(doc)
	return doc, nil
}

// LineFeeds is text with each CR LF and each lone carriage return written as a line feed. Both
// end a line in CommonMark and in the browser editor, so text a caller writes into a document is
// converted where it enters (ParseForWrite, ParseInline, and the server's other writes of caller
// text), and the parser and the renderer see line feeds alone.
func LineFeeds(text string) string {
	if !strings.Contains(text, "\r") {
		return text
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// parseUnstamped is Parse before EnsureBlockIDs: blocks keep the ids their markdown names, and a
// block that names none has none yet.
func parseUnstamped(markdown string) (*Node, error) {
	source := []byte(markdown)
	front, rest := parseFrontmatterBlock(source)
	source = source[rest:]
	root := blockReader.parse(source)
	doc, err := parseBlock(root, source, footnoteLabels(root))
	if err != nil {
		return nil, err
	}
	if front != nil {
		doc.Children = append([]*Node{front}, doc.Children...)
	}
	if len(doc.Children) == 0 {
		doc.Children = []*Node{{Type: "paragraph"}}
	}
	sortNodeMarks(doc)
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return doc, nil
}

// inlineParserOptions is how inline markdown is read: a paragraph is the only block, so a leading
// list marker, heading marker, fence, or directive is text, and the inline syntax is Parse's.
func inlineParserOptions() []parser.Option {
	return []parser.Option{
		parser.WithBlockParsers(util.Prioritized(lineRecordingParagraph{parser.NewParagraphParser()}, 1000)),
		parser.WithInlineParsers(parser.DefaultInlineParsers()...),
		parser.WithInlineParsers(
			util.Prioritized(extension.NewStrikethroughParser(), 500),
			util.Prioritized(extension.NewLinkifyParser(), 999),
		),
	}
}

// inlineMarkdownParser reads inline markdown (inlineParserOptions).
var inlineMarkdownParser = parser.NewParser(inlineParserOptions()...)

// footnoteRunParser reads inline markdown as inlineMarkdownParser does, with footnote definitions
// after it so that the references in it read as references (parseInlineWithDefinitions).
var footnoteRunParser = parser.NewParser(append(inlineParserOptions(), footnoteParserOptions()...)...)

// parseInlineWithDefinitions reads one textblock's inline markdown as ParseInline does, after a
// definition for each of labels, the footnote labels it refers to. It is the renderer's read-back
// of a run it wrote, where a reference is only a reference beside its definition.
func parseInlineWithDefinitions(markdown string, labels []string) ([]*Node, error) {
	if len(labels) == 0 {
		return ParseInline(markdown)
	}
	var full strings.Builder
	full.WriteString(markdown)
	for _, label := range labels {
		full.WriteString("\n\n[^" + escapeFootnoteLabel(label) + "]: x")
	}
	source := []byte(full.String())
	root := withLineStarts(footnoteRunParser, source, parser.NewContext())
	first, ok := root.FirstChild().(*ast.Paragraph)
	if !ok {
		return nil, fmt.Errorf("%w: inline markdown does not read as a paragraph", ErrSchema)
	}
	paragraph, err := parseBlock(first, source, footnoteLabels(root))
	if err != nil {
		return nil, err
	}
	sortNodeMarks(paragraph)
	return paragraph.Children, nil
}

// referencedLabels is the footnote labels nodes refer to, each once, in order.
func referencedLabels(nodes []*Node) []string {
	var labels []string
	for _, node := range nodes {
		if node.Type != "footnote_reference" {
			continue
		}
		if label, ok := node.Attrs["label"].(string); ok && !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	return labels
}

// ParseInline converts one textblock's worth of inline markdown into inline
// nodes. Markdown that forms more than one paragraph, or holds text after its
// paragraph's last line, is ErrSchema.
func ParseInline(markdown string) ([]*Node, error) {
	source := []byte(LineFeeds(markdown))
	root := withLineStarts(inlineMarkdownParser, source, parser.NewContext())
	if root.ChildCount() > 1 {
		return nil, fmt.Errorf("%w: inline markdown forms %d paragraphs", ErrSchema, root.ChildCount())
	}
	if root.ChildCount() == 0 {
		return nil, nil
	}
	if dropped := textOutside(root.FirstChild(), source); dropped != "" {
		return nil, fmt.Errorf("%w: inline markdown holds text outside its paragraph, %q, which would be lost", ErrSchema, dropped)
	}
	paragraph, err := parseBlock(root.FirstChild(), source, nil)
	if err != nil {
		return nil, err
	}
	sortNodeMarks(paragraph)
	return paragraph.Children, nil
}

// BlockReadError is the parser's refusal of a document-level block's markdown, or nil when the
// parser reads it back.
func BlockReadError(block *Node) error {
	markdown, err := Render(readAlone(block))
	if err != nil {
		return err
	}
	_, err = Parse(markdown)
	return err
}

// BlockShapeError says what a document-level block's markdown reads back as when that is not
// blocks of the same kinds nested the same way - the first block that reads back as another - or
// is nil when it reads back so, or the parser's refusal of the markdown. What a textblock holds is
// not compared.
func BlockShapeError(block *Node) error {
	doc := readAlone(block)
	markdown, err := Render(doc)
	if err != nil {
		return err
	}
	back, err := Parse(markdown)
	if err != nil {
		return err
	}
	if want, got := shapeDifference(doc, back); want != "" {
		return fmt.Errorf("%s reads back as %s", want, got)
	}
	return nil
}

// shapeDifference names the first block of want that got holds as another kind, or holds where
// want has none, or lacks; both are empty when the two have the same shape. An empty paragraph is
// not written, so it is not expected back, except as its container's only child: a table cell or a
// footnote definition holding only an empty paragraph reads back holding one.
func shapeDifference(want, got *Node) (string, string) {
	if want.Type != got.Type {
		return blockName(want.Type), blockName(got.Type)
	}
	if isTextblock(want.Type) {
		return "", ""
	}
	only := len(want.Children) == 1
	written := make([]*Node, 0, len(want.Children))
	for _, child := range want.Children {
		if only || child.Type != "paragraph" || len(child.Children) != 0 {
			written = append(written, child)
		}
	}
	for index := 0; index < max(len(written), len(got.Children)); index++ {
		switch {
		case index >= len(written):
			return endOf(want.Type), blockName(got.Children[index].Type)
		case index >= len(got.Children):
			return blockName(written[index].Type), "nothing"
		}
		if w, g := shapeDifference(written[index], got.Children[index]); w != "" {
			return w, g
		}
	}
	return "", ""
}

// endOf names where a block's children end, as a reader names it.
func endOf(nodeType string) string {
	if nodeType == "doc" {
		return "the document's end"
	}
	return "the " + strings.ReplaceAll(nodeType, "_", " ") + "'s end"
}

// readAlone is the document a block is read in on its own. It follows a paragraph, as a block
// holding a match does, since at a document's start a `---` line opens front matter; front matter
// is read first, where it is written, and a footnote definition after a reference to it, since
// the parser reads a definition only when something refers to it.
func readAlone(block *Node) *Node {
	switch block.Type {
	case "frontmatter":
		return &Node{Type: "doc", Children: []*Node{block}}
	case "footnote_definition":
		reference := &Node{Type: "footnote_reference", Attrs: Attrs{"label": block.Attrs["label"]}}
		return &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{reference}}, block}}
	default:
		lead := &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: "Before."}}}
		return &Node{Type: "doc", Children: []*Node{lead, block}}
	}
}

// textOutside is the first line of text after the paragraph's last line. The inline parser knows
// only paragraphs and stops at the first line it cannot open one on - an indented code block
// after a blank line - so everything from there on is skipped rather than refused, and a caller
// who is told nothing loses it. A second paragraph is refused before this is asked.
func textOutside(paragraph ast.Node, source []byte) string {
	lines := paragraph.Lines()
	rest := strings.TrimSpace(string(source[lines.At(lines.Len()-1).Stop:]))
	if end := strings.IndexByte(rest, '\n'); end >= 0 {
		rest = strings.TrimSpace(rest[:end])
	}
	return rest
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
		return &Node{Type: "blockquote", Children: emptyParagraphFirst(children, false)}, nil
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
		return nil, ErrBlockHTML
	case *extensionast.Footnote:
		children, err := parseBlocks(current, source, footnotes)
		if err != nil {
			return nil, err
		}
		return &Node{Type: "footnote_definition", Attrs: Attrs{"label": string(current.Ref)}, Children: emptyParagraphFirst(children, false)}, nil
	case *typedDirective:
		return parseTypedDirective(current, source, footnotes)
	case *unsupportedDirective:
		return nil, fmt.Errorf("%w: %s", ErrSchema, current.Reason)
	case *extensionast.Table:
		return parseTable(current, source, footnotes)
	default:
		return nil, fmt.Errorf("%w: unsupported markdown block %s", ErrSchema, node.Kind())
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
		// Goldmark puts a footnote's backlink after a definition's last block when that block is
		// not a paragraph; it is the HTML renderer's decoration, not content.
		if _, ok := child.(*extensionast.FootnoteBacklink); ok {
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
	return &Node{Type: directive.Name, Attrs: attrs, Children: emptyParagraphFirst(children, false)}, nil
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
			return nil, fmt.Errorf("%w: list contains %s", ErrSchema, child.Kind())
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
	return &Node{Type: "list_item", Attrs: attrs, Children: emptyParagraphFirst(children, true)}, nil
}

// emptyParagraphFirst is a container's blocks as the browser editor's parser reads them: a
// container that holds nothing holds one empty paragraph, as does a list item that opens with
// another block (firstParagraph), ahead of it (`- # h`). The Proof schema needs both.
func emptyParagraphFirst(children []*Node, firstParagraph bool) []*Node {
	if len(children) == 0 || firstParagraph && children[0].Type != "paragraph" {
		return append([]*Node{{Type: "paragraph"}}, children...)
	}
	return children
}

func codeBlockText(lines *gmtext.Segments, source []byte) []*Node {
	value := strings.TrimRight(segmentsText(lines, source), "\r\n")
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
			return nil, fmt.Errorf("%w: unsupported table child %s", ErrSchema, child.Kind())
		}
	}
	// A table with no body row holds one empty row, as the browser editor's parser reads it.
	if len(children) == 1 {
		children = append(children, &Node{Type: "table_row"})
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
			return nil, fmt.Errorf("%w: unsupported table cell %s", ErrSchema, child.Kind())
		}
		content, err := parseTableCellInline(cell, source, nil, footnotes)
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

func parseTableCellInline(parent ast.Node, source []byte, initial []Mark, footnotes map[int]string) ([]*Node, error) {
	return parseInlineWithTableCellLinks(parent, source, initial, footnotes, true)
}

func parseInline(parent ast.Node, source []byte, initial []Mark, footnotes map[int]string) ([]*Node, error) {
	return parseInlineWithTableCellLinks(parent, source, initial, footnotes, false)
}

func parseInlineWithTableCellLinks(parent ast.Node, source []byte, initial []Mark, footnotes map[int]string, tableCell bool) ([]*Node, error) {
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
				// white-space: break-spaces would show a literal newline as a line break. An
				// image's alt text keeps its line ending, which that parser reads as written.
				if insideImage(current) {
					appendText(&children, lineEndingAfter(source, current.Segment.Stop), active)
				} else {
					appendText(&children, " ", active)
				}
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
			content, err := parseInlineWithTableCellLinks(current, source, next, footnotes, tableCell)
			if err != nil {
				return nil, err
			}
			appendInline(&children, content)
		case *ast.CodeSpan:
			if value, ok := multilineCodeSpanText(current, source); ok {
				appendText(&children, value, append(append([]Mark(nil), active...), Mark{Type: "inlineCode"}))
				continue
			}
			content, err := parseInlineWithTableCellLinks(current, source, append(active, Mark{Type: "inlineCode"}), footnotes, tableCell)
			if err != nil {
				return nil, err
			}
			appendInline(&children, content)
		case *ast.Link:
			href := string(current.Destination)
			if tableCell {
				href = string(util.UnescapePunctuations(current.Destination))
			}
			content, err := parseInlineWithTableCellLinks(current, source, append(active, Mark{Type: "link", Attrs: Attrs{"href": href, "title": titleOrNil(current.Title)}}), footnotes, tableCell)
			if err != nil {
				return nil, err
			}
			appendInline(&children, content)
		case *ast.AutoLink:
			appendText(&children, string(current.Label(source)), append(active, Mark{Type: "link", Attrs: Attrs{"href": string(current.URL(source)), "title": nil}}))
		case *extensionast.Strikethrough:
			content, err := parseInlineWithTableCellLinks(current, source, append(active, Mark{Type: "strike_through"}), footnotes, tableCell)
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
			value := segmentsText(current.Segments, source)
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
			return nil, fmt.Errorf("%w: unsupported markdown inline %s", ErrSchema, child.Kind())
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
