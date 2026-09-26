package pmdoc

import (
	"bytes"
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
// forced line feed into the buffer it reads, past the segment's end, which overwrites source
// where a lone carriage return ends the line (lineEnds); this copies instead, and a line a lone
// carriage return ends already has its line ending.
func segmentsText(segments *gmtext.Segments, source []byte) string {
	var text strings.Builder
	for index := 0; index < segments.Len(); index++ {
		segment := segments.At(index)
		text.WriteString(strings.Repeat(" ", segment.Padding))
		value := source[segment.Start:segment.Stop]
		text.Write(value)
		if segment.ForceNewline && (len(value) == 0 || (value[len(value)-1] != '\n' && value[len(value)-1] != '\r')) {
			text.WriteByte('\n')
		}
	}
	return text.String()
}

// sourceKey holds the markdown as written, for a block parser to tell a line a lone carriage
// return began (lineEnds) from one a line feed began.
var sourceKey = parser.NewContextKey()

// parseLined parses lined, the source with its lone carriage returns as line feeds, with source
// at hand for the block parsers.
func (reader markdownReader) parseLined(lined, source []byte) ast.Node {
	context := parser.NewContext()
	context.Set(sourceKey, source)
	return withLineStarts(reader.md.Parser(), lined, context)
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
// goldmark's reference parser, and its transformer, which gathers the definitions at the end.
func footnoteParserOptions() []parser.Option {
	return []parser.Option{
		parser.WithBlockParsers(util.Prioritized(footnoteDefinitionParser{extension.NewFootnoteBlockParser()}, 999)),
		parser.WithInlineParsers(util.Prioritized(extension.NewFootnoteParser(), 101)),
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
	case rest[blank] == '\n' || rest[blank] == '\r':
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

func (p footnoteDefinitionParser) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	node, state := p.BlockParser.Open(parent, reader, pc)
	if node != nil && state&parser.HasChildren != 0 {
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
// trimmed line starts. The browser editor's parser keeps that whitespace in a code span, whether
// a line feed or a lone carriage return ends the line before (multilineCodeSpanText). Recording as the
// lines are taken keeps them whatever the paragraph becomes: a tight list item's text block, or a
// setext heading, whose paragraph is trimmed before any paragraph transformer sees it.
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

// withLineStarts parses lined with context and leaves the lines lineRecordingParagraph recorded on
// the document root.
func withLineStarts(p parser.Parser, lined []byte, context parser.Context) ast.Node {
	root := p.Parse(gmtext.NewReader(lined), parser.WithContext(context))
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
	closerAlone := stop > last.Segment.Start && isLineEnding(source[stop-1])
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
	head, tail := codePadding(text, true), codePadding(text, false)
	if head > 0 && tail > 0 && head+tail <= len(text) && strings.Trim(text, " \r\n") != "" {
		text = text[head : len(text)-tail]
	}
	return text, true
}

// codePadding is the length of the space or the line ending - a carriage return and line feed
// together, a line feed, or a lone carriage return - that a code span sheds at the start (atStart)
// or the end of text, or 0.
func codePadding(text string, atStart bool) int {
	for _, padding := range []string{"\r\n", "\n", "\r", " "} {
		if atStart && strings.HasPrefix(text, padding) || !atStart && strings.HasSuffix(text, padding) {
			return len(padding)
		}
	}
	return 0
}

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

func isLineEnding(char byte) bool { return char == '\n' || char == '\r' }

// isCodePadding reports whether char can be the space or line ending a code span sheds at each
// end.
func isCodePadding(char byte) bool { return char == ' ' || isLineEnding(char) }

// afterLoneCarriageReturn reports whether the line the reader is on began at a lone carriage
// return in the source as written.
func afterLoneCarriageReturn(reader gmtext.Reader, pc parser.Context) bool {
	source := pc.Get(sourceKey).([]byte)
	_, segment := reader.Position()
	start := lineStart(reader.Source(), segment.Start)
	return start > 0 && source[start-1] == '\r'
}

// lineStart is where the line holding position begins in source.
func lineStart(source []byte, position int) int {
	for position > 0 && source[position-1] != '\n' {
		position--
	}
	return position
}

// lineEnds is source with each carriage return that no line feed follows written as a line feed.
// CommonMark ends a line at a line feed, a carriage return, or the two together, and so does the
// browser editor's parser, while goldmark ends one only at a line feed. The two have the same
// length, so goldmark reads the structure from this and each text is read from source, keeping
// the carriage return as it was written.
func lineEnds(source []byte) []byte {
	if bytes.IndexByte(source, '\r') < 0 {
		return source
	}
	lined := bytes.Clone(source)
	for index := range lined {
		if loneCarriageReturn(source, index) {
			lined[index] = '\n'
		}
	}
	return lined
}

// loneCarriageReturn reports whether source holds a carriage return that no line feed follows at
// index.
func loneCarriageReturn(source []byte, index int) bool {
	return source[index] == '\r' && (index+1 == len(source) || source[index+1] != '\n')
}

// markdownLines is text split at every line ending markdown reads: a line feed, a lone carriage
// return, and the two together, whose carriage return stays at the end of its line.
func markdownLines(text string) []string {
	return strings.Split(string(lineEnds([]byte(text))), "\n")
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

// lineEndingAfter is the line ending that ends the line at stop, past the spaces and tabs before
// it: a carriage return and line feed, a lone carriage return, or a line feed.
func lineEndingAfter(source []byte, stop int) string {
	for stop < len(source) && (source[stop] == ' ' || source[stop] == '\t') {
		stop++
	}
	switch {
	case stop+1 < len(source) && source[stop] == '\r' && source[stop+1] == '\n':
		return "\r\n"
	case stop < len(source) && source[stop] == '\r':
		return "\r"
	}
	return "\n"
}

// parseFrontmatterBlock reads the front matter a document opens with in lined (lineEnds), as the
// browser editor's parser does: a first line that is `---` and any spaces or tabs opens it, and
// the first later line that is the same closes it. The node's text is the lines between, joined
// with line feeds between plain `---` fences, as that parser stores it. rest is where the
// document after the closing line begins; a document with no closed front matter has none, and
// rest is 0.
func parseFrontmatterBlock(lined []byte) (front *Node, rest int) {
	var content []string
	for start, index := 0, 0; start < len(lined); index++ {
		end := len(lined)
		if next := bytes.IndexByte(lined[start:], '\n'); next >= 0 {
			end = start + next + 1
		}
		line := strings.TrimRight(string(bytes.TrimSuffix(bytes.TrimSuffix(lined[start:end], []byte("\n")), []byte("\r"))), " \t")
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
			content = append(content, string(bytes.TrimSuffix(bytes.TrimSuffix(lined[start:end], []byte("\n")), []byte("\r"))))
		}
		start = end
	}
	return nil, 0
}
