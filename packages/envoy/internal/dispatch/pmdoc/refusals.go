package pmdoc

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
)

// refuseBlocks refuses a document holding a block that cannot be stored as it reads (blockRefusal),
// walking the tree goldmark read before it is converted, in document order, so that a document
// refused for two reasons names the one it meets first. It refuses every block kind conversion does
// not convert (convertedBlocks), so conversion refuses no block. Spacing is refused apart from it,
// before it (browserListSpacing). The inline content of a paragraph, a heading or a table cell holds
// no block, and the walk does not enter it. firstLine is the number source's first line has in the
// markdown the caller wrote, so a refusal naming a line names it there.
func refuseBlocks(root ast.Node, source []byte, firstLine int) error {
	htmlFlow := htmlFlowLines(root)
	return ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if err := blockRefusal(node, source, firstLine, htmlFlow); err != nil {
			return ast.WalkStop, err
		}
		switch node.(type) {
		case *ast.Paragraph, *ast.TextBlock, *ast.Heading, *extensionast.TableCell:
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
}

// blockRefusal is why node cannot be stored: the browser editor's parser reads it differently
// from goldmark or refuses it, or the Proof schema holds nothing like it. A typed block that passes
// keeps its attributes' values for conversion (typedDirective.values). htmlFlow is which kind of
// block of HTML that parser opens on each of the document's paragraph lines it reads as opening
// one, by where that line starts (htmlFlowLines).
func blockRefusal(node ast.Node, source []byte, firstLine int, htmlFlow map[int]int) error {
	refuse := func(reason string) error { return fmt.Errorf("%w: %s", ErrSchema, reason) }
	switch current := node.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		if reason, ok := paragraphDirectiveReason(current.Lines(), source, firstLine); ok {
			return refuse(reason)
		}
		if err := htmlFlowRefusal(current, htmlFlow, source, firstLine); err != nil {
			return err
		}
	case *ast.Heading:
		// A setext heading holds the lines of the paragraph its underline ended.
		if err := htmlFlowRefusal(current, htmlFlow, source, firstLine); err != nil {
			return err
		}
		if _, underlined := current.Attribute(underlinedTextAttr); underlined {
			return refuse("a lone - under a table that text stands before in its paragraph, which the browser editor's parser reads as an empty list item after the table, and goldmark as the underline of a heading holding that text")
		}
	case *ast.FencedCodeBlock:
		// The browser editor's parser decodes backslash escapes and character references in the
		// info string's first word, the language, where goldmark keeps them as written.
		language := current.Language(source)
		if decodeMarkdownText(language) != string(language) {
			return refuse("a code block whose language holds a backslash escape or a character reference, which the browser editor's parser decodes and goldmark keeps as written")
		}
	case *ast.CodeBlock:
		// An indented code block stands right after a list, outside it, only where the list's last
		// item holds its content five or more columns in (a wide ordered marker, or tabs), and
		// right after a quote on the line after the quote's last. The browser editor's parser
		// keeps that list or quote open across the code's first line, which it does not continue,
		// and a code line that list or quote does not continue ends the code, so it reads the
		// later lines as a second code block. A blank line ends a quote.
		if current.Lines().Len() <= 1 {
			return nil
		}
		switch heldOpenBy(current).(type) {
		case *ast.List:
			return refuse("an indented code block right after a list, which the browser editor's parser splits after its first line")
		case *ast.Blockquote:
			return refuse("an indented code block right after a quote, which the browser editor's parser splits after its first line")
		}
	case *ast.HTMLBlock:
		// Proof's doc accepts blocks only, while html is an inline atom.
		return ErrBlockHTML
	case *extensionast.Footnote:
		if _, nested := ancestor[*extensionast.Footnote](current); nested {
			return refuse("a footnote definition inside another footnote definition, where the browser editor reads a line of = or - continuing the inner one's paragraph as a heading's underline")
		}
		if bytes.ContainsAny(current.Ref, " \t") {
			return refuse("a footnote definition whose label holds whitespace, which the browser editor's parser reads as a paragraph")
		}
		if err := typedDefinitionRefusal(current); err != nil {
			return err
		}
	case *typedDirective:
		return typedDirectiveRefusal(current)
	case *unsupportedDirective:
		return refuse(current.Reason)
	case *extensionast.Table:
		if _, lazy := current.Attribute(lazyRowAttr); lazy {
			return refuse("a table a line continuing its container lazily would be a row of, which the browser editor's parser reads as ending the table and the container")
		}
		if _, block := current.Attribute(blockRowAttr); block {
			return refuse("a table a line opening another block would be a row of - a list item that cannot interrupt a paragraph, or indented code - which the browser editor's parser reads as that block after the table")
		}
		if value, wide := current.Attribute(wideRowAttr); wide {
			row := value.(wideRow)
			// Where the table stands is for parseTableRows, which maps only its own table's refusal.
			row.firstBlock = current.PreviousSibling() == nil && current.Parent().Kind() == ast.KindDocument
			return fmt.Errorf("%w: %w", ErrSchema, row)
		}
	default:
		if node.Type() == ast.TypeBlock && !convertedBlocks[node.Kind()] {
			return refuse("unsupported markdown block " + node.Kind().String())
		}
	}
	return nil
}

// convertedBlocks are the block kinds conversion converts: parseBlock's, and the rows and cells
// parseTable converts. blockRefusal refuses every other block, a link reference definition among
// them, so a block conversion meets outside this set is this package's bug (parseBlock).
var convertedBlocks = map[ast.NodeKind]bool{
	ast.KindDocument:             true,
	ast.KindParagraph:            true,
	ast.KindTextBlock:            true,
	ast.KindHeading:              true,
	ast.KindBlockquote:           true,
	ast.KindList:                 true,
	ast.KindListItem:             true,
	ast.KindFencedCodeBlock:      true,
	ast.KindCodeBlock:            true,
	ast.KindThematicBreak:        true,
	extensionast.KindFootnote:    true,
	kindTypedDirective:           true,
	extensionast.KindTable:       true,
	extensionast.KindTableHeader: true,
	extensionast.KindTableRow:    true,
	extensionast.KindTableCell:   true,
}

// typedDefinitionInItem and typedDefinitionHoldingBlock are why a footnote definition inside a
// typed block is refused (typedDefinitionRefusal). Each is named so that what the reader says and
// what a test asserts are one text.
const (
	typedDefinitionInItem       = "a footnote definition inside a typed block with a list item around it, whose spacing the browser editor reads otherwise"
	typedDefinitionHoldingBlock = "a footnote definition inside a typed block holding a block other than a paragraph: the browser editor reads a list there with other spacing, and the definition's other blocks are refused with it until they are shown to read alike"
)

// typedDefinitionRefusal is why a footnote definition inside a typed block cannot be stored as it
// reads, or nil. The browser editor's parser reads a typed block as a container, so a definition
// inside one reads as a definition inside a quote does, and one holding paragraphs alone, with no
// list item around it, is read alike by both parsers.
//
// Where a list item stands around the definition, inside the typed block or outside it, that
// parser counts the definition's lines and the blank lines next to it otherwise when it decides
// whether the item is spread: it reads the first line of a definition opening on a later line, or
// of an empty one, as a blank line of the typed block, and a blank line inside the typed block as
// spacing the item. Where the definition holds a block other than a paragraph, which is what this
// checks, the block measured reading otherwise is a list, which that parser reads spread where this
// reader stores it tight; the definition's other blocks (a quote, code) are refused with it because
// none of them has been shown to read alike, which is what the refusal names, rather than claiming
// that the spacing of each is misread.
func typedDefinitionRefusal(definition *extensionast.Footnote) error {
	if _, typed := ancestor[*typedDirective](definition); !typed {
		return nil
	}
	if _, item := ancestor[*ast.ListItem](definition); item {
		return fmt.Errorf("%w: %s", ErrSchema, typedDefinitionInItem)
	}
	for child := definition.FirstChild(); child != nil; child = child.NextSibling() {
		if _, paragraph := child.(*ast.Paragraph); !paragraph {
			return fmt.Errorf("%w: %s", ErrSchema, typedDefinitionHoldingBlock)
		}
	}
	return nil
}

// typedDirectiveRefusal refuses a typed block the browser editor's parser reads differently or
// refuses, or whose type or attributes the schema does not declare, and otherwise keeps its
// attributes' values, defaults included, on the directive.
func typedDirectiveRefusal(directive *typedDirective) error {
	if !directive.Closed && directive.Parent().Kind() == ast.KindDocument {
		return fmt.Errorf("%w: typed block %q is unclosed at document level", ErrSchema, directive.Name)
	}
	if _, lazy := directive.Attribute(lazyTypedParagraphAttr); lazy {
		return fmt.Errorf("%w: typed block %q holds a line continuing a paragraph in a typed block from outside the block's container, which the browser editor's parser reads as ending the typed block", ErrSchema, directive.Name)
	}
	typ, ok := typedBlock(directive.Name)
	if !ok {
		return fmt.Errorf("%w: unknown typed block %q (known types: %s)", ErrSchema, directive.Name, strings.Join(typedBlockNames(), ", "))
	}
	if err := undeclaredAttributes(directive.Name, directive.Attrs, typ.Attributes); err != nil {
		return err
	}
	values := defaultAttributes(typ)
	for name, raw := range directive.Attrs {
		if name == BlockIDAttr {
			values[name] = raw
			continue
		}
		value, err := parseTypedAttributeValue(typ.Attributes[name], raw)
		if err != nil {
			return fmt.Errorf("%w: typed block %q attribute %q: %v", ErrSchema, directive.Name, name, err)
		}
		values[name] = value
	}
	directive.values = values
	return nil
}
