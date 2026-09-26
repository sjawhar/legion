package pmdoc

import (
	"bytes"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
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
	m.Parser().AddOptions(
		parser.WithBlockParsers(util.Prioritized(footnoteDefinitionParser{extension.NewFootnoteBlockParser()}, 999)),
		parser.WithInlineParsers(util.Prioritized(extension.NewFootnoteParser(), 101)),
		parser.WithASTTransformers(util.Prioritized(extension.NewFootnoteASTTransformer(), 999)),
	)
}

type footnoteDefinitionParser struct{ parser.BlockParser }

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
// a line feed or a lone carriage return ends the line before (codeLineIndent). Recording as the
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

// codeLineIndent is the whitespace goldmark trimmed from the start of text, a code span's text on
// one of its later lines: what lies between the containers' prefix and the text, which the browser
// editor's parser reads as part of the code.
func codeLineIndent(text *ast.Text, source []byte) string {
	root := ast.Node(text)
	for root.Parent() != nil {
		root = root.Parent()
	}
	attribute, _ := root.Attribute(untrimmedLinesAttr)
	recorded, _ := attribute.(map[int]gmtext.Segment)
	line, ok := recorded[text.Segment.Start]
	if !ok {
		return ""
	}
	return strings.Repeat(" ", line.Padding) + string(source[line.Start:text.Segment.Start])
}

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
