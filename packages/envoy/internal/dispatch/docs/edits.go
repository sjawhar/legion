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
		with, err := inlineAware(op.With)
		if err != nil {
			return nil, invalidMarkdownOp("with", err)
		}
		return pmdoc.Splice(tree, r, with)
	case "delete":
		if op.Find == "" {
			return nil, invalidOp("find")
		}
		r, err := findEditQuote(tree, op.Find, op.Occurrence)
		if err != nil {
			return nil, err
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
		if (op.After == "" && op.Before == "") || (op.After != "" && op.Before != "") {
			return nil, invalidOp("after or before")
		}
		anchor := op.After
		after := anchor != ""
		if !after {
			anchor = op.Before
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
			if strings.Contains(err.Error(), "unknown typed block") {
				field = "type"
			}
			return nil, &ErrInvalidOp{Field: field, Reason: err.Error()}
		}
		return out, err
	default:
		return nil, invalidOp("op")
	}
}

func invalidMarkdownOp(field string, err error) error {
	if errors.Is(err, ErrInvalidMarkdown) {
		return &ErrInvalidOp{Field: field, Reason: err.Error()}
	}
	return err
}

func findEditQuote(tree *pmdoc.Node, quote string, occurrence *int) (pmdoc.Range, error) {
	r, err := pmdoc.FindQuote(tree, quote, occurrence, nil)
	if err != nil {
		return pmdoc.Range{}, err
	}
	if pmdoc.TargetSpansBlocks(tree, r) {
		return pmdoc.Range{}, pmdoc.ErrTargetSpansBlocks
	}
	return r, nil
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
	r, err := pmdoc.FindQuote(tree, anchor, occurrence, nil)
	if err != nil {
		return pmdoc.Range{}, true, err
	}
	return r, true, nil
}

func inlineAware(markdown string) (*pmdoc.Node, error) {
	tree, err := parseInput(markdown)
	if err != nil {
		return nil, err
	}
	if !isInlineDocument(tree) {
		return tree, nil
	}
	paragraph := tree.Children[0]
	leading := markdown[:len(markdown)-len(strings.TrimLeftFunc(markdown, unicode.IsSpace))]
	trailing := markdown[len(strings.TrimRightFunc(markdown, unicode.IsSpace)):]
	if leading == "" && trailing == "" {
		return tree, nil
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
		return tree, nil
	}
	first.Text = leading + first.Text
	last.Text += trailing
	return tree, nil
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
