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
// refused for two reasons names the one it meets first. Conversion refuses nothing a reading can
// reach. Spacing is refused apart from it, before it (browserListSpacing).
func refuseBlocks(root ast.Node, source []byte) error {
	return ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		return ast.WalkContinue, blockRefusal(node, source)
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
