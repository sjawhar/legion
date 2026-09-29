package docs

import (
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// errJoinsTwoTables is a reject whose insert runs from a cell of one table into a cell of
// another: the browser editor's reject joins the two into one table, which Splice does not, so
// the reject is refused rather than stored otherwise than the browser would store it.
var errJoinsTwoTables = errors.New("the insert runs from one table into the next, and removing it would join the two tables")

// rejectedInsert is tree with the text of the insert suggestion id deleted span by span
// (pmdoc.MarkSpans), last first so the earlier spans keep their positions, each with nothing
// spliced over it. Runs that meet across a block boundary are one span, as the browser editor's
// reject deletes them, so the blocks join: that undoes the split an insert made, and a table the
// span cuts is padded to its width afterwards (pmdoc.PadTables), as the browser's table plugin pads
// it. Text without the mark between two runs ends a span, so it is kept, where the browser's reject
// deletes it with them. A removal the document cannot hold, one the schema refuses or the renderer
// cannot write, is refused (rejectSpliceRefusal).
func rejectedInsert(tree *pmdoc.Node, id string, budget *pmdoc.TablePaddingBudget) (*pmdoc.Node, error) {
	spans := pmdoc.MarkSpans(tree, string(MarkSuggestion), id)
	nothing := &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{Type: "paragraph"}}}
	next := tree
	for index := len(spans) - 1; index >= 0; index-- {
		span := spans[index]
		if joinsTwo(next, span, "table") {
			return nil, rejectSpliceRefusal(errJoinsTwoTables)
		}
		spliced, err := pmdoc.Splice(next, span, nothing)
		if err != nil {
			return nil, rejectSpliceRefusal(err)
		}
		if next, err = padCutTables(next, spliced, span, budget); err != nil {
			return nil, rejectSpliceRefusal(err)
		}
		if err := next.Validate(); err != nil {
			return nil, rejectSpliceRefusal(err)
		}
	}
	if _, err := pmdoc.Render(next); err != nil {
		return nil, rejectSpliceRefusal(err)
	}
	return next, nil
}

// joinsTwo reports whether span runs from inside one node of nodeType into another: from one
// table into the next, or, for "table_row", from one body row into another (a header row is a
// table_header_row, so a range from it into the first body row is not one).
func joinsTwo(tree *pmdoc.Node, span pmdoc.Range, nodeType string) bool {
	holder := func(position int) *pmdoc.Node {
		at, _ := pmdoc.ContainingTextblock(tree, position)
		for _, ancestor := range at.Ancestors {
			if ancestor.Type == nodeType {
				return ancestor
			}
		}
		return nil
	}
	from, to := holder(span.From), holder(span.To)
	return from != nil && to != nil && from != to
}

// rejectSpliceRefusal words every refusal of a reject's removal, which leaves the document as it
// was and the suggestion open: the person rejecting cannot change the text, so each names what they
// can do, accept the suggestion or edit the document themselves. An insert that runs into an ask or
// callout from the text before it would join the two and leave the ask or callout empty, which the
// browser editor drops; one that runs from one table into the next would join the tables
// (errJoinsTwoTables); and any other removal leaves a shape the document cannot hold, one the
// schema refuses or the renderer cannot write.
func rejectSpliceRefusal(err error) error {
	const advice = "accept the suggestion, or remove the text by editing the document"
	switch {
	case errors.Is(err, pmdoc.ErrJoinEmptiesTypedBlock):
		return &ErrInvalidOp{Field: "anchor", Reason: "the insert runs into an ask or callout from the text before it, and " +
			"removing it would join the two and leave the ask or callout empty; " + advice}
	case errors.Is(err, errJoinsTwoTables):
		return &ErrInvalidOp{Field: "anchor", Reason: err.Error() + "; " + advice}
	case errors.Is(err, pmdoc.ErrSchema):
		return &ErrInvalidOp{Field: "anchor", Reason: fmt.Sprintf("removing the insert would leave a shape the document "+
			"cannot hold (%v); %s", err, advice)}
	}
	return err
}
