package docs

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// ErrInvalidOp identifies the malformed user-facing operation field.
type ErrInvalidOp struct {
	Field  string
	Reason string
}

func (e *ErrInvalidOp) Error() string {
	return fmt.Sprintf("invalid document operation field %q: %s", e.Field, e.Reason)
}

func invalidOp(field string) error {
	return &ErrInvalidOp{Field: field}
}

// ErrQuoteNotFound explains how document edit quotes are matched and names the closest blocks.
type ErrQuoteNotFound struct {
	Nearest []string
}

func (e *ErrQuoteNotFound) Error() string {
	return fmt.Sprintf(`quote not found; quotes match the block text as rendered (inline markdown is tolerated; use "heading:<title>", "block:<id>", "start", or "end" as insert and move anchors); nearest blocks: %s`, pmdoc.QuoteBlocks(e.Nearest))
}

func (e *ErrQuoteNotFound) Unwrap() error { return pmdoc.ErrTargetNotFound }

// applyOperations applies each operation to its predecessor's tree so a
// following operation resolves the structure created by the preceding one.
func applyOperations(tree *pmdoc.Node, ops []model.EditOp) (*pmdoc.Node, error) {
	for index, op := range ops {
		next, err := applyOperation(tree, op)
		if err != nil {
			return nil, fmt.Errorf("operation %d: %w", index, err)
		}
		tree = next
	}
	return tree, nil
}

func applyOperation(tree *pmdoc.Node, op model.EditOp) (*pmdoc.Node, error) {
	switch op.Op {
	case "replace":
		if op.Find == "" {
			return nil, invalidOp("find")
		}
		r, err := findEditQuote(tree, op.Find, op.Occurrence)
		if err != nil {
			return nil, err
		}
		with, err := inlineReplacement(op.With)
		if err != nil {
			return nil, err
		}
		return pmdoc.Splice(tree, r, with)
	case "delete":
		if op.Block != "" {
			if op.Find != "" {
				return nil, &ErrInvalidOp{Field: "find", Reason: "delete takes find or block, not both"}
			}
			out, err := pmdoc.DeleteBlock(tree, op.Block)
			return out, invalidSchemaOp("block", err)
		}
		if op.Find == "" {
			return nil, invalidOp("find or block")
		}
		r, err := findEditQuote(tree, op.Find, op.Occurrence)
		if err != nil {
			return nil, err
		}
		if out, removed, err := pmdoc.DeleteTextblock(tree, r); err != nil || removed {
			return out, invalidSchemaOp("find", err)
		}
		empty, err := parseInput("")
		if err != nil {
			return nil, err
		}
		return pmdoc.Splice(tree, r, empty)
	case "insert":
		if op.Markdown == "" {
			return nil, invalidOp("markdown")
		}
		anchor, after, err := anchorOf(op)
		if err != nil {
			return nil, err
		}
		target, plainText, err := insertTarget(tree, anchor, op.Occurrence)
		if err != nil {
			return nil, err
		}
		if plainText && pmdoc.TargetSpansBlocks(tree, target) {
			return nil, pmdoc.ErrTargetSpansBlocks
		}
		with, err := parseInput(op.Markdown)
		if err != nil {
			return nil, invalidMarkdownOp("markdown", err)
		}
		if out, inserted, err := pmdoc.InsertTableRows(tree, target, op.Markdown, after); err != nil || inserted {
			return out, err
		}
		position := target.From
		if after {
			position = target.To
		}
		if anchor != "start" && anchor != "end" {
			position, err = pmdoc.BlockBoundary(tree, target, after)
			if err != nil {
				return nil, err
			}
		}
		return pmdoc.Splice(tree, pmdoc.Range{From: position, To: position}, with)
	case "move":
		if op.Block == "" {
			return nil, invalidOp("block")
		}
		anchor, after, err := anchorOf(op)
		if err != nil {
			return nil, err
		}
		target, _, err := insertTarget(tree, anchor, op.Occurrence)
		if err != nil {
			return nil, err
		}
		// The document edges are empty ranges; MoveBlock lands on the edge they name.
		switch anchor {
		case "start":
			after = false
		case "end":
			after = true
		}
		out, err := pmdoc.MoveBlock(tree, op.Block, target, after)
		if errors.Is(err, pmdoc.ErrMoveInsideItself) {
			field := "before"
			if op.After != "" {
				field = "after"
			}
			return nil, &ErrInvalidOp{Field: field, Reason: err.Error()}
		}
		if errors.Is(err, pmdoc.ErrSchema) {
			return nil, &ErrInvalidOp{Field: "block", Reason: err.Error()}
		}
		return out, err
	case "retype":
		if op.Block == "" {
			return nil, invalidOp("block")
		}
		if op.Type == "" {
			return nil, invalidOp("type")
		}
		out, err := pmdoc.RetypeBlock(tree, op.Block, op.Type, pmdoc.Attrs(op.Attributes))
		if errors.Is(err, pmdoc.ErrSchema) {
			field := "attributes"
			switch {
			case errors.Is(err, pmdoc.ErrUnknownBlockType):
				field = "type"
			case errors.Is(err, pmdoc.ErrBlockNotRetypable):
				field = "block"
			}
			return nil, &ErrInvalidOp{Field: field, Reason: err.Error()}
		}
		return out, err
	default:
		return nil, invalidOp("op")
	}
}

// anchorOf returns the one insert or move anchor an operation names.
func anchorOf(op model.EditOp) (anchor string, after bool, err error) {
	if (op.After == "" && op.Before == "") || (op.After != "" && op.Before != "") {
		return "", false, invalidOp("after or before")
	}
	if op.After != "" {
		return op.After, true, nil
	}
	return op.Before, false, nil
}

// invalidSchemaOp reports a tree operation that would leave a container outside
// its content rule as an invalid operation on the field that named the target.
func invalidSchemaOp(field string, err error) error {
	if errors.Is(err, pmdoc.ErrSchema) {
		return &ErrInvalidOp{Field: field, Reason: err.Error()}
	}
	return err
}

func invalidMarkdownOp(field string, err error) error {
	if errors.Is(err, ErrInvalidMarkdown) {
		return &ErrInvalidOp{Field: field, Reason: err.Error()}
	}
	return err
}

func findEditQuote(tree *pmdoc.Node, quote string, occurrence *int) (pmdoc.Range, error) {
	r, err := resolveEditQuote(tree, quote, occurrence)
	if err != nil {
		return pmdoc.Range{}, err
	}
	if pmdoc.TargetSpansBlocks(tree, r) {
		return pmdoc.Range{}, pmdoc.ErrTargetSpansBlocks
	}
	return r, nil
}

func resolveEditQuote(tree *pmdoc.Node, quote string, occurrence *int) (pmdoc.Range, error) {
	r, err := pmdoc.FindQuote(tree, quote, occurrence, nil)
	if err == nil {
		return r, nil
	}
	var missing *pmdoc.ErrQuoteNotFound
	if errors.As(err, &missing) {
		return pmdoc.Range{}, &ErrQuoteNotFound{Nearest: missing.Nearest}
	}
	return pmdoc.Range{}, err
}

func insertTarget(tree *pmdoc.Node, anchor string, occurrence *int) (pmdoc.Range, bool, error) {
	switch anchor {
	case "start":
		return pmdoc.Range{}, false, nil
	case "end":
		position := pmdoc.Size(tree)
		return pmdoc.Range{From: position, To: position}, false, nil
	}
	if title, ok := strings.CutPrefix(anchor, "heading:"); ok {
		if title == "" {
			return pmdoc.Range{}, false, invalidOp("heading")
		}
		r, err := pmdoc.FindHeading(tree, title, occurrence)
		return r, false, err
	}
	if blockID, ok := strings.CutPrefix(anchor, "block:"); ok {
		if blockID == "" {
			return pmdoc.Range{}, false, invalidOp("block")
		}
		r, err := pmdoc.BlockRange(tree, blockID)
		return r, false, err
	}
	r, err := resolveEditQuote(tree, anchor, occurrence)
	if err != nil {
		return pmdoc.Range{}, true, err
	}
	return r, true, nil
}

// inlineReplacement parses replace's `with` as one textblock's inline content:
// a quote-anchored replace stays inside its textblock, so a leading list or
// heading marker is text, never a new block.
func inlineReplacement(markdown string) (*pmdoc.Node, error) {
	inline, err := pmdoc.ParseInline(markdown)
	if err != nil {
		if errors.Is(err, pmdoc.ErrSchema) {
			return nil, &ErrInvalidOp{Field: "with", Reason: fmt.Sprintf("replace is inline; %v (delete the block and insert new blocks instead)", err)}
		}
		return nil, err
	}
	paragraph := &pmdoc.Node{Type: "paragraph", Children: inline}
	continueText(paragraph, markdown)
	return pmdoc.StripAnchorMarks(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{paragraph}}), nil
}

// inlineAware parses a suggestion's replacement as blocks, keeping the edge
// whitespace of a replacement that stays inline.
func inlineAware(markdown string) (*pmdoc.Node, error) {
	tree, err := parseInput(markdown)
	if err != nil {
		return nil, err
	}
	if isInlineDocument(tree) {
		continueText(tree.Children[0], markdown)
	}
	return tree, nil
}

// continueText restores the leading and trailing whitespace markdown parsing
// drops so a replacement can continue the text around it.
func continueText(paragraph *pmdoc.Node, markdown string) {
	leading := markdown[:len(markdown)-len(strings.TrimLeftFunc(markdown, unicode.IsSpace))]
	trailing := markdown[len(strings.TrimRightFunc(markdown, unicode.IsSpace)):]
	if leading == "" && trailing == "" {
		return
	}
	var first, last *pmdoc.Node
	for _, child := range paragraph.Children {
		if child.Type == "text" {
			if first == nil {
				first = child
			}
			last = child
		}
	}
	if first == nil {
		return
	}
	first.Text = leading + first.Text
	last.Text += trailing
}

func isInlineLeaf(node *pmdoc.Node) bool {
	switch node.Type {
	case "hardbreak", "image", "html", "footnote_reference":
		return true
	default:
		return false
	}
}

func isInlineDocument(tree *pmdoc.Node) bool {
	if len(tree.Children) != 1 || tree.Children[0].Type != "paragraph" {
		return false
	}
	for _, child := range tree.Children[0].Children {
		if child.Type != "text" && !isInlineLeaf(child) {
			return false
		}
	}
	return true
}
