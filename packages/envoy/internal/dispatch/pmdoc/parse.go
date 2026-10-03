package pmdoc

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
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
		parser.WithBlockParsers(blockParsers()...),
		parser.WithInlineParsers(inlineParsers(emphasisParser{})...),
		parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...),
	)),
	goldmark.WithExtensions(linkify{}, lazyAwareTable{}, strikethrough{}, taskList{}, footnotes{}),
	goldmark.WithParserOptions(
		parser.WithBlockParsers(
			util.Prioritized(nestingGuard{BlockParser: &typedDirectiveParser{}}, 950),
			util.Prioritized(&unsupportedDirectiveParser{}, 900),
			util.Prioritized(lineRecordingParagraph{parser.NewParagraphParser()}, 999),
		),
	),
)}

// blockParsers is goldmark's default block parsers with its list parser held off an empty item
// that would interrupt a paragraph (emptyItemGuard), its list and quote parsers opening no
// container inside maxNesting blocks (nestingGuard), its list, list item, setext heading and
// fenced code parsers reading a tab in a line's indentation as the columns it spans, its indented
// code parser measuring a blank line's columns as a line of code's (codeColumns), its setext
// heading parser opening no heading under a table (underlineAfterTable), its list, list item and
// quote parsers recording where their containers' lines end (endsContainers), its list and quote
// parsers opening nothing at the document's level after an unclosed front-matter opener
// (frontmatterAttempt), and every one placing the block it opens where its text starts
// (tabIndented).
func blockParsers() []util.PrioritizedValue {
	parsers := parser.DefaultBlockParsers()
	listParser := reflect.TypeOf(parser.NewListParser())
	listItemParser := reflect.TypeOf(parser.NewListItemParser())
	setextParser := reflect.TypeOf(parser.NewSetextHeadingParser())
	fenceParser := reflect.TypeOf(parser.NewFencedCodeBlockParser())
	quoteParser := reflect.TypeOf(parser.NewBlockquoteParser())
	codeParser := reflect.TypeOf(parser.NewCodeBlockParser())
	for index, prioritized := range parsers {
		block := prioritized.Value.(parser.BlockParser)
		switch reflect.TypeOf(block) {
		case listParser:
			parsers[index].Value = frontmatterAttempt{tabIndented{endsContainers{emptyItemGuard{nestingGuard{BlockParser: block, withChild: true}}}, listMarkerStart}}
		case listItemParser:
			parsers[index].Value = tabIndented{endsContainers{listItemColumns{block}}, listMarkerStart}
		case quoteParser:
			parsers[index].Value = frontmatterAttempt{tabIndented{endsContainers{nestingGuard{BlockParser: block}}, nil}}
		case setextParser:
			parsers[index].Value = tabIndented{underlineAfterTable{block}, setextUnderline}
		case fenceParser:
			parsers[index].Value = tabIndented{fenceClosure{block}, fenceStart}
		case codeParser:
			parsers[index].Value = tabIndented{codeColumns{block}, nil}
		default:
			parsers[index].Value = tabIndented{block, nil}
		}
	}
	return parsers
}

// Parse converts markdown into the closed Proof ProseMirror tree. Its tables' short rows are
// padded only while they add at most maxTablePaddingCells cells, as a caller write's markdown.
func Parse(markdown string) (*Node, error) {
	return parseStamped(markdown, NewTablePaddingBudget())
}

// ParseRendering is Parse of markdown the renderer wrote, a read-back, whose short rows are the
// tree's: its padding spends a budget of its own (readBackPaddingBudget), not a caller write's.
func ParseRendering(markdown string) (*Node, error) {
	return parseStamped(markdown, readBackPaddingBudget())
}

func parseStamped(markdown string, budget *TablePaddingBudget) (*Node, error) {
	return parseStampedAt(markdown, 1, budget)
}

// parseStampedAt is parseStamped of markdown whose first line is line firstLine of what the caller
// wrote, so a refusal names the caller's line.
func parseStampedAt(markdown string, firstLine int, budget *TablePaddingBudget) (*Node, error) {
	doc, err := parseUnstamped(markdown, firstLine, true, budget)
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
// (RepeatedBlockID), and the repair keeps it for its first block, as settlement would. A document
// whose rendering, the markdown it is stored as, reads back otherwise is refused
// (RefuseMisreadDocument).
func ParseForWrite(markdown string, live *Node) (*Node, error) {
	doc, err := parseForWrite(markdown, live, true, NewTablePaddingBudget())
	if err != nil {
		return nil, err
	}
	if err := RefuseMisreadDocument(doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// ParseFragment parses markdown a caller writes into a document, rather than one that begins it,
// as ParseForWrite parses a fragment: a leading `---` line is a horizontal rule, as it is anywhere
// after a document's start. Written where the document begins (opensDocument), a closed
// front-matter block opening the markdown is front matter, as Parse reads it. Its tables' short
// rows are padded on budget, the caller write's, which its other fragments share.
func ParseFragment(markdown string, opensDocument bool, budget *TablePaddingBudget) (*Node, error) {
	return parseForWrite(markdown, nil, opensDocument, budget)
}

func parseForWrite(markdown string, live *Node, readFrontmatter bool, budget *TablePaddingBudget) (*Node, error) {
	doc, err := parseUnstamped(LineFeeds(markdown), 1, readFrontmatter, budget)
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
	return lineEndings.Replace(text)
}

// lineEndings writes a CR LF, and then a lone carriage return, as a line feed.
var lineEndings = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// LineFeedAttrs is attrs with LineFeeds applied to every string value, alone or in a list: the
// attributes a caller writes onto a typed block reach the document with line feeds alone too.
func LineFeedAttrs(attrs map[string]any) map[string]any {
	if attrs == nil {
		return nil
	}
	out := make(map[string]any, len(attrs))
	for name, value := range attrs {
		switch value := value.(type) {
		case string:
			out[name] = LineFeeds(value)
		case []string:
			items := make([]string, len(value))
			for index, item := range value {
				items[index] = LineFeeds(item)
			}
			out[name] = items
		case []any:
			items := make([]any, len(value))
			for index, item := range value {
				if text, ok := item.(string); ok {
					item = LineFeeds(text)
				}
				items[index] = item
			}
			out[name] = items
		default:
			out[name] = value
		}
	}
	return out
}

// parseUnstamped is Parse before EnsureBlockIDs: blocks keep the ids their markdown names, and a
// block that names none has none yet. Without readFrontmatter a closed front-matter block is read
// as the blocks its lines make. firstLine is the number markdown's first line has in what the
// caller wrote, which a refusal's line number counts from.
func parseUnstamped(markdown string, firstLine int, readFrontmatter bool, budget *TablePaddingBudget) (doc *Node, err error) {
	defer recoverPanic(&doc, &err, "reading markdown")
	source := []byte(markdown)
	var front *Node
	unclosedFrontmatter := false
	if readFrontmatter {
		var rest int
		front, rest, unclosedFrontmatter = parseFrontmatterBlock(source)
		firstLine += bytes.Count(source[:rest], []byte("\n"))
		source = source[rest:]
	}
	root, err := blockReader.parse(source, unclosedFrontmatter, budget)
	if nesting := (nestingError{}); errors.As(err, &nesting) {
		nesting.line += firstLine - 1
		return nil, nesting
	}
	if err != nil {
		return nil, err
	}
	if err := browserListSpacing(root, source); err != nil {
		return nil, err
	}
	doc, err = convert(root, source, firstLine, footnoteLabels(root))
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
// list marker, heading marker, fence, or directive is text, and the inline syntax is Parse's, its
// delimiter runs read by emphasis.
func inlineParserOptions(emphasis emphasisParser) []parser.Option {
	return []parser.Option{
		parser.WithBlockParsers(util.Prioritized(lineRecordingParagraph{parser.NewParagraphParser()}, 1000)),
		parser.WithInlineParsers(inlineParsers(emphasis)...),
		parser.WithInlineParsers(
			util.Prioritized(newStrikethroughGuard(), 500),
			util.Prioritized(newLinkifyGuard(), 999),
		),
	}
}

// inlineReader reads one textblock's inline markdown (inlineParserOptions): run alone, and with
// footnote definitions after it so that the references in it read as references
// (parseInlineWithDefinitions).
type inlineReader struct {
	run, withDefinitions parser.Parser
}

func newInlineReader(emphasis emphasisParser) inlineReader {
	return inlineReader{
		run:             parser.NewParser(inlineParserOptions(emphasis)...),
		withDefinitions: parser.NewParser(append(inlineParserOptions(emphasis), footnoteParserOptions()...)...),
	}
}

// inlineMarkdown reads inline markdown as Parse does; flankingOnlyMarkdown reads it with its
// delimiter runs judged by CommonMark's flanking rules alone (emphasisParser.flankingOnly).
var inlineMarkdown, flankingOnlyMarkdown = newInlineReader(emphasisParser{}), newInlineReader(emphasisParser{flankingOnly: true})

// parseInlineWithDefinitions reads one textblock's inline markdown with reader as ParseInline
// does, after a definition for each of labels, the footnote labels it refers to. It is the
// renderer's read-back of a run it wrote, where a reference is only a reference beside its
// definition.
func parseInlineWithDefinitions(markdown string, labels []string, reader inlineReader) ([]*Node, error) {
	if len(labels) == 0 {
		return readInline(markdown, reader.run)
	}
	var full strings.Builder
	full.WriteString(markdown)
	for _, label := range labels {
		full.WriteString("\n\n[^" + escapeFootnoteLabel(label) + "]: x")
	}
	source := []byte(full.String())
	root, err := parseSource(reader.withDefinitions, source, parser.NewContext())
	if err != nil {
		return nil, err
	}
	first, ok := root.FirstChild().(*ast.Paragraph)
	if !ok {
		return nil, fmt.Errorf("%w: inline markdown does not read as a paragraph", ErrSchema)
	}
	paragraph, err := convert(first, source, 1, footnoteLabels(root))
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
func ParseInline(markdown string) (nodes []*Node, err error) {
	return readInline(markdown, inlineMarkdown.run)
}

// readInline is ParseInline read with inline, one of an inlineReader's parsers.
func readInline(markdown string, inline parser.Parser) (nodes []*Node, err error) {
	defer recoverPanic(&nodes, &err, "reading inline markdown")
	source := []byte(LineFeeds(markdown))
	root, err := parseSource(inline, source, parser.NewContext())
	if err != nil {
		return nil, err
	}
	if root.ChildCount() > 1 {
		return nil, fmt.Errorf("%w: inline markdown forms %d paragraphs", ErrSchema, root.ChildCount())
	}
	if root.ChildCount() == 0 {
		return nil, nil
	}
	if dropped := textOutside(root.FirstChild(), source); dropped != "" {
		return nil, fmt.Errorf("%w: inline markdown holds text outside its paragraph, %q, which would be lost", ErrSchema, dropped)
	}
	paragraph, err := convert(root.FirstChild(), source, 1, nil)
	if err != nil {
		return nil, err
	}
	sortNodeMarks(paragraph)
	return paragraph.Children, nil
}

// BlockReadError is the parser's refusal of a document-level block's markdown, or nil when the
// parser reads it back. A table in it is written spanless (renderSpanless): the cells a span adds
// are empty, which the parser refuses nowhere, and the header still reaches every cell a row holds,
// so the verdict is the one its markdown written with the spans gets.
func BlockReadError(block *Node) error {
	markdown, err := renderSpanless(readAlone(block))
	if err != nil {
		return err
	}
	_, err = ParseRendering(markdown)
	return renderedNesting(block, err)
}

// BlockShapeError says what a document-level block's markdown reads back as when that is not
// blocks of the same kinds nested the same way - the first block that reads back as another - or
// is nil when it reads back so, or the parser's refusal of the markdown. What a textblock holds is
// not compared. Every verdict it gives is ErrSchema, as the parser's own refusals are. A table in
// it is written spanless (renderSpanless), so each of its rows reads back as wide as its widest row
// as it holds cells, where written with its spans a row would read back wider wherever a span
// covers a cell.
func BlockShapeError(block *Node) error {
	doc := readAlone(block)
	markdown, err := renderSpanless(doc)
	if err != nil {
		return err
	}
	back, err := ParseRendering(markdown)
	if err != nil {
		return renderedNesting(block, err)
	}
	if reason := readDifference(doc, back, shapeOnly); reason != "" {
		return readsBackAs(reason)
	}
	return nil
}

// readsBackAs is a block's markdown reading back as blocks of another shape (BlockShapeError):
// a refusal, so it is ErrSchema, worded as what reads back.
type readsBackAs string

func (reason readsBackAs) Error() string { return string(reason) }

func (readsBackAs) Unwrap() error { return ErrSchema }

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

// parseTableRows reads markdown as body rows of a table width cells wide, under a header it writes
// itself, and reports false when it is not table rows alone. Whether a row is too wide is decided
// by the parse, as for a whole document (markWideRows), so one row gets one answer on every write
// path; here that refusal is ErrTableWidth.
func parseTableRows(markdown string, width int, budget *TablePaddingBudget) ([]*Node, bool, error) {
	if width == 0 {
		return nil, false, nil
	}
	// Blank lines at either end are no rows, and nothing else is trimmed. goldmark's table
	// transformer trims a row with the Segment trims, whose set is IsSpace's (tab, line feed,
	// carriage return and space) and not the byte-slice util.TrimLeftSpace's, so a vertical tab, a
	// form feed or a space outside ASCII is a cell's text; and the first row's indentation decides
	// whether it is a row at all (markBlockRows), as on every other line.
	lines := strings.Split(markdown, "\n")
	blank := func(line string) bool { return strings.Trim(line, rowSpace) == "" }
	start, end := 0, len(markdown)
	for len(lines) > 0 && blank(lines[0]) {
		start += len(lines[0]) + 1
		lines = lines[1:]
	}
	for len(lines) > 0 && blank(lines[len(lines)-1]) {
		end -= len(lines[len(lines)-1]) + 1
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil, false, nil
	}
	for _, line := range lines {
		cells, ok := tableRowCells(line)
		if !ok || tableDelimiterRow(cells) {
			return nil, false, nil
		}
	}

	// The parse reads the caller's rows under two header lines the caller never wrote, after the
	// blank lines skipped above, so it numbers its lines from the caller's: a refusal names the line
	// in the insert's own markdown.
	header := syntheticTableHeader(width)
	firstLine := 1 + strings.Count(markdown[:start], "\n") - strings.Count(header, "\n")
	parsed, err := parseStampedAt(header+markdown[start:end]+"\n", firstLine, budget)
	// The parse opens with the header written above, so the table under it is the one document-level
	// table with nothing before it; any other is the fragment's own and keeps its own refusal.
	if wide := (wideRow{}); errors.As(err, &wide) && wide.firstBlock {
		return nil, true, fmt.Errorf("%w: got %d cells, table has %d", ErrTableWidth, wide.cells, wide.width)
	}
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

// rowSpace is what goldmark's table transformer trims from a row and its cells (util.IsSpace: tab,
// line feed, carriage return, space). A vertical tab, a form feed or a space outside ASCII is a
// cell's text to it, so the bare-row line check trims no more.
const rowSpace = " \t\n\r"

// tableRowCells splits a fragment line into cells, trimmed of rowSpace alone.
func tableRowCells(line string) ([]string, bool) {
	line = strings.Trim(line, rowSpace)
	if line == "" {
		return nil, false
	}

	cells := make([]string, 0, 2)
	var cell strings.Builder
	separatorCount := 0
	endsWithSeparator := false
	for index, char := range line {
		if char == '|' && !escapedByBackslashes(line, index) {
			cells = append(cells, cell.String())
			cell.Reset()
			separatorCount++
			endsWithSeparator = true
			continue
		}
		cell.WriteRune(char)
		endsWithSeparator = false
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
		value := strings.Trim(cell, rowSpace)
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

// convert is the one way into the tree's conversion: every block refusal first, in document order
// (refuseBlocks, which numbers source's lines from firstLine), and then the conversion, which reads
// what they leave on the tree (typedDirective.values).
func convert(node ast.Node, source []byte, firstLine int, footnotes map[int]string) (*Node, error) {
	if err := refuseBlocks(node, source, firstLine); err != nil {
		return nil, err
	}
	return parseBlock(node, source, footnotes, 0)
}

func parseBlock(node ast.Node, source []byte, footnotes map[int]string, depth int) (*Node, error) {
	if err := treeDepthError(depth); err != nil {
		return nil, err
	}
	switch current := node.(type) {
	case *ast.Document:
		children, err := parseBlocks(current, source, footnotes, depth)
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
		children, err := parseBlocks(current, source, footnotes, depth)
		if err != nil {
			return nil, err
		}
		return &Node{Type: "blockquote", Children: emptyParagraphFirst(children, false)}, nil
	case *ast.List:
		return parseList(current, source, footnotes, depth)
	case *ast.ListItem:
		return parseListItem(current, source, footnotes, depth)
	case *ast.FencedCodeBlock:
		language := string(current.Language(source))
		var value any
		if language != "" {
			value = language
		}
		return &Node{
			Type:     "code_block",
			Attrs:    Attrs{"language": value},
			Children: fencedCodeText(current, source),
		}, nil
	case *ast.CodeBlock:
		return &Node{
			Type:     "code_block",
			Attrs:    Attrs{"language": nil},
			Children: codeBlockText(current.Lines(), source),
		}, nil
	case *ast.ThematicBreak:
		return &Node{Type: "hr"}, nil
	case *extensionast.Footnote:
		children, err := parseBlocks(current, source, footnotes, depth)
		if err != nil {
			return nil, err
		}
		return &Node{Type: "footnote_definition", Attrs: Attrs{"label": unescapeMarkdownText(current.Ref)}, Children: emptyParagraphFirst(children, false)}, nil
	case *typedDirective:
		return parseTypedDirective(current, source, footnotes, depth)
	case *extensionast.Table:
		return parseTable(current, source, footnotes)
	default:
		// refuseBlocks refuses every block outside convertedBlocks before conversion starts.
		panic(fmt.Sprintf("conversion reached a %s block, which refuseBlocks passed and parseBlock does not convert (convertedBlocks)", node.Kind()))
	}
}

func parseBlocks(parent ast.Node, source []byte, footnotes map[int]string, depth int) ([]*Node, error) {
	children := make([]*Node, 0, parent.ChildCount())
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		parsed, err := parseBlock(child, source, footnotes, depth+1)
		if err != nil {
			return nil, err
		}

		children = append(children, parsed)
	}
	return children, nil
}

func parseTypedDirective(directive *typedDirective, source []byte, footnotes map[int]string, depth int) (*Node, error) {
	children, err := parseBlocks(directive, source, footnotes, depth)
	if err != nil {
		return nil, err
	}
	return &Node{Type: directive.Name, Attrs: directive.values, Children: emptyParagraphFirst(children, false)}, nil
}

// parseList reads a list's and its items' spread as browserListSpacing recorded them, or, where it
// recorded none, from goldmark's looseness: a loose list's items holding more than one block are
// spread, and the list is spread when none of them is.
func parseList(list *ast.List, source []byte, footnotes map[int]string, depth int) (*Node, error) {
	nodeType := "bullet_list"
	browserSpread, browser := list.Attribute(browserSpreadAttr)
	attrs := Attrs{"spread": !list.IsTight}
	if browser {
		attrs["spread"] = browserSpread
	}
	if list.IsOrdered() {
		nodeType = "ordered_list"
		attrs["order"] = list.Start
	}
	children := make([]*Node, 0, list.ChildCount())
	for child := list.FirstChild(); child != nil; child = child.NextSibling() {
		item, ok := child.(*ast.ListItem)
		if !ok {
			// refuseBlocks refuses every block outside convertedBlocks, and goldmark builds a list of items.
			panic(fmt.Sprintf("conversion reached a list holding a %s block, which parseList does not convert", child.Kind()))
		}
		parsed, err := parseListItem(item, source, footnotes, depth+1)
		if err != nil {
			return nil, err
		}
		if browser {
			parsed.Attrs["spread"], _ = item.Attribute(browserSpreadAttr)
		} else {
			parsed.Attrs["spread"] = !list.IsTight && item.ChildCount() > 1
			if parsed.Attrs["spread"] == true {
				attrs["spread"] = false
			}
		}
		children = append(children, parsed)
	}
	return &Node{Type: nodeType, Attrs: attrs, Children: children}, nil
}

func parseListItem(item *ast.ListItem, source []byte, footnotes map[int]string, depth int) (*Node, error) {
	attrs := Attrs{"label": "•", "listType": "bullet", "checked": nil, "spread": false}
	if firstBlock := item.FirstChild(); firstBlock != nil {
		if checkbox, ok := firstBlock.FirstChild().(*extensionast.TaskCheckBox); ok {
			attrs["checked"] = checkbox.IsChecked
		}
	}
	children, err := parseBlocks(item, source, footnotes, depth)
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
			// refuseBlocks refuses every block outside convertedBlocks, and goldmark builds a table of rows.
			panic(fmt.Sprintf("conversion reached a table holding a %s block, which parseTable does not convert", child.Kind()))
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
	columns := row.Parent().(*extensionast.Table).Alignments
	children := make([]*Node, 0, row.ChildCount())
	index := 0
	for child := row.FirstChild(); child != nil; child, index = child.NextSibling(), index+1 {
		cell, ok := child.(*extensionast.TableCell)
		if !ok {
			// refuseBlocks refuses every block outside convertedBlocks, and goldmark builds a row of cells.
			panic(fmt.Sprintf("conversion reached a table row holding a %s block, which parseTableRow does not convert", child.Kind()))
		}
		content, err := parseTableCellInline(cell, source, nil, footnotes)
		if err != nil {
			return nil, err
		}
		// A cell takes its column's alignment. goldmark gives each written cell its column's, but
		// the cells it pads a short row with none, and the renderer writes a padded cell as a cell
		// of its column, which reads back with the column's alignment. A column its delimiter row
		// aligns nowhere (`---`) has no alignment, as the browser editor reads it; goldmark names
		// that "none".
		var alignment any
		if column := columns[index]; column != extensionast.AlignNone {
			alignment = column.String()
		}
		children = append(children, &Node{
			Type:  cellType,
			Attrs: Attrs{"colspan": 1, "rowspan": 1, "colwidth": nil, "alignment": alignment},
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
	children, _, err := parseInlineMarks(parent, source, initial, footnotes, tableCell)
	return children, err
}

// parseInlineMarks reads parent's inline children under initial as the browser editor's parser
// reads marks, which a text holds as a set: a mark opened where the same mark is already open adds
// nothing, and its close ends that mark for the rest of the text around it, up to the node that
// opened it, whose own close would have ended it. So `*x *y* z*` is `x y` in emphasis and ` z`
// without, and `****a****` is strong once. ended is the marks of initial such a close inside parent
// ended, which the text after parent does not carry either.
func parseInlineMarks(parent ast.Node, source []byte, initial []Mark, footnotes map[int]string, tableCell bool) (children []*Node, ended []Mark, err error) {
	trimLineSuffixes(parent, source)
	active := append([]Mark(nil), initial...)
	// within reads a mark node's content under active and marks, then ends for the text after it
	// each open mark that a close inside it ended, or that it opened again and so closed.
	within := func(node ast.Node, marks ...Mark) ([]*Node, error) {
		next := append([]Mark(nil), active...)
		for _, mark := range marks {
			if !containsSameMark(active, mark) {
				next = append(next, mark)
			}
		}
		content, inner, err := parseInlineMarks(node, source, next, footnotes, tableCell)
		if err != nil {
			return nil, err
		}
		for _, mark := range marks {
			if containsSameMark(active, mark) {
				active = withoutSameMark(active, mark)
				ended = append(ended, mark)
			}
		}
		for _, mark := range inner {
			if !containsSameMark(marks, mark) && containsSameMark(active, mark) {
				active = withoutSameMark(active, mark)
				ended = append(ended, mark)
			}
		}
		return content, nil
	}
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		switch current := child.(type) {
		case *ast.Text:
			value := parseTextValue(current.Value(source), active)
			hard, soft := current.HardLineBreak(), current.SoftLineBreak()
			if hard || soft {
				if _, run, backslash := lineSuffix(current, source); backslash {
					value = strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "  ")
				} else {
					// The browser editor's parser reads the spaces and tabs a line ends with, which
					// trimLineSuffixes took off, as a hard break only where they are two spaces
					// or more and no tab; goldmark breaks hard at any two spaces ending the line.
					value = strings.TrimSuffix(value, "\n")
					hard = len(run) >= 2 && !strings.Contains(run, "\t")
					soft = !hard
				}
			}
			children = appendTextPiece(children, value, active)
			if hard {
				children = append(children, &Node{Type: "hardbreak", Attrs: Attrs{"isInline": false}})
			}
			if soft {
				// A soft break is a space, as CommonMark renders it; the browser editor's
				// white-space: break-spaces would show a literal newline as a line break. An
				// image's alt text keeps its line feed, which that parser reads as written.
				if insideImage(current) {
					children = appendTextPiece(children, "\n", active)
				} else {
					children = appendTextPiece(children, " ", active)
				}
			}
		case *ast.String:
			children = appendTextPiece(children, parseTextValue(current.Value, active), active)
		case *ast.Emphasis:
			var marks []Mark
			if current.Level >= 2 {
				marks = append(marks, Mark{Type: "strong", Attrs: Attrs{"marker": "*"}})
			}
			if current.Level%2 == 1 {
				marks = append(marks, Mark{Type: "emphasis", Attrs: Attrs{"marker": "*"}})
			}
			content, err := within(current, marks...)
			if err != nil {
				return nil, nil, err
			}
			children = append(children, content...)
		case *ast.CodeSpan:
			if value, ok := multilineCodeSpanText(current, source); ok {
				children = appendTextPiece(children, value, append(append([]Mark(nil), active...), Mark{Type: "inlineCode"}))
				continue
			}
			content, err := within(current, Mark{Type: "inlineCode"})
			if err != nil {
				return nil, nil, err
			}
			children = append(children, content...)
		case *ast.Link:
			href := string(current.Destination)
			if tableCell {
				href = string(util.UnescapePunctuations(current.Destination))
			}
			content, err := within(current, Mark{Type: "link", Attrs: Attrs{"href": href, "title": titleOrNil(current.Title)}})
			if err != nil {
				return nil, nil, err
			}
			children = append(children, content...)
		case *ast.AutoLink:
			children = appendTextPiece(children, string(current.Label(source)), append(active, Mark{Type: "link", Attrs: Attrs{"href": string(current.URL(source)), "title": nil}}))
		case *extensionast.Strikethrough:
			content, err := within(current, Mark{Type: "strike_through"})
			if err != nil {
				return nil, nil, err
			}
			children = append(children, content...)
		case *extensionast.TaskCheckBox:
			continue
		case *ast.Image:
			// An image is read without the marks around it, a link among them: the browser
			// editor's store (y-prosemirror) keeps a mark on text alone, so the editor stores a
			// linked image without its link as well (LEGION-365).
			image, err := parseImage(current, source, footnotes)
			if err != nil {
				return nil, nil, err
			}
			children = append(children, image)
		case *extensionast.FootnoteLink:
			definitionLabel, ok := footnotes[current.Index]
			if !ok {
				return nil, nil, fmt.Errorf("%w: footnote reference %d has no definition", ErrSchema, current.Index)
			}
			label := definitionLabel
			if written, set := current.AttributeString(string(referenceLabelAttr)); set {
				label = written.(string)
			}
			// The browser editor's parser decodes a label's escapes and character references, and
			// matches a reference to its definition by the label as written. Both labels are
			// stored decoded and written from that, so one matching only as written, before
			// decoding (`[^&AUML;]` beside `[^&auml;]: `), would not match once written.
			decoded := unescapeMarkdownText([]byte(label))
			if definition := unescapeMarkdownText([]byte(definitionLabel)); footnoteLabelKey(decoded) != footnoteLabelKey(definition) {
				return nil, nil, fmt.Errorf("%w: footnote reference %q matches its definition %q only as written, before their character references are decoded, which the document's markdown cannot keep", ErrSchema, decoded, definition)
			}
			children = append(children, &Node{Type: "footnote_reference", Attrs: Attrs{"label": decoded}})
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
			return nil, nil, fmt.Errorf("%w: unsupported markdown inline %s", ErrSchema, child.Kind())
		}
	}
	return joinTexts(children), ended, nil
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

// appendText adds value under marks to target, joined to target's last node where that is text
// under the same marks.
func appendText(target *[]*Node, value string, marks []Mark) {
	node := textNode(value, marks)
	if node == nil {
		return
	}
	if len(*target) > 0 {
		last := (*target)[len(*target)-1]
		if last.Type == "text" && marksEqual(last.Marks, node.Marks) {
			last.Text += value
			return
		}
	}
	*target = append(*target, node)
}

// appendTextPiece is nodes with value under marks as a text node after them, not yet joined to
// the text before it (joinTexts).
func appendTextPiece(nodes []*Node, value string, marks []Mark) []*Node {
	if node := textNode(value, marks); node != nil {
		return append(nodes, node)
	}
	return nodes
}

// textNode is value as a text node under a sorted copy of marks, or nil for no text.
func textNode(value string, marks []Mark) *Node {
	if value == "" {
		return nil
	}
	marks = append([]Mark(nil), marks...)
	sortMarks(marks)
	return &Node{Type: "text", Text: value, Marks: marks}
}

// joinTexts is nodes with each run of text nodes under the same marks written as one, as
// appendText joins them. A paragraph's lines are its pieces, so they are joined once, where adding
// each to the text before it copied that text again for every line.
func joinTexts(nodes []*Node) []*Node {
	out := nodes[:0]
	for index := 0; index < len(nodes); {
		node, end := nodes[index], index+1
		if node.Type == "text" {
			for end < len(nodes) && nodes[end].Type == "text" && marksEqual(nodes[end].Marks, node.Marks) {
				end++
			}
		}
		if end > index+1 {
			var text strings.Builder
			for _, piece := range nodes[index:end] {
				text.WriteString(piece.Text)
			}
			node = &Node{Type: "text", Text: text.String(), Marks: node.Marks}
		}
		out = append(out, node)
		index = end
	}
	return out
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
