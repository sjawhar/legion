package pmdoc

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/util"
)

// refuseBlocks refuses a document holding a block that cannot be stored as it reads (blockRefusal),
// walking the tree goldmark read before it is converted, in document order, so that a document
// refused for two reasons names the one it meets first. It refuses every block kind conversion does
// not convert (convertedBlocks), so conversion refuses no block. Spacing is refused apart from it,
// before it (browserListSpacing). The inline content of a paragraph, a heading or a table cell holds
// no block, and the walk does not enter it.
func refuseBlocks(root ast.Node, source []byte) error {
	return ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if err := blockRefusal(node, source); err != nil {
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
// keeps its attributes' values for conversion (typedDirective.values).
func blockRefusal(node ast.Node, source []byte) error {
	refuse := func(reason string) error { return fmt.Errorf("%w: %s", ErrSchema, reason) }
	switch current := node.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		if reason, ok := paragraphDirectiveReason(current.Lines(), source); ok {
			return refuse(reason)
		}
	case *ast.Heading:
		if _, underlined := current.Attribute(underlinedTextAttr); underlined {
			return refuse("a lone - under a table that text stands before in its paragraph, which the browser editor's parser reads as an empty list item after the table, and goldmark as the underline of a heading holding that text")
		}
	case *ast.FencedCodeBlock:
		// The browser editor's parser decodes backslash escapes and character references in the
		// info string's first word, the language, where goldmark keeps them as written.
		language := current.Language(source)
		if !bytes.Equal(util.ResolveEntityNames(util.ResolveNumericReferences(util.UnescapePunctuations(language))), language) {
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
		switch current.PreviousSibling().(type) {
		case *ast.List:
			return refuse("an indented code block right after a list, which the browser editor's parser splits after its first line")
		case *ast.Blockquote:
			if !current.HasBlankPreviousLines() {
				return refuse("an indented code block right after a quote, which the browser editor's parser splits after its first line")
			}
		}
	case *ast.HTMLBlock:
		// Proof's doc accepts blocks only, while html is an inline atom.
		return ErrBlockHTML
	case *extensionast.Footnote:
		if _, nested := ancestor[*extensionast.Footnote](current); nested {
			return refuse("a footnote definition inside another footnote definition, where the browser editor reads a line of = or - continuing the inner one's paragraph as a heading's underline")
		}
		if _, typed := ancestor[*typedDirective](current); typed {
			return refuse("a footnote definition inside a typed block, which the browser editor refers to only from inside a typed block or after it")
		}
		if bytes.ContainsAny(current.Ref, " \t") {
			return refuse("a footnote definition whose label holds whitespace, which the browser editor's parser reads as a paragraph")
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
			return refuse(fmt.Sprintf("a table row holding %d cells where its table has %d, %q: a cell ends at every | not written \\|, in code and links too, and goldmark drops the cells past the table's width, which the browser editor's parser keeps; write a | inside a cell as \\|, or give the header and delimiter rows as many cells as the row", row.cells, row.width, row.opening))
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
