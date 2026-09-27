package pmdoc

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// footnoteReferenceParser is goldmark's footnote reference parser, with a reference whose label
// matches a definition's only up to case (footnoteLabelKey) resolved to that definition, as the
// browser editor's parser resolves it: goldmark matches a label byte for byte, and read `[^A]` as
// text beside `[^a]: `. Such a reference carries its label as written (referenceLabelAttr), which
// the browser editor keeps on it.
type footnoteReferenceParser struct{ parser.InlineParser }

// footnoteLabelKey is a footnote label as the browser editor's parser compares it: with full case
// mapping, lowered and then uppercased, so `ß` matches `SS` and `İ` matches `i̇`, which simple case
// folding keeps apart.
func footnoteLabelKey(label string) string {
	return cases.Upper(language.Und).String(cases.Lower(language.Und).String(label))
}

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
	definition := footnoteDefinitionsByKey(pc)[footnoteLabelKey(string(label))]
	if definition == nil {
		return nil
	}
	if definition.Index < 0 {
		list := definition.Parent().(*extensionast.FootnoteList)
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

// footnoteKeysKey holds a parse's footnoteDefinitionsByKey.
var footnoteKeysKey = parser.NewContextKey()

// footnoteDefinitionsByKey is each footnote definition in goldmark's list by its label's key
// (footnoteLabelKey), the first in the document where two share one. It is built once per parse,
// on the first reference goldmark's parser leaves unresolved: inline text is parsed after every
// block has closed, so each definition has its slot by then.
func footnoteDefinitionsByKey(pc parser.Context) map[string]*extensionast.Footnote {
	if byKey, ok := pc.Get(footnoteKeysKey).(map[string]*extensionast.Footnote); ok {
		return byKey
	}
	slots, _ := pc.Get(footnoteSlotsKey).([]*footnoteSlot)
	byKey := make(map[string]*extensionast.Footnote, len(slots))
	for _, slot := range slots {
		definition, ok := slot.definition.(*extensionast.Footnote)
		if !ok {
			continue
		}
		if _, listed := definition.Parent().(*extensionast.FootnoteList); !listed {
			continue
		}
		key := footnoteLabelKey(string(definition.Ref))
		if _, first := byKey[key]; !first {
			byKey[key] = definition
		}
	}
	pc.Set(footnoteKeysKey, byKey)
	return byKey
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

// footnoteParserOptions registers footnotes: the definition parser (footnoteDefinitionParser) and
// goldmark's reference parser (footnoteReferenceParser), which numbers the definitions it refers
// to. Goldmark's footnote transformer is left out: it only orders the definitions, drops those
// nothing refers to and appends backlinks, and the parser keeps every definition where it is
// written (definitionsInPlace) and writes no backlink.
func footnoteParserOptions() []parser.Option {
	return []parser.Option{
		parser.WithBlockParsers(util.Prioritized(endsContainers{footnoteDefinitionParser{extension.NewFootnoteBlockParser()}}, 999)),
		parser.WithInlineParsers(util.Prioritized(footnoteReferenceParser{extension.NewFootnoteParser()}, 101)),
	}
}

type footnoteDefinitionParser struct{ parser.BlockParser }

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
// and goldmark reads no text in blocks outside the document. That list goes ahead of the blocks
// written, where no check of the block before another (interruptsOpenBlock) meets it while the
// document parses: goldmark put it at the document's end, between the blocks written before and
// after the definition closed.
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
	if list := node.Parent(); list != root.FirstChild() {
		root.InsertBefore(root, root.FirstChild(), list)
	}
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
	if list, ok := root.FirstChild().(*extensionast.FootnoteList); ok {
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
