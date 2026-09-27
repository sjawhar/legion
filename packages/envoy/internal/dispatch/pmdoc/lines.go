package pmdoc

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// segmentsText is the text segments cover in source. Goldmark's own Segments.Value appends a
// forced line feed into the buffer it reads, past the segment's end; this copies instead.
func segmentsText(segments *gmtext.Segments, source []byte) string {
	var text strings.Builder
	for index := 0; index < segments.Len(); index++ {
		segment := segments.At(index)
		text.WriteString(strings.Repeat(" ", segment.Padding))
		value := source[segment.Start:segment.Stop]
		text.Write(value)
		if segment.ForceNewline && (len(value) == 0 || value[len(value)-1] != '\n') {
			text.WriteByte('\n')
		}
	}
	return text.String()
}

// parse parses source.
func (reader markdownReader) parse(source []byte) ast.Node {
	return withLineStarts(reader.md.Parser(), source, parser.NewContext())
}

// bareMarkerLine is a line holding only a list marker.
var bareMarkerLine = regexp.MustCompile(`^ {0,3}(?:[-+*]|[0-9]{1,9}[.)])[ \t]*$`)

// emptyItemGuard is a list parser that opens no empty list item - a marker with nothing after it
// on its line - that would interrupt a paragraph, as the browser editor's parser reads it. Goldmark
// refuses one only while the paragraph is the block last opened; that parser also refuses one
// opening a container on a line that already interrupted the paragraph, so `- a\n  - -` is an item
// holding the text `-`, and `- a\n  > -` a quote holding it.
type emptyItemGuard struct{ parser.BlockParser }

func (p emptyItemGuard) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, segment := reader.PeekLine()
	bare := bareMarkerLine.Match(bytes.TrimRight(line, "\n"))
	if bare && parent.ChildCount() == 0 && interruptsParagraph(parent, reader.Source(), lineStart(reader.Source(), segment.Start)) {
		return nil, parser.NoChildren
	}
	// After an indented code block, blank lines between or not, the browser editor's parser opens
	// no list whose first item could not interrupt a paragraph: an empty one, or an ordered one
	// numbered from other than one.
	if _, code := parent.LastChild().(*ast.CodeBlock); code && (bare || orderedFromOtherThanOne(line)) {
		return nil, parser.NoChildren
	}
	return p.BlockParser.Open(parent, reader, pc)
}

// orderedMarkerStart is an ordered list marker opening a line, its number captured.
var orderedMarkerStart = regexp.MustCompile(`^ {0,3}([0-9]{1,9})[.)](?:[ \t]|\n|$)`)

// orderedFromOtherThanOne reports whether line opens an ordered list item numbered from other than
// one.
func orderedFromOtherThanOne(line []byte) bool {
	match := orderedMarkerStart.FindSubmatch(line)
	return match != nil && strings.TrimLeft(string(match[1]), "0") != "1"
}

// tabIndented is a block parser reading a line whose indentation holds a tab as CommonMark and the
// browser editor's parser do, when what follows the indentation matches opens: the tab spans the
// columns to the next multiple of four, so after a quote's `> ` it spans two. Goldmark measures such
// a line's indentation as if it began the line, or takes a list marker or a setext underline only
// after spaces, and read `> \t- a` and `> a\n> \t===` as paragraph text; and it counts a fence's
// indentation in bytes, a tab as one, so it kept a column of each code line's indentation that the
// fence's own removes (`> \t```` over `> \tc` read ` c`). Before the parser looks at such a line,
// its indentation is taken as the columns it spans, as padding. With no opens, the parser reads
// the line as goldmark's does, and only the block's position is corrected (Open).
type tabIndented struct {
	parser.BlockParser
	opens *regexp.Regexp
}

// Open also leaves goldmark the block's position where its text starts: goldmark places an opened
// block at the line's start plus the block offset, which counts the columns a tab spans as bytes,
// so a block after a tab stood a character or two past its text, or on the next line.
func (p tabIndented) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	_, start := reader.PeekLine()
	if p.opens != nil {
		expandTabIndentation(reader, p.opens)
	}
	line, segment := reader.PeekLine()
	text := util.FirstNonSpacePosition(line)
	if text >= 0 {
		pc.SetBlockOffset(text)
	}
	node, state := p.BlockParser.Open(parent, reader, pc)
	if node != nil && text >= segment.Padding {
		pc.SetBlockOffset(segment.Start + text - segment.Padding - start.Start)
	}
	return node, state
}

func (p tabIndented) Continue(node ast.Node, reader gmtext.Reader, pc parser.Context) parser.State {
	if p.opens != nil {
		expandTabIndentation(reader, p.opens)
	}
	return p.BlockParser.Continue(node, reader, pc)
}

// footnoteReferenceParser is goldmark's footnote reference parser, with a reference whose label
// matches a definition's only up to case resolved to that definition, as the browser editor's
// parser resolves it: goldmark matches a label byte for byte, and read `[^A]` as text beside
// `[^a]: `. Such a reference carries its label as written (referenceLabelAttr), which the browser
// editor keeps on it.
type footnoteReferenceParser struct{ parser.InlineParser }

// referenceLabelAttr is the label a reference resolved by footnoteReferenceParser is written with.
var referenceLabelAttr = []byte("pmdoc-reference-label")

func (p footnoteReferenceParser) Parse(parent ast.Node, block gmtext.Reader, pc parser.Context) ast.Node {
	row, position := block.Position()
	if node := p.InlineParser.Parse(parent, block, pc); node != nil {
		return node
	}
	block.SetPosition(row, position)
	line, segment := block.PeekLine()
	start := 1
	if len(line) > 0 && line[0] == '!' {
		start++
	}
	if start+1 >= len(line) || line[start] != '^' {
		return nil
	}
	closure := util.FindClosure(line[start+1:], '[', ']', false, false) //nolint:staticcheck
	if closure < 0 {
		return nil
	}
	label := line[start+1 : start+1+closure]
	slots, _ := pc.Get(footnoteSlotsKey).([]*footnoteSlot)
	for _, slot := range slots {
		definition, ok := slot.definition.(*extensionast.Footnote)
		if !ok || !bytes.EqualFold(definition.Ref, label) {
			continue
		}
		list, ok := definition.Parent().(*extensionast.FootnoteList)
		if !ok {
			continue
		}
		if definition.Index < 0 {
			list.Count++
			definition.Index = list.Count
		}
		block.Advance(start + 1 + closure + 1)
		link := extensionast.NewFootnoteLink(definition.Index)
		link.SetAttribute(referenceLabelAttr, string(label))
		if line[0] == '!' {
			parent.AppendChild(parent, ast.NewTextSegment(gmtext.NewSegment(segment.Start, segment.Start+1)))
		}
		return link
	}
	return nil
}

// setPadding sets reader's padding and drops the line it has peeked, which goldmark's
// SetPadding keeps: a zero advance drops it.
func setPadding(reader gmtext.Reader, padding int) {
	reader.SetPadding(padding)
	reader.Advance(0)
}

var (
	// fenceStart is a code fence's opening run of backticks or tildes.
	fenceStart = regexp.MustCompile("^(?:```|~~~)")
	// listMarkerStart is a list marker opening a line's text: a bullet, or an ordered item's number
	// and delimiter, followed by a space, a tab or the line's end.
	listMarkerStart = regexp.MustCompile(`^(?:[-+*]|[0-9]{1,9}[.)])(?:[ \t]|\n|$)`)
	// setextUnderline is a setext heading's underline.
	setextUnderline = regexp.MustCompile(`^(?:=+|-+)[ \t]*(?:\n|$)`)
)

// expandTabIndentation takes the indentation of reader's line as the columns it spans, as padding,
// when it holds a tab, spans fewer than four columns, and what follows it matches opens; the line
// reads the same, with spaces where a tab stood.
func expandTabIndentation(reader gmtext.Reader, opens *regexp.Regexp) {
	line, _ := reader.PeekLine()
	end := 0
	for end < len(line) && (line[end] == ' ' || line[end] == '\t') {
		end++
	}
	if bytes.IndexByte(line[:end], '\t') < 0 || !opens.Match(line[end:]) {
		return
	}
	if width, _ := util.IndentWidth(line, reader.LineOffset()); width < 4 {
		reader.AdvanceAndSetPadding(end, width)
	}
}

// tabExpandedLines is lines with each one's indentation, where it holds a tab, taken as the columns
// it spans on its source line, as padding: goldmark's table transformer measures a row's indentation
// as if the row began its line, so `> \t| a |` read as paragraph text.
func tabExpandedLines(lines *gmtext.Segments, source []byte) *gmtext.Segments {
	expanded := gmtext.NewSegments()
	for index := 0; index < lines.Len(); index++ {
		line := lines.At(index)
		end := line.Start
		for end < line.Stop && (source[end] == ' ' || source[end] == '\t') {
			end++
		}
		if line.Padding == 0 && bytes.IndexByte(source[line.Start:end], '\t') >= 0 {
			from := lineStart(source, line.Start)
			width, _ := util.IndentWidth(source[line.Start:end], columnOf(source[from:line.Start]))
			line = gmtext.Segment{Start: end, Stop: line.Stop, Padding: width, ForceNewline: line.ForceNewline}
		}
		expanded.Append(line)
	}
	return expanded
}

// columnOf is the column text, the start of a line, ends at, a tab advancing to the next multiple
// of four.
func columnOf(text []byte) int {
	column := 0
	for _, char := range text {
		if char == '\t' {
			column += 4 - column%4
		} else {
			column++
		}
	}
	return column
}

// interruptsParagraph reports whether container, opened on the line starting at start with the
// containers above it that it opens first, follows a paragraph whose last line is the one before.
func interruptsParagraph(container ast.Node, source []byte, start int) bool {
	for node := container; node != nil && node.Kind() != ast.KindDocument; node = node.Parent() {
		previous := node.PreviousSibling()
		if previous == nil {
			continue
		}
		paragraph, ok := previous.(*ast.Paragraph)
		if !ok || paragraph.Lines().Len() == 0 {
			return false
		}
		last := paragraph.Lines().At(paragraph.Lines().Len() - 1)
		return bytes.Count(source[last.Start:start], []byte("\n")) <= 1
	}
	return false
}

// footnotes is goldmark's footnote extension with its definition parser taking every space and
// tab after a definition's `]:` as part of its prefix, as the browser editor's parser does, so the
// definition's first block starts at the column its later lines are measured from: goldmark left
// them to that block, which read a fence opening the definition as indented by one (and dropped a
// space from each code line) and a list's marker one column in (which put its item's later blocks
// outside it).
type footnotes struct{}

func (footnotes) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(footnoteParserOptions()...)
}

// footnoteParserOptions registers footnotes: the definition parser (footnoteDefinitionParser),
// goldmark's reference parser (footnoteReferenceParser), and its transformer, which numbers the
// references and definitions.
func footnoteParserOptions() []parser.Option {
	return []parser.Option{
		parser.WithBlockParsers(util.Prioritized(footnoteDefinitionParser{extension.NewFootnoteBlockParser()}, 999)),
		parser.WithInlineParsers(util.Prioritized(footnoteReferenceParser{extension.NewFootnoteParser()}, 101)),
		parser.WithASTTransformers(util.Prioritized(extension.NewFootnoteASTTransformer(), 999)),
	}
}

type footnoteDefinitionParser struct{ parser.BlockParser }

// taskList reads a task list item's marker as the browser editor's parser does: `[ ]`, `[x]` or
// `[X]` opening a list item's first paragraph, followed by a space or a tab and then more text on
// the line, or by a line ending, after any spaces and tabs, that the paragraph continues past. The
// marker takes the one space or tab after it, or the line ending when the line holds nothing more
// but a hard break's spaces, which stay a hard break. Goldmark's reads a marker with anything after it, and takes every space
// after it, so it read a link opening a list item (`- [x](https://…)`) as a checked task.
type taskList struct{}

func (taskList) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithInlineParsers(util.Prioritized(taskMarkerParser{}, 0)))
}

type taskMarkerParser struct{}

func (taskMarkerParser) Trigger() []byte { return []byte{'['} }

func (taskMarkerParser) Parse(parent ast.Node, block gmtext.Reader, _ parser.Context) ast.Node {
	if _, ok := parent.Parent().(*ast.ListItem); !ok || parent.Parent().FirstChild() != parent || parent.HasChildren() {
		return nil
	}
	line, _ := block.PeekLine()
	if len(line) < 4 || !strings.ContainsRune(" \txX", rune(line[1])) || line[2] != ']' {
		return nil
	}
	rest := line[3:]
	blank := len(rest) - len(bytes.TrimLeft(rest, " \t"))
	width := 4
	switch {
	case blank > 0 && !util.IsBlank(rest):
	case rest[blank] == '\n':
		row, segment := block.Position()
		block.AdvanceLine()
		next, _ := block.PeekLine()
		block.SetPosition(row, segment)
		if next == nil {
			return nil
		}
		width = len(line)
		if spaces := len(rest) - len(bytes.TrimLeft(rest, " ")); spaces >= 2 {
			width = 3
		}
	default:
		return nil
	}
	block.Advance(width)
	return extensionast.NewTaskCheckBox(line[1] == 'x' || line[1] == 'X')
}

// footnoteSlot stands where a footnote definition is written while the document parses:
// goldmark's footnote extension moves each definition it closes into the list it gathers at the
// document's end, and its transformer drops the ones nothing refers to. definitionsInPlace puts
// each back in its slot, where the browser editor's parser keeps it.
type footnoteSlot struct {
	ast.BaseBlock
	definition ast.Node
}

var (
	kindFootnoteSlot = ast.NewNodeKind("FootnoteSlot")
	footnoteSlotsKey = parser.NewContextKey()
)

func (s *footnoteSlot) Kind() ast.NodeKind { return kindFootnoteSlot }

func (s *footnoteSlot) Dump(source []byte, level int) { ast.DumpHelper(s, source, level, nil, nil) }

// Close leaves a slot where the definition is written and hands goldmark the definition from the
// document's level, where goldmark puts the list it gathers definitions in when it closes the first
// one: a list put inside a definition would leave the document with the definition that holds it,
// and goldmark reads no text in blocks outside the document.
func (p footnoteDefinitionParser) Close(node ast.Node, reader gmtext.Reader, pc parser.Context) {
	slot := &footnoteSlot{definition: node}
	node.Parent().InsertBefore(node.Parent(), node, slot)
	slots, _ := pc.Get(footnoteSlotsKey).([]*footnoteSlot)
	pc.Set(footnoteSlotsKey, append(slots, slot))
	root := node.Parent()
	for root.Parent() != nil {
		root = root.Parent()
	}
	root.AppendChild(root, node)
	p.BlockParser.Close(node, reader, pc)
}

// definitionsInPlace puts each footnote definition back where it is written, in place of its slot,
// and removes the list goldmark gathered them in.
func definitionsInPlace(root ast.Node, context parser.Context) {
	slots, _ := context.Get(footnoteSlotsKey).([]*footnoteSlot)
	for _, slot := range slots {
		if parent := slot.definition.Parent(); parent != nil {
			parent.RemoveChild(parent, slot.definition)
		}
		slot.Parent().ReplaceChild(slot.Parent(), slot, slot.definition)
	}
	if list, ok := root.LastChild().(*extensionast.FootnoteList); ok {
		root.RemoveChild(root, list)
	}
}

// Open reads the definition's opener without the padding a tab split by the containers' prefix
// leaves ahead of it: goldmark's parser measures where the definition's text starts from the line
// without that padding, so it took the label's first characters as the text (`]: def`).
func (p footnoteDefinitionParser) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	_, segment := reader.PeekLine()
	offset := pc.BlockOffset()
	padded := segment.Padding > 0 && offset >= segment.Padding
	if padded {
		setPadding(reader, 0)
		pc.SetBlockOffset(offset - segment.Padding)
	}
	node, state := p.BlockParser.Open(parent, reader, pc)
	if node == nil {
		if padded {
			setPadding(reader, segment.Padding)
			pc.SetBlockOffset(offset)
		}
		return node, state
	}
	if state&parser.HasChildren != 0 {
		line, _ := reader.PeekLine()
		skip := 0
		for skip < len(line) && (line[skip] == ' ' || line[skip] == '\t') {
			skip++
		}
		if skip < len(line) && !util.IsBlank(line[skip:]) {
			reader.Advance(skip)
		}
	}
	return node, state
}

// lineRecordingParagraph is goldmark's paragraph parser, recording each line it takes as the
// containers left it, before the paragraph trims its leading whitespace, keyed by where the
// trimmed line starts. The browser editor's parser keeps that whitespace in a code span
// (multilineCodeSpanText). Recording as the lines are taken keeps them whatever the paragraph
// becomes: a tight list item's text block, or a setext heading, whose paragraph is trimmed before
// any paragraph transformer sees it.
type lineRecordingParagraph struct{ parser.BlockParser }

// untrimmedLinesKey holds the recorded lines while a document parses, and untrimmedLinesAttr on
// the parsed document root afterwards (withLineStarts).
var (
	untrimmedLinesKey  = parser.NewContextKey()
	untrimmedLinesAttr = []byte("pmdoc-untrimmed-lines")
)

func (p lineRecordingParagraph) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	_, segment := reader.PeekLine()
	node, state := p.BlockParser.Open(parent, reader, pc)
	if node != nil {
		recordLine(segment, reader.Source(), pc)
	}
	return node, state
}

func (p lineRecordingParagraph) Continue(node ast.Node, reader gmtext.Reader, pc parser.Context) parser.State {
	_, segment := reader.PeekLine()
	state := p.BlockParser.Continue(node, reader, pc)
	if state != parser.Close {
		recordLine(segment, reader.Source(), pc)
	}
	return state
}

func recordLine(line gmtext.Segment, source []byte, pc parser.Context) {
	recorded, _ := pc.Get(untrimmedLinesKey).(map[int]gmtext.Segment)
	if recorded == nil {
		recorded = make(map[int]gmtext.Segment)
		pc.Set(untrimmedLinesKey, recorded)
	}
	recorded[line.TrimLeftSpace(source).Start] = line
}

// withLineStarts parses source with context, with each footnote definition where it is written
// (definitionsInPlace), and leaves the lines lineRecordingParagraph recorded on the document root.
func withLineStarts(p parser.Parser, source []byte, context parser.Context) ast.Node {
	root := p.Parse(gmtext.NewReader(source), parser.WithContext(context))
	definitionsInPlace(root, context)
	if recorded := context.Get(untrimmedLinesKey); recorded != nil {
		root.SetAttribute(untrimmedLinesAttr, recorded)
	}
	return root
}

// multilineCodeSpanText is the text of a code span that runs over more than one line, as the
// browser editor's parser reads it: each later line from just after the containers' prefix, where
// goldmark's paragraph trimmed the whitespace before it - a line holding only whitespace before
// the closer included, for which goldmark keeps no text at all - and then one space or line ending
// taken from each end when both ends hold one, after the whole span is put together. ok is false
// for a span on one line, whose goldmark reading stands.
func multilineCodeSpanText(span *ast.CodeSpan, source []byte) (text string, ok bool) {
	first, _ := span.FirstChild().(*ast.Text)
	last, _ := span.LastChild().(*ast.Text)
	if first == nil || last == nil {
		return "", false
	}
	// Goldmark takes one character off each end when both are whitespace; put them back.
	trimmed := first.Segment.Start > 0 && source[first.Segment.Start-1] != '`'
	stop := last.Segment.Stop
	if trimmed {
		stop++
	}
	closerAlone := stop > last.Segment.Start && source[stop-1] == '\n'
	if first == last && !closerAlone {
		return "", false
	}
	var content strings.Builder
	for child := span.FirstChild(); child != nil; child = child.NextSibling() {
		line := child.(*ast.Text).Segment
		from, to := line.Start, line.Stop
		if child == span.FirstChild() && trimmed {
			from--
		} else if child != span.FirstChild() {
			content.WriteString(untrimmedIndent(span, line.Start, source))
		}
		if child == span.LastChild() && trimmed {
			to++
		}
		content.Write(source[from:to])
	}
	if closerAlone {
		if next := bytes.IndexByte(source[stop:], '`'); next >= 0 {
			content.WriteString(untrimmedIndent(span, stop+next, source))
		}
	}
	text = content.String()
	if codeSpanPadded(text) {
		text = text[1 : len(text)-1]
	}
	return text, true
}

// codeSpanPadded reports whether a code span's text is one the parser takes a character off each
// end of: both ends a space or a line feed, and something else between.
func codeSpanPadded(text string) bool {
	return len(text) >= 2 && isCodePadding(text[0]) && isCodePadding(text[len(text)-1]) && strings.Trim(text, " \n") != ""
}

// isCodePadding reports whether char is a space or a line feed, what a code span sheds at each end.
func isCodePadding(char byte) bool { return char == ' ' || char == '\n' }

// untrimmedIndent is the whitespace goldmark's paragraph trimmed from the line whose text now
// starts at start: what lies between the containers' prefix and it (lineRecordingParagraph).
func untrimmedIndent(node ast.Node, start int, source []byte) string {
	root := node
	for root.Parent() != nil {
		root = root.Parent()
	}
	attribute, _ := root.Attribute(untrimmedLinesAttr)
	recorded, _ := attribute.(map[int]gmtext.Segment)
	line, ok := recorded[start]
	if !ok {
		return ""
	}
	return strings.Repeat(" ", line.Padding) + string(source[line.Start:start])
}

// lineStart is where the line holding position begins in source.
func lineStart(source []byte, position int) int {
	for position > 0 && source[position-1] != '\n' {
		position--
	}
	return position
}

// insideImage reports whether node is part of an image's alt text.
func insideImage(node ast.Node) bool {
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		if _, ok := parent.(*ast.Image); ok {
			return true
		}
	}
	return false
}

// parseFrontmatterBlock reads the front matter a document opens with in source, as the
// browser editor's parser does: a first line that is `---` and any spaces or tabs opens it, and
// the first later line that is the same closes it. The node's text is the lines between, joined
// with line feeds between plain `---` fences, as that parser stores it. rest is where the
// document after the closing line begins; a document with no closed front matter has none, and
// rest is 0.
func parseFrontmatterBlock(source []byte) (front *Node, rest int) {
	var content []string
	for start, index := 0, 0; start < len(source); index++ {
		end := len(source)
		if next := bytes.IndexByte(source[start:], '\n'); next >= 0 {
			end = start + next + 1
		}
		line := strings.TrimRight(string(bytes.TrimSuffix(source[start:end], []byte("\n"))), " \t")
		switch {
		case index == 0 && line != "---":
			return nil, 0
		case index > 0 && line == "---":
			text := "---\n---"
			if joined := strings.Join(content, "\n"); joined != "" {
				text = "---\n" + joined + "\n---"
			}
			return &Node{Type: "frontmatter", Children: []*Node{{Type: "text", Text: text}}}, end
		case index > 0:
			content = append(content, string(bytes.TrimSuffix(source[start:end], []byte("\n"))))
		}
		start = end
	}
	return nil, 0
}
