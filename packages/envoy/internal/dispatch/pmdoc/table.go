package pmdoc

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// lazyAwareTable is goldmark's table extension with its paragraph transformer held off lazy
// continuation lines (lazyTableRows).
type lazyAwareTable struct{}

func (lazyAwareTable) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(
		parser.WithParagraphTransformers(util.Prioritized(lazyTableRows{table: extension.NewTableParagraphTransformer()}, 200)),
		parser.WithASTTransformers(util.Prioritized(extension.NewTableASTTransformer(), 0)),
	)
}

// lazyTableRows keeps a table's header and delimiter rows off lazy continuation lines. In GFM, and
// in the browser editor's parser, a line that continues a paragraph in a list item, a quote or a
// footnote definition without that container's prefix only continues the paragraph; goldmark's
// transformer reads a paragraph's lines as rows wherever they came from, so `- a\n|-|` became a
// table in the list item. A lazy line is one the reader began at its line start although the
// paragraph's first line began after a container's prefix.
type lazyTableRows struct{ table parser.ParagraphTransformer }

func (t lazyTableRows) Transform(node *ast.Paragraph, reader gmtext.Reader, pc parser.Context) {
	lazy := lazyLines(node.Lines(), reader.Source())
	if lazy == nil {
		t.table.Transform(node, reader, pc)
		return
	}
	// Read the paragraph as a table apart from the document to see which lines would be its
	// header and delimiter rows.
	trial := ast.NewParagraph()
	lines := gmtext.NewSegments()
	for index := 0; index < node.Lines().Len(); index++ {
		lines.Append(node.Lines().At(index))
	}
	trial.SetLines(lines)
	holder := ast.NewDocument()
	holder.AppendChild(holder, trial)
	t.table.Transform(trial, reader, parser.NewContext())
	for child := holder.FirstChild(); child != nil; child = child.NextSibling() {
		table, ok := child.(*extensionast.Table)
		if !ok || table.FirstChild() == nil {
			continue
		}
		for index := 0; index+1 < node.Lines().Len(); index++ {
			if node.Lines().At(index).Start == table.FirstChild().Pos() && (lazy[index] || lazy[index+1]) {
				return
			}
		}
	}
	t.table.Transform(node, reader, pc)
}

// lazyLines reports which of a paragraph's lines are lazy continuation lines, or nil when none is.
func lazyLines(lines *gmtext.Segments, source []byte) []bool {
	if lines.Len() < 2 {
		return nil
	}
	if first := lines.At(0); first.Start == lineStart(source, first.Start) {
		return nil
	}
	var lazy []bool
	for index := 1; index < lines.Len(); index++ {
		if line := lines.At(index); line.Start == lineStart(source, line.Start) {
			if lazy == nil {
				lazy = make([]bool, lines.Len())
			}
			lazy[index] = true
		}
	}
	return lazy
}
