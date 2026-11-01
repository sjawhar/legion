package docs

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// An accepted suggestion writes blocks, so what it stores is read back before it is written, and an
// accept whose document would not read back as the live tree is refused, naming replace_with, with
// nothing written and the suggestion left open. applySuggestion runs these in order: settleAccepted,
// refuseCodePipeInTableCell, then outside an ask refuseUnreadableAccept and refuseReshapedAccept over
// each document-level block the accept changed, the write's block-id check, refuseBrokenAsks, and
// last refuseMisreadAccept over the whole document.

// settleAccepted makes what an accept wrote hold what its markdown reads back as where that is
// unambiguous (pmdoc.AgreeWithReadBack): the empty halves a block replacement leaves of the
// textblock it lands in go where the renderer does not write them, and the lists and list items in
// the blocks it changed take the spread their markdown reads back with, except one it left
// unchanged whose spread already read back otherwise. An empty replacement keeps the paragraph it
// empties, which the renderer does not write beside other blocks. Everything else, block, mark and
// node, stays as it was.
func settleAccepted(before pmdoc.ValidDocument, after *pmdoc.Node, found matchBlocks, with string) *pmdoc.Node {
	return pmdoc.AgreeWithReadBack(before.Node(), after, found.first, found.lastAfter(after), with != "")
}

// acceptedCode is the code text an accepted suggestion writes over match in the code block at, as
// that code reads back, so the accept stores what reads back: markdown drops the line breaks that
// end code, so where only line breaks follow the match, the text loses its own ending breaks, a
// blank line it ends with included. A match can run past the code into later blocks of tree, to
// the textblock it ends in. One that takes all of that textblock's text leaves nothing after the
// text, as at the code's end, so the same rule applies. One that ends inside that text is written
// as sent: the rest of it, which the splice joins to the code, follows the text, and the rule
// cannot judge it.
func acceptedCode(tree *pmdoc.Node, with string, at pmdoc.TextblockAt, match pmdoc.Range) string {
	if match.To > at.Content.To {
		if end, _ := pmdoc.ContainingTextblock(tree, match.To); match.To != end.Content.To {
			return with
		}
	}
	var text strings.Builder
	for _, child := range at.Node.Children {
		text.WriteString(child.Text)
	}
	code := utf16.Encode([]rune(text.String()))
	to := min(match.To, at.Content.To) - at.Content.From
	if !slices.ContainsFunc(code[to:], func(unit uint16) bool { return unit != '\n' }) {
		with = strings.TrimRight(with, "\n")
	}
	return with
}

// insideAsk reports whether an accepted suggestion lands in an ask. refuseBrokenAsks is the ask
// round-trip and semantic rule, so it names an empty question or another invalid ask before a
// document-level parser failure can.
func insideAsk(at pmdoc.TextblockAt) bool {
	for _, ancestor := range at.Ancestors {
		if ancestor.Type == "ask" {
			return true
		}
	}
	return false
}

// refuseUnreadableAccept refuses a suggestion whose changed document-level block cannot be read
// from its rendered markdown.
func refuseUnreadableAccept(before pmdoc.ValidDocument, after *pmdoc.Node, found matchBlocks, match pmdoc.Range, at pmdoc.TextblockAt, with string, replacement *pmdoc.Node) error {
	return refuseAcceptBy(before, after, found, match, at, with, replacement, pmdoc.BlockReadError)
}

// refuseReshapedAccept refuses a suggestion whose changed document-level block reads back as
// blocks of another shape.
func refuseReshapedAccept(before pmdoc.ValidDocument, after *pmdoc.Node, found matchBlocks, match pmdoc.Range, at pmdoc.TextblockAt, with string, replacement *pmdoc.Node) error {
	return refuseAcceptBy(before, after, found, match, at, with, replacement, pmdoc.BlockShapeError)
}

func refuseAcceptBy(before pmdoc.ValidDocument, after *pmdoc.Node, found matchBlocks, match pmdoc.Range, at pmdoc.TextblockAt, with string, replacement *pmdoc.Node, check func(*pmdoc.Node) error) error {
	_, broke, err := replacementBroke(before.Node(), after, found, check)
	if err != nil || broke == nil {
		return err
	}
	return &ErrInvalidOp{Field: "replace_with", Reason: acceptRefusal(before.Node(), after, match, at, with, replacement, broke)}
}

// refuseMisreadAccept refuses an accept whose document reads back otherwise where it did not
// before (pmdoc.NewMisread), its attributes and text as well as its blocks, such as a task item
// emptied to `- [ ]`, which reads back as a plain item. A block that already read back otherwise
// the same way before the accept, the one it changed included, is not the accept's. The checks
// before it read one block at a time; this one reads the blocks beside each other. The refusal
// names what reads back and advises rejecting. An error reading the document back that is no
// refusal, a panic (pmdoc.ErrPanic) among them, is pmdoc's bug, and is its error.
func refuseMisreadAccept(before, after *pmdoc.Node, with string) error {
	misread, err := pmdoc.NewMisread(before, after)
	if err != nil {
		return err
	}
	if misread == "" {
		return nil
	}
	return &ErrInvalidOp{Field: "replace_with", Reason: fmt.Sprintf(
		"replace_with %q leaves blocks the document reads back otherwise (%s); reject the suggestion", with, misread,
	)}
}

// acceptRefusal says what an accepted suggestion's text does where it lands, why the document
// cannot carry it (broke), and what the person accepting can do: reject the suggestion, or ask for
// text the block can hold. It names no edit operation, since accepting takes none. Blocks that
// rewrite a typed block around the match in place land where that block stood, so that is the
// block named. The text empties the paragraph it lands in only when it renders no content, and then
// the delete it names removes only the paragraph where the rest of its block stands without it;
// blocks written at a list item's start leave the item's own line empty ahead of them.
func acceptRefusal(before, after *pmdoc.Node, match pmdoc.Range, at pmdoc.TextblockAt, with string, replacement *pmdoc.Node, broke error) string {
	holder := holderName(at.Ancestors[0])
	landed, found := pmdoc.ContainingTextblock(after, match.From)
	emptied := found && emptyTextblock(landed.Node)
	if !isInlineDocument(replacement) {
		parent := at.Ancestors[0]
		for index := 1; index < len(at.Ancestors) && pmdoc.IsTypedBlock(parent.Type) && pmdoc.RewritesBlock(replacement.Children, parent); index++ {
			parent = at.Ancestors[index]
		}
		holder = holderName(parent)
		where, ask := "in this "+holder, "text the "+holder+" can hold"
		if holder == "list item" && emptied {
			where, ask = "at the start of this list item", "the block to follow text on the item's line"
		}
		return fmt.Sprintf(
			"replace_with %q writes %s %s, which the document cannot read back there (%v); reject the suggestion, or reply asking for %s",
			with, pmdoc.BlockNames(replacement.Children), where, broke, ask,
		)
	}
	if emptyTextblock(replacement.Children[0]) && (!found || emptied) {
		advice := "reject the suggestion, or delete the " + holder + " in the document"
		if len(at.Ancestors[0].Children) > 1 {
			advice = "reject the suggestion, since the rest of the " + holder + " cannot be written without this paragraph"
			if _, err := pmdoc.DeleteBlock(before, blockID(at.Node)); err == nil {
				advice = "reject the suggestion, or delete the paragraph in the document, which leaves the rest of the " + holder
			}
		}
		return fmt.Sprintf(
			"replace_with %q empties the paragraph this %s holds, and the %s cannot be written with it empty; %s",
			with, holder, holder, advice,
		)
	}
	return fmt.Sprintf(
		"replace_with %q leaves text the document reads back as another block where it lands (%v); reject the suggestion, or reply asking for the text inside a line",
		with, broke,
	)
}

// padCutTables pads each table in the document-level blocks a splice changed, from before to after
// (matchedBlocks), to its width (pmdoc.PadTables), as the browser editor's table plugin pads a
// table after any change, spending budget, the accept's or reject's.
func padCutTables(after *pmdoc.Node, found matchBlocks, budget *pmdoc.WriteBudget) (*pmdoc.Node, error) {
	return pmdoc.PadTables(after, found.first, found.lastAfter(after), budget)
}

// padsLikeTheBrowser reports whether an accept may pad the tables its splice cut: whether the
// browser editor's accept writes the replacement where Splice does (proof-sdk marks.ts accept and
// applyMarkdownReplace). A range running from one table into the next, or from one body row into
// another, is never padded: the browser can join the two tables or rows, which Splice does not. An
// inline replacement (text that stays in the textblock, or code's literal text) replaces the
// matched range there as here; an empty one follows this accept's own rule, deleting the matched
// text, where the browser's accept of an empty suggestion only clears its mark. The browser takes
// block content across two textblocks and replaces both whole (pmdoc.MultiblockRange). Splice
// replaces what the browser does only when the match is exactly those textblocks' content and the
// first, at, is a document-level block, not one inside a callout, a quote, a list item or a table
// cell. Other block content over a table, such as a list over one cell's whole text, is not padded
// either. An accept that is not padded is judged as the splice left it, which refuses one that cut
// a table.
func padsLikeTheBrowser(tree *pmdoc.Node, match pmdoc.Range, at pmdoc.TextblockAt, inline bool) bool {
	if joinsTwo(tree, match, "table") || joinsTwo(tree, match, "table_row") {
		return false
	}
	if inline {
		return true
	}
	blocks, ok := pmdoc.MultiblockRange(tree, match)
	return ok && blocks == match && len(at.Ancestors) == 1
}

// acceptSpliceRefusal words the refusal of an accept whose replacement Splice cannot fit, naming
// what the person accepting can do: an inline replacement running into an ask or callout from the
// text before it would join the two and leave the ask or callout empty; a replacement no level of
// the document can hold where the suggestion sits; tables the splice cuts that padding back to
// their width would pad past the write's limit (pmdoc.ErrTablePadding); and any other join the
// document cannot hold.
func acceptSpliceRefusal(err error) error {
	switch {
	case errors.Is(err, pmdoc.ErrJoinEmptiesTypedBlock):
		return &ErrInvalidOp{Field: "anchor", Reason: "the suggestion runs into an ask or callout from the text before it, " +
			"and replacing it would join the two and leave the ask or callout empty; suggest a change inside one of them"}
	case errors.Is(err, pmdoc.ErrReplacementDoesNotFit):
		return &ErrInvalidOp{Field: "replace_with", Reason: "no part of the document can hold it where the suggestion sits " +
			"(a table cell's whole text, for one, can only be replaced by inline text)"}
	case errors.Is(err, pmdoc.ErrTablePadding):
		return &ErrInvalidOp{Field: "replace_with", Reason: fmt.Sprintf("padding the tables the suggestion cuts back to their "+
			"width would pad past the limit (%v); reject the suggestion, or suggest a change inside one table row", err)}
	case errors.Is(err, pmdoc.ErrSchema):
		return &ErrInvalidOp{Field: "anchor", Reason: fmt.Sprintf("the suggestion runs across blocks that replacing it "+
			"would join, which the document cannot hold together (%v); reject the suggestion, or suggest a change inside one block", err)}
	}
	return err
}

// refuseAcceptedCodeThatReshapes refuses an accept whose code changes how a block around the code
// reads back (pmdoc.BlockShapeError), in the words of the other accept refusals: the block, why,
// and what the person accepting can do, since they cannot move the code as the edit route's
// refusal (refuseCodeThatReshapesItsBlock) advises. The block named is the typed block holding
// the code when the document-level block that reads back otherwise is the one holding the code,
// and otherwise that block, such as the list of a task item that a suggestion running out of the
// code empties ahead of the item's nested list.
func refuseAcceptedCodeThatReshapes(before pmdoc.ValidDocument, after *pmdoc.Node, found matchBlocks, at pmdoc.TextblockAt, with string) error {
	broken, reshaped, err := replacementBroke(before.Node(), after, found, pmdoc.BlockShapeError)
	if err != nil || reshaped == nil {
		return err
	}
	// The document-level block holding the code is the first the accept changed (matchedBlocks),
	// at the same index before and after it.
	holding := found.first
	if broken == holding {
		for _, ancestor := range at.Ancestors {
			if pmdoc.IsTypedBlock(ancestor.Type) {
				holder := holderName(ancestor)
				return &ErrInvalidOp{Field: "replace_with", Reason: fmt.Sprintf(
					"replace_with %q changes how the %s holding this code block reads back (%v); reject the suggestion, or reply asking for code the %s can hold",
					with, holder, reshaped, holder,
				)}
			}
		}
	}
	return &ErrInvalidOp{Field: "replace_with", Reason: fmt.Sprintf(
		"replace_with %q changes how the %s reads back (%v); reject the suggestion, or reply asking for a change that stays inside the code block",
		with, holderName(after.Children[broken]), reshaped,
	)}
}

// holderName names the block holding a match as a reader names it.
func holderName(parent *pmdoc.Node) string {
	if parent.Type == "doc" {
		return "document"
	}
	return strings.ReplaceAll(parent.Type, "_", " ")
}

// emptyTextblock reports whether a textblock holds no text but whitespace.
func emptyTextblock(textblock *pmdoc.Node) bool {
	for _, child := range textblock.Children {
		if child.Type != "text" || strings.TrimSpace(child.Text) != "" {
			return false
		}
	}
	return true
}

// matchBlocks is where a write's match lies in the tree the write started from: the first
// document-level block holding the match's start, the last holding its end, and how many blocks
// that tree held. They depend on that tree and the match alone, so a write finds them once and
// reads them against each tree it goes on to make (lastAfter), where before every refusal found
// them again for itself.
type matchBlocks struct {
	first  int
	last   int
	blocks int
}

// matchedBlocks is where match lies in before, both endpoints found in one walk of its blocks
// (pmdoc.ValidDocument.BlockIndexes). A match an endpoint lands in no block of is
// pmdoc.ErrTargetNotFound, as a lookup of that endpoint gives it.
func matchedBlocks(before pmdoc.ValidDocument, match pmdoc.Range) (matchBlocks, error) {
	first, last, err := before.BlockIndexes(match)
	if err != nil {
		return matchBlocks{}, err
	}
	return matchBlocks{first: first, last: last, blocks: len(before.Node().Children)}, nil
}

// lastAfter is the last document-level block the write changed in after, one of the trees it
// made: the blocks it changed run from the one holding the match's start to as many more as the
// document gained, since the blocks after them keep their order. A replace stays inside its
// textblock, so its block holds the same index before and after; an accepted suggestion can write
// blocks, and its match can span them. A write that goes on to change the tree again (padding its
// tables, settling what it wrote) reads it against each tree, as a refusal reading that tree
// always did.
func (m matchBlocks) lastAfter(after *pmdoc.Node) int {
	return max(m.first, m.last+len(after.Children)-m.blocks)
}

// replacementBroke is the index in after of the first document-level block the write changed
// (matchedBlocks) that check fails, and what check says of it, when it said nothing of the blocks
// the match lay in before: a block that already failed the check, or another block that does, is
// no reason to refuse this write. A block before that reads back only past a read-back's padding
// budget (pmdoc.ErrTablePadding) did not fail it, so the write is refused with that, naming the
// first block it changed, rather than stored unchecked. Only a refusal (pmdoc.ErrSchema) is
// check's verdict; any other error from it, a panic (pmdoc.ErrPanic) among them, is no verdict on
// either side, but pmdoc's bug, and is its error.
func replacementBroke(before, after *pmdoc.Node, found matchBlocks, check func(*pmdoc.Node) error) (block int, broke, err error) {
	for index := found.first; index <= found.last; index++ {
		if err := check(before.Children[index]); err != nil {
			if !errors.Is(err, pmdoc.ErrSchema) {
				return -1, nil, err
			}
			if errors.Is(err, pmdoc.ErrTablePadding) {
				return found.first, err, nil
			}
			return -1, nil, nil
		}
	}
	lastAfter := found.lastAfter(after)
	for index := found.first; index <= lastAfter; index++ {
		if broke = check(after.Children[index]); broke != nil {
			if !errors.Is(broke, pmdoc.ErrSchema) {
				return -1, nil, broke
			}
			return index, broke, nil
		}
	}
	return -1, nil, nil
}
