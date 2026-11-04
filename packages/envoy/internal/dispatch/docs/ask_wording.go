package docs

import (
	"fmt"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The bound an ask's wording stands inside, in one place for the two doors into it: a document's
// ask blocks, which the writes of this package are held to (readAskBlockStrictly,
// validateWrittenAskBlocks), and the ask routes, which measure a question and its options the
// same way (AskWordingUnits, read by api/asks.go and api/reviews.go).

// validateWrittenAskBlocks is why the asks of tree may not be written, nil where they may: the
// content rule over every ask in it, then each ask's wording against the bound it shares. It is
// what a write of whole markdown is held to - an upload, a repair, a rebuild - so every door
// into a document refuses the wording a route refuses, and none of them can leave an ask nobody
// may answer. The wording is read leniently (parseAskBlockLeniently): an ask with no question is
// the content rule's refusal and the parse's, not this bound's.
func validateWrittenAskBlocks(tree *pmdoc.Node) error {
	if err := pmdoc.AskContentError(tree); err != nil {
		return err
	}
	var refusal error
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if refusal != nil {
			return false
		}
		if node.Type != "ask" {
			return true
		}
		block, err := parseAskBlockLeniently(node)
		if err != nil {
			return true
		}
		refusal = askWordingTooLong(block)
		return refusal == nil
	})
	return refusal
}

// AskWordingUnits is how much of its bound an ask's wording spends: its question and every
// option's label and description, in UTF-16 units, each part's own text with nothing counted
// between them. An ask written in a document and one opened through a route are measured the
// same way, so neither can carry a wording the other would refuse.
func AskWordingUnits(question string, options []model.AskOption) int {
	units := itemblocks.Units16(question)
	for _, option := range options {
		units += itemblocks.Units16(option.Label) + itemblocks.Units16(option.Description)
	}
	return units
}

// askWordingTooLong is why block's wording passes contracts.DispatchEventBodyMax, nil where it
// stands inside it. A decision block opens an ask like any other, and its ask.answered event
// carries the same wording, so a document writes none a route would refuse.
func askWordingTooLong(block askBlock) error {
	units := AskWordingUnits(block.question, block.options)
	if units <= contracts.DispatchEventBodyMax {
		return nil
	}
	return fmt.Errorf("ask block %q is %d characters over the %d-character limit on its question and options (%d/%d)",
		block.id, units-contracts.DispatchEventBodyMax, contracts.DispatchEventBodyMax, units, contracts.DispatchEventBodyMax)
}
