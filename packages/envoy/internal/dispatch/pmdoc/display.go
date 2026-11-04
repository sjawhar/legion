package pmdoc

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/util"
)

// DisplayMode is which of a stored document's two readings for display ReadForDisplay makes.
type DisplayMode int

const (
	// DisplayLatest is the reading of a document as it stands now: every attribute the schema
	// marks `server` takes its default (StripServerOwnedAttrs), since outside a document no
	// server wrote them, so a body's `:::ask{state="answered"}` reads as the open decision its
	// author wrote. The browser's read-only engine drops the same attributes
	// (dropServerAttributes, proof-editor's read-only-directives.ts).
	DisplayLatest DisplayMode = iota
	// DisplayVersion is the reading of a past version's text, and of the baseline a change mark
	// is drawn against: the attributes the server wrote into that text are kept, since an answer
	// a version recorded is part of what that version said.
	DisplayVersion
)

// ReadForDisplay reads markdown as the tree a reader is shown, and never refuses: a document the
// writer would not store is still a document somebody has to read. It is Parse with five
// differences, the first three of them the rules of the browser's read-only engine
// (remarkReadOnlyDirectives and remarkDecideTypedBlocks, proof-editor's read-only-directives.ts
// and typed-block-drawing.ts), which this reading must agree with block for block:
//
//   - A typed block the block schema refuses, one whose content breaks its content rule, and a
//     container directive of a name the schema does not know are drawn in place as their literal
//     fence lines, the content between them read as the markdown it is (drawDisplayBlocks).
//   - A leaf directive (`::name`), a text directive (`:name{…}`), a Pandoc fenced div
//     (`::: name`) and a directive opener standing inside maxTypedNesting open directives are the
//     paragraph text the browser's tokenizer leaves them as: no parser of displayReader opens
//     them, and the nesting guard declines the opener rather than refusing the document.
//   - Under DisplayLatest every server-owned attribute takes its schema default.
//   - A block Parse refuses reads as one paragraph of its literal text, and every other block
//     keeps its formatting.
//   - A block carries the id the markdown named for it and no other. A display tree is a thing
//     to draw, never a thing to edit: no ask row, anchor or margin card is keyed on an id this
//     read invented, because only a typed block's markdown writes an id (TypedBlockIDAttribute),
//     and a paragraph's never reaches the text. A minted id was therefore of no use to any
//     reader, cost about a tenth of a tree's bytes, and, being random, made the tree different
//     on every read - which defeats compression and leaves the kept encoding (DisplayJSON, keyed
//     by the SHA-256 of the text and the mode) holding a tree no second read would produce. The
//     browser mints what it needs itself: the editor surfaces stamp every block the server's
//     tree left unnamed as they load it (proof-editor's blockIdsPlugin, which answers the local
//     transaction setDocumentJSON dispatches), and the drawn surfaces that read `data-block-id`
//     (the margin's block offsets, the reading pane's section bar, a decision card's anchor and
//     a thread's) look blocks up by ids that stand in a stored document, which are exactly the
//     ids the markdown names.
//
// Which refusal can be answered for one block, and which only for the whole document:
//
//   - One block's. Every blockRefusal verdict (refusals.go), ErrBlockHTML among them, names the
//     block it walked to; every Validate verdict (node.go) names a node, which stands under one
//     of the document's blocks; browserListSpacing names a list; and ErrTablePadding and
//     ErrTableWidth name a table. Each of those is answered by reading the document's top-level
//     blocks one at a time (displayBlockByBlock): a block that still refuses reads as one
//     paragraph of its literal text, and the blocks around it are read as they stand.
//   - The whole document's. ErrTooManyElements (elements.go) is the weight of everything the
//     markdown makes, and the guard that carries it stops the parse part-way through, so no tree
//     reaches the blocks after it and no block can be named; a recovered panic (panic.go) leaves
//     no tree at all; and inline marks nested past maxInlineNesting leave a tree no walk may
//     enter. Each of those reads as one paragraph of literal text per part between blank lines
//     (literalDocument), which costs one pass over the markdown and no parse.
//
// The one refusal of Parse that stays a refusal of nothing here is markdown nested past
// maxNesting or maxTypedNesting: the guard declines the opener, which is then the paragraph text
// the browser's tokenizer reads it as, so there is no refusal left to answer.
func ReadForDisplay(markdown string, mode DisplayMode) *Node {
	doc := displayDocument(markdown)
	if mode == DisplayLatest {
		StripServerOwnedAttrs(doc)
	}
	return doc
}

// displayReader is blockReader with the two directive readings the browser's read-only engine
// has. Its typed-block parser opens every `:::name[label]{attributes}` container the browser's
// tokenizer opens (parseDisplayDirectiveOpen), whatever the schema knows of the name, so a
// directive it refuses is drawn in place rather than read as unrelated lines; and it registers no
// unsupported-directive parser, so a leaf directive, a text directive and a Pandoc fenced div are
// the paragraph text that engine reads them as.
var displayReader = markdownReader{display: true, md: goldmark.New(
	goldmark.WithParser(parser.NewParser(
		parser.WithBlockParsers(blockParsers()...),
		parser.WithInlineParsers(inlineParsers(editorRules)...),
		parser.WithParagraphTransformers(util.Prioritized(linkReferenceDefinitions{parser.LinkReferenceParagraphTransformer}, 100)),
	)),
	goldmark.WithExtensions(linkify{}, lazyAwareTable{}, taskList{}, footnotes{}),
	goldmark.WithParserOptions(
		parser.WithBlockParsers(
			util.Prioritized(elementBlockCounter{}, -1),
			util.Prioritized(frontmatterAttempt{nestingGuard{BlockParser: &typedDirectiveParser{display: true}}}, 950),
			util.Prioritized(lineRecordingParagraph{parser.NewParagraphParser()}, 999),
		),
		parser.WithInlineParsers(util.Prioritized(elementInlineCounter{}, -1)),
	),
)}

// displayDocument is markdown's display tree, with the fallbacks that keep the read from refusing
// (ReadForDisplay). A panic anywhere in it is logged and answered with the document's literal
// text, as recoverPanic answers one for a caller that may be refused.
func displayDocument(markdown string) (doc *Node) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("pmdoc: the display read panicked", "panic", recovered, "stack", string(debug.Stack()))
			doc = literalDocument(markdown)
		}
	}()
	source := []byte(markdown)
	front, rest, unclosedFrontmatter := parseFrontmatterBlock(source)
	body := source[rest:]
	root, context, err := displayRoot(body, unclosedFrontmatter, NewWriteBudget())
	if err != nil {
		return literalDocument(markdown)
	}
	spans := displaySpans(body, root)
	children, err := displayChildren(body, root, context)
	if err != nil {
		children = displayBlockByBlock(body, spans)
	}
	if front != nil {
		children = append([]*Node{front}, children...)
	}
	if len(children) == 0 {
		children = []*Node{{Type: "paragraph"}}
	}
	return &Node{Type: "doc", Children: children}
}

// displayRoot is goldmark's tree of body under the display reader, read on budget, with the
// parser context the parse kept, or the refusal that leaves the display read no tree to draw:
// inline marks nested past maxInlineNesting, whose tree no walk may enter, and markdown whose
// elements pass MaxDocumentElements, whose parse stopped part-way through the document. A block
// nested past maxNesting or maxTypedNesting is no refusal here (nestingRefusal reads the reader's
// display flag): the guard declined the opener, which the paragraph around it keeps as text.
func displayRoot(body []byte, unclosedFrontmatter bool, budget *WriteBudget) (ast.Node, parser.Context, error) {
	root, count, context, err := displayReader.read(body, unclosedFrontmatter, budget, 0)
	if err != nil {
		return nil, nil, err
	}
	count.weigh(root, body)
	if refusal := count.refusal(body, 1); refusal != nil {
		return nil, nil, refusal
	}
	return root, context, nil
}

// displayChildren is the document blocks body's tree reads as, or the refusal one of its blocks
// earns: the display read answers that by reading the blocks one at a time
// (displayBlockByBlock). The tables' padding and the list spacing are read before the drawing, so
// both see the blocks goldmark opened; the drawing then puts the directives the schema refuses
// back as their fence lines, and only the blocks left over are held to blockRefusal.
func displayChildren(body []byte, root ast.Node, context parser.Context) ([]*Node, error) {
	if padding, _ := context.Get(tablePaddingErrorKey).(error); padding != nil {
		return nil, padding
	}
	if err := browserListSpacing(root, body); err != nil {
		return nil, err
	}
	drawDisplayBlocks(root, body)
	if err := displayRefusals(root, body); err != nil {
		return nil, err
	}
	doc, err := parseBlock(root, body, footnoteLabels(root), 0)
	if err != nil {
		return nil, err
	}
	sortNodeMarks(doc)
	for _, child := range doc.Children {
		if !isBlockNodeType(child.Type) {
			return nil, errUnreadableForDisplay
		}
		if err := child.Validate(); err != nil {
			return nil, err
		}
	}
	return doc.Children, nil
}

// errUnreadableForDisplay is the display read's own verdict on a block it cannot hold at a
// document's level, which sends that block to be read on its own (displayBlockByBlock).
var errUnreadableForDisplay = fmt.Errorf("%w: the display read cannot hold this block at a document's level", ErrSchema)

// displaySpan is where one of a document's top-level blocks starts and stops in the body read.
type displaySpan struct{ start, stop int }

// displaySpans is where each top-level block of root stands in body, every byte of body in
// exactly one span: a block starts where goldmark records it opening (sourceStart) and runs to
// where the next one starts, so the blank lines after a block are its own, and the first span
// opens at the body's first byte. It is read before the drawing, which replaces nodes, and a
// block goldmark records no position for is left to the span before it.
func displaySpans(body []byte, root ast.Node) []displaySpan {
	var spans []displaySpan
	for block := root.FirstChild(); block != nil; block = block.NextSibling() {
		start := sourceStart(block)
		if start < 0 || (len(spans) > 0 && start <= spans[len(spans)-1].start) {
			continue
		}
		if len(spans) > 0 {
			spans[len(spans)-1].stop = start
		}
		spans = append(spans, displaySpan{start: start, stop: len(body)})
	}
	if len(spans) == 0 {
		return []displaySpan{{start: 0, stop: len(body)}}
	}
	spans[0].start = 0
	return spans
}

// displayBlockByBlock is body read one top-level block at a time, for a document one of whose
// blocks the whole read refused: each span is read on its own, and one that still refuses is the
// paragraph of its literal text that answers a refusal named for one block (ReadForDisplay). The
// spans share one budget, so the whole document still makes at most MaxDocumentElements elements
// and its tables at most one write's padding cells, as the whole read would have.
func displayBlockByBlock(body []byte, spans []displaySpan) []*Node {
	budget := NewWriteBudget()
	children := make([]*Node, 0, len(spans))
	for _, span := range spans {
		children = append(children, displaySpanNodes(body[span.start:span.stop], budget)...)
	}
	return children
}

// displaySpanNodes is one span's blocks, or the one paragraph of literal text a span the display
// read still refuses is shown as. A span is read with no front matter: a `---` line opens front
// matter only where the document does, and the document's own front matter was read before the
// body (displayDocument).
func displaySpanNodes(part []byte, budget *WriteBudget) []*Node {
	if root, context, err := displayRoot(part, false, budget); err == nil {
		if children, err := displayChildren(part, root, context); err == nil {
			return children
		}
	}
	text := strings.Trim(string(part), "\n")
	if strings.TrimLeft(text, " \t\n") == "" {
		return nil
	}
	return []*Node{{Type: "paragraph", Children: displayLiteral(text)}}
}

// literalDocument is markdown read as nothing but its own text: one paragraph of literal text per
// part between blank lines. It answers the refusals no block can be named for (ReadForDisplay)
// without parsing the markdown again, which is what a document past the element limit must not
// pay for.
func literalDocument(markdown string) *Node {
	doc := &Node{Type: "doc"}
	for _, part := range displayParts(markdown) {
		doc.Children = append(doc.Children, &Node{Type: "paragraph", Children: displayLiteral(part)})
	}
	if len(doc.Children) == 0 {
		doc.Children = []*Node{{Type: "paragraph"}}
	}
	return doc
}

// displayParts is text cut at its blank lines, the lines holding nothing but spaces and tabs,
// with every part that holds no text left out.
func displayParts(text string) []string {
	var parts []string
	for rest := text; rest != ""; {
		line, tail, _ := strings.Cut(rest, "\n")
		rest = tail
		if strings.Trim(line, " \t\r") == "" {
			continue
		}
		part := strings.Builder{}
		part.WriteString(line)
		for rest != "" {
			line, tail, _ = strings.Cut(rest, "\n")
			if strings.Trim(line, " \t\r") == "" {
				break
			}
			rest = tail
			part.WriteByte('\n')
			part.WriteString(line)
		}
		parts = append(parts, part.String())
	}
	return parts
}

// displayLiteral is text as one paragraph's inline content: each of its lines a text node, with a
// hard break between them, so the paragraph shows the markdown exactly as it is written.
func displayLiteral(text string) []*Node {
	lines := strings.Split(text, "\n")
	children := make([]*Node, 0, 2*len(lines)-1)
	for index, line := range lines {
		if index > 0 {
			children = append(children, &Node{Type: "hardbreak", Attrs: Attrs{"isInline": false}})
		}
		if line != "" {
			children = append(children, &Node{Type: "text", Text: line})
		}
	}
	return children
}

// kindDisplayText is the node drawDisplayBlocks leaves where a block the display read shows as
// its own text stood.
var kindDisplayText = ast.NewNodeKind("DisplayText")

// displayText is a block the display read shows as the markdown it is written as: one paragraph
// of literal text, which conversion makes of it (parseBlock). It carries that text rather than a
// span of the source, since the block it stands for may be written across lines a container
// prefixes.
type displayText struct {
	ast.BaseBlock
	text string
}

func (n *displayText) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

func (n *displayText) Kind() ast.NodeKind {
	return kindDisplayText
}

// displayFillAttr marks a typed block the display read keeps typed whose content Milkdown's
// NodeType.createAndFill would open with an empty paragraph, because the block's content rule
// wants one before what the markdown wrote (displayContentFit). Only the display read sets it;
// conversion reads it as the browser's parser reads the node it builds.
var displayFillAttr = []byte("pmdoc-display-fill-paragraph")

// drawDisplayBlocks rewrites the blocks of parent the display read does not convert as they
// stand, innermost first, as the browser's read-only engine decides them (decideTypedBlocks,
// proof-editor's typed-block-drawing.ts): a container directive the schema refuses, or whose
// content its content rule refuses, is drawn as its opening fence line, the blocks it held, and
// its closing fence where the markdown closes it; a directive the schema takes keeps the
// attribute values the conversion writes. Every other block conversion has no node for - an HTML
// block, which the Proof schema carries inline only - is shown as its own text. Each is decided
// after the blocks inside it, so a drawn block's lines are weighed against its parent's content
// rule in turn.
func drawDisplayBlocks(parent ast.Node, source []byte) {
	for child := parent.FirstChild(); child != nil; {
		next := child.NextSibling()
		if entersDisplayBlocks(child) {
			drawDisplayBlocks(child, source)
		}
		if directive, typed := child.(*typedDirective); typed {
			drawDisplayDirective(parent, directive, source)
		} else if child.Type() == ast.TypeBlock && !convertedBlocks[child.Kind()] && child.Kind() != kindDisplayText {
			parent.ReplaceChild(parent, child, displayTextOf(child, source))
		}
		child = next
	}
}

// entersDisplayBlocks reports whether the drawing walks into node: the blocks that hold blocks, a
// directive among them. A paragraph, a heading and a table cell hold inline content alone, and a
// table's rows hold cells, so no directive opens in any of them.
func entersDisplayBlocks(node ast.Node) bool {
	switch node.Kind() {
	case ast.KindBlockquote, ast.KindList, ast.KindListItem, kindTypedDirective, extensionast.KindFootnote:
		return true
	}
	return false
}

// drawDisplayDirective decides one container directive: it keeps the typed block the schema takes
// and whose blocks fit its content rule, writing the attribute values conversion reads
// (typedDirective.values), and otherwise puts the directive back as its fence lines around the
// blocks it held, which stay where they were written.
func drawDisplayDirective(parent ast.Node, directive *typedDirective, source []byte) {
	if values, known := displayTypedValues(directive); known {
		if fill, fits := displayContentFit(directive.Name, displayChildTypes(directive)); fits {
			directive.values = values
			if fill {
				directive.SetAttribute(displayFillAttr, true)
			}
			return
		}
	}
	opening := &displayText{text: displayOpeningLine(directive, source)}
	opening.SetPos(startOf(directive))
	parent.InsertBefore(parent, directive, opening)
	for child := directive.FirstChild(); child != nil; {
		next := child.NextSibling()
		directive.RemoveChild(directive, child)
		parent.InsertBefore(parent, directive, child)
		child = next
	}
	if closing := displayClosingFence(directive, source); closing != "" {
		fence := &displayText{text: closing}
		fence.SetPos(directive.closer)
		parent.InsertBefore(parent, directive, fence)
	}
	parent.RemoveChild(parent, directive)
}

// displayOpeningLine is the directive's opening fence line as it is written, from where the
// colons stand to the line's end, which is what the browser's drawing shows (drawDirective).
func displayOpeningLine(directive *typedDirective, source []byte) string {
	start := startOf(directive)
	stop := start
	for stop < len(source) && source[stop] != '\n' && source[stop] != '\r' {
		stop++
	}
	return string(source[start:stop])
}

// displayClosingFence is the run of colons that closed the directive, which the browser's drawing
// shows as the whole closing line (drawDirective keeps the match's colon run alone), or "" for a
// directive the markdown never closes.
func displayClosingFence(directive *typedDirective, source []byte) string {
	if !directive.Closed || directive.closer < 0 || directive.closer > len(source) {
		return ""
	}
	stop := directive.closer
	for stop < len(source) && source[stop] != '\n' && source[stop] != '\r' {
		stop++
	}
	colons := fenceColons(string(source[directive.closer:stop]))
	if colons == 0 {
		return ""
	}
	return strings.Repeat(":", colons)
}

// displayTextOf is block shown as the markdown it is written as, its lines and the line that
// closes it where goldmark records one apart from them (an HTML block's).
func displayTextOf(block ast.Node, source []byte) *displayText {
	text := segmentsText(block.Lines(), source)
	if html, ok := block.(*ast.HTMLBlock); ok && html.HasClosure() {
		text += string(html.ClosureLine.Value(source))
	}
	node := &displayText{text: strings.TrimRight(text, "\n")}
	node.SetPos(max(sourceStart(block), 0))
	return node
}

// displayTypedValues is the attribute values a typed block of this directive's name and
// attributes carries, and whether the schema takes it at all: the browser's typedBlockRefusal
// (proof-editor's block-schema.ts), which knows the name and reads every attribute the type
// declares. Unlike typedDirectiveRefusal it asks nothing of where the directive stands, since a
// reader is shown the block a browser draws, and that engine draws an unclosed directive as the
// typed block it opened.
func displayTypedValues(directive *typedDirective) (Attrs, bool) {
	typ, known := typedBlock(directive.Name)
	if !known {
		return nil, false
	}
	if err := undeclaredAttributes(directive.Name, directive.Attrs, typ.Attributes); err != nil {
		return nil, false
	}
	values := defaultAttributes(typ)
	for name, raw := range directive.Attrs {
		if name == BlockIDAttr {
			values[name] = raw
			continue
		}
		value, err := parseTypedAttributeValue(typ.Attributes.Get(name), raw)
		if err != nil {
			return nil, false
		}
		values[name] = value
	}
	return values, true
}

// displayChildTypes is the Proof node type each of the directive's blocks is read as, the
// decision already made for the directives among them: a drawn directive left paragraphs of its
// fence lines, and a kept one is a block of its own type.
func displayChildTypes(directive *typedDirective) []string {
	types := make([]string, 0, directive.ChildCount())
	for child := directive.FirstChild(); child != nil; child = child.NextSibling() {
		types = append(types, displayNodeType(child))
	}
	return types
}

// displayNodeType is the Proof node type block converts to, or "" for one no node holds.
func displayNodeType(block ast.Node) string {
	switch current := block.(type) {
	case *ast.Paragraph, *ast.TextBlock, *displayText:
		return "paragraph"
	case *ast.Heading:
		return "heading"
	case *ast.Blockquote:
		return "blockquote"
	case *ast.List:
		if current.IsOrdered() {
			return "ordered_list"
		}
		return "bullet_list"
	case *ast.FencedCodeBlock, *ast.CodeBlock:
		return "code_block"
	case *ast.ThematicBreak:
		return "hr"
	case *typedDirective:
		return current.Name
	}
	switch block.Kind() {
	case extensionast.KindFootnote:
		return "footnote_definition"
	case extensionast.KindTable:
		return "table"
	}
	return ""
}

// displayContentFit reports whether Milkdown can build the typed block name around blocks of
// these types, and whether building it would open its content with an empty paragraph: the
// browser's fitsContentRule (proof-editor's typed-block-drawing.ts), which asks
// NodeType.createAndFill, a null there being the block drawn as its fence lines instead. That
// call fills what the rule wants before the content as well as after it, so an ask written with
// its list alone keeps the empty paragraph the rule wants ahead of it (emptyParagraphFirst reads
// displayFillAttr), while one written with a heading fits nowhere and is drawn.
func displayContentFit(name string, children []string) (fill, fits bool) {
	typ, known := typedBlock(name)
	if !known {
		return false, false
	}
	switch typ.Content {
	case BlockContentBlocks:
		for _, child := range children {
			if !isBlockNodeType(child) {
				return false, false
			}
		}
		return false, true
	case BlockContentParagraphs:
		for _, child := range children {
			if child != "paragraph" {
				return false, false
			}
		}
		return false, true
	case BlockContentParagraphsOptionalBulletList:
		rest := children
		for len(rest) > 0 && rest[0] == "paragraph" {
			rest = rest[1:]
		}
		if len(rest) > 0 && rest[0] == "bullet_list" {
			rest = rest[1:]
		}
		if len(rest) > 0 {
			return false, false
		}
		return len(children) > 0 && children[0] != "paragraph", true
	}
	return false, false
}

// displayRefusals is refuseBlocks over the blocks the drawing left, in document order: a block
// this answers for is shown as its own text (ReadForDisplay). It differs from refuseBlocks in
// what it leaves out, which is what the display read has already decided: a paragraph holding a
// line that opens like a directive, which is the text the browser reads it as; a block shown as
// its own text; and a typed block the drawing kept, whose own blocks it still holds.
func displayRefusals(root ast.Node, source []byte) error {
	return ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node.(type) {
		case *ast.Paragraph, *ast.TextBlock:
			return ast.WalkSkipChildren, nil
		case *displayText:
			return ast.WalkSkipChildren, nil
		case *typedDirective:
			return ast.WalkContinue, nil
		}
		if err := blockRefusal(node, source, 1); err != nil {
			return ast.WalkStop, err
		}
		switch node.(type) {
		case *ast.Heading, *extensionast.TableCell:
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
}

// parseDisplayDirectiveOpen reads a container directive's opening line as the browser's tokenizer
// reads it (tokenizeDirectiveContainerStart, proof-editor's directive-container.ts): three or
// more colons, a name, an optional `[label]`, optional `{attributes}`, and nothing but spaces and
// tabs to the line's end. A label is the opening line's own text, which the drawing shows and the
// parse holds no block for, so it is read and dropped. Attributes that do not read, or a label
// that does not close, leave the line no directive, as they do there: that tokenizer puts the
// `{` or the `[` back and then finds the line does not end.
func parseDisplayDirectiveOpen(line string) (string, Attrs, bool) {
	colons := colonRun([]byte(line))
	if colons < 3 {
		return "", nil, false
	}
	name, rest, ok := displayDirectiveName(line[colons:])
	if !ok {
		return "", nil, false
	}
	if strings.HasPrefix(rest, "[") {
		if rest, ok = displayDirectiveLabel(rest); !ok {
			return "", nil, false
		}
	}
	attrs := Attrs{}
	if strings.HasPrefix(rest, "{") {
		inner, after, closed := displayDirectiveAttributes(rest)
		if !closed {
			return "", nil, false
		}
		parsed, err := parseDirectiveAttributes(inner)
		if err != nil {
			return "", nil, false
		}
		attrs, rest = parsed, after
	}
	if strings.Trim(rest, " \t") != "" {
		return "", nil, false
	}
	return name, attrs, true
}

// displayDirectiveName is the name at the start of rest as the browser's factoryName reads it
// (micromark-extension-directive's lib/factory-name.js, copied into proof-editor's
// directive-factories.ts): a run of characters that are neither Unicode punctuation nor
// whitespace, where `-` and `_` may stand inside it but neither may open or end it. It answers
// the name and what follows it.
func displayDirectiveName(rest string) (string, string, bool) {
	if rest == "" {
		return "", "", false
	}
	char, size := utf8.DecodeRuneInString(rest)
	if jsWhitespace(char) || directivePunctuation(char) {
		return "", "", false
	}
	end, last := size, char
	for end < len(rest) {
		next, width := utf8.DecodeRuneInString(rest[end:])
		if jsWhitespace(next) || directivePunctuation(next) && next != '-' && next != '_' {
			break
		}
		end, last = end+width, next
	}
	if last == '-' || last == '_' {
		return "", "", false
	}
	return rest[:end], rest[end:], true
}

// directivePunctuation reports whether char is what micromark reads as punctuation in a
// directive's name: Unicode punctuation or a Unicode symbol (micromark-util-character's
// unicodePunctuation, `\p{P}|\p{S}`).
func directivePunctuation(char rune) bool {
	return unicode.IsPunct(char) || unicode.IsSymbol(char)
}

// displayDirectiveLabel skips the `[label]` rest opens with as the browser's factoryLabel reads
// it with line endings disallowed: brackets balance up to maxDirectiveLabelBrackets deep, a
// backslash escapes `[`, `\` and `]`, and a label the line does not close is no label.
func displayDirectiveLabel(rest string) (string, bool) {
	balance := 0
	for index := 1; index < len(rest); index++ {
		switch rest[index] {
		case '[':
			if balance++; balance > maxDirectiveLabelBrackets {
				return "", false
			}
		case ']':
			if balance == 0 {
				return rest[index+1:], true
			}
			balance--
		case '\\':
			if index+1 < len(rest) && strings.IndexByte(`[\]`, rest[index+1]) >= 0 {
				index++
			}
		}
	}
	return "", false
}

// maxDirectiveLabelBrackets is how many brackets a directive's label may open inside one another
// before the browser's factoryLabel reads it as no label.
const maxDirectiveLabelBrackets = 32

// displayDirectiveAttributes is the text between the braces rest opens with and what follows the
// closing brace: the brace that closes them is the first one outside a quoted value, as the
// browser's factoryAttributes reads them.
func displayDirectiveAttributes(rest string) (inner, after string, closed bool) {
	quote := byte(0)
	for index := 1; index < len(rest); index++ {
		switch char := rest[index]; {
		case quote != 0:
			if char == quote {
				quote = 0
			}
		case char == '"' || char == '\'':
			quote = char
		case char == '}':
			return rest[1:index], rest[index+1:], true
		}
	}
	return "", "", false
}
